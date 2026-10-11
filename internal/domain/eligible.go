package domain

// Eligible reports whether an upstream may serve a request of class c given
// the best round known across the fleet and the sync tolerance.
//
// The rules, in order: reachable; not draining; breaker closed; not
// throttled; time-travel history when the request needs it; synced (within
// tolerance of best) unless the class is local-only or the request is for a
// specific round the node holds; the round is in the window the class needs
// (block or account state); developer API when needed; no broadcast to
// followers.
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
	if c.TimeTravel && !u.Caps.TimeTravel {
		return false
	}
	if c.Round != nil {
		r := *c.Round
		if r > u.LastRound {
			return false
		}
		switch {
		case u.Caps.TimeTravel && c.HistoryAnyRound:
			// The history covers every round at or before the node's last.
		case u.Caps.TimeTravel:
			// History does not extend the block window here (state deltas,
			// full blocks): the stored block is still required.
			if r < u.Caps.OldestRound {
				return false
			}
		case c.RoundWindow != WindowBlock:
			if !stateWindowCovers(u, r, c.RoundWindow) {
				return false
			}
		default:
			if r < u.Caps.OldestRound {
				return false
			}
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

// stateWindowCovers reports whether a plain node answers a state question at
// round r in window w. A zero lookback (a heartbeat from an older agent, or a
// config that omits the key) falls back to the block window rather than
// treating the state window as empty.
func stateWindowCovers(u Upstream, r uint64, w RoundWindow) bool {
	oldest, ok := stateOldest(u, w)
	if !ok {
		return r >= u.Caps.OldestRound
	}
	return r >= oldest
}

// stateOldest returns the oldest round a plain node answers in state window w:
// the ledger answers Latest-MaxAcctLookback..Latest, the oldest round from
// disk, but keeps a delta only for the rounds after it. ok is false when the
// lookback is unknown (zero).
func stateOldest(u Upstream, w RoundWindow) (oldest uint64, ok bool) {
	lb := u.Caps.MaxAcctLookback
	if lb == 0 {
		return 0, false
	}
	if u.LastRound > lb {
		oldest = u.LastRound - lb // otherwise the window reaches genesis
	}
	if w == WindowDelta {
		oldest++
	}
	return oldest, true
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
