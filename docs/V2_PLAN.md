# algod-loadb-mesh v2 — design and delivery plan

Status: accepted 2026-09-13; phases 0–3 implemented the same day in Go under
`cmd/`, `internal/` and `test/` (see README). The TypeScript v1 stays in
the `algod-loadb` repository (`src/`) until the fleet is migrated. Not yet done from this plan: the iroh
transport (phase 5), contract tests against a real algod, and packaging
beyond the systemd unit in `deploy/`.

Implementation notes that differ from the text below:

- Records are JSON inside AES-256-GCM (key from HKDF of the sync seed, box
  name as associated data) rather than msgpack + XChaCha20; the on-chain box
  format is versioned so this can change.
- Agent heartbeat keys are derived from the sync seed and the node id, so no
  extra key file is needed; static/shared-secret fleets derive them from
  `mesh.shared_secret`.
- Heartbeats carry the full capability set (about 300 bytes) instead of a
  hash; simpler and still far below one datagram.
- Health is judged by the receiving agent (`SyncHealth` + `ReturnHysteresis`
  in the domain); the monitor only reports facts about the local node.
- The contract is an ARC-4 application in an AlgoKit project (`contract/`,
  Algorand TypeScript); the Go client embeds its compiled TEAL, bytecode and
  ARC-56 spec, checked against the build output in the test suite. `registry init` uses algod's
  own compiler when the node's developer API is on.
- An unknown heartbeat sender triggers a rate-limited registry refresh, so
  new nodes join within seconds rather than one refresh period.
- Sync judgement is time-aware: nodes receive a block some hundreds of
  milliseconds apart, so being one round behind only counts as `Lagging`
  after `routing.lag_grace` (default 1.5 s); two or more rounds behind is
  lagging at once. Return hysteresis counts new synced rounds, not
  observations. Both live in `domain.SyncJudge`.

## 1. What v1 does today

v1 is a single Express/Bun process that sits in front of N algod nodes and
plays reverse proxy. Everything lives in `src/services/manager.ts` and
`src/services/backend.ts` (~800 lines together).

Per backend (`Backend` class):

- **Startup probes**, each with unbounded exponential-backoff retry:
  `/v2/status` (online + last round), `/v2/blocks/0` (archival: 200 means
  full archival, a 404/500 whose message contains "failed" and "ledger" means
  non-archival), `/genesis` (network), `POST /v2/teal/disassemble` (developer
  API enabled).
- **Partial archival** is manual config: `partialArchival: { since }` or
  `{ trail }`. There is no automatic detection.
- **Round tracking** is one long-poll loop per backend on
  `/v2/status/wait-for-block-after/{lastRound}` with a 20 s client timeout.
  This is the cheapest possible way to follow a node and must stay.
- **Multiple hosts per backend** (two WireGuard interfaces); the active host
  rotates on failure.
- **wait-for-block-after fan-in**: client waiters register on the local loop,
  so any number of clients waiting on a round cost zero extra algod requests.

Routing (`Manager` class):

- "Synced" means `lastRound == max(lastRound)` across backends. Tolerance 0.
- `/v2/blocks/{r}` with `latest - r > 998` needs an archival backend whose
  window covers `r`; `/v2/teal/*` needs a developer backend. Everything else
  picks a random synced backend; POSTs round-robin.
- `/v2/transactions/pending/{txid}` is tried sequentially across backends
  ordered by round (a pending txn is only known where it was submitted).
- Optional `multiBroadcast` sends `POST /v2/transactions` to every distinct
  node and answers with the first success.
- Graceful shutdown drains in-flight requests up to `maxShutdownWait`.
- Client auth is one shared `X-Algo-API-Token`; per-backend tokens are
  forwarded.

What holds it back:

- **Single point of failure** and a single hop for every request. A
  `client-config-mainnet.ts` shows the workaround: a second loadb layered in
  front of two loadb instances.
- **Config is TypeScript with node tokens committed to git**; changing the
  fleet is a code change and a restart.
- **No tests.** `test.sh` starts the testnet instance. The manager is a
  process-wide singleton, the backend does I/O in its constructor's start
  path, and selection logic is interleaved with logging, which makes unit
  tests impractical.
