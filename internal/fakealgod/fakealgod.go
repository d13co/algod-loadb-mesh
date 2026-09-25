// Package fakealgod is an in-process HTTP simulator of the algod REST subset
// the agent uses. It is the keystone of the test suite and doubles as a local
// development fleet (algod-loadb-mesh dev).
package fakealgod

import (
	"crypto/sha512"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/d13co/algod-loadb-mesh/internal/ports"
)

// Options configures a fake node.
type Options struct {
	ID           string
	Token        string
	GenesisID    string
	StartRound   uint64
	OldestRound  uint64 // fixed; use SetOldest to move it
	DeveloperAPI bool
	FollowMode   bool
	Version      string
}

// poolTxn is what a pending lookup reports for one txid.
type poolTxn struct {
	txn       json.RawMessage
	poolError string
	confirmed uint64
}

// Node is one fake algod behind an httptest server.
type Node struct {
	opts Options
	srv  *httptest.Server

	mu        sync.Mutex
	round     uint64
	oldest    uint64
	changed   chan struct{}
	failing   bool
	rejecting bool // every broadcast answers 400, as algod does for a rejected transaction
	latency   time.Duration
	boxes     map[uint64]map[string][]byte
	pool      map[string]poolTxn
	hits      map[string]int
}

// New starts a fake node.
func New(o Options) *Node {
	if o.Token == "" {
		o.Token = "faketoken"
	}
	if o.GenesisID == "" {
		o.GenesisID = "fakenet-v1"
	}
	if o.Version == "" {
		o.Version = "3.99.0"
	}
	n := &Node{opts: o, round: o.StartRound, oldest: o.OldestRound, changed: make(chan struct{}),
		boxes: map[uint64]map[string][]byte{}, pool: map[string]poolTxn{}, hits: map[string]int{}}
	n.srv = httptest.NewServer(http.HandlerFunc(n.handle))
	return n
}

// URL is the base URL clients dial.
func (n *Node) URL() string { return n.srv.URL }

// Token is the API token the node expects.
func (n *Node) Token() string { return n.opts.Token }

// ID is the node's name, echoed in X-Fake-Node.
func (n *Node) ID() string { return n.opts.ID }

// Close stops the server.
func (n *Node) Close() { n.srv.Close() }

// Round returns the current last round.
func (n *Node) Round() uint64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.round
}

// SetRound sets the last round and wakes waiters.
func (n *Node) SetRound(r uint64) {
	n.mu.Lock()
	n.round = r
	close(n.changed)
	n.changed = make(chan struct{})
	n.mu.Unlock()
}

// Advance moves the chain forward by k rounds.
func (n *Node) Advance(k uint64) { n.SetRound(n.Round() + k) }

// SetOldest changes the oldest servable round.
func (n *Node) SetOldest(r uint64) {
	n.mu.Lock()
	n.oldest = r
	n.mu.Unlock()
}

// SetRejecting makes every broadcast answer 400 (true), as algod does for a
// transaction it rejects, or behave normally (false).
func (n *Node) SetRejecting(f bool) {
	n.mu.Lock()
	n.rejecting = f
	n.mu.Unlock()
}

// SetFailing makes every request answer 503 (true) or behave normally (false).
func (n *Node) SetFailing(f bool) {
	n.mu.Lock()
	n.failing = f
	n.mu.Unlock()
}

// SetLatency adds a fixed delay to every response.
func (n *Node) SetLatency(d time.Duration) {
	n.mu.Lock()
	n.latency = d
	n.mu.Unlock()
}

// Hits returns how many requests matched a path prefix, for assertions.
func (n *Node) Hits(prefix string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	total := 0
	for p, c := range n.hits {
		if strings.HasPrefix(p, prefix) {
			total += c
		}
	}
	return total
}

// SetPendingTxn makes pending lookups of id answer with a pool error and/or
// confirmed round, as if the node had seen the txn.
func (n *Node) SetPendingTxn(id, poolError string, confirmed uint64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.pool[id] = poolTxn{txn: json.RawMessage(`{}`), poolError: poolError, confirmed: confirmed}
}

// PutBox stores a box value for an application.
func (n *Node) PutBox(app uint64, name, value []byte) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.boxes[app] == nil {
		n.boxes[app] = map[string][]byte{}
	}
	n.boxes[app][string(name)] = append([]byte(nil), value...)
}

