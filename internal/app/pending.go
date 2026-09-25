package app

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/algorand/go-algorand-sdk/v2/encoding/msgpack"
	"github.com/algorand/go-codec/codec"

	"github.com/d13co/algod-loadb-mesh/internal/domain"
	"github.com/d13co/algod-loadb-mesh/internal/ports"
)

// pendingGrace is how long a pending lookup waits for a better answer (a
// confirmation or a pool error) once some node has answered 200.
const pendingGrace = 250 * time.Millisecond

// Pending lookup answers, worst to best.
const (
	pendingFailed    = iota // transport error, 5xx, 429
	pendingOther            // any other non-200, e.g. 400 for a malformed id
	pendingNotFound         // 404
	pendingInPool           // 200, still waiting in the pool
	pendingPoolError        // 200, dropped from the pool with an error
	pendingConfirmed        // 200 with confirmed-round
)

var pendingAnswer = [...]string{"failed", "other", "not_found", "in_pool", "pool_error", "confirmed"}

// handlePending looks a pending transaction up on the node that accepted it,
// then on every other node at once. A pool error lives only in the pool of
// the nodes that dropped the txn, so the best answer wins: confirmed, then
// pool error, then still pending, then 404.
func (r *Router) handlePending(w http.ResponseWriter, req *http.Request, class domain.RequestClass, cands []domain.Upstream, best uint64) {
	sel, cands, best, ok := r.pick(req.Context(), class, cands, best)
	if !ok {
		r.noUpstream(w, class, cands, best)
		return
	}
	order := append([]domain.Upstream{sel.Chosen}, sel.Alternates...)
	sortByRoundDesc(order) // ties in rank go to the highest round

	var pinned *ports.Outcome
	if id, ok := r.recallTxn(class.PendingID); ok {
		for i, u := range order {
			if u.ID != id {
				continue
			}
			out := r.forward(w, req, u, class, map[int]bool{404: true})
			if out.HeadersSent || req.Context().Err() != nil {
				return
			}
			pinned = &out
			order = append(order[:i:i], order[i+1:]...)
			break
		}
	}
	if len(order) == 0 {
		if pinned.Status == 404 {
			writeMessage(w, 404, "algod-loadb-mesh: transaction not found on any node")
			return
		}
		r.answerFailure(w, req, *pinned)
		return
	}

	pos := make(map[string]int, len(order))
	for i, u := range order {
		pos[u.ID] = i
	}
	ctx, cancel := context.WithTimeout(req.Context(), r.opts.UpstreamTimeout)
	defer cancel()
	results := r.fanOut(ctx, req, nil, order, class)
	var (
		chosen *fanResult
		rank   = -1
		grace  <-chan time.Time
	)
collect:
	for range order {
		var res fanResult
		select {
		case res = <-results:
		case <-grace:
			break collect
		case <-req.Context().Done():
			return
		}
		k := pendingRank(res)
		if k > rank || (k == rank && pos[res.u.ID] < pos[chosen.u.ID]) {
			res := res
			chosen, rank = &res, k
		}
		if rank == pendingConfirmed {
			break
		}
		if rank >= pendingInPool && grace == nil {
			tick, stop := r.clock.Tick(pendingGrace)
			defer stop()
			grace = tick
		}
	}
	cancel()
	r.metric.Inc("loadb_pending_lookups", "answer", pendingAnswer[rank])
	if rank == pendingInPool || rank == pendingPoolError {
		r.rememberTxn(class.PendingID, chosen.u.ID) // later polls go straight there
	}
	if rank == pendingFailed && chosen.err != nil {
		writeMessage(w, 502, "algod-loadb-mesh: pending lookup failed on every node: "+chosen.err.Error())
		return
	}
	writeFan(w, *chosen)
}

// pendingRank classifies one node's answer to a pending lookup.
func pendingRank(res fanResult) int {
	if res.err != nil {
		return pendingFailed
	}
	switch st := res.resp.StatusCode; {
	case st >= 500 || st == 429:
		return pendingFailed
	case st == 404:
		return pendingNotFound
	case st != 200:
		return pendingOther
	}
	var v struct {
		ConfirmedRound uint64 `json:"confirmed-round" codec:"confirmed-round"`
		PoolError      string `json:"pool-error" codec:"pool-error"`
	}
	var err error
	if strings.Contains(res.resp.Header.Get("Content-Type"), "msgpack") {
		err = codec.NewDecoderBytes(res.body, msgpack.LenientCodecHandle).Decode(&v)
	} else {
		err = json.Unmarshal(res.body, &v)
	}
	switch {
	case err != nil:
		return pendingInPool // a 200 we cannot read is still an answer
	case v.ConfirmedRound > 0:
		return pendingConfirmed
	case v.PoolError != "":
		return pendingPoolError
	}
	return pendingInPool
}
