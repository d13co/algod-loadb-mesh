package app

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/d13co/algod-loadb-mesh/internal/domain"
	"github.com/d13co/algod-loadb-mesh/internal/ports"
)

// RouterOptions configures request routing.
type RouterOptions struct {
	Mode            domain.Mode
	Balancer        bool // no local node
	ClientToken     string
	AdminToken      string // required for /loadb/* except health; also a valid client token
	SyncTolerance   uint64
	UpstreamTimeout time.Duration
	WaitTimeout     time.Duration
	PendingTTL      time.Duration
	RetryBudget     int // further mesh upstreams tried after a failed one; negative for every eligible one
	MultiBroadcast  bool
	Weights         domain.Weights
	Version         string
}

func (o *RouterOptions) defaults() {
	if o.Mode == "" {
		o.Mode = domain.ModeFallback
	}
	if o.UpstreamTimeout == 0 {
		o.UpstreamTimeout = 60 * time.Second
	}
	if o.WaitTimeout == 0 {
		o.WaitTimeout = 60 * time.Second
	}
	if o.PendingTTL == 0 {
		o.PendingTTL = 10 * time.Second
	}
}

// Router is the HTTP entry point: it classifies, selects, forwards and
// records. It knows nothing about sockets beyond net/http's handler API.
type Router struct {
	opts    RouterOptions
	dir     *Directory
	monitor *Monitor
	fwd     ports.Forwarder
	httpc   *http.Client
	stats   *StatsBook
	clock   ports.Clock
	log     ports.Logger
	metric  ports.Metrics
	rnd     domain.Rand
	metrics io.WriterTo // Prometheus text, may be nil

	inflight    atomic.Int64
	draining    atomic.Bool
	passthrough atomic.Pointer[func() PassthroughStatus]
	started     time.Time

	pins *txnPins

	// A balancer's coalesced wait-for-block-after: the latest status body
	// and the fetch in flight, shared by every waiter.
	statusMu    sync.Mutex
	status      *statusFetch // last completed fetch
	statusFetch *statusFetch // in flight
}

// statusFetch is one GET /v2/status for all balancer waiters.
type statusFetch struct {
	done     chan struct{}
	want     uint64 // round the fetch was started for; fixed
	round    uint64 // last-round in body; set before done closes
	upstream domain.Upstream
	body     []byte
	err      error
}

// NewRouter wires the router.
func NewRouter(o RouterOptions, dir *Directory, monitor *Monitor, fwd ports.Forwarder, httpc *http.Client,
	stats *StatsBook, clock ports.Clock, log ports.Logger, metric ports.Metrics, rnd domain.Rand, metricsText io.WriterTo) *Router {
	o.defaults()
	if httpc == nil {
		httpc = &http.Client{Timeout: o.UpstreamTimeout}
	}
	return &Router{opts: o, dir: dir, monitor: monitor, fwd: fwd, httpc: httpc, stats: stats, clock: clock,
		log: log, metric: metric, rnd: rnd, metrics: metricsText, pins: newTxnPins(o.PendingTTL, clock.Now()), started: clock.Now()}
}

// PassthroughStatus is the algod pass-through as /loadb/status shows it:
// the addresses bound for algod, the configured ones algod already covered,
// the address the splice dials and the connections open right now.
type PassthroughStatus struct {
	Addrs   []string `json:"addrs"`
	Skipped []string `json:"skipped,omitempty"`
	Target  string   `json:"target"`
	Active  int64    `json:"active"`
}

// SetPassthrough adds a "passthrough" key to /loadb/status, read from f on
// every request. The composition root sets it once the listeners are bound.
func (r *Router) SetPassthrough(f func() PassthroughStatus) { r.passthrough.Store(&f) }

// Draining marks the agent as shutting down; new requests are still served
// until the listener closes, but health reports 503 and peers are told.
func (r *Router) Draining(v bool) {
	r.draining.Store(v)
	r.dir.SetDraining(v)
}

// Inflight is the number of requests currently being served.
func (r *Router) Inflight() int64 { return r.inflight.Load() }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeMessage(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"message": msg})
}

// ServeHTTP implements http.Handler.
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.inflight.Add(1)
	defer r.inflight.Add(-1)
	start := r.clock.Now()
	class := domain.Classify(req.Method, req.URL.Path)
	sw := &statusWriter{ResponseWriter: w}
	defer r.logRequest(sw, req, class, start)
	r.serve(sw, req, class)
}

