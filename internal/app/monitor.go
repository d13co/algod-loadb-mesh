package app

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/d13co/algod-loadb-mesh/internal/domain"
	"github.com/d13co/algod-loadb-mesh/internal/ports"
)

// MonitorOptions configures the local node monitor.
type MonitorOptions struct {
	NodeID         string
	WaitTimeout    time.Duration // bound on the status-after long-poll; above algod's own timeout
	StatusTimeout  time.Duration
	MaxBackoff     time.Duration
	VerifyInterval time.Duration // empirical re-check of oldest round
	ConfigRecheck  time.Duration // how often config.json's mtime is polled
	Overrides      *domain.CapabilityOverrides
	// Absent means there is no co-located node (a balancer): the monitor
	// only reports Network as the genesis id and never goes online.
	Absent  bool
	Network string
}

func (o *MonitorOptions) defaults() {
	if o.WaitTimeout == 0 {
		// Above algod's own 60s wait-for-block timeout, so that hitting this
		// one means algod is wedged rather than the chain stalled.
		o.WaitTimeout = 70 * time.Second
	}
	if o.StatusTimeout == 0 {
		o.StatusTimeout = 10 * time.Second
	}
	if o.MaxBackoff == 0 {
		o.MaxBackoff = 90 * time.Second
	}
	if o.VerifyInterval == 0 {
		o.VerifyInterval = time.Hour
	}
	if o.ConfigRecheck == 0 {
		o.ConfigRecheck = time.Minute
	}
}

// LocalState is everything the monitor knows, as a value.
type LocalState struct {
	Online     bool                `json:"online"`
	LastRound  uint64              `json:"last_round"`
	Caps       domain.Capabilities `json:"caps"`
	Config     ports.NodeConfig    `json:"-"`
	ConfigOK   bool                `json:"config_ok"`
	Verified   bool                `json:"verified"` // oldest round confirmed empirically
	LastStatus []byte              `json:"-"`
	LastError  string              `json:"last_error,omitempty"`
	Endpoint   string              `json:"endpoint"`
	Seq        uint64              `json:"seq"` // increments on every change
}

// Monitor follows the co-located algod with one status-after long-poll and
// derives its capabilities from the data directory plus an empirical probe.
type Monitor struct {
	opts   MonitorOptions
	algod  ports.AlgodClient
	cfgr   ports.NodeConfigReader
	clock  ports.Clock
	log    ports.Logger
	metric ports.Metrics

	mu      sync.Mutex
	state   LocalState
	changed chan struct{}
	verify  chan struct{} // request an empirical verification
}

// NewMonitor wires the monitor; nothing runs until Run.
func NewMonitor(o MonitorOptions, algod ports.AlgodClient, cfgr ports.NodeConfigReader, clock ports.Clock, log ports.Logger, metric ports.Metrics) *Monitor {
	o.defaults()
	m := &Monitor{opts: o, algod: algod, cfgr: cfgr, clock: clock, log: log, metric: metric,
		changed: make(chan struct{}), verify: make(chan struct{}, 1)}
	if o.Absent {
		m.state.Caps.GenesisID = o.Network
		m.state.LastError = "no local node"
	}
	return m
}

// State returns a copy of the current state.
func (m *Monitor) State() LocalState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// Changed returns a channel closed on the next state change.
func (m *Monitor) Changed() <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.changed
}

func (m *Monitor) update(f func(s *LocalState)) {
	m.mu.Lock()
	f(&m.state)
	m.state.Seq++
	close(m.changed)
	m.changed = make(chan struct{})
	m.mu.Unlock()
}

// WaitForBlockAfter resolves once the local node is past round, replaying the
// latest status body. Any number of callers cost zero extra algod requests.
func (m *Monitor) WaitForBlockAfter(ctx context.Context, round uint64) ([]byte, error) {
	for {
		m.mu.Lock()
		st, ch := m.state, m.changed
		m.mu.Unlock()
		if st.LastRound > round && st.LastStatus != nil {
			return st.LastStatus, nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return st.LastStatus, ctx.Err()
		}
	}
}

// Run blocks until ctx is done.
func (m *Monitor) Run(ctx context.Context) error {
	if m.opts.Absent {
		<-ctx.Done()
		return ctx.Err()
	}
	m.readConfig()
	go m.configLoop(ctx)
	go m.verifyLoop(ctx)
	backoff := newBackoff(time.Second, m.opts.MaxBackoff)
	for ctx.Err() == nil {
		if !m.awaitOnline(ctx, backoff) {
			return ctx.Err()
		}
		m.requestVerify()
		backoff.reset()
		m.watch(ctx)
		if ctx.Err() == nil {
			m.metric.Inc("loadb_local_disconnects")
			_ = m.clock.Sleep(ctx, backoff.next())
		}
	}
	return ctx.Err()
}

