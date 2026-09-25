// Package sim runs multi-agent scenarios: N fake algods + N agents in one
// process, in-memory gossip, no sockets except loopback HTTP.
package sim

import (
	"context"
	"fmt"
	"os"

	"encoding/json"
	"github.com/d13co/algod-loadb-mesh/internal/agent"
	"github.com/d13co/algod-loadb-mesh/internal/app"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/d13co/algod-loadb-mesh/internal/config"
	"github.com/d13co/algod-loadb-mesh/internal/devfleet"
	"github.com/d13co/algod-loadb-mesh/internal/domain"
	"github.com/d13co/algod-loadb-mesh/internal/fakealgod"
)

type resp struct {
	code   int
	node   string // X-Fake-Node
	up     string // X-Algod-Loadb-Mesh-Upstream
	body   []byte
	header http.Header
}

func call(t *testing.T, method, url, token string, body string) resp {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if token != "" {
		req.Header.Set("X-Algo-API-Token", token)
	}
	r, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return resp{code: r.StatusCode, node: r.Header.Get("X-Fake-Node"), up: r.Header.Get("X-Algod-Loadb-Mesh-Upstream"), body: b, header: r.Header}
}

// defaultToken is sent with every helper request; tests that configure a
// client token set it.
var defaultToken string

func get(t *testing.T, url string) resp { return call(t, "GET", url, defaultToken, "") }

// adminToken is sent to /loadb/* by agentGet; tests that configure one set it.
var adminToken string

func agentGet(t *testing.T, url string) resp { return call(t, "GET", url, adminToken, "") }

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func status(t *testing.T, agentURL string) (map[string]any, []domain.Upstream) {
	t.Helper()
	r := agentGet(t, agentURL+"/loadb/status")
	var st struct {
		Upstreams []domain.Upstream `json:"upstreams"`
		Other     map[string]any    `json:"-"`
	}
	if err := json.Unmarshal(r.body, &st); err != nil {
		t.Fatalf("status decode: %v %s", err, r.body)
	}
	var m map[string]any
	_ = json.Unmarshal(r.body, &m)
	return m, st.Upstreams
}

func health(t *testing.T, agentURL, id string) domain.Health {
	_, ups := status(t, agentURL)
	for _, u := range ups {
		if u.ID == id {
			return u.Health
		}
	}
	return domain.HealthStarting
}

func start(t *testing.T, o devfleet.Options) *devfleet.Fleet {
	t.Helper()
	if o.Log == nil && os.Getenv("SIM_LOG") != "" {
		o.Log = agent.Logger(config.Config{Log: config.Log{Level: os.Getenv("SIM_LOG")}})
	}
	ctx, cancel := context.WithCancel(context.Background())
	f, err := devfleet.Start(ctx, o)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close(); cancel() })
	// Every agent sees every peer as synced.
	for i := range f.Agents {
		url := f.Servers[i].URL
		deadline := time.Now().Add(10 * time.Second)
		converged := func() bool {
			_, ups := status(t, url)
			if len(ups) < len(o.Nodes)+len(o.Externals) {
				return false
			}
			for _, u := range ups {
				if u.Kind != domain.KindExternal && u.Health != domain.HealthSynced {
					return false
				}
				if u.Kind == domain.KindPeer && u.Source != "heartbeat" {
					return false
				}
			}
			return true
		}
		for !converged() {
			if time.Now().After(deadline) {
				for j := range f.Agents {
					t.Logf("agent %d status: %s", j, agentGet(t, f.Servers[j].URL+"/loadb/status").body)
				}
				t.Fatalf("fleet did not converge at %s", url)
			}
			time.Sleep(25 * time.Millisecond)
		}
	}
	return f
}

func threeNodes() []devfleet.NodeSpec {
	return []devfleet.NodeSpec{
		{ID: "arch", StartRound: 5000, Archival: true, OldestRound: 0, Tier: 1},
		{ID: "plain", StartRound: 5000, OldestRound: 4000, Tier: 1},
		{ID: "devn", StartRound: 5000, OldestRound: 4000, DeveloperAPI: true, Tier: 2},
	}
}

