import { AlgorandClient } from '@algorandfoundation/algokit-utils'
import { Address } from 'algosdk'
import { RegistryClient, RegistryFactory } from '../artifacts/registry/RegistryClient'

// Client-side helpers mirroring docs/REGISTRY_CONTRACT.md §8, on top of the
// generated typed client. The Go agent is the production client; these exist
// for tests and `algokit project deploy`.

export const CREATE_NOTE = 'algod-loadb-mesh registry'
export const BOX_PREFIX = 'n'

// Protocol limits as of consensus v42 (AVM 13).
/** Summed length of all application args of one call. */
export const MAX_APP_ARGS_LEN = 16384
/** Arg bytes beyond this cost PER_BYTE_SURCHARGE each. */
export const FREE_APP_ARGS_LEN = 2048
/** Length of a single arg (the AVM's largest byte string). */
export const MAX_ARG_LEN = 4096
/** Fee surcharge per charged byte, in millionths of the minimum fee. */
export const PER_BYTE_SURCHARGE = 100n
/** Box I/O granted per box reference. */
export const BOX_REF_BUDGET = 2048

// ABI encoding of put(string,byte[],byte[],byte[],byte[])void.
export const SELECTOR_LEN = 4
export const LENGTH_PREFIX = 2
export const PUT_PARTS = 4
/** Largest part: one arg minus its ABI length prefix. */
export const MAX_PART_LEN = MAX_ARG_LEN - LENGTH_PREFIX

export const enc = (s: string) => new TextEncoder().encode(s)

export const boxKey = (id: string) => enc(BOX_PREFIX + id)

/** mbr(box) = 2500 + 400 × (len(key) + len(value)) microAlgo */
export const boxMbr = (keyLen: number, valueLen: number) => 2500n + 400n * BigInt(keyLen + valueLen)

/** Summed length of the args of put(id, value). */
export const putArgsLen = (id: string, valueLen: number) => SELECTOR_LEN + LENGTH_PREFIX + enc(id).length + PUT_PARTS * LENGTH_PREFIX + valueLen

/** Largest value a single put can carry for the given id. */
export const maxValueLen = (id: string) => MAX_APP_ARGS_LEN - putArgsLen(id, 0)

/**
 * References needed so any box the registry can hold may be read (box_del of
 * the old value) and written. A put can never create a box larger than
 * MAX_APP_ARGS_LEN, so this is constant.
 */
export const BOX_REFS = Math.ceil(MAX_APP_ARGS_LEN / BOX_REF_BUDGET)

type Parts = [Uint8Array, Uint8Array, Uint8Array, Uint8Array]

/** Splits a value into the four parts of put, filling each before the next. */
export function parts(value: Uint8Array): Parts {
  const part = (i: number) => value.subarray(Math.min(i * MAX_PART_LEN, value.length), Math.min((i + 1) * MAX_PART_LEN, value.length))
  return [part(0), part(1), part(2), part(3)]
}

/** Assumed minimum fee (no congestion). */
export const MIN_FEE = 1000n

/** Fee for a call whose args total argsLen bytes: minFee × (1 + 0.0001 × bytes over 2048), rounded up. */
export function callFee(minFee: bigint, argsLen: number) {
  const factor = 1_000_000n + PER_BYTE_SURCHARGE * BigInt(Math.max(0, argsLen - FREE_APP_ARGS_LEN))
  return (minFee * factor + 999_999n) / 1_000_000n
}

export async function createRegistry(algorand: AlgorandClient, sender: Address) {
  const factory = algorand.client.getTypedAppFactory(RegistryFactory, { defaultSender: sender })
  const { appClient } = await factory.send.create.bare({ note: CREATE_NOTE })
  return appClient
}

type CallOpts = { refs?: number; fee?: bigint }

/** Box references for a call on node `id`: all of them name the same box. */
export const boxRefs = (id: string, n = BOX_REFS) => Array.from({ length: n }, () => ({ appId: 0n, name: boxKey(id) }))

export function putParams(id: string, value: Uint8Array | Parts, opts: CallOpts = {}) {
  const [part0, part1, part2, part3] = value instanceof Uint8Array ? parts(value) : value
  const valueLen = part0.length + part1.length + part2.length + part3.length
  return {
    args: { id, part0, part1, part2, part3 },
    boxReferences: boxRefs(id, opts.refs),
    staticFee: (opts.fee ?? callFee(MIN_FEE, putArgsLen(id, valueLen))).microAlgo(),
    populateAppCallResources: false,
  }
}

export function removeParams(id: string, opts: CallOpts = {}) {
  return { args: { id }, boxReferences: boxRefs(id, opts.refs), populateAppCallResources: false }
}

export type { RegistryClient }