// logRequest writes the access log line: INFO, except health probes at DEBUG.
func (r *Router) logRequest(w *statusWriter, req *http.Request, class domain.RequestClass, start time.Time) {
	kv := []any{"method", req.Method, "path", req.URL.RequestURI(), "status", w.status, "bytes", w.bytes,
		"duration", r.clock.Now().Sub(start), "ip", clientIP(req)}
	if up := w.Header().Get("X-Algod-Loadb-Mesh-Upstream"); up != "" {
		kv = append(kv, "upstream", up)
	}
	if !class.Agent {
		kv = append(kv, "class", class.String())
	}
	if req.Context().Err() != nil {
		kv = append(kv, "client_gone", true)
	}
	if isHealth(req, class) {
		r.log.Debug("request", kv...)
		return
	}
	r.log.Info("request", kv...)
}

// authorize checks X-Algo-API-Token and answers 401/403 itself when the
// request may not proceed. /loadb/health is open; the rest of /loadb/* needs
// the admin token; everything else needs the client or the admin token. With
// neither token configured, auth is off.
func (r *Router) authorize(w http.ResponseWriter, req *http.Request, class domain.RequestClass) bool {
	tok := req.Header.Get("X-Algo-API-Token")
	admin := r.opts.AdminToken != "" && tokenEqual(tok, r.opts.AdminToken)
	switch {
	case admin, isHealth(req, class), r.opts.AdminToken == "" && r.opts.ClientToken == "":
		return true
	case class.Agent && r.opts.AdminToken == "":
		r.metric.Inc("loadb_requests_unauthorized")
		writeMessage(w, 403, "algod-loadb-mesh: /loadb endpoints need an admin token, and none is configured")
		return false
	case !class.Agent && (r.opts.ClientToken == "" || tokenEqual(tok, r.opts.ClientToken)):
		return true
	}
	r.metric.Inc("loadb_requests_unauthorized")
	writeMessage(w, 401, "Invalid API Token")
	return false
}

