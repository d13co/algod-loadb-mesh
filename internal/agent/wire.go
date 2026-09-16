package agent

import (
	"crypto/ed25519"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/d13co/algod-loadb-mesh/internal/adapters/algodhttp"
	"github.com/d13co/algod-loadb-mesh/internal/adapters/clock"
	"github.com/d13co/algod-loadb-mesh/internal/adapters/datadir"
	"github.com/d13co/algod-loadb-mesh/internal/adapters/gossipudp"
	"github.com/d13co/algod-loadb-mesh/internal/adapters/logging"
	"github.com/d13co/algod-loadb-mesh/internal/adapters/metrics"
	"github.com/d13co/algod-loadb-mesh/internal/adapters/proxy"
	"github.com/d13co/algod-loadb-mesh/internal/adapters/registryalgo"
	"github.com/d13co/algod-loadb-mesh/internal/adapters/registryfile"
	"github.com/d13co/algod-loadb-mesh/internal/config"
	"github.com/d13co/algod-loadb-mesh/internal/domain"
	"github.com/d13co/algod-loadb-mesh/internal/ports"
)

// Logger builds the process logger from config.
func Logger(c config.Config) logging.Slog {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(strings.ToUpper(c.Log.Level))); err != nil {
		lvl = slog.LevelInfo
	}
	return logging.New(os.Stderr, lvl, c.Log.JSON)
}

// FromConfig builds production deps: real data dir, REST, UDP gossip, the
// configured registry backend, streaming proxy and wall clock.
func FromConfig(c config.Config) (Deps, error) {
	if err := c.Finish(); err != nil {
		return Deps{}, err
	}
	log := Logger(c)
	factory := algodhttp.Factory{}
	// A balancer has no node: no data dir, no local client.
	var reader ports.NodeConfigReader
	var local ports.AlgodClient
	var nc ports.NodeConfig
	if !c.Balancer() {
		dr := datadir.Reader{Dir: c.Local.DataDir}
		var err error
		if nc, err = dr.Read(); err != nil {
			return Deps{}, err
		}
		reader, local = dr, factory.NewAlgodClient(nc.Endpoint, nc.Token)
	}

	material, err := c.KeyMaterial()
	if err != nil {
		return Deps{}, err
	}
	key, err := AgentKey(material, c.Local.ID)
	if err != nil {
		return Deps{}, err
	}

	var reg ports.Registry
	var cache ports.RegistryCache
	switch c.Registry.Type {
	case "algorand":
		seed, err := c.SyncSeed()
		if err != nil {
			return Deps{}, err
		}
		url, token := c.Registry.AlgodURL, c.Registry.AlgodToken
		regReader := local
		if url == "" {
			url, token = nc.Endpoint, nc.Token
		} else {
			regReader = factory.NewAlgodClient(url, token)
		}
		r, err := registryalgo.New(c.Registry.AppID, seed, c.Registry.SyncAddress, regReader, url, token, log)
		if err != nil {
			return Deps{}, err
		}
		reg = r
		if c.Registry.Cache != "" {
			cache = registryfile.Cache{Path: c.Registry.Cache}
		}
	case "file":
		reg = &registryfile.Registry{Path: c.Registry.File}
	case "memory":
		reg = &registryfile.Registry{}
	default:
		recs := make([]domain.NodeRecord, 0, len(c.Registry.Static))
		for _, r := range c.Registry.Static {
			if len(r.Agent.PubKey) == 0 {
				k, err := AgentKey(material, r.ID)
				if err != nil {
					return Deps{}, err
				}
				r.Agent.PubKey = []byte(k.Public().(ed25519.PublicKey))
			}
			recs = append(recs, r)
		}
		reg = registryfile.Static{Records: recs}
	}

	gossip, err := gossipudp.Listen(c.Mesh.Listen)
	if err != nil {
		return Deps{}, fmt.Errorf("mesh.listen: %w", err)
	}
	m := metrics.New()
	return Deps{Config: c, Algod: local, ConfigReader: reader, Clients: factory, Gossip: gossip, Registry: reg, Cache: cache,
		Forwarder: proxy.New(nil), Clock: clock.Real{}, Log: log, Metrics: m, MetricsText: m, AgentKey: key,
		HTTPClient: &http.Client{Timeout: c.Routing.UpstreamTimeout + time.Second}}, nil
}
