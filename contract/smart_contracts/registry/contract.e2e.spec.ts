import { Config } from '@algorandfoundation/algokit-utils'
import { algorandFixture } from '@algorandfoundation/algokit-utils/testing'
import { nullLogger } from '@algorandfoundation/algokit-utils/types/logging'
import algosdk, { Address } from 'algosdk'
import * as fs from 'node:fs'
import * as path from 'node:path'
import { beforeAll, beforeEach, describe, expect, test } from 'vitest'
import { APP_SPEC } from '../artifacts/registry/RegistryClient'
import {
  BOX_REF_BUDGET,
  BOX_REFS,
  boxKey,
  boxMbr,
  boxRefs,
  callFee,
  CREATE_NOTE,
  createRegistry,
  removeParams,
  enc,
  MAX_APP_ARGS_LEN,
  MAX_PART_LEN,
  maxValueLen,
  MIN_FEE,
  putArgsLen,
  putParams,
  RegistryClient,
} from './client'

// Contract tests against algokit localnet (`algokit localnet start`), which
// must run consensus v42 or later (`algokit localnet reset --update`).
// Each claim of docs/REGISTRY_CONTRACT.md §7–§8 has a test here.

const PUT = 'put(string,byte[],byte[],byte[],byte[])void'
const REMOVE = 'remove(string)void'

const selector = (signature: string) => algosdk.ABIMethod.fromSignature(signature).getSelector()
const abiBytes = (b: Uint8Array) => new algosdk.ABIArrayDynamicType(new algosdk.ABIUintType(8)).encode(Array.from(b))
const abiString = (s: string) => new algosdk.ABIStringType().encode(s)

const artifacts = path.resolve(__dirname, '../artifacts/registry')
const programs = () => ({
  approval: new Uint8Array(fs.readFileSync(path.join(artifacts, 'Registry.approval.bin'))),
  clear: new Uint8Array(fs.readFileSync(path.join(artifacts, 'Registry.clear.bin'))),
})

