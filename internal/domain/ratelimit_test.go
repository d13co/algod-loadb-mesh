package domain

import (
	"strings"
	"testing"
	"time"
)

func TestWindowRefill(t *testing.T) {
	w := Window{Capacity: 60, Period: time.Minute} // one credit per second
	t0 := time.Unix(1000, 0)
	b := w.Refill(Bucket{}, t0)
	if b.Level != 60 || !b.At.Equal(t0) {
		t.Fatalf("new bucket %+v, want full at t0", b)
	}
	b.Level = -5 // a debt refills like the rest
	if b = w.Refill(b, t0.Add(10*time.Second)); b.Level != 5 {
		t.Fatalf("after 10s: %v, want 5", b.Level)
	}
	if b = w.Refill(b, t0.Add(time.Hour)); b.Level != 60 {
		t.Fatalf("after an hour: %v, want the capacity", b.Level)
	}
	// Saved under a larger plan: never more than this one's capacity.
	if b = w.Refill(Bucket{Level: 500, At: t0}, t0); b.Level != 60 {
		t.Fatalf("over capacity: %v", b.Level)
	}
	if !w.Full(b) || w.Full(Bucket{Level: 59}) {
		t.Fatal("Full")
	}
	if (Window{}).Limited() || !(Window{}).Full(Bucket{}) {
		t.Fatal("a zero window limits nothing")
	}
}

func TestWindowWait(t *testing.T) {
	w := Window{Capacity: 60, Period: time.Minute}
	if d := w.Wait(10, 5); d != 0 {
		t.Fatalf("enough credits: wait %v", d)
	}
	if d := w.Wait(-2, 3); d != 5*time.Second {
		t.Fatalf("5 short at 1/s: wait %v", d)
	}
	if d := w.UntilFull(30); d != 30*time.Second {
		t.Fatalf("until full: %v", d)
	}
	if d := (Window{}).Wait(-100, 1); d != 0 {
		t.Fatalf("unlimited: wait %v", d)
	}
}

func TestSimulateCapsCheck(t *testing.T) {
	caps := SimulateCaps{MaxGroups: 2, MaxRounds: 4, MaxExtraOpcodeBudget: 1000, DenyExecTrace: true, DenyUnnamedResources: true}
	for _, tc := range []struct {
		n    SimulateNeeds
		want string // a word of the refusal; "" for none
	}{
		{SimulateNeeds{Groups: 2, Rounds: 4, ExtraOpcodeBudget: 1000}, ""},
		{SimulateNeeds{Groups: 3}, "groups"},
		{SimulateNeeds{Groups: 1, Rounds: 5}, "rounds"},
		{SimulateNeeds{Groups: 1, ExtraOpcodeBudget: 1001}, "extra-opcode-budget"},
		{SimulateNeeds{Groups: 1, ExecTrace: true}, "exec-trace"},
		{SimulateNeeds{Groups: 1, UnnamedResources: true}, "unnamed"},
	} {
		got := caps.Check(tc.n)
		if (got == "") != (tc.want == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%+v: %q, want %q", tc.n, got, tc.want)
		}
	}
	if got := (SimulateCaps{}).Check(SimulateNeeds{Groups: 256, Rounds: 256, ExtraOpcodeBudget: 1 << 40, ExecTrace: true, UnnamedResources: true}); got != "" {
		t.Errorf("no caps: %q", got)
	}
}