func tokenEqual(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func isHealth(req *http.Request, class domain.RequestClass) bool {
	return class.Agent && strings.TrimSuffix(req.URL.Path, "/") == "/loadb/health"
}

// clientIP is the leftmost X-Forwarded-For entry, then X-Real-IP, then the
// socket peer. The headers are client-controlled; they are only trustworthy
// behind a proxy that rewrites them.
func clientIP(req *http.Request) string {
	ip, _, _ := strings.Cut(req.Header.Get("X-Forwarded-For"), ",")
	if ip = strings.TrimSpace(ip); ip == "" {
		ip = strings.TrimSpace(req.Header.Get("X-Real-IP"))
	}
	if ip == "" {
		ip = req.RemoteAddr
		if host, _, err := net.SplitHostPort(ip); err == nil {
			ip = host
		}
	}
	return strings.TrimPrefix(ip, "::ffff:")
}

func (r *Router) serve(w http.ResponseWriter, req *http.Request, class domain.RequestClass) {
	if !r.authorize(w, req, class) {
		return
	}
	if class.Agent {
		r.serveAgent(w, req)
		return
	}
	cands, best := r.dir.Snapshot()
	w.Header().Set("X-Algod-Loadb-Mesh-Mode", string(r.opts.Mode))
	switch {
	case class.WaitAfter != nil:
		r.handleWait(w, req, class, cands, best)
	case class.Broadcast:
		r.handleBroadcast(w, req, class, cands, best)
	case class.PendingID != "":
		r.handlePending(w, req, class, cands, best)
	default:
		r.handleDefault(w, req, class, cands, best)
	}
}

func (r *Router) sel(cands []domain.Upstream, class domain.RequestClass, best uint64) (domain.Selection, bool) {
	return domain.Select(r.opts.Mode, cands, class, best, r.opts.SyncTolerance, r.opts.Weights, r.rnd)
}

// mayUseExternals is false for a request no external can ever serve, so
// that it never triggers a check of one.
func mayUseExternals(class domain.RequestClass) bool { return !class.LocalOnly }

// pick selects an upstream for the request. When nothing in the mesh can
// serve it, the externals are checked — they are only checked on demand, so
// a third-party RPC costs nothing while the mesh is healthy — and the
// selection is repeated on a fresh snapshot, which is returned for the
// caller's reporting.
func (r *Router) pick(ctx context.Context, class domain.RequestClass, cands []domain.Upstream, best uint64) (domain.Selection, []domain.Upstream, uint64, bool) {
	if sel, ok := r.sel(cands, class, best); ok {
		return sel, cands, best, true
	}
	if !mayUseExternals(class) || !r.dir.checkExternals(ctx) {
		return domain.Selection{}, cands, best, false
	}
	cands, best = r.dir.Snapshot()
	sel, ok := r.sel(cands, class, best)
	return sel, cands, best, ok
}

// forward sends the request to one upstream and records the outcome.
func (r *Router) forward(w http.ResponseWriter, req *http.Request, u domain.Upstream, class domain.RequestClass, retry map[int]bool) ports.Outcome {
	w.Header().Set("X-Algod-Loadb-Mesh-Upstream", u.ID)
	w.Header().Set("X-Algod-Loadb-Mesh-Tier", fmt.Sprint(u.Tier))
	ctx, cancel := context.WithTimeout(req.Context(), r.opts.UpstreamTimeout)
	defer cancel()
	release := r.stats.Begin(u.ID)
	out := r.fwd.Forward(w, req.WithContext(ctx), ports.Target{BaseURL: u.BaseURL, Token: u.Token, RetryStatus: retry})
	release()
	if req.Context().Err() == nil { // client still there: the outcome is about the upstream
		r.stats.Record(u.ID, class.StatKey(), out)
	}
	r.metric.Inc("loadb_upstream_requests", "upstream", u.ID, "class", class.StatKey(), "failed", boolStr(out.Failed()))
	r.metric.Observe("loadb_upstream_latency_ms", float64(out.Duration)/float64(time.Millisecond), "upstream", u.ID, "class", class.StatKey())
	return out
}

func (r *Router) noUpstream(w http.ResponseWriter, class domain.RequestClass, cands []domain.Upstream, best uint64) {
	r.metric.Inc("loadb_requests_unroutable", "class", class.StatKey())
	summary := make([]string, 0, len(cands))
	for _, u := range cands {
		summary = append(summary, fmt.Sprintf("%s(%s r=%d oldest=%d dev=%v follow=%v breaker=%v draining=%v)", u.ID, u.Health,
			u.LastRound, u.Caps.OldestRound, u.Caps.DeveloperAPI, u.Caps.FollowMode, u.Stats.BreakerOpen, u.Draining))
	}
	r.log.Warn("no eligible upstream", "class", class.String(), "best", best, "candidates", strings.Join(summary, " "))
	if class.LocalOnly && r.opts.Balancer {
		writeMessage(w, 503, "algod-loadb-mesh: this agent is a balancer with no local node, and this request is only answered by one")
		return
	}
	writeMessage(w, 503, "algod-loadb-mesh: no eligible upstream for this request")
}

func (r *Router) handleDefault(w http.ResponseWriter, req *http.Request, class domain.RequestClass, cands []domain.Upstream, best uint64) {
	sel, cands, best, ok := r.pick(req.Context(), class, cands, best)
	if !ok {
		r.noUpstream(w, class, cands, best)
		return
	}
	r.forwardSelected(w, req, class, sel)
}

// maxReplayBody bounds a request body held so that the request can be sent
// to more than one upstream; a larger one is sent once.
const maxReplayBody = 4 << 20

// replayable holds the request body, when there is one and it fits, so that
// the request can be forwarded more than once. It returns a reset that
// installs a fresh copy before each forward, and false when the request can
// only be sent once.
func replayable(req *http.Request) (func(), bool) {
	if req.Body == nil || req.Body == http.NoBody || req.ContentLength == 0 {
		return func() {}, true
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, maxReplayBody+1))
	if err != nil || len(body) > maxReplayBody {
		req.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), req.Body))
		return func() {}, false
	}
	req.ContentLength = int64(len(body))
	reset := func() { req.Body = io.NopCloser(bytes.NewReader(body)) }
	reset()
	return reset, true
}

