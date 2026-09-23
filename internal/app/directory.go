package app

import (
	"context"
	"crypto/ed25519"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/d13co/algod-loadb-mesh/internal/domain"
	"github.com/d13co/algod-loadb-mesh/internal/ports"
)

// ExternalUpstream is a static third-party RPC with no agent.
type ExternalUpstream struct {
	Name         string
	URL          string
	Token        string
	Tier         int
	Capabilities *domain.CapabilityOverrides
	HealthCheck  time.Duration
}

// PeerOverride is a local correction to a registry record.
type PeerOverride struct {
	Tier *int
}

// DirectoryOptions configures the peer directory.
type DirectoryOptions struct {
	LocalID       string
	LocalTier     int
	NoLocal       bool          // balancer: no local upstream, no heartbeats of its own
	SuspectAfter  time.Duration // no heartbeat for this long: probe directly
	DownAfter     time.Duration // no heartbeat and no probe success: offline
	ProbeInterval time.Duration // direct /v2/status probing of a silent peer
	KeepAlive     time.Duration // heartbeat when nothing changed
	// PathProbeInterval is the periodic ping of every address of a peer;
	// PathTimeout is how long a ping may go unanswered before it is a loss.
	PathProbeInterval time.Duration
	PathTimeout       time.Duration
	SyncTolerance     uint64
	LagGrace          time.Duration // how long one round behind is tolerated before "lagging"
	ReturnHysteresis  int
	PeerOverrides     map[string]PeerOverride
	Externals         []ExternalUpstream
}

func (o *DirectoryOptions) defaults() {
	if o.SuspectAfter == 0 {
		o.SuspectAfter = 15 * time.Second // three keepalives, so a silent peer's ping can mark a path deaf
	}
	if o.DownAfter == 0 {
		o.DownAfter = 30 * time.Second
	}
	if o.ProbeInterval == 0 {
		o.ProbeInterval = 10 * time.Second
	}
	if o.KeepAlive == 0 {
		o.KeepAlive = 5 * time.Second
	}
	if o.PathProbeInterval == 0 {
		o.PathProbeInterval = 30 * time.Second
	}
	if o.PathTimeout == 0 {
		o.PathTimeout = 2 * time.Second
	}
	if o.ReturnHysteresis == 0 {
		o.ReturnHysteresis = 3
	}
	if o.LagGrace == 0 {
		o.LagGrace = 1500 * time.Millisecond
	}
}

func (o DirectoryOptions) judge() domain.SyncJudge {
	return domain.NewSyncJudge(o.SyncTolerance, o.LagGrace, o.ReturnHysteresis)
}

type peerState struct {
	rec     domain.NodeRecord
	hb      domain.Heartbeat
	seenAt  time.Time
	hasHB   bool
	silent  bool // the ladder has started: reset when a heartbeat is accepted
	probeAt time.Time
	probeOK bool
	probeRd uint64
	judge   domain.SyncJudge
	ln      *link
	hosts   []string // domain.EndpointHosts(rec.Endpoints), parsed once per record
}

// pathState is one address of a link and what we have measured about it.
// All of it lives under Directory.mu.
type pathState struct {
	addr     string        // normalised (domain.NormalizeAddr)
	rtt      time.Duration // EWMA, 0 until measured
	pingedAt time.Time     // zero: ping at the next tick
	pongedAt time.Time
	lastSeen time.Time // any authenticated datagram from this address: the inbound direction, shown as last_seen_age_s
	nonce    uint64    // outstanding ping, 0 when none
	fails    int       // consecutive unanswered pings; >= domain.PathFailures is dead
	noRoute  bool      // Send returned ErrNoRoute; cleared by a successful Send (pings keep trying, uncounted)
	deaf     bool      // the peer reported not hearing us while this was our path; a pong clears it
}

// link is everything about one peer's addresses and which of them carries
// our heartbeats.
type link struct {
	id         string
	pub        ed25519.PublicKey
	paths      []*pathState
	cur        int       // index into paths; -1 = none usable
	switchedAt time.Time // when cur was last chosen
	heardAt    time.Time // last authenticated heartbeat from this peer, any path; zero = never
}

// LinkStatus is one link as /loadb/status shows it.
type LinkStatus struct {
	Peer     string       `json:"peer"`
	Balancer bool         `json:"balancer,omitempty"`
	Path     string       `json:"path"` // the address carrying heartbeats, "" when none
	Paths    []PathStatus `json:"paths"`
}

