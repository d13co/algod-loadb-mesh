package app

import (
	"sync"
	"time"

	"github.com/d13co/algod-loadb-mesh/internal/domain"
	"github.com/d13co/algod-loadb-mesh/internal/ports"
)

// BreakerOptions tunes the per-upstream circuit breaker.
type BreakerOptions struct {
	Threshold int           // consecutive failures that open the breaker
	OpenFor   time.Duration // how long requests are refused
	Window    int           // outcomes kept for the error rate
	Alpha     float64       // EWMA weight of the newest latency sample
}

// DefaultBreaker is a conservative default.
var DefaultBreaker = BreakerOptions{Threshold: 5, OpenFor: 10 * time.Second, Window: 50, Alpha: 0.2}

// StatsBook records passive observations per upstream id and produces the
// domain.Stats snapshot the selector consumes.
type StatsBook struct {
	opts  BreakerOptions
	clock ports.Clock

	mu sync.Mutex
	m  map[string]*upstreamStats
}

type upstreamStats struct {
	latency        map[string]float64
	window         []bool // true = failed
	windowPos      int
	windowFull     bool
	inflight       int
	failures       int
	probing        bool
	openUntil      time.Time
	throttledUntil time.Time
}

// NewStatsBook creates an empty book.
func NewStatsBook(opts BreakerOptions, clock ports.Clock) *StatsBook {
	if opts.Threshold == 0 {
		opts = DefaultBreaker
	}
	return &StatsBook{opts: opts, clock: clock, m: map[string]*upstreamStats{}}
}

func (b *StatsBook) get(id string) *upstreamStats {
	s, ok := b.m[id]
	if !ok {
		s = &upstreamStats{latency: map[string]float64{}, window: make([]bool, b.opts.Window)}
		b.m[id] = s
	}
	return s
}

// Begin marks a request in flight; call the returned func when done.
func (b *StatsBook) Begin(id string) func() {
	b.mu.Lock()
	b.get(id).inflight++
	b.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			b.get(id).inflight--
			b.mu.Unlock()
		})
	}
}

// Record feeds one outcome for the given stat key.
func (b *StatsBook) Record(id, key string, out ports.Outcome) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.get(id)
	now := b.clock.Now()
	failed := out.Failed()
	s.window[s.windowPos] = failed
	s.windowPos = (s.windowPos + 1) % len(s.window)
	if s.windowPos == 0 {
		s.windowFull = true
	}
	if failed {
		s.failures++
		if s.failures >= b.opts.Threshold || s.probing {
			s.openUntil = now.Add(b.opts.OpenFor)
			s.probing = true
		}
		if out.Status == 429 {
			s.throttledUntil = now.Add(b.opts.OpenFor)
		}
		return
	}
	s.failures = 0
	s.probing = false
	ms := float64(out.Duration) / float64(time.Millisecond)
	if prev, ok := s.latency[key]; ok {
		s.latency[key] = prev + b.opts.Alpha*(ms-prev)
	} else {
		s.latency[key] = ms
	}
}

// Snapshot returns the current stats for an upstream.
func (b *StatsBook) Snapshot(id string) domain.Stats {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.m[id]
	if !ok {
		return domain.Stats{}
	}
	now := b.clock.Now()
	n := s.windowPos
	if s.windowFull {
		n = len(s.window)
	}
	fails := 0
	for i := 0; i < n; i++ {
		if s.window[i] {
			fails++
		}
	}
	lat := make(map[string]float64, len(s.latency))
	for k, v := range s.latency {
		lat[k] = v
	}
	st := domain.Stats{LatencyMS: lat, Inflight: s.inflight,
		BreakerOpen: now.Before(s.openUntil), Throttled: now.Before(s.throttledUntil)}
	if n > 0 {
		st.ErrorRate = float64(fails) / float64(n)
	}
	return st
}
