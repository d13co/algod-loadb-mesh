// Package devfleet composes N fake algod nodes and N agents in one process
// with in-memory gossip and an in-memory registry. It backs `algod-loadb-mesh dev`
// and the simulator tests.
package devfleet

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	"github.com/d13co/algod-loadb-mesh/internal/adapters/algodhttp"
	"github.com/d13co/algod-loadb-mesh/internal/adapters/clock"
	"github.com/d13co/algod-loadb-mesh/internal/adapters/gossipmem"
	"github.com/d13co/algod-loadb-mesh/internal/adapters/metrics"
	"github.com/d13co/algod-loadb-mesh/internal/adapters/proxy"
	"github.com/d13co/algod-loadb-mesh/internal/adapters/registryfile"
	"github.com/d13co/algod-loadb-mesh/internal/agent"
	"github.com/d13co/algod-loadb-mesh/internal/config"
	"github.com/d13co/algod-loadb-mesh/internal/fakealgod"
	"github.com/d13co/algod-loadb-mesh/internal/ports"
)

// NodeSpec describes one fake node and its agent.
type NodeSpec struct {
	ID           string
	StartRound   uint64
	Archival     bool   // config.json Archival
	Lookback     uint64 // MaxBlockHistoryLookback
	OldestRound  uint64 // what the fake node really serves
	DeveloperAPI bool
	FollowMode   bool
	Tier         int
}

// Options configures the fleet.
type Options struct {
	Nodes            []NodeSpec
	GenesisID        string
	Mode             string
	ClientToken      string
	AdminToken       string
	SyncTolerance    uint64
	ReturnHysteresis int
	LagGrace         time.Duration
	Externals        []config.External
	KeepAlive        time.Duration
	SuspectAfter     time.Duration
	ProbeInterval    time.Duration
	RegistryRefresh  time.Duration
	Log              ports.Logger
	Timing           Timing
}

// Timing shortens service intervals so tests run in real time quickly.
type Timing struct {
	WaitTimeout time.Duration
}

// Fleet is the running set.
type Fleet struct {
	Hub      *gossipmem.Hub
	Registry *registryfile.Registry
	Nodes    []*fakealgod.Node
	Agents   []*agent.Agent
	Servers  []*httptest.Server
	Metrics  []*metrics.Registry
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

// Start builds and runs the fleet until Close.
func Start(ctx context.Context, o Options) (*Fleet, error) {
	if o.GenesisID == "" {
		o.GenesisID = "devnet-v1"
	}
	if o.KeepAlive == 0 {
		o.KeepAlive = 300 * time.Millisecond
	}
	if o.SuspectAfter == 0 {
		o.SuspectAfter = time.Second
	}
	if o.ProbeInterval == 0 {
		o.ProbeInterval = 500 * time.Millisecond
	}
	if o.RegistryRefresh == 0 {
		o.RegistryRefresh = time.Second
	}
	if o.LagGrace == 0 {
		o.LagGrace = 400 * time.Millisecond
	}
	if o.Log == nil {
		o.Log = nopLog{}
	}
	ctx, cancel := context.WithCancel(ctx)
	f := &Fleet{Hub: gossipmem.NewHub(), Registry: &registryfile.Registry{}, cancel: cancel}
	material := []byte("devfleet-shared-secret-material-32b")
	for i, spec := range o.Nodes {
		if spec.ID == "" {
			spec.ID = fmt.Sprintf("n%d", i+1)
		}
		node := fakealgod.New(fakealgod.Options{ID: spec.ID, GenesisID: o.GenesisID, StartRound: spec.StartRound,
			OldestRound: spec.OldestRound, DeveloperAPI: spec.DeveloperAPI, FollowMode: spec.FollowMode})
		f.Nodes = append(f.Nodes, node)
		key, err := agent.AgentKey(material, spec.ID)
		if err != nil {
			f.Close()
			return nil, err
		}
		cfg := config.Config{Mode: o.Mode, Listen: "127.0.0.1:0", ClientToken: o.ClientToken, AdminToken: o.AdminToken}
		cfg.Local.ID = spec.ID
		cfg.Local.DataDir = "/dev/null"
		cfg.Local.AdvertiseEndpoints = []string{node.URL()}
		cfg.Local.Tier = spec.Tier
		cfg.Local.WaitTimeout = o.Timing.WaitTimeout
		cfg.Registry.Type = "memory"
		cfg.Registry.Refresh = o.RegistryRefresh
		cfg.Mesh.Advertise = "mem:" + spec.ID
		cfg.Mesh.KeepAlive, cfg.Mesh.SuspectAfter, cfg.Mesh.ProbeInterval = o.KeepAlive, o.SuspectAfter, o.ProbeInterval
		cfg.Mesh.DownAfter = 3 * o.SuspectAfter
		cfg.Routing.SyncTolerance = o.SyncTolerance
		cfg.Routing.ReturnHysteresisRounds = o.ReturnHysteresis
		cfg.Routing.LagGrace = o.LagGrace
		cfg.Routing.UpstreamTimeout = 10 * time.Second
		cfg.Routing.Breaker.OpenFor = 2 * time.Second
		cfg.Tiers.External = o.Externals
		if err := cfg.Finish(); err != nil {
			f.Close()
			return nil, err
		}
		m := metrics.New()
		factory := algodhttp.Factory{}
		deps := agent.Deps{Config: cfg, Algod: factory.NewAlgodClient(node.URL(), node.Token()),
			ConfigReader: fakealgod.ConfigReader{Node: node, Archival: spec.Archival, Lookback: spec.Lookback,
				DeveloperAPI: spec.DeveloperAPI, FollowMode: spec.FollowMode},
			Clients: factory, Gossip: f.Hub.Join("mem:" + spec.ID), Registry: f.Registry, Forwarder: proxy.New(nil),
			Clock: clock.Real{}, Log: o.Log, Metrics: m, MetricsText: m, AgentKey: key, HTTPClient: &http.Client{Timeout: 10 * time.Second}}
		a, err := agent.New(deps)
		if err != nil {
			f.Close()
			return nil, err
		}
		f.Agents = append(f.Agents, a)
		f.Metrics = append(f.Metrics, m)
		f.Servers = append(f.Servers, httptest.NewServer(a.Handler))
		f.wg.Add(1)
		go func() { defer f.wg.Done(); _ = a.Run(ctx) }()
	}
	return f, nil
}

// Advance moves every fake node forward by k rounds.
func (f *Fleet) Advance(k uint64) {
	for _, n := range f.Nodes {
		n.Advance(k)
	}
}

// Close stops everything.
func (f *Fleet) Close() {
	f.cancel()
	f.wg.Wait()
	for _, s := range f.Servers {
		s.Close()
	}
	for _, n := range f.Nodes {
		n.Close()
	}
}

type nopLog struct{}

func (nopLog) Info(string, ...any)  {}
func (nopLog) Warn(string, ...any)  {}
func (nopLog) Error(string, ...any) {}
func (nopLog) Debug(string, ...any) {}

// AgentPubKey returns the heartbeat public key the fleet derived for a node.
func (f *Fleet) AgentPubKey(id string) []byte {
	k, _ := agent.AgentKey([]byte("devfleet-shared-secret-material-32b"), id)
	return []byte(k.Public().(ed25519.PublicKey))
}