// PathStatus is one candidate address of a link.
type PathStatus struct {
	Addr        string  `json:"addr"`
	RTTMS       float64 `json:"rtt_ms"`
	Alive       bool    `json:"alive"`
	Deaf        bool    `json:"deaf,omitempty"`
	NoRoute     bool    `json:"no_route,omitempty"`
	Losses      int     `json:"losses,omitempty"`
	LastPongAge float64 `json:"last_pong_age_s"` // -1 when never
	LastSeenAge float64 `json:"last_seen_age_s"` // any datagram from the address, -1 when never
}

type extState struct {
	cfg       ExternalUpstream
	client    ports.AlgodClient
	checkedAt time.Time
	ok        bool
	round     uint64
	judge     domain.SyncJudge
}

// Directory merges the static registry with live gossip and external probes
// into the upstream snapshot the router selects from.
type Directory struct {
	opts    DirectoryOptions
	monitor *Monitor
	gossip  ports.Gossip
	clients ports.AlgodClientFactory
	stats   *StatsBook
	clock   ports.Clock
	log     ports.Logger
	metric  ports.Metrics
	priv    ed25519.PrivateKey

	// onUnknown is called (outside the lock) when a heartbeat arrives from a
	// node the registry has not told us about yet.
	onUnknown func(id string)

	// nudge asks the Run loop for a heartbeat now: a path switch must not wait
	// for the next keepalive. Buffered so a signal is never lost or blocking.
	nudge  chan struct{}
	policy domain.PathPolicy

	mu         sync.Mutex
	network    string
	peers      map[string]*peerState
	balancers  map[string]*link // registry balancers: heartbeat destinations, never upstreams
	nonce      uint64           // last ping nonce issued
	hbRound    uint64           // highest round an accepted, online heartbeat reported
	hbChanged  chan struct{}    // closed when hbRound rises
	externals  map[string]*extState
	localJudge domain.SyncJudge
	localSeq   uint64
	hbSeq      uint64
	draining   bool
}

// NewDirectory wires the directory; Run starts its loops.
func NewDirectory(o DirectoryOptions, monitor *Monitor, gossip ports.Gossip, clients ports.AlgodClientFactory,
	stats *StatsBook, clock ports.Clock, log ports.Logger, metric ports.Metrics, priv ed25519.PrivateKey) *Directory {
	o.defaults()
	d := &Directory{opts: o, monitor: monitor, gossip: gossip, clients: clients, stats: stats, clock: clock,
		log: log, metric: metric, priv: priv, peers: map[string]*peerState{}, balancers: map[string]*link{}, hbChanged: make(chan struct{}), externals: map[string]*extState{},
		localJudge: o.judge(), nudge: make(chan struct{}, 1), policy: domain.PathPolicy{ProbeInterval: o.PathProbeInterval}}
	for _, e := range o.Externals {
		if e.HealthCheck == 0 {
			e.HealthCheck = time.Minute
		}
		d.externals[e.Name] = &extState{cfg: e, client: clients.NewAlgodClient(e.URL, e.Token), judge: o.judge()}
	}
	return d
}

// OnUnknownPeer registers a hook for heartbeats from unknown node ids; the
// composition root wires it to a rate-limited registry refresh.
func (d *Directory) OnUnknownPeer(f func(id string)) { d.onUnknown = f }

// SetDraining marks the local node as leaving; it is gossiped immediately.
func (d *Directory) SetDraining(v bool) {
	d.mu.Lock()
	d.draining = v
	d.mu.Unlock()
	d.sendHeartbeat(context.Background(), d.monitor.State())
}

// SetRecords replaces the static peer set. Records for other networks and
// for the local node itself are ignored. Balancers become heartbeat
// destinations only; a balancer sends no heartbeats at all.
func (d *Directory) SetRecords(recs []domain.NodeRecord) {
	local := d.monitor.State()
	network := local.Caps.GenesisID
	now := d.clock.Now()
	d.mu.Lock()
	d.network = network
	seen := map[string]bool{}
	balancers := map[string]*link{}
	switched := false
	for _, r := range recs {
		if r.ID == d.opts.LocalID || (network != "" && r.Network != network) {
			continue
		}
		if r.IsBalancer() {
			l := syncLink(d.balancers[r.ID], r)
			balancers[r.ID] = l
			switched = d.chooseLocked(l, now) || switched
			continue
		}
		if ov, ok := d.opts.PeerOverrides[r.ID]; ok && ov.Tier != nil {
			r.Tier = *ov.Tier
		}
		seen[r.ID] = true
		p, ok := d.peers[r.ID]
		if !ok {
			p = &peerState{judge: d.opts.judge()}
			d.peers[r.ID] = p
		}
		p.rec, p.hosts = r, domain.EndpointHosts(r.Endpoints)
		p.ln = syncLink(p.ln, r)
		switched = d.chooseLocked(p.ln, now) || switched
	}
	for id := range d.peers {
		if !seen[id] {
			delete(d.peers, id)
		}
	}
	d.balancers = balancers
	d.mu.Unlock()
	if switched {
		d.nudgeHeartbeat()
	}
	d.metric.Gauge("loadb_peers", float64(len(seen)))
	d.metric.Gauge("loadb_balancers", float64(len(balancers)))
}

