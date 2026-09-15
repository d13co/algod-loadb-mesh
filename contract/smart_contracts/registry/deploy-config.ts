import { AlgorandClient } from '@algorandfoundation/algokit-utils'
import { createRegistry } from './client'

// `algokit project deploy <network>`: creates a new registry app owned by
// DEPLOYER (the sync account) and funds its account for the first put, like
// `algod-loadb-mesh registry init`. Every run creates a new app.
export async function deploy() {
  const algorand = AlgorandClient.fromEnvironment()
  const deployer = await algorand.account.fromEnvironment('DEPLOYER')

  const client = await createRegistry(algorand, deployer.addr)
  await client.appClient.fundAppAccount({ amount: (0.2).algo(), note: 'algod-loadb box mbr' })

  console.log(`registry app id ${client.appId} (account ${client.appAddress}), creator ${deployer.addr}`)
  console.log(`set registry.app_id: ${client.appId}`)
}
