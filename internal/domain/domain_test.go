package domain

import (
	"crypto/ed25519"
	crand "crypto/rand"
	"encoding/json"
	"math/rand/v2"
	"strings"
	"testing"
	"time"
)

func u64(v uint64) *uint64 { return &v }

func TestClassify(t *testing.T) {
	cases := []struct {
		method, path string
		want         RequestClass
	}{
		{"GET", "/v2/status", RequestClass{Idempotent: true}},
		{"GET", "/v2/blocks/123", RequestClass{Round: u64(123), Idempotent: true}},
		{"GET", "/v2/blocks/123/hash", RequestClass{Round: u64(123), Idempotent: true}},
		{"GET", "/v2/deltas/55", RequestClass{Round: u64(55), Idempotent: true}},
		{"GET", "/v2/deltas/txn/group/abc", RequestClass{Idempotent: true}},
		{"GET", "/v2/stateproofs/9", RequestClass{Round: u64(9), Idempotent: true}},
		{"GET", "/v2/status/wait-for-block-after/70", RequestClass{WaitAfter: u64(70), Idempotent: true}},
		{"POST", "/v2/teal/compile", RequestClass{NeedsDev: true, Idempotent: true}},
		{"POST", "/v2/transactions", RequestClass{Broadcast: true}},
		{"POST", "/v2/transactions/simulate", RequestClass{Idempotent: true}},
		{"GET", "/v2/transactions/pending/TXID", RequestClass{PendingID: "TXID", Idempotent: true}},
		{"GET", "/v2/transactions/pending", RequestClass{LocalOnly: true, Idempotent: true}},
		{"GET", "/health", RequestClass{LocalOnly: true, Idempotent: true}},
		{"GET", "/loadb/status", RequestClass{Agent: true, LocalOnly: true, Idempotent: true}},
		{"POST", "/v2/participation", RequestClass{LocalOnly: true}},
	}
	for _, c := range cases {
		got := Classify(c.method, c.path)
		if (got.Round == nil) != (c.want.Round == nil) || (got.Round != nil && *got.Round != *c.want.Round) {
			t.Errorf("%s %s: round %v want %v", c.method, c.path, got.Round, c.want.Round)
		}
		if (got.WaitAfter == nil) != (c.want.WaitAfter == nil) || (got.WaitAfter != nil && *got.WaitAfter != *c.want.WaitAfter) {
			t.Errorf("%s %s: wait %v want %v", c.method, c.path, got.WaitAfter, c.want.WaitAfter)
		}
		got.Round, got.WaitAfter, c.want.Round, c.want.WaitAfter = nil, nil, nil, nil
		if got != c.want {
			t.Errorf("%s %s: got %+v want %+v", c.method, c.path, got, c.want)
		}
	}
}

func up(id string, kind UpstreamKind, tier int, h Health, round uint64) Upstream {
	return Upstream{ID: id, Kind: kind, Tier: tier, Health: h, LastRound: round,
		Caps: Capabilities{OldestRound: OldestRoundFor(Archival{Kind: ArchivalNone}, round)}}
}