// syncLink brings a link up to date with its record: measurements of
// surviving addresses are kept, the rest dropped, new ones appended in
// registry order. A vanished current path leaves cur at -1 for chooseLocked.
func syncLink(l *link, r domain.NodeRecord) *link {
	if l == nil {
		l = &link{id: r.ID, cur: -1}
	}
	l.pub = ed25519.PublicKey(r.Agent.PubKey)
	old := map[string]*pathState{}
	for _, p := range l.paths {
		old[p.addr] = p
	}
	var curAddr string
	if l.cur >= 0 && l.cur < len(l.paths) {
		curAddr = l.paths[l.cur].addr
	}
	paths := make([]*pathState, 0, len(r.Agent.GossipAddrs()))
	seen := map[string]bool{}
	l.cur = -1
	for _, a := range r.Agent.GossipAddrs() {
		a = domain.NormalizeAddr(a)
		if seen[a] {
			continue
		}
		seen[a] = true
		p, ok := old[a]
		if !ok {
			p = &pathState{addr: a}
		}
		if a == curAddr {
			l.cur = len(paths)
		}
		paths = append(paths, p)
	}
	l.paths = paths
	return l
}

// stats is the link as the path policy sees it.
func (l *link) stats() []domain.PathStat {
	out := make([]domain.PathStat, len(l.paths))
	for i, p := range l.paths {
		out[i] = domain.PathStat{Addr: p.addr, RTT: p.rtt, Alive: p.alive(), Deaf: p.deaf}
	}
	return out
}

func (p *pathState) alive() bool {
	return !p.noRoute && !p.deaf && p.fails < domain.PathFailures
}

// curPath is the path carrying heartbeats, nil when none.
func (l *link) curPath() *pathState {
	if l.cur < 0 || l.cur >= len(l.paths) {
		return nil
	}
	return l.paths[l.cur]
}

// curAddr is the address of the path carrying heartbeats, "" when none.
func (l *link) curAddr() string {
	if p := l.curPath(); p != nil {
		return p.addr
	}
	return ""
}

// chooseLocked runs the policy on a link and reports whether the chosen path
// changed. A balancer sends no heartbeats, so its links stay at -1.
func (d *Directory) chooseLocked(l *link, now time.Time) bool {
	if d.opts.NoLocal {
		return false
	}
	idx, reason := d.policy.Choose(now, l.stats(), l.cur, l.switchedAt)
	if idx == l.cur {
		return false
	}
	was, addr, rtt := "", "", time.Duration(0)
	if p := l.curPath(); p != nil {
		was = p.addr
	}
	if idx >= 0 {
		addr, rtt = l.paths[idx].addr, l.paths[idx].rtt
	}
	l.cur, l.switchedAt = idx, now
	d.metric.Inc("loadb_path_switches", "reason", reason)
	d.log.Info("mesh path", "peer", l.id, "addr", addr, "was", was, "rtt_ms", rtt.Milliseconds(), "reason", reason)
	return true
}

// nudgeHeartbeat asks the Run loop to send a heartbeat now.
func (d *Directory) nudgeHeartbeat() {
	select {
	case d.nudge <- struct{}{}:
	default:
	}
}

// heardMS is what a probe tells the peer: ms since its last heartbeat was
// accepted here, -1 for never.
func (l *link) heardMS(now time.Time) int64 {
	if l.heardAt.IsZero() {
		return -1
	}
	return now.Sub(l.heardAt).Milliseconds()
}

