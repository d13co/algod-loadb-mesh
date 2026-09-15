# algod-loadb-mesh

Load balancer for [algod](https://github.com/algorand/go-algorand) REST endpoints.

This is v2, a mesh agent that runs on every algod host. Design and
rationale: [docs/V2_PLAN.md](docs/V2_PLAN.md). The original single-hub
TypeScript proxy (v1) lives in the `algod-loadb` repository until the fleet
has migrated.

## v2 in short

Each host runs one agent next to its algod. The agent:

- follows its own node with a single `wait-for-block-after` long-poll and
  reads the node's data directory for capabilities (archival window,
  developer API, follow mode), verifying the archival window empirically;
- gossips a signed heartbeat per round to the other agents over UDP (on the
  WireGuard network), so nobody polls anybody else's node;
- discovers the fleet from an Algorand application whose boxes hold
  encrypted node records (or from a static list in the config);
- routes client requests straight to the chosen algod: the local node first
  in `fallback` mode, or the best-scoring node in `loadbalancer` mode, with
  tiers and static external RPCs (e.g. Nodely) as the last resort;
- keeps v1's client-facing behaviour: `X-Algo-API-Token` auth,
  wait-for-block coalescing, capability-aware routing, sequential pending
  lookups, optional multi-broadcast, drain on shutdown, and the upstream
  response header (now `X-Algod-Loadb-Mesh-Upstream`).

## Build and test

    make build          # bin/algod-loadb-mesh (static binary, no cgo)
    make test           # unit, adapter and simulator tests
    make race
    make dev            # 3 fake nodes + 3 agents in one process

## Run on a node

1. Create the sync account once per fleet and fund it with a few ALGO:

       algod-loadb-mesh registry gen-key

2. Deploy the registry application (needs a config with `registry.sync_key_file`
   and `local.data_dir`; the local node submits the transaction):

       algod-loadb-mesh registry init -config /etc/algod-loadb-mesh/config.yaml

3. Put the printed `app_id` into every host's config (see
   [deploy/config.example.yaml](deploy/config.example.yaml)), install
   [deploy/algod-loadb-mesh.service](deploy/algod-loadb-mesh.service), start it. Agents
   register themselves on first boot and pick each other up within one
   `registry.refresh` (sooner when they hear an unknown heartbeat).

Useful commands:

    algod-loadb-mesh check-node -data-dir /var/lib/algorand -probe   # what the agent will advertise
    algod-loadb-mesh registry list -config ...                        # decrypted fleet view
    algod-loadb-mesh registry rm -config ... -id old-node             # retire a node
    curl -H 'X-Algo-API-Token: ...' http://127.0.0.1:4000/loadb/status

Agent endpoints: `/loadb/health` (no token), `/loadb/status`, `/loadb/peers`,
`/loadb/metrics` (Prometheus text).

Without a chain registry, `registry.type: static` lists peers in the config
and `mesh.shared_secret` replaces the sync key as key material
([deploy/config.static.example.yaml](deploy/config.static.example.yaml)).

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
