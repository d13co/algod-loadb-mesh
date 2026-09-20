package app

import (
	"context"
	"crypto/ed25519"
	crand "crypto/rand"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/d13co/algod-loadb-mesh/internal/adapters/clock"
	"github.com/d13co/algod-loadb-mesh/internal/adapters/logging"
	"github.com/d13co/algod-loadb-mesh/internal/adapters/metrics"
	"github.com/d13co/algod-loadb-mesh/internal/domain"
	"github.com/d13co/algod-loadb-mesh/internal/ports"
)

// fakeGossip records every Send and lets the test inject datagrams.
type fakeGossip struct {
	mu         sync.Mutex
	sent       []sentMsg
	unroutable map[string]bool
	hooks      map[string]func(domain.Envelope) error // called instead of delivering; its error is Send's
	recv       chan ports.GossipMessage
}

type sentMsg struct {
	addr string
	env  domain.Envelope
}

func newFakeGossip() *fakeGossip {
	return &fakeGossip{unroutable: map[string]bool{}, hooks: map[string]func(domain.Envelope) error{}, recv: make(chan ports.GossipMessage, 64)}
}

func (g *fakeGossip) Send(_ context.Context, addr string, payload []byte) error {
	g.mu.Lock()
	if g.unroutable[addr] {
		g.mu.Unlock()
		return ports.ErrNoRoute
	}
	env, err := domain.DecodeMessage(payload)
	if err != nil {
		panic(err)
	}
	if hook := g.hooks[addr]; hook != nil {
		g.mu.Unlock() // the hook may inject and wait on the directory
		return hook(env)
	}
	g.sent = append(g.sent, sentMsg{addr, env})
	g.mu.Unlock()
	return nil
}

// hook makes every Send to addr call fn instead of delivering.
func (g *fakeGossip) hook(addr string, fn func(domain.Envelope) error) {
	g.mu.Lock()
	g.hooks[addr] = fn
	g.mu.Unlock()
}

func (g *fakeGossip) Receive() <-chan ports.GossipMessage { return g.recv }
func (g *fakeGossip) Close() error                        { return nil }

func (g *fakeGossip) route(addr string, ok bool) {
	g.mu.Lock()
	g.unroutable[addr] = !ok
	g.mu.Unlock()
}

// count returns how many sends matched.
func (g *fakeGossip) count(match func(sentMsg) bool) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, m := range g.sent {
		if match(m) {
			n++
		}
	}
	return n
}

func heartbeatsTo(addr string) func(sentMsg) bool {
	return func(m sentMsg) bool { return m.addr == addr && m.env.HB != nil }
}

func pongsTo(addr string) func(sentMsg) bool {
	return func(m sentMsg) bool {
		return m.addr == addr && m.env.Probe != nil && m.env.Probe.Type == domain.ProbePong
	}
}

// harness is one directory under test with a fake clock and gossip.
type harness struct {
	t       *testing.T
	d       *Directory
	g       *fakeGossip
	fc      *clock.Fake
	m       *metrics.Registry
	peerKey ed25519.PrivateKey
	peerPub ed25519.PublicKey
	opts    DirectoryOptions
	answers map[string]bool // addresses whose pings the fake peer answers
	pinged  int             // sends already scanned for pings
}

const (
	addrA = "10.0.0.1:4001"
	addrB = "10.0.1.1:4001"
)