describe('registry contract', () => {
  const localnet = algorandFixture({ testAccountFunding: (100).algo() })
  beforeAll(() => Config.configure({ logger: nullLogger })) // rejections below are expected
  beforeEach(localnet.newScope, 30_000)

  const setup = async () => {
    const { algorand, testAccount: creator } = localnet.context
    const client = await createRegistry(algorand, creator)
    await client.appClient.fundAppAccount({ amount: (10).algo() }) // a 16 KB box needs ~6.6 ALGO
    return { algorand, creator, client, appId: client.appId, appAddress: client.appAddress }
  }

  const stranger = async (client: RegistryClient) => {
    const s = await localnet.context.generateAccount({ initialFunds: (1).algo() })
    return { addr: s.addr, client: client.clone({ defaultSender: s.addr }) }
  }

  const bytes = (n: number, fill = 0xab) => new Uint8Array(n).fill(fill)
  const records = async (client: RegistryClient) => client.state.box.records.getMap()

  describe('interface', () => {
    test('exposes put and remove as ARC-4 methods, and bare create, update and delete', () => {
      expect(APP_SPEC.arcs).toEqual(expect.arrayContaining([22, 28]))
      expect(APP_SPEC.methods.map((m) => new algosdk.ABIMethod(m).getSignature())).toEqual([PUT, REMOVE])
      expect(APP_SPEC.bareActions).toEqual({ create: ['NoOp'], call: ['DeleteApplication', 'UpdateApplication'] })
      expect(APP_SPEC.state.maps.box.records).toMatchObject({ keyType: 'AVMString', valueType: 'AVMBytes', prefix: Buffer.from('n').toString('base64') })
      expect(Buffer.from(selector(PUT)).toString('hex')).toBe('4cc15367')
      expect(Buffer.from(selector(REMOVE)).toString('hex')).toBe('8e8900b9')
    })
  })

  describe('creation', () => {
    test('is a bare create with zero schemas', async () => {
      const { algorand, client, creator } = await setup()
      const app = await algorand.app.getById(client.appId)
      const { approval, clear } = programs()

      expect(app.creator.toString()).toBe(creator.toString())
      expect(app.approvalProgram).toEqual(approval)
      expect(app.clearStateProgram).toEqual(clear)
      expect([app.globalInts, app.globalByteSlices, app.localInts, app.localByteSlices]).toEqual([0, 0, 0, 0])
      expect(app.extraProgramPages ?? 0).toBe(0)
    })

    test('carries the informational note', async () => {
      const { algorand, testAccount } = localnet.context
      const { approval, clear } = programs()
      const result = await algorand.send.appCreate({ sender: testAccount, approvalProgram: approval, clearStateProgram: clear, note: CREATE_NOTE })
      expect(new TextDecoder().decode(result.transaction.note)).toBe(CREATE_NOTE)
    })

    test('a method call cannot create', async () => {
      const { algorand, testAccount } = localnet.context
      const { approval, clear } = programs()
      await expect(
        algorand.send.appCreate({ sender: testAccount, approvalProgram: approval, clearStateProgram: clear, args: [selector(REMOVE), abiString('k44')] }),
      ).rejects.toThrow(/logic eval error/)
    })
  })

  describe('put', () => {
    test('stores the value verbatim in box n<id>', async () => {
      const { algorand, client } = await setup()
      const value = bytes(500)

      await client.send.put(putParams('k44', value))

      expect(await algorand.app.getBoxValue(client.appId, boxKey('k44'))).toEqual(value)
      expect(await records(client)).toEqual(new Map([['k44', value]]))
    })

    test('is a full replace, growing and shrinking the box', async () => {
      const { client } = await setup()
      for (const len of [300, 1900, 29, MAX_PART_LEN, MAX_PART_LEN + 1, 16000, 2048, 2049, 600]) {
        const value = bytes(len, len & 0xff)
        await client.send.put(putParams('k44', value))
        expect((await records(client)).get('k44')).toEqual(value)
      }
    })

    test('concatenates parts of any size in order', async () => {
      const { client } = await setup()
      const value = Uint8Array.from({ length: 1 + MAX_PART_LEN + 17 + 3000 }, (_, i) => i % 251)
      const cut = [1, 1 + MAX_PART_LEN, 1 + MAX_PART_LEN + 17]

      await client.send.put(putParams('k44', [value.subarray(0, cut[0]), value.subarray(cut[0], cut[1]), value.subarray(cut[1], cut[2]), value.subarray(cut[2])]))
      expect((await records(client)).get('k44')).toEqual(value)

      // empty parts contribute nothing, wherever they are
      const empty = new Uint8Array()
      await client.send.put(putParams('k44', [empty, value.subarray(0, MAX_PART_LEN), empty, value.subarray(MAX_PART_LEN)]))
      expect((await records(client)).get('k44')).toEqual(value)
    })

    test('accepts an empty value', async () => {
      const { client } = await setup()
      await client.send.put(putParams('empty', new Uint8Array()))
      expect(await records(client)).toEqual(new Map([['empty', new Uint8Array()]]))
    })

    test('keeps records independent', async () => {
      const { client } = await setup()
      await client.send.put(putParams('a', bytes(10, 1)))
      await client.send.put(putParams('b', bytes(20, 2)))
      await client.send.remove(removeParams('a'))

      expect(await records(client)).toEqual(new Map([['b', bytes(20, 2)]]))
    })
  })

  describe('remove', () => {
    test('removes the box', async () => {
      const { algorand, client } = await setup()
      await client.send.put(putParams('k44', bytes(100)))
      await client.send.remove(removeParams('k44'))
      expect(await algorand.app.getBoxNames(client.appId)).toEqual([])
    })

    test('is idempotent', async () => {
      const { client } = await setup()
      await client.send.remove(removeParams('never-written'))
      await client.send.remove(removeParams('never-written'))
    })
  })

  describe('rejections', () => {
    const raw = (appId: bigint, sender: Address, args: Uint8Array[]) => ({ sender, appId, args, boxReferences: boxRefs('k44'), populateAppCallResources: false })

    test('unknown method selector', async () => {
      const { algorand, creator, appId } = await setup()
      await expect(algorand.send.appCall(raw(appId, creator, [selector('get(string)byte[]'), abiString('k44')]))).rejects.toThrow(/logic eval error/)
    })

    test('the former raw interface', async () => {
      const { algorand, creator, appId } = await setup()
      await expect(algorand.send.appCall(raw(appId, creator, [enc('put'), boxKey('k44'), bytes(10)]))).rejects.toThrow(/logic eval error/)
      await expect(algorand.send.appCall(raw(appId, creator, [enc('del'), boxKey('k44')]))).rejects.toThrow(/logic eval error/)
    })

    test('put with missing parts', async () => {
      const { algorand, creator, appId } = await setup()
      await expect(algorand.send.appCall(raw(appId, creator, [selector(PUT), abiString('k44'), abiBytes(bytes(10))]))).rejects.toThrow(/logic eval error/)
    })

    test('args whose ABI length prefix does not match', async () => {
      const { algorand, creator, appId } = await setup()
      const part = abiBytes(bytes(10))
      const truncated = part.subarray(0, part.length - 1)
      const empty = abiBytes(new Uint8Array())
      await expect(algorand.send.appCall(raw(appId, creator, [selector(PUT), abiString('k44'), truncated, empty, empty, empty]))).rejects.toThrow(/invalid number of bytes/)
      await expect(algorand.send.appCall(raw(appId, creator, [selector(REMOVE), enc('k44')]))).rejects.toThrow(/invalid number of bytes/)
      expect(await algorand.app.getBoxNames(appId)).toEqual([])
    })

    test('a bare no-op call', async () => {
      const { algorand, creator, appId } = await setup()
      await expect(algorand.send.appCall({ sender: creator, appId, populateAppCallResources: false })).rejects.toThrow(/rejected by ApprovalProgram/)
    })

    test('opt-in and close-out, even from the creator', async () => {
      const { algorand, creator, appId } = await setup()
      await expect(algorand.send.appCall({ sender: creator, appId, onComplete: algosdk.OnApplicationComplete.OptInOC })).rejects.toThrow(/logic eval error/)
      await expect(algorand.send.appCall({ sender: creator, appId, onComplete: algosdk.OnApplicationComplete.CloseOutOC })).rejects.toThrow(/logic eval error/)
    })

    test('methods with another on-completion', async () => {
      const { algorand, creator, appId } = await setup()
      const empty = abiBytes(new Uint8Array())
      const put = [selector(PUT), abiString('k44'), abiBytes(bytes(10)), empty, empty, empty]
      await algorand.send.appCall(raw(appId, creator, put)) // valid as a no-op
      await expect(algorand.send.appCall({ ...raw(appId, creator, put), onComplete: algosdk.OnApplicationComplete.OptInOC })).rejects.toThrow(/logic eval error/)
    })

    describe('non-creator', () => {
      test('put', async () => {
        const { client } = await setup()
        const s = await stranger(client)
        await expect(s.client.send.put(putParams('k44', bytes(10)))).rejects.toThrow(/creator only/)
        expect((await records(client)).size).toBe(0)
      })

      test('remove', async () => {
        const { client } = await setup()
        await client.send.put(putParams('k44', bytes(10)))
        const s = await stranger(client)
        await expect(s.client.send.remove(removeParams('k44'))).rejects.toThrow(/creator only/)
        expect((await records(client)).get('k44')).toEqual(bytes(10))
      })

      test('update and delete of the application', async () => {
        const { algorand, client } = await setup()
        const s = await stranger(client)
        const { approval, clear } = programs()
        await expect(algorand.send.appUpdate({ sender: s.addr, appId: client.appId, approvalProgram: approval, clearStateProgram: clear })).rejects.toThrow(
          /creator only/,
        )
        await expect(algorand.send.appDelete({ sender: s.addr, appId: client.appId })).rejects.toThrow(/creator only/)
      })
    })
  })

  describe('creator application lifecycle', () => {
    test('update keeps records', async () => {
      const { algorand, creator, client } = await setup()
      await client.send.put(putParams('k44', bytes(10)))
      const { approval, clear } = programs()
      await algorand.send.appUpdate({ sender: creator, appId: client.appId, approvalProgram: approval, clearStateProgram: clear })
      expect((await records(client)).get('k44')).toEqual(bytes(10))
    })

    test('delete', async () => {
      const { algorand, client } = await setup()
      await client.send.delete.bare()
      await expect(algorand.app.getById(client.appId)).rejects.toThrow()
    })
  })

  describe('minimum balance', () => {
    const minBalance = async (algorand: typeof localnet.algorand, addr: Address) => (await algorand.account.getInformation(addr)).minBalance.microAlgo

    test('each box costs 2500 + 400 × (len(key) + len(value)), released on remove', async () => {
      const { algorand, client, appAddress } = await setup()
      const key = boxKey('k44')
      const base = await minBalance(algorand, appAddress)

      await client.send.put(putParams('k44', bytes(500)))
      expect((await minBalance(algorand, appAddress)) - base).toBe(boxMbr(key.length, 500))

      await client.send.put(putParams('k44', bytes(40)))
      expect((await minBalance(algorand, appAddress)) - base).toBe(boxMbr(key.length, 40))

      await client.send.remove(removeParams('k44'))
      expect(await minBalance(algorand, appAddress)).toBe(base)
    })

    test('put fails until the app account is funded', async () => {
      const { algorand, testAccount: creator } = localnet.context
      const client = await createRegistry(algorand, creator)
      const value = bytes(500)

      await expect(client.send.put(putParams('k44', value))).rejects.toThrow(/balance .* below min/)

      // §8: required = min-balance + mbr(new box) + 100 000, topped up by the sync account
      const info = await algorand.account.getInformation(client.appAddress)
      const required = info.minBalance.microAlgo + boxMbr(boxKey('k44').length, value.length) + 100_000n
      await client.appClient.fundAppAccount({ amount: (required - info.balance.microAlgo).microAlgo() })

      await client.send.put(putParams('k44', value))
      expect((await records(client)).get('k44')).toEqual(value)
    })
  })

  describe('transaction limits (consensus v42)', () => {
    test('args of put are selector, id and four parts, each with a 2-byte length prefix', async () => {
      const { client } = await setup()
      const result = await client.send.put(putParams('k44', bytes(10)))
      const args = result.transaction.applicationCall!.appArgs
      expect(args.map((a) => a.length)).toEqual([4, 2 + 3, 2 + 10, 2, 2, 2])
      expect(args.reduce((n, a) => n + a.length, 0)).toBe(putArgsLen('k44', 10))
    })

    test(`the largest value is ${MAX_APP_ARGS_LEN} - ${putArgsLen('', 0)} - len(id)`, async () => {
      const { client } = await setup()
      const id = 'x'.repeat(48) // longest allowed id
      const max = maxValueLen(id)
      expect(max).toBe(16322)

      await client.send.put(putParams(id, bytes(max)))
      expect((await records(client)).get(id)?.length).toBe(max)

      await expect(client.send.put(putParams(id, bytes(max + 1)))).rejects.toThrow(/total length is too long/)
    })

    test(`a part is limited to ${MAX_PART_LEN} bytes, one arg less its length prefix`, async () => {
      const { client } = await setup()
      const empty = new Uint8Array()
      await client.send.put(putParams('k44', [bytes(MAX_PART_LEN), empty, empty, empty]))
      await expect(client.send.put(putParams('k44', [bytes(MAX_PART_LEN + 1), empty, empty, empty]))).rejects.toThrow(/length is too long/)
    })

    test('arg bytes beyond 2048 cost 0.0001 min fee each, rounded up', async () => {
      const { client } = await setup()
      for (const argsLen of [2048, 2049, 3048, 16384]) {
        const value = bytes(argsLen - putArgsLen('k44', 0))
        const fee = callFee(MIN_FEE, argsLen)
        await expect(client.send.put(putParams('k44', value, { fee: fee - 1n }))).rejects.toThrow(/fees is less than/)
        await client.send.put(putParams('k44', value, { fee }))
      }
      expect([2048, 2049, 3048, 16384].map((n) => callFee(MIN_FEE, n))).toEqual([1000n, 1001n, 1100n, 2434n])
    })

    test(`${BOX_REFS} box references cover any put and remove`, async () => {
      const { client } = await setup()
      const max = maxValueLen('k44')
      await client.send.put(putParams('k44', bytes(max)))
      await client.send.put(putParams('k44', bytes(1)))
      await client.send.put(putParams('k44', bytes(max)))
      await client.send.remove(removeParams('k44'))
    })

    test(`one reference budgets ${BOX_REF_BUDGET} bytes of box value, excluding the key`, async () => {
      const { client } = await setup()
      await client.send.put(putParams('k44', bytes(BOX_REF_BUDGET), { refs: 1 }))
      await expect(client.send.put(putParams('k44', bytes(BOX_REF_BUDGET + 1), { refs: 1 }))).rejects.toThrow(/write budget exceeded/)
    })

    test('references must also cover the value being replaced or removed', async () => {
      const { client } = await setup()
      await client.send.put(putParams('k44', bytes(3000)))

      // A small new value does not make a single reference enough: box_del reads the old box.
      await expect(client.send.put(putParams('k44', bytes(10), { refs: 1 }))).rejects.toThrow(/read budget exceeded/)
      await expect(client.send.remove(removeParams('k44', { refs: 1 }))).rejects.toThrow(/read budget exceeded/)
    })
  })
})
