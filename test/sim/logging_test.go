package sim

import (
	"bufio"
	"bytes"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/d13co/algod-loadb-mesh/internal/adapters/logging"
	"github.com/d13co/algod-loadb-mesh/internal/devfleet"
)

// lockedBuffer lets the test read log output while agents write it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) entries(msg string) []map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(b.buf.Bytes()))
	for sc.Scan() {
		var e map[string]any
		if json.Unmarshal(sc.Bytes(), &e) == nil && e["msg"] == msg {
			out = append(out, e)
		}
	}
	return out
}

func TestRequestAndGossipLogging(t *testing.T) {
	var logs lockedBuffer
	f := start(t, devfleet.Options{Nodes: threeNodes(), Mode: "fallback", Log: logging.New(&logs, slog.LevelDebug, true)})
	plain := f.Servers[1].URL

	call(t, "GET", plain+"/v2/blocks/100", "", "")
	call(t, "GET", plain+"/loadb/health", "", "")

	var block, health map[string]any
	waitFor(t, 2*time.Second, "request log lines", func() bool {
		for _, e := range logs.entries("request") {
			switch e["path"] {
			case "/v2/blocks/100":
				block = e
			case "/loadb/health":
				health = e
			}
		}
		return block != nil && health != nil
	})
	if block["level"] != "INFO" || block["method"] != "GET" || block["status"] != float64(200) || block["upstream"] != "arch" ||
		block["ip"] != "127.0.0.1" || block["class"] == nil || block["bytes"].(float64) <= 0 {
		t.Errorf("request log: %v", block)
	}
	if health["level"] != "DEBUG" {
		t.Errorf("health probes log at DEBUG: %v", health)
	}

	var accepted map[string]any
	for _, e := range logs.entries("udp received") {
		if e["level"] == "DEBUG" && e["result"] == "accepted" && e["id"] != nil && e["from"] != nil && e["seq"] != nil {
			accepted = e
			break
		}
	}
	if accepted == nil {
		t.Error("no DEBUG log line for an accepted heartbeat")
	}
}
