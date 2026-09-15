package devfleet

import (
	"context"
	"testing"
	"time"

	"github.com/d13co/algod-loadb-mesh/internal/domain"
)

// TestHeartbeatPropagationLatency guards against missed monitor changes: the
// keepalive period equals the round period so the two often coincide, which
// once caused the round's heartbeat to be skipped until the next keepalive.
func TestHeartbeatPropagationLatency(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f, err := Start(ctx, Options{Nodes: []NodeSpec{{ID: "a", StartRound: 5000, Archival: true}, {ID: "b", StartRound: 5000, OldestRound: 4000}, {ID: "c", StartRound: 5000, OldestRound: 4000, DeveloperAPI: true}},
		KeepAlive: time.Second, SuspectAfter: 10 * time.Second, ProbeInterval: 5 * time.Second, RegistryRefresh: 5 * time.Second, ReturnHysteresis: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	time.Sleep(3 * time.Second)
	var worst time.Duration
	for i := 0; i < 12; i++ {
		f.Advance(1)
		want := f.Nodes[0].Round()
		start := time.Now()
		for {
			ups, _ := f.Agents[0].Directory.Snapshot()
			ok := true
			for _, u := range ups {
				if u.Kind == domain.KindPeer && u.LastRound != want {
					ok = false
				}
			}
			if ok {
				break
			}
			if time.Since(start) > 3*time.Second {
				for _, u := range ups {
					t.Logf("round %d: %s at %d (%s, %s)", want, u.ID, u.LastRound, u.Health, u.Source)
				}
				for j, a := range f.Agents {
					st := a.Monitor.State()
					t.Logf("agent %d monitor: online=%v round=%d seq=%d err=%q", j, st.Online, st.LastRound, st.Seq, st.LastError)
				}
				t.Fatalf("propagation took > 3s at round %d", want)
			}
			time.Sleep(time.Millisecond)
		}
		if d := time.Since(start); d > worst {
			worst = d
		}
		time.Sleep(time.Second)
	}
	t.Logf("worst propagation latency: %v", worst)
	if worst > 200*time.Millisecond {
		t.Fatalf("heartbeats too slow: %v", worst)
	}
}
