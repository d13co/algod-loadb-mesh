// Package passthrough binds algod's port on the interfaces algod does not
// listen on and splices each accepted TCP connection to the address algod
// bound. It knows nothing about HTTP or tokens: peers send the node's own
// algod token, and long-polls and streams pass untouched.
package passthrough

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/d13co/algod-loadb-mesh/internal/ports"
)

const dialTimeout = 5 * time.Second

// Server holds the pass-through listeners and the spliced connections.
type Server struct {
	lns       []net.Listener
	target    func() string // algod's host:port, read per connection
	log       ports.Logger
	metric    ports.Metrics
	accepting sync.WaitGroup  // one per accept loop, added under mu
	wg        sync.WaitGroup  // one per spliced connection, added by an accept loop
	dialCtx   context.Context // dials run under it; cutDials cancels it with the cut
	cutDials  context.CancelFunc
	mu        sync.Mutex
	closed    bool // Shutdown ran; Serve must not start accepting
	cut       bool // the drain window ended; anything tracked now is closed at once
	ncut      int  // connections closed by the cut
	conns     map[net.Conn]struct{}
	active    atomic.Int64
}

// Listen binds every address up front, closing what it opened if one fails.
// target is read at accept time so the splice follows a moved algod.
func Listen(addrs []string, target func() string, log ports.Logger, metric ports.Metrics) (*Server, error) {
	lns := make([]net.Listener, 0, len(addrs))
	for _, addr := range addrs {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			for _, ln := range lns {
				_ = ln.Close()
			}
			return nil, fmt.Errorf("passthrough %s: %w", addr, err)
		}
		lns = append(lns, ln)
	}
	return newServer(lns, target, log, metric), nil
}

// newServer takes listeners already bound; tests script them.
func newServer(lns []net.Listener, target func() string, log ports.Logger, metric ports.Metrics) *Server {
	s := &Server{lns: lns, target: target, log: log, metric: metric, conns: map[net.Conn]struct{}{}}
	s.dialCtx, s.cutDials = context.WithCancel(context.Background())
	return s
}

// Addrs lists the bound addresses.
func (s *Server) Addrs() []string {
	out := make([]string, 0, len(s.lns))
	for _, ln := range s.lns {
		out = append(out, ln.Addr().String())
	}
	return out
}

// Active is the number of spliced connections right now.
func (s *Server) Active() int64 { return s.active.Load() }

// Serve accepts on every listener until ctx is done or an Accept fails for
// good. It returns nil on cancellation: it shares an error channel with the
// HTTP listeners, where any error means "stop before draining".
func (s *Server) Serve(ctx context.Context) error {
	// Added under mu so Shutdown either sees the accept loops or runs first,
	// in which case there is nothing to serve.
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.accepting.Add(len(s.lns))
	s.mu.Unlock()
	errc := make(chan error, len(s.lns))
	for _, ln := range s.lns {
		go func() {
			defer s.accepting.Done()
			errc <- s.accept(ctx, ln)
		}()
	}
	var err error
	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	s.closeListeners()
	s.accepting.Wait()
	return err
}

func (s *Server) accept(ctx context.Context, ln net.Listener) error {
	delay := 5 * time.Millisecond
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(delay):
				}
				if delay *= 2; delay > time.Second {
					delay = time.Second
				}
				continue
			}
			return fmt.Errorf("passthrough %s: %w", ln.Addr(), err)
		}
		delay = 5 * time.Millisecond
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.splice(ctx, ln.Addr().String(), c)
		}()
	}
}

// splice moves bytes both ways between c and a new connection to algod,
// half-closing each side when the other finishes writing.
func (s *Server) splice(ctx context.Context, addr string, c net.Conn) {
	defer c.Close()
	s.metric.Inc("loadb_passthrough_accepted", "addr", addr)
	t := s.target()
	if t == "" {
		s.metric.Inc("loadb_passthrough_dial_failures", "reason", "no_target")
		return
	}
	// Under dialCtx, not ctx: a dial in flight when the drain window ends
	// is abandoned, while one during the drain still completes.
	dctx, cancel := context.WithTimeout(s.dialCtx, dialTimeout)
	up, err := (&net.Dialer{}).DialContext(dctx, "tcp", t)
	cancel()
	if err != nil {
		s.metric.Inc("loadb_passthrough_dial_failures", "reason", "dial")
		s.log.Debug("passthrough dial failed", "target", t, "err", err)
		return
	}
	defer up.Close()
	s.track(c, up, true)
	defer s.track(c, up, false)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); copyThenClose(up, c) }()
	go func() { defer wg.Done(); copyThenClose(c, up) }()
	wg.Wait()
}

// track records a spliced pair, or forgets it. A pair tracked after the cut
// is closed on the spot, so a splice that was still dialing when the cut
// ran cannot outlive Shutdown.
func (s *Server) track(c, up net.Conn, add bool) {
	s.mu.Lock()
	if add {
		s.conns[c], s.conns[up] = struct{}{}, struct{}{}
		if s.cut {
			_, _ = c.Close(), up.Close()
			s.ncut++
		}
	} else {
		delete(s.conns, c)
		delete(s.conns, up)
	}
	s.mu.Unlock()
	delta := int64(-1)
	if add {
		delta = 1
	}
	n := s.active.Add(delta)
	s.metric.Gauge("loadb_passthrough_active", float64(n))
}

// copyThenClose copies src to dst, then tells dst there is nothing more to
// read (CloseWrite, or Close when the connection cannot half-close).
func copyThenClose(dst, src net.Conn) {
	_, _ = io.Copy(dst, src)
	if cw, ok := dst.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	} else {
		_ = dst.Close()
	}
}

// Shutdown stops accepting, waits until ctx is done for the spliced
// connections to finish, then cuts the rest. It always returns nil: cut
// connections are logged, not propagated.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	s.closeListeners()
	s.mu.Unlock()
	// An accept loop ends once its listener is closed, and only after its
	// last wg.Add: waiting on the loops first keeps every Add ahead of the
	// Wait below, as WaitGroup requires.
	s.accepting.Wait()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
	}
	s.mu.Lock()
	s.cut = true
	s.ncut += len(s.conns) / 2
	for c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()
	s.cutDials()
	<-done
	s.mu.Lock()
	n := s.ncut
	s.mu.Unlock()
	if n > 0 {
		s.log.Info("passthrough cut open connections at shutdown", "n", n)
	}
	return nil
}

// Close is Shutdown without a drain window: every spliced connection is cut
// at once. For the paths that stop the agent without draining.
func (s *Server) Close() error {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return s.Shutdown(ctx)
}

func (s *Server) closeListeners() {
	for _, ln := range s.lns {
		_ = ln.Close()
	}
}

// HostPort is the host:port of a base URL such as Monitor.State().Endpoint,
// with IPv6 brackets kept so the result is dialable; "" when unparsable.
func HostPort(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	return u.Host
}