- Capability detection is heuristic (string-matching error messages,
  `/v2/blocks/0`), and the `998` cutoff is a guess at algod's non-archival
  window rather than the node's real oldest block.
- No latency or error-rate tracking, so no basis for performance routing;
  no retry on a failed GET; no circuit breaker.
- Bodies are fully buffered (`arraybuffer`) both ways. Blocks can be MBs.
- No status, health or metrics endpoint for the balancer itself (a TODO
  since 2022).
- Ageing dependencies: axios 0.26, express 4.16, Bun-specific 408 issues.

Worth carrying into v2 unchanged in spirit: the status-after loop,
wait-for-block coalescing, sync tolerance 0, capability-aware routing,
POST round-robin, sequential pending lookups, multi-endpoint nodes, drain on
shutdown, and the `X-Algod-Loadb-Backend` response header.

## 2. v2 goals and non-goals

Goals:

1. **Mesh, not hub.** One agent per algod host. No central process is
   required for the fleet to work. Any agent can lose any other agent.
2. **Direct data plane.** Client requests go from an agent straight to the
   chosen algod's REST endpoint, never through the remote agent. The
   control plane (node state, discovery) is agent-to-agent.
3. **Two routing modes**: `fallback` (local node unless it cannot serve) and
   `loadbalancer` (score by latency, errors and sync lag).
4. **Tiers**, with static external upstreams (Nodely etc.) as a last tier.
5. **Discovery via an Algorand application** holding encrypted node records
   in boxes, decrypted with one shared "sync" key.
6. **Lighter on algod than v1**: still exactly one status-after long-poll
   per node, no cross-node polling at all.
7. **Automatic capability reporting** from the node's own data directory
   (archival window, developer API, follow mode, versions), verified by a
   one-off empirical probe.
8. **Clean architecture and testability**: a pure domain core, ports and
   adapters, an in-process fake algod, and a deterministic multi-agent
   simulator.

Non-goals for v2.0: NAT hole punching (everything is on WireGuard); separate
per-agent registry keys or RBAC; an indexer; TLS termination; any admin
(`/v2/participation`) API proxying.

## 3. Architecture

```
  host A                                  host B
  +-------------------------------+       +-------------------------------+
  | algod  <--- status-after ---  |       |  --- status-after --->  algod |
  |   ^         loadb-agent       |       |       loadb-agent         ^   |
  |   |          |   |   ^        |       |        ^   |   |          |   |
  |   |  clients |   |   | gossip (heartbeats, wg) |   |   | clients  |   |
  |   |          |   |   +------------------------>+   |   |          |   |
  |   +----------+   |   direct REST (wg)              |   +----------+   |
  |                  +---------------------------------|----------------->|
  +-------------------------------+       +-------------------------------+
                  \                                   /
                   \   read/write boxes (via local algod)
                    v                                v
                 [ Algorand app: encrypted node registry ]

                 [ external tier: nodely (static, no agent) ]
```

Each agent has four cooperating services and one pure core:

| Component | Responsibility | Talks to |
|---|---|---|
| `LocalMonitor` | Follows the co-located algod: status-after loop, health state machine, capability detection. Produces the local `NodeRecord`. | local algod REST, data dir |
| `PeerDirectory` | Merges the static registry (who exists, where, with what token) with live gossip (what round, healthy or not) into a `PeerTable`. | registry, gossip |
| `Router` | Classifies each client request, filters eligible upstreams, selects one per mode and tier, forwards, records outcome. | any algod REST, external upstreams |
| `RegistrySync` | Reads boxes on a timer, decrypts, validates; writes/updates this node's own box when its static record changes. | local algod (or any reachable algod) |
| domain core | Pure types and functions used by all of the above. | nothing |

There is no central mode. Every agent has a local node; migration is done
host by host, and a v2 agent can point at the v1 hub as an external tier
while the fleet is mixed.

## 4. Domain model