// Run drives gossip send/receive and maintenance until ctx is done. The
// loop subscribes to the monitor *before* reading its state so that a change
// landing between the two is never missed (a missed change would leave
// peers with a stale round until the next keepalive).
func (d *Directory) Run(ctx context.Context) error {
	go d.receiveLoop(ctx)
	go d.maintenanceLoop(ctx)
	keep, stop := d.clock.Tick(d.opts.KeepAlive)
	defer stop()
	var sentSeq uint64
	for {
		ch := d.monitor.Changed()
		st := d.monitor.State()
		if st.Seq != sentSeq {
			sentSeq = st.Seq
			d.onLocalChange(st)
			d.sendHeartbeat(ctx, st)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
		case <-keep:
			d.sendHeartbeat(ctx, d.monitor.State())
		case <-d.nudge:
			d.sendHeartbeat(ctx, d.monitor.State())
		}
	}
}

func (d *Directory) onLocalChange(st LocalState) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if st.Seq == d.localSeq {
		return
	}
	d.localSeq = st.Seq
	d.localJudge.Judge(d.clock.Now(), st.Online, st.LastRound, d.bestRoundLocked(st))
}

// sendJob is one datagram to one path, collected under the lock and sent
// outside it. The path pointer is stable and only read under the lock;
// addr is copied out so the send touches nothing shared.
type sendJob struct {
	link *link
	path *pathState
	addr string
	wire []byte
}

func newSendJob(l *link, p *pathState, wire []byte) sendJob {
	return sendJob{link: l, path: p, addr: p.addr, wire: wire}
}

// sendHeartbeat signs one heartbeat and sends it to the chosen path of every
// link — peers and balancers alike — exactly one datagram per link.
func (d *Directory) sendHeartbeat(ctx context.Context, st LocalState) {
	if d.opts.NoLocal {
		return
	}
	d.mu.Lock()
	d.hbSeq++
	hb := domain.Heartbeat{NodeID: d.opts.LocalID, Seq: d.hbSeq, LastRound: st.LastRound, Online: st.Online,
		Caps: st.Caps, Draining: d.draining}
	jobs := make([]sendJob, 0, len(d.peers)+len(d.balancers))
	for _, p := range d.peers {
		if path := p.ln.curPath(); path != nil {
			jobs = append(jobs, newSendJob(p.ln, path, nil))
		}
	}
	for _, l := range d.balancers {
		if path := l.curPath(); path != nil {
			jobs = append(jobs, newSendJob(l, path, nil))
		}
	}
	d.mu.Unlock()
	wire, err := domain.EncodeHeartbeat(d.priv, hb)
	if err != nil {
		d.log.Error("encode heartbeat", "err", err)
		return
	}
	for i := range jobs {
		jobs[i].wire = wire
	}
	d.send(ctx, jobs)
	d.metric.Inc("loadb_heartbeats_sent")
	d.log.Debug("heartbeat sent", "seq", hb.Seq, "round", hb.LastRound, "online", hb.Online, "to", len(jobs))
}

// send delivers jobs outside the lock and returns each job's error, then
// records what the sends proved: ErrNoRoute excludes the path until a later
// Send succeeds. Any other error, or a cancelled context, proves nothing and
// leaves the flag as a concurrent pong or send left it while the lock was
// released. The next tick re-chooses.
func (d *Directory) send(ctx context.Context, jobs []sendJob) []error {
	if len(jobs) == 0 {
		return nil
	}
	errs := make([]error, len(jobs))
	for i, j := range jobs {
		errs[i] = d.gossip.Send(ctx, j.addr, j.wire)
		if err := errs[i]; err != nil && !errors.Is(err, ports.ErrNoRoute) && ctx.Err() == nil {
			d.log.Debug("gossip send", "peer", j.link.id, "addr", j.addr, "err", err)
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for i, j := range jobs {
		noRoute := errors.Is(errs[i], ports.ErrNoRoute)
		if errs[i] != nil && !noRoute {
			continue
		}
		if noRoute && !j.path.noRoute {
			d.log.Debug("mesh path unroutable", "peer", j.link.id, "addr", j.addr)
		}
		j.path.noRoute = noRoute
	}
	return errs
}

func (d *Directory) receiveLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-d.gossip.Receive():
			if !ok {
				return
			}
			d.handle(ctx, msg)
		}
	}
}

// handle decodes outside the lock, updates state under it, and sends any
// reply after releasing it: gossip.Send is never called with d.mu held.
func (d *Directory) handle(ctx context.Context, msg ports.GossipMessage) {
	env, err := domain.DecodeMessage(msg.Payload)
	if err != nil {
		d.metric.Inc("loadb_heartbeats_rejected", "reason", "decode")
		d.log.Debug("udp received", "from", msg.From, "bytes", len(msg.Payload), "result", "undecodable", "err", err)
		return
	}
	reply, switched := d.handleLocked(msg, env)
	if switched {
		d.nudgeHeartbeat()
	}
	if reply != nil {
		wire, err := domain.EncodeProbe(d.priv, *reply)
		if err != nil {
			d.log.Error("encode pong", "err", err)
			return
		}
		if err := d.gossip.Send(ctx, msg.From, wire); err != nil && ctx.Err() == nil {
			d.log.Debug("gossip send", "peer", env.NodeID, "addr", msg.From, "err", err)
		}
	}
}

