// Package gossipmem is an in-memory gossip transport for tests and the
// simulator: a Hub connects any number of Endpoints without sockets.
//
// An endpoint joins under one opaque address per net: address i of every
// endpoint is net i, and "" means "not on that net". Sending to another
// endpoint's net-i address from an endpoint with no net-i address is
// ErrNoRoute, which is how the mixed topology falls out of Join alone.
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
	hub   *Hub
	addrs []string
	recv  chan ports.GossipMessage
}

// Join creates an endpoint with one address per net; "" skips a net.
func (h *Hub) Join(addrs ...string) *Endpoint {
	e := &Endpoint{hub: h, addrs: append([]string(nil), addrs...), recv: make(chan ports.GossipMessage, 256)}
	h.mu.Lock()
	for _, a := range addrs {
		if a != "" {
			h.endpoints[a] = e
		}
	}
	h.mu.Unlock()
	return e
}

// Partition cuts one address off from everyone (true) or reconnects it
// (false): nothing is delivered to it, and nothing sent from it. Partition
// keys on the per-net address on both ends, so in a multi-net fleet it cuts
// one path; in a single-net fleet the one address is the whole node.
func (h *Hub) Partition(addr string, cut bool) {
	h.mu.Lock()
	h.partition[addr] = cut
	h.mu.Unlock()
}

// Addr returns the endpoint's net-0 address.
func (e *Endpoint) Addr() string { return e.addrs[0] }

// Addrs returns the endpoint's addresses, "" for nets it is not on.
func (e *Endpoint) Addrs() []string { return append([]string(nil), e.addrs...) }

// Send implements ports.Gossip. An unknown address is a black hole, as on a
// real network: nil, and nothing arrives.
func (e *Endpoint) Send(ctx context.Context, addr string, payload []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.hub.mu.RLock()
	defer e.hub.mu.RUnlock()
	dst, ok := e.hub.endpoints[addr]
	if !ok {
		return nil
	}
	src := ""
	for i, a := range dst.addrs {
		if a == addr && i < len(e.addrs) {
			src = e.addrs[i]
		}
	}
	if src == "" {
		return ports.ErrNoRoute
	}
	if e.hub.partition[src] || e.hub.partition[addr] {
		return nil
	}
	msg := ports.GossipMessage{From: src, Payload: append([]byte(nil), payload...)}
	select {
	case dst.recv <- msg:
	default:
	}
	return nil
}

// Receive implements ports.Gossip.
func (e *Endpoint) Receive() <-chan ports.GossipMessage { return e.recv }

// Close implements ports.Gossip.
func (e *Endpoint) Close() error {
	e.hub.mu.Lock()
	for _, a := range e.addrs {
		if a != "" && e.hub.endpoints[a] == e {
			delete(e.hub.endpoints, a)
		}
	}
	e.hub.mu.Unlock()
	return nil
}