// forwardSelected sends the request to the chosen upstream and, while it
// fails without a byte reaching the client, to each alternate in turn: every
// eligible mesh node once (fewer under an explicit retry budget), then the
// externals. A failure here is a transport error or a 502/503/504, which the
// forwarder withholds from the client; any answer the upstream produced,
// such as a rejected transaction, is the client's and ends the attempts.
// Writes are repeated too: a signed transaction has one id, so a node that
// already holds it answers for it rather than applying it twice, and algod's
// other writes are idempotent by content. A body too large to hold is sent
// once.
func (r *Router) forwardSelected(w http.ResponseWriter, req *http.Request, class domain.RequestClass, sel domain.Selection) {
	tries := append([]domain.Upstream{sel.Chosen}, sel.Alternates...)
	if b := r.opts.RetryBudget; b >= 0 && len(tries) > 1+b {
		tries = tries[:1+b]
	}
	reset, replay := replayable(req)
	if !replay {
		tries = tries[:1]
	}
	var last ports.Outcome
	for i, u := range tries {
		if i > 0 {
			r.metric.Inc("loadb_retries")
			r.log.Debug("retrying on next upstream", "path", req.URL.Path, "failed", tries[i-1].ID, "err", last.Err, "status", last.Status)
			reset()
		}
		last = r.forward(w, req, u, class, nil)
		if !last.Failed() || last.HeadersSent || req.Context().Err() != nil {
			return
		}
	}
	if replay {
		if ext, ok := r.externalFallback(req.Context(), class, tries); ok {
			r.metric.Inc("loadb_retries")
			r.log.Debug("retrying on external", "path", req.URL.Path, "failed", tries[len(tries)-1].ID, "external", ext.ID)
			reset()
			last = r.forward(w, req, ext, class, nil)
			if !last.Failed() || last.HeadersSent || req.Context().Err() != nil {
				return
			}
		}
	}
	r.answerFailure(w, req, last)
}

// externalFallback is the last resort once every upstream tried has failed
// without answering the client: the externals, which were not candidates
// while the mesh looked able to serve, are checked and the best of them is
// returned. There is none when an external was already tried, or when no
// external can serve the request.
func (r *Router) externalFallback(ctx context.Context, class domain.RequestClass, tried []domain.Upstream) (domain.Upstream, bool) {
	if !mayUseExternals(class) {
		return domain.Upstream{}, false
	}
	for _, u := range tried {
		if u.Kind == domain.KindExternal {
			return domain.Upstream{}, false
		}
	}
	if !r.dir.checkExternals(ctx) {
		return domain.Upstream{}, false
	}
	cands, best := r.dir.Snapshot()
	var ext []domain.Upstream
	for _, u := range cands {
		if u.Kind == domain.KindExternal {
			ext = append(ext, u)
		}
	}
	sel, ok := r.sel(ext, class, best)
	return sel.Chosen, ok
}

func (r *Router) answerFailure(w http.ResponseWriter, req *http.Request, out ports.Outcome) {
	if out.HeadersSent {
		return
	}
	switch {
	case req.Context().Err() != nil:
		return
	case out.Err != nil && errors.Is(out.Err, context.DeadlineExceeded):
		writeMessage(w, 504, "algod-loadb-mesh: upstream timeout")
	case out.Status >= 500 || out.Status == 429:
		writeMessage(w, 502, fmt.Sprintf("algod-loadb-mesh: upstream answered %d", out.Status))
	default:
		writeMessage(w, 502, "algod-loadb-mesh: upstream unreachable")
	}
}

// handleWait coalesces wait-for-block-after on the local monitor whenever the
// local node may serve it, and on heartbeats on a balancer; otherwise it is
// held until a peer reports the round, then forwarded to that peer.
func (r *Router) handleWait(w http.ResponseWriter, req *http.Request, class domain.RequestClass, cands []domain.Upstream, best uint64) {
	if r.opts.Balancer {
		r.handleBalancerWait(w, req, class, cands, best)
		return
	}
	if len(cands) > 0 && cands[0].Kind == domain.KindLocal && domain.Eligible(cands[0], class, best, r.opts.SyncTolerance) {
		local := cands[0]
		ctx, cancel := context.WithTimeout(req.Context(), r.opts.WaitTimeout)
		defer cancel()
		r.metric.Inc("loadb_wait_coalesced")
		body, err := r.monitor.WaitForBlockAfter(ctx, *class.WaitAfter)
		if req.Context().Err() != nil {
			return
		}
		if body == nil {
			writeMessage(w, 503, "algod-loadb-mesh: local node status unavailable")
			return
		}
		_ = err // on timeout algod itself answers with the current status; so do we
		w.Header().Set("X-Algod-Loadb-Mesh-Upstream", local.ID)
		w.Header().Set("X-Algod-Loadb-Mesh-Tier", fmt.Sprint(local.Tier))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write(body)
		return
	}
	r.handlePeerWait(w, req, class, cands, best)
}

