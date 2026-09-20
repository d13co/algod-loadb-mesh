package deploy

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/algorand/go-algorand-sdk/v2/types"

	"github.com/d13co/algod-loadb-mesh/internal/config"
)

// The examples must keep up with the config schema: every key known, and
// valid once the placeholders a real host fills in are set.
func TestExamplesLoad(t *testing.T) {
	for name, text := range map[string]string{"config.example.yaml": ConfigExample, "config.static.example.yaml": ConfigStaticExample,
		"config.balancer.example.yaml": ConfigBalancerExample} {
		c, err := config.Decode([]byte(text))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		c.ClientToken = "t" // instead of reading client_token_file
		c.AdminToken = "a"  // instead of reading algod.admin.token from data_dir or admin_token_file
		if c.Registry.SyncKeyFile != "" {
			c.Registry.SyncKey = "00000000000000000000000000000000000000000000000000000000000000ff"
		}
		if c.Registry.Type == "algorand" && c.Registry.AppID == 0 {
			c.Registry.AppID = 1
		}
		if err := c.Finish(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// commentedOption matches a commented-out `key: value` or `- key: value` line.
var commentedOption = regexp.MustCompile(`(?m)^(\s*)# (\s*(?:- )?[a-z_]+:.*)$`)

// The full example documents every option, and its commented options are
// real keys with values of the right type.
func TestExampleListsEveryOption(t *testing.T) {
	all := commentedOption.ReplaceAllString(ConfigExample, "$1$2")
	if _, err := config.Decode([]byte(all)); err != nil {
		t.Fatalf("example with every option uncommented: %v", err)
	}
	// Registry bookkeeping, not something a static entry sets.
	skip := map[string]bool{"version": true, "updatedat": true}
	for _, key := range yamlKeys(reflect.TypeOf(config.Config{})) {
		if !skip[key] && !regexp.MustCompile(`[\s{,]`+key+`:`).MatchString(all) {
			t.Errorf("option %q missing from config.example.yaml", key)
		}
	}
}

func yamlKeys(t reflect.Type) []string {
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Map:
		return yamlKeys(t.Elem())
	case reflect.Struct:
	default:
		return nil
	}
	var keys []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if name == "" {
			name = strings.ToLower(f.Name) // yaml.v3's default
		}
		keys = append(keys, name)
		keys = append(keys, yamlKeys(f.Type)...)
	}
	return keys
}

// autoconfig.sh output loads, with everything it would detect passed in.
func TestAutoconfigLoads(t *testing.T) {
	dir := t.TempDir()
	c := autoconfig(t, dir, "", "--app-id", "7", "--sync-key-file", writeSyncKey(t, dir))
	if c.Local.ID != "k44" || c.Local.AdvertiseEndpoints[0] != "http://10.112.0.44:8080" || c.Mesh.Advertise.String() != "10.112.0.44:4001" || c.Registry.AppID != 7 {
		t.Errorf("unexpected config: %+v", c)
	}
	// The mesh binds what it advertises.
	if c.Mesh.Listen.String() != "10.112.0.44:4001" {
		t.Errorf("mesh.listen = %q", c.Mesh.Listen)
	}
}

// Several --address values: all bound and advertised, the first one hosts
// the algod endpoint.
func TestAutoconfigSeveralAddresses(t *testing.T) {
	dir := t.TempDir()
	c := autoconfigEnv(t, dir, "", nil, "--address", "10.112.0.44", "--address", "10.114.0.44,10.115.0.44", "--app-id", "7", "--sync-key-file", writeSyncKey(t, dir))
	want := "10.112.0.44:4001, 10.114.0.44:4001, 10.115.0.44:4001"
	if c.Mesh.Advertise.String() != want || c.Mesh.Listen.String() != want {
		t.Errorf("mesh listen %q, advertise %q", c.Mesh.Listen, c.Mesh.Advertise)
	}
	if c.Local.AdvertiseEndpoints[0] != "http://10.112.0.44:8080" {
		t.Errorf("endpoints = %v", c.Local.AdvertiseEndpoints)
	}
}

// --nodely adds Nodely for the genesis network; --no-nodely and an unknown
// network add nothing.
func TestAutoconfigNodely(t *testing.T) {
	for _, tc := range []struct {
		genesis, arg, url string
	}{
		{`{"network":"testnet"}`, "--nodely", "https://testnet-api.4160.nodely.dev"},
		{`{"network":"mainnet"}`, "--no-nodely", ""},
		{`{"network":"devnet"}`, "--nodely", ""},
	} {
		bundle := config.Bundle{AppID: 7, Seed: make([]byte, 32)}.String()
		c := autoconfigGenesis(t, t.TempDir(), tc.genesis, "", bundle, tc.arg)
		got := ""
		if len(c.Tiers.External) == 1 {
			got = c.Tiers.External[0].URL
		}
		if got != tc.url || len(c.Tiers.External) > 1 {
			t.Errorf("%s %s: externals %+v, want url %q", tc.genesis, tc.arg, c.Tiers.External, tc.url)
		}
	}
}

// The registry bundle's app id, sync address and key survive the script's
// decoding, whether passed as an argument or on stdin.
func TestAutoconfigBundle(t *testing.T) {
	seed := bytes.Repeat([]byte{0, 9}, 16) // leading zero bytes and a 0x09 (tab) survive
	for _, tc := range []struct{ name, syncAddress string }{
		{"default sender", ""},
		{"rekeyed", types.Address{0, 1, 2, 3, 250}.String()},
	} {
		want := config.Bundle{AppID: 3141592653, SyncAddress: tc.syncAddress, Seed: seed}
		for _, stdin := range []bool{false, true} {
			arg, in := want.String(), ""
			if stdin {
				arg, in = "-", want.String()+"\n"
			}
			c := autoconfig(t, t.TempDir(), in, arg)
			got, err := c.SyncSeed()
			if err != nil {
				t.Fatal(err)
			}
			if c.Registry.AppID != want.AppID || c.Registry.SyncAddress != want.SyncAddress || !bytes.Equal(got, seed) {
				t.Errorf("%s (stdin %v): app %d, address %q, seed %x", tc.name, stdin, c.Registry.AppID, c.Registry.SyncAddress, got)
			}
		}
	}
}

// fakeIP puts an `ip` earlier in PATH that reports the given IPv4 interfaces.
func fakeIP(t *testing.T, dir string, lines ...string) []string {
	t.Helper()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	fake := "#!/usr/bin/env bash\ncase \"$*\" in *-6*) exit 0 ;; esac\ncat <<'OUT'\n" + strings.Join(lines, "\n") + "\nOUT\n"
	if err := os.WriteFile(filepath.Join(bin, "ip"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	return []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH")}
}

// The client listener and the mesh are every WireGuard address the host has,
// so the agent is reachable over the mesh but not on a public interface.
func TestAutoconfigListensOnEveryMeshAddress(t *testing.T) {
	dir := t.TempDir()
	env := fakeIP(t, dir,
		"2: wg0    inet 10.112.0.44/16 brd 10.112.255.255 scope global wg0",
		"3: wg1    inet 10.114.0.44/16 brd 10.114.255.255 scope global wg1",
		"4: eth0    inet 203.0.113.7/24 brd 203.0.113.255 scope global eth0")
	c := autoconfigEnv(t, dir, "", env, "--app-id", "7", "--sync-key-file", writeSyncKey(t, dir))
	if got := c.Listen.String(); got != "10.112.0.44:4000, 10.114.0.44:4000" {
		t.Errorf("listen = %q", got)
	}
	want := "10.112.0.44:4001, 10.114.0.44:4001"
	if c.Mesh.Advertise.String() != want || c.Mesh.Listen.String() != want {
		t.Errorf("mesh listen %q, advertise %q", c.Mesh.Listen, c.Mesh.Advertise)
	}
	if c.Local.AdvertiseEndpoints[0] != "http://10.112.0.44:8080" {
		t.Errorf("endpoints = %v", c.Local.AdvertiseEndpoints)
	}
}

// Without a WireGuard address the fallback is the old shape: gossip on every
// interface, the public address advertised.
func TestAutoconfigPublicFallback(t *testing.T) {
	dir := t.TempDir()
	env := fakeIP(t, dir, "4: eth0    inet 203.0.113.7/24 brd 203.0.113.255 scope global eth0")
	c := autoconfigEnv(t, dir, "", env, "--app-id", "7", "--sync-key-file", writeSyncKey(t, dir))
	if c.Mesh.Listen.String() != "0.0.0.0:4001" || c.Mesh.Advertise.String() != "203.0.113.7:4001" {
		t.Errorf("mesh listen %q, advertise %q", c.Mesh.Listen, c.Mesh.Advertise)
	}
	if c.Listen.String() != "0.0.0.0:4000" {
		t.Errorf("listen = %q", c.Listen)
	}
}

// --listen overrides detection; an address without a port gets --client-port.
func TestAutoconfigListenFlag(t *testing.T) {
	dir := t.TempDir()
	c := autoconfig(t, dir, "", "--app-id", "7", "--sync-key-file", writeSyncKey(t, dir),
		"--listen", "10.112.0.44,10.114.0.44:4444", "--listen", "127.0.0.1", "--client-port", "4100")
	if got := c.Listen.String(); got != "10.112.0.44:4100, 10.114.0.44:4444, 127.0.0.1:4100" {
		t.Errorf("listen = %q", got)
	}
}

func writeSyncKey(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "sync.key")
	if err := os.WriteFile(p, []byte("00000000000000000000000000000000000000000000000000000000000000ff"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// autoconfig runs autoconfig.sh for a fake data dir under dir and loads the
// config it writes.
func autoconfig(t *testing.T, dir, stdin string, args ...string) config.Config {
	t.Helper()
	return autoconfigGenesis(t, dir, "{}", stdin, append([]string{"--no-nodely"}, args...)...)
}

// autoconfigGenesis is autoconfig with the genesis.json content given and no
// Nodely answer, so the caller can pass one.
func autoconfigGenesis(t *testing.T, dir, genesis, stdin string, args ...string) config.Config {
	t.Helper()
	return autoconfigRun(t, dir, genesis, stdin, nil, append([]string{"--address", "10.112.0.44"}, args...)...)
}

// autoconfigEnv is autoconfig with extra environment, and without the address
// override, for the detection this host would do itself.
func autoconfigEnv(t *testing.T, dir, stdin string, env []string, args ...string) config.Config {
	t.Helper()
	return autoconfigRun(t, dir, "{}", stdin, env, append([]string{"--no-nodely"}, args...)...)
}

// autoconfigRun runs autoconfig.sh with exactly the arguments given.
func autoconfigRun(t *testing.T, dir, genesis, stdin string, env []string, args ...string) config.Config {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	dataDir := filepath.Join(dir, "data")
	if err := os.Mkdir(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"data/genesis.json": genesis,
		"data/algod.net":    "0.0.0.0:8080",
		"client.token":      "t",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out := filepath.Join(dir, "config.yaml")
	cmd := exec.Command("bash", append([]string{"autoconfig.sh", "-o", out, "--id", "k44", "--data-dir", dataDir,
		"--client-token-file", filepath.Join(dir, "client.token")}, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Env = append(os.Environ(), env...)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("autoconfig.sh: %v\n%s", err, b)
	}
	c, err := config.Load(out)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
