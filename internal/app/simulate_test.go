package app

import (
	"bytes"
	"crypto/ed25519"
	crand "crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/algorand/go-codec/codec"

	"github.com/d13co/algod-loadb-mesh/internal/adapters/algodhttp"
	"github.com/d13co/algod-loadb-mesh/internal/adapters/logging"
	"github.com/d13co/algod-loadb-mesh/internal/adapters/proxy"
	"github.com/d13co/algod-loadb-mesh/internal/domain"
	"github.com/d13co/algod-loadb-mesh/internal/fakealgod"
)

// simFleet is a router in front of two peers: b, a stock algod, and c, a
// dtalgorand with the given simulate limits (nil: stock as well).
type simFleet struct {
	r    *Router
	b, c *fakealgod.Node
	h    *harness
	cKey ed25519.PrivateKey
}

func newSimFleet(t *testing.T, cCaps domain.Capabilities) *simFleet {
	t.Helper()
	return newSimFleetWith(t, cCaps, RouterOptions{Mode: domain.ModeFallback, RetryBudget: 1})
}

func newSimFleetWith(t *testing.T, cCaps domain.Capabilities, o RouterOptions) *simFleet {
	t.Helper()
	return newSimFleetDir(t, cCaps, o, DirectoryOptions{})
}

func newSimFleetDir(t *testing.T, cCaps domain.Capabilities, o RouterOptions, do DirectoryOptions) *simFleet {
	t.Helper()
	h := newHarnessWith(t, do, algodhttp.Factory{})
	b := fakealgod.New(fakealgod.Options{ID: "b", StartRound: 100})
	t.Cleanup(b.Close)
	c := fakealgod.New(fakealgod.Options{ID: "c", StartRound: 100, Simulate: cCaps.Simulate, TimeTravel: cCaps.TimeTravel})
	t.Cleanup(c.Close)
	cPub, cKey, _ := ed25519.GenerateKey(crand.Reader)
	recB := h.peerRecord(addrA)
	recB.Endpoints, recB.Token = []string{b.URL()}, b.Token()
	recC := domain.NodeRecord{ID: "c", Network: "n", Agent: domain.AgentInfo{Addrs: []string{addrB}, PubKey: []byte(cPub)},
		Endpoints: []string{c.URL()}, Token: c.Token()}
	h.d.SetRecords([]domain.NodeRecord{recB, recC})
	r := NewRouter(o,
		h.d, h.d.monitor, proxy.New(nil), nil, h.d.stats, h.fc, logging.Nop{}, h.m, nil, nil)
	cCaps.MaxAcctLookback = 4
	h.inject(addrA, signedHeartbeat(t, h.peerKey, "b", 1, 100, domain.Capabilities{MaxAcctLookback: 4}))
	h.inject(addrB, signedHeartbeat(t, cKey, "c", 1, 100, cCaps))
	waitFor(t, 2*time.Second, func() bool {
		cands, _ := h.d.Snapshot()
		up := 0
		for _, u := range cands {
			if u.Health.Reachable() {
				up++
			}
		}
		return up == 2
	})
	return &simFleet{r: r, b: b, c: c, h: h, cKey: cKey}
}

func (f *simFleet) do(method, path string, body io.Reader) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	f.r.ServeHTTP(rec, httptest.NewRequest(method, path, body))
	return rec
}

func (f *simFleet) simulate(t *testing.T, body string) string {
	t.Helper()
	rec := f.do(http.MethodPost, "/v2/transactions/simulate", strings.NewReader(body))
	if rec.Code != 200 {
		t.Fatalf("simulate %s: %d %s", body, rec.Code, rec.Body.String())
	}
	return rec.Header().Get("X-Algod-Loadb-Mesh-Upstream")
}

var testLimits = domain.SimulateLimits{MaxTxnGroups: 16, MaxRounds: 8, MaxExtraOpcodeBudget: 1_000_000, ResourceLimitMultiplier: 2, MaxExecTraceBytes: 1 << 20, TimeLimitSeconds: 30}

