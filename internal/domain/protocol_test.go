package domain

import (
	"encoding/hex"
	"testing"
)

// The registry key derivation is a protocol constant: its outputs must never
// change, or existing registries become unreadable and old and new nodes
// stop understanding each other. These vectors pin it.
func TestRegistryKeyDerivationIsFrozen(t *testing.T) {
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i)
	}
	key, err := DeriveSyncKey(seed)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := hex.EncodeToString(key[:]), "150f6b0f513806718a401db3eb01afe9ba81bc46eae623b655be0f4255c24095"; got != want {
		t.Errorf("DeriveSyncKey = %s, want %s", got, want)
	}
	tag := BoxTag(key, "node-1")
	if got, want := hex.EncodeToString(tag[:]), "2f9498369a9ac3a9ac19037ed2bf4f76"; got != want {
		t.Errorf("BoxTag = %s, want %s", got, want)
	}
}
