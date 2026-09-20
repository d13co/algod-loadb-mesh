package gossipudp

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/d13co/algod-loadb-mesh/internal/ports"
)

func recv(t *testing.T, tr *Transport) ports.GossipMessage {
	t.Helper()
	select {
	case m := <-tr.Receive():
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("nothing received")
		return ports.GossipMessage{}
	}
}

func TestSocketsFeedOneChannel(t *testing.T) {
	ctx := context.Background()
	a, err := Listen([]string{"127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Listen([]string{"127.0.0.1:0", "127.0.0.2:0"})
	if err != nil {
		t.Skipf("loopback alias 127.0.0.2 not bindable: %v", err)
	}
	defer b.Close()
	for i, addr := range b.LocalAddrs() {
		if err := a.Send(ctx, addr, []byte{byte(i)}); err != nil {
			t.Fatal(err)
		}
		m := recv(t, b)
		// From is the sender's address, normalised: no [::ffff:...] spelling.
		if m.From != a.LocalAddrs()[0] || len(m.Payload) != 1 || m.Payload[0] != byte(i) {
			t.Fatalf("via %s: got %+v from %s", addr, m, a.LocalAddrs()[0])
		}
	}
	if err := b.Send(ctx, a.LocalAddrs()[0], []byte("x")); err != nil {
		t.Fatal(err)
	}
	if m := recv(t, a); string(m.Payload) != "x" {
		t.Fatalf("reply: %+v", m)
	}
}

func TestSendRefusesWhatItCannotRoute(t *testing.T) {
	ctx := context.Background()
	a, err := Listen([]string{"127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err := a.Send(ctx, "nope", []byte("x")); !errors.Is(err, ports.ErrNoRoute) {
		t.Fatalf("unparseable address: %v", err)
	}
	if err := a.Send(ctx, a.LocalAddrs()[0], make([]byte, MaxDatagram+1)); err == nil || errors.Is(err, ports.ErrNoRoute) {
		t.Fatalf("oversize payload: %v", err)
	}
	// A transport bound only on 127.0.0.2 would leave for 127.0.0.1 from
	// 127.0.0.1, which it is not bound on: refused rather than sent from the
	// wrong interface.
	c, err := Listen([]string{"127.0.0.2:0"})
	if err != nil {
		t.Skipf("loopback alias 127.0.0.2 not bindable: %v", err)
	}
	defer c.Close()
	if err := c.Send(ctx, a.LocalAddrs()[0], []byte("x")); !errors.Is(err, ports.ErrNoRoute) {
		t.Fatalf("unbound source must be ErrNoRoute: %v", err)
	}
	// A wildcard socket sends anywhere.
	w, err := Listen([]string{"0.0.0.0:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := w.Send(ctx, a.LocalAddrs()[0], []byte("w")); err != nil {
		t.Fatal(err)
	}
	if m := recv(t, a); string(m.Payload) != "w" {
		t.Fatalf("via wildcard: %+v", m)
	}
}

func TestListenClosesOnPartialFailure(t *testing.T) {
	a, err := Listen([]string{"127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	// The second bind collides with a's port; the first must be released.
	if _, err := Listen([]string{"127.0.0.1:0", a.LocalAddrs()[0]}); err == nil {
		t.Fatal("colliding bind must fail")
	}
}

func TestCloseTwice(t *testing.T) {
	a, err := Listen([]string{"127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok := <-a.Receive(); ok {
		t.Fatal("receive channel must be closed")
	}
}