func TestEligible(t *testing.T) {
	local := up("local", KindLocal, 1, HealthSynced, 1000)
	tests := []struct {
		name string
		u    Upstream
		c    RequestClass
		best uint64
		want bool
	}{
		{"synced default", local, RequestClass{}, 1000, true},
		{"lagging default", up("p", KindPeer, 1, HealthLagging, 999), RequestClass{}, 1000, false},
		{"offline", up("p", KindPeer, 1, HealthOffline, 1000), RequestClass{}, 1000, false},
		{"draining", func() Upstream { u := local; u.Draining = true; return u }(), RequestClass{}, 1000, false},
		{"breaker", func() Upstream { u := local; u.Stats.BreakerOpen = true; return u }(), RequestClass{}, 1000, false},
		{"round in window", local, RequestClass{Round: u64(500)}, 1000, true},
		{"round pruned", up("l", KindLocal, 1, HealthSynced, 5000), RequestClass{Round: u64(1)}, 5000, false},
		{"round in future", local, RequestClass{Round: u64(1001)}, 1000, false},
		{"round while lagging but held", up("p", KindPeer, 1, HealthLagging, 990), RequestClass{Round: u64(980)}, 1000, true},
		{"needs dev no", local, RequestClass{NeedsDev: true}, 1000, false},
		{"needs dev yes", func() Upstream { u := local; u.Caps.DeveloperAPI = true; return u }(), RequestClass{NeedsDev: true}, 1000, true},
		{"broadcast follower", func() Upstream { u := local; u.Caps.FollowMode = true; return u }(), RequestClass{Broadcast: true}, 1000, false},
		{"local only on peer", up("p", KindPeer, 1, HealthSynced, 1000), RequestClass{LocalOnly: true}, 1000, false},
		{"local only on local online", up("l", KindLocal, 1, HealthOnline, 900), RequestClass{LocalOnly: true}, 1000, true},
		{"wait after far ahead", local, RequestClass{WaitAfter: u64(1005)}, 1000, false},
		{"wait after next", local, RequestClass{WaitAfter: u64(1000)}, 1000, true},
	}
	for _, tt := range tests {
		if got := Eligible(tt.u, tt.c, tt.best, 0); got != tt.want {
			t.Errorf("%s: got %v want %v", tt.name, got, tt.want)
		}
	}
	if !Eligible(up("p", KindPeer, 1, HealthOnline, 999), RequestClass{}, 1000, 1) {
		t.Error("tolerance 1 should admit one round of lag")
	}
	if Eligible(up("p", KindPeer, 1, HealthOnline, 998), RequestClass{}, 1000, 1) {
		t.Error("tolerance 1 must reject two rounds of lag")
	}
}

func TestSelectFallbackPrefersLocalThenTiers(t *testing.T) {
	local := up("local", KindLocal, 5, HealthSynced, 100)
	local.Stats.LatencyMS = map[string]float64{"default": 900}
	p1 := up("p1", KindPeer, 1, HealthSynced, 100)
	p1.Stats.LatencyMS = map[string]float64{"default": 1}
	p2 := up("p2", KindPeer, 2, HealthSynced, 100)
	ext := up("ext", KindExternal, 0, HealthSynced, 100)
	cands := []Upstream{ext, p2, p1, local}

	sel, ok := Select(ModeFallback, cands, RequestClass{}, 100, 0, Weights{}, nil)
	if !ok || sel.Chosen.ID != "local" {
		t.Fatalf("fallback should choose local regardless of latency, got %+v", sel.Chosen.ID)
	}
	local.Health = HealthLagging
	cands = []Upstream{ext, p2, p1, local}
	sel, _ = Select(ModeFallback, cands, RequestClass{}, 100, 0, Weights{}, nil)
	if sel.Chosen.ID != "p1" {
		t.Fatalf("fallback should choose tier 1 peer, got %s", sel.Chosen.ID)
	}
	if len(sel.Alternates) != 2 || sel.Alternates[0].ID != "p2" || sel.Alternates[1].ID != "ext" {
		t.Fatalf("alternates should be p2 then ext, got %+v", sel.Alternates)
	}
	// Only the external remains.
	cands = []Upstream{ext, up("p2", KindPeer, 2, HealthOffline, 0), up("p1", KindPeer, 1, HealthOffline, 0)}
	sel, ok = Select(ModeFallback, cands, RequestClass{}, 100, 0, Weights{}, nil)
	if !ok || sel.Chosen.ID != "ext" {
		t.Fatalf("external is the last resort, got %v %v", ok, sel.Chosen.ID)
	}
}