// A simulate request that only dtalgorand can run reaches the dtalgorand
// node; a stock node would reject it. One that fits stock algod still goes
// where routing would send it anyway.
func TestSimulateRoutesForkRequestsToDtalgorand(t *testing.T) {
	f := newSimFleet(t, domain.Capabilities{Simulate: &testLimits})
	if up := f.simulate(t, `{"txn-groups":[{"txns":[]}]}`); up != "b" {
		t.Fatalf("stock request went to %q, want the first peer", up)
	}
	for _, body := range []string{
		`{"txn-groups":[{"txns":[]},{"txns":[]}]}`,
		`{"txn-groups":[{"txns":[]}],"rounds":[0,0]}`,
		`{"txn-groups":[{"txns":[]}],"extra-opcode-budget":500000}`,
		`{"txn-groups":[{"txns":[]}],"parallel-groups":true}`,
		`{"txn-groups":[{"txns":[]}],"protocol":"latest"}`,
	} {
		if up := f.simulate(t, body); up != "c" {
			t.Errorf("%s went to %q, want c", body, up)
		}
	}

	// The SDKs post msgpack; the groups are counted all the same.
	type group struct {
		Txns []any `codec:"txns"`
	}
	type simReq struct {
		TxnGroups []group `codec:"txn-groups"`
	}
	var buf bytes.Buffer
	if err := codec.NewEncoder(&buf, &codec.MsgpackHandle{}).Encode(simReq{TxnGroups: []group{{}, {}, {}}}); err != nil {
		t.Fatal(err)
	}
	rec := f.do(http.MethodPost, "/v2/transactions/simulate", &buf)
	if up := rec.Header().Get("X-Algod-Loadb-Mesh-Upstream"); rec.Code != 200 || up != "c" {
		t.Fatalf("msgpack three groups: %d upstream %q", rec.Code, up)
	}
	if n := f.b.Hits("/v2/transactions/simulate"); n != 1 {
		t.Fatalf("stock node saw %d simulations, want only the stock one", n)
	}
}

// When no node fits, the request is relaxed and sent on, so that a node
// answers it (with its own 400, on a real algod) rather than the router with
// a 503.
func TestSimulateRelaxedWhenNothingFits(t *testing.T) {
	f := newSimFleet(t, domain.Capabilities{Simulate: &testLimits})
	groups := strings.Repeat(`{"txns":[]},`, 20)
	if up := f.simulate(t, `{"txn-groups":[`+strings.TrimSuffix(groups, ",")+`]}`); up == "" {
		t.Fatal("no upstream for a request over every limit")
	}
	// Rounds beyond every node are dropped before the rest: the dtalgorand
	// node, which accepts rounds, reports the missing ones per round.
	if up := f.simulate(t, `{"txn-groups":[{"txns":[]}],"rounds":[50,5000]}`); up != "c" {
		t.Fatalf("rounds past every node went to %q, want c", up)
	}
	// A single round no node holds stays refused: a node without it would
	// answer the latest state or an error.
	rec := f.do(http.MethodPost, "/v2/transactions/simulate", strings.NewReader(`{"txn-groups":[],"round":5000}`))
	if rec.Code != 503 {
		t.Fatalf("future round: %d %s", rec.Code, rec.Body.String())
	}
}

// The rounds of a rounds list must all be on the node that runs it.
func TestSimulateRoundsNeedEveryRound(t *testing.T) {
	f := newSimFleet(t, domain.Capabilities{Simulate: &testLimits, TimeTravel: true})
	// b, a plain node at 100 with lookback 4, holds 96..100; only c the rest.
	if up := f.simulate(t, `{"txn-groups":[],"round":97}`); up != "b" {
		t.Fatalf("round in the state window went to %q, want b", up)
	}
	if up := f.simulate(t, `{"txn-groups":[],"rounds":[97,12]}`); up != "c" {
		t.Fatalf("rounds reaching past the window went to %q, want c", up)
	}
}

// The router answers the limits for the fleet: what a request sent to it can
// ask for, since it routes each request to a node that allows it.
func TestSimulateLimitsForFleet(t *testing.T) {
	f := newSimFleet(t, domain.Capabilities{Simulate: &testLimits, TimeTravel: true})
	rec := f.do(http.MethodGet, "/v2/transactions/simulate/limits", nil)
	if rec.Code != 200 {
		t.Fatalf("limits: %d %s", rec.Code, rec.Body.String())
	}
	var got domain.FleetSimulateLimitsReply
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := domain.FleetSimulateLimitsReply{MaxTxnGroups: 16, MaxRounds: 8, MaxExtraOpcodeBudget: 1_000_000, ResourceLimitMultiplier: 1, TimeTravel: true, MaxExecTraceBytes: 1 << 20, TimeLimitSeconds: 30}
	if got != want {
		t.Fatalf("limits %+v, want %+v", got, want)
	}
	if n := f.b.Hits("/v2/transactions/simulate/limits") + f.c.Hits("/v2/transactions/simulate/limits"); n != 0 {
		t.Fatalf("limits forwarded %d times", n)
	}
}

