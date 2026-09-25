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
	"net"
	"net/http"
	"time"

	"github.com/d13co/algod-loadb-mesh/internal/adapters/passthrough"
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
	monitor := app.NewMonitor(app.MonitorOptions{NodeID: c.Local.ID,
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
		KeepAlive: c.Mesh.KeepAlive, PathProbeInterval: c.Mesh.PathProbeInterval, PathTimeout: c.Mesh.PathTimeout, SyncTolerance: c.Routing.SyncTolerance, LagGrace: c.Routing.LagGrace, ReturnHysteresis: c.Routing.ReturnHysteresisRounds,
		PeerOverrides: overrides, Externals: externals}, monitor, d.Gossip, d.Clients, stats, d.Clock, d.Log, d.Metrics, d.AgentKey)

	retry := -1 // every eligible mesh upstream, then the externals
	if c.Routing.RetryBudget != nil {
		retry = *c.Routing.RetryBudget
	}
	router := app.NewRouter(app.RouterOptions{Mode: mode, Balancer: balancer, ClientToken: c.ClientToken, AdminToken: c.AdminToken, SyncTolerance: c.Routing.SyncTolerance,
		UpstreamTimeout: c.Routing.UpstreamTimeout, WaitTimeout: c.Local.WaitTimeout, PendingTTL: c.Routing.PendingTTL,
		RetryBudget: retry, MultiBroadcast: c.Routing.MultiBroadcast, Version: Version},
		dir, monitor, d.Forwarder, d.HTTPClient, stats, d.Clock, d.Log, d.Metrics, d.Rand, d.MetricsText)

	pub := d.AgentKey.Public().(ed25519.PublicKey)
	localRecord := func() (domain.NodeRecord, bool) {
		if balancer {
			return domain.NodeRecord{ID: c.Local.ID, Role: domain.RoleBalancer, Network: c.Local.Network,
				Agent: domain.AgentInfo{Addrs: c.Mesh.Advertise, PubKey: []byte(pub)}, Tags: c.Local.Tags}, true
		}
		st := monitor.State()
		if !st.ConfigOK || st.Caps.GenesisID == "" {
			return domain.NodeRecord{}, false
		}
		return domain.NodeRecord{ID: c.Local.ID, Network: st.Caps.GenesisID, Endpoints: c.Local.AdvertiseEndpoints,
			Token: st.Config.Token, Agent: domain.AgentInfo{Addrs: c.Mesh.Advertise, PubKey: []byte(pub)},
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

// Serve runs the agent and its HTTP listeners until ctx is done, then drains.
// One handler is served on every config.Listen address, and algod is passed
// through on every local.passthrough address it does not bind itself;
// binding comes first, so a bad address fails before any service starts.
func (a *Agent) Serve(ctx context.Context) error {
	addrs := a.deps.Config.Listen
	lns, err := listenAll(addrs)
	if err != nil {
		return err
	}
	defer closeAll(lns)
	ps, err := a.passthrough()
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: a.Handler, ReadHeaderTimeout: 10 * time.Second}
	// One slot per producer: Run, each listener, the pass-through.
	errc := make(chan error, 2+len(lns))
	go func() { errc <- a.Run(ctx) }()
	a.deps.Log.Info("listening", "addr", addrs.String(), "role", a.deps.Config.Role, "mode", a.deps.Config.Mode, "id", a.deps.Config.Local.ID)
	for _, ln := range lns {
		go func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("listen %s: %w", ln.Addr(), err)
			}
		}()
	}
	if ps != nil {
		// Returns nil on cancellation; anything else is a failed Accept.
		go func() { errc <- ps.Serve(ctx) }()
	}
	select {
	case <-ctx.Done():
	case err := <-errc:
		if err != nil {
			// Stopping without a drain: the deferred closeAll takes the
			// client listeners, the pass-through must let go of its port too.
			if ps != nil {
				_ = ps.Close()
			}
			return err
		}
	}
	a.Router.Draining(true)
	a.deps.Log.Info("draining", "inflight", a.Router.Inflight(), "timeout", a.deps.Config.Routing.DrainTimeout)
	dctx, cancel := context.WithTimeout(context.Background(), a.deps.Config.Routing.DrainTimeout)
	defer cancel()
	// Shutdown closes every listener Serve was given.
	err = srv.Shutdown(dctx)
	if ps != nil {
		_ = ps.Shutdown(dctx) // always nil; cut connections are logged
	}
	_ = a.deps.Gossip.Close()
	return err
}

// passthrough binds local.passthrough minus what algod already covers, judged
// from algod.net now, and publishes the outcome in /loadb/status. It returns
// nil when nothing is configured or nothing is left to bind: a Server with no
// listeners would return from Serve at once and stop the agent.
func (a *Agent) passthrough() (*passthrough.Server, error) {
	pt := a.deps.Config.Local.Passthrough
	if len(pt) == 0 {
		return nil, nil
	}
	nc, err := a.deps.ConfigReader.Read()
	if err != nil {
		return nil, err
	}
	keep, skipped := filterPassthrough(pt, nc.NetAddr, a.deps.Log)
	// Read per connection, so the splice follows algod.net as the monitor
	// re-reads it; the wildcard→loopback rewrite is the right dial target.
	target := func() string { return passthrough.HostPort(a.Monitor.State().Endpoint) }
	var ps *passthrough.Server
	if len(keep) > 0 {
		if ps, err = passthrough.Listen(keep, target, a.deps.Log, a.deps.Metrics); err != nil {
			return nil, err
		}
	}
	a.Router.SetPassthrough(func() app.PassthroughStatus {
		st := app.PassthroughStatus{Addrs: []string{}, Skipped: skipped, Target: target()}
		if ps != nil {
			st.Addrs, st.Active = ps.Addrs(), ps.Active()
		}
		return st
	})
	a.deps.Log.Info("passthrough", "addrs", keep.String(), "skipped", skipped.String(), "target", passthrough.HostPort(nc.Endpoint))
	return ps, nil
}

// filterPassthrough splits the entries into those to bind and those algod
// already binds, warning about each of the latter: bound anyway, they would
// take the port from an algod that restarts while the agent holds it.
func filterPassthrough(pt config.Addrs, algodNet string, log ports.Logger) (keep, skipped config.Addrs) {
	for _, addr := range pt {
		if domain.AlgodCovers(algodNet, addr) {
			log.Warn("passthrough address skipped: algod already listens there", "addr", addr, "algod", algodNet)
			skipped = append(skipped, addr)
			continue
		}
		keep = append(keep, addr)
	}
	return keep, skipped
}

// listenAll binds every address, closing what it opened if one fails.
func listenAll(addrs config.Addrs) ([]net.Listener, error) {
	lns := make([]net.Listener, 0, len(addrs))
	for _, addr := range addrs {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			closeAll(lns)
			return nil, fmt.Errorf("listen %s: %w", addr, err)
		}
		lns = append(lns, ln)
	}
	return lns, nil
}

func closeAll(lns []net.Listener) {
	for _, ln := range lns {
		_ = ln.Close()
	}
}
