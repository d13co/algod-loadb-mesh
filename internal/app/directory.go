package app

import (
	"context"
	"crypto/ed25519"
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
	LocalID          string
	LocalTier        int
	SuspectAfter     time.Duration // no heartbeat for this long: probe directly
	DownAfter        time.Duration // no heartbeat and no probe success: offline
	ProbeInterval    time.Duration // direct /v2/status probing of a silent peer
	KeepAlive        time.Duration // heartbeat when nothing changed
	SyncTolerance    uint64
	LagGrace         time.Duration // how long one round behind is tolerated before "lagging"
	ReturnHysteresis int
	PeerOverrides    map[string]PeerOverride
	Externals        []ExternalUpstream
}

func (o *DirectoryOptions) defaults() {
	if o.SuspectAfter == 0 {
		o.SuspectAfter = 10 * time.Second
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
	probeAt time.Time
	probeOK bool
	probeRd uint64
	judge   domain.SyncJudge
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

	mu         sync.Mutex
	network    string
	peers      map[string]*peerState
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
		log: log, metric: metric, priv: priv, peers: map[string]*peerState{}, externals: map[string]*extState{},
		localJudge: o.judge()}
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
// for the local node itself are ignored.
func (d *Directory) SetRecords(recs []domain.NodeRecord) {
	local := d.monitor.State()
	network := local.Caps.GenesisID
	d.mu.Lock()
	d.network = network
	seen := map[string]bool{}
	var addrs []string
	for _, r := range recs {
		if r.ID == d.opts.LocalID || (network != "" && r.Network != network) {
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
		p.rec = r
		if r.Agent.Addr != "" {
			addrs = append(addrs, r.Agent.Addr)
		}
	}
	for id := range d.peers {
		if !seen[id] {
			delete(d.peers, id)
		}
	}
	d.mu.Unlock()
	d.gossip.SetPeers(addrs)
	d.metric.Gauge("loadb_peers", float64(len(seen)))
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

func (d *Directory) sendHeartbeat(ctx context.Context, st LocalState) {
	d.mu.Lock()
	d.hbSeq++
	hb := domain.Heartbeat{NodeID: d.opts.LocalID, Seq: d.hbSeq, LastRound: st.LastRound, Online: st.Online,
		Caps: st.Caps, Draining: d.draining}
	d.mu.Unlock()
	wire, err := domain.EncodeHeartbeat(d.priv, hb)
	if err != nil {
		d.log.Error("encode heartbeat", "err", err)
		return
	}
	if err := d.gossip.Broadcast(ctx, wire); err != nil && ctx.Err() == nil {
		d.log.Warn("gossip broadcast", "err", err)
	}
	d.metric.Inc("loadb_heartbeats_sent")
	d.log.Debug("heartbeat sent", "seq", hb.Seq, "round", hb.LastRound, "online", hb.Online)
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
			d.handle(msg)
		}
	}
}

func (d *Directory) handle(msg ports.GossipMessage) {
	hb, pub, err := domain.DecodeHeartbeat(msg.Payload)
	if err != nil {
		d.metric.Inc("loadb_heartbeats_rejected", "reason", "decode")
		d.log.Debug("udp received", "from", msg.From, "bytes", len(msg.Payload), "result", "undecodable", "err", err)
		return
	}
	logMsg := func(result string) {
		d.log.Debug("udp received", "from", msg.From, "bytes", len(msg.Payload), "result", result, "id", hb.NodeID,
			"seq", hb.Seq, "round", hb.LastRound, "online", hb.Online, "draining", hb.Draining)
	}
	now := d.clock.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	p, ok := d.peers[hb.NodeID]
	if !ok {
		d.metric.Inc("loadb_heartbeats_rejected", "reason", "unknown")
		logMsg("unknown_node")
		if d.onUnknown != nil {
			go d.onUnknown(hb.NodeID)
		}
		return
	}
	if len(p.rec.Agent.PubKey) != ed25519.PublicKeySize || !pub.Equal(ed25519.PublicKey(p.rec.Agent.PubKey)) {
		d.metric.Inc("loadb_heartbeats_rejected", "reason", "key")
		logMsg("wrong_key")
		return
	}
	if p.hasHB && hb.Seq <= p.hb.Seq && now.Sub(p.seenAt) < d.opts.DownAfter {
		logMsg("stale")
		return // stale or duplicate
	}
	d.metric.Inc("loadb_heartbeats_received")
	logMsg("accepted")
	p.hb, p.seenAt, p.hasHB = hb, now, true
	p.judge.Judge(now, hb.Online, hb.LastRound, d.bestRoundLocked(d.monitor.State()))
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
		if silent && now.Sub(p.probeAt) >= d.opts.ProbeInterval && len(p.rec.Endpoints) > 0 {
			p.probeAt = now
			jobs = append(jobs, job{id, p.rec.Endpoints[0], p.rec.Token})
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

	lh := d.localJudge.Judge(now, local.Online, local.LastRound, best)
	out = append(out, domain.Upstream{ID: d.opts.LocalID, Kind: domain.KindLocal, Tier: d.opts.LocalTier,
		BaseURL: local.Endpoint, Token: local.Config.Token, Health: lh, LastRound: local.LastRound,
		Caps: local.Caps, Draining: d.draining, Stats: d.stats.Snapshot(d.opts.LocalID), Source: "local"})

	ids := make([]string, 0, len(d.peers))
	for id := range d.peers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		p := d.peers[id]
		u := domain.Upstream{ID: id, Kind: domain.KindPeer, Tier: p.rec.Tier, Token: p.rec.Token,
			Stats: d.stats.Snapshot(id), Health: domain.HealthOffline}
		if len(p.rec.Endpoints) > 0 {
			u.BaseURL = p.rec.Endpoints[0]
		}
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

// PeerCount is for status output.
func (d *Directory) PeerCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.peers)
}
