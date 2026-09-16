package config

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"github.com/algorand/go-algorand-sdk/v2/types"
)

// A registry bundle carries what a new host needs to join an on-chain
// registry, as standard base64 of
//
//	app id (8 bytes, big endian) | sync address (32 bytes, only when it is
//	not the sync key's own address, i.e. rekeyed) | sync seed (32 bytes)
//
// deploy/autoconfig.sh decodes the same layout.
type Bundle struct {
	AppID       uint64
	SyncAddress string // empty: the seed's own address
	Seed        []byte // 32 bytes; what the sync key mnemonic encodes
}

// Bundle builds the registry bundle of an algorand registry config.
func (c *Config) Bundle() (Bundle, error) {
	if c.Registry.AppID == 0 {
		return Bundle{}, errors.New("registry.app_id is not set")
	}
	seed, err := c.SyncSeed()
	if err != nil {
		return Bundle{}, err
	}
	if seed == nil {
		return Bundle{}, errors.New("registry.sync_key is not set")
	}
	return Bundle{AppID: c.Registry.AppID, SyncAddress: c.Registry.SyncAddress, Seed: seed}, nil
}

func (b Bundle) String() string {
	buf := binary.BigEndian.AppendUint64(nil, b.AppID)
	if b.SyncAddress != "" {
		addr, err := types.DecodeAddress(b.SyncAddress)
		own := types.Address(ed25519.NewKeyFromSeed(b.Seed).Public().(ed25519.PublicKey))
		if err == nil && addr != own {
			buf = append(buf, addr[:]...)
		}
	}
	return base64.StdEncoding.EncodeToString(append(buf, b.Seed...))
}

// ParseBundle decodes a registry bundle (standard or URL-safe base64, with
// or without padding).
func ParseBundle(s string) (Bundle, error) {
	s = strings.TrimRight(strings.TrimSpace(s), "=")
	raw, err := base64.RawStdEncoding.DecodeString(s)
	if err != nil {
		if raw, err = base64.RawURLEncoding.DecodeString(s); err != nil {
			return Bundle{}, fmt.Errorf("registry bundle: %w", err)
		}
	}
	var b Bundle
	switch len(raw) {
	case 8 + 32:
	case 8 + 32 + 32:
		b.SyncAddress = types.Address(raw[8:40]).String()
	default:
		return Bundle{}, fmt.Errorf("registry bundle: %d bytes, want 40 or 72", len(raw))
	}
	b.AppID = binary.BigEndian.Uint64(raw)
	b.Seed = bytes.Clone(raw[len(raw)-32:])
	if b.AppID == 0 {
		return Bundle{}, errors.New("registry bundle: app id 0")
	}
	return b, nil
}
