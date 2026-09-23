package domain

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
)

// Registry box format: version(1) || nonce(12) || AES-256-GCM(ciphertext+tag)
// of the JSON-encoded NodeRecord. The AEAD's additional data is the box key so
// a record cannot be copied under another node's name.
//
// The box key is BoxKeyPrefix || BoxTag: a keyed hash of the node id, so the
// chain shows neither ids nor their lengths.
const (
	recordVersion  = 1
	recordNonceLen = 12
	BoxKeyPrefix   = "n"
	BoxTagLen      = 16
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

// BoxTag names the box of a node id: HMAC-SHA256 of the id under a key
// derived from the sync key, truncated to BoxTagLen bytes. Only key holders
// can compute it or tell which node a box belongs to.
func BoxTag(key SyncKey, id string) [BoxTagLen]byte {
	tagKey, err := hkdf.Key(sha256.New, key[:], []byte("algod-loadb-mesh"), "registry-box-tag-v1", 32)
	if err != nil {
		panic(err) // only fails for lengths beyond 255 × 32
	}
	mac := hmac.New(sha256.New, tagKey)
	mac.Write([]byte(id))
	var tag [BoxTagLen]byte
	copy(tag[:], mac.Sum(nil))
	return tag
}

// BoxName is the box key for a node id: BoxKeyPrefix || BoxTag.
func BoxName(key SyncKey, id string) []byte {
	tag := BoxTag(key, id)
	return append([]byte(BoxKeyPrefix), tag[:]...)
}

// IsRecordBox reports whether a box key has the shape of a record box. Which
// node it belongs to is known only once its record is opened.
func IsRecordBox(name []byte) bool {
	return len(name) == len(BoxKeyPrefix)+BoxTagLen && string(name[:len(BoxKeyPrefix)]) == BoxKeyPrefix
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
	out = aead.Seal(out, nonce, plain, BoxName(key, rec.ID))
	return out, nil
}

// OpenRecord decrypts and validates the value of the box named name. The
// record's id must be the one the name was derived from.
func OpenRecord(key SyncKey, name []byte, blob []byte) (NodeRecord, error) {
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
	plain, err := aead.Open(nil, nonce, blob[1+recordNonceLen:], name)
	if err != nil {
		return rec, fmt.Errorf("open box %x: %w", name, err)
	}
	if err := json.Unmarshal(plain, &rec); err != nil {
		return rec, fmt.Errorf("open box %x: %w", name, err)
	}
	if !bytes.Equal(BoxName(key, rec.ID), name) {
		return rec, fmt.Errorf("open: box %x holds record for %q, which is not its id", name, rec.ID)
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

// Gossip wire format: pubkey(32) || sig(64) || JSON body, the signature over
// the body. The receiver checks the pubkey against the registry record of
// the claimed node id, so a message can only speak for the node whose key
// signed it. The body is a Heartbeat, or a Probe when it carries "t" — a
// field a heartbeat never has.
const heartbeatHeader = ed25519.PublicKeySize + ed25519.SignatureSize

// Probe is a per-path ping or its pong. HeardMS says how long ago the sender
// last accepted a heartbeat from the recipient over any path (-1: never), so
// the recipient learns whether its chosen path is being heard.
type Probe struct {
	Type    string `json:"t"` // "ping" | "pong"
	NodeID  string `json:"id"`
	Nonce   uint64 `json:"n"` // echoed verbatim by the pong
	HeardMS int64  `json:"heard_ms"`
}

const (
	ProbePing = "ping"
	ProbePong = "pong"
)

// Envelope is one verified gossip message: exactly one of HB and Probe is set.
type Envelope struct {
	PubKey ed25519.PublicKey
	NodeID string
	HB     *Heartbeat
	Probe  *Probe
}

func encodeSigned(priv ed25519.PrivateKey, body []byte) []byte {
	out := make([]byte, 0, heartbeatHeader+len(body))
	out = append(out, priv.Public().(ed25519.PublicKey)...)
	out = append(out, ed25519.Sign(priv, body)...)
	return append(out, body...)
}

// EncodeHeartbeat signs and serialises a heartbeat.
func EncodeHeartbeat(priv ed25519.PrivateKey, hb Heartbeat) ([]byte, error) {
	body, err := json.Marshal(hb)
	if err != nil {
		return nil, err
	}
	return encodeSigned(priv, body), nil
}

// EncodeProbe signs and serialises a ping or pong.
func EncodeProbe(priv ed25519.PrivateKey, p Probe) ([]byte, error) {
	if p.Type != ProbePing && p.Type != ProbePong {
		return nil, fmt.Errorf("probe: bad type %q", p.Type)
	}
	body, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	return encodeSigned(priv, body), nil
}

// DecodeMessage verifies the signature once and returns the heartbeat or
// probe it carries with the signing key. Trust of the key is the caller's
// decision.
func DecodeMessage(wire []byte) (Envelope, error) {
	var env Envelope
	if len(wire) < heartbeatHeader+2 {
		return env, errors.New("gossip: too short")
	}
	pub := ed25519.PublicKey(wire[:ed25519.PublicKeySize])
	sig := wire[ed25519.PublicKeySize:heartbeatHeader]
	body := wire[heartbeatHeader:]
	if !ed25519.Verify(pub, body, sig) {
		return env, errors.New("gossip: bad signature")
	}
	var kind struct {
		Type string `json:"t"`
	}
	if err := json.Unmarshal(body, &kind); err != nil {
		return env, err
	}
	env.PubKey = pub
	if kind.Type == "" {
		var hb Heartbeat
		if err := json.Unmarshal(body, &hb); err != nil {
			return env, err
		}
		if hb.NodeID == "" {
			return env, errors.New("heartbeat: empty node id")
		}
		env.HB, env.NodeID = &hb, hb.NodeID
		return env, nil
	}
	var p Probe
	if err := json.Unmarshal(body, &p); err != nil {
		return env, err
	}
	if p.Type != ProbePing && p.Type != ProbePong {
		return env, fmt.Errorf("probe: bad type %q", p.Type)
	}
	if p.NodeID == "" {
		return env, errors.New("probe: empty node id")
	}
	env.Probe, env.NodeID = &p, p.NodeID
	return env, nil
}

// DecodeHeartbeat verifies a heartbeat and returns it with the signing key.
// A probe is an error here, so a ping can never land as a Seq 0 heartbeat.
func DecodeHeartbeat(wire []byte) (Heartbeat, ed25519.PublicKey, error) {
	env, err := DecodeMessage(wire)
	if err != nil {
		return Heartbeat{}, nil, err
	}
	if env.HB == nil {
		return Heartbeat{}, nil, errors.New("heartbeat: message is a probe")
	}
	return *env.HB, env.PubKey, nil
}
