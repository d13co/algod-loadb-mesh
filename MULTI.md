# Multi-interface mesh: bind all, advertise all, one path per link

## Context

The hosts sit on two WireGuard nets, 10.112.\* and 10.114.\*. The client
listener already binds both (`config.Addrs`, `Agent.Serve`, autoconfig's
`detect_listen`). The mesh has not caught up: `mesh.listen` is a single address
defaulting to `0.0.0.0:4001`, so gossip answers on the public interface, and
`mesh.advertise` is a single address, so a node publishes one way to be reached
and a peer has no second path when it breaks.

This binds every mesh interface, publishes all of them in the registry, and has
each pair of agents settle on **one** path for their heartbeats: chosen by
measured RTT, re-checked periodically, and abandoned quickly when it stops
working — including the asymmetric case, where the only party that can see the
break is the one that has stopped receiving.

Settled with the user: topology is **mixed** (a pair may share two paths, one,
or none); path choice is **measured RTT, re-checked**; the fleet upgrades at
once, so no mixed-version wire compatibility is required (cheap read-tolerance
of old records is kept anyway); the chosen path carries **heartbeats only** —
proxied algod traffic keeps using `Endpoints[0]`, and the proxy path, the
selector and `domain.Upstream` are not touched (the router only gains a `links`
key in `/loadb/status`, §7); it lands as **one change**.

Two invariants to protect, both verified as real constraints: `recordVersion`
stays 1 (an older binary hard-errors and `Registry.List` logs a warning and
drops the box), and nothing new may enter the heartbeat send path — `TestHeartbeatPropagationLatency`
fails above a 200 ms worst case.

## Design

### 1. Gossip port — `internal/ports/ports.go`

Path choice is policy and belongs in `internal/app`, so the transport stops
keeping a peer table. `SetPeers` and `Broadcast` go; unicast replaces them:

```go
type Gossip interface {
    // Send hands one datagram to the network. A nil error means it was sent,
    // not that it arrived. ErrNoRoute means the address is unusable from this
    // host and the caller should try another.
    Send(ctx context.Context, addr string, payload []byte) error
    Receive() <-chan GossipMessage
    Close() error
}
var ErrNoRoute = errors.New("gossip: no route")
```

`GossipMessage{From, Payload}` keeps its shape; `From` is the source address
the transport saw — the path the datagram arrived on and where a reply goes,
never an identity. Addresses stay opaque strings, so `"mem:<id>"` still works.
A black-holed peer must still return nil: liveness stays the receiver's
judgement.

### 2. Registry record — `internal/domain/types.go`

```go
type AgentInfo struct {
    Addrs  []string `json:"addrs,omitempty"` // one per mesh interface, preferred first
    Addr   string   `json:"addr,omitempty"`  // pre-multipath; read, never written
    PubKey []byte   `json:"pubkey"`
}
func (a AgentInfo) GossipAddrs() []string // Addrs, then Addr if not already there
```

Every consumer goes through `GossipAddrs()`. `Validate` (`types.go:154`):
balancers need `len(GossipAddrs()) > 0`. `StaticEqual` (`types.go:172`):
`equalStrings(GossipAddrs(), …)`, order-sensitive like `Endpoints` — without
this an upgraded node never republishes its second address. Keeping `Addr`
readable costs one accessor and keeps hand-written `registry.static:` YAML
loading under `KnownFields(true)`.

~46 bytes on a 250-450 byte record against a 16356-byte ceiling; ~0.018 ALGO
more MBR, absorbed by `ensureFunded`. `TestRecordCodecGolden` still passes — it
pins `version||nonce` and the first three ciphertext bytes; under AES-GCM's
CTR keystream those depend only on the first three plaintext bytes, `{"i` of
`{"id":…`, and `agent` sits far later in the plaintext.

### 3. Probe messages — `internal/domain/codec.go`

Reuse the heartbeat envelope (`pubkey(32) || sig(64) || JSON`, signature over
the body) and discriminate on a JSON field a heartbeat never has, so the
existing format, `EncodeHeartbeat`/`DecodeHeartbeat` and `TestHeartbeatCodec`
are untouched:

