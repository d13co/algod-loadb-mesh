package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func load(t *testing.T, yaml string) (Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

func TestLoadDefaultsAndValidation(t *testing.T) {
	c, err := load(t, "local:\n  id: k44\n  data_dir: /tmp/x\nregistry:\n  type: memory\n  auto_register: false\n")
	if err != nil {
		t.Fatal(err)
	}
	if c.Mode != "fallback" || c.Listen != "127.0.0.1:4000" || c.Registry.Refresh.Minutes() != 5 || *c.Routing.RetryBudget != 1 {
		t.Fatalf("defaults: %+v", c)
	}
	if _, err := load(t, "local:\n  data_dir: /tmp/x\nregistry: {app_id: 5}\n"); err == nil || !strings.Contains(err.Error(), "sync_key") {
		t.Fatalf("algorand registry needs a sync key: %v", err)
	}
	if _, err := load(t, "local:\n  data_dir: /tmp/x\nregistry: {app_id: 5, sync_key: k, sync_address: nope, auto_register: false}\n"); err == nil || !strings.Contains(err.Error(), "sync_address") {
		t.Fatalf("bad sync_address must fail: %v", err)
	}
	if _, err := load(t, "mode: weird\nlocal: {id: a, data_dir: /x}\n"); err == nil {
		t.Fatal("bad mode must fail")
	}
	if _, err := load(t, "local: {id: a, data_dir: /x}\nunknown_key: 1\n"); err == nil {
		t.Fatal("unknown keys must fail")
	}
	// auto_register needs advertise settings.
	_, err = load(t, "local: {id: a, data_dir: /x}\nregistry: {type: memory}\nmesh: {}\n")
	if err == nil || !strings.Contains(err.Error(), "advertise") {
		t.Fatalf("expected advertise error, got %v", err)
	}
}

func TestSyncSeedFormats(t *testing.T) {
	c := Config{}
	c.Registry.SyncKey = strings.Repeat("00", 32)
	seed, err := c.SyncSeed()
	if err != nil || len(seed) != 32 {
		t.Fatalf("hex: %v", err)
	}
	c.Registry.SyncKey = "not a key"
	if _, err := c.SyncSeed(); err == nil {
		t.Fatal("garbage must fail")
	}
	c.Registry.SyncKey = ""
	c.Mesh.SharedSecret = "s"
	m, err := c.KeyMaterial()
	if err != nil || len(m) != 32 {
		t.Fatalf("shared secret material: %v", err)
	}
}
