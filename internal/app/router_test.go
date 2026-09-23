package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/d13co/algod-loadb-mesh/internal/adapters/logging"
	"github.com/d13co/algod-loadb-mesh/internal/adapters/proxy"
	"github.com/d13co/algod-loadb-mesh/internal/domain"
	"github.com/d13co/algod-loadb-mesh/internal/fakealgod"
)

// waitHarness is a directory whose local node is absent, one peer backed by
// a fake algod, and a fallback-mode router in front of them.
type waitHarness struct {
	*harness
	n *fakealgod.Node
	r *Router
}

func newWaitHarness(t *testing.T, waitTimeout time.Duration) *waitHarness {
	t.Helper()
	h := newHarness(t, DirectoryOptions{})
	n := fakealgod.New(fakealgod.Options{ID: "b", StartRound: 10})
	t.Cleanup(n.Close)
	rec := h.peerRecord(addrA)
	rec.Endpoints, rec.Token = []string{n.URL()}, n.Token()
	h.d.SetRecords([]domain.NodeRecord{rec})
	r := NewRouter(RouterOptions{Mode: domain.ModeFallback, WaitTimeout: waitTimeout, RetryBudget: 1},
		h.d, h.d.monitor, proxy.New(nil), nil, h.d.stats, h.fc, logging.Nop{}, h.m, nil, nil)
	return &waitHarness{harness: h, n: n, r: r}
}

// heartbeat delivers the peer's heartbeat at round and waits until the
// directory shows it.
func (h *waitHarness) heartbeat(seq, round uint64) {
	h.t.Helper()
	wire, _ := domain.EncodeHeartbeat(h.peerKey, domain.Heartbeat{NodeID: "b", Seq: seq, Online: true, LastRound: round})
	h.inject(addrA, wire)
	waitFor(h.t, 2*time.Second, func() bool {
		cands, _ := h.d.Snapshot()
		for _, u := range cands {
			if u.ID == "b" && u.LastRound == round && u.Health.Reachable() {
				return true
			}
		}
		return false
	})
}

func (h *waitHarness) get(path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func lastRound(t *testing.T, rec *httptest.ResponseRecorder) uint64 {
	t.Helper()
	var st struct {
		LastRound uint64 `json:"last-round"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("status body %q: %v", rec.Body.String(), err)
	}
	return st.LastRound
}

func TestPeerWaitHoldsUntilHeartbeatThenForwards(t *testing.T) {
	h := newWaitHarness(t, 5*time.Second)
	h.heartbeat(1, 10)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- h.get("/v2/status/wait-for-block-after/10") }()

	// The peer's algod commits the round before its heartbeat arrives: the
	// request stays held rather than being answered from a round the
	// directory has not seen.
	time.Sleep(50 * time.Millisecond)
	h.n.Advance(1)
	select {
	case rec := <-done:
		t.Fatalf("answered before any heartbeat: %d %s", rec.Code, rec.Body.String())
	case <-time.After(150 * time.Millisecond):
	}
	if hits := h.n.Hits("/v2/status/wait-for-block-after"); hits != 0 {
		t.Fatalf("forwarded before the heartbeat: %d hits", hits)
	}

	h.heartbeat(2, 11)
	var rec *httptest.ResponseRecorder
	select {
	case rec = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("still held after the heartbeat")
	}
	if rec.Code != 200 || rec.Header().Get("X-Algod-Loadb-Mesh-Upstream") != "b" || lastRound(t, rec) != 11 {
		t.Fatalf("wait answer: %d upstream=%q body=%s", rec.Code, rec.Header().Get("X-Algod-Loadb-Mesh-Upstream"), rec.Body.String())
	}
	if hits := h.n.Hits("/v2/status/wait-for-block-after"); hits != 1 {
		t.Fatalf("wait forwarded %d times", hits)
	}

	// The client's next request, for the block the answer announced, finds
	// the peer at that round.
	if rec := h.get("/v2/blocks/11"); rec.Code != 200 || rec.Header().Get("X-Algod-Loadb-Mesh-Upstream") != "b" {
		t.Fatalf("block after the wait: %d %s", rec.Code, rec.Body.String())
	}
}

func TestPeerWaitPastRoundForwardsAtOnce(t *testing.T) {
	h := newWaitHarness(t, 5*time.Second)
	h.n.Advance(2)
	h.heartbeat(1, 12)
	start := time.Now()
	rec := h.get("/v2/status/wait-for-block-after/10")
	if rec.Code != 200 || lastRound(t, rec) != 12 || time.Since(start) > time.Second {
		t.Fatalf("wait for a past round: %d %s after %s", rec.Code, rec.Body.String(), time.Since(start))
	}
}

func TestPeerWaitTimeoutAnswersWithStatus(t *testing.T) {
	h := newWaitHarness(t, 300*time.Millisecond)
	h.heartbeat(1, 10)
	rec := h.get("/v2/status/wait-for-block-after/10")
	if rec.Code != 200 || rec.Header().Get("X-Algod-Loadb-Mesh-Upstream") != "b" || lastRound(t, rec) != 10 {
		t.Fatalf("timed-out wait: %d upstream=%q body=%s", rec.Code, rec.Header().Get("X-Algod-Loadb-Mesh-Upstream"), rec.Body.String())
	}
	if hits := h.n.Hits("/v2/status/wait-for-block-after"); hits != 0 {
		t.Fatalf("timed-out wait was forwarded %d times", hits)
	}
	if hits := h.n.Hits("/v2/status"); hits != 1 {
		t.Fatalf("status fetched %d times", hits)
	}
}

func TestPeerWaitWithoutReachablePeerIsNotHeld(t *testing.T) {
	h := newWaitHarness(t, 5*time.Second)
	// No heartbeat yet: the peer is unknown, and nothing is worth waiting for.
	start := time.Now()
	rec := h.get("/v2/status/wait-for-block-after/10")
	if rec.Code != 503 || time.Since(start) > time.Second {
		t.Fatalf("wait with no peer: %d %s after %s", rec.Code, rec.Body.String(), time.Since(start))
	}
}
