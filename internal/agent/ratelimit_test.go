package agent

import (
	"testing"
	"time"

	"github.com/d13co/algod-loadb-mesh/internal/config"
)

// The ratelimit section reaches the router as the limiter's options; off,
// there is no limiter.
func TestLimiterOptions(t *testing.T) {
	if limiterOptions(config.RateLimit{}) != nil {
		t.Fatal("disabled section gave a limiter")
	}
	no, anon, rate := false, "free", 20.0
	r := config.RateLimit{Enabled: true, AnonymousPlan: &anon, SimulatePerSecond: &rate, IPv6Prefix: 56, MaxKeys: 9,
		TrustedProxies: []string{"10.0.0.0/8"}, Weights: map[string]float64{"default": 2},
		Plans: map[string]config.RatePlan{"free": {PerMinute: 300, PerDay: 50000,
			Simulate: config.SimulateCaps{MaxGroups: 1, MaxRounds: 2, MaxExtraOpcodeBudget: 3, ExecTrace: &no}}},
		Tokens: []config.RateToken{{Name: "acme", Token: "s", Plan: "free"}}}
	o := limiterOptions(r)
	p := o.Plans["free"]
	if p.Minute.Capacity != 300 || p.Minute.Period != time.Minute || p.Day.Capacity != 50000 || p.Day.Period != 24*time.Hour {
		t.Fatalf("windows: %+v", p)
	}
	if s := p.Simulate; s.MaxGroups != 1 || s.MaxRounds != 2 || s.MaxExtraOpcodeBudget != 3 || !s.DenyExecTrace || s.DenyUnnamedResources {
		t.Fatalf("simulate caps: %+v", s)
	}
	if o.AnonymousPlan != "free" || o.SimulatePerSecond != 20 || o.IPv6Prefix != 56 || o.MaxKeys != 9 || len(o.TrustedProxies) != 1 ||
		o.Weights["default"] != 2 || len(o.Tokens) != 1 || o.Tokens[0].Token != "s" {
		t.Fatalf("options: %+v", o)
	}
}
