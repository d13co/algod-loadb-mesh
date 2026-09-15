package app

import (
	"context"
	"sync"
	"time"

	"github.com/d13co/algod-loadb-mesh/internal/domain"
	"github.com/d13co/algod-loadb-mesh/internal/ports"
)

// RegistrySyncOptions configures registry polling and self-registration.
type RegistrySyncOptions struct {
	Refresh      time.Duration
	AutoRegister bool
}

// RegistrySync reads the registry on a timer, caches it, feeds the directory
// and keeps this node's own record up to date.
type RegistrySync struct {
	opts   RegistrySyncOptions
	reg    ports.Registry
	cache  ports.RegistryCache
	dir    *Directory
	clock  ports.Clock
	log    ports.Logger
	metric ports.Metrics
	// local returns the record this agent wants published, when known.
	local func() (domain.NodeRecord, bool)
	kick  chan struct{}

	mu       sync.Mutex
	lastKick time.Time
}

// NewRegistrySync wires the service. cache may be nil.
func NewRegistrySync(o RegistrySyncOptions, reg ports.Registry, cache ports.RegistryCache, dir *Directory,
	clock ports.Clock, log ports.Logger, metric ports.Metrics, local func() (domain.NodeRecord, bool)) *RegistrySync {
	if o.Refresh == 0 {
		o.Refresh = 5 * time.Minute
	}
	return &RegistrySync{opts: o, reg: reg, cache: cache, dir: dir, clock: clock, log: log, metric: metric,
		local: local, kick: make(chan struct{}, 1)}
}

// RefreshNow asks for an immediate refresh, at most once per tenth of the
// refresh period (a burst of heartbeats from a new node is one refresh).
func (s *RegistrySync) RefreshNow() {
	now := s.clock.Now()
	s.mu.Lock()
	if now.Sub(s.lastKick) < s.opts.Refresh/10 {
		s.mu.Unlock()
		return
	}
	s.lastKick = now
	s.mu.Unlock()
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// Run blocks until ctx is done.
func (s *RegistrySync) Run(ctx context.Context) error {
	if s.cache != nil {
		if recs, err := s.cache.Load(); err != nil {
			s.log.Warn("registry cache unreadable", "err", err)
		} else if len(recs) > 0 {
			s.dir.SetRecords(recs)
			s.log.Info("registry loaded from cache", "records", len(recs))
		}
	}
	tick, stop := s.clock.Tick(s.opts.Refresh)
	defer stop()
	registered := false
	for {
		if s.refresh(ctx, &registered) && s.opts.AutoRegister && !registered {
			// The local record is not known yet (monitor still reading the
			// node); retry sooner than the refresh period.
			_ = s.clock.Sleep(ctx, min(s.opts.Refresh, 10*time.Second)/2)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick:
		case <-s.kick:
		}
	}
}

// refresh does one read (and possibly one write). It returns true when the
// read succeeded.
func (s *RegistrySync) refresh(ctx context.Context, registered *bool) bool {
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	recs, round, err := s.reg.List(rctx)
	cancel()
	if err != nil {
		s.metric.Inc("loadb_registry_reads", "ok", "false")
		s.log.Warn("registry read failed", "err", err)
		return false
	}
	s.metric.Inc("loadb_registry_reads", "ok", "true")
	s.metric.Gauge("loadb_registry_round", float64(round))
	if s.opts.AutoRegister && !*registered {
		if want, ok := s.local(); ok {
			if rec, changed := s.register(ctx, recs, want, round); changed {
				recs = upsert(recs, rec)
			}
			*registered = true
		}
	}
	s.dir.SetRecords(recs)
	if s.cache != nil {
		if err := s.cache.Save(recs); err != nil {
			s.log.Warn("registry cache write failed", "err", err)
		}
	}
	return true
}

func (s *RegistrySync) register(ctx context.Context, recs []domain.NodeRecord, want domain.NodeRecord, round uint64) (domain.NodeRecord, bool) {
	for _, r := range recs {
		if r.ID != want.ID {
			continue
		}
		if r.StaticEqual(want) {
			return r, false
		}
		want.Version = r.Version + 1
		break
	}
	if want.Version == 0 {
		want.Version = 1
	}
	want.UpdatedAt = round
	wctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := s.reg.Put(wctx, want); err != nil {
		s.metric.Inc("loadb_registry_writes", "ok", "false")
		s.log.Warn("self-registration failed", "err", err)
		return want, false
	}
	s.metric.Inc("loadb_registry_writes", "ok", "true")
	s.log.Info("registered this node", "id", want.ID, "version", want.Version, "endpoints", want.Endpoints)
	return want, true
}

func upsert(recs []domain.NodeRecord, rec domain.NodeRecord) []domain.NodeRecord {
	for i := range recs {
		if recs[i].ID == rec.ID {
			recs[i] = rec
			return recs
		}
	}
	return append(recs, rec)
}