```
NodeRecord                          (static; lives in the registry, encrypted)
  id            string              short stable name ("k44")
  network       string              genesis-id ("mainnet-v1.0")
  endpoints     []URL               algod REST base URLs, preferred order
  token         string              algod API token for direct access
  agent         { addr, pubkey }    gossip address and signing key
  tier          int                 default 1
  tags          []string
  declared      Capabilities        optional manual overrides
  version       int, updated_at round

Capabilities                        (dynamic; derived by LocalMonitor, gossiped)
  oldest_round  uint64              earliest block the node can serve
  archival      full | trailing(n) | since(r) | none    (how oldest_round evolves)
  developer_api bool
  follow_mode   bool                cannot broadcast transactions
  experimental_api bool
  algod_version string, genesis_id, storage_engine
  max_acct_lookback int

Heartbeat                           (gossiped every round or on change, signed)
  node_id, seq, last_round, round_seen_at (monotonic), health, caps_hash

Health                              (state machine in LocalMonitor)
  Starting -> Online -> Synced <-> Lagging -> Offline
  with hysteresis: Lagging->Synced only after N consecutive synced rounds

RequestClass                        (from classify(method, path, query))
  round        *uint64              /v2/blocks/{r}, /v2/deltas/{r}, /v2/stateproofs/{r}, ...
  needs_dev    bool                 /v2/teal/*
  broadcast    bool                 POST /v2/transactions
  pending_id   *txid                /v2/transactions/pending/{id}
  wait_after   *uint64              /v2/status/wait-for-block-after/{r}
  local_only   bool                 /health, /metrics, /v2/transactions/pending (pool)
  idempotent   bool                 retry policy input

Upstream                            (what the Router chooses between)
  node *NodeRecord | external *ExternalUpstream
  live  Heartbeat | probe result
  stats EWMA latency, error window, inflight, breaker state
```

`oldest_round` is the universal representation of archival-ness. A request
for round `r` is servable iff `oldest_round <= r <= last_round`. Full
archival is `oldest_round = 0`; a trailing node's `oldest_round` advances
with `last_round`; a "since" node's is fixed. This replaces v1's three-way
config and the `998` heuristic.

## 5. Local node monitoring and capability reporting

The agent runs on the node, so it can read what the API never exposes.

**Discovery of the local node**: given `data_dir`, read `algod.net`
(listen address), `algod.token`, `genesis.json` (network), and
`config.json`. Zero manual endpoint config for the local node.

**From `config.json`** (using the node's own config schema, with defaults
applied for keys that are absent):

| Key | Reported as |
|---|---|
| `Archival` | `archival = full`, `oldest_round = 0` |
| `MaxBlockHistoryLookback` (v31, default 0) | `archival = trailing(n)`, `oldest_round = last_round - n` |
| `EnableDeveloperAPI` | `developer_api` |
| `EnableFollowMode` | `follow_mode` (excluded from broadcast routing) |
| `EnableExperimentalAPI` | `experimental_api` |
| `MaxAcctLookback` | `max_acct_lookback` |
| `StorageEngine`, `CatchpointInterval` | informational |
| `RestWriteTimeoutSeconds` | upper bound for upstream timeouts |

**Empirical verification**, because config can lie (a node archival since
round X, a lookback that was raised after the blocks were pruned, a
pending catchup):

- On startup and whenever `config.json`'s mtime changes: binary-search the
  oldest servable round with `GET /v2/blocks/{r}?format=msgpack` treated as
  a boolean. About 26 requests for a 50M-round chain, once.
- Thereafter maintain arithmetically and re-verify hourly with two requests
  (`oldest` and `oldest - 1`). If reality disagrees with config, reality wins
  and a warning is logged.
