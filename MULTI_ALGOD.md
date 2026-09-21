# algod pass-through: expose one algod on every mesh interface

## Context

Hosts sit on two WireGuard nets, 10.112.\* and 10.114.\*. Since commit
`512d014` the agent binds and advertises every mesh interface for gossip and
each pair of agents settles on one heartbeat path. algod has not caught up:
`EndpointAddress` binds **one** address, so a node whose algod listens on
10.112.0.5:8080 is unreachable for a peer that only shares the 10.114 net.
Today `deploy/autoconfig.sh` merely warns about it (`autoconfig.sh:279-292`)
and advertises the single reachable endpoint; on some servers the operator
duplicates the port with iptables DNAT by hand.

The agent takes over that job: it binds algod's port on the mesh interfaces
algod does not listen on and passes those TCP connections straight through to
the address algod actually bound, read from `<data_dir>/algod.net`. And peers
start using the advertised endpoint that sits on the interface of their chosen
heartbeat path, so the new listeners are actually used and algod traffic fails
over with the path.

Settled with the user:

- **Explicit list, checked at runtime.** New `local.passthrough` list of
  `host:port`. At start the agent reads `algod.net` and skips, with a warning,
  every entry algod already covers (same host:port, or a wildcard bind on that
  port), so it never competes with algod for a socket. `config.json`'s
  `EndpointAddress` stays parsed-and-discarded (`datadir.go:44`): `algod.net`
  is the runtime truth. autoconfig keeps its `algod.net`-first,
  `config.json`-second guess (`autoconfig.sh:182-192`) for generating the
  config; the runtime check is what protects.
- **Raw TCP splice**, not an HTTP reverse proxy: peers already send the node's
  own algod token, and long-polls and streaming stay untouched.
- **Peers follow the path**: `Endpoints[0]` at `directory.go:679` and `:920`
  becomes "the endpoint whose IP is the current heartbeat path's IP, else
  `Endpoints[0]`".

Why not iptables: DNAT needs root / CAP_NET_ADMIN and persistence across
reboots, conntrack plus WireGuard cryptokey routing make the return path easy
to get wrong (the source-selection problem MULTI.md §5 solved for UDP, but in
the kernel and invisible), and nothing shows it in `/loadb/status` or a test.
A userspace splice terminates TCP locally so the reply leaves by the socket
that accepted it, costs one copy per direction on REST-sized traffic, runs as
the `algorand` user under the existing systemd hardening, and is testable.

Invariants from MULTI.md still hold: `recordVersion` stays 1, nothing enters
the heartbeat send path (`internal/devfleet/propagation_test.go`), no network
I/O under `d.mu`, every timestamp from `d.clock`, `KnownFields(true)` YAML.

## Design

### 1. Pure helpers — `internal/domain/endpoint.go` (new)

```go
// EndpointHosts returns, per advertised endpoint, its host normalised the way
// NormalizeAddr normalises (IPv4-mapped IPv6 → IPv4); "" for an endpoint
// whose host is not an IP (mem:x, hostnames). Computed once per record.
func EndpointHosts(endpoints []string) []string

// EndpointFor picks the endpoint whose precomputed host is the host of
// pathAddr (the link's current heartbeat path, already normalised), else
// endpoints[0]; "" when there are none. len(hosts) == len(endpoints).
func EndpointFor(endpoints, hosts []string, pathAddr string) string

// AlgodCovers reports whether an algod bound to algodNet (host:port verbatim
// from algod.net) already answers on addr, so the agent must not bind it.
func AlgodCovers(algodNet, addr string) bool
```

The parsing lives in `EndpointHosts` (`url.Parse` → `Hostname()` →
`net.ParseIP`/`To4()`, so `[::ffff:10.114.0.5]` matches `10.114.0.5`; reuse
the IPv4-mapped rule of `NormalizeAddr`, `internal/domain/path.go:85`), run
once when a record is synced (§6). `EndpointFor` is `net.SplitHostPort` on
`pathAddr` plus string compares: no `url.Parse`, no allocation, because it
runs under `d.mu` inside `Snapshot`, which the router calls on every request
(`router.go:208, 350, 386, 608`); today `Snapshot` parses nothing and that
must stay true.

