package agent

import (
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/sha256"
)

// AgentKey derives a node's heartbeat signing key from the fleet key material
// and its id. Every holder of the material can derive every agent's key,
// which is the trust model requested (one sync account, no RBAC); moving to
// per-agent keys later only changes this function and the registry record.
func AgentKey(material []byte, nodeID string) (ed25519.PrivateKey, error) {
	seed, err := hkdf.Key(sha256.New, material, []byte("algod-loadb"), "agent-key:"+nodeID, ed25519.SeedSize)
	if err != nil {
		return nil, err
	}
	return ed25519.NewKeyFromSeed(seed), nil
}