- `/versions` and `/genesis` once at startup and after any restart of algod
  (detected by the status loop's reconnect).

**Manual overrides** in config (`local.overrides`) win over both, for the
cases we have not thought of.

**Round tracking** is unchanged from v1: one
`/v2/status/wait-for-block-after/{last}` long-poll with a 20 s client
timeout, falling back to `/v2/status` on error with exponential backoff. The
`Lagging` state is entered when the local round is behind the best gossiped
round by more than `sync_tolerance` (default 0), exactly v1's rule but
evaluated against gossip instead of local polling of peers.

Everything the monitor learns is published as `Capabilities` in the
heartbeat, so remote agents route by facts instead of config.

## 6. Peer discovery: on-chain registry

**Contract**: one application per fleet. Global state holds a schema
version. Boxes hold one `NodeRecord` each:

- key: `n` || 16-byte keyed hash of the id, so ids stay private
- value: `version || nonce || AEAD(ciphertext of msgpack(NodeRecord))`,
  a few hundred bytes (limit 32 KB)
- approval program: only the creator address may create, replace or delete
  boxes. Nothing else. The program is trivially small and auditable.

**Sync key**: config holds `app_id` plus the creator account's key (25-word
mnemonic or raw 64 bytes). A symmetric key is derived from it with HKDF and
a fixed context string; box values are sealed with XChaCha20-Poly1305 (or
AES-GCM). One key for the fleet, as requested; every agent can read and
write. Records are additionally signed by the writing agent's own key so
tampering by anything other than a key holder is detectable.

**Reading**: `GET /v2/applications/{app}/boxes` then
`GET /v2/applications/{app}/box?name=...` against the local algod. These
serve current state and work on any synced node; no indexer, no archival
requirement. Polled every `registry.refresh` (default 5 min) and on demand
when a heartbeat carries a newer `registry_round`. Cost: about one request
per node per five minutes.

**Writing**: only when this node's static record changes (first boot,
endpoint or token change) or from the CLI. One app-call transaction with a
box reference, signed with the sync key, submitted through the local algod.
Fee 0.001 ALGO; box minimum balance about 0.2 ALGO per record, refundable on
delete.

**Cache**: the decrypted registry is persisted to `registry.cache` so an
agent boots and routes even when its local algod is down; it can then
refresh through any reachable peer's algod, since it holds their tokens.

**CLI**: `algod-loadb-mesh registry init | list | add | rm | rotate-token`. `rm`
is the only way a dead node leaves the fleet; liveness is gossip's job.

**Network binding**: records carry `network`; an agent only peers with
records matching its local node's genesis. The registry app may live on a
different network than the nodes it describes (`registry.algod` may point
anywhere).

## 7. Live state: gossip

The one rule that keeps algod load flat: **an agent only ever long-polls its
own node**. Everything a remote agent knows about that node arrives in a
heartbeat.

- One `Heartbeat` per new round (about every 2.8 s) or on health change,
  roughly 150 bytes, signed, to every peer. For 10 nodes this is under
  1 KB/s per host.
- Transport for v2.0: UDP datagrams over WireGuard, addresses from the
  registry. A peer is `Suspect` after `suspect_after` (default 3 rounds
  without a heartbeat) and `Down` after 10.
- **Degraded probing**: when a peer's agent is silent but its algod might be
  fine, the local agent may probe that algod's `/v2/status` directly, at
  most every 10 s, until heartbeats resume. This is the only case where an
  agent touches a remote algod for control-plane reasons.
- Static external upstreams have no agent: one `/v2/status` per
  `health_check` interval (default 60 s), plus passive outcome tracking.

Heartbeats use sequence numbers and receiver-side monotonic time; wall clocks
are never compared across hosts.

## 8. Routing

The pipeline is the same in both modes; only `select` differs.

```
classify(req) -> class
candidates = tiers in order: [local], [mesh peers], [external]
for tier in tiers:
    elig = eligible(tier, class, now)       # pure
    if elig not empty: upstream = select(mode, elig, stats); break
forward(upstream, req) -> outcome; stats.record(upstream, class, outcome)
```

**Eligibility** (pure function, table-tested): health is `Synced` (or
`Online` when the class is `local_only`), `oldest_round <= class.round`,
`developer_api` when `needs_dev`, not `follow_mode` when `broadcast`,
breaker not open, not draining.

**`fallback` mode**: tier 0 is the local node alone. It is selected
whenever eligible. Return to local after an outage only after
`return_hysteresis` consecutive synced rounds, to avoid flapping. Remote
peers are ordered by tier then score.