func (m *Monitor) readConfig() {
	cfg, err := m.cfgr.Read()
	if err != nil {
		m.log.Warn("node config unavailable", "err", err)
		m.update(func(s *LocalState) { s.ConfigOK = false; s.LastError = err.Error() })
		return
	}
	m.update(func(s *LocalState) {
		changed := s.Config.ModTime != cfg.ModTime || !s.ConfigOK
		s.Config, s.ConfigOK, s.Endpoint = cfg, true, cfg.Endpoint
		s.Caps = capsFromConfig(cfg, s.Caps, s.LastRound, m.opts.Overrides)
		if changed {
			s.Verified = false
		}
	})
	m.log.Info("node config", "endpoint", cfg.Endpoint, "genesis", cfg.GenesisID, "archival", cfg.Archival,
		"lookback", cfg.MaxBlockHistoryLookback, "dev", cfg.EnableDeveloperAPI, "follow", cfg.EnableFollowMode)
}

func capsFromConfig(cfg ports.NodeConfig, prev domain.Capabilities, lastRound uint64, ov *domain.CapabilityOverrides) domain.Capabilities {
	c := domain.Capabilities{
		DeveloperAPI: cfg.EnableDeveloperAPI, FollowMode: cfg.EnableFollowMode, ExperimentalAPI: cfg.EnableExperimentalAPI,
		GenesisID: cfg.GenesisID, StorageEngine: cfg.StorageEngine, MaxAcctLookback: cfg.MaxAcctLookback,
		AlgodVersion: prev.AlgodVersion,
	}
	switch {
	case cfg.Archival:
		c.Archival = domain.Archival{Kind: domain.ArchivalFull}
	case cfg.MaxBlockHistoryLookback > 0:
		c.Archival = domain.Archival{Kind: domain.ArchivalTrailing, N: cfg.MaxBlockHistoryLookback}
	default:
		c.Archival = domain.Archival{Kind: domain.ArchivalNone}
	}
	c.OldestRound = domain.OldestRoundFor(c.Archival, lastRound)
	return ov.Apply(c, lastRound)
}

func (m *Monitor) configLoop(ctx context.Context) {
	tick, stop := m.clock.Tick(m.opts.ConfigRecheck)
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			cfg, err := m.cfgr.Read()
			if err != nil {
				continue
			}
			st := m.State()
			if !st.ConfigOK || cfg.ModTime != st.Config.ModTime || cfg.Endpoint != st.Config.Endpoint {
				m.log.Info("node config changed, re-reading")
				m.readConfig()
				m.requestVerify()
			}
		}
	}
}

func (m *Monitor) awaitOnline(ctx context.Context, b *backoff) bool {
	for ctx.Err() == nil {
		sctx, cancel := context.WithTimeout(ctx, m.opts.StatusTimeout)
		st, err := m.algod.Status(sctx)
		cancel()
		if err == nil {
			ver, verr := m.algod.Versions(ctx)
			m.update(func(s *LocalState) {
				s.Online, s.LastRound, s.LastStatus, s.LastError = true, st.LastRound, st.Raw, ""
				if verr == nil {
					s.Caps.AlgodVersion = ver.Build
					if s.Caps.GenesisID == "" {
						s.Caps.GenesisID = ver.GenesisID
					}
				}
				s.Caps.OldestRound = domain.OldestRoundFor(s.Caps.Archival, s.LastRound)
			})
			m.log.Info("local node online", "round", st.LastRound)
			return true
		}
		m.update(func(s *LocalState) { s.Online = false; s.LastError = err.Error() })
		d := b.next()
		m.log.Warn("local node not reachable", "err", err, "retry_in", d)
		if m.clock.Sleep(ctx, d) != nil {
			return false
		}
	}
	return false
}

// watch is the status-after loop. It returns on the first error so the
// caller can back off and re-establish.
func (m *Monitor) watch(ctx context.Context) {
	for ctx.Err() == nil {
		last := m.State().LastRound
		wctx, cancel := context.WithTimeout(ctx, m.opts.WaitTimeout)
		st, err := m.algod.WaitForBlockAfter(wctx, last)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, context.DeadlineExceeded) {
				// algod answers within its own timeout normally; a client-side
				// timeout means it is wedged or the network is gone.
				m.log.Warn("wait-for-block-after timed out", "round", last)
			} else {
				m.log.Warn("wait-for-block-after failed", "err", err)
			}
			m.update(func(s *LocalState) { s.Online = false; s.LastError = err.Error() })
			return
		}
		m.metric.Inc("loadb_local_rounds")
		m.log.Debug("round", "round", st.LastRound)
		m.update(func(s *LocalState) {
			s.Online, s.LastRound, s.LastStatus, s.LastError = true, st.LastRound, st.Raw, ""
			s.Caps.OldestRound = domain.OldestRoundFor(s.Caps.Archival, s.LastRound)
			s.Caps = m.opts.Overrides.Apply(s.Caps, s.LastRound)
		})
	}
}