Matching is by **host only**, on purpose: the path address carries the gossip
port, the endpoint the REST port. Consequence: a record advertising two
endpoints on one host (two REST ports) always gets the first one whatever the
path. Fine for the current data model; a unit test pins it so nobody "fixes"
it later.

`AlgodCovers`: ports must be equal, else false; `0.0.0.0` covers any IPv4
host; `::` and `""` cover both families (Go's `net.Listen("tcp", "[::]:p")`
is dual-stack on Linux, and go-algorand listens with `net.Listen("tcp", …)`);
a loopback algod covers only an equal address; otherwise equal normalised
hosts. Same spirit as `config.wildcardCovers` (`config.go:120`), which stays:
port handling differs.

### 2. NodeConfig — `internal/ports/ports.go`, `internal/adapters/datadir/datadir.go`, `internal/fakealgod`

`endpointURL` rewrites a wildcard to `127.0.0.1`, so `NodeConfig.Endpoint`
cannot tell `0.0.0.0:8080` from a loopback bind. Add one field,
`NodeConfig.NetAddr string // algod.net verbatim (host:port)`. `datadir.Read`
sets it where `Endpoint` is assigned (`datadir.go:57`):
`nc.NetAddr, nc.Endpoint = netAddr, endpointURL(netAddr)`;
`fakealgod.ConfigReader.Read` (`fakealgod.go:369`) sets it to the host:port
of the fake node's URL. `EndpointAddress` from `config.json` stays unused.

### 3. Config — `internal/config/config.go`

`Local` (`config.go:197`) gains `Passthrough Addrs \`yaml:"passthrough"\``
after `AdvertiseEndpoints`. In `Finish()`, next to the `mesh.advertise` block
(`config.go:445`):

- resolve with `resolvePeers` semantics (port required, wildcard rejected,
  trim, dedupe). `resolvePeers`'s wildcard error says "peers cannot send to a
  wildcard address", which is wrong here: give it a reason string (or a small
  wrapper) so the message reads "a wildcard would take the port from algod".
- `balancer && len(Passthrough) > 0` → error: a balancer has no algod.
- an entry equal to a `c.Listen` address (the TCP client listeners;
  `mesh.listen` is UDP and cannot collide), or whose **port equals** a
  `c.Listen` wildcard's port and whose host that wildcard covers → error: the
  agent would fight itself for the port. `wildcardCovers` (`config.go:120`)
  ignores the port on purpose and its callers compare ports first
  (`config.go:108-113`); this check does the same, so `listen: 0.0.0.0:4000`
  with `passthrough: 10.114.0.5:8080` passes.

`Warnings()` (`config.go:503`) gains two: a public passthrough address
(`Addrs.Public()`) "exposes algod to the internet"; a passthrough entry whose
host:port is in none of `local.advertise_endpoints` (compare via `url.Parse` +
`NormalizeAddr`) "peers will never use it". Warnings, not errors:
`advertise_endpoints` is legitimately empty with `auto_register: false`.
`Addrs.Public()` skips hostnames, so a pass-through given as a hostname that
resolves publicly does not warn; accepted, the entries are mesh IPs by
construction and autoconfig never writes a hostname.

`config check` (`cmd/algod-loadb-mesh/main.go:175-183`): node branch adds a
`passthrough %s` line via `orNone` **after** the `listen …, advertising …`
line, so `deploy/setup.sh`'s `sed -n 's/^listen \([^,]*\).*/\1/p'` still
matches.

Examples: `deploy/config.example.yaml` gets
`# passthrough: [10.114.0.44:8080]   # also bind these for algod and pass TCP straight through to it (algod binds one address; peers on the other mesh net use this); skipped with a warning when algod already listens there`
under `local:` (`TestExampleListsEveryOption` enforces it);
`config.static.example.yaml` gets the same line by hand; the balancer example
gets nothing.

### 4. Adapter — `internal/adapters/passthrough/passthrough.go` (new package)

Its own adapter: network I/O with a lifecycle and loopback tests, and adapters
cannot import `agent`, so the ten-line bind-all-or-close-all loop from
`agent.listenAll` is repeated rather than shared.

