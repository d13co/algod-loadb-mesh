package app

import (
	"context"
	"testing"
	"time"

	"github.com/d13co/algod-loadb-mesh/internal/adapters/algodhttp"
	"github.com/d13co/algod-loadb-mesh/internal/adapters/clock"
	"github.com/d13co/algod-loadb-mesh/internal/adapters/logging"
	"github.com/d13co/algod-loadb-mesh/internal/adapters/metrics"
	"github.com/d13co/algod-loadb-mesh/internal/domain"
	"github.com/d13co/algod-loadb-mesh/internal/fakealgod"
)

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func TestMonitorFollowsNodeAndVerifiesOldestRound(t *testing.T) {
	// config.json claims full archival, but the node only serves from 3000.
	n := fakealgod.New(fakealgod.Options{ID: "a", StartRound: 10000, OldestRound: 3000})
	defer n.Close()
	m := NewMonitor(MonitorOptions{NodeID: "a", WaitTimeout: 2 * time.Second},
		algodhttp.New(n.URL(), n.Token(), nil), fakealgod.ConfigReader{Node: n, Archival: true}, clock.Real{}, logging.Nop{}, metrics.New())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	waitFor(t, 5*time.Second, func() bool { return m.State().Online && m.State().Verified })
	st := m.State()
	if st.LastRound != 10000 || st.Caps.OldestRound != 3000 || st.Caps.Archival.Kind != domain.ArchivalSince {
		t.Fatalf("state %+v", st)
	}
	if hits := n.Hits("/v2/blocks/"); hits > 20 {
		t.Fatalf("binary search used %d requests", hits)
	}

	// Coalesced waiting: many waiters, one long-poll.
	done := make(chan []byte, 5)
	for i := 0; i < 5; i++ {
		go func() {
			b, _ := m.WaitForBlockAfter(ctx, 10000)
			done <- b
		}()
	}
	time.Sleep(50 * time.Millisecond)
	n.Advance(1)
	for i := 0; i < 5; i++ {
		select {
		case b := <-done:
			if len(b) == 0 {
				t.Fatal("empty status body")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("waiter stuck")
		}
	}
	waitFor(t, time.Second, func() bool { return m.State().LastRound == 10001 })
	if hits := n.Hits("/v2/status/wait-for-block-after"); hits > 3 {
		t.Fatalf("expected one long-poll per round, saw %d", hits)
	}

	// Node goes away: monitor reports offline, then recovers.
	n.SetFailing(true)
	waitFor(t, 5*time.Second, func() bool { return !m.State().Online })
	n.SetFailing(false)
	waitFor(t, 5*time.Second, func() bool { return m.State().Online })
}

func TestMonitorTrailingWindow(t *testing.T) {
	n := fakealgod.New(fakealgod.Options{ID: "t", StartRound: 5000, OldestRound: 4000})
	defer n.Close()
	m := NewMonitor(MonitorOptions{NodeID: "t", WaitTimeout: time.Second},
		algodhttp.New(n.URL(), n.Token(), nil), fakealgod.ConfigReader{Node: n, Lookback: 1000}, clock.Real{}, logging.Nop{}, metrics.New())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)
	waitFor(t, 5*time.Second, func() bool { return m.State().Verified })
	if st := m.State(); st.Caps.OldestRound != 4000 || st.Caps.Archival.Kind != domain.ArchivalTrailing {
		t.Fatalf("%+v", st.Caps)
	}
	n.SetOldest(4010)
	n.Advance(10)
	waitFor(t, 2*time.Second, func() bool { return m.State().Caps.OldestRound == 4010 })
}

func TestStatsBreaker(t *testing.T) {
	fc := clock.NewFake(time.Unix(0, 0))
	b := NewStatsBook(BreakerOptions{Threshold: 2, OpenFor: 5 * time.Second, Window: 10, Alpha: 0.5}, fc)
	ok := func(ms int) { b.Record("u", "default", outcome(200, ms)) }
	bad := func() { b.Record("u", "default", outcome(503, 1)) }
	ok(10)
	ok(30)
	if s := b.Snapshot("u"); s.LatencyMS["default"] != 20 || s.ErrorRate != 0 || s.BreakerOpen {
		t.Fatalf("%+v", s)
	}
	bad()
	if b.Snapshot("u").BreakerOpen {
		t.Fatal("one failure must not open")
	}
	bad()
	if s := b.Snapshot("u"); !s.BreakerOpen || s.ErrorRate != 0.5 {
		t.Fatalf("%+v", s)
	}
	fc.Advance(6 * time.Second)
	if b.Snapshot("u").BreakerOpen {
		t.Fatal("breaker should be half-open after OpenFor")
	}
	bad()
	if !b.Snapshot("u").BreakerOpen {
		t.Fatal("a failure while probing must re-open immediately")
	}
	fc.Advance(6 * time.Second)
	ok(10)
	if b.Snapshot("u").BreakerOpen {
		t.Fatal("success closes")
	}
	release := b.Begin("u")
	if b.Snapshot("u").Inflight != 1 {
		t.Fatal("inflight")
	}
	release()
	release()
	if b.Snapshot("u").Inflight != 0 {
		t.Fatal("inflight release is idempotent")
	}
}
