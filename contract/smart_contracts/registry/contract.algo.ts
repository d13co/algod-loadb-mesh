import { abimethod, assert, baremethod, BoxMap, bytes, Contract, Global, Txn, uint64 } from '@algorandfoundation/algorand-typescript'

/**
 * algod-loadb-mesh fleet registry (docs/REGISTRY_CONTRACT.md §7).
 *
 * ARC-4 application. Boxes named `n<tag>` hold one sealed NodeRecord each,
 * where the tag is a 16-byte keyed hash of the node id computed off chain, so
 * the chain never sees node ids. The program never looks inside the tag or
 * the record. Only the creator (the sync account) may call a method, update
 * or delete the application.
 */
export class Registry extends Contract {
  /** Sealed NodeRecords by the tag of their node id. */
  records = BoxMap<bytes<16>, bytes>({ keyPrefix: 'n' })

  /**
   * Stores the concatenation of the parts as the record under `tag`,
   * replacing any previous record.
   *
   * The value arrives in parts because one application arg is limited to
   * 4096 bytes while all args of a call may total 16384 (consensus v42), so
   * four parts carry any value that fits in a call. Unused parts are empty.
   */
  @abimethod()
  put(tag: bytes<16>, part0: bytes, part1: bytes, part2: bytes, part3: bytes): void {
    this.onlyCreator()
    const box = this.records(tag)
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

  /** Removes the record under `tag`. Removing a missing record is not an error. */
  @abimethod()
  remove(tag: bytes<16>): void {
    this.onlyCreator()
    this.records(tag).delete()
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
