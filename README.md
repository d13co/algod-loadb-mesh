# algod-loadb-mesh

A load balancer and failover proxy for [algod](https://github.com/algorand/go-algorand) REST APIs, built as a mesh: a small agent runs next to each of your Algorand nodes, the agents tell each other what their nodes can do, and every agent can route a client's request to whichever node in the fleet is best placed to answer it.

Point your apps, indexers and bots at the agent instead of at algod. They keep using the plain algod API and token, and stop caring which node is up, synced, archival or has the developer API turned on.

```
            your app ──► agent :4000 ──► local algod            (healthy, can answer)
                              │
                              └────────► peer algod             (local node lagging, or lacks the capability)
                                              │
                                              └──► Nodely etc.  (nothing in the fleet can answer)
```

## Contents

- [Features](#features)
- [Try it locally](#try-it-locally)
- [Install](#install)
- [Set up a fleet](#set-up-a-fleet)
- [Connection model](#connection-model)
- [How a request is routed](#how-a-request-is-routed)
- [Example layouts](#example-layouts)
- [Strengths and weaknesses](#strengths-and-weaknesses)
- [Operating it](#operating-it)
- [Configuration reference](#configuration-reference)
- [Development](#development)

## Features

- **Drop-in algod API.** Clients send the same requests and the same `X-Algo-API-Token` they send to algod. CORS headers match algod's, so browser dApps work too.
- **Two routing modes.** `fallback` serves from the local node and moves away only when it can't answer; `loadbalancer` spreads requests across the fleet by measured latency, error rate, lag and load.
- **Capability-aware routing.** Each agent reads its node's data directory and advertises what the node can do: archival window (measured, not just taken from config), developer API (`/v2/teal/*`), follow mode, and time travel. Each request goes only to nodes that can answer it.
- **Round-aware routing.** A request for a specific round (a block, a delta, a box listing or a simulation at a round, state at a past round) goes only to a node that still holds that round.
- **Sync tracking with no polling.** Each agent follows its own node with one `wait-for-block-after` long-poll and pushes a signed UDP heartbeat to its peers every round. Agents never poll each other's nodes, so a new round reaches the whole fleet in milliseconds.
- **Retries and failover.** A request whose upstream fails without answering (connection error, 502/503/504) is retried on the next eligible node, then on an external RPC, within a single deadline. An answer from algod, such as a rejected transaction, goes back to the client and is never retried. Circuit breakers and 429 back-off keep traffic away from a node that is failing.
- **External RPCs as a last resort.** Public endpoints such as Nodely are used only when nothing in your fleet can serve a request. They are not polled in the background.
- **Fleet discovery.** Nodes register themselves in an Algorand smart contract, with each record encrypted in a box. A static list in the config, or a shared JSON file, works too.
- **Balancer-only hosts.** An agent can run on a host with no algod and route clients to the fleet.
- **Smart handling of algod's awkward endpoints.**
  - Concurrent `wait-for-block-after` calls for the same round share one upstream call.
  - A wait for a round the local node hasn't reached is answered as soon as any node in the fleet has it.
  - Pending-transaction lookups go to the node that accepted the transaction, or ask every node at once and return the most informative answer.
  - Optional multi-broadcast sends `POST /v2/transactions` to every eligible node.
- **Multi-homed networks.** On hosts with more than one private network (for example two WireGuard meshes), each pair of agents picks the lowest-latency path and switches away from it as soon as it stops delivering. The agent can also expose algod on the interfaces algod itself doesn't bind.
- **Observable.** It has a status JSON view, Prometheus metrics, and response headers that name the node that served each request.
- **Easy to deploy.** It ships as a single static Go binary and a Debian/Ubuntu apt package, with a setup script that detects the host's settings and a one-line join for new hosts.

## Try it locally

You can try the whole thing without an Algorand node. `dev` mode starts fake algods and real agents in one process: `dev1` is archival, the last node has the developer API and time travel, and blocks advance every 2.8 s.

```sh
git clone https://github.com/d13co/algod-loadb-mesh && cd algod-loadb-mesh
make dev                                  # or: ./bin/algod-loadb-mesh dev -nodes 3 -balancers 1 -mode loadbalancer
```

It prints each agent's URL. Then:

```sh
curl -s  http://127.0.0.1:PORT/loadb/status | jq      # what the agent sees
curl -si http://127.0.0.1:PORT/v2/status               # see X-Algod-Loadb-Mesh-Upstream
```

Building needs Go 1.27 or newer.

## Install

Linux, amd64 or arm64. Run the installer as your normal user: it downloads and verifies as you, and uses sudo only to install.

```sh
curl -fsSL https://raw.githubusercontent.com/d13co/algod-loadb-mesh/stable/install.sh | bash
```

On **Debian and Ubuntu** the installer adds the apt repository at `https://apt.d13.co` and installs the `algod-loadb-mesh` package, so later upgrades come with `apt upgrade` (and restart the agent if it is running).

The package also installs `/etc/apt/apt.conf.d/51algod-loadb-mesh-upgrades`, which lets unattended-upgrades install new releases from apt.d13.co on its daily run (this package, and `dtalgorand` if installed). To upgrade by hand only, delete that file; upgrades keep it deleted. On hosts that also run `dtalgorand`, its own `51dtalgorand-upgrades` allows the same repository, so remove both.

Before trusting the repository key, the installer checks that it is exactly this one (when gpg is installed):

    F66E 7713 3063 650C 159F  405A C4D2 171F B5B6 0471

To add the repository by hand instead:

```sh
sudo install -d -m 0755 /etc/apt/keyrings
sudo curl -fsSL https://apt.d13.co/key.asc -o /etc/apt/keyrings/d13co.asc
echo "deb [signed-by=/etc/apt/keyrings/d13co.asc] https://apt.d13.co stable main" |
  sudo tee /etc/apt/sources.list.d/d13co.list
sudo apt update && sudo apt install algod-loadb-mesh
```

On **other distributions**, or with `--no-apt`, the installer puts the release binary and scripts in `/usr/local/bin`. That install doesn't upgrade itself; run the installer again to upgrade.

Either way you get three commands, and no agent is configured yet:

| command | what it does |
|---|---|
| `algod-loadb-mesh` | the agent and its CLI (`run`, `config`, `registry`, `check-node`, `dev`) |
| `algod-loadb-mesh-autoconfig` | detects the host (algod data dir, mesh addresses, ports) and prints a config |
| `algod-loadb-mesh-setup` | runs autoconfig, writes `/etc/algod-loadb-mesh/config.yaml`, installs the systemd unit and starts the service |

Installer options: `--version v1.2.3` installs a specific release, `--no-apt` skips the apt repository, and `--setup ARGS...` runs setup straight after installing.

<details>
<summary>Moving to the apt package (from a <code>/usr/local</code> install)</summary>

A host set up with `--no-apt` (or by an older installer) runs `/usr/local/bin/algod-loadb-mesh`, which the package doesn't update. The package install and the installer both warn about this. After installing the package, remove the old files, write the unit again (the config is kept) and restart:

```sh
sudo rm /usr/local/bin/algod-loadb-mesh /usr/local/bin/algod-loadb-mesh-autoconfig \
  /usr/local/bin/algod-loadb-mesh-setup
sudo rm -r /usr/local/share/algod-loadb-mesh
sudo algod-loadb-mesh-setup --no-start    # add --user USER if you set one before
sudo systemctl restart algod-loadb-mesh
```

A host with [several agents](#several-algods-on-one-host) also needs `sudo algod-loadb-mesh-setup --instance NAME --no-start` (any one of its names: they share the template unit) and `sudo systemctl restart 'algod-loadb-mesh@*'`.

</details>

## Set up a fleet

Before you start, check these things on each node host:

- The hosts can reach each other on a private network (WireGuard, a VPC, a LAN). Autoconfig looks for `10.112.*` and `10.114.*` interfaces and falls back to a public address; use `--address` for other ranges.
- Peers send proxied requests straight to algod, so algod's port must be reachable from them. If algod only listens on `127.0.0.1` (or on one interface), setup has the agent bind algod's port on the mesh addresses and pass the connections through to it.
- The agent's user (default `algorand`) can read the algod data directory.

### First host: create the registry

The registry is a small Algorand application that holds the fleet's node records. It is owned by a *sync account*, whose key every agent shares.

```sh
# 1. Create the sync account, fund the printed address with a few ALGO,
#    and store the mnemonic where the agent's user can read it.
algod-loadb-mesh registry gen-key
sudo install -d -m 0750 -o algorand -g algorand /etc/algod-loadb-mesh
sudo -u algorand sh -c 'umask 077 && cat > /etc/algod-loadb-mesh/sync.key'   # paste the mnemonic, then Ctrl-D

# 2. Deploy the registry app. Your local node submits the transaction; the
#    throwaway config only says where the node and the key are.
printf 'local: {data_dir: /var/lib/algorand}\nregistry: {sync_key_file: /etc/algod-loadb-mesh/sync.key}\n' > init.yaml
sudo algod-loadb-mesh registry init -config init.yaml && rm init.yaml

# 3. Configure and start the agent with the printed app id.
sudo algod-loadb-mesh-setup --app-id <id>
```

### Every other host: join with a bundle

On any host that already runs the mesh, print the *bundle*: the app id and sync key (and the sync address, if it is rekeyed) as one string.

```sh
sudo algod-loadb-mesh registry bundle -config /etc/algod-loadb-mesh/config.yaml
```

On the new host, install and paste the bundle into setup:

```sh
curl -fsSL https://raw.githubusercontent.com/d13co/algod-loadb-mesh/stable/install.sh | bash
sudo algod-loadb-mesh-setup -        # paste the bundle, then Ctrl-D
```

The new agent registers itself on first start. The others pick it up within one `registry.refresh` (5 min), or sooner if they hear a heartbeat from an unknown node first.

The bundle contains the sync key. Anyone who has it can read every node's algod token from the registry and change the fleet, so treat it like a private key.

### What setup detects

`algod-loadb-mesh-setup -h` and `algod-loadb-mesh-autoconfig -h` list every option. By default:

- **node id**: the hostname, minus `.local` (`--id`)
- **algod data dir**: detected; it asks when there are several (`--data-dir`)
- **client token**: the node's own `algod.token`, so clients keep their current token (`--client-token-file`)
- **addresses**: every `10.112.*`/`10.114.*` interface, else a public one (`--address`). Clients and gossip are served on all of them and peers are told about all of them.
- **ports**: 4000/tcp for clients and 4001/udp for gossip, or the next free pair if [other agents](#several-algods-on-one-host) run on the host (`--client-port`, `--mesh-port`)
- **algod pass-through**: algod is advertised on every mesh address. Algod binds the address in `algod.net`, and the agent binds the rest and passes them through (see [multi-homed hosts](#multi-homed-hosts)).
- **external fallback**: it asks whether to add Nodely for the node's network (`--nodely`/`--no-nodely`)
- **routing tier**: 1 (`--tier`)

To install a config you wrote yourself, use `--config FILE`. To start from the fully commented example, run `algod-loadb-mesh config example -o FILE`.

### Several algods on one host

One agent serves one algod, so a host with several algods (mainnet and testnet, or an archival and a time-travel node of one network) runs one agent for each. The first is set up as usual; each further one is a named instance:

```sh
sudo algod-loadb-mesh-setup --instance testnet --data-dir /var/lib/algorand/testnet -   # paste testnet's bundle
```

| | first agent | instance `NAME` |
|---|---|---|
| config | `/etc/algod-loadb-mesh/config.yaml` | `/etc/algod-loadb-mesh/NAME.yaml` |
| service | `algod-loadb-mesh` | `algod-loadb-mesh@NAME` |
| `local.id` | hostname | `HOSTNAME-NAME` |
| registry cache | `/var/lib/algod-loadb-mesh/registry.cache` | `/var/lib/algod-loadb-mesh/NAME.registry.cache` |

Setup gives each agent the first client and gossip ports no other agent in `/etc/algod-loadb-mesh` binds: 4000/4001, then 4002/4003, and so on (`--client-port` and `--mesh-port` choose them). It leaves out of detection the data dirs other agents already serve, and refuses a config that shares a data dir, a port, a node id or a registry cache with one of them. A `--force` re-run keeps the agent's ports while they are free. Run without `--instance` and with a `--data-dir` other than the one `config.yaml` serves, it stops and suggests a name instead of replacing that config (without `--data-dir`, it keeps `config.yaml` as always). All agents on a host run as one user, so a different `--user` is refused while other agents exist. Every `*.yaml` in `/etc/algod-loadb-mesh` counts as an agent's config: setup stops on one that does not load.

Each agent belongs to the fleet of its own network, so it takes that fleet's bundle; two agents of one network join the same fleet with the same bundle. The other commands take the instance's config, for example `algod-loadb-mesh registry bundle -config /etc/algod-loadb-mesh/testnet.yaml` and `systemctl status algod-loadb-mesh@testnet`. Package upgrades restart every running agent.

Configs written by hand need the same care: a distinct `local.id` (the hostname default collides), `listen` and `mesh.listen` ports, and `registry.cache` path for each.

### Without the on-chain registry

If you'd rather not keep the fleet on chain, list the peers in every host's config with `registry.type: static`, and give every host the same `mesh.shared_secret` in place of the sync key. Start from `algod-loadb-mesh config example -static` ([deploy/config.static.example.yaml](deploy/config.static.example.yaml)). Adding a node then means editing every host's config.

## Connection model

An agent talks over four kinds of connection. None of them pass through another agent: a request that goes to a peer is sent straight to that peer's algod.

```
   clients                                         ┌──────────── host B ───────────┐
      │ HTTP :4000  (algod API + token)            │                               │
      ▼                                            │  agent B ◄──── algod B :8080  │
 ┌──────────── host A ────────────┐   UDP :4001    │     ▲   wait-for-block-after  │
 │  agent A ◄───── algod A :8080  │◄─────────────► │     │                         │
 │     │  wait-for-block-after    │   heartbeats   └─────┼───────────────▲─────────┘
 │     │                          │   path pings         │               │
 └─────┼──────────────────────────┘                      │               │
       │  HTTP to algod B :8080 (B's algod token) ───────┼───────────────┘
       │  HTTPS to external RPC (last resort) ──► Nodely │
       │                                                 │
       └─ algod (any synced node) ──► registry app on chain: encrypted node records
```

| connection | protocol, default port | what it carries |
|---|---|---|
| **client → agent** | HTTP, `listen` (default `127.0.0.1:4000`) | The algod REST API, authenticated with `X-Algo-API-Token`. The agent's own `/loadb/*` endpoints are here too. |
| **agent → algod** (local or peer) | HTTP, the node's `advertise_endpoints` (e.g. `:8080`) | Proxied client requests, using that node's algod token. The agent also keeps one long-poll open on its local node to follow new rounds. |
| **agent ↔ agent** | UDP, `mesh.listen` (default `:4001`) | Small signed heartbeats (round, online, capabilities, draining) on every new round and every `keepalive` (5 s), plus path-latency pings. When a peer goes quiet, the agent checks the peer's algod `/v2/status` directly before marking it down. |
| **agent → registry** | algod REST, through the router | Reads the fleet's records every `registry.refresh` and writes its own record on start. |
| **agent → external** | HTTPS | Only when no node in the fleet can serve a request, with a status check at most once per `health_check`. |

What this means in practice:

- **Open these ports between hosts:** algod's port (tcp) and the mesh port (udp, 4001). Open the client port (tcp, 4000) to your clients.
- **Proxied traffic takes one hop.** Client → agent → the algod that answers. No agent forwards to another agent.
- **Peer liveness costs nothing at idle.** Heartbeats are pushed, not polled. The agent contacts a peer's algod directly only when that peer goes quiet.
- **Trust is fleet-wide.** Each node record holds the node's algod endpoints and token, so every member of the fleet can call every node's algod. Records are encrypted with AES-GCM using a key derived from the sync key, and box names are keyed hashes, so node ids never appear on chain. Heartbeats are signed with per-node ed25519 keys derived from the same secret.
- **Clients connect to one agent.** The agent itself is not replicated. If a client must survive its agent's host going down, give it several agents to choose from (see [layouts](#example-layouts)).

## How a request is routed

1. **Classify.** The path and query decide what the request needs: a specific round (and whether the block store or recent account state must hold it), the developer API, time-travel history, the local node only (`/health`, `/metrics`, the admin API, and so on), or a broadcast.
2. **Filter.** A node is eligible only if it is reachable, not draining, its circuit breaker is closed, it is not being throttled, it has the needed capability, and it is synced (within `sync_tolerance` of the best round in the fleet). A request for a past round only needs a node that still holds that round, synced or not. A node that fell behind has to stay synced for `return_hysteresis_rounds` rounds before it serves again, so it doesn't flap.
3. **Order by tier.** In `fallback` mode the local node always comes first. After it come peers by `tier` (lower is preferred), and externals always come last.
4. **Pick within the tier.** `fallback` takes the best score, which keeps the choice stable. `loadbalancer` uses power-of-two-choices on the score. The score adds up measured latency for this kind of request, error rate, rounds behind, and in-flight requests, with a small bonus for the local node.
5. **Retry.** If the chosen node fails without answering, the request goes to each remaining eligible node in turn, then to the externals, all within `request_timeout`. Writes are retried too: resubmitting a transaction is safe because algod deduplicates by transaction id.

Every response says who answered it, in the `X-Algod-Loadb-Mesh-Upstream`, `X-Algod-Loadb-Mesh-Tier` and `X-Algod-Loadb-Mesh-Mode` headers.

### Time-travel nodes

A node built from the time-travel fork (`EnableTimeTravel` in `config.json`) keeps every ledger object's value at every round, so it can answer state questions about any past round. Stock algod reaches back much less, and how far depends on the request:

| request | stock node can answer | time-travel node can answer |
|---|---|---|
| account, asset, app, box or supply state with `?round=`, and `.../versions` histories | never (it ignores `round` and returns the latest state) | any round |
| block header `?header-only=true` | its block window (archival: all; otherwise ~1000 rounds) | any round |
| box listing with `?round=`, simulate with `"round"` in the body | its account-state lookback (`MaxAcctLookback`, a few rounds) | any round |
| state delta `/v2/deltas/{round}` | its account-state lookback, minus the oldest round | its block window |
| full block, block hash, state proof | its block window | its block window |

The agent detects the capability and sends it in its heartbeats, so a mixed fleet starts routing correctly as soon as the agents hear each other. If a round sits right at the edge of a stock node's window, the agent prefers a time-travel node, because the stock node may already have moved past it. To declare the capability on a node the agent can't inspect, set `time_travel: true` under `local.overrides`, `tiers.external[].capabilities`, or a static record's `declared`.

## Example layouts

### 1. One node with a public fallback

```
  app ──► agent :4000 ──► algod (local)
                    └───► Nodely  (when algod is down, catching up, or the request needs archival data)
```

This is the smallest useful setup: a single host, no peers and a static registry with no entries, plus one external tier. Your app gets transparent failover during algod restarts and catchup, and archival queries go to Nodely when your node is non-archival.

### 2. Sidecar fleet (fallback mode)

```
  ┌─ host A ──────────────┐  ┌─ host B ──────────────┐  ┌─ host C ──────────────┐
  │ app ► agent ► algod A │  │ app ► agent ► algod B │  │ app ► agent ► algod C │
  └───────────┬───────────┘  └───────────┬───────────┘  └───────────┬───────────┘
              └────────── WireGuard: heartbeats + peer algod ───────┘
```

Each app talks to the agent on `127.0.0.1:4000` of its own host. Requests are served locally while the local node is healthy and move to a peer the moment it lags, restarts or lacks a capability. This is the default mode, and it suits services that run on your node hosts.

### 3. Dedicated balancers in front of a fleet (loadbalancer mode)

```
                       clients
                          │
                  DNS / L4 load balancer
                    ┌─────┴─────┐
                 balancer1  balancer2        role: balancer, mode: loadbalancer (no algod)
                    └─────┬─────┘
          ┌───────────────┼───────────────┐
       algod A         algod B         algod C        each with its own agent, in the registry
```

Balancers register with `role: balancer`, and every node sends them heartbeats, so they see each new round as quickly as a node does. Spread the load over the whole fleet, and put several balancers behind DNS or an L4 load balancer so that losing one doesn't matter. Requests that only a local node can answer, such as the transaction pool, get a 503 from a balancer. Generate a balancer config with `algod-loadb-mesh config example -balancer` ([deploy/config.balancer.example.yaml](deploy/config.balancer.example.yaml)). A balancer needs `local.network` (the genesis id), `mesh.advertise`, `admin_token_file`, and, with the on-chain registry, `registry.algod_url`/`algod_token` of any synced node.

### 4. Mixed capabilities

```
  tier 1: algod A, B   (non-archival, fast, cheap)
  tier 1: algod C      (archival)
  tier 1: algod D      (time travel + developer API)
  tier 100: Nodely     (external)
```

Clients hit any agent and never think about which node to use. Current-state reads and transaction submits spread over all four nodes. `/v2/blocks/1000` goes to C or D. `/v2/teal/compile` and `/v2/accounts/X?round=…` go to D. Nodely is used only when none of them can answer. Use tiers to keep traffic on cheaper nodes: put the expensive ones on tier 2, and they'll only get the requests tier 1 can't serve.

### 5. Multi-homed hosts

```
          10.112.0.0/16 (wg0)                 10.114.0.0/16 (wg1)
  host A ───────────────────── host B ───────────────────── host C
  (wg0 + wg1)                 (wg0 + wg1)                   (wg1 only)
```

Hosts on two private networks list both addresses in `mesh.listen` and `mesh.advertise`, and autoconfig does this for you. See [multi-homed hosts](#multi-homed-hosts) for how paths are chosen and how algod is exposed on both networks.

## Strengths and weaknesses

**Strengths**

- No changes for clients: same API, same token, same SDK config with a different URL.
- Fast, cheap fleet awareness. Round changes are pushed in one signed UDP datagram, there's no background polling, and the client path takes one HTTP hop.
- Routes on what each node can actually do, measured from its data directory and checked against the real archival window, not on a hand-kept list.
- Knows algod's quirks: rounds past the tip, wait-for-block coalescing, pending-transaction lookups, the time-travel windows.
- Fails over within a single request: a client sees one slow answer instead of an error.
- Simple to run: a static binary, one config file per host, self-registration, an apt package and a one-line join.
- Works without the registry too, using a static list or a shared file.

**Weaknesses and limits**

- **Not a highly available endpoint on its own.** A client depends on the agent it connects to. Put agents on every app host (layout 2) or several balancers behind DNS/L4 (layout 3).
- **Assumes a trusted private network.** Gossip and proxied traffic are plain UDP/HTTP, and every member can use every node's algod token. Run it over WireGuard or a VPC, not the open internet. `config check` warns about public or wildcard mesh addresses.
- **No TLS on the client listener.** Put a TLS-terminating proxy in front if clients connect over untrusted networks.
- **One agent serves one algod.** A host with several algods (for example mainnet and testnet) runs one agent per algod, each with its own config, ports and node id. Setup handles this with named instances (see [several algods on one host](#several-algods-on-one-host)).
- **Linux and systemd only** for the packaged install and setup scripts. The binary itself is plain Go.
- **The on-chain registry has a small cost.** The sync account pays box storage and fees (a few ALGO), and the sync key is a shared secret to manage. The static registry avoids both but must be edited on every host.
- **Externals are trusted as declared.** Nothing is detected for an external RPC: you state its capabilities in the config, and it is only checked once nothing in the fleet can serve.
- **Balancers can't answer node-local requests.** The transaction pool and the admin API need a local node.

## Operating it

```sh
algod-loadb-mesh config check -config /etc/algod-loadb-mesh/config.yaml        # what a config resolves to, with warnings
algod-loadb-mesh check-node -data-dir /var/lib/algorand -probe                # what the agent will advertise for this node
algod-loadb-mesh registry list -config /etc/algod-loadb-mesh/config.yaml      # decrypted fleet view
algod-loadb-mesh registry rm -config /etc/algod-loadb-mesh/config.yaml -id old-node   # retire a node
algod-loadb-mesh registry status -config /etc/algod-loadb-mesh/config.yaml    # registry app build, boxes, balance
systemctl status algod-loadb-mesh; journalctl -u algod-loadb-mesh -f
```

Agent endpoints:

| endpoint | auth | what |
|---|---|---|
| `/loadb/health` | none | liveness |
| `/loadb/status` | admin token | this agent's view: local node, peers, links and paths, externals, pass-through |
| `/loadb/peers` | admin token | the peer table |
| `/loadb/metrics` | admin token | Prometheus text format |

```sh
curl -H "X-Algo-API-Token: $(sudo cat /var/lib/algorand/algod.admin.token)" http://127.0.0.1:4000/loadb/status
```

The admin token defaults to the node's `algod.admin.token` (set `admin_token` or `admin_token_file` to change it), and it is accepted as a client token too.

When the agent shuts down, it marks itself as draining in its heartbeats so peers stop routing to it, and gives in-flight requests `drain_timeout` to finish.

### Multi-homed hosts

When two hosts share several mesh networks, the pair agrees on **one** path for heartbeats: registry order at first, then the lowest measured round-trip time, re-checked with a ping on every address each `mesh.path_probe_interval` (30 s). A path is dropped at once when:

- three pings in a row go unanswered (`mesh.path_timeout`, 2 s each),
- the host has no route to it, or
- the peer reports that our heartbeats stopped arriving. In that one-way failure, only the receiving side can see the break.

Proxied algod traffic follows the same path. `/loadb/status` shows every link under `links`, and the metrics `loadb_path_rtt_ms`, `loadb_path_losses` and `loadb_path_switches{reason}` track them.

algod binds only one address, so a node listening on `10.112.0.5:8080` can't be reached by a peer that is only on `10.114`. To fix that, list the other addresses in `local.passthrough`: the agent binds each one and passes every TCP connection straight through to algod, untouched. It skips, with a warning, any address that algod already covers, so it never takes algod's port. Peers use a pass-through address only when it is also listed in `local.advertise_endpoints`. Traffic on it goes directly to algod: it needs the node's algod token, not a client token, and doesn't show up in `/loadb`. Ports are released only on restart, so to move algod onto a wildcard bind, remove the entry and restart the agent first.

## Configuration reference

`algod-loadb-mesh config example` prints every option with its default and a comment ([deploy/config.example.yaml](deploy/config.example.yaml)). The sections:

| section | what it controls |
|---|---|
| top level | `role` (node/balancer), `mode` (fallback/loadbalancer), `listen`, client and admin tokens, `log` |
| `local` | node id, algod data dir, `advertise_endpoints`, `passthrough`, `tier`, `tags`, capability `overrides` |
| `registry` | `algorand` (app id, sync key), `static` list, or shared `file`; refresh interval, cache, auto-registration |
| `mesh` | gossip `listen`/`advertise`, timeouts for suspect/down peers, keepalive, path probing, per-peer overrides |
| `tiers.external` | last-resort RPCs: URL, token, tier, declared capabilities, health-check age |
| `routing` | sync tolerance, lag grace and hysteresis, multi-broadcast, retry budget, timeouts, pending-txn stickiness, circuit breaker |

## Development

```sh
make build          # bin/algod-loadb-mesh (static binary, no cgo)
make test           # unit, adapter and multi-agent simulator tests
make race
make lint
make dev            # fake fleet in one process
make contract       # rebuild the registry app (needs Node 22+, AlgoKit)
make contract-test  # contract tests against `algokit localnet`
```

```
cmd/algod-loadb-mesh   CLI: run, config, check-node, registry, dev
internal/domain        pure core: types, classify, eligible, select, health, codecs
internal/ports         interfaces the services depend on
internal/app           services: monitor, directory, router, registry sync, stats
internal/adapters      algod REST, data dir, UDP/in-memory gossip, proxy, pass-through, registries, metrics
internal/agent         composition root
internal/config        config loading, defaults and bundles
internal/fakealgod     in-process algod simulator used by tests and `dev`
internal/devfleet      N fake nodes + N agents wired together
test/sim               multi-agent scenarios
test/contract          Go registry client against algokit localnet
contract               registry application (AlgoKit project, Algorand TypeScript)
deploy                 example configs, systemd unit, autoconfig and setup scripts
```

Design notes for the multi-interface mesh and the algod pass-through are in [MULTI.md](MULTI.md) and [MULTI_ALGOD.md](MULTI_ALGOD.md). The registry contract is described in [contract/README.md](contract/README.md).

**Releases** are cut from version tags. Pushing `v1.2.3` builds the linux amd64/arm64 binaries and `.deb` packages ([nfpm.yaml](nfpm.yaml)), publishes them with `checksums.txt` ([release.yml](.github/workflows/release.yml)), fast-forwards the `stable` branch so the scripts the installer fetches match the binary, and publishes the packages to the signed apt repository ([apt-publish.sh](.github/apt-publish.sh)).