// handlePeerWait serves a wait-for-block-after the local node cannot: it holds
// the request until a heartbeat reports a round past the caller's, then
// forwards it to that peer, whose algod answers at once. Forwarding straight
// away would let the peer answer before its heartbeat reached this directory,
// and the client's next request, for the block the answer announced, would
// find no upstream at that round. The local node's own progress wakes the
// hold too: when it was behind the caller's round and catches up, the request
// goes to it. When the mesh does not pass the round within the wait timeout
// the request is answered with a node's current status, as algod's own
// timeout does. When no mesh node could serve the request once at the round,
// there is nothing to wait for and it is forwarded at once.
func (r *Router) handlePeerWait(w http.ResponseWriter, req *http.Request, class domain.RequestClass, cands []domain.Upstream, best uint64) {
	if !r.worthWaiting(cands, class, best) {
		r.handleDefault(w, req, class, cands, best)
		return
	}
	after := *class.WaitAfter
	ctx, cancel := context.WithTimeout(req.Context(), r.opts.WaitTimeout)
	defer cancel()
	if r.dir.HeartbeatRound() <= after {
		r.metric.Inc("loadb_wait_held")
	}
	round, err := r.dir.WaitForHeartbeatRound(ctx, after)
	if req.Context().Err() != nil {
		return
	}
	cands, best = r.dir.Snapshot()
	if err == nil {
		var past []domain.Upstream
		for _, u := range cands {
			if u.Kind != domain.KindExternal && u.LastRound > after {
				past = append(past, u)
			}
		}
		if sel, ok := r.sel(past, class, best); ok {
			r.forwardSelected(w, req, class, sel)
			return
		}
		// The node that reported the round is not eligible after all.
		r.handleDefault(w, req, class, cands, best)
		return
	}
	f := r.statusAt(round)
	if f.err != nil {
		r.log.Debug("held wait timed out with no status, forwarding", "after", after, "err", f.err)
		r.handleDefault(w, req, class, cands, best)
		return
	}
	w.Header().Set("X-Algod-Loadb-Mesh-Upstream", f.upstream.ID)
	w.Header().Set("X-Algod-Loadb-Mesh-Tier", fmt.Sprint(f.upstream.Tier))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	_, _ = w.Write(f.body)
}

// worthWaiting is true when some mesh node could serve the request once it
// reaches the round: eligible for it in every respect but the round itself.
// A lagging or unusable peer is not waited for; the request is forwarded (or
// refused) at once, as it was before the hold existed.
func (r *Router) worthWaiting(cands []domain.Upstream, class domain.RequestClass, best uint64) bool {
	class.WaitAfter = nil
	for _, u := range cands {
		if u.Kind != domain.KindExternal && domain.Eligible(u, class, best, r.opts.SyncTolerance) {
			return true
		}
	}
	return false
}

// handleBalancerWait holds the request until a heartbeat reports a round past
// the requested one (or the wait times out, as algod's own does), then answers
// with the status of a node at that round. The status is fetched once for all
// waiters, so any number of them cost one algod request per round.
func (r *Router) handleBalancerWait(w http.ResponseWriter, req *http.Request, class domain.RequestClass, cands []domain.Upstream, best uint64) {
	ctx, cancel := context.WithTimeout(req.Context(), r.opts.WaitTimeout)
	defer cancel()
	r.metric.Inc("loadb_wait_coalesced")
	round, _ := r.dir.WaitForHeartbeatRound(ctx, *class.WaitAfter)
	if req.Context().Err() != nil {
		return
	}
	f := r.statusAt(round)
	if f.err != nil {
		// No node known at that round answered: an ordinary forward.
		r.log.Debug("coalesced status unavailable, forwarding", "round", round, "err", f.err)
		cands, best = r.dir.Snapshot()
		r.handleDefault(w, req, class, cands, best)
		return
	}
	w.Header().Set("X-Algod-Loadb-Mesh-Upstream", f.upstream.ID)
	w.Header().Set("X-Algod-Loadb-Mesh-Tier", fmt.Sprint(f.upstream.Tier))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	_, _ = w.Write(f.body)
}