func TestFallbackServesLocallyAndRoutesByCapability(t *testing.T) {
	f := start(t, devfleet.Options{Nodes: threeNodes(), Mode: "fallback", ReturnHysteresis: 2})
	arch, plain, devn := f.Servers[0].URL, f.Servers[1].URL, f.Servers[2].URL

	if r := get(t, plain+"/v2/status"); r.node != "plain" || r.header.Get("X-Algod-Loadb-Mesh-Mode") != "fallback" {
		t.Fatalf("local first: %+v", r)
	}
	// Old block from a non-archival agent goes to the archival node.
	if r := get(t, plain+"/v2/blocks/100"); r.code != 200 || r.node != "arch" {
		t.Fatalf("archival routing: %d %s %s", r.code, r.node, r.body)
	}
	// Recent block stays local.
	if r := get(t, plain+"/v2/blocks/4500"); r.node != "plain" {
		t.Fatalf("recent block should be local, got %s", r.node)
	}
	// Teal goes to the developer node even though it is tier 2.
	if r := call(t, "POST", arch+"/v2/teal/compile", "", "int 1"); r.code != 200 || r.node != "devn" {
		t.Fatalf("teal routing: %d %s", r.code, r.node)
	}
	// Local-only endpoints never leave the host.
	if r := get(t, devn+"/v2/transactions/pending"); r.node != "devn" {
		t.Fatalf("pool dump must be local, got %s", r.node)
	}
	// A block nobody has is unroutable.
	if r := get(t, plain+"/v2/blocks/999999"); r.code != 503 {
		t.Fatalf("future block: %d %s", r.code, r.body)
	}
}

func TestFallbackRoutesAwayWhenLocalLagsAndReturnsWithHysteresis(t *testing.T) {
	f := start(t, devfleet.Options{Nodes: threeNodes(), Mode: "fallback", ReturnHysteresis: 3})
	plain := f.Servers[1].URL

	// Everyone but "plain" advances: plain is now lagging.
	f.Nodes[0].Advance(1)
	f.Nodes[2].Advance(1)
	waitFor(t, 5*time.Second, "plain judged lagging", func() bool { return health(t, plain, "plain") == domain.HealthLagging })
	if r := get(t, plain+"/v2/status"); r.node == "plain" {
		t.Fatal("lagging local node must not serve sync-sensitive requests")
	}
	// Block it still holds is fine to serve locally while lagging.
	if r := get(t, plain+"/v2/blocks/4500"); r.node != "plain" {
		t.Fatalf("held block should still be served locally, got %s", r.node)
	}

	// Catch up. Hysteresis requires 3 consecutive synced rounds.
	f.Nodes[1].Advance(1)
	time.Sleep(300 * time.Millisecond)
	if h := health(t, plain, "plain"); h == domain.HealthSynced {
		t.Fatal("should not be trusted immediately after catching up")
	}
	for i := 0; i < 3; i++ {
		f.Advance(1)
		time.Sleep(150 * time.Millisecond)
	}
	waitFor(t, 5*time.Second, "plain trusted again", func() bool { return health(t, plain, "plain") == domain.HealthSynced })
	if r := get(t, plain+"/v2/status"); r.node != "plain" {
		t.Fatalf("expected local after hysteresis, got %s", r.node)
	}
}

func TestSilentAgentDegradedProbing(t *testing.T) {
	f := start(t, devfleet.Options{Nodes: threeNodes(), Mode: "fallback", ReturnHysteresis: 1})
	arch := f.Servers[0].URL
	// Cut the "plain" agent off the gossip network; its algod keeps running.
	f.Hub.Partition("mem:plain", true)
	waitFor(t, 5*time.Second, "plain still reachable via direct probe", func() bool {
		return f.Nodes[1].Hits("/v2/status") > 2 && health(t, arch, "plain") == domain.HealthSynced
	})
	// Now the node itself dies: probes fail, peer goes offline.
	f.Nodes[1].SetFailing(true)
	waitFor(t, 5*time.Second, "plain offline", func() bool { return health(t, arch, "plain") == domain.HealthOffline })
	f.Hub.Partition("mem:plain", false)
	f.Nodes[1].SetFailing(false)
	// Recovery needs a new synced round (hysteresis counts rounds), so the
	// chain must move for trust to return.
	waitFor(t, 8*time.Second, "plain back", func() bool {
		f.Advance(1)
		time.Sleep(150 * time.Millisecond)
		return health(t, arch, "plain") == domain.HealthSynced
	})
}