// DeleteBox removes a box.
func (n *Node) DeleteBox(app uint64, name []byte) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.boxes[app], string(name))
}

func (n *Node) handle(w http.ResponseWriter, r *http.Request) {
	n.mu.Lock()
	n.hits[r.URL.Path]++
	failing, latency, round, oldest := n.failing, n.latency, n.round, n.oldest
	n.mu.Unlock()
	w.Header().Set("X-Fake-Node", n.opts.ID)
	if latency > 0 {
		time.Sleep(latency)
	}
	if failing {
		writeJSON(w, 503, map[string]string{"message": "fake node is failing"})
		return
	}
	if r.Header.Get("X-Algo-API-Token") != n.opts.Token {
		writeJSON(w, 401, map[string]string{"message": "Invalid API Token"})
		return
	}
	p := strings.TrimSuffix(r.URL.Path, "/")
	segs := strings.Split(strings.TrimPrefix(p, "/"), "/")
	seg := func(i int) string {
		if i < len(segs) {
			return segs[i]
		}
		return ""
	}
	switch {
	case p == "/health" || p == "/ready":
		w.WriteHeader(200)
	case p == "/genesis":
		writeJSON(w, 200, map[string]any{"id": strings.TrimSuffix(n.opts.GenesisID, "-v1.0"), "network": n.opts.GenesisID})
	case p == "/versions":
		writeJSON(w, 200, map[string]any{"genesis_id": n.opts.GenesisID, "versions": []string{"v2"},
			"build": map[string]any{"major": 3, "minor": 99, "build_number": 0, "channel": "fake"}})
	case p == "/v2/status":
		writeJSON(w, 200, n.status(round))
	case seg(0) == "v2" && seg(1) == "status" && seg(2) == "wait-for-block-after":
		after, _ := strconv.ParseUint(seg(3), 10, 64)
		n.waitAfter(w, r, after)
	case seg(0) == "v2" && seg(1) == "blocks":
		rr, err := strconv.ParseUint(seg(2), 10, 64)
		if err != nil {
			writeJSON(w, 400, map[string]string{"message": "bad round"})
			return
		}
		if rr < oldest || rr > round {
			writeJSON(w, 404, map[string]string{"message": "failed to retrieve information from the ledger"})
			return
		}
		if seg(3) == "hash" {
			writeJSON(w, 200, map[string]string{"blockHash": fmt.Sprintf("hash-%d", rr)})
			return
		}
		writeJSON(w, 200, map[string]any{"block": map[string]any{"rnd": rr, "gen": n.opts.GenesisID}, "node": n.opts.ID})
	case seg(0) == "v2" && seg(1) == "teal":
		if !n.opts.DeveloperAPI {
			writeJSON(w, 404, map[string]string{"message": "developer API not enabled"})
			return
		}
		writeJSON(w, 200, map[string]any{"result": "#pragma version 8\nint 1", "node": n.opts.ID})
	case p == "/v2/transactions" && r.Method == "POST":
		n.mu.Lock()
		rejecting := n.rejecting
		n.mu.Unlock()
		if rejecting {
			writeJSON(w, 400, map[string]string{"message": "TransactionPool.Remember: transaction rejected"})
			return
		}
		if n.opts.FollowMode {
			writeJSON(w, 400, map[string]string{"message": "follow mode node does not broadcast"})
			return
		}
		var buf strings.Builder
		if _, err := copyN(&buf, r, 1<<20); err != nil {
			writeJSON(w, 400, map[string]string{"message": err.Error()})
			return
		}
		id := txID([]byte(buf.String()))
		n.mu.Lock()
		raw, _ := json.Marshal(map[string]any{"node": n.opts.ID, "size": buf.Len()})
		n.pool[id] = poolTxn{txn: raw}
		n.mu.Unlock()
		writeJSON(w, 200, map[string]string{"txId": id})
	case seg(0) == "v2" && seg(1) == "transactions" && seg(2) == "pending" && seg(3) != "":
		n.mu.Lock()
		tx, ok := n.pool[seg(3)]
		n.mu.Unlock()
		if !ok {
			writeJSON(w, 404, map[string]string{"message": "transaction not found"})
			return
		}
		body := map[string]any{"pool-error": tx.poolError, "txn": tx.txn, "node": n.opts.ID}
		if tx.confirmed > 0 {
			body["confirmed-round"] = tx.confirmed
		}
		writeJSON(w, 200, body)
	case seg(0) == "v2" && seg(1) == "transactions" && seg(2) == "pending":
		n.mu.Lock()
		total := len(n.pool)
		n.mu.Unlock()
		writeJSON(w, 200, map[string]any{"top-transactions": []any{}, "total-transactions": total, "node": n.opts.ID})
	case seg(0) == "v2" && seg(1) == "applications" && seg(3) == "boxes":
		app, _ := strconv.ParseUint(seg(2), 10, 64)
		n.mu.Lock()
		var names []map[string]string
		for k := range n.boxes[app] {
			names = append(names, map[string]string{"name": base64.StdEncoding.EncodeToString([]byte(k))})
		}
		n.mu.Unlock()
		if names == nil {
			names = []map[string]string{}
		}
		writeJSON(w, 200, map[string]any{"boxes": names})
	case seg(0) == "v2" && seg(1) == "applications" && seg(3) == "box":
		app, _ := strconv.ParseUint(seg(2), 10, 64)
		name, err := decodeBoxName(r.URL.Query().Get("name"))
		if err != nil {
			writeJSON(w, 400, map[string]string{"message": err.Error()})
			return
		}
		n.mu.Lock()
		v, ok := n.boxes[app][string(name)]
		n.mu.Unlock()
		if !ok {
			writeJSON(w, 404, map[string]string{"message": "box not found"})
			return
		}
		writeJSON(w, 200, map[string]any{"name": base64.StdEncoding.EncodeToString(name),
			"value": base64.StdEncoding.EncodeToString(v), "round": round})
	default:
		writeJSON(w, 200, map[string]any{"node": n.opts.ID, "path": p, "method": r.Method, "round": round})
	}
}