```go
// Server binds the pass-through addresses and splices each accepted TCP
// connection to the local algod. It knows nothing about HTTP or tokens.
type Server struct { /* lns []net.Listener; target func() string; log; metric; wg; mu; conns map[net.Conn]struct{}; active atomic.Int64 */ }

// Listen binds every address up front, closing what it opened if one fails.
func Listen(addrs []string, target func() string, log ports.Logger, metric ports.Metrics) (*Server, error)
// Serve accepts on every listener until ctx is done or an Accept fails for good.
func (s *Server) Serve(ctx context.Context) error
// Shutdown stops accepting, waits up to ctx for spliced connections, then closes them.
func (s *Server) Shutdown(ctx context.Context) error
// Close is Shutdown without a drain window, for a stop that does not drain.
func (s *Server) Close() error
func (s *Server) Addrs() []string // bound addresses, ln.Addr().String()
func (s *Server) Active() int64

// HostPort strips the scheme off a base URL (Monitor.State().Endpoint):
// url.Parse(...).Host, so IPv6 brackets survive and the result is dialable
// ("http://[::1]:8080" → "[::1]:8080", never "::1:8080").
func HostPort(baseURL string) string
```

`Serve` returns `nil` when ctx is cancelled, never `ctx.Err()`: it shares
`errc` with the HTTP listeners in `Agent.Serve`, whose select
(`agent.go:152-158`) treats any non-nil error as a failure and returns
**before** draining, closing the listeners or closing gossip. A
`context.Canceled` racing `case <-ctx.Done()` would skip the whole shutdown
sequence on SIGTERM. `Serve` is never built with zero listeners (§5).

Per connection: `Inc("loadb_passthrough_accepted", "addr", ln)`; `t :=
target()`; empty → `Inc("loadb_passthrough_dial_failures", "reason",
"no_target")`, close; `net.Dialer.DialContext` with a 5 s timeout, failure →
`reason=dial`, one debug log; then `active++` / `Gauge("loadb_passthrough_active")`,
two `io.Copy` goroutines, each followed by `CloseWrite()` on the side it
finished writing to (type-assert `interface{ CloseWrite() error }`, else
`Close`); wait for both; on teardown `active--`, re-`Gauge`, delete from
`conns`, `Close` both. Nothing logged per successful connection. `Accept`
errors: `net.Error` timeouts back off (5 ms doubling to 1 s), `net.ErrClosed`
returns `nil`, anything else returns the error.

`Shutdown(ctx)`: close every listener, **wait for the accept loops to end**
(a second `WaitGroup`, added to under the mutex so a `Serve` that starts
after `Shutdown` does nothing), and only then wait until the connection
`WaitGroup` drains or ctx is done. The order matters: an accept loop adds to
the connection group, and `WaitGroup` forbids an `Add` from zero while a
`Wait` is in progress; letting the loops end first puts every `Add` ahead of
the `Wait`. Then `Close` every conn in `conns` (that, not the deferred
`Close`, is what unblocks the two `io.Copy` goroutines of a connection the
client keeps open), cancel the dial context so a splice still dialing algod
gives up, and mark the cut so a pair tracked after it is closed on the spot
(a splice that was dialing when the cut ran is not in `conns` yet; without
this it outlives `Shutdown`, which then waits on it forever). Wait for the
group, log the number cut at Info when it is non-zero, return `nil`.
`Agent.Serve` keeps returning `srv.Shutdown`'s error as today; the
pass-through never contributes one. Dials run under the server's own context,
not `Serve`'s: a dial during the drain window still completes.

`target` is read at accept time, so the splice follows `Monitor.State().Endpoint`
as `configLoop` (`monitor.go:191-210`) re-reads `algod.net` every minute, and
`endpointURL`'s wildcard→`127.0.0.1` rewrite is exactly the right dial target.
`readConfig` runs first in `Monitor.Run` (`monitor.go:135`), so the target
exists before algod is even online.

### 5. Composition — `internal/agent/agent.go`

```go
// filterPassthrough drops the entries algod already binds, judged from
// algod.net now, and warns about each: bound anyway, they would take the
// port from an algod that restarts while the agent holds it.
func filterPassthrough(pt config.Addrs, algodNet string, log ports.Logger) config.Addrs
```

