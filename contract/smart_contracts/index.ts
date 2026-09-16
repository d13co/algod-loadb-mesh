import { Config } from '@algorandfoundation/algokit-utils'
import { consoleLogger } from '@algorandfoundation/algokit-utils/types/logging'
import { deploy } from './registry/deploy-config'

Config.configure({ logger: consoleLogger })

deploy().catch((e) => {
  console.error('registry deploy failed:', e)
  process.exit(1)
})
