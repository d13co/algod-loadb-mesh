# algod-loadb-mesh

Load balancer for [algod](https://github.com/algorand/go-algorand) REST endpoints.

This is v2, a mesh agent that runs on every algod host. The original
single-hub TypeScript proxy (v1) lives in the `algod-loadb` repository until
the fleet has migrated.

## v2 in short

Each host runs one agent next to its algod. The agent:

- follows its own node with a single `wait-for-block-after` long-poll and
  reads the node's data directory for capabilities (archival window,
  developer API, follow mode), verifying the archival window empirically;
- gossips a signed heartbeat per round to the other agents over UDP (on the
  WireGuard networks), so nobody polls anybody else's node; with several
  mesh interfaces each pair of agents settles on the path with the lowest
  measured RTT and abandons it the moment it stops delivering;
- discovers the fleet from an Algorand application whose boxes hold
  encrypted node records (or from a static list in the config);
- routes client requests straight to the chosen algod: the local node first
  in `fallback` mode, or the best-scoring node in `loadbalancer` mode, with
  tiers and static external RPCs (e.g. Nodely) as the last resort;
- keeps v1's client-facing behaviour: `X-Algo-API-Token` auth,
  wait-for-block coalescing, capability-aware routing, optional
  multi-broadcast, drain on shutdown, and the upstream response header (now
  `X-Algod-Loadb-Mesh-Upstream`); a `wait-for-block-after` the local node
  cannot serve is held until a peer's heartbeat, or the local node, reports
  the round, then forwarded to that node, so the block it announces is
  already known to the router when the client asks for it;
- answers pending-transaction lookups from the node that accepted the txn,
  otherwise asks every node at once and returns the most informative answer
  (confirmed, then pool error, then still pending, then 404).

## Build and test

    make build          # bin/algod-loadb-mesh (static binary, no cgo)
    make test           # unit, adapter and simulator tests
    make race
    make dev            # 3 fake nodes + 3 agents in one process

## Install

On a linux host (amd64 or arm64), run the installer as your user. It downloads
and verifies as you, and uses sudo only to install:

    curl -fsSL https://raw.githubusercontent.com/d13co/algod-loadb-mesh/stable/install.sh | bash

On Debian and Ubuntu, it adds the apt repository at `https://apt.d13.co` and
installs the `algod-loadb-mesh` package. The package puts `algod-loadb-mesh`,
`algod-loadb-mesh-autoconfig` and `algod-loadb-mesh-setup` in `/usr/bin` and
configures nothing. Later upgrades come with `apt upgrade`, and they restart
the agent if it is running. Before trusting the repository key, the installer
checks that it is exactly this key (when gpg is installed):

    F66E 7713 3063 650C 159F  405A C4D2 171F B5B6 0471

To add the repository by hand instead:

    sudo install -d -m 0755 /etc/apt/keyrings
    sudo curl -fsSL https://apt.d13.co/key.asc -o /etc/apt/keyrings/d13co.asc
    echo "deb [signed-by=/etc/apt/keyrings/d13co.asc] https://apt.d13.co stable main" |
      sudo tee /etc/apt/sources.list.d/d13co.list
    sudo apt update && sudo apt install algod-loadb-mesh

On other distributions, or with `--no-apt`, the installer fetches the release
binary and the deploy scripts into `/usr/local/bin` under the same names
instead. That install gets no automatic upgrades.

Setup is a separate step that configures the host: it writes the config,
installs the systemd unit and starts the service. On a host that already runs
the mesh, print the registry bundle (app id, sync key, and the sync address when
it is rekeyed) and paste it into the setup on the new host:

    algod-loadb-mesh registry bundle -config /etc/algod-loadb-mesh/config.yaml

    sudo algod-loadb-mesh-setup -       # paste the bundle, then Ctrl-D

Releases come from version tags: pushing `v1.2.3` builds the linux amd64 and
arm64 binaries and `.deb` packages ([nfpm.yaml](nfpm.yaml)), publishes them
with `checksums.txt`
([.github/workflows/release.yml](.github/workflows/release.yml)) and
fast-forwards `stable` to that commit, so the scripts install.sh fetches from
`stable` match the binary it downloads. `--version v1.2.3` installs an older
release.