```go
type Probe struct {
    Type    string `json:"t"`        // "ping" | "pong"
    NodeID  string `json:"id"`
    Nonce   uint64 `json:"n"`        // echoed verbatim
    HeardMS int64  `json:"heard_ms"` // ms since I last authenticated a
                                     // heartbeat from you, any path; -1 = never
}
type Envelope struct { PubKey ed25519.PublicKey; NodeID string; HB *Heartbeat; Probe *Probe }

func EncodeProbe(priv, Probe) ([]byte, error)
func DecodeMessage(wire []byte) (Envelope, error) // verify once, peek at "t", unmarshal
```

`DecodeHeartbeat` becomes a two-line wrapper over `DecodeMessage` that errors
unless `HB != nil`, so there is one verify path and a ping can never land as a
`Seq: 0` heartbeat. `TestHeartbeatCodec` passes unchanged.

**`HeardMS` rides on both probe types, never on the heartbeat.** A single
broadcast payload cannot carry per-recipient data, and per-recipient heartbeat
bodies would mean one ed25519 signature per peer per cycle — inside the path
the 200 ms propagation test measures. Probes are already per-peer and off that
path. It also keeps `domain.Heartbeat` a comparable struct, so
`TestHeartbeatCodec`'s `got != hb` comparison is unaffected. Carrying it on the ping is
what makes the asymmetric case fast: the side that stopped receiving is the
one that pings at `SuspectAfter`, and that ping tells the other side "I have
not heard you" before its own periodic probe would (§7).

### 4. Path policy — `internal/domain/path.go` (new, pure, mirrors `SyncJudge`)

```go
// Alive: routable, fewer than PathFailures consecutive losses, not deaf.
// Deaf: the peer told us it is not hearing us on it (§7); implies !Alive.
type PathStat struct { Addr string; RTT time.Duration; Alive, Deaf bool }
type PathPolicy struct { ProbeInterval time.Duration }
// PathFailures is exported: internal/app counts the losses and applies it.
const ( pathSwitchMargin = 0.25; PathFailures = 3 ) // not config: nobody tunes these
// Cooldown is 2*ProbeInterval: a "faster" switch needs two consecutive probes to agree.
// Choose returns the index to use and why: initial | dead | deaf | faster | keep | none.
func (p PathPolicy) Choose(now time.Time, stats []PathStat, cur int, since time.Time) (int, string)
```

A dead, deaf or vanished current path switches at once — `deaf` is reported
as its own reason so the metric and log separate "the peer said so" from
"three pings went unanswered"; a merely slower one needs both the margin and
the cooldown. Unmeasured-but-alive paths rank after
measured ones, and with nothing measured index 0 — registry order — wins, so
the first heartbeat never waits for an RTT sample. EWMA alpha 0.2, matching
`StatsBook`; the first sample seeds it. The reason string is used verbatim as
the metric label and log field.

### 5. Transport — `internal/adapters/gossipudp`, `internal/adapters/gossipmem`

`Listen(addrs []string)` binds one `*net.UDPConn` per address, one receive loop
each into the single cap-256 channel, closing what it opened if a later bind
fails. `Close` becomes idempotent (`sync.Once` + `WaitGroup`), since several
loops now race to notice.

Source selection is the crux: a datagram to a 10.114 peer must leave with a
10.114 source or WireGuard's cryptokey routing drops it. Ask the kernel rather
than doing subnet arithmetic — `pick(addr)`, cached per destination:

1. `net.ResolveUDPAddr` — a parse error is permanent.
2. `net.DialUDP("udp", nil, ua)` sends nothing; `connect(2)` only does the
   route lookup, and its failure is exactly how a host on one net learns it
   cannot reach the other → `ErrNoRoute`.
3. `src := c.LocalAddr().IP`, close, then match the socket bound to `src`;
   else a wildcard socket of the right family; else `ErrNoRoute` — so binding
   the WG addresses explicitly turns "this would leave via the public
   interface" into a clean rejection, which is the second half of "off the
   public internet".

`Send` writes via that socket. The cache holds successful picks only — an
`ErrNoRoute` is never cached — and an entry is dropped on a write error. No
TTL: an interface coming up is noticed by the directory's next periodic ping
to that address (§7), which is the retry anyway. `MaxDatagram` stays 1400.

`From` is normalised once per candidate address (parse, `To4()` when it is
IPv4, re-format) and again on receipt before it is compared to anything: a
dual-stack `[::]` listener reports IPv4 sources as `[::ffff:a.b.c.d]:port`,
which would never match an advertised `a.b.c.d:port`.

