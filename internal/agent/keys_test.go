package agent

import (
	"encoding/hex"
	"testing"
)

// Agent key derivation is a protocol constant: a changed output would make
// every node's heartbeat key differ from what its peers expect.
func TestAgentKeyIsFrozen(t *testing.T) {
	material := make([]byte, 32)
	for i := range material {
		material[i] = byte(i)
	}
	k, err := AgentKey(material, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := hex.EncodeToString(k.Seed()), "406ef32b90c524c8caad28a97696afb8a964cd2e9d4dc964fed19bf549ab7482"; got != want {
		t.Errorf("AgentKey seed = %s, want %s", got, want)
	}
}
