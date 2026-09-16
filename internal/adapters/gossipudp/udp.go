// Package gossipudp is the v2.0 control-plane transport: one UDP datagram per
// heartbeat to every peer, over the WireGuard network.
package gossipudp

import (
	"context"
	"errors"
	"net"
	"sync"

	"github.com/d13co/algod-loadb-mesh/internal/ports"
)

// Transport implements ports.Gossip over a single UDP socket.
type Transport struct {
	conn *net.UDPConn
	recv chan ports.GossipMessage

	mu    sync.RWMutex
	peers []*net.UDPAddr
	done  chan struct{}
}

// MaxDatagram bounds heartbeat size; anything larger is dropped.
const MaxDatagram = 1400

// Listen binds the socket and starts the receive loop.
func Listen(addr string) (*Transport, error) {
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp", ua)
	if err != nil {
		return nil, err
	}
	t := &Transport{conn: conn, recv: make(chan ports.GossipMessage, 256), done: make(chan struct{})}
	go t.loop()
	return t, nil
}

// LocalAddr is the bound address, useful with port 0 in tests.
func (t *Transport) LocalAddr() string { return t.conn.LocalAddr().String() }

func (t *Transport) loop() {
	defer close(t.recv)
	buf := make([]byte, MaxDatagram)
	for {
		n, from, err := t.conn.ReadFromUDP(buf)
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
		msg := ports.GossipMessage{From: from.String(), Payload: append([]byte(nil), buf[:n]...)}
		select {
		case t.recv <- msg:
		default: // receiver is slow; heartbeats are idempotent, drop
		}
	}
}

// SetPeers implements ports.Gossip.
func (t *Transport) SetPeers(addrs []string) {
	peers := make([]*net.UDPAddr, 0, len(addrs))
	for _, a := range addrs {
		if ua, err := net.ResolveUDPAddr("udp", a); err == nil {
			peers = append(peers, ua)
		}
	}
	t.mu.Lock()
	t.peers = peers
	t.mu.Unlock()
}

// Broadcast implements ports.Gossip. Send errors to individual peers are
// ignored: liveness is judged by the receiver, not the sender.
func (t *Transport) Broadcast(ctx context.Context, payload []byte) error {
	if len(payload) > MaxDatagram {
		return errors.New("gossipudp: payload exceeds datagram size")
	}
	t.mu.RLock()
	peers := t.peers
	t.mu.RUnlock()
	for _, p := range peers {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		_, _ = t.conn.WriteToUDP(payload, p)
	}
	return nil
}

// Receive implements ports.Gossip.
func (t *Transport) Receive() <-chan ports.GossipMessage { return t.recv }

// Close implements ports.Gossip.
func (t *Transport) Close() error {
	close(t.done)
	return t.conn.Close()
}