// handleLocked returns the pong to send back, if any, and whether the chosen
// path of a link changed.
func (d *Directory) handleLocked(msg ports.GossipMessage, env domain.Envelope) (*domain.Probe, bool) {
	from := domain.NormalizeAddr(msg.From)
	kind, rejected := "loadb_heartbeats_rejected", "heartbeat"
	if env.Probe != nil {
		kind, rejected = "loadb_probes_rejected", env.Probe.Type
	}
	logMsg := func(result string) {
		if env.HB != nil {
			d.log.Debug("udp received", "from", from, "bytes", len(msg.Payload), "result", result, "id", env.NodeID,
				"seq", env.HB.Seq, "round", env.HB.LastRound, "online", env.HB.Online, "draining", env.HB.Draining)
			return
		}
		d.log.Debug("udp received", "from", from, "bytes", len(msg.Payload), "result", result, "id", env.NodeID,
			"type", rejected, "nonce", env.Probe.Nonce, "heard_ms", env.Probe.HeardMS)
	}
	now := d.clock.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	p, ok := d.peers[env.NodeID]
	var l *link
	switch {
	case ok:
		l = p.ln
	case d.balancers[env.NodeID] != nil && env.HB != nil:
		d.metric.Inc(kind, "reason", "balancer")
		logMsg("balancer")
		return nil, false
	case d.balancers[env.NodeID] != nil:
		l = d.balancers[env.NodeID]
	default:
		d.metric.Inc(kind, "reason", "unknown")
		logMsg("unknown_node")
		if d.onUnknown != nil {
			go d.onUnknown(env.NodeID)
		}
		return nil, false
	}
	if len(l.pub) != ed25519.PublicKeySize || !env.PubKey.Equal(l.pub) {
		d.metric.Inc(kind, "reason", "key")
		logMsg("wrong_key")
		return nil, false
	}
	// Authenticated: whatever it is, this path delivers.
	for _, path := range l.paths {
		if path.addr == from {
			path.lastSeen = now
			path.fails = 0
		}
	}
	if env.Probe != nil {
		return d.handleProbeLocked(l, from, *env.Probe, now, logMsg)
	}
	hb := *env.HB
	l.heardAt = now
	if p.hasHB && hb.Seq <= p.hb.Seq && now.Sub(p.seenAt) < d.opts.DownAfter {
		logMsg("stale")
		return nil, false // stale or duplicate
	}
	d.metric.Inc("loadb_heartbeats_received")
	logMsg("accepted")
	p.hb, p.seenAt, p.hasHB, p.silent = hb, now, true, false
	if hb.Online && hb.LastRound > d.hbRound {
		d.hbRound = hb.LastRound
		close(d.hbChanged)
		d.hbChanged = make(chan struct{})
	}
	p.judge.Judge(now, hb.Online, hb.LastRound, d.bestRoundLocked(d.monitor.State()))
	return nil, false
}

