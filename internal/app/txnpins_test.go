package app

import (
	"testing"
	"time"
)

func TestTxnPinsLiveBetweenTTLAndTwiceTTL(t *testing.T) {
	t0 := time.Unix(1000, 0)
	p := newTxnPins(10*time.Second, t0)

	p.remember("A", "n1", t0)
	p.remember("", "n1", t0) // ignored
	if u, ok := p.recall("A", t0.Add(9*time.Second)); !ok || u != "n1" {
		t.Fatalf("A before ttl: %q %v", u, ok)
	}
	// Rotation at ttl moves A to the previous generation; still found.
	p.remember("B", "n2", t0.Add(10*time.Second))
	if u, ok := p.recall("A", t0.Add(19*time.Second)); !ok || u != "n1" {
		t.Fatalf("A in prev generation: %q %v", u, ok)
	}
	// The next rotation drops A but keeps B.
	if _, ok := p.recall("A", t0.Add(20*time.Second)); ok {
		t.Fatal("A should have expired")
	}
	if u, ok := p.recall("B", t0.Add(20*time.Second)); !ok || u != "n2" {
		t.Fatalf("B after one rotation: %q %v", u, ok)
	}
	// A long idle gap clears both generations at once.
	if _, ok := p.recall("B", t0.Add(time.Hour)); ok {
		t.Fatal("B should have expired after idle gap")
	}
	if _, ok := p.recall("", t0); ok {
		t.Fatal("empty txid must not match")
	}
}
