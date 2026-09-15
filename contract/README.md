# algod-loadb-mesh registry contract

AlgoKit project for the registry application described in
[`docs/REGISTRY_CONTRACT.md`](../docs/REGISTRY_CONTRACT.md). The contract is
written in Algorand TypeScript and compiled with puya-ts to TEAL (AVM 10).

```
smart_contracts/registry/
  contract.algo.ts         the ARC-4 contract
  client.ts                §8 call helpers (parts, box refs, fees, MBR) over the typed client, for tests and deploy
  contract.e2e.spec.ts     contract tests against LocalNet
  deploy-config.ts         `algokit project deploy`: create + fund a registry app
smart_contracts/artifacts/registry/
  Registry.{approval,clear}.{teal,bin}   build output, committed
  Registry.arc56.json                    ARC-56 app spec
  RegistryClient.ts                      typed client generated from the spec
```

The interface is two ARC-4 methods, both creator only, over the box map
`records` (`node:<id>` → sealed record):

- `put(string id, byte[] part0, byte[] part1, byte[] part2, byte[] part3)void`
  stores the concatenated parts as the record of `id`, replacing any previous
  one.
- `remove(string id)void` deletes it.

Creation is a bare call; update and delete are bare calls restricted to the
creator. The value comes in four parts because one arg is limited to 4096
bytes while a call's args may total 16384 (consensus v42), so records can
reach about 16 KB.

## Working on it

Needs Node 22+, AlgoKit CLI and Docker.

```sh
npm install
algokit localnet start # tests need consensus v42: `algokit localnet reset --update` on an old image
npm run build          # compile to smart_contracts/artifacts and generate the typed client
npm test               # contract tests on LocalNet
```

From the repository root, `make contract` builds and copies the programs and
ARC-56 spec into `internal/adapters/registryalgo/program/`, where the Go
client embeds them
(`go test ./internal/adapters/registryalgo` fails if the copies drift), and
`make contract-test` runs these tests plus the Go client's contract tests.

## Deploying

The Go CLI's `algod-loadb-mesh registry init` is the normal way to create a
registry. For an ad-hoc deploy with the sync account as `DEPLOYER`:

```sh
DEPLOYER_MNEMONIC="..." ALGOD_SERVER=... ALGOD_TOKEN=... algokit project deploy testnet
```

Every run creates a new application; set the printed id as `registry.app_id`.
