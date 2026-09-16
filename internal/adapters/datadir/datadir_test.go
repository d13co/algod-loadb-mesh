package datadir

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadDataDir(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("algod.net", "0.0.0.0:8080\n")
	write("algod.token", "abc\n")
	write("genesis.json", `{"network":"mainnet","id":"v1.0"}`)
	nc, err := Reader{Dir: dir}.Read()
	if err != nil {
		t.Fatal(err)
	}
	if nc.Endpoint != "http://127.0.0.1:8080" || nc.Token != "abc" || nc.GenesisID != "mainnet-v1.0" {
		t.Fatalf("%+v", nc)
	}
	if nc.Archival || nc.MaxBlockHistoryLookback != 0 || nc.MaxAcctLookback != 8 || nc.RestWriteTimeoutSeconds != 120 {
		t.Fatalf("defaults: %+v", nc)
	}
	write("config.json", `{"Version": 34, "MaxBlockHistoryLookback": 3500000, "EnableDeveloperAPI": true, "EnableFollowMode": false, "UnknownKey": 1}`)
	nc, err = Reader{Dir: dir}.Read()
	if err != nil {
		t.Fatal(err)
	}
	if nc.MaxBlockHistoryLookback != 3500000 || !nc.EnableDeveloperAPI || nc.ModTime.IsZero() {
		t.Fatalf("%+v", nc)
	}
	if _, err := (Reader{Dir: filepath.Join(dir, "missing")}).Read(); err == nil {
		t.Fatal("missing dir must fail")
	}
}

func TestEndpointURL(t *testing.T) {
	cases := map[string]string{
		"0.0.0.0:8080":   "http://127.0.0.1:8080",
		"127.0.0.1:8080": "http://127.0.0.1:8080",
		"10.1.2.3:51616": "http://10.1.2.3:51616",
		"[::]:8080":      "http://127.0.0.1:8080",
		"http://x:1":     "http://x:1",
	}
	for in, want := range cases {
		if got := endpointURL(in); got != want {
			t.Errorf("%s: got %s want %s", in, got, want)
		}
	}
}