func TestSelectLoadBalancerNeverPicksIneligible(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 2000; i++ {
		var cands []Upstream
		for j := 0; j < 6; j++ {
			h := []Health{HealthSynced, HealthLagging, HealthOffline}[r.IntN(3)]
			u := up(string(rune('a'+j)), []UpstreamKind{KindLocal, KindPeer, KindPeer, KindExternal}[r.IntN(4)], r.IntN(3), h, 100)
			u.Stats.LatencyMS = map[string]float64{"default": float64(r.IntN(100))}
			u.Stats.BreakerOpen = r.IntN(10) == 0
			cands = append(cands, u)
		}
		c := RequestClass{}
		if r.IntN(2) == 0 {
			c.Round = u64(uint64(r.IntN(120)))
		}
		sel, ok := Select(ModeLoadBalancer, cands, c, 100, 0, Weights{}, r)
		if !ok {
			continue
		}
		if !Eligible(sel.Chosen, c, 100, 0) {
			t.Fatalf("chose ineligible %+v", sel.Chosen)
		}
		for _, a := range sel.Alternates {
			if !Eligible(a, c, 100, 0) {
				t.Fatalf("alternate ineligible %+v", a)
			}
		}
	}
}

func TestSelectLoadBalancerSpreads(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 7))
	a := up("a", KindPeer, 1, HealthSynced, 100)
	b := up("b", KindPeer, 1, HealthSynced, 100)
	a.Stats.LatencyMS = map[string]float64{"default": 10}
	b.Stats.LatencyMS = map[string]float64{"default": 10}
	counts := map[string]int{}
	for i := 0; i < 1000; i++ {
		sel, _ := Select(ModeLoadBalancer, []Upstream{a, b}, RequestClass{}, 100, 0, Weights{}, r)
		counts[sel.Chosen.ID]++
	}
	if counts["a"] < 300 || counts["b"] < 300 {
		t.Fatalf("expected spread, got %v", counts)
	}
}

func TestReturnHysteresis(t *testing.T) {
	h := NewReturnHysteresis(3)
	if h.Observe(HealthSynced) != HealthSynced {
		t.Fatal("initially trusted")
	}
	if h.Observe(HealthLagging) != HealthLagging {
		t.Fatal("lagging passes through")
	}
	got := []Health{h.Observe(HealthSynced), h.Observe(HealthSynced), h.Observe(HealthSynced), h.Observe(HealthSynced)}
	want := []Health{HealthLagging, HealthLagging, HealthSynced, HealthSynced}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("step %d: got %v want %v (all %v)", i, got[i], want[i], got)
		}
	}
	h.Observe(HealthOffline)
	h.Observe(HealthSynced)
	if h.Observe(HealthLagging) != HealthLagging || h.Observe(HealthSynced) != HealthLagging {
		t.Fatal("streak must restart after an interruption")
	}
}

func TestOldestRound(t *testing.T) {
	if OldestRoundFor(Archival{Kind: ArchivalFull}, 5e6) != 0 {
		t.Fatal("full")
	}
	if OldestRoundFor(Archival{Kind: ArchivalTrailing, N: 100}, 500) != 400 {
		t.Fatal("trailing")
	}
	if OldestRoundFor(Archival{Kind: ArchivalSince, N: 42}, 500) != 42 {
		t.Fatal("since")
	}
	if OldestRoundFor(Archival{Kind: ArchivalNone}, 5000) != 4000 {
		t.Fatal("none")
	}
}