// handleProbeLocked answers a ping with a pong and matches a pong to its
// ping. Either kind carries the peer's HeardMS, which is the asymmetric-case
// signal: if the peer has not heard our heartbeats for 3*KeepAlive (three
// lost in a row, like PathFailures for pings) while we have been sending them
// on the current path for longer than that, the path is deaf and left at
// once. SuspectAfter defaults to the same 3*KeepAlive so that the silent
// peer's first ping is enough to trip this. A negative HeardMS (never heard) is left to loss
// counting, because a restarted peer legitimately reports it.
func (d *Directory) handleProbeLocked(l *link, from string, pr domain.Probe, now time.Time, logMsg func(string)) (*domain.Probe, bool) {
	var reply *domain.Probe
	switch pr.Type {
	case domain.ProbePing:
		d.metric.Inc("loadb_probes_received", "type", "ping")
		logMsg("ping")
		reply = &domain.Probe{Type: domain.ProbePong, NodeID: d.opts.LocalID, Nonce: pr.Nonce, HeardMS: l.heardMS(now)}
	case domain.ProbePong:
		matched := false
		for _, path := range l.paths {
			if path.nonce != 0 && path.nonce == pr.Nonce {
				matched = true
				sample := now.Sub(path.pingedAt)
				if path.rtt == 0 {
					path.rtt = sample
				} else {
					path.rtt = time.Duration(0.8*float64(path.rtt) + 0.2*float64(sample))
				}
				path.nonce, path.pongedAt, path.fails, path.deaf, path.noRoute = 0, now, 0, false, false
				d.metric.Gauge("loadb_path_rtt_ms", float64(path.rtt.Microseconds())/1000, "peer", l.id, "addr", path.addr)
			}
		}
		d.metric.Inc("loadb_probes_received", "type", "pong", "matched", boolStr(matched))
		if !matched {
			logMsg("pong_unmatched")
			return nil, false
		}
		logMsg("pong")
	}
	stale := d.opts.KeepAlive * 3
	if cur := l.curPath(); cur != nil && pr.HeardMS >= 0 && time.Duration(pr.HeardMS)*time.Millisecond > stale &&
		now.Sub(l.switchedAt) > stale && !cur.deaf {
		cur.deaf = true
		d.log.Info("mesh path deaf", "peer", l.id, "addr", cur.addr, "heard_ms", pr.HeardMS)
	}
	return reply, d.chooseLocked(l, now)
}

func (d *Directory) maintenanceLoop(ctx context.Context) {
	tick, stop := d.clock.Tick(time.Second)
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			d.probeSilentPeers(ctx)
			d.checkPaths(ctx)
			d.checkExternals(ctx)
		}
	}
}

// probeSilentPeers is the only control-plane traffic to a remote algod: when
// a peer's agent is silent its node may still be fine, so ask it directly,
// at most once per ProbeInterval.
func (d *Directory) probeSilentPeers(ctx context.Context) {
	now := d.clock.Now()
	type job struct {
		id  string
		url string
		tok string
	}
	var jobs []job
	d.mu.Lock()
	for id, p := range d.peers {
		silent := !p.hasHB || now.Sub(p.seenAt) > d.opts.SuspectAfter
		if silent && !p.silent {
			// Entering silence: ping every path of the link on this same tick
			// (checkPaths follows), so the pings — and the HeardMS they carry
			// — go out with the ladder's first direct probe, not after it.
			p.silent = true
			for _, path := range p.ln.paths {
				if path.nonce == 0 {
					path.pingedAt = time.Time{}
				}
			}
		}
		if silent && now.Sub(p.probeAt) >= d.opts.ProbeInterval && len(p.rec.Endpoints) > 0 {
			p.probeAt = now
			jobs = append(jobs, job{id, domain.EndpointFor(p.rec.Endpoints, p.hosts, p.ln.curAddr()), p.rec.Token})
		}
	}
	d.mu.Unlock()
	for _, j := range jobs {
		go func(j job) {
			pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			st, err := d.clients.NewAlgodClient(j.url, j.tok).Status(pctx)
			d.metric.Inc("loadb_peer_probes", "ok", boolStr(err == nil))
			d.mu.Lock()
			defer d.mu.Unlock()
			p, ok := d.peers[j.id]
			if !ok {
				return
			}
			p.probeOK = err == nil
			if err == nil {
				p.probeRd = st.LastRound
			}
		}(j)
	}
}

