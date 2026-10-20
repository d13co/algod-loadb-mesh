package domain

import (
	"strings"
	"testing"
)

var dtLimits = SimulateLimits{MaxTxnGroups: 16, MaxRounds: 8, MaxExtraOpcodeBudget: 1_000_000, ResourceLimitMultiplier: 4}

func simClass(b SimulateBody) RequestClass {
	c := Classify("POST", "/v2/transactions/simulate", "")
	c.ApplySimulate(b)
	return c
}

func TestClassifySimulateLimits(t *testing.T) {
	if c := Classify("GET", "/v2/transactions/simulate/limits", ""); !c.SimulateLimits || c.Simulate || !c.Idempotent {
		t.Fatalf("limits: %+v", c)
	}
	if c := Classify("POST", "/v2/transactions/simulate/limits", ""); c.SimulateLimits || c.Simulate {
		t.Fatalf("POST limits is no limits request: %+v", c)
	}
}

func TestApplySimulate(t *testing.T) {
	// A round of 0 is the latest, as omitting it: any synced node.
	if c := simClass(SimulateBody{Round: 0, TxnGroups: make([]struct{}, 1)}); c.Round != nil || c.SimNeeds == nil || c.SimNeeds.Groups != 1 {
		t.Fatalf("round 0: %+v", c)
	}
	c := simClass(SimulateBody{Round: 7})
	if c.Round == nil || *c.Round != 7 || c.RoundWindow != WindowState || !c.HistoryAnyRound || c.RoundHigh != 0 {
		t.Fatalf("round 7: %+v", c)
	}
	// rounds: the span of the non-zero entries; 0 is the node's latest.
	c = simClass(SimulateBody{Rounds: []uint64{40, 0, 12, 40, 30}})
	if c.Round == nil || *c.Round != 12 || c.RoundHigh != 40 || !c.SimNeeds.RoundsSpan || c.SimNeeds.Rounds != 5 || !c.HistoryAnyRound {
		t.Fatalf("rounds: %+v %+v", c, c.SimNeeds)
	}
	if c = simClass(SimulateBody{Rounds: []uint64{0, 0}}); c.Round != nil || c.SimNeeds.RoundsSpan || c.SimNeeds.Rounds != 2 {
		t.Fatalf("rounds of latest only: %+v %+v", c, c.SimNeeds)
	}
	for _, b := range []SimulateBody{{ParallelGroups: true}, {SkipValidityWindow: true}, {RewriteValidityWindow: true}, {Protocol: "v43"}} {
		if c := simClass(b); !c.SimNeeds.Fork || c.SimNeeds.DeveloperAPI {
			t.Errorf("%+v: needs %+v", b, c.SimNeeds)
		}
	}
	var trace SimulateBody
	trace.ExecTraceConfig.Enable = true
	for _, b := range []SimulateBody{trace, {Protocol: "future"}} {
		if c := simClass(b); !c.SimNeeds.DeveloperAPI {
			t.Errorf("%+v: needs %+v", b, c.SimNeeds)
		}
	}
	if c := simClass(SimulateBody{ExtraOpcodeBudget: -5}); c.SimNeeds.ExtraOpcodeBudget != 0 {
		t.Fatalf("negative budget: %+v", c.SimNeeds)
	}
	c = simClass(SimulateBody{Rounds: []uint64{3, 9}, TxnGroups: make([]struct{}, 3), ExtraOpcodeBudget: 500_000, ParallelGroups: true})
	for _, want := range []string{"round=3", "round_high=9", "sim_groups=3", "sim_rounds=2", "sim_budget=500000", "sim_fork"} {
		if !strings.Contains(c.String(), want) {
			t.Errorf("class %q lacks %q", c.String(), want)
		}
	}
	if c.StatKey() != "simulate" {
		t.Fatalf("stat key %q", c.StatKey())
	}
}