func TestExternalTierIsLastResort(t *testing.T) {
	ext := fakealgod.New(fakealgod.Options{ID: "nodely", StartRound: 5000, OldestRound: 0, Token: "exttoken"})
	defer ext.Close()
	full := domain.Archival{Kind: domain.ArchivalFull}
	f := start(t, devfleet.Options{Nodes: threeNodes()[:2], Mode: "fallback", ReturnHysteresis: 1,
		Externals: []config.External{{Name: "nodely", URL: ext.URL(), Token: "exttoken", Tier: 9,
			Capabilities: &domain.CapabilityOverrides{Archival: &full}, HealthCheck: 200 * time.Millisecond}}})
	plain := f.Servers[1].URL

	if r := get(t, plain+"/v2/status"); r.node != "plain" {
		t.Fatalf("mesh healthy: local expected, got %s", r.node)
	}
	if r := get(t, plain+"/v2/blocks/10"); r.node != "arch" {
		t.Fatalf("mesh archival preferred over external, got %s", r.node)
	}
	// While the mesh serves, the external is never checked, nor shown as
	// anything but unknown.
	if h := health(t, plain, "nodely"); h != domain.HealthOffline {
		t.Fatalf("external judged %s without a check", h)
	}
	if n := ext.Hits("/"); n != 0 {
		t.Fatalf("external hit %d times while the mesh serves", n)
	}
	// Kill the whole mesh: the external is checked on demand and takes over.
	f.Nodes[0].SetFailing(true)
	f.Nodes[1].SetFailing(true)
	waitFor(t, 8*time.Second, "external serving", func() bool {
		r := get(t, plain+"/v2/status")
		return r.node == "nodely"
	})
	if h := health(t, plain, "nodely"); h != domain.HealthSynced {
		t.Fatalf("external serving but judged %s", h)
	}
	// Wait for the mesh to be known down (offline or breaker open), so that
	// the broadcast below reaches the external directly rather than after
	// failing on the nodes.
	waitFor(t, 8*time.Second, "mesh known down", func() bool {
		get(t, plain+"/v2/status") // failures for the breakers to count
		_, ups := status(t, plain)
		down := 0
		for _, u := range ups {
			if u.Kind != domain.KindExternal && (!u.Health.Reachable() || u.Stats.BreakerOpen) {
				down++
			}
		}
		return down == 2
	})
	if r := get(t, plain+"/v2/blocks/10"); r.node != "nodely" {
		t.Fatalf("external archival, got %s", r.node)
	}
	// External rejects broadcasts when declared follow-mode? Not declared: it accepts.
	if r := call(t, "POST", plain+"/v2/transactions", "", "txnbytes"); r.node != "nodely" || r.code != 200 {
		t.Fatalf("broadcast via external: %d %s", r.code, r.node)
	}
	f.Nodes[0].SetFailing(false)
	f.Nodes[1].SetFailing(false)
	waitFor(t, 10*time.Second, "mesh back", func() bool { return get(t, plain+"/v2/status").node == "plain" })
	// With the mesh back the checks stop, even after the last one expired.
	checks := ext.Hits("/v2/status")
	time.Sleep(600 * time.Millisecond)
	for i := 0; i < 5; i++ {
		if r := get(t, plain+"/v2/status"); r.node != "plain" {
			t.Fatalf("mesh back: got %s", r.node)
		}
	}
	if n := ext.Hits("/v2/status"); n != checks {
		t.Fatalf("external checked %d times with the mesh back", n-checks)
	}
}

