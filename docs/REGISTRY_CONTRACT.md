# algod-loadb-mesh registry contract specification

Version 1 — 2026-09-14. Describes the Algorand application that stores the
fleet directory for algod-loadb-mesh v2, the box format, the cryptography around
it, and the client protocol. Implementation: `contract/` (the application,
an AlgoKit project), `internal/adapters/registryalgo` (embedded programs,
transactions) and `internal/domain/codec.go` (record format).

## 1. Purpose and scope

The registry answers one question for every agent: **which nodes exist, where
are they, and how do I reach them.** It holds static facts only. Liveness,
sync state and capabilities travel over gossip and are never written on
chain.

Requirements it satisfies:

- Any agent can read the full fleet through any synced algod, with no
  indexer and no archival node.
- Only holders of one fleet-wide secret (the *sync account*) can read the
  contents or change them. Everyone else sees opaque bytes.
- Writes are rare (a node joins, changes its endpoint or token, or leaves)
  and cost cents.
- A dead node does not disappear on its own; removal is an explicit act.

Non-goals: per-agent keys, roles, revocation of individual readers, hiding
the *number* of nodes or the *ids* (box names are plaintext, see §9).

## 2. On-chain structure

| Item | Value |
|---|---|
| Application | one per fleet; the id is `registry.app_id` in every agent's config |
| Creator | the sync account (§5); it is the only account allowed to call the app after creation |
| Global state schema | 0 uints, 0 byte slices |
| Local state schema | 0 / 0 (no opt-in exists) |
| Extra program pages | 0 |
| Boxes | one per node record, key `n<id>`, value = sealed record (§4) |
| Creation note | `algod-loadb-mesh registry` (informational) |

A registry app may live on a different network than the nodes it describes:
records carry the node's genesis id, and agents ignore records for other
networks. `registry.algod_url` selects the algod used to read and write it;
by default it is the local node.

## 3. Box naming

```
key   = "n" || id
id    = 1..48 bytes, the NodeRecord.ID (stable short name, e.g. "k44")
```

Box keys are therefore 2..49 bytes, within Algorand's 64-byte limit; the
one-byte prefix keeps the per-box minimum balance down (§8). Readers list
boxes with `GET /v2/applications/{app}/boxes`, keep those whose name starts
with `n`, and fetch each with `GET /v2/applications/{app}/box?name=b64:<key>`.
Boxes with any other first byte are reserved for future use and must be
ignored; a future box kind must therefore not start with `n`.

## 4. Box value format

```
offset  size  field
0       1     version, currently 0x01
1       12    nonce, random per write
13      n+16  AES-256-GCM ciphertext of the plaintext record, with 16-byte tag
```

- Cipher: AES-256-GCM (Go `crypto/cipher.NewGCM`).
- Key: the 32-byte *sync key* of §5.
- Nonce: 12 random bytes from the writer's CSPRNG, never reused with the
  same key by construction (96-bit random nonces; the fleet writes at most a
  few thousand records over its lifetime).
- Additional authenticated data: the **box key** (`n<id>`). A ciphertext
  copied into another node's box fails to open, so a key holder cannot be
  tricked into treating record A's endpoints as node B's.
- Plaintext: the JSON encoding of `NodeRecord` (§6), UTF-8, no framing.

Readers must reject a value whose version byte is unknown, whose length is
below 13 + 16 bytes, whose tag does not verify, whose JSON does not parse,
or whose decoded `id` differs from the box name's id. Rejected boxes are
skipped with a warning; they never abort a listing.

Size: a typical record is 300–600 bytes. The limit is not the 32 KB box value
but what one `put` call can carry (§8): application args total at most 16384
bytes, of which the ABI encoding takes `14 + len(id)`, so the sealed value
may be at most `16370 − len(id)` bytes (16322 for a 48-byte id), about 16.3 KB
of JSON. The client refuses larger records before sending.

## 5. Keys

**Sync account.** An ordinary Algorand account. Its 25-word mnemonic (or the
32-byte seed / 64-byte private key, hex or base64) is `registry.sync_key`
on every host, normally via `registry.sync_key_file`. It:

- signs every application call and the funding payments (§8);
- is the *creator* of the app, which the approval program checks;
- is the input to the encryption key below.

`algod-loadb-mesh registry gen-key` creates one. It must hold enough ALGO for
fees plus the box minimum balances of §8.

**Rekeyed sync account.** If the sync account has been rekeyed, set
`registry.sync_address` to the account's address and `registry.sync_key` to
its current auth key. Transactions are then sent from `sync_address` (so the
creator check still passes) and signed by the auth key, with the signed
transaction's `sgnr` set to the key's address. The `registry` CLI's write
subcommands first check on chain that the account's `auth-addr` matches the
key.

Everything below is derived from `sync_key`, not from the account address:
rekeying to a different key changes the encryption key and every agent key.
Existing records become unreadable to hosts on the new key, so switch every
host at once and let the agents re-register (or re-`add` static entries).

**Sync key (encryption).**

```
sync_key = HKDF-SHA256(IKM = seed[0:32], salt = "algod-loadb-mesh", info = "registry-v1", L = 32)
```

where `seed` is the ed25519 seed of the sync account. Deriving through HKDF
means the on-chain signing key is never used directly as an AEAD key, and a
future format can use a different `info` string without changing the account.

**Agent heartbeat keys.** Not part of the contract, but stored in records:

```
agent_seed = HKDF-SHA256(IKM = seed[0:32], salt = "algod-loadb-mesh", info = "agent-key:" || id, L = 32)
agent_key  = ed25519.NewKeyFromSeed(agent_seed)
```

Every holder of the sync seed can derive every agent's key. That is the
requested trust model (one secret, no RBAC): anyone who can rewrite the
registry can already impersonate any node, so per-agent keys would add no
security today. The record carries the public key regardless, so a later
version can switch to independently generated keys by changing only key
management, not the record format. Fleets that use `registry.type: static`
derive the same keys from `mesh.shared_secret` instead of the sync seed.

## 6. Record schema

JSON object, field order as emitted by Go's encoder (readers must not depend
on order).

| Field | Type | Meaning |
|---|---|---|
| `id` | string, 1..48 bytes | node name; must equal the box name suffix |
| `network` | string | genesis id of the node, e.g. `mainnet-v1.0`; agents only peer within their own network |
| `endpoints` | array of string, ≥ 1 | algod REST base URLs other agents dial, preferred order (e.g. two WireGuard addresses) |
| `token` | string | the node's `algod.token`; grants full API access to the node |
| `agent.addr` | string | `host:port` the agent receives UDP heartbeats on |
| `agent.pubkey` | base64 bytes (32) | ed25519 key that signs the node's heartbeats |
| `tier` | integer ≥ 0 | routing tier; lower is preferred; may be overridden locally |
| `tags` | array of string, optional | free-form labels |
| `declared` | object, optional | manual capability overrides: `archival {kind, n}`, `developer_api`, `follow_mode` |
| `version` | integer | monotonically increasing per node; a writer sets `existing.version + 1` |
| `updated_at` | integer | the round the writer observed when it wrote |

Validation (`NodeRecord.Validate`) rejects an empty id, an id over 48 bytes,
an empty network, no endpoints, or a negative tier. Unknown fields are
ignored on read so a newer agent can add fields without breaking older ones;
removing or renaming a field requires a new value version byte.

## 7. Programs

### Source

The application is an ARC-4 contract in the AlgoKit project `contract/`,
written in Algorand TypeScript
(`contract/smart_contracts/registry/contract.algo.ts`) and compiled by puya-ts
to TEAL targeting AVM 10. The build also emits its ARC-56 description
(`Registry.arc56.json`: methods, bare actions, the `records` box map) and a
typed TypeScript client generated from it. The program uses only AVM 8
opcodes; the 16 KB record size depends on the network running consensus v42
(AVM 13) or later, not on the program version. The build output
(`Registry.{approval,clear}.{teal,bin}` and `Registry.arc56.json`) is
committed and copied by `make contract` into
`internal/adapters/registryalgo/program/`, where the Go client embeds it with
`go:embed` and takes the method definitions from the ARC-56 file; a unit test
fails if the copies differ from the build.