func TestRecordCodecRoundTrip(t *testing.T) {
	seed := make([]byte, 32)
	crand.Read(seed)
	key, err := DeriveSyncKey(seed)
	if err != nil {
		t.Fatal(err)
	}
	rec := NodeRecord{ID: "k44", Network: "mainnet-v1.0", Endpoints: []string{"http://10.0.0.1:8080"},
		Token: "tok", Agent: AgentInfo{Addr: "10.0.0.1:4001", PubKey: []byte("k")}, Tier: 1, Version: 1}
	nonce := make([]byte, 12)
	crand.Read(nonce)
	blob, err := SealRecord(key, rec, nonce)
	if err != nil {
		t.Fatal(err)
	}
	got, err := OpenRecord(key, BoxName(key, "k44"), blob)
	if err != nil {
		t.Fatal(err)
	}
	if !got.StaticEqual(rec) {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if _, err := OpenRecord(key, BoxName(key, "k45"), blob); err == nil {
		t.Fatal("box name is bound as AAD; opening under another id must fail")
	}
	other, _ := DeriveSyncKey(append([]byte("x"), seed[1:]...))
	if _, err := OpenRecord(other, BoxName(key, "k44"), blob); err == nil {
		t.Fatal("wrong key must fail")
	}
	blob[len(blob)-1] ^= 1
	if _, err := OpenRecord(key, BoxName(key, "k44"), blob); err == nil {
		t.Fatal("tamper must fail")
	}
}

func TestRecordCodecGolden(t *testing.T) {
	// Fixed key and nonce: the ciphertext must not drift across versions.
	key, _ := DeriveSyncKey(make([]byte, 32))
	rec := NodeRecord{ID: "g", Network: "n", Endpoints: []string{"http://x"}, Version: 1}
	blob, err := SealRecord(key, rec, make([]byte, 12))
	if err != nil {
		t.Fatal(err)
	}
	const want = "010000000000000000000000001ad589"
	got := hexPrefix(blob, 16)
	if got != want {
		t.Fatalf("golden prefix changed: %s want %s (full %x)", got, want, blob)
	}
}

func hexPrefix(b []byte, n int) string {
	const hexd = "0123456789abcdef"
	out := make([]byte, 0, 2*n)
	for _, c := range b[:n] {
		out = append(out, hexd[c>>4], hexd[c&15])
	}
	return string(out)
}

func TestHeartbeatCodec(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(crand.Reader)
	hb := Heartbeat{NodeID: "k44", Seq: 9, LastRound: 1234, Online: true, Caps: Capabilities{OldestRound: 5}}
	wire, err := EncodeHeartbeat(priv, hb)
	if err != nil {
		t.Fatal(err)
	}
	got, gotPub, err := DecodeHeartbeat(wire)
	if err != nil {
		t.Fatal(err)
	}
	if !gotPub.Equal(pub) || got != hb {
		t.Fatalf("mismatch %+v", got)
	}
	wire[len(wire)-3] ^= 1
	if _, _, err := DecodeHeartbeat(wire); err == nil {
		t.Fatal("tampered heartbeat must fail")
	}
}

func TestSyncJudge(t *testing.T) {
	t0 := time.Unix(1000, 0)
	j := NewSyncJudge(0, 1500*time.Millisecond, 3)
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }
	// Same round as best: synced.
	if j.Judge(at(0), true, 100, 100) != HealthSynced {
		t.Fatal("synced at start")
	}
	// Best moves on; within grace we still call it synced.
	if j.Judge(at(100), true, 100, 101) != HealthSynced {
		t.Fatal("one round behind inside grace must stay synced")
	}
	// Node catches up 300ms later: never lagging, no hysteresis penalty.
	if j.Judge(at(400), true, 101, 101) != HealthSynced {
		t.Fatal("caught up inside grace")
	}
	// Now it stays behind for longer than grace.
	j.Judge(at(1000), true, 101, 102)
	if j.Judge(at(2600), true, 101, 102) != HealthLagging {
		t.Fatal("behind for > grace is lagging")
	}
	// Two rounds behind is lagging immediately.
	j2 := NewSyncJudge(0, time.Second, 3)
	if j2.Judge(at(0), true, 100, 102) != HealthLagging {
		t.Fatal("two behind is lagging at once")
	}
	// Recovery: needs 3 distinct synced rounds, not 3 observations.
	if j.Judge(at(3000), true, 102, 102) != HealthLagging {
		t.Fatal("first synced round after lag: not trusted yet")
	}
	for i := 0; i < 10; i++ {
		if j.Judge(at(3000+i), true, 102, 102) != HealthLagging {
			t.Fatal("repeated observations of the same round must not count")
		}
	}
	j.Judge(at(6000), true, 103, 103)
	if j.Judge(at(9000), true, 104, 104) != HealthSynced {
		t.Fatal("three synced rounds restore trust")
	}
	if j.Judge(at(9100), false, 104, 104) != HealthOffline {
		t.Fatal("unreachable")
	}
	if j.Judge(at(9200), true, 104, 104) != HealthLagging {
		t.Fatal("an outage after being seen costs hysteresis")
	}
	// A fresh judge fed "unreachable" before ever seeing the node up is
	// synced as soon as the node appears.
	j3 := NewSyncJudge(0, time.Second, 3)
	j3.Judge(at(0), false, 0, 0)
	j3.Judge(at(1), false, 0, 100)
	if j3.Judge(at(2), true, 100, 100) != HealthSynced {
		t.Fatal("first sight must be trusted")
	}
}

