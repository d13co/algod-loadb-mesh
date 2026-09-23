package agent

import (
	"context"
	"crypto/ed25519"
	crand "crypto/rand"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/d13co/algod-loadb-mesh/internal/adapters/algodhttp"
	"github.com/d13co/algod-loadb-mesh/internal/adapters/clock"
	"github.com/d13co/algod-loadb-mesh/internal/adapters/gossipmem"
	"github.com/d13co/algod-loadb-mesh/internal/adapters/logging"
	"github.com/d13co/algod-loadb-mesh/internal/adapters/metrics"
	"github.com/d13co/algod-loadb-mesh/internal/adapters/proxy"
	"github.com/d13co/algod-loadb-mesh/internal/adapters/registryfile"
	"github.com/d13co/algod-loadb-mesh/internal/app"
	"github.com/d13co/algod-loadb-mesh/internal/config"
	"github.com/d13co/algod-loadb-mesh/internal/fakealgod"
	"github.com/d13co/algod-loadb-mesh/internal/ports"
)

type countingLog struct {
	logging.Nop
	warns int
}

func (l *countingLog) Warn(string, ...any) { l.warns++ }

func TestFilterPassthrough(t *testing.T) {
	pt := config.Addrs{"10.114.0.5:8080", "10.112.0.5:8080", "[fd00::5]:8080", "10.114.0.5:8081"}
	cases := []struct {
		algod         string
		keep, skipped string
	}{
		{"0.0.0.0:8080", "[fd00::5]:8080, 10.114.0.5:8081", "10.114.0.5:8080, 10.112.0.5:8080"},
		{"[::]:8080", "10.114.0.5:8081", "10.114.0.5:8080, 10.112.0.5:8080, [fd00::5]:8080"},
		{"10.112.0.5:8080", "10.114.0.5:8080, [fd00::5]:8080, 10.114.0.5:8081", "10.112.0.5:8080"},
		{"127.0.0.1:8080", "10.114.0.5:8080, 10.112.0.5:8080, [fd00::5]:8080, 10.114.0.5:8081", ""},
	}
	for _, c := range cases {
		log := &countingLog{}
		keep, skipped := filterPassthrough(pt, c.algod, log)
		if keep.String() != c.keep || skipped.String() != c.skipped {
			t.Errorf("algod %s: keep %q skipped %q", c.algod, keep, skipped)
		}
		if log.warns != len(skipped) {
			t.Errorf("algod %s: %d warnings for %d skipped", c.algod, log.warns, len(skipped))
		}
	}
}

// netReader overrides what algod.net says.
type netReader struct {
	ports.NodeConfigReader
	netAddr string
}

func (r netReader) Read() (ports.NodeConfig, error) {
	nc, err := r.NodeConfigReader.Read()
	nc.NetAddr = r.netAddr
	return nc, err
}

// newAgent builds a node agent on a fake algod, with in-memory gossip and
// registry, the way devfleet does.
func newAgent(t *testing.T, node *fakealgod.Node, reader ports.NodeConfigReader, passthrough ...string) *Agent {
	t.Helper()
	off := false
	cfg := config.Config{Listen: config.Addrs{"127.0.0.1:0"}}
	cfg.Local.ID, cfg.Local.DataDir = "a", "/dev/null"
	cfg.Local.AdvertiseEndpoints = []string{node.URL()}
	cfg.Local.Passthrough = passthrough
	cfg.Registry.Type, cfg.Registry.AutoRegister = "memory", &off
	cfg.Routing.DrainTimeout = 200 * time.Millisecond
	if err := cfg.Finish(); err != nil {
		t.Fatal(err)
	}
	_, key, _ := ed25519.GenerateKey(crand.Reader)
	m := metrics.New()
	factory := algodhttp.Factory{}
	a, err := New(Deps{Config: cfg, Algod: factory.NewAlgodClient(node.URL(), node.Token()), ConfigReader: reader,
		Clients: factory, Gossip: gossipmem.NewHub().Join("mem:a"), Registry: &registryfile.Registry{}, Forwarder: proxy.New(nil),
		Clock: clock.Real{}, Log: logging.Nop{}, Metrics: m, MetricsText: m, AgentKey: key, HTTPClient: &http.Client{Timeout: 5 * time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func serve(t *testing.T, a *Agent) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Serve(ctx) }()
	return cancel, done
}

// status waits for Serve to publish the pass-through in /loadb/status, which
// it does once the listeners are bound.
func status(t *testing.T, a *Agent) app.PassthroughStatus {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		rec := httptest.NewRecorder()
		a.Handler.ServeHTTP(rec, httptest.NewRequest("GET", "/loadb/status", nil))
		var body struct {
			Passthrough *app.PassthroughStatus `json:"passthrough"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil || rec.Code != 200 {
			t.Fatalf("status %d: %v", rec.Code, err)
		}
		if body.Passthrough != nil {
			return *body.Passthrough
		}
		if time.Now().After(deadline) {
			t.Fatal("no passthrough key in /loadb/status")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Every configured entry covered by algod.net: nothing is bound, the agent
// keeps running, and the status says why.
func TestServeSkipsCoveredPassthrough(t *testing.T) {
	node := fakealgod.New(fakealgod.Options{ID: "a"})
	defer node.Close()
	reader := netReader{fakealgod.ConfigReader{Node: node}, "0.0.0.0:8080"}
	a := newAgent(t, node, reader, "127.0.0.2:8080", "127.0.0.3:8080")
	cancel, done := serve(t, a)
	select {
	case err := <-done:
		t.Fatalf("Serve returned early: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	st := status(t, a)
	if len(st.Addrs) != 0 || strings.Join(st.Skipped, ", ") != "127.0.0.2:8080, 127.0.0.3:8080" || st.Target == "" {
		t.Fatalf("status: %+v", st)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// One uncovered entry: bound, spliced to the fake algod, released on exit.
func TestServePassthrough(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Skipf("loopback alias 127.0.0.2 not bindable: %v", err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()
	node := fakealgod.New(fakealgod.Options{ID: "a"})
	defer node.Close()
	a := newAgent(t, node, fakealgod.ConfigReader{Node: node}, addr)
	cancel, done := serve(t, a)
	if st := status(t, a); strings.Join(st.Addrs, ", ") != addr {
		t.Fatalf("status before the request: %+v", st)
	}
	req, _ := http.NewRequest("GET", "http://"+addr+"/v2/status", nil)
	req.Header.Set("X-Algo-API-Token", node.Token())
	req.Close = true
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "last-round") {
		t.Fatalf("through the pass-through: %d %s", resp.StatusCode, body)
	}
	st := status(t, a)
	if strings.Join(st.Addrs, ", ") != addr || len(st.Skipped) != 0 || st.Target != strings.TrimPrefix(node.URL(), "http://") {
		t.Fatalf("status: %+v", st)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("%s not released: %v", addr, err)
	}
	_ = ln.Close()
}
