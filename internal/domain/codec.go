package domain

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
)

// Registry box format: version(1) || nonce(12) || AES-256-GCM(ciphertext+tag)
// of the JSON-encoded NodeRecord. The AEAD's additional data is the box key so
// a record cannot be copied under another node's name.
const (
	recordVersion  = 1
	recordNonceLen = 12
	BoxKeyPrefix   = "n"
)

// SyncKey is the fleet-wide symmetric key derived from the sync account.
type SyncKey [32]byte

// DeriveSyncKey turns the sync account's 32-byte ed25519 seed into the AEAD key
// with HKDF-SHA256 and a fixed context so the on-chain key is never reused
// for encryption directly.
func DeriveSyncKey(seed []byte) (SyncKey, error) {
	var k SyncKey
	if len(seed) < 32 {
		return k, errors.New("sync key: seed shorter than 32 bytes")
	}
	out, err := hkdf.Key(sha256.New, seed[:32], []byte("algod-loadb-mesh"), "registry-v1", 32)
	if err != nil {
		return k, err
	}
	copy(k[:], out)
	return k, nil
}

// BoxName is the box key for a node id.
func BoxName(id string) []byte { return []byte(BoxKeyPrefix + id) }

// IDFromBoxName is the inverse of BoxName; ok is false for foreign boxes.
func IDFromBoxName(name []byte) (string, bool) {
	s := string(name)
	if len(s) <= len(BoxKeyPrefix) || s[:len(BoxKeyPrefix)] != BoxKeyPrefix {
		return "", false
	}
	return s[len(BoxKeyPrefix):], true
}

// SealRecord encrypts a record for storage in its box. nonce must be 12
// random bytes supplied by the caller (randomness is a port, not a domain
// concern).
func SealRecord(key SyncKey, rec NodeRecord, nonce []byte) ([]byte, error) {
	if err := rec.Validate(); err != nil {
		return nil, err
	}
	if len(nonce) != recordNonceLen {
		return nil, fmt.Errorf("seal: nonce must be %d bytes", recordNonceLen)
	}
	plain, err := json.Marshal(rec)
	if err != nil {
		return nil, err
	}
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, 1+recordNonceLen+len(plain)+aead.Overhead())
	out = append(out, recordVersion)
	out = append(out, nonce...)
	out = aead.Seal(out, nonce, plain, BoxName(rec.ID))
	return out, nil
}

// OpenRecord decrypts and validates a box value. The id is taken from the box
// name and must match the record inside.
func OpenRecord(key SyncKey, id string, blob []byte) (NodeRecord, error) {
	var rec NodeRecord
	if len(blob) < 1+recordNonceLen {
		return rec, errors.New("open: blob too short")
	}
	if blob[0] != recordVersion {
		return rec, fmt.Errorf("open: unsupported record version %d", blob[0])
	}
	aead, err := newAEAD(key)
	if err != nil {
		return rec, err
	}
	nonce := blob[1 : 1+recordNonceLen]
	plain, err := aead.Open(nil, nonce, blob[1+recordNonceLen:], BoxName(id))
	if err != nil {
		return rec, fmt.Errorf("open %q: %w", id, err)
	}
	if err := json.Unmarshal(plain, &rec); err != nil {
		return rec, fmt.Errorf("open %q: %w", id, err)
	}
	if rec.ID != id {
		return rec, fmt.Errorf("open: box %q holds record for %q", id, rec.ID)
	}
	return rec, rec.Validate()
}

func newAEAD(key SyncKey) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Heartbeat wire format: pubkey(32) || sig(64) || JSON(Heartbeat).
// The receiver checks the pubkey against the registry record of the claimed
// node id, so a heartbeat can only speak for the node whose key signed it.
const heartbeatHeader = ed25519.PublicKeySize + ed25519.SignatureSize

// EncodeHeartbeat signs and serialises a heartbeat.
func EncodeHeartbeat(priv ed25519.PrivateKey, hb Heartbeat) ([]byte, error) {
	body, err := json.Marshal(hb)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, heartbeatHeader+len(body))
	out = append(out, priv.Public().(ed25519.PublicKey)...)
	out = append(out, ed25519.Sign(priv, body)...)
	return append(out, body...), nil
}

// DecodeHeartbeat verifies the signature and returns the heartbeat together
// with the signing key. Trust of the key is the caller's decision.
func DecodeHeartbeat(wire []byte) (Heartbeat, ed25519.PublicKey, error) {
	var hb Heartbeat
	if len(wire) < heartbeatHeader+2 {
		return hb, nil, errors.New("heartbeat: too short")
	}
	pub := ed25519.PublicKey(wire[:ed25519.PublicKeySize])
	sig := wire[ed25519.PublicKeySize:heartbeatHeader]
	body := wire[heartbeatHeader:]
	if !ed25519.Verify(pub, body, sig) {
		return hb, nil, errors.New("heartbeat: bad signature")
	}
	if err := json.Unmarshal(body, &hb); err != nil {
		return hb, nil, err
	}
	if hb.NodeID == "" {
		return hb, nil, errors.New("heartbeat: empty node id")
	}
	return hb, pub, nil
}