func TestBoxName(t *testing.T) {
	key, _ := DeriveSyncKey(make([]byte, 32))
	name := BoxName(key, "k44")
	if len(name) != 1+BoxTagLen || name[0] != 'n' || !IsRecordBox(name) {
		t.Fatalf("BoxName = %x", name)
	}
	tag := BoxTag(key, "k44")
	if string(name[1:]) != string(tag[:]) {
		t.Fatal("BoxName is not the prefix and BoxTag")
	}
	// Fixed key: the tag must not drift across versions, or every existing
	// record becomes unaddressable.
	if got, want := hexPrefix(tag[:], BoxTagLen), "6fd848fd861291e60b536937f14a5aec"; got != want {
		t.Fatalf("tag of k44 = %s, want %s", got, want)
	}
	if string(BoxName(key, "k45")) == string(name) {
		t.Fatal("different ids share a box")
	}
	other, _ := DeriveSyncKey(append([]byte("x"), make([]byte, 31)...))
	if string(BoxName(other, "k44")) == string(name) {
		t.Fatal("tag does not depend on the key")
	}
	// Ids of any length give names of one length, and nothing of the id shows.
	long := BoxName(key, "a-rather-long-node-name-0123456789abcdef0123456789")
	if len(long) != len(name) || strings.Contains(string(long), "rather") {
		t.Fatalf("long id name %x", long)
	}
	for _, n := range [][]byte{nil, []byte("n"), []byte("nk44"), append([]byte("x"), tag[:]...), append(name, 0)} {
		if IsRecordBox(n) {
			t.Errorf("IsRecordBox(%x) = true", n)
		}
	}
}

func TestOpenRecordChecksID(t *testing.T) {
	key, _ := DeriveSyncKey(make([]byte, 32))
	rec := NodeRecord{ID: "k44", Network: "n", Endpoints: []string{"http://x"}, Version: 1}
	// A key holder sealing a record under another node's name, with that
	// name as AAD, still produces a box that does not open.
	plain, _ := json.Marshal(rec)
	aead, _ := newAEAD(key)
	nonce := make([]byte, recordNonceLen)
	forged := aead.Seal(append([]byte{recordVersion}, nonce...), nonce, plain, BoxName(key, "k45"))
	if _, err := OpenRecord(key, BoxName(key, "k45"), forged); err == nil || !strings.Contains(err.Error(), "not its id") {
		t.Fatalf("err = %v", err)
	}
}

