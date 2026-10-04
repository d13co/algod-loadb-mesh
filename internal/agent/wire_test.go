package agent

import (
	"context"
	"crypto/ed25519"
	crand "crypto/rand"
	"net/http"
	"testing"
	"time"

	"github.com/d13co/algod-loadb-mesh/internal/adapters/algodhttp"
	"github.com/d13co/algod-loadb-mesh/internal/adapters/clock"
	"github.com/d13co/algod-loadb-mesh/internal/adapters/gossipmem"
	"github.com/d13co/algod-loadb-mesh/internal/adapters/logging"
	"github.com/d13co/algod-loadb-mesh/internal/adapters/metrics"
	"github.com/d13co/algod-loadb-mesh/internal/adapters/proxy"
	"github.com/d13co/algod-loadb-mesh/internal/adapters/registryfile"
	"github.com/d13co/algod-loadb-mesh/internal/config"
	"github.com/d13co/algod-loadb-mesh/internal/fakealgod"
)

// While the local node cannot answer (in catchup algod answers 503),
// registry reads through the loopback are served by an external.
func TestLoopbackFallsBackToExternal(t *testing.T) {
	node := fakealgod.New(fakealgod.Options{ID: "a"})
	defer node.Close()
	node.SetFailing(true)
	ext := fakealgod.New(fakealgod.Options{ID: "ext"})
	defer ext.Close()
	ext.PutBox(7, []byte("n1"), []byte("v"))

	off := false
	cfg := config.Config{Listen: config.Addrs{"127.0.0.1:0"}, ClientToken: "client"}
	cfg.Local.ID, cfg.Local.DataDir = "a", "/dev/null"
	cfg.Registry.Type, cfg.Registry.AutoRegister = "memory", &off
	cfg.Tiers.External = []config.External{{Name: "ext", URL: ext.URL(), Token: ext.Token()}}
	if err := cfg.Finish(); err != nil {
		t.Fatal(err)
	}
	_, key, _ := ed25519.GenerateKey(crand.Reader)
	factory := algodhttp.Factory{}
	lb := &loopback{}
	if _, err := New(Deps{Config: cfg, Algod: factory.NewAlgodClient(node.URL(), node.Token()), ConfigReader: fakealgod.ConfigReader{Node: node},
		Clients: factory, Gossip: gossipmem.NewHub().Join("mem:a"), Registry: &registryfile.Registry{}, Forwarder: proxy.New(nil),
		Clock: clock.Real{}, Log: logging.Nop{}, Metrics: metrics.New(), AgentKey: key, HTTPClient: &http.Client{Timeout: 5 * time.Second},
		loopback: lb}); err != nil {
		t.Fatal(err)
	}

	c := algodhttp.New("http://loopback", cfg.ClientToken, &http.Client{Transport: lb})
	names, err := c.BoxNames(context.Background(), 7)
	if err != nil || len(names) != 1 || string(names[0]) != "n1" {
		t.Fatalf("box names through the loopback: %q %v", names, err)
	}
}