**`loadbalancer` mode**: tier 0 and the mesh tier merge. Score =
weighted latency EWMA (per class, since a block fetch and a status call
differ by 10x) + error-rate penalty + sync-lag penalty + inflight; the local
node gets a fixed bonus for the free network hop. Selection is
power-of-two-choices on score, which spreads load without herd effects and
is trivial to test.

**Tiers**: a tier is only consulted when every higher tier has no eligible
upstream. Tier is a registry field with a local `peer_overrides` map. The
external tier is static config: URL, optional token, declared capabilities,
`max_rps`, and 429 handling (back off, mark `Throttled` for a cooldown).

**Special routes**, all carried over from v1:

- `wait-for-block-after/{r}`: coalesced on the local monitor's loop when
  the local node is `Synced`; otherwise forwarded to the best peer as a
  normal request. Stale rounds answer immediately from the last status.
- `POST /v2/transactions`: round-robin among eligible non-follower nodes;
  never auto-retried after bytes were sent; `multi_broadcast` optional. The
  txid is remembered for `pending_ttl` (default 60 s) so that
  `pending/{txid}` goes first to the node that received it, then sequentially
  to the rest by round.
- `/v2/transactions/pending` (pool dump), `/health`, `/metrics`, `/ready`:
  local only.

**Forwarding**: streaming in both directions, no buffering, msgpack
untouched. Idempotent GETs retry once on connect error or 502/503/504 on the
next eligible upstream, within `retry_budget`. Per-upstream circuit breaker
(closed / open / half-open). Response headers `X-Algod-Loadb-Mesh-Upstream`,
`X-Algod-Loadb-Mesh-Mode`, `X-Algod-Loadb-Mesh-Tier`.

**Agent endpoints**: `/loadb/status` (JSON: mode, local record, peer table,
scores), `/loadb/health`, `/loadb/metrics` (Prometheus), `/loadb/peers`.

## 9. Transport and iroh assessment