func TestSimulateNeedsFits(t *testing.T) {
	stock := Capabilities{}
	dt := Capabilities{Simulate: &dtLimits}
	dtDev := Capabilities{Simulate: &dtLimits, DeveloperAPI: true}
	cases := []struct {
		name  string
		n     SimulateNeeds
		caps  Capabilities
		wants bool
	}{
		{"one group on stock", SimulateNeeds{Groups: 1, ExtraOpcodeBudget: 320000}, stock, true},
		{"two groups on stock", SimulateNeeds{Groups: 2}, stock, false},
		{"two groups on dtalgorand", SimulateNeeds{Groups: 2}, dt, true},
		{"too many groups", SimulateNeeds{Groups: 17}, dt, false},
		{"rounds on stock", SimulateNeeds{Rounds: 1}, stock, false},
		{"rounds within limit", SimulateNeeds{Rounds: 8}, dt, true},
		{"too many rounds", SimulateNeeds{Rounds: 9}, dt, false},
		{"rounds disabled", SimulateNeeds{Rounds: 1}, Capabilities{Simulate: &SimulateLimits{MaxTxnGroups: 16}}, false},
		{"budget above stock", SimulateNeeds{ExtraOpcodeBudget: 320001}, stock, false},
		{"budget within dtalgorand", SimulateNeeds{ExtraOpcodeBudget: 1_000_000}, dt, true},
		{"budget above dtalgorand", SimulateNeeds{ExtraOpcodeBudget: 1_000_001}, dt, false},
		{"fork option on stock", SimulateNeeds{Fork: true}, stock, false},
		{"fork option on dtalgorand", SimulateNeeds{Fork: true}, dt, true},
		{"trace without dev", SimulateNeeds{DeveloperAPI: true}, dt, false},
		{"trace with dev", SimulateNeeds{DeveloperAPI: true}, dtDev, true},
		// A hand-written override of zeros allows what stock algod does.
		{"zero override", SimulateNeeds{Groups: 1, ExtraOpcodeBudget: 320000}, Capabilities{Simulate: &SimulateLimits{}}, true},
	}
	for _, c := range cases {
		if got := c.n.Fits(c.caps); got != c.wants {
			t.Errorf("%s: fits %v, want %v", c.name, got, c.wants)
		}
	}
}

func TestEligibleSimulateRounds(t *testing.T) {
	plain := Upstream{Health: HealthSynced, LastRound: 100, Caps: Capabilities{MaxAcctLookback: 4, Simulate: &dtLimits}}
	tt := plain
	tt.Caps.TimeTravel = true
	behind := tt
	behind.LastRound = 39
	c := simClass(SimulateBody{Rounds: []uint64{12, 40}})
	if Eligible(plain, c, 100, 2) {
		t.Fatal("a plain node outside its state window took the rounds")
	}
	if !Eligible(tt, c, 100, 2) {
		t.Fatal("time-travel node refused rounds it holds")
	}
	if Eligible(behind, c, 100, 2) {
		t.Fatal("a node short of the latest listed round took the rounds")
	}
	if !Eligible(plain, simClass(SimulateBody{Rounds: []uint64{97, 100}}), 100, 2) {
		t.Fatal("plain node refused rounds within its state window")
	}
}

func TestRelaxed(t *testing.T) {
	if r := Classify("GET", "/v2/status", "").Relaxed(); r != nil {
		t.Fatalf("plain request relaxed: %+v", r)
	}
	r := simClass(SimulateBody{Round: 7, TxnGroups: make([]struct{}, 2)}).Relaxed()
	if len(r) != 1 || r[0].SimNeeds != nil || r[0].Round == nil || *r[0].Round != 7 {
		t.Fatalf("single round keeps its round: %+v", r)
	}
	r = simClass(SimulateBody{Rounds: []uint64{12, 40}, TxnGroups: make([]struct{}, 2)}).Relaxed()
	if len(r) != 2 {
		t.Fatalf("rounds relaxations: %+v", r)
	}
	if r[0].Round != nil || r[0].RoundHigh != 0 || r[0].HistoryAnyRound || r[0].SimNeeds == nil || r[0].SimNeeds.Groups != 2 {
		t.Fatalf("first relaxation drops the rounds only: %+v %+v", r[0], r[0].SimNeeds)
	}
	if r[1].Round != nil || r[1].SimNeeds != nil {
		t.Fatalf("second relaxation drops every need: %+v", r[1])
	}
}

