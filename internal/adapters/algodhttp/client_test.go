package algodhttp

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/d13co/algod-mesh/internal/domain"
	"github.com/d13co/algod-mesh/internal/fakealgod"
	"github.com/d13co/algod-mesh/internal/ports"
)

func TestClientAgainstFakeAlgod(t *testing.T) {
	n := fakealgod.New(fakealgod.Options{ID: "a", StartRound: 500, OldestRound: 100})
	defer n.Close()
	c := New(n.URL(), n.Token(), nil)
	ctx := context.Background()

	st, err := c.Status(ctx)
	if err != nil || st.LastRound != 500 || len(st.Raw) == 0 {
		t.Fatalf("status: %v %+v", err, st)
	}
	for r, want := range map[uint64]bool{99: false, 100: true, 500: true, 501: false} {
		got, err := c.HasBlock(ctx, r)
		if err != nil || got != want {
			t.Fatalf("HasBlock(%d)=%v,%v want %v", r, got, err, want)
		}
	}
	v, err := c.Versions(ctx)
	if err != nil || v.GenesisID != "fakenet-v1" {
		t.Fatalf("versions: %v %+v", err, v)
	}
	go func() { time.Sleep(50 * time.Millisecond); n.Advance(1) }()
	st, err = c.WaitForBlockAfter(ctx, 500)
	if err != nil || st.LastRound != 501 {
		t.Fatalf("wait: %v %+v", err, st)
	}
	n.PutBox(7, []byte("nx"), []byte("val"))
	names, err := c.BoxNames(ctx, 7)
	if err != nil || len(names) != 1 || string(names[0]) != "nx" {
		t.Fatalf("boxes: %v %q", err, names)
	}
	val, err := c.Box(ctx, 7, []byte("nx"))
	if err != nil || string(val) != "val" {
		t.Fatalf("box: %v %q", err, val)
	}
	if _, err := c.Box(ctx, 7, []byte("nope")); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("missing box: %v", err)
	}
	if lim, err := c.SimulateLimits(ctx); err != nil || lim != nil {
		t.Fatalf("stock simulate limits: %v %+v", err, lim)
	}
	want := domain.SimulateLimits{MaxTxnGroups: 16, MaxRounds: 4, MaxExtraOpcodeBudget: 640000, ResourceLimitMultiplier: 2, MaxExecTraceBytes: 1 << 28, TimeLimitSeconds: 64}
	dt := fakealgod.New(fakealgod.Options{ID: "dt", StartRound: 5, Simulate: &want})
	defer dt.Close()
	if lim, err := New(dt.URL(), dt.Token(), nil).SimulateLimits(ctx); err != nil || lim == nil || *lim != want {
		t.Fatalf("dtalgorand simulate limits: %v %+v", err, lim)
	}
	bad := New(n.URL(), "wrong", nil)
	if _, err := bad.Status(ctx); err == nil {
		t.Fatal("wrong token must fail")
	}
}