`Serve` (`agent.go:134`): after `listenAll(addrs)` and before any service
starts, when `len(c.Local.Passthrough) > 0`: `nc, err :=
a.deps.ConfigReader.Read()` (error fails `Serve`, the rule `FromConfig`
already applies: algod must be running); `pt := filterPassthrough(...)`.
**When `pt` is empty no `Server` is built and nothing is started**: a `Serve`
with no listeners that returned `nil` would land on `errc`, and the select at
`agent.go:152-158` takes a `nil` there as the signal to drain and exit, so
the agent would die at startup with every entry redundant. Otherwise
`ps, err := passthrough.Listen(pt, func() string { return
passthrough.HostPort(a.Monitor.State().Endpoint) }, log, metric)` — on error
return (the deferred `closeAll(lns)` closes the HTTP listeners). Run
`ps.Serve(ctx)` on the same `errc` as the HTTP listeners, whose capacity
becomes `2+len(lns)` (`agent.go:142` sizes it for exactly `a.Run` plus one
per listener; one producer short and a late send blocks forever, a goroutine
leak in tests); log `"passthrough"` with `addrs`, `skipped` and `target`
once. `ps` stays a local of `Serve` (no new `Agent` field: only `Shutdown`
and the status closure need it). `Router.SetPassthrough` (§6) is called
whenever `local.passthrough` is non-empty, with an empty `Addrs` and the
skipped entries when nothing was bound. Shutdown order: `Router.Draining(true)`
→ `srv.Shutdown(dctx)` → `ps.Shutdown(dctx)` (same `DrainTimeout` context,
returns nothing to propagate) → `Gossip.Close()`. On the error path (a
listener or `Run` failed) `Serve` returns at once without draining: there
`ps.Close()` releases the pass-through port and cuts its connections, as the
deferred `closeAll` does for the client listeners.

The decision lives in `Serve`, not `FromConfig`, because `FromConfig` wires
and must neither bind sockets nor mutate `Deps.Config`. Note that devfleet
calls `a.Run`, not `Serve` (`internal/devfleet/devfleet.go:185`), so nothing
in devfleet or `test/sim` exercises the pass-through: coverage is the new
`internal/agent` tests (including the empty-filter start) plus the manual run
below.

Runtime changes to `algod.net` (operator moves algod to a wildcard) are **not**
acted on: the splice target follows automatically, but a port is never
released at runtime. The monitor cannot help either: editing `config.json`
changes its `ModTime`, so `configLoop` (`monitor.go:204`) re-reads, but
`endpointURL` maps `127.0.0.1:8080` and `0.0.0.0:8080` to the same
`http://127.0.0.1:8080`, so nothing observable changes and the only symptom
is algod's `EADDRINUSE`. Documented: change `EndpointAddress` → remove the
entry → restart the agent.

### 6. Directory and router — `internal/app/directory.go`, `internal/app/router.go`

`link` gains `func (l *link) curAddr() string` (`""` when `curPath()` is
nil, `directory.go:313`; path addresses are already normalised). `peerState`
gains `hosts []string`, set in `SetRecords` next to `p.rec = r`
(`directory.go:244`) as `domain.EndpointHosts(r.Endpoints)`: parsed once per
record, not per request. The two `p.rec.Endpoints[0]` sites become
`domain.EndpointFor(p.rec.Endpoints, p.hosts, p.ln.curAddr())`: `Snapshot`
(`directory.go:919-921`, under `d.mu`, string compares only) and the
silent-peer probe job (`directory.go:677-679`, collected under the lock; the
`len(p.rec.Endpoints) > 0` guard stays). Balancers are not in `d.peers` and
are untouched.

Router: `type PassthroughStatus struct { Addrs []string; Skipped []string;
Target string; Active int64 }` (json tags `addrs`, `skipped,omitempty`,
`target`, `active`) and `func (r *Router) SetPassthrough(f func()
PassthroughStatus)` (an `atomic.Pointer`, set from `agent.Serve`).
`serveAgent`'s `/loadb/status` map (`router.go:632`) gains `"passthrough"`
only when set, next to `"links"`. It is set whenever `local.passthrough` is
configured, even when every entry was skipped (`addrs: []`, `skipped` full,
`target` still filled), so an operator can tell "configured, redundant" from
"not configured" without the startup log. `app` never imports the adapter;
the agent builds the closure from `ps.Addrs()`, the skipped list, the target
func and `ps.Active()`.

### 7. autoconfig — `deploy/autoconfig.sh`

