package main

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/d13co/algod-loadb-mesh/internal/config"
)

// setup.sh takes the first client address off the "listen" line of
// `config check` with this exact sed expression; the passthrough line that
// follows must not change that.
func TestCheckSummaryListenLine(t *testing.T) {
	if _, err := exec.LookPath("sed"); err != nil {
		t.Skip("no sed")
	}
	c := config.Config{Listen: config.Addrs{"10.112.0.44:4000", "10.114.0.44:4000"}}
	c.Local.ID, c.Local.DataDir = "k44", "/var/lib/algorand"
	c.Local.AdvertiseEndpoints = []string{"http://10.112.0.44:8080"}
	c.Local.Passthrough = config.Addrs{"10.114.0.44:8080"}
	c.Registry.Type = "static"
	if err := c.Finish(); err != nil {
		t.Fatal(err)
	}
	out := checkSummary(c)
	cmd := exec.Command("sed", "-n", `s/^listen \([^,]*\).*/\1/p`)
	cmd.Stdin = strings.NewReader(out)
	got, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(got)) != "10.112.0.44:4000" {
		t.Fatalf("sed on %q: %q %v", out, got, err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if lines[1] != "listen 10.112.0.44:4000, 10.114.0.44:4000, advertising http://10.112.0.44:8080" || lines[2] != "passthrough 10.114.0.44:8080" {
		t.Fatalf("lines: %q", lines)
	}
	c.Local.Passthrough = nil
	if !strings.Contains(checkSummary(c), "\npassthrough none\n") {
		t.Fatalf("no passthrough: %q", checkSummary(c))
	}
}
