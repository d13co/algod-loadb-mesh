package domain

// Eligible reports whether an upstream may serve a request of class c given
// the best round known across the fleet and the sync tolerance.
//
// The rules, in order: reachable; not draining; breaker closed; not
// throttled; synced (within tolerance of best) unless the class is local-only
// or the request is for a specific round the node holds; round window covers
// the requested round; developer API when needed; no broadcast to followers.
func Eligible(u Upstream, c RequestClass, bestRound, tolerance uint64) bool {
	if !u.Health.Reachable() || u.Draining {
		return false
	}
	if u.Stats.BreakerOpen || u.Stats.Throttled {
		return false
	}
	if c.LocalOnly && u.Kind != KindLocal {
		return false
	}
	if c.Round != nil {
		r := *c.Round
		if r < u.Caps.OldestRound || r > u.LastRound {
			return false
		}
		// A node that holds the round can serve it even while lagging.
	} else if !c.LocalOnly && !inSync(u, bestRound, tolerance) {
		return false
	}
	if c.WaitAfter != nil && *c.WaitAfter > u.LastRound+1 {
		// Waiting on a node that is behind the caller's round would block
		// for longer than necessary; prefer nodes at or beyond it.
		return false
	}
	if c.NeedsDev && !u.Caps.DeveloperAPI {
		return false
	}
	if c.Broadcast && u.Caps.FollowMode {
		return false
	}
	return true
}

// inSync trusts a Synced/Lagging judgement when one was made (it may carry
// hysteresis) and falls back to comparing rounds for a plain Online upstream.
func inSync(u Upstream, best, tol uint64) bool {
	switch u.Health {
	case HealthSynced:
		return true
	case HealthLagging:
		return false
	}
	return synced(u.LastRound, best, tol)
}

func synced(round, best, tol uint64) bool {
	return round+tol >= best
}

// SyncHealth judges Synced vs Lagging for a reachable upstream. Unreachable
// health values pass through unchanged.
func SyncHealth(h Health, round, best, tol uint64) Health {
	if !h.Reachable() {
		return h
	}
	if synced(round, best, tol) {
		return HealthSynced
	}
	return HealthLagging
}

// BestRound returns the highest last round among upstreams that are reachable.
// External upstreams count too: if the whole mesh is lagging behind the world
// we want to know.
func BestRound(us []Upstream) uint64 {
	var best uint64
	for _, u := range us {
		if u.Health.Reachable() && u.LastRound > best {
			best = u.LastRound
		}
	}
	return best
}