`gossipmem`: `Join(addrs ...string)` registers one endpoint under several
opaque addresses; address *i* of every endpoint is net *i*, and `""` means
"not on that net". A `Send` from an endpoint with no net-*i* address to another
endpoint's net-*i* address returns `ErrNoRoute` — the mixed topology falls
out of `Join`, no extra call. The hub's map holds every address; `Send(addr)`
finds the destination and its net *i* from `addr`, uses the sender's own net-*i*
address as the source and `From`, and drops the datagram when **either that
source or `addr`** is partitioned — never the endpoint's primary address, which
`Addr()` still returns as the net-0 alias. So `Partition(addr)` keeps its
meaning and now cuts **one path**, which is the per-net outage the sim needs,
while `Partition("mem:plain")` in today's single-net fleets still cuts the node
entirely because net 0 is its only net.

### 6. Config — `internal/config/config.go`, `deploy/`

`Mesh.Listen` and `Mesh.Advertise` become `config.Addrs`. `Listen` uses the
existing `resolve("mesh.listen", "0.0.0.0:4001")` — its wildcard-plus-specific
rule is exactly UDP's `EADDRINUSE` case. Advertise needs a second resolver,
since a wildcard is meaningless as a destination:

```go
// resolvePeers validates addresses others send to: a port is required and
// 0.0.0.0 / :: are rejected.
func (a Addrs) resolvePeers(what string) (Addrs, error)
```

The auto_register requirement keeps its wording (`config_test.go` greps for
`mesh.advertise`), now `len(c.Mesh.Advertise) == 0`. Two new options:
`path_probe_interval` 30s and `path_timeout` 2s. Failures, margin and cooldown
are constants or derived (§4) — each knob costs a field, a default, an example
line, a `config check` line and docs, and nobody will tune them.

"Off the public internet" is a deployment property, so config **warns**, never
errors (a single-homed public host must still work): `Addrs.Public()` and
`Addrs.Wildcard()` (via `net.ParseIP` + `IsPrivate`/`IsLoopback`/`IsLinkLocal*`),
surfaced by `agent.FromConfig` at startup and by `config check`.

`config check` prints both mesh lists, but its `listen …` line must keep the
shape `listen <first>, <rest>`: `deploy/setup.sh` already takes the first
client address off that line with `sed -n 's/^listen \([^,]*\).*/\1/p'` for
its "peers:" hint.

`deploy/autoconfig.sh` **binds what it advertises**: one list, written to both
`mesh.listen` and `mesh.advertise` — every `--address` given, else all
detected 10.112/10.114 addresses via `detect_listen`/`global_v4` (generalise
`listen_yaml` to `addr_list_yaml KEY ADDR…` and `with_port` to take a port
instead of reading `$client_port`). Only the no-WG fallback differs: listen
`0.0.0.0:4001`, advertise the single detected/public address, with the warning
the client listener already prints. `--address` becomes repeatable; its first
value still picks the `advertise_endpoints` host. Update the three example
configs — `TestExampleListsEveryOption` only enforces the new keys in
`config.example.yaml`; the static and balancer examples are updated by hand and
`TestExamplesLoad` only checks that they still load — and the README.

### 7. Link state machine — `internal/app/directory.go`

```go
type pathState struct {
    addr     string        // normalised (§5)
    rtt      time.Duration // EWMA, 0 until measured
    pingedAt, pongedAt time.Time
    lastSeen time.Time     // any datagram from this address: liveness for free
    nonce    uint64        // outstanding ping
    fails    int           // consecutive unanswered pings; >= PathFailures is dead
    noRoute  bool
    deaf     bool          // peer reported stale HeardMS while this was our path; a pong clears it
}
type link struct {
    id string; pub ed25519.PublicKey
    paths []*pathState
    cur int            // -1 = none usable
    switchedAt time.Time
}
```

`peerState` gains `link`; `d.balancers` becomes `map[string]*link` (`Balancers()`
output unchanged) and stores their pubkeys, since balancers answer pings while
never sending heartbeats. On a **node**, a balancer link is an ordinary send
destination: it gets a chosen path, heartbeats and probes exactly like a peer
link. On a **balancer** (`NoLocal`), every link has `cur = -1` for good: it
sends no heartbeats, and its links are consulted only by `handleLocked` for the
pubkey check on incoming pings and by `probeSilentPeers`/`checkPaths`, which
still ping silent nodes — that ping is what tells a node its path to the
balancer is deaf.

