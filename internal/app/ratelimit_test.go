package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/d13co/algod-loadb-mesh/internal/adapters/clock"
	"github.com/d13co/algod-loadb-mesh/internal/adapters/logging"
	"github.com/d13co/algod-loadb-mesh/internal/adapters/metrics"
	"github.com/d13co/algod-loadb-mesh/internal/domain"
)

// testWeights makes every request cost one credit.
var testWeights = func() domain.CostWeights {
	w := domain.CostWeights{}
	for _, k := range domain.CostKeys {
		w[k] = 1
	}
	return w
}()

func perMinute(n float64) domain.Window { return domain.Window{Capacity: n, Period: time.Minute} }
func perDay(n float64) domain.Window    { return domain.Window{Capacity: n, Period: 24 * time.Hour} }

// newLimitedFleet is a simFleet whose router limits clients with plans:
// anonymous clients under "free", and the token "acme-token" under "pro".
func newLimitedFleet(t *testing.T, free, pro domain.Plan, perSecond float64) *simFleet {
	t.Helper()
	return newSimFleetWith(t, domain.Capabilities{Simulate: &testLimits}, RouterOptions{Mode: domain.ModeFallback, RetryBudget: 1,
		AdminToken: "admin-token", RateLimit: &LimiterOptions{
			Plans:  map[string]domain.Plan{"free": free, "pro": pro},
			Tokens: []LimitToken{{Name: "acme", Token: "acme-token", Plan: "pro"}}, AnonymousPlan: "free",
			Weights: testWeights, SimulatePerSecond: perSecond}})
}

// call sends a request from addr with token (none when "").
func (f *simFleet) call(method, path, token, addr string, body string) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	req.RemoteAddr = addr
	if token != "" {
		req.Header.Set("X-Algo-API-Token", token)
	}
	rec := httptest.NewRecorder()
	f.r.ServeHTTP(rec, req)
	return rec
}

const clientA, clientB = "192.0.2.1:5000", "192.0.2.2:5000"

