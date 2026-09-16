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
	if err := os.WriteFile(filepath.Join(dir, "sync.key"), []byte("00000000000000000000000000000000000000000000000000000000000000ff"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := autoconfig(t, dir, "", "--app-id", "7", "--sync-key-file", filepath.Join(dir, "sync.key"))
	if c.Local.ID != "k44" || c.Local.AdvertiseEndpoints[0] != "http://10.112.0.44:8080" || c.Mesh.Advertise != "10.112.0.44:4001" || c.Registry.AppID != 7 {
		t.Errorf("unexpected config: %+v", c)
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

// autoconfig runs autoconfig.sh for a fake data dir under dir and loads the
// config it writes.
func autoconfig(t *testing.T, dir, stdin string, args ...string) config.Config {
	t.Helper()
	return autoconfigGenesis(t, dir, "{}", stdin, append([]string{"--no-nodely"}, args...)...)
}

// autoconfigGenesis is autoconfig with the genesis.json content given.
func autoconfigGenesis(t *testing.T, dir, genesis, stdin string, args ...string) config.Config {
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
		"--address", "10.112.0.44", "--client-token-file", filepath.Join(dir, "client.token")}, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("autoconfig.sh: %v\n%s", err, b)
	}
	c, err := config.Load(out)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