func TestFleetSimulateLimits(t *testing.T) {
	small := SimulateLimits{MaxTxnGroups: 4, MaxRounds: 16, MaxExtraOpcodeBudget: 320000, ResourceLimitMultiplier: 8}
	us := []Upstream{
		{ID: "stock", Health: HealthSynced, LastRound: 100},
		{ID: "dt", Health: HealthSynced, LastRound: 100, Caps: Capabilities{Simulate: &dtLimits, TimeTravel: true}},
		{ID: "small", Health: HealthSynced, LastRound: 100, Caps: Capabilities{Simulate: &small}},
		{ID: "down", Health: HealthOffline, LastRound: 100, Caps: Capabilities{Simulate: &SimulateLimits{MaxTxnGroups: 256}, DeveloperAPI: true}},
	}
	// dt allows more groups, small more rounds: the answer is dt's alone,
	// not small's rounds with dt's groups, which no node runs together.
	// The multiplier is the smallest, the stock node's.
	got, ok := FleetSimulateLimits(us, 100, 2)
	want := FleetSimulateLimitsReply{MaxTxnGroups: 16, MaxRounds: 8, MaxExtraOpcodeBudget: 1_000_000, ResourceLimitMultiplier: 1, TimeTravel: true}
	if !ok || got != want {
		t.Fatalf("fleet limits %+v %v, want %+v", got, ok, want)
	}
	// A node at least as large everywhere wins whatever the order.
	big := SimulateLimits{MaxTxnGroups: 16, MaxRounds: 16, MaxExtraOpcodeBudget: 1_000_000, ResourceLimitMultiplier: 8}
	more := append([]Upstream{{ID: "big", Health: HealthSynced, LastRound: 100, Caps: Capabilities{Simulate: &big, TimeTravel: true, DeveloperAPI: true}}}, us[1:]...)
	got, _ = FleetSimulateLimits(more, 100, 2)
	want = FleetSimulateLimitsReply{MaxTxnGroups: 16, MaxRounds: 16, MaxExtraOpcodeBudget: 1_000_000, ResourceLimitMultiplier: 4, TimeTravel: true, DeveloperAPI: true}
	if got != want {
		t.Fatalf("dominant node: %+v, want %+v", got, want)
	}
	// Equal limits: time travel breaks the tie.
	tied := []Upstream{
		{ID: "a", Health: HealthSynced, LastRound: 100, Caps: Capabilities{Simulate: &small}},
		{ID: "b", Health: HealthSynced, LastRound: 100, Caps: Capabilities{Simulate: &small, TimeTravel: true}},
	}
	if got, _ := FleetSimulateLimits(tied, 100, 2); !got.TimeTravel {
		t.Fatalf("tie: %+v", got)
	}
	if _, ok := FleetSimulateLimits(us[:1], 100, 2); ok {
		t.Fatal("a fleet of stock nodes reported limits")
	}
	if _, ok := FleetSimulateLimits(us[3:], 100, 2); ok {
		t.Fatal("an unreachable node's limits counted")
	}
}

func TestSimulateLimitsClamped(t *testing.T) {
	huge := Capabilities{Simulate: &SimulateLimits{MaxTxnGroups: 1000, MaxRounds: 1000, MaxExtraOpcodeBudget: 1 << 40, ResourceLimitMultiplier: 1 << 20}}
	want := SimulateLimits{MaxTxnGroups: 256, MaxRounds: 256, MaxExtraOpcodeBudget: 320_000_000, ResourceLimitMultiplier: 1024}
	if got := huge.SimulateLimits(); got != want {
		t.Fatalf("clamped %+v, want %+v", got, want)
	}
	if (SimulateNeeds{Groups: 257}).Fits(huge) {
		t.Fatal("an override promised more groups than the fork runs")
	}
}

// A rounds list with 0 asks for the node's own latest round, so a lagging
// node is out even though it holds the past rounds.
func TestEligibleSimulateRoundsWithLatest(t *testing.T) {
	lagging := Upstream{Health: HealthLagging, LastRound: 90, Caps: Capabilities{TimeTravel: true, Simulate: &dtLimits}}
	if !Eligible(lagging, simClass(SimulateBody{Rounds: []uint64{12, 40}}), 100, 2) {
		t.Fatal("lagging node refused past rounds it holds")
	}
	if Eligible(lagging, simClass(SimulateBody{Rounds: []uint64{12, 0}}), 100, 2) {
		t.Fatal("lagging node took a rounds list with the latest round")
	}
}

func TestCouldServe(t *testing.T) {
	down := Upstream{Health: HealthOffline, Draining: true, LastRound: 100, Stats: Stats{BreakerOpen: true}, Caps: Capabilities{Simulate: &dtLimits}}
	stock := Upstream{Health: HealthSynced, LastRound: 100}
	two := simClass(SimulateBody{TxnGroups: make([]struct{}, 2)})
	if Eligible(down, two, 100, 2) || !CouldServe([]Upstream{stock, down}, two) {
		t.Fatal("a fitting node that is out must count as able to serve")
	}
	if CouldServe([]Upstream{stock}, two) {
		t.Fatal("a stock node cannot serve two groups")
	}
	if CouldServe([]Upstream{down}, simClass(SimulateBody{Rounds: []uint64{5000}})) {
		t.Fatal("a node whose last round is short of the rounds cannot serve them")
	}
}