- **`SetRecords`** stops building the flat address slice and calls
  `syncLink(l, r.Agent.GossipAddrs())`: keep measurements for surviving
  addresses, drop the rest, append new ones in registry order, `cur = -1` if
  the current path vanished. A balancer builds no outgoing links.
- **`sendHeartbeat`** keeps its single encode and signature, then under the
  lock collects `[]sendJob{peer, addr}` from **both `d.peers` and
  `d.balancers`** — exactly one per link with `cur >= 0`, its chosen path —
  releases the lock and sends. Same syscall count as today's `Broadcast`.
  `ErrNoRoute` marks that path and the next tick re-chooses. A path switch
  nudges `hbChanged` so the next heartbeat leaves now rather than at the next
  keepalive — a trigger, not a branch inside the send path.
- **`handle`** decodes outside the lock, does its work in a `handleLocked` that
  returns an optional reply, and sends **after** releasing the mutex. Heartbeat
  handling is unchanged except that a datagram from a known path address
  refreshes `lastSeen` and clears `fails`. A ping is answered with a pong (same
  id/pubkey checks as a heartbeat; no rate limit — only authenticated peers
  get one) carrying `HeardMS`. A pong matches its outstanding nonce and updates
  the EWMA. **Either** probe type from a peer whose `HeardMS` is older than
  `3*KeepAlive` (three lost in a row, matching `PathFailures`), while we have
  been sending on the current path longer than that, sets `deaf` on that path — it reports `Alive=false,
  Deaf=true` and `Choose` switches with reason `deaf` — without waiting for a
  loss. A pong on that path clears the flag. That is the asymmetric case, and
  it is caught at the receiver's `SuspectAfter` plus one RTT, because the
  receiver's silence-triggered ping is the message. Two details settled in
  implementation: `HeardMS` counts **heartbeats** only, not pings — our own
  periodic pings on the other path would otherwise keep refreshing it and
  mask the break; and a **negative** `HeardMS` never marks a path deaf,
  because a restarted peer legitimately reports it on its first pings and a
  path that never worked is caught by loss counting anyway.
- **`checkPaths`** joins `probeSilentPeers`/`checkExternals` on the existing 1 s
  tick, collecting pings under the lock and sending outside it (no goroutine
  per ping — a UDP write is microseconds). One cadence rule: ping a path when
  `now - pingedAt >= interval`, where interval is `PathProbeInterval` if its
  last ping was answered and `2*PathTimeout` otherwise (never measured, or
  lost). The silence trigger is new state on the existing loop:
  `probeSilentPeers`, on finding a peer silent, also zeroes `pingedAt` on every
  path of that link, so the pings go out on the same tick as the ladder's
  first direct probe. An unanswered ping past `PathTimeout` counts a loss;
  `domain.PathFailures` losses is dead. `noRoute` paths are pinged on the same
  cadence: a successful `Send` clears the flag, a pong makes the path alive.
  Then `PathPolicy.Choose`; on a change count `loadb_path_switches{reason}` and
  log `"mesh path"` with peer, addr, was, rtt_ms, reason.
- **The existing ladder is untouched underneath**: `SuspectAfter` still starts
  direct `/v2/status` probes, `DownAfter` still decides offline, `Snapshot`
  still reports `heartbeat|probe|none`. Failover does **not** beat the ladder
  to its first probe — nothing can, since the only signal that a heartbeat
  path died is the receiver's silence, and the ladder fires on that same
  `SuspectAfter` tick. What failover does is end the probing: heartbeats are
  back well before `DownAfter`, `Snapshot` returns to `heartbeat`, and
  `probeSilentPeers` stops. The chain, for A→B on path *p*:
  1. *p* stops delivering A's heartbeats (either direction of *p* may be the
     one that died; B's own heartbeats to A may well still use *p* the other
     way). B has heard nothing from A for `SuspectAfter`.
  2. Same tick: B's ladder sends one `/v2/status`, and `probeSilentPeers`
     zeroes `pingedAt` on every path to A, so `checkPaths` pings them all,
     each ping carrying `HeardMS > SuspectAfter ≥ 3*KeepAlive` (the default
     `SuspectAfter` is 15 s for exactly this reason; a config that sets it
     lower gets a warning).
  3. Any of those pings that reaches A (over *q*, or over *p* if only A→B
     died) marks A's path *p* deaf; A switches to *q* with reason `deaf` and
     nudges a heartbeat. Total: `SuspectAfter + RTT` from the break, not the
     `≈12 s` an earlier draft claimed.
  4. Loss counting is the fallback for when no ping arrives — the peer is down
     outright, or the candidate was never current — and it is slow by design:
     silence at `SuspectAfter`, then a ping every `2*PathTimeout` until
     `PathFailures` go unanswered, ≈ `SuspectAfter + 10 s` with the defaults.
     A dead path that is nobody's current path is found by the periodic
     `PathProbeInterval` ping and merely stops being a candidate.
  Duplicate heartbeats during a switch are dropped by the existing `Seq`
  staleness rule.
