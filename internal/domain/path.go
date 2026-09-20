package domain

import (
	"net"
	"time"
)

// PathStat is one candidate address of a link, as the policy sees it.
type PathStat struct {
	Addr  string
	RTT   time.Duration // EWMA, 0 until measured
	Alive bool          // routable, fewer than PathFailures consecutive losses, not deaf
	Deaf  bool          // the peer said it is not hearing us on it; implies !Alive
}

// PathFailures is how many consecutive unanswered pings make a path dead.
// Exported because the application counts the losses and applies it.
const PathFailures = 3

// pathSwitchMargin is how much faster a challenger must be before a working
// path is abandoned for it. Not configuration: nobody tunes it.
const pathSwitchMargin = 0.25

// PathPolicy picks which of a link's addresses carries the heartbeats. It
// mirrors SyncJudge: pure, clock-fed, unit-testable.
type PathPolicy struct {
	ProbeInterval time.Duration // the periodic per-path ping cadence
}

// Choose returns the index to use and why: initial | dead | deaf | faster |
// keep | none. A dead, deaf or vanished current path is left at once, for the
// best alive one or -1. A merely slower one is left only for a measured path
// faster by the margin, and only 2*ProbeInterval after the last switch (at
// since), so two consecutive probes must agree. Measured paths rank before
// unmeasured ones; with nothing measured the lowest index — registry order —
// wins, so the first heartbeat never waits for an RTT sample.
func (p PathPolicy) Choose(now time.Time, stats []PathStat, cur int, since time.Time) (int, string) {
	best := -1
	for i, s := range stats {
		if !s.Alive {
			continue
		}
		if best < 0 || better(s, stats[best]) {
			best = i
		}
	}
	if cur < 0 || cur >= len(stats) {
		if best < 0 {
			return -1, "none"
		}
		return best, "initial"
	}
	if c := stats[cur]; !c.Alive {
		if c.Deaf {
			return best, "deaf"
		}
		return best, "dead"
	}
	if best < 0 || best == cur {
		return cur, "keep"
	}
	c, b := stats[cur], stats[best]
	if c.RTT > 0 && b.RTT > 0 && float64(b.RTT) < float64(c.RTT)*(1-pathSwitchMargin) && now.Sub(since) >= 2*p.ProbeInterval {
		return best, "faster"
	}
	return cur, "keep"
}

// better orders two alive paths: measured before unmeasured, then by RTT.
func better(a, b PathStat) bool {
	switch {
	case a.RTT > 0 && b.RTT > 0:
		return a.RTT < b.RTT
	case a.RTT > 0:
		return true
	default:
		return false
	}
}

// NormalizeAddr rewrites host:port so that the same address always compares
// equal: an IPv4 address that arrived as IPv4-mapped IPv6 ([::ffff:a.b.c.d])
// on a dual-stack socket becomes a.b.c.d. Anything unparseable is returned
// as it came.
func NormalizeAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return addr
	}
	if ip4 := ip.To4(); ip4 != nil {
		ip = ip4
	}
	return net.JoinHostPort(ip.String(), port)
}