// checkPaths counts losses, pings the paths that are due and re-runs the
// policy. One cadence rule: a path is pinged when now - pingedAt reaches
// PathProbeInterval if its last ping was answered (or it is already dead),
// and 2*PathTimeout otherwise — never measured, or lost. A silent peer's
// paths are re-pinged at once (probeSilentPeers zeroes pingedAt). Pings are
// collected under the lock and sent outside it.
func (d *Directory) checkPaths(ctx context.Context) {
	now := d.clock.Now()
	type ping struct {
		job sendJob
		pr  domain.Probe
	}
	var pings []ping
	d.mu.Lock()
	links := make([]*link, 0, len(d.peers)+len(d.balancers))
	for _, p := range d.peers {
		links = append(links, p.ln)
	}
	for _, l := range d.balancers {
		links = append(links, l)
	}
	for _, l := range links {
		for _, path := range l.paths {
			if path.nonce != 0 && now.Sub(path.pingedAt) >= d.opts.PathTimeout {
				path.nonce = 0
				path.fails++
				d.metric.Inc("loadb_path_losses", "peer", l.id)
				d.log.Debug("mesh path ping lost", "peer", l.id, "addr", path.addr, "losses", path.fails)
			}
			if path.nonce != 0 {
				continue
			}
			answered := !path.pongedAt.Before(path.pingedAt)
			interval := 2 * d.opts.PathTimeout
			if answered || path.fails >= domain.PathFailures || path.noRoute {
				interval = d.opts.PathProbeInterval
			}
			if !path.pingedAt.IsZero() && now.Sub(path.pingedAt) < interval {
				continue
			}
			d.nonce++
			path.nonce, path.pingedAt = d.nonce, now
			pings = append(pings, ping{newSendJob(l, path, nil), domain.Probe{Type: domain.ProbePing, NodeID: d.opts.LocalID, Nonce: d.nonce, HeardMS: l.heardMS(now)}})
		}
	}
	d.mu.Unlock()
	kept := pings[:0]
	for _, pg := range pings {
		wire, err := domain.EncodeProbe(d.priv, pg.pr)
		if err != nil {
			d.log.Error("encode ping", "err", err)
			continue
		}
		pg.job.wire = wire
		kept = append(kept, pg)
	}
	pings = kept
	jobs := make([]sendJob, len(pings))
	for i, pg := range pings {
		jobs[i] = pg.job
	}
	errs := d.send(ctx, jobs)
	d.mu.Lock()
	// A ping that never left (no route) is not a loss: forget its nonce so
	// the timeout above does not count it. noRoute keeps the path excluded,
	// and the ping at the next PathProbeInterval is how it is readmitted.
	for i, pg := range pings {
		if errors.Is(errs[i], ports.ErrNoRoute) && pg.job.path.nonce == pg.pr.Nonce {
			pg.job.path.nonce = 0
		}
	}
	switched := false
	for _, l := range links {
		switched = d.chooseLocked(l, now) || switched
	}
	d.mu.Unlock()
	if switched {
		d.nudgeHeartbeat()
	}
}

// LinkSnapshot reports every link and its candidate paths for /loadb/status.
func (d *Directory) LinkSnapshot() []LinkStatus {
	now := d.clock.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]LinkStatus, 0, len(d.peers)+len(d.balancers))
	add := func(l *link, balancer bool) {
		ls := LinkStatus{Peer: l.id, Balancer: balancer, Paths: make([]PathStatus, 0, len(l.paths))}
		if p := l.curPath(); p != nil {
			ls.Path = p.addr
		}
		age := func(t time.Time) float64 {
			if t.IsZero() {
				return -1
			}
			return now.Sub(t).Seconds()
		}
		for _, p := range l.paths {
			ls.Paths = append(ls.Paths, PathStatus{Addr: p.addr, RTTMS: float64(p.rtt.Microseconds()) / 1000, Alive: p.alive(),
				Deaf: p.deaf, NoRoute: p.noRoute, Losses: p.fails, LastPongAge: age(p.pongedAt), LastSeenAge: age(p.lastSeen)})
		}
		out = append(out, ls)
	}
	ids := make([]string, 0, len(d.peers))
	for id := range d.peers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		add(d.peers[id].ln, false)
	}
	ids = ids[:0]
	for id := range d.balancers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		add(d.balancers[id], true)
	}
	return out
}