func (n *Node) status(round uint64) map[string]any {
	return map[string]any{"last-round": round, "time-since-last-round": 1000, "catchup-time": 0,
		"last-version": "fake", "next-version": "fake", "next-version-round": round + 1,
		"next-version-supported": true, "stopped-at-unsupported-round": false, "node": n.opts.ID}
}

func (n *Node) waitAfter(w http.ResponseWriter, r *http.Request, after uint64) {
	deadline := time.After(60 * time.Second)
	for {
		n.mu.Lock()
		round, ch := n.round, n.changed
		n.mu.Unlock()
		if round > after {
			writeJSON(w, 200, n.status(round))
			return
		}
		select {
		case <-ch:
		case <-deadline:
			writeJSON(w, 200, n.status(round))
			return
		case <-r.Context().Done():
			return
		}
	}
}

func decodeBoxName(q string) ([]byte, error) {
	switch {
	case strings.HasPrefix(q, "b64:"):
		return base64.StdEncoding.DecodeString(q[4:])
	case strings.HasPrefix(q, "str:"):
		return []byte(q[4:]), nil
	}
	return nil, fmt.Errorf("box name must be b64: or str: encoded")
}

func txID(body []byte) string {
	sum := sha512.Sum512_256(body)
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:])
}

func copyN(dst *strings.Builder, r *http.Request, max int64) (int64, error) {
	buf := make([]byte, 32*1024)
	var total int64
	for {
		k, err := r.Body.Read(buf)
		if k > 0 {
			total += int64(k)
			if total > max {
				return total, fmt.Errorf("body too large")
			}
			dst.Write(buf[:k])
		}
		if err != nil {
			if err.Error() == "EOF" {
				return total, nil
			}
			return total, err
		}
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// ConfigReader is a ports.NodeConfigReader describing a fake node.
type ConfigReader struct {
	Node         *Node
	Archival     bool
	Lookback     uint64
	DeveloperAPI bool
	FollowMode   bool
}

// Read implements ports.NodeConfigReader.
func (c ConfigReader) Read() (ports.NodeConfig, error) {
	return ports.NodeConfig{Endpoint: c.Node.URL(), NetAddr: strings.TrimPrefix(c.Node.URL(), "http://"), Token: c.Node.Token(), GenesisID: c.Node.opts.GenesisID,
		Archival: c.Archival, MaxBlockHistoryLookback: c.Lookback, EnableDeveloperAPI: c.DeveloperAPI,
		EnableFollowMode: c.FollowMode, MaxAcctLookback: 8, StorageEngine: "fake", RestWriteTimeoutSeconds: 120}, nil
}