- **Visibility** without touching `domain.Upstream`: `Directory.LinkSnapshot()
  []LinkStatus` (chosen path, and per candidate the RTT, alive, no_route,
  last-pong age, and last-seen age — the newest authenticated datagram from
  that address, which is the inbound direction: the address the peer's
  heartbeats arrive on stays fresh), exposed as a `links` key in
  `/loadb/status`. Metrics
  `loadb_path_rtt_ms{peer,addr}`, `loadb_path_losses{peer}`,
  `loadb_path_switches{reason}`.

## Order of work

1. `internal/ports` (port + `ErrNoRoute`) — compile errors drive the rest.
2. `internal/domain`: `AgentInfo`, `Probe`/`Envelope`/`DecodeMessage`, new
   `path.go`.
3. `internal/config`: mesh `Addrs`, `resolvePeers`, path options, warnings.
4. `internal/adapters/gossipudp`, then `internal/adapters/gossipmem`.
5. A recording fake `ports.Gossip` for `internal/app` tests — none exists in
   the repo today: `Send` appends `{addr, payload}`, `Receive` is an injectable
   channel, an `unroutable` set returns `ErrNoRoute`.
6. `internal/app/directory.go` (the bulk), `internal/app/router.go` (`links`).
7. `internal/agent/{agent,wire}.go`, `cmd/algod-loadb-mesh/main.go`
   (`-agent` takes a comma-separated list, `config check` prints both lists,
   `listen` line shape preserved for `setup.sh`).
8. `internal/devfleet`: `Options.Nets`, `NodeSpec.Nets`, and
   `Options.PathProbeInterval`/`PathTimeout` scaled like the rest of its
   timings (defaults 2 s and 200 ms next to `SuspectAfter` 1 s, `KeepAlive`
   300 ms); net 0 keeps the `"mem:<id>"` spelling so `Partition("mem:plain", …)`
   and the hard-coded `"mem:devn"` in `sim_test.go:368` keep working.
9. `deploy/`, `README.md`. 10. Tests.

**Concurrency rules** (each is a live hazard): never call `gossip.Send` while
holding `d.mu` — `handle` loses its blanket `defer d.mu.Unlock()`, and both
`sendHeartbeat` and `checkPaths` use the collect-then-send shape
`probeSilentPeers` already demonstrates; all path state lives under `d.mu`;
every timestamp comes from `d.clock` so the fake clock drives the machine;
`gossipudp` needs the `WaitGroup` because N receive loops must not each
`close(t.recv)`, and `Close` must tolerate a second call.

## Verification

- `make lint`, `go build ./...`, `go test ./internal/... ./deploy/...`,
  `go test ./test/sim/`. (`TestBalancerWaitForBlockAfterWaitsForHeartbeat` is a
  known timing flake under load — re-run it alone before believing a failure.)
- **New `internal/adapters/gossipudp/udp_test.go`** — the package has none.
  Both sockets feed one channel; a receiver bound only on `127.0.0.2` sees
  `From` with that IP (exercises `pick` end to end); unparseable and unroutable
  addresses return `ErrNoRoute`; oversize payload refused; `Close` twice does
  not panic. Skip when `127.0.0.2` cannot be bound — nothing in the repo has
  bound a loopback alias yet, so this is the first time it is tried.
- **`internal/domain`** — `TestProbeCodec`, `TestDecodeMessageDiscriminates` (a
  ping must not decode as a heartbeat), `TestAgentInfoGossipAddrs`,
  `TestStaticEqualAddrs`, `TestPathPolicyChoose` (initial, margin not met,
  one faster probe does not switch and two do, dead switches regardless, all
  dead → -1). `TestHeartbeatCodec` and both record-codec tests stay untouched —
  a goal.