### Interface

```ts
export class Registry extends Contract {
  records = BoxMap<string, bytes>({ keyPrefix: 'n' })

  @abimethod()
  put(id: string, part0: bytes, part1: bytes, part2: bytes, part3: bytes): void {
    this.onlyCreator()
    const box = this.records(id)
    box.delete() // the size may change, so start over
    box.create({ size: part0.length + part1.length + part2.length + part3.length })
    let offset: uint64 = 0
    box.replace(offset, part0)
    offset += part0.length
    box.replace(offset, part1)
    offset += part1.length
    box.replace(offset, part2)
    offset += part2.length
    box.replace(offset, part3)
  }

  @abimethod()
  remove(id: string): void {
    this.onlyCreator()
    this.records(id).delete()
  }

  @baremethod({ allowActions: 'UpdateApplication' })
  update(): void {
    this.onlyCreator()
  }

  @baremethod({ allowActions: 'DeleteApplication' })
  destroy(): void {
    this.onlyCreator()
  }

  private onlyCreator(): void {
    assert(Txn.sender === Global.creatorAddress, 'creator only')
  }
}
```

| Call | Selector | Effect |
|---|---|---|
| bare create (NoOp, no args) | | create the application |
| `put(string,byte[],byte[],byte[],byte[])void` | `4cc15367` | delete box `n<id>` if present, create it with size Σ len(partᵢ), write the parts in order |
| `remove(string)void` | `8e8900b9` | delete box `n<id>` if present |
| bare UpdateApplication | | replace the programs |
| bare DeleteApplication | | delete the application |

Args follow ARC-4: `ApplicationArgs[0]` is the selector, `string` and
`byte[]` are a big-endian uint16 length followed by the bytes, and the router
rejects an arg whose length prefix does not match its size. A box name is the
map prefix followed by the raw id bytes, so the program can only ever touch
boxes whose name starts with `n`.

The value travels in four parts because a single application arg, like any
AVM byte string, is limited to 4096 bytes, so a part holds at most 4094. Four
parts of 4094 bytes exceed the largest value the args limit allows (§4), so
four always suffice; unused parts are empty. The compiled TEAL is
`contract/smart_contracts/artifacts/registry/Registry.approval.teal`.

Semantics the program guarantees:

1. Only the creator can call `put` and `remove`, or update or delete the
   application.
2. The program never inspects, decrypts or validates record contents; the
   chain stores what the key holder writes.
3. `put` is a full replace: the box ends up holding exactly the
   concatenated parts. There is no partial update and no read-modify-write
   on chain; concurrency is resolved by the `version` field off chain.
4. `remove` is idempotent.
5. Anything else is rejected: unknown selectors, method calls with an
   on-completion other than NoOp, bare NoOp calls after creation, opt-in and
   close-out (from anyone, the creator included). Clear-state is approved, as
   it always takes effect; there is no local state for it to clear.

### Clear program

```
#pragma version 10
#pragma typetrack false
main:
    pushint 1
    return
```

### Bytecode

```
approval  0a 20 03 00 02 01 31 1b 41 00 1d 31 19 14 44 31 18 44 82 02 04 4c c1
          53 67 04 8e 89 00 b9 36 1a 00 8e 02 00 26 00 b5 00 31 19 8d 06 00 11
          ff ef ff ef ff ef 00 09 00 01 00 31 18 44 88 00 b9 24 43 31 18 44 88
          00 b1 24 43 31 18 14 43 36 1a 01 49 22 59 23 08 4b 01 15 12 44 57 02
          00 36 1a 02 49 22 59 23 08 4b 01 15 12 44 57 02 00 36 1a 03 49 22 59
          23 08 4b 01 15 12 44 57 02 00 36 1a 04 49 22 59 23 08 4b 01 15 12 44
          57 02 00 36 1a 05 49 22 59 23 08 4b 01 15 12 44 57 02 00 88 00 58 80
          01 6e 4f 05 50 49 bc 48 4b 04 15 4b 04 15 4b 01 08 4b 04 15 4b 01 08
          4b 04 15 4b 01 08 4b 04 4c b9 48 4b 03 22 4f 09 bb 4b 03 4f 03 4f 07
          bb 4b 02 4f 02 4f 05 bb 4f 02 bb 24 43 36 1a 01 49 22 59 23 08 4b 01
          15 12 44 57 02 00 88 00 09 80 01 6e 4c 50 bc 48 24 43 31 00 32 09 12
          44 89                                                                 (255 bytes)
clear     0a 81 01 43
```

