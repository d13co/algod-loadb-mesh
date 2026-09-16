package config

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/algorand/go-algorand-sdk/v2/types"
)

func TestBundleRoundTrip(t *testing.T) {
	seed := bytes.Repeat([]byte{7}, 32)
	own := types.Address(ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)).String()
	other := types.Address{1, 2, 3}.String()
	for _, tc := range []struct {
		name, addr, want string
		size             int
	}{
		{"default sender", "", "", 40},
		{"sync_address is the key's own", own, "", 40},
		{"rekeyed", other, other, 72},
	} {
		s := Bundle{AppID: 3141592653, SyncAddress: tc.addr, Seed: seed}.String()
		if n := base64.StdEncoding.DecodedLen(len(s)) - strings.Count(s, "="); n != tc.size {
			t.Errorf("%s: %d bytes, want %d", tc.name, n, tc.size)
		}
		b, err := ParseBundle(s)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if b.AppID != 3141592653 || b.SyncAddress != tc.want || !bytes.Equal(b.Seed, seed) {
			t.Errorf("%s: got %+v", tc.name, b)
		}
	}
	for _, bad := range []string{"", "AAAA", Bundle{Seed: seed}.String(), "not base64!"} {
		if _, err := ParseBundle(bad); err == nil {
			t.Errorf("ParseBundle(%q) accepted", bad)
		}
	}
}
