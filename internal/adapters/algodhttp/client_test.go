package algodhttp

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/d13co/algod-loadb-mesh/internal/fakealgod"
	"github.com/d13co/algod-loadb-mesh/internal/ports"
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
	n.PutBox(7, []byte("node:x"), []byte("val"))
	names, err := c.BoxNames(ctx, 7)
	if err != nil || len(names) != 1 || string(names[0]) != "node:x" {
		t.Fatalf("boxes: %v %q", err, names)
	}
	val, err := c.Box(ctx, 7, []byte("node:x"))
	if err != nil || string(val) != "val" {
		t.Fatalf("box: %v %q", err, val)
	}
	if _, err := c.Box(ctx, 7, []byte("nope")); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("missing box: %v", err)
	}
	bad := New(n.URL(), "wrong", nil)
	if _, err := bad.Status(ctx); err == nil {
		t.Fatal("wrong token must fail")
	}
}
