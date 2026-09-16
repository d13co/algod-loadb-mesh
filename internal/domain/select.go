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
	// Order: effective tier, then score. Ties broken by ID for determinism.
	sort.SliceStable(elig, func(i, j int) bool {
		ti, tj := effectiveTier(mode, elig[i]), effectiveTier(mode, elig[j])
		if ti != tj {
			return ti < tj
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
		a, b := rnd.IntN(n), rnd.IntN(n)
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
