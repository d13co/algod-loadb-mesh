// Package gossipmem is an in-memory gossip transport for tests and the
// simulator: a Hub connects any number of Endpoints without sockets.
package gossipmem

import (
	"context"
	"sync"

	"github.com/d13co/algod-loadb-mesh/internal/ports"
)

// Hub routes datagrams between endpoints by address.
type Hub struct {
	mu        sync.RWMutex
	endpoints map[string]*Endpoint
	partition map[string]bool // addresses cut off from everyone
}

// NewHub creates an empty hub.
func NewHub() *Hub {
	return &Hub{endpoints: map[string]*Endpoint{}, partition: map[string]bool{}}
}

// Endpoint implements ports.Gossip.
type Endpoint struct {
	hub  *Hub
	addr string
	recv chan ports.GossipMessage

	mu    sync.RWMutex
	peers []string
}

// Join creates an endpoint with the given address.
func (h *Hub) Join(addr string) *Endpoint {
	e := &Endpoint{hub: h, addr: addr, recv: make(chan ports.GossipMessage, 256)}
	h.mu.Lock()
	h.endpoints[addr] = e
	h.mu.Unlock()
	return e
}

// Partition isolates an address (true) or reconnects it (false).
func (h *Hub) Partition(addr string, cut bool) {
	h.mu.Lock()
	h.partition[addr] = cut
	h.mu.Unlock()
}

// Addr returns the endpoint's address.
func (e *Endpoint) Addr() string { return e.addr }

// SetPeers implements ports.Gossip.
func (e *Endpoint) SetPeers(addrs []string) {
	e.mu.Lock()
	e.peers = append([]string(nil), addrs...)
	e.mu.Unlock()
}

// Broadcast implements ports.Gossip.
func (e *Endpoint) Broadcast(ctx context.Context, payload []byte) error {
	e.mu.RLock()
	peers := e.peers
	e.mu.RUnlock()
	e.hub.mu.RLock()
	defer e.hub.mu.RUnlock()
	if e.hub.partition[e.addr] {
		return nil
	}
	for _, p := range peers {
		dst, ok := e.hub.endpoints[p]
		if !ok || e.hub.partition[p] {
			continue
		}
		msg := ports.GossipMessage{From: e.addr, Payload: append([]byte(nil), payload...)}
		select {
		case dst.recv <- msg:
		default:
		}
	}
	return nil
}

// Receive implements ports.Gossip.
func (e *Endpoint) Receive() <-chan ports.GossipMessage { return e.recv }

// Close implements ports.Gossip.
func (e *Endpoint) Close() error {
	e.hub.mu.Lock()
	delete(e.hub.endpoints, e.addr)
	e.hub.mu.Unlock()
	return nil
}