Replace the `endpoint_host` block (`autoconfig.sh:279-292`) with two arrays
built from `algod_host`, `algod_port` and `mesh_addrs`. `mesh_hosts` is
`mesh_addrs` when `mesh_bind=1` and empty in the public fallback
(`mesh_bind=0`): autoconfig must never publish algod on a public address on
its own.

| algod.net host | `advertise_endpoints` | `passthrough` |
|---|---|---|
| wildcard (`""`, `0.0.0.0`, `::`, `*`) | every mesh addr at `algod_port` | none |
| loopback (`127.*`, `::1`, `localhost`) | every mesh addr | every mesh addr (warn: "algod listens on X only; the agent passes A, B through to it") |
| specific, in `mesh_addrs` | algod's own first, then the rest | the rest |
| specific, not in `mesh_addrs` | algod's own first, then every mesh addr | every mesh addr (today's warning kept) |
| public fallback, algod loopback | `address` at `algod_port` (unchanged) | none (today's warning stays) |
| public fallback, algod specific | algod's own host (unchanged; today's warning when it is not `address`) | none |

Endpoints stay one `    - http://…` line each (`AdvertiseEndpoints` is
`[]string`; `addr_list_yaml` is for `Addrs`); `passthrough` uses
`addr_list_yaml passthrough "  " …` and is omitted when empty. Reuse
`hostport` and `with_port` (`autoconfig.sh:257-268`); `algod_port` drives
every address.

`deploy/deploy_test.go`: `autoconfigRun` (`:246`) hard-codes `algod.net` as
`0.0.0.0:8080` (`:257`). It is reached only through the two wrappers
(`:235`, `:242`); add an `algodNet` argument to `autoconfigRun` and have both
wrappers pass `0.0.0.0:8080`, so every existing test keeps the wildcard row
(no pass-through), and the new table test calls `autoconfigRun` itself.

### 8. README

"Several mesh interfaces" (`README.md` ~147-163): drop "Proxied algod
traffic keeps using the node's first endpoint"; say a peer proxies to the
advertised endpoint on the host of its current heartbeat path, else the first;
then the pass-through paragraph: what `local.passthrough` does, the startup
skip and why, that algod moving to a wildcard needs the entry removed and a
restart, the `/loadb/status` key and metrics, that peers only use an
address that is also in `advertise_endpoints`, and the exposure in one
sentence: the splice publishes algod itself on those addresses, outside the
agent's listener (no client token, no routing, breaker or retry, no `/loadb`
view of those requests), so a host on the mesh holding the node's algod token
reaches algod directly, as it already can on the address algod binds. The
autoconfig recap (`README.md` ~83-92) replaces "the first one hosts the algod
endpoint" with the table's rule in one sentence.

`MULTI.md:22` states as settled that "proxied algod traffic keeps using
`Endpoints[0]`, and the proxy path … are not touched"; this document reverses
exactly that. Add one parenthetical there, "superseded by MULTI_ALGOD.md:
proxied traffic follows the path too", rather than rewriting the old plan.

## Order of work

1. `internal/domain/endpoint.go` + tests (nothing depends on the rest).
2. `ports.NodeConfig.NetAddr`; `datadir.Read`; `fakealgod.ConfigReader`.
3. `internal/config`: field, `Finish`, `Warnings`, tests; the three example YAMLs.
4. `internal/adapters/passthrough` + tests.
5. `internal/app`: `curAddr`, the two `EndpointFor` sites,
   `PassthroughStatus`/`SetPassthrough`, the status key; `directory_test.go`.
6. `internal/agent`: `filterPassthrough`, `Serve`, tests; `main.go` `config check` line and its test.
7. `deploy/autoconfig.sh`, `deploy_test.go`.
8. `README.md`, the `MULTI.md:22` note.

## Tests

- `internal/domain` — `TestEndpointHosts`: IPv4, bracketed IPv6,
  IPv4-mapped → IPv4, `mem:x` and hostnames → `""`. `TestEndpointFor`:
  matching host wins regardless of position; IPv4-mapped path matches; no
  match → first; `mem:x` → first; empty → `""`; two endpoints on the same
  host with the path on that host → the first (§1, host-only matching).
  `TestAlgodCovers`: same host:port → true; `0.0.0.0:8080` covers
  `10.114.0.5:8080`; `[::]:8080` covers `10.114.0.5:8080` and
  `[fd00::5]:8080`; `0.0.0.0:8080` does not cover `[fd00::5]:8080`; different
  port → false; `127.0.0.1:8080` covers only itself;
  `[::ffff:10.114.0.5]:8080` equals `10.114.0.5:8080`.