`algod-loadb-mesh-setup -h` lists the rest: `--config FILE` to install a config
you already have, `--no-start`, `--user`, and anything autoconfig takes.
Add `--setup` to the install command to do both at once
([install.sh](install.sh), [deploy/setup.sh](deploy/setup.sh)).

### Moving to the apt package

A host set up by an older install.sh (or with `--no-apt`) runs
`/usr/local/bin/algod-loadb-mesh`, which the package does not update. The
package install and the installer both warn about this. After installing the
package as above, remove the old files, write the unit again (the config is
kept) and restart:

    sudo rm /usr/local/bin/algod-loadb-mesh /usr/local/bin/algod-loadb-mesh-autoconfig \
      /usr/local/bin/algod-loadb-mesh-setup
    sudo rm -r /usr/local/share/algod-loadb-mesh
    sudo algod-loadb-mesh-setup --no-start    # add --user USER if you set one before
    sudo systemctl restart algod-loadb-mesh

## Run on a node

1. Create the sync account once per fleet and fund it with a few ALGO:

       algod-loadb-mesh registry gen-key

2. Write a config from the example and edit it (at least `local`,
   `registry.sync_key_file` and `mesh.advertise`, the gossip addresses peers
   reach this host on, one per mesh interface; add `-static` for a static
   peer list):

       algod-loadb-mesh config example -o /etc/algod-loadb-mesh/config.yaml

   Or let [deploy/autoconfig.sh](deploy/autoconfig.sh) fill those in from the
   host: id from the hostname (minus `.local`), the algod data dir (it asks
   when there are several) and the addresses from every 10.112.* and 10.114.*
   interface, else a public one. Clients and gossip are served on all of
   them, peers are told all of them (`mesh.listen` and `mesh.advertise` are
   the same list) and algod is advertised on all of them too: the address it
   binds (from `algod.net`) as is, the rest bound by the agent and passed
   through to it (`local.passthrough`); with no such address everything is
   served on all interfaces and algod is never passed through. It also asks whether to
   add Nodely as the last-resort fallback (`--nodely`/`--no-nodely` to skip
   the question; `--address`, `--listen`, `--client-port` and the rest of
   `-h` override the rest):

       algod-loadb-mesh-autoconfig --app-id 1234 -o /etc/algod-loadb-mesh/config.yaml

   Then deploy the registry application (the local node submits the
   transaction):

       algod-loadb-mesh registry init -config /etc/algod-loadb-mesh/config.yaml

3. Put the printed `app_id` into every host's config (the example is also at
   [deploy/config.example.yaml](deploy/config.example.yaml)), install
   [deploy/algod-loadb-mesh.service](deploy/algod-loadb-mesh.service), start it. Agents
   register themselves on first boot and pick each other up within one
   `registry.refresh` (sooner when they hear an unknown heartbeat).

Useful commands:

    algod-loadb-mesh config check -config ...                         # what a config resolves to
    algod-loadb-mesh check-node -data-dir /var/lib/algorand -probe   # what the agent will advertise
    algod-loadb-mesh registry list -config ...                        # decrypted fleet view
    algod-loadb-mesh registry rm -config ... -id old-node             # retire a node
    curl -H "X-Algo-API-Token: $(cat /var/lib/algorand/algod.admin.token)" http://127.0.0.1:4000/loadb/status

Agent endpoints: `/loadb/health` (no token), and `/loadb/status`,
`/loadb/peers`, `/loadb/metrics` (Prometheus text), which need the admin token
in `X-Algo-API-Token`. The admin token defaults to the node's
`algod.admin.token` (`admin_token` / `admin_token_file` override it) and is
accepted as a client token too.

Without a chain registry, `registry.type: static` lists peers in the config
and `mesh.shared_secret` replaces the sync key as key material
([deploy/config.static.example.yaml](deploy/config.static.example.yaml)).

## Run without a node (balancer)

