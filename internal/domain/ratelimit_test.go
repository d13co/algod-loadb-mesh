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