// Anonymous clients each get the per-minute allowance, refilled steadily;
// past it they get 429 with the wait, and the RateLimit headers tell them
// where they stand.
func TestRateLimitPerMinute(t *testing.T) {
	f := newLimitedFleet(t, domain.Plan{Minute: perMinute(3)}, domain.Plan{}, 0)
	for i := range 3 {
		rec := f.call("GET", "/v2/status", "", clientA, "")
		if rec.Code != 200 {
			t.Fatalf("request %d: %d %s", i, rec.Code, rec.Body.String())
		}
		if got, want := rec.Header().Get("RateLimit"), `"minute";r=`+string(rune('2'-i))+`;t=`; !strings.HasPrefix(got, want) {
			t.Fatalf("request %d: RateLimit %q, want %s...", i, got, want)
		}
		if rec.Header().Get(costHeader) != "1" || rec.Header().Get("RateLimit-Policy") != `"minute";q=3;w=60` {
			t.Fatalf("request %d: cost %q, policy %q", i, rec.Header().Get(costHeader), rec.Header().Get("RateLimit-Policy"))
		}
	}
	rec := f.call("GET", "/v2/status", "", clientA, "")
	if rec.Code != 429 || rec.Header().Get("Retry-After") != "20" {
		t.Fatalf("past the allowance: %d, Retry-After %q: %s", rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
	}
	// Another address has an allowance of its own.
	if rec := f.call("GET", "/v2/status", "", clientB, ""); rec.Code != 200 {
		t.Fatalf("second client: %d", rec.Code)
	}
}

// The daily allowance holds even when the per-minute one has room.
func TestRateLimitPerDay(t *testing.T) {
	f := newLimitedFleet(t, domain.Plan{Minute: perMinute(100), Day: perDay(2)}, domain.Plan{}, 0)
	for range 2 {
		if rec := f.call("GET", "/v2/status", "", clientA, ""); rec.Code != 200 {
			t.Fatalf("%d", rec.Code)
		}
	}
	rec := f.call("GET", "/v2/status", "", clientA, "")
	if rec.Code != 429 || rec.Header().Get("Retry-After") != "43200" || !strings.Contains(rec.Body.String(), "24-hour") {
		t.Fatalf("past the daily allowance: %d, Retry-After %q: %s", rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
	}
}

// A configured token is limited by its own plan, apart from its address; any
// other token is refused; the admin token and the agent's own reads are not
// limited at all.
func TestRateLimitTokens(t *testing.T) {
	f := newLimitedFleet(t, domain.Plan{Minute: perMinute(1)}, domain.Plan{Minute: perMinute(2)}, 0)
	if rec := f.call("GET", "/v2/status", "", clientA, ""); rec.Code != 200 {
		t.Fatalf("anonymous: %d", rec.Code)
	}
	if rec := f.call("GET", "/v2/status", "", clientA, ""); rec.Code != 429 {
		t.Fatalf("anonymous, second: %d", rec.Code)
	}
	for i := range 2 {
		if rec := f.call("GET", "/v2/status", "acme-token", clientA, ""); rec.Code != 200 {
			t.Fatalf("token request %d: %d", i, rec.Code)
		}
	}
	if rec := f.call("GET", "/v2/status", "acme-token", clientB, ""); rec.Code != 429 {
		t.Fatalf("token from another address shares the token's allowance: %d", rec.Code)
	}
	if rec := f.call("GET", "/v2/status", "typo-token", clientB, ""); rec.Code != 401 {
		t.Fatalf("unknown token: %d", rec.Code)
	}
	for range 3 {
		if rec := f.call("GET", "/v2/status", "admin-token", clientA, ""); rec.Code != 200 || rec.Header().Get("RateLimit") != "" {
			t.Fatalf("admin token: %d, RateLimit %q", rec.Code, rec.Header().Get("RateLimit"))
		}
		if rec := f.call("GET", "/v2/status", "", loopbackAddr, ""); rec.Code != 200 {
			t.Fatalf("loopback: %d", rec.Code)
		}
	}
	if rec := f.call("GET", "/loadb/status", "acme-token", clientA, ""); rec.Code != 401 {
		t.Fatalf("/loadb with a client token: %d", rec.Code)
	}
}

// With rate limits on, the agent's endpoints stay closed without an admin
// token: the endpoint is public.
func TestRateLimitKeepsAgentEndpointsClosed(t *testing.T) {
	f := newSimFleetWith(t, domain.Capabilities{}, RouterOptions{Mode: domain.ModeFallback, RetryBudget: 1, RateLimit: &LimiterOptions{
		Plans: map[string]domain.Plan{"free": {}}, AnonymousPlan: "free"}})
	if rec := f.call("GET", "/loadb/status", "", clientA, ""); rec.Code != 403 {
		t.Fatalf("/loadb/status: %d", rec.Code)
	}
	if rec := f.call("GET", "/loadb/health", "", clientA, ""); rec.Code != 200 {
		t.Fatalf("/loadb/health: %d", rec.Code)
	}
}

// A request that costs nothing goes through even while its client is in debt.
func TestRateLimitFreeRequestInDebt(t *testing.T) {
	fc := clock.NewFake(time.Unix(1000, 0))
	w := domain.CostWeights{}
	for k, v := range testWeights {
		w[k] = v
	}
	w["default"] = 0
	l := NewLimiter(LimiterOptions{Plans: map[string]domain.Plan{"free": {Minute: perMinute(1)}}, AnonymousPlan: "free", Weights: w},
		fc, logging.Nop{}, metrics.New())
	id := Identity{Key: "ip:x", Plan: "free"}
	l.keys["ip:x"] = &limitKey{id: "ip:x", plan: l.plans["free"], minute: domain.Bucket{Level: -5, At: fc.Now()}, flights: map[*Ticket]struct{}{}}
	if _, d := l.Admit(id, domain.Classify("GET", "/v2/status", ""), 0); d != nil {
		t.Fatalf("free request in debt: %d %s", d.Status, d.Message)
	}
	if _, d := l.Admit(id, domain.Classify("GET", "/v2/blocks/5", ""), 0); d == nil || d.Status != 429 {
		t.Fatal("paid request in debt admitted")
	}
}

// Without an anonymous plan, a request without a token is refused.
func TestRateLimitNoAnonymousPlan(t *testing.T) {
	f := newSimFleetWith(t, domain.Capabilities{}, RouterOptions{Mode: domain.ModeFallback, RetryBudget: 1, RateLimit: &LimiterOptions{
		Plans: map[string]domain.Plan{"pro": {}}, Tokens: []LimitToken{{Name: "acme", Token: "acme-token", Plan: "pro"}}}})
	if rec := f.call("GET", "/v2/status", "", clientA, ""); rec.Code != 401 {
		t.Fatalf("no token: %d", rec.Code)
	}
	if rec := f.call("GET", "/v2/status", "acme-token", clientA, ""); rec.Code != 200 {
		t.Fatalf("token: %d", rec.Code)
	}
}

// A simulate request beyond the plan's caps, or costing more than a whole
// allowance, gets 403: retrying would not help. It costs its weight per
// group and round.
func TestRateLimitSimulateCaps(t *testing.T) {
	free := domain.Plan{Minute: perMinute(10), Simulate: domain.SimulateCaps{MaxGroups: 4, DenyExecTrace: true}}
	f := newLimitedFleet(t, free, domain.Plan{}, 0)
	for _, body := range []string{
		`{"txn-groups":[{},{},{},{},{}]}`,
		`{"txn-groups":[{}],"exec-trace-config":{"enable":true}}`,
		`{"txn-groups":[{},{}],"rounds":[1,2,3,4,5,6]}`, // 12 credits, over the 10 a minute
	} {
		rec := f.call("POST", "/v2/transactions/simulate", "", clientA, body)
		if rec.Code != 403 {
			t.Errorf("%s: %d %s", body, rec.Code, rec.Body.String())
		}
	}
	if rec := f.call("POST", "/v2/transactions/simulate", "", clientA, `not a simulate request`); rec.Code != 400 {
		t.Errorf("unreadable body under caps: %d %s", rec.Code, rec.Body.String())
	}
	rec := f.call("POST", "/v2/transactions/simulate", "", clientA, `{"txn-groups":[{},{}],"rounds":[1,2]}`)
	if rec.Code != 200 || rec.Header().Get(costHeader) != "4" {
		t.Fatalf("within the caps: %d, cost %q: %s", rec.Code, rec.Header().Get(costHeader), rec.Body.String())
	}
	// The unreadable request above was charged: 10 - 1 - 4 left.
	if got := rec.Header().Get("RateLimit"); !strings.HasPrefix(got, `"minute";r=5;`) {
		t.Fatalf("RateLimit %q, want 5 left", got)
	}
}

// A simulation is metered while it runs; once what it ran up overdraws its
// client, it is cancelled, the node sees the client go, and the client gets
// 429. It is not run again elsewhere, and the node is not blamed.
func TestRateLimitCancelsSimulation(t *testing.T) {
	f := newLimitedFleet(t, domain.Plan{Minute: perMinute(10)}, domain.Plan{}, 100)
	f.b.SetSimulate(10*time.Second, 0, 0)
	done := make(chan *httptest.ResponseRecorder)
	go func() { done <- f.call("POST", "/v2/transactions/simulate", "", clientA, `{"txn-groups":[{}]}`) }()
	waitFor(t, 2*time.Second, func() bool { return f.b.Hits("/v2/transactions/simulate") == 1 })
	l := f.r.Limiter()
	l.tick() // no time has passed: nothing run up yet
	select {
	case rec := <-done:
		t.Fatalf("cancelled too early: %d", rec.Code)
	case <-time.After(50 * time.Millisecond):
	}
	f.h.fc.Advance(time.Second) // 100 credits run up, with 9 left
	l.tick()
	var rec *httptest.ResponseRecorder
	select {
	case rec = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("simulation not cancelled")
	}
	if rec.Code != 429 || rec.Header().Get(costHeader) != "101" || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("cancelled: %d, cost %q, Retry-After %q: %s", rec.Code, rec.Header().Get(costHeader), rec.Header().Get("Retry-After"), rec.Body.String())
	}
	waitFor(t, 2*time.Second, func() bool { return f.b.SimulatesCancelled() == 1 })
	if n := f.c.Hits("/v2/transactions/simulate"); n != 0 {
		t.Fatalf("cancelled simulation run again: %d", n)
	}
	if st := f.r.stats.Snapshot("b"); st.ErrorRate != 0 || st.BreakerOpen {
		t.Fatalf("node blamed for the cancel: %+v", st)
	}
	// The debt is billed: the client is refused until it refills.
	if rec := f.call("GET", "/v2/status", "", clientA, ""); rec.Code != 429 {
		t.Fatalf("after the debt: %d", rec.Code)
	}
}

// A simulation whose response has begun is no longer cancelled: the node's
// work is done by then. The meter stops with it.
func TestRateLimitResponseStopsMeter(t *testing.T) {
	fc := clock.NewFake(time.Unix(1000, 0))
	l := NewLimiter(LimiterOptions{Plans: map[string]domain.Plan{"free": {Minute: perMinute(10)}}, AnonymousPlan: "free",
		Weights: testWeights, SimulatePerSecond: 2}, fc, logging.Nop{}, metrics.New())
	sim := domain.Classify("POST", "/v2/transactions/simulate", "")
	tk, d := l.Admit(Identity{Key: "ip:x", Plan: "free"}, sim, 0)
	if d != nil {
		t.Fatal(d.Message)
	}
	cancelled := false
	tk.Track(func(error) { cancelled = true })
	fc.Advance(2 * time.Second)
	h := http.Header{}
	tk.Responding(h)
	if h.Get(costHeader) != "5" {
		t.Fatalf("cost at the response: %q", h.Get(costHeader))
	}
	fc.Advance(time.Minute)
	l.tick()
	if cancelled {
		t.Fatal("a responding simulation was cancelled")
	}
	tk.Done()
	k := l.keys["ip:x"]
	k.refill(fc.Now())
	// Billed 5: 1 up front, then 2s at 2/s once it ends; not a minute's worth.
	if k.minute.Level != 6 {
		t.Fatalf("billed: %v left of 10, want 6", k.minute.Level)
	}
	// Unanswered, the meter runs until the end: 1 + 4s at 2/s.
	tk2, _ := l.Admit(Identity{Key: "ip:x", Plan: "free"}, sim, 0)
	fc.Advance(4 * time.Second)
	tk2.Done()
	k.refill(fc.Now())
	if want := 6 - 1 + 4*10/60.0 - 8; k.minute.Level < want-1e-9 || k.minute.Level > want+1e-9 {
		t.Fatalf("billed: %v left, want %v", k.minute.Level, want)
	}
}

// A node whose simulate slots are full answers 503 with Retry-After: the
// request goes to another node, the node is passed over for simulations for
// that long but not blamed, and keeps serving the rest.
func TestSimulateBusyNode(t *testing.T) {
	f := newSimFleet(t, domain.Capabilities{Simulate: &testLimits})
	f.b.SetSimulate(0, 503, 2)
	if up := f.simulate(t, `{"txn-groups":[{}]}`); up != "c" {
		t.Fatalf("busy node's simulation went to %q", up)
	}
	if up := f.simulate(t, `{"txn-groups":[{}]}`); up != "c" || f.b.Hits("/v2/transactions/simulate") != 1 {
		t.Fatalf("second simulation went to %q, busy node saw %d", up, f.b.Hits("/v2/transactions/simulate"))
	}
	if st := f.r.stats.Snapshot("b"); !st.Busy || st.ErrorRate != 0 || st.BreakerOpen {
		t.Fatalf("busy node stats: %+v", st)
	}
	if rec := f.do("GET", "/v2/status", nil); rec.Header().Get("X-Algod-Loadb-Mesh-Upstream") != "b" {
		t.Fatalf("plain request went to %q", rec.Header().Get("X-Algod-Loadb-Mesh-Upstream"))
	}
	// Every node busy: the client hears it, with the wait.
	f.c.SetSimulate(0, 503, 1)
	f.h.fc.Advance(3 * time.Second)
	f.h.inject(addrA, signedHeartbeat(t, f.h.peerKey, "b", 3, 100, domain.Capabilities{MaxAcctLookback: 4}))
	f.h.inject(addrB, signedHeartbeat(t, f.cKey, "c", 3, 100, domain.Capabilities{Simulate: &testLimits, MaxAcctLookback: 4}))
	rec := f.do("POST", "/v2/transactions/simulate", strings.NewReader(`{"txn-groups":[{}]}`))
	if rec.Code != 503 || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("every node busy: %d, Retry-After %q: %s", rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
	}
}

// dtalgorand's 504 for a simulation past its time limit reaches the client
// as is: the next node would only run it as long, and the node is not to
// blame.
func TestSimulateTimeLimitPassedThrough(t *testing.T) {
	f := newSimFleet(t, domain.Capabilities{Simulate: &testLimits})
	f.b.SetSimulate(0, 504, 0)
	rec := f.do("POST", "/v2/transactions/simulate", strings.NewReader(`{"txn-groups":[{}]}`))
	if rec.Code != 504 || !strings.Contains(rec.Body.String(), "fake simulate answers 504") {
		t.Fatalf("time limit: %d %s", rec.Code, rec.Body.String())
	}
	if n := f.c.Hits("/v2/transactions/simulate"); n != 0 {
		t.Fatalf("run again elsewhere: %d", n)
	}
	if st := f.r.stats.Snapshot("b"); st.ErrorRate != 0 {
		t.Fatalf("node blamed: %+v", st)
	}
	// A plain request's 504 is still withheld and retried.
	f.b.SetFailing(true)
	if rec := f.do("GET", "/v2/status", nil); rec.Code != 200 || rec.Header().Get("X-Algod-Loadb-Mesh-Upstream") != "c" {
		t.Fatalf("plain request: %d via %q", rec.Code, rec.Header().Get("X-Algod-Loadb-Mesh-Upstream"))
	}
}

// A history page past dtalgorand's time limit gets its 504 as well.
func TestVersionsTimeLimitPassedThrough(t *testing.T) {
	f := newSimFleet(t, domain.Capabilities{Simulate: &testLimits, TimeTravel: true})
	f.c.SetPathStatus("/v2/accounts/A/versions", 504)
	rec := f.do("GET", "/v2/accounts/A/versions", nil)
	if rec.Code != 504 || !strings.Contains(rec.Body.String(), "fake node answers 504") {
		t.Fatalf("versions time limit: %d %s", rec.Code, rec.Body.String())
	}
	if st := f.r.stats.Snapshot("c"); st.ErrorRate != 0 {
		t.Fatalf("node blamed: %+v", st)
	}
}

// The allowances survive a restart: what was spent is still spent, less
// what refilled while the agent was down.
func TestRateLimitStateFile(t *testing.T) {
	fc := clock.NewFake(time.Unix(1000, 0))
	o := LimiterOptions{Plans: map[string]domain.Plan{"free": {Minute: perMinute(60), Day: perDay(1000)}, "pro": {Day: perDay(100)}},
		Tokens: []LimitToken{{Name: "acme", Token: "t", Plan: "pro"}}, AnonymousPlan: "free", Weights: testWeights,
		StateFile: filepath.Join(t.TempDir(), "sub", "ratelimit.json")}
	l := NewLimiter(o, fc, logging.Nop{}, metrics.New())
	get := domain.Classify("GET", "/v2/status", "")
	for range 30 {
		tk, _ := l.Admit(Identity{Key: "ip:a", Plan: "free"}, get, 0)
		tk.Done()
	}
	tk, _ := l.Admit(Identity{Key: "token:acme", Plan: "pro"}, get, 0)
	tk.Done()
	tk, _ = l.Admit(Identity{Key: "token:gone", Plan: "pro"}, get, 0)
	tk.Done()
	if err := l.save(); err != nil {
		t.Fatal(err)
	}
	fc.Advance(10 * time.Second)
	o.Tokens = o.Tokens[:1] // "gone" is no longer configured
	l2 := NewLimiter(o, fc, logging.Nop{}, metrics.New())
	if err := l2.load(); err != nil {
		t.Fatal(err)
	}
	k := l2.keys["ip:a"]
	if k == nil || k.minute.Level != 40 || k.day.Level != 970+10*1000/86400.0 {
		t.Fatalf("anonymous after restart: %+v", k)
	}
	if k := l2.keys["token:acme"]; k == nil || k.day.Level > 99.02 {
		t.Fatalf("token after restart: %+v", k)
	}
	if _, ok := l2.keys["token:gone"]; ok {
		t.Fatal("a token no longer configured was loaded")
	}
	// Full allowances are not written; a sweep forgets them.
	fc.Advance(48 * time.Hour)
	l2.sweep()
	if len(l2.keys) != 0 {
		t.Fatalf("full allowances kept: %d", len(l2.keys))
	}
}

// The client's address is the socket peer, or behind a trusted proxy the
// last address it did not add; IPv6 clients are grouped by prefix.
func TestRateLimitClientKey(t *testing.T) {
	l := NewLimiter(LimiterOptions{Plans: map[string]domain.Plan{"free": {}}, AnonymousPlan: "free",
		TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}, clock.NewFake(time.Unix(0, 0)), logging.Nop{}, metrics.New())
	for _, tc := range []struct{ remote, xff, want string }{
		{"192.0.2.1:5000", "", "192.0.2.1"},
		{"192.0.2.1:5000", "203.0.113.9", "192.0.2.1"}, // not a trusted proxy: its header is ignored
		{"10.0.0.5:5000", "203.0.113.9", "203.0.113.9"},
		{"10.0.0.5:5000", "198.51.100.1, 203.0.113.9, 10.0.0.7", "203.0.113.9"}, // the leftmost could be forged
		{"10.0.0.5:5000", "", "10.0.0.5"},
		{"[2001:db8:1:2:3:4:5:6]:5000", "", "2001:db8:1:2::/64"},
		{"[::ffff:192.0.2.1]:5000", "", "192.0.2.1"},
	} {
		req := httptest.NewRequest("GET", "/v2/status", nil)
		req.RemoteAddr = tc.remote
		if tc.xff != "" {
			req.Header.Set("X-Forwarded-For", tc.xff)
		}
		id, ok := l.Identify(req, "")
		if !ok || id.Key != "ip:"+tc.want {
			t.Errorf("%s via %q: %q, want ip:%s", tc.remote, tc.xff, id.Key, tc.want)
		}
	}
}

// Quota cancellation is told apart from the client leaving and timeouts.
func TestQuotaExceededCause(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	child, stop := context.WithTimeout(ctx, time.Hour)
	defer stop()
	cancel(errQuotaExceeded)
	if !quotaExceeded(child) || errors.Is(child.Err(), context.DeadlineExceeded) {
		t.Fatal("cause lost through a derived context")
	}
	if quotaExceeded(context.Background()) {
		t.Fatal("no cause")
	}
}
