package gossipmem

import (
	"context"
	"errors"
	"testing"

	"github.com/d13co/algod-loadb-mesh/internal/ports"
)

func got(e *Endpoint) (ports.GossipMessage, bool) {
	select {
	case m := <-e.Receive():
		return m, true
	default:
		return ports.GossipMessage{}, false
	}
}

func TestPartitionCutsOnePath(t *testing.T) {
	ctx := context.Background()
	h := NewHub()
	a, b, c := h.Join("mem:a", "mem1:a"), h.Join("mem:b", "mem1:b"), h.Join("mem:c", "")
	if err := c.Send(ctx, "mem1:b", []byte("x")); !errors.Is(err, ports.ErrNoRoute) {
		t.Fatalf("no net-1 address: %v", err)
	}
	if err := c.Send(ctx, "mem:b", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if m, ok := got(b); !ok || m.From != "mem:c" {
		t.Fatalf("c to b: %+v %v", m, ok)
	}
	h.Partition("mem:a", true)
	_ = a.Send(ctx, "mem:b", []byte("0"))
	if _, ok := got(b); ok {
		t.Fatal("net 0 is cut")
	}
	_ = a.Send(ctx, "mem1:b", []byte("1"))
	if m, ok := got(b); !ok || m.From != "mem1:a" || string(m.Payload) != "1" {
		t.Fatalf("net 1 still delivers: %+v %v", m, ok)
	}
	_ = b.Send(ctx, "mem:a", []byte("0"))
	if _, ok := got(a); ok {
		t.Fatal("nothing arrives on the cut address")
	}
	_ = b.Send(ctx, "mem1:a", []byte("1"))
	if m, ok := got(a); !ok || m.From != "mem1:b" {
		t.Fatalf("the other path still delivers: %+v %v", m, ok)
	}
	h.Partition("mem:a", false)
	_ = a.Send(ctx, "mem:b", []byte("0"))
	if _, ok := got(b); !ok {
		t.Fatal("reconnected")
	}
	if err := a.Send(ctx, "mem:nobody", []byte("x")); err != nil {
		t.Fatalf("an unknown address is a black hole, not an error: %v", err)
	}
}
