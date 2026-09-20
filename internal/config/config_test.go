package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	if c.Mode != "fallback" || c.Listen.String() != "127.0.0.1:4000" || c.Registry.Refresh.Minutes() != 5 || *c.Routing.RetryBudget != 1 {
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

func TestListenAddresses(t *testing.T) {
	base := "local: {id: a, data_dir: /x}\nregistry: {type: memory, auto_register: false}\n"
	for _, tc := range []struct{ name, listen, want string }{
		{"one address", "listen: 10.112.0.44:4000\n", "10.112.0.44:4000"},
		{"comma separated", "listen: 10.112.0.44:4000, 10.114.0.44:4000\n", "10.112.0.44:4000, 10.114.0.44:4000"},
		{"flow list", "listen: [10.112.0.44:4000, 10.114.0.44:4000]\n", "10.112.0.44:4000, 10.114.0.44:4000"},
		{"block list", "listen:\n  - 10.112.0.44:4000\n  - \"[fd00::1]:4000\"\n", "10.112.0.44:4000, [fd00::1]:4000"},
		{"duplicates collapse", "listen: [10.112.0.44:4000, 10.112.0.44:4000]\n", "10.112.0.44:4000"},
		{"empty list defaults", "listen: []\n", "127.0.0.1:4000"},
		{"ipv4 wildcard beside an ipv6 address", "listen: [0.0.0.0:4000, \"[fd00::1]:4000\"]\n", "0.0.0.0:4000, [fd00::1]:4000"},
	} {
		c, err := load(t, tc.listen+base)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if c.Listen.String() != tc.want {
			t.Errorf("%s: listen %q, want %q", tc.name, c.Listen, tc.want)
		}
		// Finish runs again on the agent's copy of the config.
		if err := c.Finish(); err != nil || c.Listen.String() != tc.want {
			t.Errorf("%s: Finish must be idempotent: %v %q", tc.name, err, c.Listen)
		}
	}
	for _, tc := range []struct{ name, listen, want string }{
		{"no port", "listen: 10.112.0.44\n", "port"},
		{"wildcard covers an address", "listen: [0.0.0.0:4000, 10.112.0.44:4000]\n", "every interface"},
		{"address covered by a later wildcard", "listen: [10.112.0.44:4000, 0.0.0.0:4000]\n", "every interface"},
		{"two wildcards", "listen: [0.0.0.0:4000, \"[::]:4000\"]\n", "every interface"},
		{"dual-stack wildcard covers an ipv6 address", "listen: [\"[::]:4000\", \"[fd00::1]:4000\"]\n", "every interface"},
		{"bare port covers an ipv6 address", "listen: [\":4000\", \"[fd00::1]:4000\"]\n", "every interface"},
	} {
		_, err := load(t, tc.listen+base)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want an error mentioning %q, got %v", tc.name, tc.want, err)
		}
	}
	// Different ports on the same wildcard are fine.
	if c, err := load(t, "listen: [0.0.0.0:4000, 0.0.0.0:4002]\n"+base); err != nil || len(c.Listen) != 2 {
		t.Errorf("two wildcard ports: %v %q", err, c.Listen)
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

func TestURLsWithoutSchemeGetHTTP(t *testing.T) {
	c, err := load(t, `local:
  id: a
  data_dir: /x
  advertise_endpoints: [157.173.109.122:51088, "https://k48.example:443", " 10.0.0.1:8080/ "]
registry:
  type: static
  algod_url: 127.0.0.1:8080
  static:
    - {id: b, network: n, endpoints: [10.112.0.27:54000]}
tiers:
  external:
    - {name: nodely, url: mainnet-api.4160.nodely.dev}
`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"http://157.173.109.122:51088", "https://k48.example:443", "http://10.0.0.1:8080/"}
	for i, u := range c.Local.AdvertiseEndpoints {
		if u != want[i] {
			t.Errorf("advertise_endpoints[%d] = %q, want %q", i, u, want[i])
		}
	}
	if c.Registry.AlgodURL != "http://127.0.0.1:8080" || c.Registry.Static[0].Endpoints[0] != "http://10.112.0.27:54000" ||
		c.Tiers.External[0].URL != "http://mainnet-api.4160.nodely.dev" {
		t.Errorf("got %q %q %q", c.Registry.AlgodURL, c.Registry.Static[0].Endpoints[0], c.Tiers.External[0].URL)
	}
	if err := c.Finish(); err != nil || c.Local.AdvertiseEndpoints[0] != "http://157.173.109.122:51088" {
		t.Errorf("Finish must be idempotent: %v %q", err, c.Local.AdvertiseEndpoints[0])
	}
}

func TestAdminToken(t *testing.T) {
	dir := t.TempDir()
	cfg := "local: {id: a, data_dir: " + dir + "}\nregistry: {type: static}\n"

	c, err := load(t, cfg)
	if err != nil || c.AdminToken != "" {
		t.Fatalf("no algod.admin.token: %q %v", c.AdminToken, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "algod.admin.token"), []byte("algod-admin\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err = load(t, cfg); err != nil || c.AdminToken != "algod-admin" {
		t.Fatalf("default from data dir: %q %v", c.AdminToken, err)
	}
	own := filepath.Join(t.TempDir(), "admin")
	if err := os.WriteFile(own, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err = load(t, cfg+"admin_token_file: "+own+"\n"); err != nil || c.AdminToken != "mine" {
		t.Fatalf("admin_token_file: %q %v", c.AdminToken, err)
	}
	if c, err = load(t, cfg+"admin_token: inline\nadmin_token_file: "+own+"\n"); err != nil || c.AdminToken != "inline" {
		t.Fatalf("admin_token wins: %q %v", c.AdminToken, err)
	}
	if _, err = load(t, cfg+"admin_token_file: "+own+".missing\n"); err == nil || !strings.Contains(err.Error(), "admin_token_file") {
		t.Fatalf("an explicit missing file must fail: %v", err)
	}
	// Unreadable default file: fail rather than silently leaving /loadb closed.
	if os.Getuid() != 0 {
		if err := os.Chmod(filepath.Join(dir, "algod.admin.token"), 0); err != nil {
			t.Fatal(err)
		}
		if _, err = load(t, cfg); err == nil {
			t.Fatal("unreadable algod.admin.token must fail")
		}
	}
}

func TestBalancerRole(t *testing.T) {
	const base = "role: balancer\nlocal: {id: lb1, network: mainnet-v1.0}\nmesh: {advertise: 10.112.0.9:4001}\n"
	c, err := load(t, base+"registry: {type: memory}\n")
	if err != nil {
		t.Fatal(err)
	}
	if !c.Balancer() || c.Mode != "loadbalancer" || c.AdminToken != "" || !*c.Registry.AutoRegister {
		t.Fatalf("balancer defaults: %+v", c)
	}
	if c, err = load(t, base+"mode: fallback\nregistry: {type: memory}\n"); err != nil || c.Mode != "fallback" {
		t.Fatalf("explicit mode is kept: %q %v", c.Mode, err)
	}
	if _, err := load(t, "role: balancer\nlocal: {id: lb1}\nregistry: {type: memory, auto_register: false}\n"); err == nil || !strings.Contains(err.Error(), "local.network") {
		t.Fatalf("balancer needs a network: %v", err)
	}
	if _, err := load(t, base+"registry: {app_id: 5, sync_key: k}\n"); err == nil || !strings.Contains(err.Error(), "algod_url") {
		t.Fatalf("on-chain registry needs algod_url on a balancer: %v", err)
	}
	if _, err := load(t, base+"registry: {app_id: 5, sync_key: k, algod_url: 10.112.0.44:8080}\n"); err != nil {
		t.Fatalf("balancer with algod_url: %v", err)
	}
	if _, err := load(t, "role: balancer\nlocal: {id: lb1, network: n}\nregistry: {type: memory}\n"); err == nil || !strings.Contains(err.Error(), "mesh.advertise") {
		t.Fatalf("a registering balancer needs mesh.advertise: %v", err)
	}
	if _, err := load(t, "local: {id: a, data_dir: /x, network: n}\nregistry: {type: static}\n"); err == nil || !strings.Contains(err.Error(), "role balancer") {
		t.Fatalf("local.network on a node must fail: %v", err)
	}
	if _, err := load(t, "role: router\nlocal: {id: a, data_dir: /x}\n"); err == nil {
		t.Fatal("unknown role must fail")
	}
}

func TestMeshAddresses(t *testing.T) {
	base := "local: {id: a, data_dir: /x}\nregistry: {type: memory, auto_register: false}\n"
	c, err := load(t, base+"mesh:\n  listen: 10.112.0.1:4001, 10.114.0.1:4001\n  advertise: [10.112.0.1:4001, 10.114.0.1:4001, 10.112.0.1:4001]\n")
	if err != nil {
		t.Fatal(err)
	}
	if c.Mesh.Listen.String() != "10.112.0.1:4001, 10.114.0.1:4001" || c.Mesh.Advertise.String() != "10.112.0.1:4001, 10.114.0.1:4001" {
		t.Fatalf("mesh lists: %q / %q", c.Mesh.Listen, c.Mesh.Advertise)
	}
	if c.Mesh.PathProbeInterval != 30*time.Second || c.Mesh.PathTimeout != 2*time.Second {
		t.Fatalf("path defaults: %v %v", c.Mesh.PathProbeInterval, c.Mesh.PathTimeout)
	}
	if len(c.Warnings()) != 0 {
		t.Fatalf("private mesh addresses must not warn: %v", c.Warnings())
	}
	c, err = load(t, base)
	if err != nil || c.Mesh.Listen.String() != "0.0.0.0:4001" {
		t.Fatalf("default mesh.listen: %q %v", c.Mesh.Listen, err)
	}
	if w := c.Warnings(); len(w) != 1 || !strings.Contains(w[0], "every interface") {
		t.Fatalf("a wildcard mesh.listen warns: %v", w)
	}
	c, err = load(t, base+"mesh: {listen: 10.112.0.1:4001, advertise: 203.0.113.7:4001}\n")
	if err != nil {
		t.Fatal(err)
	}
	if w := c.Warnings(); len(w) != 1 || !strings.Contains(w[0], "public") {
		t.Fatalf("a public mesh.advertise warns: %v", w)
	}
	c, err = load(t, base+"mesh: {listen: 10.112.0.1:4001, suspect_after: 10s}\n")
	if err != nil {
		t.Fatal(err)
	}
	if w := c.Warnings(); len(w) != 1 || !strings.Contains(w[0], "three keepalives") {
		t.Fatalf("suspect_after under three keepalives warns: %v", w)
	}
	for _, bad := range []string{"mesh: {advertise: 0.0.0.0:4001}\n", "mesh: {advertise: 10.112.0.1}\n", "mesh: {listen: [0.0.0.0:4001, 10.112.0.1:4001]}\n"} {
		if _, err := load(t, base+bad); err == nil {
			t.Errorf("%q must fail", bad)
		}
	}
	if _, err := load(t, "local: {id: a, data_dir: /x, advertise_endpoints: [http://x]}\nregistry: {type: memory}\nmesh: {advertise: [' ']}\n"); err == nil || !strings.Contains(err.Error(), "mesh.advertise") {
		t.Fatalf("auto_register with no advertise address: %v", err)
	}
}
