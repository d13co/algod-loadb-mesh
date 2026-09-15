// Package clock provides the real clock and a controllable fake.
package clock

import (
	"context"
	"sync"
	"time"
)

// Real implements ports.Clock with the system clock.
type Real struct{}

func (Real) Now() time.Time { return time.Now() }

func (Real) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (Real) Tick(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(d)
	return t.C, t.Stop
}

// Fake is a manually advanced clock for service tests.
type Fake struct {
	mu      sync.Mutex
	now     time.Time
	waiters []fakeWaiter
	tickers []*fakeTicker
}

type fakeWaiter struct {
	at time.Time
	ch chan struct{}
}

type fakeTicker struct {
	period time.Duration
	next   time.Time
	ch     chan time.Time
	stop   bool
}

// NewFake starts at the given time.
func NewFake(start time.Time) *Fake { return &Fake{now: start} }

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *Fake) Sleep(ctx context.Context, d time.Duration) error {
	f.mu.Lock()
	w := fakeWaiter{at: f.now.Add(d), ch: make(chan struct{})}
	f.waiters = append(f.waiters, w)
	f.mu.Unlock()
	select {
	case <-w.ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *Fake) Tick(d time.Duration) (<-chan time.Time, func()) {
	f.mu.Lock()
	t := &fakeTicker{period: d, next: f.now.Add(d), ch: make(chan time.Time, 1)}
	f.tickers = append(f.tickers, t)
	f.mu.Unlock()
	return t.ch, func() {
		f.mu.Lock()
		t.stop = true
		f.mu.Unlock()
	}
}

// Advance moves time forward, waking sleepers and firing tickers in order.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	target := f.now.Add(d)
	for {
		// Find the earliest event not after target.
		var next time.Time
		found := false
		for _, w := range f.waiters {
			if !found || w.at.Before(next) {
				next, found = w.at, true
			}
		}
		for _, t := range f.tickers {
			if !t.stop && (!found || t.next.Before(next)) {
				next, found = t.next, true
			}
		}
		if !found || next.After(target) {
			break
		}
		f.now = next
		rest := f.waiters[:0]
		for _, w := range f.waiters {
			if !w.at.After(f.now) {
				close(w.ch)
			} else {
				rest = append(rest, w)
			}
		}
		f.waiters = rest
		for _, t := range f.tickers {
			if !t.stop && !t.next.After(f.now) {
				select {
				case t.ch <- f.now:
				default:
				}
				t.next = t.next.Add(t.period)
			}
		}
	}
	f.now = target
}