func TestWaitForBlockAfterIsCoalesced(t *testing.T) {
	f := start(t, devfleet.Options{Nodes: threeNodes(), Mode: "fallback"})
	plain := f.Servers[1].URL
	before := f.Nodes[1].Hits("/v2/status/wait-for-block-after")
	results := make(chan resp, 8)
	for i := 0; i < 8; i++ {
		go func() { results <- get(t, plain+"/v2/status/wait-for-block-after/5000") }()
	}
	time.Sleep(200 * time.Millisecond)
	f.Advance(1)
	for i := 0; i < 8; i++ {
		r := <-results
		if r.code != 200 || r.up != "plain" || !strings.Contains(string(r.body), `"last-round":5001`) {
			t.Fatalf("wait result: %d %s %s", r.code, r.up, r.body)
		}
	}
	if extra := f.Nodes[1].Hits("/v2/status/wait-for-block-after") - before; extra > 2 {
		t.Fatalf("8 client waits cost %d algod long-polls", extra)
	}
	// A stale round answers immediately.
	r := get(t, plain+"/v2/status/wait-for-block-after/10")
	if r.code != 200 || !strings.Contains(string(r.body), `"last-round":5001`) {
		t.Fatalf("stale wait: %d %s", r.code, r.body)
	}
}

func TestPendingLookupFollowsBroadcast(t *testing.T) {
	f := start(t, devfleet.Options{Nodes: threeNodes(), Mode: "fallback"})
	arch, plain := f.Servers[0].URL, f.Servers[1].URL
	r := call(t, "POST", arch+"/v2/transactions", "", "signed-txn-bytes")
	if r.code != 200 || r.node != "arch" {
		t.Fatalf("broadcast: %d %s %s", r.code, r.node, r.body)
	}
	var tx struct {
		TxID string `json:"txId"`
	}
	_ = json.Unmarshal(r.body, &tx)
	// Same agent: remembered upstream first.
	if p := get(t, arch+"/v2/transactions/pending/"+tx.TxID); p.code != 200 || p.node != "arch" {
		t.Fatalf("pending via same agent: %d %s", p.code, p.node)
	}
	if f.Nodes[1].Hits("/v2/transactions/pending/") != 0 {
		t.Fatal("remembered node should be tried first, plain was asked")
	}
	// Other agent: asking every node finds it on arch; unknown id ends in 404.
	if p := get(t, plain+"/v2/transactions/pending/"+tx.TxID); p.code != 200 || p.node != "arch" {
		t.Fatalf("pending via other agent: %d %s %s", p.code, p.node, p.body)
	}
	if p := get(t, plain+"/v2/transactions/pending/NOPE"); p.code != 404 {
		t.Fatalf("unknown pending: %d", p.code)
	}
}

func TestPendingLookupPrefersPoolErrorAndConfirmation(t *testing.T) {
	f := start(t, devfleet.Options{Nodes: threeNodes(), Mode: "fallback"})
	arch, plain, devn := f.Nodes[0], f.Nodes[1], f.Nodes[2]
	via := f.Servers[2].URL // devn's agent: its own node answers first in fallback mode

	// Still pending on two nodes, dropped with an error on arch: the error wins.
	devn.SetPendingTxn("DROPPED", "", 0)
	plain.SetPendingTxn("DROPPED", "", 0)
	arch.SetPendingTxn("DROPPED", "overspend", 0)
	if p := get(t, via+"/v2/transactions/pending/DROPPED"); p.code != 200 || p.node != "arch" || !strings.Contains(string(p.body), "overspend") {
		t.Fatalf("pool error: %d %s %s", p.code, p.node, p.body)
	}
	// The answering node is pinned: the next poll asks arch alone.
	before := plain.Hits("/v2/transactions/pending/DROPPED")
	if p := get(t, via+"/v2/transactions/pending/DROPPED"); p.code != 200 || p.node != "arch" {
		t.Fatalf("pinned poll: %d %s", p.code, p.node)
	}
	if plain.Hits("/v2/transactions/pending/DROPPED") != before {
		t.Fatal("pinned poll should not fan out")
	}

	// A confirmation beats a pool error elsewhere.
	devn.SetPendingTxn("DONE", "", 0)
	arch.SetPendingTxn("DONE", "txn dead", 0)
	plain.SetPendingTxn("DONE", "", 5001)
	if p := get(t, via+"/v2/transactions/pending/DONE"); p.code != 200 || p.node != "plain" || !strings.Contains(string(p.body), `"confirmed-round":5001`) {
		t.Fatalf("confirmed: %d %s %s", p.code, p.node, p.body)
	}

	// Found on one node only, 404 elsewhere.
	plain.SetPendingTxn("ONLY", "", 0)
	if p := get(t, via+"/v2/transactions/pending/ONLY"); p.code != 200 || p.node != "plain" {
		t.Fatalf("only on plain: %d %s %s", p.code, p.node, p.body)
	}
}

