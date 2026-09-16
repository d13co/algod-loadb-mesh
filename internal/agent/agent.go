// Package agent is the composition root: it wires ports to adapters and
// services into one runnable agent. Nothing else in the tree knows concrete
// adapter types.
package agent

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/d13co/algod-loadb-mesh/internal/app"
	"github.com/d13co/algod-loadb-mesh/internal/config"
	"github.com/d13co/algod-loadb-mesh/internal/domain"
	"github.com/d13co/algod-loadb-mesh/internal/ports"
)

// Version is set by the build.
var Version = "dev"

// Deps are the ports an Agent is built from. Tests supply fakes.
type Deps struct {
	Config       config.Config
	Algod        ports.AlgodClient // the local node
	ConfigReader ports.NodeConfigReader
	Clients      ports.AlgodClientFactory
	Gossip       ports.Gossip
	Registry     ports.Registry
	Cache        ports.RegistryCache // may be nil
	Forwarder    ports.Forwarder
	Clock        ports.Clock
	Log          ports.Logger
	Metrics      ports.Metrics
	MetricsText  io.WriterTo // may be nil
	AgentKey     ed25519.PrivateKey
	HTTPClient   *http.Client
	Rand         domain.Rand
}

// Agent is one running loadb instance.
type Agent struct {
	Monitor   *app.Monitor
	Directory *app.Directory
	Router    *app.Router
	Registry  *app.RegistrySync
	Handler   http.Handler
	deps      Deps
}

// New wires services from deps. Nothing runs until Run.
func New(d Deps) (*Agent, error) {
	c := d.Config
	if err := c.Finish(); err != nil {
		return nil, err
	}
	mode, _ := domain.ParseMode(c.Mode)
	if d.Rand == nil {
		d.Rand = rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 1))
	}
	stats := app.NewStatsBook(app.BreakerOptions{Threshold: c.Routing.Breaker.Threshold, OpenFor: c.Routing.Breaker.OpenFor,
		Window: 50, Alpha: 0.2}, d.Clock)

	balancer := c.Balancer()
	monitor := app.NewMonitor(app.MonitorOptions{NodeID: c.Local.ID, WaitTimeout: c.Local.WaitTimeout,
		VerifyInterval: c.Local.VerifyInterval, Overrides: c.Local.Overrides, Absent: balancer, Network: c.Local.Network},
		d.Algod, d.ConfigReader, d.Clock, d.Log, d.Metrics)

	var externals []app.ExternalUpstream
	for _, e := range c.Tiers.External {
		externals = append(externals, app.ExternalUpstream{Name: e.Name, URL: e.URL, Token: e.Token, Tier: e.Tier,
			Capabilities: e.Capabilities, HealthCheck: e.HealthCheck})
	}
	overrides := map[string]app.PeerOverride{}
	for id, o := range c.Mesh.PeerOverrides {
		overrides[id] = app.PeerOverride{Tier: o.Tier}
	}
	dir := app.NewDirectory(app.DirectoryOptions{LocalID: c.Local.ID, LocalTier: c.Local.Tier, NoLocal: balancer,
		SuspectAfter: c.Mesh.SuspectAfter, DownAfter: c.Mesh.DownAfter, ProbeInterval: c.Mesh.ProbeInterval,
		KeepAlive: c.Mesh.KeepAlive, SyncTolerance: c.Routing.SyncTolerance, LagGrace: c.Routing.LagGrace, ReturnHysteresis: c.Routing.ReturnHysteresisRounds,
		PeerOverrides: overrides, Externals: externals}, monitor, d.Gossip, d.Clients, stats, d.Clock, d.Log, d.Metrics, d.AgentKey)

	router := app.NewRouter(app.RouterOptions{Mode: mode, Balancer: balancer, ClientToken: c.ClientToken, AdminToken: c.AdminToken, SyncTolerance: c.Routing.SyncTolerance,
		UpstreamTimeout: c.Routing.UpstreamTimeout, WaitTimeout: c.Local.WaitTimeout, PendingTTL: c.Routing.PendingTTL,
		RetryBudget: *c.Routing.RetryBudget, MultiBroadcast: c.Routing.MultiBroadcast, Version: Version},
		dir, monitor, d.Forwarder, d.HTTPClient, stats, d.Clock, d.Log, d.Metrics, d.Rand, d.MetricsText)

	pub := d.AgentKey.Public().(ed25519.PublicKey)
	localRecord := func() (domain.NodeRecord, bool) {
		if balancer {
			return domain.NodeRecord{ID: c.Local.ID, Role: domain.RoleBalancer, Network: c.Local.Network,
				Agent: domain.AgentInfo{Addr: c.Mesh.Advertise, PubKey: []byte(pub)}, Tags: c.Local.Tags}, true
		}
		st := monitor.State()
		if !st.ConfigOK || st.Caps.GenesisID == "" {
			return domain.NodeRecord{}, false
		}
		return domain.NodeRecord{ID: c.Local.ID, Network: st.Caps.GenesisID, Endpoints: c.Local.AdvertiseEndpoints,
			Token: st.Config.Token, Agent: domain.AgentInfo{Addr: c.Mesh.Advertise, PubKey: []byte(pub)},
			Tier: c.Local.Tier, Tags: c.Local.Tags, Declared: c.Local.Overrides}, true
	}
	reg := app.NewRegistrySync(app.RegistrySyncOptions{Refresh: c.Registry.Refresh, AutoRegister: *c.Registry.AutoRegister},
		d.Registry, d.Cache, dir, d.Clock, d.Log, d.Metrics, localRecord)

	dir.OnUnknownPeer(func(string) { reg.RefreshNow() })
	return &Agent{Monitor: monitor, Directory: dir, Router: router, Registry: reg, Handler: router, deps: d}, nil
}

// Run starts every service and blocks until ctx is done or one fails.
func (a *Agent) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := make(chan error, 3)
	go func() { errc <- a.Monitor.Run(ctx) }()
	go func() { errc <- a.Directory.Run(ctx) }()
	go func() { errc <- a.Registry.Run(ctx) }()
	err := <-errc
	cancel()
	<-errc
	<-errc
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// Serve runs the agent and its HTTP listener until ctx is done, then drains.
func (a *Agent) Serve(ctx context.Context) error {
	srv := &http.Server{Addr: a.deps.Config.Listen, Handler: a.Handler, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 2)
	go func() { errc <- a.Run(ctx) }()
	go func() {
		a.deps.Log.Info("listening", "addr", srv.Addr, "role", a.deps.Config.Role, "mode", a.deps.Config.Mode, "id", a.deps.Config.Local.ID)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- fmt.Errorf("listen %s: %w", srv.Addr, err)
		}
	}()
	select {
	case <-ctx.Done():
	case err := <-errc:
		if err != nil {
			return err
		}
	}
	a.Router.Draining(true)
	a.deps.Log.Info("draining", "inflight", a.Router.Inflight(), "timeout", a.deps.Config.Routing.DrainTimeout)
	dctx, cancel := context.WithTimeout(context.Background(), a.deps.Config.Routing.DrainTimeout)
	defer cancel()
	err := srv.Shutdown(dctx)
	_ = a.deps.Gossip.Close()
	return err
}