Facts as of September 2026: iroh 1.0 shipped on 2026-06-15 with a
wire-compatibility promise across 1.x, QUIC-native NAT traversal (about 90%
direct-connection success in n0's fleet), stateless encrypted relays that
can be self-hosted, `iroh-gossip` for pub/sub overlays, and custom protocols
by ALPN. Official bindings are Rust, Python, Node.js, Swift and Kotlin. Go
bindings exist only as community FFI projects.

What iroh would give this project:

- Stable node identity (the key is the address), authenticated encrypted
  agent-to-agent channels without depending on WireGuard for the control
  plane, a ready-made gossip overlay, and a clean path to the optional
  "route via remote agent" tunnel for hosts outside the VPN.

What it costs:

- A large Rust dependency (tens of MB of binary, a background relay
  connection per endpoint, more RSS than a UDP socket), language coupling
  (native in Rust, FFI elsewhere), and a public relay dependency unless one
  is self-hosted, even though everything is already reachable over
  WireGuard today.

Verdict: **suitable, not necessary for v2.0**. Design the control plane
behind a `Gossip` port with three adapters: `inmem` (tests), `udp` (v2.0,
WireGuard), `iroh` (v2.x, feature-flagged). If iroh is wanted with a
non-Rust agent, run it as a small Rust sidecar exposing a Unix socket rather
than through FFI.

## 10. Language decision

| Criterion | Go | Rust | TypeScript (Bun) |
|---|---|---|---|
| Reverse proxy quality | `net/http` + `httputil.ReverseProxy`, streaming, pooled, battle-tested | hyper/axum, excellent, more assembly | v1 pain: buffering, 408/Bun issues, needs care |
| Algorand SDK | first-party `go-algorand-sdk`; can import go-algorand's own `config` package to parse `config.json` with correct per-version defaults | community `algonaut`, thin; txn encoding largely hand-rolled | `algosdk`, best maintained JS SDK |
| Footprint | ~15-30 MB RSS, single static binary | lowest, single binary | 60-150 MB RSS; `bun build --compile` |
| iroh | community FFI only; sidecar advisable | native | official `@number0/iroh` (Node; Bun compatibility unverified) |
| Testability | interfaces, `httptest`, table tests, race detector | traits, mocks, good | fine with `bun test`/vitest |
| Continuity with v1 | none | none | full |

Recommendation: **Go**. The two things this project is mostly made of, an
HTTP proxy and an algod client, are strongest there, the binary is small
enough to sit next to algod on every host, and reusing go-algorand's config
schema makes the capability reporting in section 5 exact rather than
approximate. Choose Rust instead only if iroh and NAT traversal become a
v2.0 requirement. TypeScript is the continuity option, but v1's proxy
troubles argue against it for a component that runs on every node.

## 11. Clean architecture and testing

Hexagonal layout; dependencies point inward only. Shown with Go paths, the
layering is language-neutral.

```
cmd/algod-loadb-mesh/            composition root, CLI (run, registry, status, check-node)
internal/domain/            PURE. types, classify, eligible, select, score,
                            health state machine, hysteresis, registry codec.
                            Zero I/O, zero imports outside stdlib.
internal/app/               services orchestrating domain through ports:
                            monitor, directory, router, registrysync, drain
internal/ports/             interfaces: AlgodClient, NodeConfigReader, Registry,
                            Gossip, Forwarder, Clock, Metrics, Rand
internal/adapters/
    algod/http              AlgodClient over REST
    datadir/                NodeConfigReader (algod.net, algod.token, config.json)
    registry/algorand       boxes + app-call txns via SDK
    registry/file           cache, and a fake for tests
    gossip/udp              v2.0 transport
    gossip/inmem            tests and the simulator
    gossip/iroh             later, feature-flagged
    proxy/                  Forwarder over httputil.ReverseProxy
    metrics/prometheus, clock/real
internal/fakealgod/         in-process HTTP algod simulator (see below)
test/sim/                   multi-agent scenarios: N fake algods + N agents,
                            in-memory gossip, fake clock, no sockets
test/contract/              optional: real algod (algokit localnet), tagged
```

Rules that keep it testable:

- **Domain is pure.** `classify`, `eligible`, `select`, `score`, the health
  machine and the registry codec take values and return values. Time and
  randomness are parameters. Property tests: selection never returns an
  ineligible upstream; fallback returns local whenever local is eligible;
  hysteresis never flaps within N rounds.
- **Services take ports in the constructor**, never construct adapters, and
  never touch `time.Now` or sockets. Every service test uses fakes plus a
  fake clock, runs in milliseconds and is deterministic.
- **Adapters are thin** and tested against `httptest` servers or the fake
  algod, with golden files for wire formats (registry box bytes, heartbeat
  encoding) so the format cannot drift silently.
- **One composition root** wires everything; nothing else knows concrete
  types.

`fakealgod` is the keystone: a small HTTP server that implements the subset
of the algod API the agent uses, with scripted behaviour: rounds advance on
a fake clock, a configurable `oldest_round`, developer API on or off,
follow mode, error injection (timeouts, 500s, wrong network, catchup lag),
and box storage for the registry adapter. It doubles as a local dev
environment (`algod-loadb-mesh dev --fake-nodes 3`).

Test pyramid, in order of volume: domain unit tests, service tests with
fakes, adapter tests with fakealgod, simulator scenarios ("k44 goes
Lagging for 20 rounds; all agents route away and return after
hysteresis"; "registry adds a tier-2 node; every agent sees it within one
refresh"; "external tier is used only when the mesh is empty"), and a
handful of contract tests against a real algod that run on demand.

## 12. Configuration sketch

Plain YAML file, secrets by file path or env, reloaded on SIGHUP. No
secrets in git.

```yaml
mode: fallback                      # fallback | loadbalancer
listen: 0.0.0.0:4000
client_token_file: /etc/algod-loadb-mesh/client.token

local:
  data_dir: /var/lib/algorand       # auto: algod.net, algod.token, config.json, genesis.json
  advertise_endpoints:              # what remote agents dial; must be listened on by algod
    - http://10.112.0.44:51616
    - http://10.114.0.44:51616
  tier: 1
  tags: [k44, dc-a]
  overrides: {}                     # e.g. archival: {oldest_round: 25000000}

registry:
  app_id: 123456789
  sync_key_file: /etc/algod-loadb-mesh/sync.key
  algod: local                      # or {url, token}
  refresh: 5m
  auto_register: true
  cache: /var/lib/algod-loadb-mesh/registry.cache

mesh:
  transport: udp                    # udp | iroh
  listen: 10.112.0.44:4001
  suspect_after_rounds: 3
  peer_overrides:
    bambi: {tier: 2}

tiers:
  external:
    - name: nodely
      url: https://mainnet-api.4160.nodely.dev
      capabilities: {archival: full, developer_api: false}
      health_check: 60s
      max_rps: 20

routing:
  sync_tolerance: 0
  return_hysteresis_rounds: 3
  multi_broadcast: false
  retry_budget: 1
  upstream_timeout: 60s
  wait_for_block_timeout: 20s
  pending_ttl: 60s
  drain_timeout: 10s
```

## 13. Resource budget

Per host, fleet of N nodes:

| Resource | v1 | v2 target |
|---|---|---|
| algod long-polls per node | 1 (from the hub) | 1 (from its own agent) |
| algod requests from other agents | 0 | 0 (except degraded probing, ≤ 0.1/s) |
| capability probing | 3 requests at start | ~30 at start, 2 per hour |
| registry reads | none | 1 per 5 min per agent |
| agent-to-agent traffic | none | (N-1) × ~150 B per round |
| agent RSS | 60-150 MB (Bun) | < 50 MB (Go) |
| idle CPU | low | ~0 |

## 14. Delivery phases

Effort is a rough estimate for one engineer.

| Phase | Deliverable | Est. |
|---|---|---|
| 0 | Repo skeleton, domain types, `classify`/`eligible`/`select` with tests, `fakealgod`. | 1-2 wk |
| 1 | **Node-local agent.** `LocalMonitor` with data-dir capability detection and empirical probe, streaming proxy, special routes, drain, UDP gossip, `PeerDirectory`, `fallback` mode with hysteresis, `/loadb/status`, simulator scenarios. | 3-4 wk |
| 2 | **Registry.** Contract, codec, sync key, `registry/algorand` adapter, cache, CLI, auto-register. | 1-2 wk |
| 3 | **Load balancing and tiers.** Stats, scoring, P2C, circuit breaker, external tier with rate limiting, metrics. | 1-2 wk |
| 4 | Packaging (systemd unit, tarball or deb), docs, contract tests against algokit localnet, rollout across the fleet. | 1 wk |
| 5 | Optional: `gossip/iroh` adapter and agent-tunnel data plane for hosts outside the VPN. | 1-2 wk |

Phase 1 is the first deployable agent; each later phase is independently
shippable behind config.

## 15. Risks and decisions

Decisions taken:

1. Language: **Go**; Rust only if iroh becomes a hard requirement.
2. Registry network: a config value (`registry.algod`), so a mainnet app can
   describe every fleet or a testnet app can keep mainnet spend at zero.
3. No central mode; the agent always has a local node.
4. Multiple algods on one host (e.g. mainnet + testnet): one agent process
   per algod, each with its own config file. A single process serving
   several networks is a later change to the composition root only.

Risks and how the design contains them:

- **Every host holds the registry admin key.** Accepted by requirement.
  Contained by: box writes are rare and logged, records are signed by the
  writing agent, the CLI can rotate the key, and per-agent keys can be a
  v3 change to the codec only.
- **Every agent holds every algod token.** Inherent to the direct data
  plane; exposure is bounded by WireGuard. algod must listen on the VPN
  address (`EndpointAddress`), which the agent verifies at registration.
- **Clock skew** cannot affect liveness: sequence numbers and receiver
  monotonic time only.
- **Registry unreachable** (local algod down): cached registry, then refresh
  through any peer's algod.
- **Silent capability drift** (config edited, blocks pruned): hourly
  re-verification, and reality overrides config.
- **Bun/TypeScript continuity** is lost with Go; mitigated by keeping v1's
  client-facing behaviour and headers so clients need no change.