func TestRegistryChangesPropagate(t *testing.T) {
	f := start(t, devfleet.Options{Nodes: threeNodes(), Mode: "fallback", RegistryRefresh: 300 * time.Millisecond})
	arch := f.Servers[0].URL
	ctx := context.Background()
	if err := f.Registry.Delete(ctx, "devn"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "devn removed", func() bool { _, ups := status(t, arch); return len(ups) == 2 })
	recs, _, _ := f.Registry.List(ctx)
	if len(recs) != 2 {
		t.Fatalf("registry has %d records", len(recs))
	}
	// Re-add with a different tier: visible within one refresh.
	rec := domain.NodeRecord{ID: "devn", Network: "devnet-v1", Endpoints: []string{f.Nodes[2].URL()}, Token: f.Nodes[2].Token(),
		Agent: domain.AgentInfo{Addr: "mem:devn"}, Tier: 7, Version: 2}
	// Agent key must match what the devn agent signs with; copy it from the earlier record.
	for _, u := range recsBefore(t, f) {
		if u.ID == "devn" {
			rec.Agent.PubKey = u.Agent.PubKey
		}
	}
	if err := f.Registry.Put(ctx, rec); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "devn back at tier 7", func() bool {
		_, ups := status(t, arch)
		for _, u := range ups {
			if u.ID == "devn" && u.Tier == 7 && u.Health == domain.HealthSynced {
				return true
			}
		}
		return false
	})
}

// recsBefore reads the current records straight from the devn agent's own
// registration so the test can reuse its public key.
func recsBefore(t *testing.T, f *devfleet.Fleet) []domain.NodeRecord {
	t.Helper()
	// The devn agent re-registers itself only on first success, so derive
	// the key the same way the fleet did.
	var out []domain.NodeRecord
	r := agentGet(t, f.Servers[2].URL+"/loadb/status")
	var st struct {
		Local struct {
			Online bool `json:"online"`
		} `json:"local"`
	}
	_ = json.Unmarshal(r.body, &st)
	out = append(out, domain.NodeRecord{ID: "devn", Agent: domain.AgentInfo{PubKey: f.AgentPubKey("devn")}})
	return out
}

func TestLoadBalancerSpreadsAndAvoidsBrokenNode(t *testing.T) {
	f := start(t, devfleet.Options{Nodes: threeNodes(), Mode: "loadbalancer", ReturnHysteresis: 1})
	arch := f.Servers[0].URL
	counts := map[string]int{}
	for i := 0; i < 60; i++ {
		counts[get(t, arch+"/v2/status").node]++
	}
	if len(counts) < 2 {
		t.Fatalf("loadbalancer should spread across tier-1 nodes, got %v", counts)
	}
	if counts["devn"] != 0 {
		t.Fatalf("tier 2 must not be used while tier 1 is healthy: %v", counts)
	}
	// plain starts failing: breaker opens after a few errors, traffic moves.
	f.Nodes[1].SetFailing(true)
	for i := 0; i < 20; i++ {
		get(t, arch+"/v2/status")
	}
	after := map[string]int{}
	for i := 0; i < 20; i++ {
		r := get(t, arch+"/v2/status")
		after[r.node]++
		if r.code != 200 {
			t.Fatalf("request failed while a healthy node exists: %d %s", r.code, r.body)
		}
	}
	if after["plain"] != 0 {
		t.Fatalf("broken node still selected: %v", after)
	}
}