func TestBalancerRecords(t *testing.T) {
	bal := NodeRecord{ID: "lb1", Role: RoleBalancer, Network: "n", Agent: AgentInfo{Addr: "10.0.0.9:4001"}}
	if err := bal.Validate(); err != nil || !bal.IsBalancer() {
		t.Fatalf("a balancer needs no endpoints: %v", err)
	}
	if err := (NodeRecord{ID: "lb1", Role: RoleBalancer, Network: "n"}).Validate(); err == nil {
		t.Fatal("a balancer without an agent address must fail")
	}
	if err := (NodeRecord{ID: "x", Role: "router", Network: "n", Endpoints: []string{"http://x"}}).Validate(); err == nil {
		t.Fatal("unknown role must fail")
	}
	node := NodeRecord{ID: "k", Network: "n", Endpoints: []string{"http://x"}, Agent: AgentInfo{Addr: "a:1"}}
	named := node
	named.Role = RoleNode
	if node.IsBalancer() || !node.StaticEqual(named) {
		t.Fatal(`role "" and "node" are the same`)
	}
	moved := node
	moved.Role = RoleBalancer
	if node.StaticEqual(moved) {
		t.Fatal("a role change is a static change")
	}
}

func TestProbeCodec(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(crand.Reader)
	pr := Probe{Type: ProbePing, NodeID: "k44", Nonce: 77, HeardMS: -1}
	wire, err := EncodeProbe(priv, pr)
	if err != nil {
		t.Fatal(err)
	}
	env, err := DecodeMessage(wire)
	if err != nil {
		t.Fatal(err)
	}
	if !env.PubKey.Equal(pub) || env.NodeID != "k44" || env.HB != nil || env.Probe == nil || *env.Probe != pr {
		t.Fatalf("mismatch %+v", env)
	}
	if _, err := EncodeProbe(priv, Probe{Type: "pang", NodeID: "k44"}); err == nil {
		t.Fatal("bad probe type must fail to encode")
	}
	wire[len(wire)-1] ^= 1
	if _, err := DecodeMessage(wire); err == nil {
		t.Fatal("tampered probe must fail")
	}
}

// A ping must never land as a heartbeat, and a heartbeat decodes as one.
func TestDecodeMessageDiscriminates(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(crand.Reader)
	ping, _ := EncodeProbe(priv, Probe{Type: ProbePing, NodeID: "k44", Nonce: 1})
	if _, _, err := DecodeHeartbeat(ping); err == nil {
		t.Fatal("a ping decoded as a heartbeat")
	}
	hb, _ := EncodeHeartbeat(priv, Heartbeat{NodeID: "k44", Seq: 3})
	env, err := DecodeMessage(hb)
	if err != nil || env.HB == nil || env.Probe != nil || env.HB.Seq != 3 {
		t.Fatalf("heartbeat via DecodeMessage: %+v %v", env, err)
	}
}

func TestAgentInfoGossipAddrs(t *testing.T) {
	cases := []struct {
		in   AgentInfo
		want []string
	}{
		{AgentInfo{Addr: "a:1"}, []string{"a:1"}},
		{AgentInfo{Addrs: []string{"a:1", "b:1"}}, []string{"a:1", "b:1"}},
		{AgentInfo{Addrs: []string{"a:1", "b:1"}, Addr: "a:1"}, []string{"a:1", "b:1"}},
		{AgentInfo{Addrs: []string{"b:1"}, Addr: "a:1"}, []string{"b:1", "a:1"}},
		{AgentInfo{}, nil},
	}
	for _, c := range cases {
		if got := c.in.GossipAddrs(); !equalStrings(got, c.want) {
			t.Errorf("%+v: got %v want %v", c.in, got, c.want)
		}
	}
	if err := (NodeRecord{ID: "lb", Role: RoleBalancer, Network: "n", Agent: AgentInfo{Addrs: []string{"a:1"}}}).Validate(); err != nil {
		t.Fatalf("a balancer with addrs is valid: %v", err)
	}
}

