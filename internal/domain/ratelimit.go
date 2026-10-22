package domain

import (
	"fmt"
	"math"
	"time"
)

// Window is one allowance of a rate-limit plan: Capacity credits, refilled
// steadily over Period, so that a client who spends nothing is back to the
// full allowance after one period. A Capacity of 0 or less is no limit.
type Window struct {
	Capacity float64
	Period   time.Duration
}

// Limited reports whether the window limits anything.
func (w Window) Limited() bool { return w.Capacity > 0 && w.Period > 0 }

// rate is the refill in credits per second.
func (w Window) rate() float64 { return w.Capacity / w.Period.Seconds() }

// Bucket is one client's credits in one window. Level may be negative: a
// request billed more than was left leaves a debt that refills like the rest.
type Bucket struct {
	Level float64   `json:"level"`
	At    time.Time `json:"at"` // when Level was last brought up to date
}

// Refill brings b up to now: a new bucket starts full, and no bucket holds
// more than the capacity, even one saved under a larger plan.
func (w Window) Refill(b Bucket, now time.Time) Bucket {
	if !w.Limited() {
		return Bucket{At: now}
	}
	if b.At.IsZero() {
		return Bucket{Level: w.Capacity, At: now}
	}
	if dt := now.Sub(b.At); dt > 0 {
		b.Level += dt.Seconds() * w.rate()
		b.At = now
	}
	b.Level = min(b.Level, w.Capacity)
	return b
}

// Wait is how long until a bucket at level (refilled, less what is owed
// already) holds cost credits.
func (w Window) Wait(level, cost float64) time.Duration {
	if !w.Limited() || level >= cost {
		return 0
	}
	return time.Duration(math.Ceil((cost - level) / w.rate() * float64(time.Second)))
}

// UntilFull is how long until a bucket at level is back to the capacity.
func (w Window) UntilFull(level float64) time.Duration { return w.Wait(level, w.Capacity) }

// Full reports whether a refilled bucket holds the whole allowance, so that
// forgetting it loses nothing.
func (w Window) Full(b Bucket) bool { return !w.Limited() || b.Level >= w.Capacity }

// Plan is what one client may spend: credits per minute and per day, and what
// a simulate request may ask of a node.
type Plan struct {
	Name     string
	Minute   Window
	Day      Window
	Simulate SimulateCaps
}

// SimulateCaps bounds what one simulate request of a plan may ask for. A zero
// cap is no cap; the options are allowed unless denied.
type SimulateCaps struct {
	MaxGroups            int
	MaxRounds            int
	MaxExtraOpcodeBudget uint64
	DenyUnnamedResources bool
	DenyExecTrace        bool
}

// Check returns why a simulate request asking for n is beyond the caps, or ""
// when it is within them. A request that names one round with "round" asks
// for no rounds list, so MaxRounds does not apply to it.
func (c SimulateCaps) Check(n SimulateNeeds) string {
	switch {
	case c.MaxGroups > 0 && n.Groups > c.MaxGroups:
		return fmt.Sprintf("this plan allows at most %d transaction groups in one simulate request", c.MaxGroups)
	case c.MaxRounds > 0 && n.Rounds > c.MaxRounds:
		return fmt.Sprintf("this plan allows at most %d rounds in one simulate request", c.MaxRounds)
	case c.MaxExtraOpcodeBudget > 0 && n.ExtraOpcodeBudget > c.MaxExtraOpcodeBudget:
		return fmt.Sprintf("this plan allows an extra-opcode-budget of at most %d", c.MaxExtraOpcodeBudget)
	case c.DenyUnnamedResources && n.UnnamedResources:
		return "this plan does not allow allow-unnamed-resources"
	case c.DenyExecTrace && n.ExecTrace:
		return "this plan does not allow exec-trace-config"
	}
	return ""
}

// The cost keys: what a request is billed as before it runs. They are the
// stat keys (StatKey), with two heavy kinds of time-travel request split out.
const (
	CostVersions      = "versions"       // an object history page
	CostDeltasHistory = "deltas_history" // a state delta re-evaluated from the history
)

// CostKeys lists every key a CostWeights may set.
var CostKeys = []string{"default", "round", "timetravel", "simulate", "broadcast", "pending", "wait", "teal", CostVersions, CostDeltasHistory}

// DefaultCostWeights is what each kind of request costs unless configured.
var DefaultCostWeights = CostWeights{"default": 1, "round": 1, "timetravel": 2, "simulate": 5, "broadcast": 1,
	"pending": 1, "wait": 1, "teal": 1, CostVersions: 10, CostDeltasHistory: 50}

// recentDeltaRounds is how far behind the best round a state delta is still
// served from the node's own lookback (MaxAcctLookback, 4 by default) rather
// than re-evaluated from the history, with some margin.
const recentDeltaRounds = 8

// CostKey is what request c is billed as, best being the fleet's best round.
func CostKey(c RequestClass, best uint64) string {
	switch {
	case c.Versions:
		return CostVersions
	case c.Deltas && c.Round != nil && *c.Round+recentDeltaRounds < best:
		return CostDeltasHistory
	}
	return c.StatKey()
}

// CostWeights is the credits each cost key costs. A simulate request costs
// its weight once per group and round it asks for.
type CostWeights map[string]float64

// Base is what request c costs before it runs.
func (w CostWeights) Base(c RequestClass, best uint64) float64 {
	v := w[CostKey(c, best)]
	if n := c.SimNeeds; c.Simulate && n != nil {
		v *= float64(max(n.Groups, 1) * max(n.Rounds, 1))
	}
	return v
}