func TestClientTokenAndAgentEndpoints(t *testing.T) {
	defaultToken, adminToken = "secret", "admin"
	t.Cleanup(func() { defaultToken, adminToken = "", "" })
	f := start(t, devfleet.Options{Nodes: threeNodes()[:1], Mode: "fallback", ClientToken: "secret", AdminToken: "admin"})
	u := f.Servers[0].URL
	if r := call(t, "GET", u+"/v2/status", "", ""); r.code != 401 {
		t.Fatalf("missing token: %d", r.code)
	}
	if r := call(t, "GET", u+"/v2/status", "secret", ""); r.code != 200 || r.node != "arch" {
		t.Fatalf("with token: %d", r.code)
	}
	if r := call(t, "GET", u+"/v2/status", "admin", ""); r.code != 200 {
		t.Fatalf("admin token is a client token too: %d", r.code)
	}
	if r := call(t, "GET", u+"/loadb/health", "", ""); r.code != 200 {
		t.Fatalf("health is open: %d %s", r.code, r.body)
	}
	for _, path := range []string{"/loadb/status", "/loadb/peers", "/loadb/metrics", "/loadb/nope"} {
		for _, tok := range []string{"", "secret", "wrong"} {
			if r := call(t, "GET", u+path, tok, ""); r.code != 401 {
				t.Errorf("%s with token %q: %d, want 401", path, tok, r.code)
			}
		}
	}
	if r := call(t, "GET", u+"/loadb/metrics", "admin", ""); r.code != 200 || !strings.Contains(string(r.body), "loadb_heartbeats_sent_total") {
		t.Fatalf("metrics: %d %s", r.code, r.body)
	}
	if r := call(t, "GET", u+"/loadb/peers", "admin", ""); r.code != 200 {
		t.Fatalf("peers: %d", r.code)
	}
}