// statusAt returns a status body at round or later: the last one when it is
// recent enough, else the result of a fetch shared with concurrent callers.
func (r *Router) statusAt(round uint64) *statusFetch {
	r.statusMu.Lock()
	if last := r.status; last != nil && last.round >= round {
		r.statusMu.Unlock()
		return last
	}
	f := r.statusFetch
	if f == nil || f.want < round {
		f = &statusFetch{done: make(chan struct{}), want: round}
		r.statusFetch = f
		go r.fetchStatus(f)
	}
	r.statusMu.Unlock()
	<-f.done
	return f
}

// fetchStatus asks eligible nodes at f.round or later for /v2/status, best
// first, within the retry budget.
func (r *Router) fetchStatus(f *statusFetch) {
	defer close(f.done)
	want := f.want
	class := domain.Classify(http.MethodGet, "/v2/status")
	cands, best := r.dir.Snapshot()
	at := cands[:0:0]
	for _, u := range cands {
		if u.Kind != domain.KindExternal && u.LastRound >= want {
			at = append(at, u)
		}
	}
	f.err = errors.New("no eligible node at that round")
	if sel, ok := r.sel(at, class, best); ok {
		tries := append([]domain.Upstream{sel.Chosen}, sel.Alternates...)
		if b := r.opts.RetryBudget; b >= 0 && len(tries) > 1+b {
			tries = tries[:1+b]
		}
		for _, u := range tries {
			if f.round, f.body, f.err = r.getStatus(u, class); f.err == nil {
				f.upstream = u
				break
			}
		}
	}
	r.statusMu.Lock()
	defer r.statusMu.Unlock()
	if r.statusFetch == f {
		r.statusFetch = nil
	}
	if f.err == nil && (r.status == nil || f.round > r.status.round) {
		r.status = f
	}
}

func (r *Router) getStatus(u domain.Upstream, class domain.RequestClass) (uint64, []byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), r.opts.UpstreamTimeout)
	defer cancel()
	rq, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(u.BaseURL, "/")+"/v2/status", nil)
	if err != nil {
		return 0, nil, err
	}
	rq.Header.Set("X-Algo-API-Token", u.Token)
	start := r.clock.Now()
	release := r.stats.Begin(u.ID)
	resp, err := r.httpc.Do(rq)
	release()
	out := ports.Outcome{Err: err}
	var body []byte
	if err == nil {
		out.Status = resp.StatusCode
		body, err = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if err == nil && resp.StatusCode != 200 {
			err = fmt.Errorf("%s answered %d", u.ID, resp.StatusCode)
		}
	}
	out.Duration = r.clock.Now().Sub(start)
	r.stats.Record(u.ID, class.StatKey(), out)
	r.metric.Inc("loadb_upstream_requests", "upstream", u.ID, "class", class.StatKey(), "failed", boolStr(out.Failed() || err != nil))
	if err != nil {
		return 0, nil, err
	}
	var st struct {
		LastRound uint64 `json:"last-round"`
	}
	if err := json.Unmarshal(body, &st); err != nil {
		return 0, nil, fmt.Errorf("%s status: %w", u.ID, err)
	}
	return st.LastRound, body, nil
}

func (r *Router) rememberTxn(txid, upstream string) {
	r.pins.remember(txid, upstream, r.clock.Now())
}

func (r *Router) recallTxn(txid string) (string, bool) {
	return r.pins.recall(txid, r.clock.Now())
}