func newHarness(t *testing.T, o DirectoryOptions) *harness {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(crand.Reader)
	peerPub, peerKey, _ := ed25519.GenerateKey(crand.Reader)
	fc := clock.NewFake(time.Unix(1000, 0))
	m := metrics.New()
	g := newFakeGossip()
	if o.LocalID == "" {
		o.LocalID = "me"
	}
	if o.KeepAlive == 0 {
		o.KeepAlive = time.Second
	}
	if o.SuspectAfter == 0 {
		o.SuspectAfter = 3 * time.Second
	}
	if o.PathProbeInterval == 0 {
		o.PathProbeInterval = 10 * time.Second
	}
	if o.PathTimeout == 0 {
		o.PathTimeout = time.Second
	}
	mon := NewMonitor(MonitorOptions{NodeID: o.LocalID, Absent: true, Network: "n"}, nil, nil, fc, logging.Nop{}, m)
	d := NewDirectory(o, mon, g, nil, NewStatsBook(BreakerOptions{Threshold: 5, OpenFor: time.Second, Window: 10, Alpha: 0.2}, fc), fc, logging.Nop{}, m, priv)
	h := &harness{t: t, d: d, g: g, fc: fc, m: m, peerKey: peerKey, peerPub: peerPub, opts: o, answers: map[string]bool{}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go d.Run(ctx)
	time.Sleep(30 * time.Millisecond) // the loops must register their tickers before the clock moves
	return h
}

func (h *harness) peerRecord(addrs ...string) domain.NodeRecord {
	return domain.NodeRecord{ID: "b", Network: "n", Agent: domain.AgentInfo{Addrs: addrs, PubKey: []byte(h.peerPub)}}
}

// inject delivers a datagram signed by the peer, as if from addr.
func (h *harness) inject(from string, wire []byte) {
	h.g.recv <- ports.GossipMessage{From: from, Payload: wire}
}

func (h *harness) injectHeartbeat(from string, seq uint64) {
	wire, _ := domain.EncodeHeartbeat(h.peerKey, domain.Heartbeat{NodeID: "b", Seq: seq, Online: true, LastRound: 10})
	h.inject(from, wire)
}

func (h *harness) injectProbe(from string, pr domain.Probe) {
	pr.NodeID = "b"
	wire, _ := domain.EncodeProbe(h.peerKey, pr)
	h.inject(from, wire)
}

// answer replies to every unanswered ping sent to an address in answers,
// after rtt of fake time, and waits for the pongs to be processed.
func (h *harness) answer(rtt map[string]time.Duration) {
	h.t.Helper()
	h.g.mu.Lock()
	var pending []sentMsg
	for _, m := range h.g.sent[h.pinged:] {
		if m.env.Probe != nil && m.env.Probe.Type == domain.ProbePing && h.answers[m.addr] {
			pending = append(pending, m)
		}
	}
	h.pinged = len(h.g.sent)
	h.g.mu.Unlock()
	// All pending pings left on the same tick: answer in RTT order, moving
	// the clock by the difference each time.
	sort.SliceStable(pending, func(i, j int) bool { return rtt[pending[i].addr] < rtt[pending[j].addr] })
	var elapsed time.Duration
	for _, m := range pending {
		if d := rtt[m.addr]; d > elapsed {
			h.fc.Advance(d - elapsed)
			elapsed = d
		}
		h.injectProbe(m.addr, domain.Probe{Type: domain.ProbePong, Nonce: m.env.Probe.Nonce, HeardMS: 100})
		time.Sleep(5 * time.Millisecond) // processed before the clock moves again
	}
	time.Sleep(10 * time.Millisecond)
}

// tick advances one maintenance tick and lets the loops run.
func (h *harness) tick() {
	h.fc.Advance(time.Second)
	time.Sleep(15 * time.Millisecond)
}

func (h *harness) link(id string) LinkStatus {
	h.t.Helper()
	for _, l := range h.d.LinkSnapshot() {
		if l.Peer == id {
			return l
		}
	}
	h.t.Fatalf("no link %s", id)
	return LinkStatus{}
}

// lastPong is the most recent pong sent to addr.
func (h *harness) lastPong(addr string) *domain.Probe {
	h.t.Helper()
	h.g.mu.Lock()
	defer h.g.mu.Unlock()
	for i := len(h.g.sent) - 1; i >= 0; i-- {
		if m := h.g.sent[i]; pongsTo(addr)(m) {
			return m.env.Probe
		}
	}
	h.t.Fatalf("no pong to %s", addr)
	return nil
}

func (h *harness) metricsText() string {
	var b strings.Builder
	_, _ = h.m.WriteTo(&b)
	return b.String()
}

func TestHeartbeatsGoToTheChosenPathOnly(t *testing.T) {
	h := newHarness(t, DirectoryOptions{})
	h.d.SetRecords([]domain.NodeRecord{h.peerRecord(addrA, addrB), {ID: "lb", Role: domain.RoleBalancer, Network: "n",
		Agent: domain.AgentInfo{Addrs: []string{"10.0.0.9:4001"}, PubKey: []byte(h.peerPub)}}})
	if l := h.link("b"); l.Path != addrA {
		t.Fatalf("registry order wins with nothing measured: %+v", l)
	}
	h.tick()
	// One at the initial choice (a new link is a switch), one at the keepalive.
	if n := h.g.count(heartbeatsTo(addrA)); n != 2 {
		t.Fatalf("heartbeats to %s: %d", addrA, n)
	}
	if n := h.g.count(heartbeatsTo(addrB)); n != 0 {
		t.Fatalf("heartbeats to the other path: %d", n)
	}
	// Balancers are ordinary heartbeat destinations on a node.
	if n := h.g.count(heartbeatsTo("10.0.0.9:4001")); n != 2 {
		t.Fatalf("heartbeats to the balancer: %d", n)
	}
	// The first tick pinged every path of every link.
	if n := h.g.count(func(m sentMsg) bool { return m.env.Probe != nil && m.env.Probe.Type == domain.ProbePing }); n != 3 {
		t.Fatalf("pings on the first tick: %d", n)
	}
}

func TestPingIsAnsweredWithNonceAndHeardMS(t *testing.T) {
	h := newHarness(t, DirectoryOptions{})
	h.d.SetRecords([]domain.NodeRecord{h.peerRecord(addrA, addrB)})
	h.injectProbe(addrB, domain.Probe{Type: domain.ProbePing, Nonce: 42, HeardMS: -1})
	waitFor(t, time.Second, func() bool { return h.g.count(pongsTo(addrB)) == 1 })
	if pong := h.lastPong(addrB); pong.Nonce != 42 || pong.HeardMS != -1 || pong.NodeID != "me" {
		t.Fatalf("pong: %+v", pong)
	}
	// The ping refreshed the inbound side of path B only.
	if l := h.link("b"); l.Paths[1].LastSeenAge < 0 || l.Paths[0].LastSeenAge != -1 {
		t.Fatalf("last_seen_age_s after a ping from B: %+v", l)
	}
	h.injectHeartbeat(addrA, 1)
	time.Sleep(10 * time.Millisecond)
	h.fc.Advance(2500 * time.Millisecond)
	h.injectProbe(addrB, domain.Probe{Type: domain.ProbePing, Nonce: 43, HeardMS: -1})
	waitFor(t, time.Second, func() bool { return h.g.count(pongsTo(addrB)) == 2 })
	if pong := h.lastPong(addrB); pong.Nonce != 43 || pong.HeardMS != 2500 {
		t.Fatalf("pong after a heartbeat: %+v", pong)
	}
	// Unknown ids and wrong keys get nothing.
	_, other, _ := ed25519.GenerateKey(crand.Reader)
	wire, _ := domain.EncodeProbe(other, domain.Probe{Type: domain.ProbePing, NodeID: "b", Nonce: 44})
	h.inject(addrB, wire)
	time.Sleep(20 * time.Millisecond)
	if n := h.g.count(pongsTo(addrB)); n != 2 {
		t.Fatalf("a ping under the wrong key was answered: %d pongs", n)
	}
}

func TestDeadPathIsLeftAfterLosses(t *testing.T) {
	h := newHarness(t, DirectoryOptions{})
	h.d.SetRecords([]domain.NodeRecord{h.peerRecord(addrA, addrB)})
	h.answers[addrB] = true
	// Only B answers: A loses one ping per 2*PathTimeout until it is dead.
	for i := 0; i < 2*domain.PathFailures+2; i++ {
		h.tick()
		h.answer(map[string]time.Duration{addrB: 5 * time.Millisecond})
		if h.link("b").Path == addrB {
			break
		}
	}
	l := h.link("b")
	if l.Path != addrB || l.Paths[0].Alive || l.Paths[0].Losses < domain.PathFailures || !l.Paths[1].Alive || l.Paths[1].RTTMS == 0 {
		t.Fatalf("after losses: %+v", l)
	}
	if !strings.Contains(h.metricsText(), `loadb_path_switches_total{reason="dead"} 1`) {
		t.Fatalf("metrics:\n%s", h.metricsText())
	}
	// The switch nudged a heartbeat onto the new path without waiting for
	// the keepalive.
	if n := h.g.count(heartbeatsTo(addrB)); n == 0 {
		t.Fatal("no heartbeat on the new path")
	}
}

func TestFasterPathWinsOnlyByMarginAndAfterCooldown(t *testing.T) {
	h := newHarness(t, DirectoryOptions{})
	h.d.SetRecords([]domain.NodeRecord{h.peerRecord(addrA, addrB)})
	h.answers[addrA], h.answers[addrB] = true, true
	// 10% faster never wins.
	run := func(rtt map[string]time.Duration, seconds int) {
		for i := 0; i < seconds; i++ {
			h.tick()
			h.answer(rtt)
		}
	}
	run(map[string]time.Duration{addrA: 10 * time.Millisecond, addrB: 9 * time.Millisecond}, 3*int(h.opts.PathProbeInterval/time.Second))
	if l := h.link("b"); l.Path != addrA {
		t.Fatalf("10%% faster must not win: %+v", l)
	}
	if strings.Contains(h.metricsText(), `reason="faster"`) {
		t.Fatalf("no switch expected:\n%s", h.metricsText())
	}
	// 3x faster wins, but only once two consecutive probes agree.
	h2 := newHarness(t, DirectoryOptions{})
	h2.d.SetRecords([]domain.NodeRecord{h2.peerRecord(addrA, addrB)})
	h2.answers[addrA], h2.answers[addrB] = true, true
	fast := map[string]time.Duration{addrA: 30 * time.Millisecond, addrB: 10 * time.Millisecond}
	for i := 0; i < int(h2.opts.PathProbeInterval/time.Second)+2; i++ {
		h2.tick()
		h2.answer(fast)
	}
	if l := h2.link("b"); l.Path != addrA {
		t.Fatalf("one faster probe must not switch: %+v", l)
	}
	for i := 0; i < int(h2.opts.PathProbeInterval/time.Second)+2; i++ {
		h2.tick()
		h2.answer(fast)
	}
	if l := h2.link("b"); l.Path != addrB {
		t.Fatalf("two faster probes must switch: %+v", l)
	}
	if !strings.Contains(h2.metricsText(), `loadb_path_switches_total{reason="faster"} 1`) {
		t.Fatalf("metrics:\n%s", h2.metricsText())
	}
}

// The asymmetric case: the peer's ping says it has not heard us, and that
// alone moves the heartbeats without waiting for a loss.
func TestStaleHeardMSMarksPathDeaf(t *testing.T) {
	h := newHarness(t, DirectoryOptions{})
	h.d.SetRecords([]domain.NodeRecord{h.peerRecord(addrA, addrB)})
	h.answers[addrA], h.answers[addrB] = true, true
	for i := 0; i < 4; i++ { // past 3*KeepAlive on the current path
		h.tick()
		h.answer(map[string]time.Duration{addrA: time.Millisecond, addrB: time.Millisecond})
	}
	before := h.g.count(heartbeatsTo(addrB))
	// A negative HeardMS is a restarted peer, not a deaf path.
	h.injectProbe(addrB, domain.Probe{Type: domain.ProbePing, Nonce: 1, HeardMS: -1})
	waitFor(t, time.Second, func() bool { return h.g.count(pongsTo(addrB)) == 1 })
	if l := h.link("b"); l.Path != addrA {
		t.Fatalf("HeardMS -1 must not switch: %+v", l)
	}
	// Two missed keepalives are ordinary loss, not deafness.
	h.injectProbe(addrB, domain.Probe{Type: domain.ProbePing, Nonce: 3, HeardMS: 2500})
	waitFor(t, time.Second, func() bool { return h.g.count(pongsTo(addrB)) == 2 })
	if l := h.link("b"); l.Path != addrA || l.Paths[0].Deaf {
		t.Fatalf("HeardMS under 3*KeepAlive must not switch: %+v", l)
	}
	h.injectProbe(addrB, domain.Probe{Type: domain.ProbePing, Nonce: 2, HeardMS: 5000})
	waitFor(t, time.Second, func() bool { return h.link("b").Path == addrB })
	l := h.link("b")
	if !l.Paths[0].Deaf || l.Paths[0].Alive {
		t.Fatalf("path A must be deaf: %+v", l)
	}
	if !strings.Contains(h.metricsText(), `loadb_path_switches_total{reason="deaf"} 1`) {
		t.Fatalf("metrics:\n%s", h.metricsText())
	}
	// Switching sent a heartbeat at once, with no clock movement.
	waitFor(t, time.Second, func() bool { return h.g.count(heartbeatsTo(addrB)) > before })
	// A pong on the deaf path clears the flag; the choice stays.
	for i := 0; i < int(h.opts.PathProbeInterval/time.Second)+2; i++ {
		h.tick()
		h.answer(map[string]time.Duration{addrA: time.Millisecond, addrB: time.Millisecond})
	}
	if l := h.link("b"); l.Paths[0].Deaf || !l.Paths[0].Alive || l.Path != addrB {
		t.Fatalf("after a pong on A: %+v", l)
	}
}

func TestNoRouteExcludesAndReadmits(t *testing.T) {
	h := newHarness(t, DirectoryOptions{})
	h.g.route(addrA, false)
	h.d.SetRecords([]domain.NodeRecord{h.peerRecord(addrA, addrB)})
	h.answers[addrA], h.answers[addrB] = true, true
	h.tick() // the ping to A fails with ErrNoRoute
	h.answer(nil)
	h.tick()
	l := h.link("b")
	if l.Path != addrB || !l.Paths[0].NoRoute || l.Paths[0].Alive {
		t.Fatalf("unroutable path must be excluded: %+v", l)
	}
	// A ping that never left is not a loss, and the path is not hammered:
	// it is retried at PathProbeInterval, not 2*PathTimeout.
	for i := 0; i < 4; i++ {
		h.tick()
		h.answer(nil)
	}
	if l := h.link("b"); l.Paths[0].Losses != 0 || strings.Contains(h.metricsText(), "loadb_path_losses") {
		t.Fatalf("no-route pings must not count as losses: %+v\n%s", l, h.metricsText())
	}
	h.g.route(addrA, true)
	for i := 0; i < int(h.opts.PathProbeInterval/time.Second)+2; i++ {
		h.tick()
		h.answer(map[string]time.Duration{addrA: time.Millisecond, addrB: time.Millisecond})
	}
	if l := h.link("b"); l.Paths[0].NoRoute || !l.Paths[0].Alive {
		t.Fatalf("a successful send readmits the path: %+v", l)
	}
}

// A send that fails for a reason other than no route proves nothing about
// the path, so it must not restore a no-route flag that a pong cleared while
// the lock was released for the send.
func TestUnknownSendErrorKeepsConcurrentPong(t *testing.T) {
	h := newHarness(t, DirectoryOptions{})
	h.g.route(addrA, false)
	h.g.route(addrB, false)
	h.d.SetRecords([]domain.NodeRecord{h.peerRecord(addrA, addrB)})
	h.tick()
	if l := h.link("b"); !l.Paths[0].NoRoute || !l.Paths[1].NoRoute {
		t.Fatalf("both paths must start unroutable: %+v", l)
	}
	// Next probe round: B is routable again, so the batch has a change to
	// record, and A's send is overtaken by a pong before it fails.
	h.g.route(addrA, true)
	h.g.route(addrB, true)
	h.g.hook(addrA, func(env domain.Envelope) error {
		if env.Probe != nil && env.Probe.Type == domain.ProbePing {
			h.injectProbe(addrA, domain.Probe{Type: domain.ProbePong, Nonce: env.Probe.Nonce, HeardMS: 100})
			waitFor(t, time.Second, func() bool { return !h.link("b").Paths[0].NoRoute })
		}
		return errors.New("sendto: operation not permitted")
	})
	for i := 0; i < int(h.opts.PathProbeInterval/time.Second)+1; i++ {
		h.tick()
	}
	if l := h.link("b"); l.Paths[0].NoRoute || l.Paths[1].NoRoute {
		t.Fatalf("an unknown send error must not undo the pong: %+v", l)
	}
}

func TestIPv4MappedFromMatchesItsPath(t *testing.T) {
	h := newHarness(t, DirectoryOptions{})
	h.d.SetRecords([]domain.NodeRecord{h.peerRecord(addrA, addrB)})
	h.answers[addrB] = true
	h.tick()
	h.answer(nil)
	h.tick() // A's ping is now a loss
	waitFor(t, time.Second, func() bool { return h.link("b").Paths[0].Losses == 1 })
	h.injectHeartbeat("[::ffff:10.0.0.1]:4001", 1)
	waitFor(t, time.Second, func() bool { return h.link("b").Paths[0].Losses == 0 })
}

func TestVanishedPathIsLeftAtOnce(t *testing.T) {
	h := newHarness(t, DirectoryOptions{})
	h.d.SetRecords([]domain.NodeRecord{h.peerRecord(addrA, addrB)})
	h.d.SetRecords([]domain.NodeRecord{h.peerRecord(addrB)})
	if l := h.link("b"); l.Path != addrB || len(l.Paths) != 1 {
		t.Fatalf("after the registry dropped A: %+v", l)
	}
	if !strings.Contains(h.metricsText(), `loadb_path_switches_total{reason="initial"} 2`) {
		t.Fatalf("metrics:\n%s", h.metricsText())
	}
}

func TestBalancerAnswersPingsAndSendsNothing(t *testing.T) {
	h := newHarness(t, DirectoryOptions{LocalID: "lb", NoLocal: true})
	h.d.SetRecords([]domain.NodeRecord{h.peerRecord(addrA, addrB)})
	if l := h.link("b"); l.Path != "" {
		t.Fatalf("a balancer chooses no path: %+v", l)
	}
	h.tick()
	h.tick()
	if n := h.g.count(func(m sentMsg) bool { return m.env.HB != nil }); n != 0 {
		t.Fatalf("a balancer sent %d heartbeats", n)
	}
	// It still pings silent peers, which is how a node learns its path to
	// the balancer is deaf.
	if n := h.g.count(func(m sentMsg) bool { return m.env.Probe != nil && m.env.Probe.Type == domain.ProbePing }); n == 0 {
		t.Fatal("a balancer must ping")
	}
	h.injectProbe(addrA, domain.Probe{Type: domain.ProbePing, Nonce: 9, HeardMS: 100})
	waitFor(t, time.Second, func() bool { return h.g.count(pongsTo(addrA)) == 1 })
	if l := h.link("b"); l.Path != "" {
		t.Fatalf("still no path on a balancer: %+v", l)
	}
}
