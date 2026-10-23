package config

import (
	"encoding/hex"
	"testing"
)

// The shared-secret key material is a protocol constant: a changed output
// would change every agent key derived from it.
func TestSharedSecretKeyMaterialIsFrozen(t *testing.T) {
	var c Config
	c.Mesh.SharedSecret = "s3cret"
	m, err := c.KeyMaterial()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := hex.EncodeToString(m), "a3aa420300db56e5fa3f6420cf938ae56983366a53c3f9141ca80c482c9cac44"; got != want {
		t.Errorf("KeyMaterial = %s, want %s", got, want)
	}
}