// handleBroadcast sends POST /v2/transactions to the eligible non-follower
// nodes in turn, until one answers (a rejection is an answer), or, with
// multi_broadcast, to all of them at once, answering with the first success.
// Either way the externals follow when every node failed to answer.
func (r *Router) handleBroadcast(w http.ResponseWriter, req *http.Request, class domain.RequestClass, cands []domain.Upstream, best uint64) {
	sel, cands, best, ok := r.pick(req.Context(), class, cands, best)
	if !ok {
		r.noUpstream(w, class, cands, best)
		return
	}
	if !r.opts.MultiBroadcast {
		cw := &captureWriter{ResponseWriter: w, limit: 8 << 10}
		r.forwardSelected(cw, req, class, sel)
		if cw.status == 200 {
			r.rememberTxn(txIDFrom(cw.buf.Bytes()), cw.Header().Get("X-Algod-Loadb-Mesh-Upstream"))
		}
		return
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, 4<<20))
	if err != nil {
		writeMessage(w, 400, "algod-loadb-mesh: cannot read body")
		return
	}
	targets := dedupeByURL(meshFirst(append([]domain.Upstream{sel.Chosen}, sel.Alternates...)))
	ctx, cancel := context.WithTimeout(req.Context(), r.opts.UpstreamTimeout)
	defer cancel()
	results := r.fanOut(ctx, req, body, targets, class)
	var fallback *fanResult
	unanswered := 0
	for range targets {
		res := <-results
		if res.err == nil && res.resp.StatusCode == 200 {
			r.metric.Inc("loadb_multibroadcast", "outcome", "ok")
			r.rememberTxn(txIDFrom(res.body), res.u.ID)
			cancel() // other broadcasts are best effort
			writeFan(w, res)
			return
		}
		if res.err != nil || withheldStatus(res.resp.StatusCode) {
			unanswered++
		}
		// An answer (a rejection, say) outranks a withheld status, which
		// outranks a transport error.
		if fallback == nil || fanRank(res) > fanRank(*fallback) {
			res := res
			fallback = &res
		}
	}
	r.metric.Inc("loadb_multibroadcast", "outcome", "failed")
	if unanswered == len(targets) {
		// No node answered at all: the externals are the last resort.
		if ext, ok := r.externalFallback(req.Context(), class, targets); ok {
			r.metric.Inc("loadb_retries")
			r.log.Debug("retrying on external", "path", req.URL.Path, "external", ext.ID)
			req.Body, req.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
			cw := &captureWriter{ResponseWriter: w, limit: 8 << 10}
			out := r.forward(cw, req, ext, class, nil)
			if !out.Failed() || out.HeadersSent || req.Context().Err() != nil {
				if cw.status == 200 {
					r.rememberTxn(txIDFrom(cw.buf.Bytes()), ext.ID)
				}
				return
			}
		}
	}
	if fallback.err != nil {
		writeMessage(w, 502, "algod-loadb-mesh: broadcast failed on every node: "+fallback.err.Error())
		return
	}
	writeFan(w, *fallback)
}

// fanRank orders a fanned-out request's failures for the answer to give:
// an upstream's own answer over a status the forwarder would withhold, over
// a transport error.
func fanRank(res fanResult) int {
	switch {
	case res.err != nil:
		return 0
	case withheldStatus(res.resp.StatusCode):
		return 1
	default:
		return 2
	}
}

// meshFirst drops the externals from a selection that holds a mesh node:
// they are the last resort, reached only when no mesh node answers. A
// selection of externals alone is returned as is.
func meshFirst(us []domain.Upstream) []domain.Upstream {
	mesh := us[:0:0]
	for _, u := range us {
		if u.Kind != domain.KindExternal {
			mesh = append(mesh, u)
		}
	}
	if len(mesh) == 0 {
		return us
	}
	return mesh
}

// withheldStatus is true for the upstream statuses the forwarder withholds
// from the client as failures of the upstream rather than answers.
func withheldStatus(code int) bool {
	return code == 502 || code == 503 || code == 504
}

// maxFanBody bounds one buffered upstream answer; pending lookups of
// confirmed txns carry logs and inner txns.
const maxFanBody = 16 << 20

// fanResult is one upstream's buffered answer to a fanned-out request.
type fanResult struct {
	u    domain.Upstream
	resp *http.Response // body already read into body and closed
	body []byte
	err  error
}

// fanOut sends the request to every target at once, buffering each answer.
// Results arrive in completion order; the channel holds all of them, so a
// caller may stop reading early.
func (r *Router) fanOut(ctx context.Context, req *http.Request, body []byte, targets []domain.Upstream, class domain.RequestClass) <-chan fanResult {
	results := make(chan fanResult, len(targets))
	for _, u := range targets {
		go func(u domain.Upstream) {
			start := r.clock.Now()
			rq, _ := http.NewRequestWithContext(ctx, req.Method, strings.TrimSuffix(u.BaseURL, "/")+req.URL.RequestURI(), bytes.NewReader(body))
			rq.Header.Set("X-Algo-API-Token", u.Token)
			if ct := req.Header.Get("Content-Type"); ct != "" {
				rq.Header.Set("Content-Type", ct)
			}
			release := r.stats.Begin(u.ID)
			resp, err := r.httpc.Do(rq)
			res := fanResult{u: u, resp: resp, err: err}
			if err == nil {
				res.body, res.err = io.ReadAll(io.LimitReader(resp.Body, maxFanBody))
				resp.Body.Close()
			}
			release()
			out := ports.Outcome{Err: res.err, Duration: r.clock.Now().Sub(start)}
			if resp != nil {
				out.Status = resp.StatusCode
			}
			if ctx.Err() == nil { // not cancelled by us or the client: the outcome is about the upstream
				r.stats.Record(u.ID, class.StatKey(), out)
			}
			r.metric.Inc("loadb_upstream_requests", "upstream", u.ID, "class", class.StatKey(), "failed", boolStr(out.Failed()))
			results <- res
		}(u)
	}
	return results
}