func (m *Monitor) requestVerify() {
	select {
	case m.verify <- struct{}{}:
	default:
	}
}

// verifyLoop measures the oldest servable round on request and on a timer.
func (m *Monitor) verifyLoop(ctx context.Context) {
	tick, stop := m.clock.Tick(m.opts.VerifyInterval)
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.verify:
			m.verifyOldest(ctx, true)
		case <-tick:
			m.verifyOldest(ctx, false)
		}
	}
}

// verifyOldest binary-searches the oldest block the node serves (full
// search) or re-checks the current edge with two requests (incremental).
func (m *Monitor) verifyOldest(ctx context.Context, full bool) {
	st := m.State()
	if !st.Online || st.LastRound == 0 {
		return
	}
	if m.opts.Overrides != nil && m.opts.Overrides.Archival != nil {
		m.update(func(s *LocalState) { s.Verified = true })
		return
	}
	expected := st.Caps.OldestRound
	var measured uint64
	var err error
	if full || !st.Verified {
		measured, err = m.searchOldest(ctx, st.LastRound)
	} else {
		measured, err = m.checkEdge(ctx, expected, st.LastRound)
	}
	if err != nil {
		m.log.Warn("oldest-round verification failed", "err", err)
		return
	}
	m.metric.Gauge("loadb_local_oldest_round", float64(measured))
	if measured > expected {
		m.log.Warn("node serves fewer blocks than its config claims; using measured oldest round",
			"configured", expected, "measured", measured)
	} else if measured < expected {
		m.log.Info("node serves more blocks than configured; keeping the configured window", "configured", expected, "measured", measured)
	}
	m.update(func(s *LocalState) {
		s.Verified = true
		if measured > s.Caps.OldestRound {
			switch s.Caps.Archival.Kind {
			case domain.ArchivalFull:
				s.Caps.Archival = domain.Archival{Kind: domain.ArchivalSince, N: measured}
			case domain.ArchivalTrailing:
				if s.LastRound > measured {
					s.Caps.Archival.N = s.LastRound - measured
				}
			}
			s.Caps.OldestRound = domain.OldestRoundFor(s.Caps.Archival, s.LastRound)
			if s.Caps.OldestRound < measured {
				s.Caps.OldestRound = measured
			}
		}
	})
}

func (m *Monitor) searchOldest(ctx context.Context, last uint64) (uint64, error) {
	return ProbeOldestRound(ctx, m.algod, last)
}

// ProbeOldestRound finds the smallest round the node serves with a binary
// search over HasBlock: about log2(last) requests, each a tiny block-hash
// lookup.
func ProbeOldestRound(ctx context.Context, algod ports.AlgodClient, last uint64) (uint64, error) {
	ok, err := algod.HasBlock(ctx, 0)
	if err != nil {
		return 0, err
	}
	if ok {
		return 0, nil
	}
	ok, err = algod.HasBlock(ctx, last)
	if err != nil {
		return 0, err
	}
	if !ok {
		return last, errors.New("node cannot serve its own last round")
	}
	lo, hi := uint64(0), last // HasBlock(lo)=false, HasBlock(hi)=true
	for hi-lo > 1 {
		mid := lo + (hi-lo)/2
		ok, err := algod.HasBlock(ctx, mid)
		if err != nil {
			return 0, err
		}
		if ok {
			hi = mid
		} else {
			lo = mid
		}
	}
	return hi, nil
}

// checkEdge confirms expected is served and expected-1 is not; on surprise it
// falls back to a full search.
func (m *Monitor) checkEdge(ctx context.Context, expected, last uint64) (uint64, error) {
	ok, err := m.algod.HasBlock(ctx, expected)
	if err != nil {
		return 0, err
	}
	if !ok {
		return m.searchOldest(ctx, last)
	}
	if expected == 0 {
		return 0, nil
	}
	below, err := m.algod.HasBlock(ctx, expected-1)
	if err != nil {
		return 0, err
	}
	if below {
		return m.searchOldest(ctx, last)
	}
	return expected, nil
}

type backoff struct {
	base, max, cur time.Duration
}

func newBackoff(base, max time.Duration) *backoff { return &backoff{base: base, max: max} }

func (b *backoff) next() time.Duration {
	if b.cur == 0 {
		b.cur = b.base
	} else {
		b.cur *= 2
		if b.cur > b.max {
			b.cur = b.max
		}
	}
	return b.cur
}

func (b *backoff) reset() { b.cur = 0 }
