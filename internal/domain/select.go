package domain

import (
	"math"
	"sort"
)

// Weights tunes the load-balancer score. Zero values are replaced by
// DefaultWeights in Score.
type Weights struct {
	ErrorPenaltyMS  float64 // added per unit of error rate (0..1)
	LagPenaltyMS    float64 // added per round of lag behind best
	InflightMS      float64 // added per in-flight request
	LocalBonusMS    float64 // subtracted for the local node (free network hop)
	UnknownLatMS    float64 // assumed latency when no sample exists
	ExternalPenalty float64 // added to external upstreams so the mesh wins ties
}

// DefaultWeights are sane starting values in milliseconds.
var DefaultWeights = Weights{
	ErrorPenaltyMS:  2000,
	LagPenaltyMS:    50,
	InflightMS:      5,
	LocalBonusMS:    2,
	UnknownLatMS:    20,
	ExternalPenalty: 100,
}

// Score is lower-is-better. It is a pure function of the snapshot.
func Score(u Upstream, c RequestClass, bestRound uint64, w Weights) float64 {
	if w == (Weights{}) {
		w = DefaultWeights
	}
	lat, ok := u.Stats.LatencyMS[c.StatKey()]
	if !ok {
		// Fall back to the default bucket, then to the unknown assumption.
		if lat, ok = u.Stats.LatencyMS["default"]; !ok {
			lat = w.UnknownLatMS
		}
	}
	s := lat
	s += w.ErrorPenaltyMS * u.Stats.ErrorRate
	if bestRound > u.LastRound {
		s += w.LagPenaltyMS * float64(bestRound-u.LastRound)
	}
	s += w.InflightMS * float64(u.Stats.Inflight)
	if u.Kind == KindLocal {
		s -= w.LocalBonusMS
	}
	if u.Kind == KindExternal {
		s += w.ExternalPenalty
	}
	return s
}

// Rand is the randomness the selector needs. math/rand/v2 satisfies it.
type Rand interface {
	IntN(n int) int
}

// Selection is the router's decision plus the information needed to retry.
type Selection struct {
	Chosen     Upstream
	Alternates []Upstream // remaining eligible upstreams, best first
	Tier       int
}

// Select picks an upstream for class c out of candidates according to mode.
//
// Tiers are consulted in order. The local node is its own tier ahead of
// everything in fallback mode; in loadbalancer mode it competes with peers of
// the same tier. Within a tier, fallback picks the best score
// deterministically (stable, no herd because it is one client per host) and
// loadbalancer uses power-of-two-choices on score.
func Select(mode Mode, cands []Upstream, c RequestClass, bestRound, tolerance uint64, w Weights, rnd Rand) (Selection, bool) {
	elig := make([]Upstream, 0, len(cands))
	for _, u := range cands {
		if Eligible(u, c, bestRound, tolerance) {
			u.Score = Score(u, c, bestRound, w)
			elig = append(elig, u)
		}
	}
	if len(elig) == 0 {
		return Selection{}, false
	}
	// A time-travel node is the tie-breaker against a peer sitting on the
	// edge of its state window: its LastRound comes from a heartbeat that can
	// trail the node by a round, so the round may already be unservable
	// there, while history answers it exactly.
	hasTT := false
	for _, u := range elig {
		if u.Caps.TimeTravel {
			hasTT = true
			break
		}
	}
	// Order: effective tier, then the edge preference, then score. Ties
	// broken by ID for determinism.
	sort.SliceStable(elig, func(i, j int) bool {
		ti, tj := effectiveTier(mode, elig[i]), effectiveTier(mode, elig[j])
		if ti != tj {
			return ti < tj
		}
		if hasTT {
			ei, ej := stateWindowEdge(elig[i], c), stateWindowEdge(elig[j], c)
			if ei != ej {
				return ej // a peer on the edge sorts after a time-travel node
			}
		}
		if elig[i].Score != elig[j].Score {
			return elig[i].Score < elig[j].Score
		}
		return elig[i].ID < elig[j].ID
	})
	top := effectiveTier(mode, elig[0])
	n := 0
	for n < len(elig) && effectiveTier(mode, elig[n]) == top {
		n++
	}
	idx := 0
	if mode == ModeLoadBalancer && n > 1 && rnd != nil {
		// Keep an edge peer out of the random pool when a time-travel node is
		// eligible, so power-of-two-choices cannot fall on the risky peer.
		pool := make([]int, 0, n)
		for i := 0; i < n; i++ {
			if hasTT && stateWindowEdge(elig[i], c) {
				continue
			}
			pool = append(pool, i)
		}
		if len(pool) == 0 {
			for i := 0; i < n; i++ {
				pool = append(pool, i)
			}
		}
		a := pool[rnd.IntN(len(pool))]
		b := pool[rnd.IntN(len(pool))]
		if elig[b].Score < elig[a].Score {
			a = b
		}
		idx = a
	}
	chosen := elig[idx]
	alts := make([]Upstream, 0, len(elig)-1)
	alts = append(alts, elig[:idx]...)
	alts = append(alts, elig[idx+1:]...)
	return Selection{Chosen: chosen, Alternates: alts, Tier: chosen.Tier}, true
}

// stateWindowEdge reports whether u is a non-local, non-time-travel node whose
// account-state window (or delta window) begins exactly at the requested
// round. It is a selection-only preference: eligible stays exact, and a fleet
// with no time-travel node still routes such a round to this node.
func stateWindowEdge(u Upstream, c RequestClass) bool {
	if c.Round == nil || c.RoundWindow == WindowBlock || u.Caps.TimeTravel || u.Kind == KindLocal {
		return false
	}
	if u.LastRound < u.Caps.MaxAcctLookback {
		return false // the window reaches genesis: no edge to fall off
	}
	oldest, ok := stateOldest(u, c.RoundWindow)
	return ok && *c.Round == oldest
}

// effectiveTier places the local node ahead of everything in fallback mode
// and externals behind everything in both modes.
func effectiveTier(mode Mode, u Upstream) int {
	switch u.Kind {
	case KindLocal:
		if mode == ModeFallback {
			return math.MinInt32
		}
		return u.Tier
	case KindExternal:
		return math.MaxInt32/2 + u.Tier
	}
	return u.Tier
}