// With a client token but no admin token, the agent endpoints stay closed.
// (start() is not used: its convergence check needs /loadb/status.)
func TestAgentEndpointsClosedWithoutAdminToken(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	f, err := devfleet.Start(ctx, devfleet.Options{Nodes: threeNodes()[:1], Mode: "fallback", ClientToken: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Close)
	u := f.Servers[0].URL
	if r := call(t, "GET", u+"/loadb/status", "secret", ""); r.code != 403 {
		t.Fatalf("status without admin token configured: %d %s", r.code, r.body)
	}
	if r := call(t, "GET", u+"/loadb/health", "", ""); r.code == 401 || r.code == 403 {
		t.Fatalf("health must stay open: %d", r.code)
	}
}

// A balancer registers itself, nodes push heartbeats to it, and it routes
// every request to the fleet without being an upstream itself.
func TestBalancerReceivesHeartbeatsAndRoutes(t *testing.T) {
	f := start(t, devfleet.Options{Nodes: threeNodes(), Balancers: []string{"lb"}, Mode: "loadbalancer"})
	lb, arch := f.Servers[3].URL, f.Servers[0].URL

	// start() already waited for the balancer to see every node by heartbeat.
	st, ups := status(t, lb)
	if st["role"] != "balancer" || st["local"] != nil || len(ups) != 3 {
		t.Fatalf("balancer status: role %v, local %v, %d upstreams", st["role"], st["local"], len(ups))
	}
	for _, u := range ups {
		if u.Kind != domain.KindPeer || u.Source != "heartbeat" {
			t.Fatalf("balancer upstream %s: kind %s, source %s", u.ID, u.Kind, u.Source)
		}
	}
	_, ups = status(t, arch)
	for _, u := range ups {
		if u.ID == "lb" {
			t.Fatal("a balancer must never be an upstream")
		}
	}

	// Rounds reach the balancer by heartbeat, not by probing.
	f.Advance(1)
	want := f.Nodes[0].Round()
	waitFor(t, 2*time.Second, "balancer sees the new round", func() bool {
		_, ups := status(t, lb)
		for _, u := range ups {
			if u.LastRound != want || u.Source != "heartbeat" {
				return false
			}
		}
		return true
	})

	counts := map[string]int{}
	for i := 0; i < 40; i++ {
		r := get(t, lb+"/v2/status")
		if r.code != 200 {
			t.Fatalf("status via balancer: %d %s", r.code, r.body)
		}
		counts[r.node]++
	}
	if len(counts) < 2 || counts["devn"] != 0 {
		t.Fatalf("balancer should spread across tier 1: %v", counts)
	}
	if r := call(t, "POST", lb+"/v2/teal/compile", "", "int 1"); r.code != 200 || r.node != "devn" {
		t.Fatalf("teal via balancer: %d %s", r.code, r.node)
	}
	if r := get(t, lb+"/v2/blocks/100"); r.code != 200 || r.node != "arch" {
		t.Fatalf("archival via balancer: %d %s", r.code, r.node)
	}
	if r := get(t, lb+"/v2/transactions/pending"); r.code != 503 || !strings.Contains(string(r.body), "balancer") {
		t.Fatalf("local-only request on a balancer: %d %s", r.code, r.body)
	}
	if r := agentGet(t, lb+"/loadb/health"); r.code != 200 {
		t.Fatalf("balancer health: %d %s", r.code, r.body)
	}
	if recs, _, _ := f.Registry.List(context.Background()); len(recs) != 4 {
		t.Fatalf("registry should hold 3 nodes and the balancer, got %d", len(recs))
	}
}

// On a balancer, wait-for-block-after is held until a heartbeat reports a
// later round, and every waiter shares one status request.
func TestBalancerWaitForBlockAfterWaitsForHeartbeat(t *testing.T) {
	f := start(t, devfleet.Options{Nodes: threeNodes(), Balancers: []string{"lb"}, Mode: "loadbalancer"})
	lb := f.Servers[3].URL
	statusHits := func() int {
		n := 0
		for _, node := range f.Nodes {
			n += node.Hits("/v2/status") - node.Hits("/v2/status/wait-for-block-after")
		}
		return n
	}
	round := f.Nodes[0].Round()
	path := fmt.Sprintf("%s/v2/status/wait-for-block-after/%d", lb, round)
	before := statusHits()
	results := make(chan resp, 8)
	for i := 0; i < 8; i++ {
		go func() { results <- get(t, path) }()
	}
	select {
	case r := <-results:
		t.Fatalf("answered before the round advanced: %d %s", r.code, r.body)
	case <-time.After(500 * time.Millisecond):
	}
	waits := 0
	for _, node := range f.Nodes {
		waits += node.Hits(fmt.Sprintf("/v2/status/wait-for-block-after/%d", round))
	}
	if waits > len(f.Nodes) { // each node's own monitor long-polls once
		t.Fatalf("the balancer long-polled nodes instead of waiting for heartbeats (%d polls)", waits)
	}
	f.Advance(1)
	want := fmt.Sprintf(`"last-round":%d`, round+1)
	for i := 0; i < 8; i++ {
		select {
		case r := <-results:
			if r.code != 200 || r.up == "" || !strings.Contains(string(r.body), want) {
				t.Fatalf("wait result: %d %s %s", r.code, r.up, r.body)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("waiters not released after the round advanced")
		}
	}
	if extra := statusHits() - before; extra > 1 {
		t.Fatalf("8 balancer waits cost %d status requests", extra)
	}
	// A round already passed answers at once.
	if r := get(t, lb+"/v2/status/wait-for-block-after/10"); r.code != 200 || !strings.Contains(string(r.body), want) {
		t.Fatalf("stale wait: %d %s", r.code, r.body)
	}
}

// links reads the mesh links an agent reports.
func links(t *testing.T, agentURL string) []app.LinkStatus {
	t.Helper()
	r := agentGet(t, agentURL+"/loadb/status")
	var st struct {
		Links []app.LinkStatus `json:"links"`
	}
	if err := json.Unmarshal(r.body, &st); err != nil {
		t.Fatalf("status decode: %v %s", err, r.body)
	}
	return st.Links
}

// allByHeartbeat reports whether every peer of every agent is synced and
// heard by heartbeat, failing the test if any peer went offline.
func allByHeartbeat(t *testing.T, f *devfleet.Fleet) bool {
	t.Helper()
	for i := range f.Agents {
		_, ups := status(t, f.Servers[i].URL)
		for _, u := range ups {
			if u.Kind != domain.KindPeer {
				continue
			}
			if u.Health == domain.HealthOffline {
				t.Fatalf("agent %d sees %s offline: failover did not beat DownAfter", i, u.ID)
			}
			if u.Source != "heartbeat" || u.Health != domain.HealthSynced {
				return false
			}
		}
	}
	return true
}

// Three nodes on two nets; one node loses net 0. Every link to it moves to
// net 1 and heartbeats resume, so the direct-probe ladder stops: the mirror
// of TestSilentAgentDegradedProbing, where the hits keep climbing. The
// ladder's first probe legitimately fires on the tick that detects the
// silence, so what is asserted is that the hits stop growing.
func TestMeshPathFailover(t *testing.T) {
	f := start(t, devfleet.Options{Nodes: threeNodes(), Mode: "fallback", ReturnHysteresis: 1, Nets: 2})
	arch, plain := f.Servers[0].URL, f.Servers[1].URL
	for _, l := range links(t, arch) {
		if l.Path != "mem:"+l.Peer || len(l.Paths) != 2 {
			t.Fatalf("before the cut, registry order carries heartbeats: %+v", l)
		}
	}
	f.Hub.Partition("mem:plain", true)
	// Trust returns with a new synced round (hysteresis counts rounds), so
	// the chain must keep moving, as it would.
	waitFor(t, 8*time.Second, "every link to and from plain on net 1", func() bool {
		f.Advance(1)
		time.Sleep(100 * time.Millisecond)
		for _, l := range links(t, arch) {
			if l.Peer == "plain" && l.Path != "mem1:plain" {
				return false
			}
		}
		for _, l := range links(t, plain) {
			if l.Path != "mem1:"+l.Peer {
				return false
			}
		}
		return allByHeartbeat(t, f)
	})
	m := agentGet(t, plain+"/loadb/metrics")
	if !strings.Contains(string(m.body), `loadb_path_switches_total{reason="deaf"}`) {
		t.Fatalf("plain must have learnt from the peers' HeardMS:\n%s", m.body)
	}
	// Healed: no more direct probes of anyone. The agents' own long-polls
	// share the prefix and are not probes.
	hits := func() int {
		n := 0
		for _, node := range f.Nodes {
			n += node.Hits("/v2/status") - node.Hits("/v2/status/wait-for-block-after")
		}
		return n
	}
	before := hits()
	time.Sleep(2 * time.Second) // several ProbeIntervals
	if after := hits(); after != before {
		t.Fatalf("/v2/status hits still growing after failover: %d -> %d", before, after)
	}
	if !allByHeartbeat(t, f) {
		t.Fatal("not every peer stayed on heartbeats")
	}
	f.Hub.Partition("mem:plain", false)
}

// A on net 0 only, B on net 1 only, C on both: A and B fall back to probing
// each other's node directly while C keeps heartbeats with both, and A's
// link to B shows the candidate it has no route to.
func TestMixedTopology(t *testing.T) {
	nodes := threeNodes()
	nodes[0].Nets, nodes[1].Nets = []int{0}, []int{1}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f, err := devfleet.Start(ctx, devfleet.Options{Nodes: nodes, Mode: "fallback", ReturnHysteresis: 1, Nets: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	arch, plain, devn := f.Servers[0].URL, f.Servers[1].URL, f.Servers[2].URL
	source := func(agentURL, id string) string {
		_, ups := status(t, agentURL)
		for _, u := range ups {
			if u.ID == id && u.Health == domain.HealthSynced {
				return u.Source
			}
		}
		return ""
	}
	waitFor(t, 10*time.Second, "A and B probe each other, everyone hears C", func() bool {
		return source(arch, "plain") == "probe" && source(plain, "arch") == "probe" &&
			source(arch, "devn") == "heartbeat" && source(plain, "devn") == "heartbeat" &&
			source(devn, "arch") == "heartbeat" && source(devn, "plain") == "heartbeat"
	})
	for _, l := range links(t, arch) {
		if l.Peer == "plain" && (l.Path != "" || len(l.Paths) != 1 || !l.Paths[0].NoRoute) {
			t.Fatalf("A's link to B must show its one candidate as unroutable: %+v", l)
		}
	}
	for _, l := range links(t, devn) {
		want := map[string]string{"arch": "mem:arch", "plain": "mem1:plain"}[l.Peer]
		if l.Path != want {
			t.Fatalf("C reaches %s via %s, not %s", l.Peer, want, l.Path)
		}
	}
}
