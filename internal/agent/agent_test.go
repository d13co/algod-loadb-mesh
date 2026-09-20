package agent

import (
	"net"
	"testing"

	"github.com/d13co/algod-loadb-mesh/internal/config"
)

func TestListenAll(t *testing.T) {
	lns, err := listenAll(config.Addrs{"127.0.0.1:0", "127.0.0.1:0"})
	if err != nil || len(lns) != 2 {
		t.Fatalf("two listeners: %v %d", err, len(lns))
	}
	if lns[0].Addr().String() == lns[1].Addr().String() {
		t.Errorf("both listeners on %s", lns[0].Addr())
	}
	closeAll(lns)

	// One address that cannot be bound fails the whole call, and the
	// listeners it had already opened are closed.
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	free := freeAddr(t)
	if _, err := listenAll(config.Addrs{free, busy.Addr().String()}); err == nil {
		t.Fatal("a busy address must fail")
	}
	ln, err := net.Listen("tcp", free)
	if err != nil {
		t.Fatalf("%s was left bound: %v", free, err)
	}
	ln.Close()
}

// freeAddr returns an address that was free a moment ago.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}