// writeFan relays a buffered upstream answer with its end-to-end headers.
func writeFan(w http.ResponseWriter, res fanResult) {
	for k, vs := range res.resp.Header {
		switch k {
		case "Connection", "Keep-Alive", "Transfer-Encoding", "Content-Length", "Trailer", "Upgrade":
			continue
		}
		w.Header()[k] = vs
	}
	w.Header().Set("X-Algod-Loadb-Mesh-Upstream", res.u.ID)
	w.Header().Set("X-Algod-Loadb-Mesh-Tier", fmt.Sprint(res.u.Tier))
	w.WriteHeader(res.resp.StatusCode)
	_, _ = w.Write(res.body)
}

func dedupeByURL(us []domain.Upstream) []domain.Upstream {
	seen := map[string]bool{}
	out := us[:0]
	for _, u := range us {
		if !seen[u.BaseURL] {
			seen[u.BaseURL] = true
			out = append(out, u)
		}
	}
	return out
}

func txIDFrom(body []byte) string {
	var v struct {
		TxID string `json:"txId"`
	}
	if json.Unmarshal(body, &v) != nil {
		return ""
	}
	return v.TxID
}

func sortByRoundDesc(us []domain.Upstream) {
	for i := 1; i < len(us); i++ {
		for j := i; j > 0 && us[j].LastRound > us[j-1].LastRound; j-- {
			us[j], us[j-1] = us[j-1], us[j]
		}
	}
}

// serveAgent answers /loadb/* from the agent itself.
func (r *Router) serveAgent(w http.ResponseWriter, req *http.Request) {
	cands, best := r.dir.Snapshot()
	switch strings.TrimSuffix(req.URL.Path, "/") {
	case "/loadb/health":
		if r.draining.Load() {
			writeMessage(w, 503, "draining")
			return
		}
		for _, u := range cands {
			if domain.Eligible(u, domain.RequestClass{}, best, r.opts.SyncTolerance) {
				writeJSON(w, 200, map[string]any{"status": "ok", "best_round": best})
				return
			}
		}
		writeMessage(w, 503, "no eligible upstream")
	case "/loadb/status":
		role := domain.RoleNode
		var local any = r.monitor.State()
		if r.opts.Balancer {
			role, local = domain.RoleBalancer, nil
		}
		status := map[string]any{
			"version": r.opts.Version, "role": role, "mode": r.opts.Mode, "draining": r.draining.Load(),
			"uptime_s": int(r.clock.Now().Sub(r.started).Seconds()), "inflight": r.inflight.Load(),
			"best_round": best, "local": local, "upstreams": cands, "balancers": r.dir.Balancers(), "links": r.dir.LinkSnapshot(),
		}
		if f := r.passthrough.Load(); f != nil {
			status["passthrough"] = (*f)()
		}
		writeJSON(w, 200, status)
	case "/loadb/peers":
		writeJSON(w, 200, cands)
	case "/loadb/metrics":
		if r.metrics == nil {
			writeMessage(w, 404, "metrics disabled")
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = r.metrics.WriteTo(w)
	default:
		writeMessage(w, 404, "unknown agent endpoint")
	}
}

// captureWriter tees the first `limit` bytes of a response for inspection.
type captureWriter struct {
	http.ResponseWriter
	buf    bytes.Buffer
	limit  int
	status int
}

func (c *captureWriter) WriteHeader(code int) {
	if c.status == 0 && code >= 200 {
		c.status = code
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *captureWriter) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	if c.buf.Len() < c.limit {
		n := c.limit - c.buf.Len()
		if n > len(b) {
			n = len(b)
		}
		c.buf.Write(b[:n])
	}
	return c.ResponseWriter.Write(b)
}

func (c *captureWriter) Flush() {
	if fl, ok := c.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

func (c *captureWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// statusWriter records the status and body size sent to the client.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (s *statusWriter) WriteHeader(code int) {
	if s.status == 0 && code >= 200 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += int64(n)
	return n, err
}

func (s *statusWriter) Flush() {
	if fl, ok := s.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }
