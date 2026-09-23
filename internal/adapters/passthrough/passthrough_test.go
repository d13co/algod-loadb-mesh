package passthrough

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/d13co/algod-loadb-mesh/internal/adapters/logging"
)

type fakeMetrics struct {
	mu     sync.Mutex
	counts map[string]int
	gauges map[string]float64
}

func newMetrics() *fakeMetrics {
	return &fakeMetrics{counts: map[string]int{}, gauges: map[string]float64{}}
}

func (m *fakeMetrics) Inc(name string, labels ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counts[name+"{"+strings.Join(labels, ",")+"}"]++
}
func (m *fakeMetrics) Observe(string, float64, ...string) {}
func (m *fakeMetrics) Gauge(name string, v float64, labels ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gauges[name] = v
}
func (m *fakeMetrics) count(prefix string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for k, v := range m.counts {
		if strings.HasPrefix(k, prefix) {
			n += v
		}
	}
	return n
}

// alias skips the test when the loopback alias 127.0.0.2 cannot be bound;
// any later Listen failure is then a real failure.
func alias(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Skipf("loopback alias 127.0.0.2 not bindable: %v", err)
	}
	_ = ln.Close()
	return "127.0.0.2:0"
}

func start(t *testing.T, target func() string) (*Server, *fakeMetrics, context.CancelFunc) {
	t.Helper()
	m := newMetrics()
	s, err := Listen([]string{alias(t)}, target, logging.Nop{}, m)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return s, m, cancel
}

func fixed(addr string) func() string { return func() string { return addr } }

func TestSplice(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "hello") }))
	defer up.Close()
	s, m, _ := start(t, fixed(HostPort(up.URL)))
	resp, err := http.Get("http://" + s.Addrs()[0] + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "hello" {
		t.Fatalf("body %q", body)
	}
	if m.count("loadb_passthrough_accepted") != 1 {
		t.Fatalf("accepted: %v", m.counts)
	}
}

func echo(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, _ = io.Copy(c, c)
				_ = c.Close()
			}()
		}
	}()
	return ln.Addr().String()
}

func TestHalfClose(t *testing.T) {
	s, _, _ := start(t, fixed(echo(t)))
	c, err := net.Dial("tcp", s.Addrs()[0])
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	msg := strings.Repeat("x", 1<<16)
	if _, err := io.WriteString(c, msg); err != nil {
		t.Fatal(err)
	}
	_ = c.(*net.TCPConn).CloseWrite()
	got, err := io.ReadAll(c)
	if err != nil || string(got) != msg {
		t.Fatalf("read %d bytes, err %v", len(got), err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for s.Active() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if s.Active() != 0 {
		t.Fatalf("active %d after both sides closed", s.Active())
	}
}

func TestDialFailure(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := ln.Addr().String()
	_ = ln.Close()
	target := closed
	var mu sync.Mutex
	s, m, _ := start(t, func() string { mu.Lock(); defer mu.Unlock(); return target })
	c, err := net.Dial("tcp", s.Addrs()[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(c); err != nil {
		t.Fatalf("expected a clean EOF, got %v", err)
	}
	_ = c.Close()
	mu.Lock()
	target = ""
	mu.Unlock()
	c, _ = net.Dial("tcp", s.Addrs()[0])
	_, _ = io.ReadAll(c)
	_ = c.Close()
	if m.count("loadb_passthrough_dial_failures{reason,dial}") != 1 || m.count("loadb_passthrough_dial_failures{reason,no_target}") != 1 {
		t.Fatalf("dial failures: %v", m.counts)
	}
}

func TestTargetFollows(t *testing.T) {
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "a") }))
	defer a.Close()
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "b") }))
	defer b.Close()
	var mu sync.Mutex
	target := HostPort(a.URL)
	s, _, _ := start(t, func() string { mu.Lock(); defer mu.Unlock(); return target })
	get := func() string {
		req, _ := http.NewRequest("GET", "http://"+s.Addrs()[0]+"/", nil)
		req.Close = true // a new connection each time, so the target is re-read
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return string(body)
	}
	if got := get(); got != "a" {
		t.Fatalf("first: %q", got)
	}
	mu.Lock()
	target = HostPort(b.URL)
	mu.Unlock()
	if got := get(); got != "b" {
		t.Fatalf("after the move: %q", got)
	}
}

func TestServeReturnsNilOnCancel(t *testing.T) {
	s, err := Listen([]string{alias(t)}, fixed(""), logging.Nop{}, newMetrics())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v on cancel", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return")
	}
	if _, err := net.Dial("tcp", s.Addrs()[0]); err == nil {
		t.Fatal("listener still open after Serve returned")
	}
}

func TestShutdownCutsOpenConnections(t *testing.T) {
	s, m, cancel := start(t, fixed(echo(t)))
	c, err := net.Dial("tcp", s.Addrs()[0])
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := io.WriteString(c, "ping"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatal(err)
	}
	if s.Active() != 1 {
		t.Fatalf("active %d", s.Active())
	}
	cancel()
	ctx, stop := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer stop()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Read(buf); err == nil {
		t.Fatal("client connection survived Shutdown")
	}
	if s.Active() != 0 || m.gauges["loadb_passthrough_active"] != 0 {
		t.Fatalf("active %d gauge %v", s.Active(), m.gauges["loadb_passthrough_active"])
	}
}

// Shutdown while clients keep dialing and nothing is spliced yet: a late
// accept must not add to the connection count while Shutdown is already
// waiting on it (a WaitGroup misuse -race reports), and no connection the
// server spliced may outlive Shutdown, not even one still dialing algod
// when the cut ran. A dialer keeps a connection only once its echo came
// back, which proves it was spliced: one the kernel queued but the server
// never accepted is not the server's to cut. Repeated because the windows
// are a few instructions wide.
func TestShutdownWhileAccepting(t *testing.T) {
	target := echo(t)
	for range 10 {
		s, _, _ := start(t, fixed(target))
		addr := s.Addrs()[0]
		var mu sync.Mutex
		var spliced []net.Conn
		var dials sync.WaitGroup
		for range 4 {
			dials.Add(1)
			go func() {
				defer dials.Done()
				buf := make([]byte, 1)
				for {
					c, err := net.Dial("tcp", addr)
					if err != nil {
						return // listener closed
					}
					_ = c.SetDeadline(time.Now().Add(2 * time.Second))
					if _, err := io.WriteString(c, "x"); err == nil {
						_, err = io.ReadFull(c, buf)
					}
					if err != nil {
						_ = c.Close() // never spliced, or cut already
						continue
					}
					mu.Lock()
					spliced = append(spliced, c)
					mu.Unlock()
				}
			}()
		}
		time.Sleep(3 * time.Millisecond)
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		dials.Wait()
		if s.Active() != 0 {
			t.Fatalf("active %d after Shutdown", s.Active())
		}
		buf := make([]byte, 1)
		for _, c := range spliced {
			_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
			_, err := c.Read(buf)
			var ne net.Error
			if err == nil || (errors.As(err, &ne) && ne.Timeout()) {
				t.Fatalf("spliced connection survived Shutdown: read err %v", err)
			}
			_ = c.Close()
		}
	}
}

// scripted is a Listener whose Accept hands out the connections the test
// feeds it and blocks otherwise, and whose Close only reports on a channel:
// the test can then feed one more connection, as an Accept that returned
// just before the listener closed would, before ending the feed.
type scripted struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func (g *scripted) Accept() (net.Conn, error) {
	c, ok := <-g.conns
	if !ok {
		return nil, net.ErrClosed
	}
	return c, nil
}
func (g *scripted) Close() error   { g.once.Do(func() { close(g.closed) }); return nil }
func (g *scripted) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 2), Port: 8080} }

func waitActive(t *testing.T, s *Server, n int64) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); s.Active() != n; {
		if time.Now().After(deadline) {
			t.Fatalf("active %d, want %d", s.Active(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// Shutdown must let the accept loops end before it waits on the spliced
// connections. Otherwise an accept that returned just as the listener closed
// adds to a count Shutdown is already waiting on (a WaitGroup misuse that
// -race reports once a live connection has finished during the wait) and
// that connection outlives Shutdown. The listener is scripted so the order
// is exact: one live connection, Shutdown, the live one ends, then the late
// accept.
func TestShutdownWaitsForAcceptLoops(t *testing.T) {
	g := &scripted{conns: make(chan net.Conn), closed: make(chan struct{})}
	s := newServer([]net.Listener{g}, fixed(echo(t)), logging.Nop{}, newMetrics())
	served := make(chan error, 1)
	go func() { served <- s.Serve(context.Background()) }()

	live, liveSrv := net.Pipe()
	g.conns <- liveSrv
	waitActive(t, s, 1)

	shut := make(chan struct{})
	go func() { _ = s.Close(); close(shut) }()
	<-g.closed
	time.Sleep(20 * time.Millisecond) // Shutdown reaches its wait
	_ = live.Close()
	waitActive(t, s, 0)

	late, lateSrv := net.Pipe()
	defer late.Close()
	g.conns <- lateSrv
	close(g.conns)
	<-shut
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	_ = late.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	_, err := late.Read(make([]byte, 1))
	var ne net.Error
	if err == nil || (errors.As(err, &ne) && ne.Timeout()) {
		t.Fatalf("connection accepted as the listener closed outlived Shutdown: read err %v", err)
	}
}

// Close cuts at once, and a Serve that starts after it does not accept.
func TestCloseThenServe(t *testing.T) {
	s, err := Listen([]string{alias(t)}, fixed(echo(t)), logging.Nop{}, newMetrics())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Serve(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve after Close returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve after Close did not return")
	}
}

func TestHostPort(t *testing.T) {
	for in, want := range map[string]string{"http://[::1]:8080": "[::1]:8080", "http://127.0.0.1:8080": "127.0.0.1:8080", "http://10.1.2.3:8080/": "10.1.2.3:8080", "::bad": ""} {
		if got := HostPort(in); got != want {
			t.Errorf("HostPort(%q) = %q want %q", in, got, want)
		}
	}
}

func TestListenClosesOnFailure(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	first, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := first.Addr().String()
	_ = first.Close()
	if _, err := Listen([]string{addr, held.Addr().String()}, fixed(""), logging.Nop{}, newMetrics()); err == nil {
		t.Fatal("expected the second bind to fail")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("first address not released: %v", err)
	}
	_ = ln.Close()
}