// An upgraded node that gains a second address must republish, and a legacy
// record with the same single address must not.
func TestStaticEqualAddrs(t *testing.T) {
	legacy := NodeRecord{ID: "k", Network: "n", Endpoints: []string{"http://x"}, Agent: AgentInfo{Addr: "a:1"}}
	single := legacy
	single.Agent = AgentInfo{Addrs: []string{"a:1"}}
	if !legacy.StaticEqual(single) {
		t.Fatal("addr and addrs [addr] are the same record")
	}
	double := legacy
	double.Agent = AgentInfo{Addrs: []string{"a:1", "b:1"}}
	if legacy.StaticEqual(double) {
		t.Fatal("a second address is a static change")
	}
	swapped := legacy
	swapped.Agent = AgentInfo{Addrs: []string{"b:1", "a:1"}}
	if double.StaticEqual(swapped) {
		t.Fatal("address order is preference and must be compared")
	}
}

func TestPathPolicyChoose(t *testing.T) {
	t0 := time.Unix(1000, 0)
	p := PathPolicy{ProbeInterval: 30 * time.Second}
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	check := func(name string, stats []PathStat, cur int, since time.Time, now time.Time, wantIdx int, wantWhy string) {
		t.Helper()
		if idx, why := p.Choose(now, stats, cur, since); idx != wantIdx || why != wantWhy {
			t.Errorf("%s: got %d %q want %d %q", name, idx, why, wantIdx, wantWhy)
		}
	}
	unmeasured := []PathStat{{Addr: "a", Alive: true}, {Addr: "b", Alive: true}}
	check("nothing measured: registry order", unmeasured, -1, time.Time{}, t0, 0, "initial")
	measuredSecond := []PathStat{{Addr: "a", Alive: true}, {Addr: "b", Alive: true, RTT: ms(5)}}
	check("measured ranks first", measuredSecond, -1, time.Time{}, t0, 1, "initial")
	check("current unmeasured is kept", measuredSecond, 0, t0, t0.Add(time.Hour), 0, "keep")

	slightlyFaster := []PathStat{{Addr: "a", Alive: true, RTT: ms(10)}, {Addr: "b", Alive: true, RTT: ms(9)}}
	check("10% faster does not win", slightlyFaster, 0, t0, t0.Add(time.Hour), 0, "keep")
	muchFaster := []PathStat{{Addr: "a", Alive: true, RTT: ms(10)}, {Addr: "b", Alive: true, RTT: ms(3)}}
	check("one probe is not enough", muchFaster, 0, t0, t0.Add(p.ProbeInterval), 0, "keep")
	check("two probes agree", muchFaster, 0, t0, t0.Add(2*p.ProbeInterval), 1, "faster")

	dead := []PathStat{{Addr: "a", Alive: false}, {Addr: "b", Alive: true, RTT: ms(50)}}
	check("dead switches regardless of cooldown", dead, 0, t0, t0.Add(time.Second), 1, "dead")
	deaf := []PathStat{{Addr: "a", Alive: false, Deaf: true}, {Addr: "b", Alive: true}}
	check("deaf is its own reason", deaf, 0, t0, t0.Add(time.Second), 1, "deaf")
	allDead := []PathStat{{Addr: "a"}, {Addr: "b"}}
	check("all dead", allDead, 0, t0, t0, -1, "dead")
	check("still none", allDead, -1, t0, t0, -1, "none")
	check("vanished current", unmeasured[:1], 1, t0, t0, 0, "initial")
}

func TestNormalizeAddr(t *testing.T) {
	for in, want := range map[string]string{
		"[::ffff:10.0.0.1]:4001": "10.0.0.1:4001",
		"10.0.0.1:4001":          "10.0.0.1:4001",
		"[fd00::1]:4001":         "[fd00::1]:4001",
		"mem:plain":              "mem:plain",
		"garbage":                "garbage",
	} {
		if got := NormalizeAddr(in); got != want {
			t.Errorf("%s: got %s want %s", in, got, want)
		}
	}
}
