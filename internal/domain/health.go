package domain

import "time"

// ReturnHysteresis prevents flapping back onto an upstream that has just
// recovered: it must be observed Synced for N consecutive observations before
// the machine reports Synced again. It is a pure value type; callers hold it.
type ReturnHysteresis struct {
	Required int
	streak   int
	trusted  bool
}

// NewReturnHysteresis starts trusting the upstream immediately (the first
// outage costs nothing extra).
func NewReturnHysteresis(required int) ReturnHysteresis {
	return ReturnHysteresis{Required: required, trusted: true}
}

// Observe feeds one judgement and returns the health to publish.
func (h *ReturnHysteresis) Observe(judged Health) Health {
	if judged != HealthSynced {
		h.streak = 0
		h.trusted = false
		return judged
	}
	if h.trusted {
		return HealthSynced
	}
	h.streak++
	if h.streak >= h.Required {
		h.trusted = true
		return HealthSynced
	}
	return HealthLagging
}

// Streak reports progress towards trust, for status output.
func (h ReturnHysteresis) Streak() (int, bool) { return h.streak, h.trusted }

// Current reports the health the machine would publish with no new evidence.
func (h ReturnHysteresis) Current() Health {
	if h.trusted {
		return HealthSynced
	}
	return HealthLagging
}

// SyncJudge decides Synced vs Lagging for one upstream over time. Nodes
// receive each block some hundreds of milliseconds apart, so being one round
// behind is only lagging once it has lasted longer than Grace (or the gap is
// wider than Tolerance+1 rounds). Recovery goes through ReturnHysteresis,
// counted once per new round so that "N consecutive synced rounds" means
// rounds, not observations.
type SyncJudge struct {
	Tolerance uint64
	Grace     time.Duration
	hyst      ReturnHysteresis
	behind    time.Time
	lastRound uint64
	seen      bool
}

// NewSyncJudge creates a judge that trusts the upstream initially.
func NewSyncJudge(tolerance uint64, grace time.Duration, hysteresisRounds int) SyncJudge {
	return SyncJudge{Tolerance: tolerance, Grace: grace, hyst: NewReturnHysteresis(hysteresisRounds)}
}

// Judge feeds one observation and returns the health to publish.
func (j *SyncJudge) Judge(now time.Time, reachable bool, round, best uint64) Health {
	if !reachable {
		j.behind = time.Time{}
		if !j.seen {
			// Never seen up: not an outage, no hysteresis debt on first sight.
			return HealthOffline
		}
		return j.hyst.Observe(HealthOffline)
	}
	lagging := false
	if best > round+j.Tolerance {
		if j.behind.IsZero() {
			j.behind = now
		}
		lagging = best > round+j.Tolerance+1 || now.Sub(j.behind) >= j.Grace
	} else {
		j.behind = time.Time{}
	}
	if lagging {
		return j.hyst.Observe(HealthLagging)
	}
	if !j.seen || round != j.lastRound {
		j.seen, j.lastRound = true, round
		return j.hyst.Observe(HealthSynced)
	}
	return j.hyst.Current()
}

// Streak exposes hysteresis progress for status output.
func (j SyncJudge) Streak() (int, bool) { return j.hyst.Streak() }