func TestCostBase(t *testing.T) {
	w := DefaultCostWeights
	sim := func(body SimulateBody) RequestClass {
		c := Classify("POST", "/v2/transactions/simulate", "")
		c.ApplySimulate(body)
		return c
	}
	groups := func(n int) []struct{} { return make([]struct{}, n) }
	for _, tc := range []struct {
		name  string
		class RequestClass
		key   string
		cost  float64
	}{
		{"status", Classify("GET", "/v2/status", ""), "default", 1},
		{"block", Classify("GET", "/v2/blocks/990", ""), "round", 1},
		{"account at a round", Classify("GET", "/v2/accounts/A", "round=5"), "timetravel", 2},
		{"versions", Classify("GET", "/v2/accounts/A/versions", ""), CostVersions, 10},
		{"recent delta", Classify("GET", "/v2/deltas/995", ""), "round", 1},
		{"old delta", Classify("GET", "/v2/deltas/500", ""), CostDeltasHistory, 50},
		{"one group", sim(SimulateBody{TxnGroups: groups(1)}), "simulate", 5},
		{"groups by rounds", sim(SimulateBody{TxnGroups: groups(3), Rounds: []uint64{10, 20}}), "simulate", 30},
		{"unread body", Classify("POST", "/v2/transactions/simulate", ""), "simulate", 5},
	} {
		if k := CostKey(tc.class, 1000); k != tc.key {
			t.Errorf("%s: key %q, want %q", tc.name, k, tc.key)
		}
		if c := w.Base(tc.class, 1000); c != tc.cost {
			t.Errorf("%s: cost %v, want %v", tc.name, c, tc.cost)
		}
	}
	for _, k := range CostKeys {
		if _, ok := DefaultCostWeights[k]; !ok {
			t.Errorf("no default weight for %q", k)
		}
	}
}

func TestParseEffort(t *testing.T) {
	const rest = ", history=0, scanned=0, trace-bytes=0"
	for _, tc := range []struct {
		v    string
		want Effort
		ok   bool
	}{
		{"opcodes=184220, lookups=912, history=640, scanned=0, trace-bytes=0, eval-us=5310",
			Effort{Opcodes: 184220, Lookups: 912, History: 640, EvalUS: 5310}, true},
		{"trace-bytes=9,scanned=2,history=1,lookups=3,opcodes=7", Effort{7, 3, 1, 2, 9, 0}, true},         // order and spacing are free; eval-us may be left out
		{"opcodes=7;x=1, future=?1, other=\"s\", lookups=1" + rest, Effort{Opcodes: 7, Lookups: 1}, true}, // parameters and unknown keys are ignored
		{"", Effort{}, false},
		{" , ", Effort{}, false},
		{"foo=1", Effort{}, false}, // no billed keys
		{"opcodes=7, lookups=3", Effort{}, false},
		{"opcodes=-1, lookups=1" + rest, Effort{}, false},
		{"opcodes=1.5, lookups=1" + rest, Effort{}, false},
		{"opcodes, lookups=1" + rest, Effort{}, false},              // a boolean, not a count
		{"opcodes;a=1, lookups=1" + rest, Effort{}, false},          // likewise with a parameter
		{"opcodes=9, opcodes=0, lookups=1" + rest, Effort{}, false}, // a key twice
		{"opcodes =5, lookups=1" + rest, Effort{}, false},
		{"opcodes=abc, lookups=1" + rest, Effort{}, false},
	} {
		got, ok := ParseEffort(tc.v)
		if ok != tc.ok || got != tc.want {
			t.Errorf("%q: %+v %v, want %+v %v", tc.v, got, ok, tc.want, tc.ok)
		}
	}
}

func TestEffortCredits(t *testing.T) {
	r := EffortRates{OpcodesPerCredit: 1000, LookupsPerCredit: 100, HistoryPerCredit: 10, ScannedPerCredit: 50, TraceKBPerCredit: 1}
	e := Effort{Opcodes: 2000, Lookups: 200, History: 20, Scanned: 100, TraceBytes: 2048, EvalUS: 1 << 30}
	if c := r.Credits(e); c != 2+2+2+2+2 {
		t.Fatalf("credits %v, want 10 (eval-us is not billed)", c)
	}
	if c := (EffortRates{}).Credits(e); c != 0 {
		t.Fatalf("no rates: %v", c)
	}
	if s := e.Add(e); s.Opcodes != 4000 || s.TraceBytes != 4096 {
		t.Fatalf("add: %+v", s)
	}
}