func (d *Directory) checkExternals(ctx context.Context) {
	now := d.clock.Now()
	var due []*extState
	d.mu.Lock()
	for _, e := range d.externals {
		if now.Sub(e.checkedAt) >= e.cfg.HealthCheck {
			e.checkedAt = now
			due = append(due, e)
		}
	}
	d.mu.Unlock()
	for _, e := range due {
		go func(e *extState) {
			pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			st, err := e.client.Status(pctx)
			d.metric.Inc("loadb_external_checks", "name", e.cfg.Name, "ok", boolStr(err == nil))
			d.mu.Lock()
			defer d.mu.Unlock()
			e.ok = err == nil
			if err == nil {
				e.round = st.LastRound
			} else {
				d.log.Warn("external upstream check failed", "name", e.cfg.Name, "err", err)
			}
		}(e)
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// bestRoundLocked is the highest round among local, fresh peers and externals.
func (d *Directory) bestRoundLocked(local LocalState) uint64 {
	now := d.clock.Now()
	best := uint64(0)
	if local.Online {
		best = local.LastRound
	}
	for _, p := range d.peers {
		if r, ok := d.peerRoundLocked(p, now); ok && r > best {
			best = r
		}
	}
	for _, e := range d.externals {
		if e.ok && e.round > best {
			best = e.round
		}
	}
	return best
}

// peerRoundLocked returns the peer's round and whether it is reachable.
func (d *Directory) peerRoundLocked(p *peerState, now time.Time) (uint64, bool) {
	fresh := p.hasHB && now.Sub(p.seenAt) <= d.opts.SuspectAfter
	if fresh {
		return p.hb.LastRound, p.hb.Online
	}
	if p.probeOK && now.Sub(p.probeAt) <= 2*d.opts.ProbeInterval {
		return p.probeRd, true
	}
	return 0, false
}

// Snapshot builds the candidate list for one routing decision.
func (d *Directory) Snapshot() ([]domain.Upstream, uint64) {
	local := d.monitor.State()
	now := d.clock.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	best := d.bestRoundLocked(local)
	out := make([]domain.Upstream, 0, 1+len(d.peers)+len(d.externals))

	if !d.opts.NoLocal {
		lh := d.localJudge.Judge(now, local.Online, local.LastRound, best)
		out = append(out, domain.Upstream{ID: d.opts.LocalID, Kind: domain.KindLocal, Tier: d.opts.LocalTier,
			BaseURL: local.Endpoint, Token: local.Config.Token, Health: lh, LastRound: local.LastRound,
			Caps: local.Caps, Draining: d.draining, Stats: d.stats.Snapshot(d.opts.LocalID), Source: "local"})
	}

	ids := make([]string, 0, len(d.peers))
	for id := range d.peers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		p := d.peers[id]
		u := domain.Upstream{ID: id, Kind: domain.KindPeer, Tier: p.rec.Tier, Token: p.rec.Token,
			Stats: d.stats.Snapshot(id), Health: domain.HealthOffline}
		// The endpoint on the host of the current heartbeat path, so algod
		// traffic fails over with it; else the first.
		u.BaseURL = domain.EndpointFor(p.rec.Endpoints, p.hosts, p.ln.curAddr())
		fresh := p.hasHB && now.Sub(p.seenAt) <= d.opts.SuspectAfter
		switch {
		case fresh:
			u.Source = "heartbeat"
			u.LastRound, u.Caps, u.Draining = p.hb.LastRound, p.hb.Caps, p.hb.Draining
			u.Health = p.judge.Judge(now, p.hb.Online, u.LastRound, best)
		case p.probeOK && now.Sub(p.probeAt) <= 2*d.opts.ProbeInterval:
			// Agent silent, node answers: degraded knowledge. Use the last
			// heartbeat's capabilities (or the declared ones) and the probed round.
			u.Source = "probe"
			u.LastRound = p.probeRd
			u.Caps = p.hb.Caps
			if !p.hasHB {
				u.Caps = p.rec.Declared.Apply(domain.Capabilities{OldestRound: domain.OldestRoundFor(domain.Archival{Kind: domain.ArchivalNone}, p.probeRd)}, p.probeRd)
			}
			u.Health = p.judge.Judge(now, true, u.LastRound, best)
		default:
			u.Source = "none"
			p.judge.Judge(now, false, 0, best)
		}
		out = append(out, u)
	}

	names := make([]string, 0, len(d.externals))
	for n := range d.externals {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		e := d.externals[n]
		caps := e.cfg.Capabilities.Apply(domain.Capabilities{
			OldestRound: domain.OldestRoundFor(domain.Archival{Kind: domain.ArchivalNone}, e.round),
			Archival:    domain.Archival{Kind: domain.ArchivalNone}}, e.round)
		u := domain.Upstream{ID: n, Kind: domain.KindExternal, Tier: e.cfg.Tier, BaseURL: e.cfg.URL, Token: e.cfg.Token,
			LastRound: e.round, Caps: caps, Stats: d.stats.Snapshot(n), Health: domain.HealthOffline, Source: "check"}
		u.Health = e.judge.Judge(now, e.ok, e.round, best)
		out = append(out, u)
	}
	return out, best
}

// WaitForHeartbeatRound blocks until a heartbeat reports a round past round,
// or ctx is done, and returns the highest heartbeat round seen.
func (d *Directory) WaitForHeartbeatRound(ctx context.Context, round uint64) (uint64, error) {
	for {
		d.mu.Lock()
		best, ch := d.hbRound, d.hbChanged
		d.mu.Unlock()
		if best > round {
			return best, nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return best, ctx.Err()
		}
	}
}

// Balancers lists the balancer ids this agent sends heartbeats to.
func (d *Directory) Balancers() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	ids := make([]string, 0, len(d.balancers))
	for id := range d.balancers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// PeerCount is for status output.
func (d *Directory) PeerCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.peers)
}