// A fleet of stock algods has no limits endpoint, and the mesh has none
// either: the request is forwarded and answered 404.
func TestSimulateLimitsForwardedWithoutDtalgorand(t *testing.T) {
	f := newSimFleet(t, domain.Capabilities{})
	if rec := f.do(http.MethodGet, "/v2/transactions/simulate/limits", nil); rec.Code != 404 {
		t.Fatalf("limits on a stock fleet: %d %s", rec.Code, rec.Body.String())
	}
}

// drainC has c announce that it is draining: up, but taking no requests.
func (f *simFleet) drainC(t *testing.T, caps domain.Capabilities) {
	t.Helper()
	caps.MaxAcctLookback = 4
	wire, err := domain.EncodeHeartbeat(f.cKey, domain.Heartbeat{NodeID: "c", Seq: 2, Online: true, LastRound: 100, Caps: caps, Draining: true})
	if err != nil {
		t.Fatal(err)
	}
	f.h.inject(addrB, wire)
	waitFor(t, 2*time.Second, func() bool {
		cands, _ := f.h.d.Snapshot()
		for _, u := range cands {
			if u.ID == "c" {
				return u.Draining
			}
		}
		return false
	})
}

// A fitting node that is only briefly out (draining here) gets the client a
// 503 to retry, not a stock node's rejection of what it cannot run.
func TestSimulateNotRelaxedWhileFittingNodeIsOut(t *testing.T) {
	caps := domain.Capabilities{Simulate: &testLimits}
	f := newSimFleet(t, caps)
	f.drainC(t, caps)
	rec := f.do(http.MethodPost, "/v2/transactions/simulate", strings.NewReader(`{"txn-groups":[{"txns":[]},{"txns":[]}]}`))
	if rec.Code != 503 {
		t.Fatalf("two groups while the dtalgorand node drains: %d %s", rec.Code, rec.Body.String())
	}
	if n := f.b.Hits("/v2/transactions/simulate"); n != 0 {
		t.Fatalf("stock node saw %d simulations", n)
	}
	// The limits are not the stock node's 404 either.
	if rec := f.do(http.MethodGet, "/v2/transactions/simulate/limits", nil); rec.Code != 503 {
		t.Fatalf("limits while the dtalgorand node drains: %d %s", rec.Code, rec.Body.String())
	}
}

// A simulation that times out is not run again on the next node.
func TestSimulateNotRetriedAfterTimeout(t *testing.T) {
	f := newSimFleetWith(t, domain.Capabilities{Simulate: &testLimits},
		RouterOptions{Mode: domain.ModeFallback, RetryBudget: 1, UpstreamTimeout: 200 * time.Millisecond, RequestTimeout: 2 * time.Second})
	f.b.SetLatency(time.Second) // past the upstream timeout, but answers in the end
	rec := f.do(http.MethodPost, "/v2/transactions/simulate", strings.NewReader(`{"txn-groups":[{"txns":[]}]}`))
	if rec.Code != 504 {
		t.Fatalf("timed-out simulation: %d %s", rec.Code, rec.Body.String())
	}
	if n := f.c.Hits("/v2/transactions/simulate"); n != 0 {
		t.Fatalf("timed-out simulation retried %d times", n)
	}
}

// With the only dtalgorand node excluded, the fleet answers as a stock one:
// the limits request is forwarded and gets algod's 404, and a time-travel
// request is told the fleet has no such node.
func TestExcludedDtalgorandIsAbsent(t *testing.T) {
	f := newSimFleetDir(t, domain.Capabilities{Simulate: &testLimits, TimeTravel: true},
		RouterOptions{Mode: domain.ModeFallback, RetryBudget: 1}, DirectoryOptions{Exclude: []string{"c"}})
	if rec := f.do(http.MethodGet, "/v2/transactions/simulate/limits", nil); rec.Code != 404 {
		t.Fatalf("limits with the dtalgorand node excluded: %d %s", rec.Code, rec.Body.String())
	}
	if n := f.c.Hits("/v2/transactions/simulate/limits"); n != 0 {
		t.Fatalf("excluded node hit %d times", n)
	}
	cands, _ := f.h.d.Snapshot()
	if anyTimeTravel(cands) {
		t.Fatal("excluded time-travel node counted")
	}
}

// A busy excluded node does not make the router tell a heavy request that
// every node able to serve it is busy: no retry would reach that node.
func TestExcludedBusyNodeIsNotReported(t *testing.T) {
	busy := domain.Upstream{ID: "x", Stats: domain.Stats{Busy: true}}
	if !anyBusy([]domain.Upstream{busy}) {
		t.Fatal("busy node not counted")
	}
	busy.Excluded = true
	if anyBusy([]domain.Upstream{busy}) {
		t.Fatal("busy excluded node counted")
	}
}