- **New `internal/app/directory_test.go`** with `clock.Fake` and the fake
  `Gossip` from step 5: heartbeats go only to the chosen path; a ping is
  answered with the same nonce and a sane `HeardMS`; the current path stops
  answering and no probe arrives → switch with reason `dead` once
  `PathFailures` pings have gone unanswered; a 10% faster
  challenger does not win, a 3× faster one wins after the cooldown; an
  incoming **ping** with stale `HeardMS` switches at once with reason `deaf`
  and a pong on that path clears the flag (the asymmetric case); a switch is
  followed by a heartbeat before the next keepalive; `ErrNoRoute` excludes a candidate and a later
  successful `Send` readmits it; an IPv4-mapped `From` still matches its
  candidate; a balancer answers pings and sends nothing.
- **`test/sim`** — `TestMeshPathFailover`: three nodes on two nets, partition
  one node's net 0, wait for `loadb_path_switches{reason="deaf"}` to move and
  every peer to be back at `Source == "heartbeat"` and `HealthSynced`, then
  assert that `/v2/status` hits **stop growing** over a window of several
  `ProbeInterval`s and that nobody reached `offline` — the mirror of
  `TestSilentAgentDegradedProbing`, where the hits keep climbing. It does not
  assert zero hits: the ladder's first probe legitimately fires on the same
  tick as detection (§7). The single-net fleet of the existing test is kept. `TestMixedTopology`: A on net 0, B on net 1,
  C on both; A and B fall back to probing each other while C keeps heartbeats
  with both, and `links` shows one `no_route` candidate.
- **`internal/devfleet/propagation_test.go`** is the budget: run it before and
  after and compare the logged worst case; no signing, locking or probing may
  enter the heartbeat path, and the first heartbeat must never wait for an RTT.
- **`deploy/deploy_test.go`** — the autoconfig tests move to
  `c.Mesh.Advertise.String()` and additionally assert both mesh lists are the
  two WireGuard addresses with the public one excluded.
- **Manual**: two agents on loopback aliases with
  `mesh.listen: [127.0.0.1:4001, 127.0.0.2:4001]` advertising both against a
  static registry; `/loadb/status` shows the chosen path and an RTT; drop one
  path with `iptables -A INPUT -p udp -d 127.0.0.2 --dport 4001 -j DROP` and
  confirm `loadb_path_switches` moves while the peer never leaves `synced`.
  `config check` on a public mesh address prints the warning.

## Risks and rejected alternatives

- **Asymmetric paths are allowed** — each side picks its own direction, so A→B
  may use net 1 while B→A uses net 2. Correct for UDP and cheaper than
  negotiating; the `HeardMS` echo covers a one-way death. Worth a comment so
  nobody "fixes" it.
- **A wildcard `mesh.listen` weakens path attribution**: the source becomes
  route-chosen, and `pick` can no longer reject a destination that would leave
  via the public interface. Degrades to today's behaviour, never worse.
- **Stale advertised addresses** cost one dead candidate until `PathFailures`
  losses. **Metric cardinality** is fleet-sized; drop the `addr` label if it
  bites. **Pong amplification** is a non-issue: same size as the ping, and
  only a peer that passed the id/pubkey check gets one.
- Rejected: fan-out (every heartbeat to every advertised address, dedup by
  `Seq`) — it would delete §3, §4 and most of §7 for 2× datagrams of ~400 B,
  but the settled requirement is one path per link; bumping `recordVersion`
  (fleet-wide blackout while records are unreadable); dropping `Addr` outright
  (hand-written static YAML would fail strict decoding with a poor message);
  keeping `Broadcast` with a transport-side path table (policy in the adapter,
  duplicated state); per-recipient signed heartbeats (N signatures inside the
  propagation budget, for what the probe already carries); a kind byte in the
  envelope (more churn than the `"t"` field for the same result);
  `SO_BINDTODEVICE`/raw ICMP for path selection (privileges, when `DialUDP`'s
  route lookup answers the same question portably); inferring RTT from
  heartbeat arrival times (no request/response, unsynchronised clocks); a
  "shotgun" burst on every candidate after a switch (a hedge on a hedge, and a
  branch inside the heartbeat send path); a per-source pong rate limit (state
  to defend against your own fleet); separate `path_failures`,
  `path_switch_margin` and `path_switch_cooldown` options (knobs nobody turns).
