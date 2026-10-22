package domain

// Eligible reports whether an upstream may serve a request of class c given
// the best round known across the fleet and the sync tolerance.
//
// The rules, in order: reachable; not draining; breaker closed; not
// throttled, nor busy for a heavy request; time-travel history when the request needs it; synced (within
// tolerance of best) unless the class is local-only or the request is for a
// specific round the node holds; the round is in the window the class needs
// (block or account state), and every round a simulation lists has been
// reached; developer API when needed; the simulate limits and options the
// request needs; no broadcast to followers.
func Eligible(u Upstream, c RequestClass, bestRound, tolerance uint64) bool {
	if !u.Health.Reachable() || u.Draining {
		return false
	}
	if u.Stats.BreakerOpen || u.Stats.Throttled || u.Stats.Busy && c.Heavy() {
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
		if r > u.LastRound || c.RoundHigh > u.LastRound {
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
		// A node that holds the round can serve it even while lagging,
		// unless a simulation also asks for the latest round.
		if c.SimNeeds != nil && c.SimNeeds.Latest && !inSync(u, bestRound, tolerance) {
			return false
		}
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
	if c.SimNeeds != nil && !c.SimNeeds.Fits(u.Caps) {
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

// CouldServe reports whether some upstream has what class c needs, whatever
// its state: reachable or not, draining, breaker open, lagging. Its last
// known round still has to cover the rounds c names. The router relaxes a
// simulate request only when nothing could serve it, not while the nodes
// that could are briefly out, which a client should hear as a 503 and retry.
func CouldServe(us []Upstream, c RequestClass) bool {
	for _, u := range us {
		u.Health, u.Draining, u.Stats = HealthSynced, false, Stats{}
		if Eligible(u, c, u.LastRound, 0) {
			return true
		}
	}
	return false
}

// FleetSimulateLimitsReply is GET /v2/transactions/simulate/limits, in
// dtalgorand's own form, answered for the fleet.
type FleetSimulateLimitsReply struct {
	DeveloperAPI            bool   `json:"developer-api"`
	MaxExtraOpcodeBudget    uint64 `json:"max-extra-opcode-budget"`
	MaxRounds               uint64 `json:"max-rounds"`
	MaxTxnGroups            uint64 `json:"max-txn-groups"`
	ResourceLimitMultiplier uint64 `json:"resource-limit-multiplier"`
	TimeTravel              bool   `json:"time-travel"`
	MaxExecTraceBytes       uint64 `json:"max-exec-trace-bytes"`
	TimeLimitSeconds        uint64 `json:"time-limit-seconds"`
}

// FleetSimulateLimits is what a simulate request sent to the mesh may ask
// for: the limits of one eligible dtalgorand node, in full, so that any
// request within them fits that node and the router can send it there. Limits
// combined across nodes could promise a request no single node runs (many
// groups from one, many rounds from another). The node is the one whose
// limits are largest, compared in order: groups, rounds, extra opcode budget,
// time travel, developer API, then its id for a stable answer. A node whose
// limits are all at least another's always wins. The resource limit
// multiplier is the smallest of every eligible node instead, stock ones
// included: no request asks for it, so the router may send a request to any
// of them, and that is what it is sure to get. ok is false when no eligible
// upstream reports limits, a fleet of stock algods.
func FleetSimulateLimits(us []Upstream, bestRound, tolerance uint64) (FleetSimulateLimitsReply, bool) {
	var best *Upstream
	var multiplier uint64
	plain := RequestClass{Simulate: true, Idempotent: true}
	for i := range us {
		u := &us[i]
		if !Eligible(*u, plain, bestRound, tolerance) {
			continue
		}
		if m := u.Caps.SimulateLimits().ResourceLimitMultiplier; multiplier == 0 || m < multiplier {
			multiplier = m
		}
		if u.Caps.Simulate != nil && (best == nil || simulateLimitsAbove(*u, *best)) {
			best = u
		}
	}
	if best == nil {
		return FleetSimulateLimitsReply{}, false
	}
	l := best.Caps.SimulateLimits()
	return FleetSimulateLimitsReply{MaxTxnGroups: l.MaxTxnGroups, MaxRounds: l.MaxRounds, MaxExtraOpcodeBudget: l.MaxExtraOpcodeBudget,
		ResourceLimitMultiplier: multiplier, TimeTravel: best.Caps.TimeTravel, DeveloperAPI: best.Caps.DeveloperAPI,
		MaxExecTraceBytes: l.MaxExecTraceBytes, TimeLimitSeconds: l.TimeLimitSeconds}, true
}

// simulateLimitsAbove orders nodes for FleetSimulateLimits: whether a's
// limits come before b's.
func simulateLimitsAbove(a, b Upstream) bool {
	la, lb := a.Caps.SimulateLimits(), b.Caps.SimulateLimits()
	switch {
	case la.MaxTxnGroups != lb.MaxTxnGroups:
		return la.MaxTxnGroups > lb.MaxTxnGroups
	case la.MaxRounds != lb.MaxRounds:
		return la.MaxRounds > lb.MaxRounds
	case la.MaxExtraOpcodeBudget != lb.MaxExtraOpcodeBudget:
		return la.MaxExtraOpcodeBudget > lb.MaxExtraOpcodeBudget
	case a.Caps.TimeTravel != b.Caps.TimeTravel:
		return a.Caps.TimeTravel
	case a.Caps.DeveloperAPI != b.Caps.DeveloperAPI:
		return a.Caps.DeveloperAPI
	}
	return a.ID < b.ID
}