A host without algod can run the agent as a balancer: it serves clients from
the fleet in `loadbalancer` mode and is never an upstream itself. It
registers a `role: balancer` record, and every node sends its heartbeats to
the balancer's `mesh.advertise` as it does to other nodes, so the balancer
sees each round as quickly as a node does. `wait-for-block-after` is held
until a heartbeat reports a later round, then answered with one
`/v2/status` fetch from a node at that round, shared by all waiters.

    algod-loadb-mesh config example -balancer -o /etc/algod-loadb-mesh/config.yaml

It needs `local.network` (the genesis id), `mesh.advertise`, and, with the
on-chain registry, `registry.algod_url` and `algod_token` of any synced node
to read the registry and register through. No algod data dir means no
default admin token: set `admin_token_file`. Requests only a local node can
answer (for example the pending transaction pool) get a 503. Nodes must run a
build that knows roles before they send heartbeats to balancers.
With a static registry, list the balancer on the nodes as
`{id: lb1, role: balancer, network: mainnet-v1.0, agent: {addrs: [10.112.0.9:4001, 10.114.0.9:4001]}}`.
`algod-loadb-mesh dev -balancers 1` adds one to the fake fleet.

## Several mesh interfaces

Hosts on two WireGuard networks list both addresses in `mesh.listen` (one
UDP socket each, so gossip never answers on a public interface) and
`mesh.advertise` (what the registry tells peers, preferred first; a static
registry lists them under `agent: {addrs: [...]}`). Every pair of agents
picks **one** path for its heartbeats: registry order at first, then the
lowest measured RTT, re-checked by a ping on every address each
`mesh.path_probe_interval` (30s). A path is abandoned at once when its pings
go unanswered (three `mesh.path_timeout`s, 2s each), when the transport has
no route to it, or when the peer's probes report that it has stopped hearing
our heartbeats — the asymmetric case, where only the receiving side can see
the break. Proxied algod traffic follows the path: a peer sends to the
advertised endpoint on the host of its current heartbeat path, else the
first one. `/loadb/status` lists every link under `links` with the chosen
path and each candidate's RTT, and the metrics `loadb_path_rtt_ms`,
`loadb_path_losses` and `loadb_path_switches{reason}` follow it. `config
check` warns when a mesh address is public or a wildcard.

algod itself binds one address (`EndpointAddress`), so a node whose algod
listens on 10.112.0.5:8080 would be unreachable for a peer that only shares
the 10.114 net. `local.passthrough` lists the other `host:port`s to serve
algod on: the agent binds each and splices every TCP connection straight
through to the address in `algod.net`, tokens, long-polls and streams
untouched. At start it reads `algod.net` and skips, with a warning, every
entry algod already covers (same address, or a wildcard bind on that port),
so it never takes a port from algod; the skipped entries show under
`passthrough.skipped` in `/loadb/status` next to the bound `addrs`, the
`target` and the `active` connections, and the metrics
`loadb_passthrough_accepted{addr}`, `loadb_passthrough_dial_failures{reason}`
and `loadb_passthrough_active` count them. Ports are only released at
restart: to move algod itself onto a wildcard, remove the entry first and
restart the agent, or algod fails to bind. A pass-through is only used by
peers when the same address is in `local.advertise_endpoints` (`config
check` warns otherwise), and it is algod itself on that address, outside the
agent's listener: no client token, no routing, no `/loadb` view of those
requests, so anyone on that mesh net holding the node's algod token reaches
algod directly, as they already can on the address algod binds. `config
check` warns when a pass-through address is public.

## Layout

    cmd/algod-loadb-mesh        CLI: run, check-node, registry, dev
    internal/domain        pure core: types, classify, eligible, select, health, codecs
    internal/ports         interfaces the services depend on
    internal/app           services: monitor, directory, router, registry sync, stats
    internal/adapters      algod REST, data dir, UDP/in-memory gossip, proxy, registries, clock, metrics
    internal/agent         composition root
    internal/fakealgod     in-process algod simulator used by tests and `dev`
    internal/devfleet      N fake nodes + N agents wired together
    test/sim               multi-agent scenarios
    test/contract          Go registry client against algokit localnet (`make contract-test`)
    contract               registry application, AlgoKit project (`make contract`)