`registry init` submits the node's own compilation of the TEAL when the
local algod has `EnableDeveloperAPI` (via `POST /v2/teal/compile`) and logs
whether it matches the embedded bytes; otherwise it uses the embedded bytes.
Nodes must support AVM 10. Records over about 2 KB need consensus v42:
earlier protocols cap the args of a call at 2048 bytes.

## 8. Transactions

All transactions are single (ungrouped), signed by the sync account, sent
through `POST /v2/transactions` and waited on for up to 8 rounds.

**Create** (`registry init`): bare `ApplicationCall`, `ApplicationID = 0`,
no args, programs from §7, zero schemas, note `algod-loadb-mesh registry`. The new
id is taken from the confirmation's `application-index`. Immediately
afterwards the app account is funded (below) so the first `put` does not
fail.

**Put**: NoOp call of `put(id, part0, part1, part2, part3)`, where the parts
are the sealed value split in order into pieces of at most 4094 bytes, each
filled before the next (the remaining parts are empty), with eight box
references to `n<id>` (app id 0 = the called app). The args are
`4 + (2 + len(id)) + 4 × 2 + len(value)` bytes. Protocol limits as of
consensus v42:

| Limit | Value | Consequence |
|---|---|---|
| Summed length of application args | 16384 bytes | value ≤ `16370 − len(id)` (§4); checked by the client before funding or sending |
| Length of one arg | 4096 bytes | a part holds at most 4094 bytes; the value is split across four |
| Arg bytes covered by the minimum fee | 2048 | see fees below |
| Box I/O per box reference | 2048 bytes | see below |
| Box references per transaction | 8 | |

Box references: the I/O budget is counted over box *values* (the key does
not count) and must cover every box the program touches — including the old
value that `box_del` reads before the new one is created. A reference count
sized to the new value alone fails whenever a record shrinks (`read budget
exceeded`). Since no box can be larger than the args limit allows,
⌈16384/2048⌉ = 8 references always suffice, and the client always sends 8.

**Remove**: NoOp call of `remove(id)` with eight references, for the same
reason.

**Funding**: boxes are paid for by the *application account*
(`GetApplicationAddress(app_id)`), which must hold

```
mbr(box) = 2500 + 400 × (len(key) + len(value))   microAlgo
```

per box on top of the account minimum. Before every `put` the client reads
the app account, computes `required = min-balance + mbr(new box) + 100 000`
and, if the balance is lower, sends a payment for the difference from the
sync account (note `algod-loadb-mesh box mbr`). `remove` releases the box's share
of the minimum balance back to the app account; it is not swept.

Fees: a call whose args total `n` bytes needs
`⌈minFee × (1 + 0.0001 × max(0, n − 2048))⌉`: one minimum fee (0.001 ALGO)
up to 2048 bytes, 0.001100 ALGO at 3048, 0.002434 ALGO at the 16384 maximum.
The client uses the larger of that and the SDK's suggested fee. Each funding
payment costs one minimum fee.

## 9. Security model

What the design protects against, and what it does not:

| Threat | Outcome |
|---|---|
| Public observer reads the chain | Sees the app, the number of boxes, the node ids in box names, record sizes and write times. Cannot read endpoints, tokens or keys. |
| Observer without the key writes a box | Rejected by the approval program (creator check). |
| Key holder or compromised host | Full read/write of the registry and, through the tokens inside it, full API access to every node. This is inherent to "one shared secret" and is bounded by the WireGuard network the endpoints live on. Rotation: create a new sync account, `registry init` a fresh app, roll the config, delete the old app. |
| Ciphertext moved between boxes | Fails authentication (box key is AAD). |
| Old value replayed into a box | Only the creator can write, so a replay needs the key; the `version` field lets agents detect regression if that ever matters. |
| Algod serving stale boxes | Reads carry the node's `last-round`; agents keep serving from their cache and refresh through any peer's algod. |
| Malformed record written by a buggy client | Skipped on read; the rest of the fleet still loads. |