- `internal/config/config_test.go` — `TestPassthrough`: dedupe and trim;
  `0.0.0.0:8080` and a port-less entry fail; a balancer with it fails; an
  entry equal to a `listen` address fails; `listen: 0.0.0.0:8080` with
  `passthrough: 10.114.0.5:8080` fails and `listen: 0.0.0.0:4000` with the
  same entry passes (port first, then `wildcardCovers`); a public entry
  warns; an entry absent from `advertise_endpoints` warns; one present in
  both does not.
- `internal/adapters/passthrough/passthrough_test.go` — a helper first
  binds `127.0.0.2:0` bare and `t.Skipf`s only when *that* fails (as
  `gossipudp/udp_test.go:30-33`), so a `Listen` failure for any other reason
  is a real failure. `TestSplice`: an `httptest.Server` on 127.0.0.1,
  pass-through on `127.0.0.2:0`, `http.Get` through it returns the body and
  `loadb_passthrough_accepted` is 1. `TestHalfClose`: against a raw TCP echo
  server, the client `CloseWrite`s and still reads everything back.
  `TestDialFailure`: closed target port → client sees EOF,
  `dial_failures{reason="dial"}`; `target()` returning `""` →
  `reason="no_target"`. `TestTargetFollows`: the target func flips to a
  second server between two connections and the second one reaches it.
  `TestServeReturnsNilOnCancel`. `TestShutdownCutsOpenConnections`: a
  client that never closes is cut when the drain ctx expires and the `active`
  gauge returns to 0. `TestShutdownWaitsForAcceptLoops`: a scripted
  `net.Listener` (an unexported `newServer` takes listeners already bound)
  hands out one more connection after its `Close`, as an `Accept` that
  returned just before the listener closed; with a live connection ending
  during the wait, the wrong order is a `-race` report and the late
  connection outlives `Shutdown`. `TestShutdownWhileAccepting`: four dialers
  hammer the listener while `Close` runs, and every connection the server
  echoed on is cut afterwards (one the kernel queued but the server never
  accepted is not the server's to cut). `TestCloseThenServe`.
  `TestHostPort` with `http://[::1]:8080`.
  `TestListenClosesOnFailure` (mirrors `agent_test.go` `TestListenAll`).
  This package has real goroutines and a shared `conns` map: it is in the
  `-race` run, not just `go test`.
- `internal/app/directory_test.go` — `TestEndpointFollowsThePath`: record
  with `Addrs [addrA, addrB]` and endpoints on both hosts; `Snapshot` reports
  the first endpoint, then `addrA` is made unroutable, the fake clock advances
  until the switch, and `BaseURL` is the `addrB` host's endpoint. The probe
  half needs a recording `AlgodClientFactory` in the harness first:
  `probeSilentPeers` calls `d.clients.NewAlgodClient` unconditionally
  (`directory.go:687`) and the harness passes `nil` (`directory_test.go:139`),
  so the probe path cannot run at all until that factory exists. With it, the
  test asserts the probe hits the same URL as `Snapshot`.
- `internal/agent/agent_test.go` — `TestFilterPassthrough`: covered entries
  dropped (wildcard, equal, `[::]`), uncovered kept, order preserved, one
  warning per drop. `TestServeSkipsCoveredPassthrough`: a fake
  `ConfigReader` with `NetAddr` `0.0.0.0:8080` and `passthrough:
  [127.0.0.2:8080]` → `Serve` is still running after the services have
  started, `/loadb/status` shows `passthrough.addrs` empty and `skipped`
  full, and cancelling returns `nil`; this is the regression the design most
  needs. `TestServePassthrough`: `NetAddr` set to a loopback `httptest`
  server, one pass-through entry on `127.0.0.2` at a free port → a GET through
  it returns the body, then cancel returns `nil` and the port is free again.
- `cmd/algod-loadb-mesh` — pull the node-branch summary of `config check`
  into `func checkSummary(c config.Config) string` and add `main_test.go`:
  the output's `listen` line, fed through the exact `sed` expression from
  `deploy/setup.sh:136`, yields the first client address, and a
  `passthrough` line follows it. The design depends on that line shape.
- `deploy/deploy_test.go` — `TestAutoconfigPassthrough`, table over
  `algod.net` = `0.0.0.0:8080`, `[::]:8080`, `127.0.0.1:8080`,
  `10.114.0.44:8080`, `10.9.9.9:8080`, `10.112.0.44:8081` with the two-WG
  `fakeIP`, asserting `AdvertiseEndpoints` order and
  `Local.Passthrough.String()` per §7; `TestAutoconfigListensOnEveryMeshAddress`
  now asserts both endpoints; `TestAutoconfigPublicFallback` asserts no
  pass-through. `TestExampleListsEveryOption` passes once the example line exists.
- `test/sim`: no new scenario. devfleet addresses are `mem:<id>` (no IP), so
  `EndpointFor` can only fall back there; the directory test with the fake
  clock covers path-following deterministically.
- Budget: run `internal/devfleet/propagation_test.go` before and after;
  nothing here touches `sendHeartbeat`.

## Verification (manual, no WireGuard)

1. Stand-in algod: a temp data dir with `algod.net` = `127.0.0.1:8080`,
   `algod.token`, `genesis.json` (`{"network":"x","id":"y"}`);
   `python3 -m http.server 8080 --bind 127.0.0.1` in it (the monitor reports
   the node offline; the pass-through does not care).
2. Config: `listen: 127.0.0.1:4000`, `local: {id: a, data_dir: <tmp>,
   advertise_endpoints: [http://127.0.0.2:8080], passthrough:
   [127.0.0.2:8080, 127.0.0.3:8080]}`, `registry: {type: static}`, `mesh:
   {listen: 127.0.0.1:4001}`. `config check` prints the `passthrough` line
   and the "127.0.0.3:8080 is not in local.advertise_endpoints" warning.
3. `run`: `ss -ltn` shows `127.0.0.2:8080` and `127.0.0.3:8080`;
   `curl http://127.0.0.2:8080/` returns the directory listing;
   `/loadb/status` shows `passthrough: {addrs, target: "127.0.0.1:8080",
   active}`; `/loadb/metrics` shows `loadb_passthrough_accepted`.
4. Skip: `algod.net` = `0.0.0.0:8080` (server rebound with `--bind 0.0.0.0`),
   restart the agent: the log warns both entries are covered, nothing extra
   is bound. Then `algod.net` = `127.0.0.2:8080` with the server there: only
   `127.0.0.3:8080` is bound.
5. Drain: hold a `curl --max-time 30` through the pass-through open, SIGTERM
   the agent: the connection closes when the drain window ends.
6. `make lint build test race`, `bash -n deploy/autoconfig.sh`.

## Risks and rejected alternatives

- **The port conflict is the operational risk.** If the agent holds
  `10.114.0.5:8080` and algod is later restarted with `EndpointAddress` moved
  to `0.0.0.0:8080`, algod fails to bind. The startup skip catches it at agent
  start; the runtime case is documented (remove the entry, restart the
  agent). Not handled at runtime on purpose: it would put the pass-through
  under the monitor's config loop for a change the operator makes by hand,
  and that loop cannot even see the move (§5: same `Endpoint` after
  `endpointURL`).
- **Exposure.** Every pass-through address is algod itself, reachable by
  anyone on that mesh net who holds the node's algod token, with none of the
  agent's client-token check, routing or observability. That is already true
  of the address algod binds; the splice adds interfaces, not a new class of
  access. The README says so (§8), `Warnings()` flags public addresses, and
  autoconfig never writes one.
- **`NodeConfig` grows by one field** (`NetAddr`); `Endpoint`'s wildcard
  rewrite stays, it is what every dial wants.
- **Rejected**: an HTTP reverse proxy (token handling, header rewriting, for
  traffic that is already HTTP end to end); `SO_REUSEPORT` / sharing the port
  with algod (different address, and algod is not ours); a wildcard
  pass-through (would take the port from algod); an error when an entry is
  absent from `advertise_endpoints` (blocks static / `auto_register: false`
  setups); a sim scenario (devfleet addresses carry no IP to match on);
  releasing a newly covered port at runtime (see above); iptables DNAT (see
  Context).
