// Package gossipudp is the v2.0 control-plane transport: one UDP datagram per
// heartbeat or probe, over the WireGuard network, from one socket per mesh
// interface.
package gossipudp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/d13co/algod-loadb-mesh/internal/domain"
	"github.com/d13co/algod-loadb-mesh/internal/ports"
)

// Transport implements ports.Gossip over one UDP socket per listen address.
type Transport struct {
	conns []*net.UDPConn
	recv  chan ports.GossipMessage

	mu     sync.Mutex
	routes map[string]route // destination -> resolved once, and the socket it leaves from; successful picks only

	once sync.Once
	done chan struct{}
	wg   sync.WaitGroup
}

// route is what Send caches per destination string: the resolved address,
// so a hostname in mesh.advertise is looked up once rather than per
// datagram, and the socket the kernel routes it from.
type route struct {
	to   *net.UDPAddr
	conn *net.UDPConn
}

// MaxDatagram bounds heartbeat size; anything larger is dropped.
const MaxDatagram = 1400

// Listen binds every address and starts one receive loop per socket, all
// feeding one channel. If a later bind fails, what was opened is closed.
func Listen(addrs []string) (*Transport, error) {
	if len(addrs) == 0 {
		return nil, errors.New("gossipudp: no listen address")
	}
	t := &Transport{recv: make(chan ports.GossipMessage, 256), routes: map[string]route{}, done: make(chan struct{})}
	for _, addr := range addrs {
		ua, err := net.ResolveUDPAddr("udp", addr)
		if err == nil {
			var conn *net.UDPConn
			conn, err = net.ListenUDP("udp", ua)
			if err == nil {
				t.conns = append(t.conns, conn)
				continue
			}
		}
		for _, c := range t.conns {
			_ = c.Close()
		}
		return nil, fmt.Errorf("%s: %w", addr, err)
	}
	for _, c := range t.conns {
		t.wg.Add(1)
		go t.loop(c)
	}
	return t, nil
}

// LocalAddrs lists the bound addresses, useful with port 0 in tests.
func (t *Transport) LocalAddrs() []string {
	out := make([]string, 0, len(t.conns))
	for _, c := range t.conns {
		out = append(out, domain.NormalizeAddr(c.LocalAddr().String()))
	}
	return out
}

func (t *Transport) loop(conn *net.UDPConn) {
	defer t.wg.Done()
	buf := make([]byte, MaxDatagram)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-t.done:
				return
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		msg := ports.GossipMessage{From: domain.NormalizeAddr(from.String()), Payload: append([]byte(nil), buf[:n]...)}
		select {
		case t.recv <- msg:
		default: // receiver is slow; heartbeats are idempotent, drop
		}
	}
}

// Send implements ports.Gossip. The datagram leaves from the socket bound to
// the address the kernel would route it from, so a 10.114 peer is reached
// with a 10.114 source and WireGuard's cryptokey routing lets it through.
// A write error drops the cached route, so the next Send resolves and
// picks again.
func (t *Transport) Send(ctx context.Context, addr string, payload []byte) error {
	if len(payload) > MaxDatagram {
		return errors.New("gossipudp: payload exceeds datagram size")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	r, err := t.pick(addr)
	if err != nil {
		return err
	}
	if _, err := r.conn.WriteToUDP(payload, r.to); err != nil {
		t.mu.Lock()
		delete(t.routes, addr)
		t.mu.Unlock()
		return err
	}
	return nil
}

// pick resolves addr and finds the socket to send to it from, cached per
// destination. The kernel is asked rather than doing subnet arithmetic: a
// connect(2) on an unbound UDP socket sends nothing and only does the route
// lookup. No socket bound to the source the kernel chose (or a wildcard of
// its family) means the datagram would leave via an interface the mesh is
// not bound on, which is refused as ErrNoRoute.
func (t *Transport) pick(addr string) (route, error) {
	t.mu.Lock()
	r, ok := t.routes[addr]
	t.mu.Unlock()
	if ok {
		return r, nil
	}
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return route{}, fmt.Errorf("%w: %s: %v", ports.ErrNoRoute, addr, err)
	}
	probe, err := net.DialUDP("udp", nil, ua)
	if err != nil {
		return route{}, fmt.Errorf("%w: %s: %v", ports.ErrNoRoute, addr, err)
	}
	src := probe.LocalAddr().(*net.UDPAddr).IP
	_ = probe.Close()
	var conn, wildcard *net.UDPConn
	for _, c := range t.conns {
		local := c.LocalAddr().(*net.UDPAddr).IP
		switch {
		case local.Equal(src):
			conn = c
		case local.IsUnspecified() && wildcard == nil && sameFamily(local, src):
			wildcard = c
		}
	}
	if conn == nil {
		conn = wildcard
	}
	if conn == nil {
		return route{}, fmt.Errorf("%w: %s would leave from %s, which the mesh is not bound on", ports.ErrNoRoute, addr, src)
	}
	r = route{to: ua, conn: conn}
	t.mu.Lock()
	t.routes[addr] = r
	t.mu.Unlock()
	return r, nil
}

// sameFamily reports whether a wildcard socket on local can send from src:
// 0.0.0.0 only IPv4, [::] both (dual-stack, the Go default).
func sameFamily(local, src net.IP) bool {
	if local.To4() != nil {
		return src.To4() != nil
	}
	return true
}

// Receive implements ports.Gossip.
func (t *Transport) Receive() <-chan ports.GossipMessage { return t.recv }

// Close implements ports.Gossip. It is idempotent: several receive loops
// race to notice, and the agent may close after a failed start.
func (t *Transport) Close() error {
	var err error
	t.once.Do(func() {
		close(t.done)
		for _, c := range t.conns {
			if e := c.Close(); e != nil && err == nil {
				err = e
			}
		}
		t.wg.Wait()
		close(t.recv)
	})
	return err
}