The node ids being visible is accepted: they are short labels with no
routing value. If that ever changes, hashing the id into the box name is a
version-2 change of §3 only.

## 10. Client protocol

**Read** (`Registry.List`), every `registry.refresh` (default 5 min), at boot
from the disk cache first, and on demand when a heartbeat arrives from an id
the agent does not know (rate-limited to one refresh per tenth of the
period):

1. `boxes` for the app; ignore names without the `n` prefix; stop after
   1000 names (`MaxRecords`) as a runaway guard.
2. `box` for each name; a 404 between list and get means "deleted, skip".
3. Open and validate each value; skip failures with a warning.
4. Return the records plus the algod's current round; the caller persists
   them to `registry.cache` and hands them to the peer directory.

**Self-registration** (`auto_register: true`, the default): once the local
monitor knows the node's genesis id and token, the agent builds its own
record from config (`local.id`, `local.advertise_endpoints`, `local.tier`,
`local.tags`, `local.overrides`, `mesh.advertise`, derived agent key). If no
record with that id exists, or the existing one differs in any static field
(`StaticEqual` ignores `version` and `updated_at`), it writes
`version = existing.version + 1` (or 1). This happens at most once per
process lifetime; later config changes take effect on restart.

**Operator commands** (`algod-loadb-mesh registry ...`): `list` prints the
decrypted records with tokens masked unless `-show-tokens`; `add` writes a
record by hand for a node that runs no agent yet; `rm` deletes; `init`
creates the app. All need the sync key and an algod to submit through.

## 11. Versioning

- The value's first byte is the format version. Readers reject unknown
  versions; a writer must never emit a version older readers accept with a
  different meaning.
- Adding optional JSON fields is backward compatible within a version.
- The approval program has no version marker; the app itself is the unit of
  upgrade, and the ARC-56 spec describes the interface of a given build. `UpdateApplication` by the creator is allowed if a program change
  is ever needed without migrating boxes.
- The HKDF `info` strings are the key-derivation version.

## 12. Verification status

- Record codec: round trip, wrong key, wrong box name, tampering and a
  golden ciphertext prefix are unit-tested.
- Programs: the Go-embedded TEAL, bytecode and ARC-56 spec are checked
  byte-for-byte against the AlgoKit build output, and the method signatures,
  selectors and arg encoding against fixed values
  (`go test ./internal/adapters/registryalgo`).
- Contract (`contract/`, `npm test`, algokit localnet on consensus v42): the
  ARC-56 interface (methods, selectors, bare actions, box map); bare creation
  with zero schemas, and no creation through a method; `put` storing under
  `n<id>`, growing, shrinking and replacing up to 16 KB; parts of uneven
  sizes (including empty ones anywhere) concatenated in order; `remove` and
  its idempotence; rejection of unknown selectors, the former raw
  `"put"`/`"del"` args, missing parts, mismatched ABI length prefixes, bare
  NoOp calls, methods under another on-completion, opt-in and close-out, and
  every non-creator call (put, remove, update, delete); creator update (boxes
  kept) and delete; the box MBR formula and its release on `remove`; `put`
  failing on an unfunded app account and succeeding after the §8 top-up; the
  16384-byte args limit; the 4094-byte part limit; the per-byte fee surcharge
  (one µAlgo short is rejected); the 2048-byte-per-reference write budget;
  and the read budget on the replaced or removed value.
- Go client (`test/contract`, tag `contract`, `make contract-test`):
  `Create` (including algod-compiled programs equal to the embedded
  bytecode), app account funding, `Put`/`List`/`Delete` round trips, a
  record spanning several parts (with surcharged fee) and shrinking back,
  refusal of oversized records, a rekeyed sync account, and a second key
  holder being unable to write.
