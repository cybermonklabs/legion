import { expect, test } from "bun:test"
import { existsSync, readdirSync, readFileSync } from "node:fs"
import path from "node:path"
import { daemonContract, type PluginManifest } from "@legion/contracts/daemon-contract"

function json(relative: string): unknown {
  return JSON.parse(readFileSync(path.resolve(import.meta.dir, relative), "utf8"))
}

// A Claude Code pane registers with this build's contract as its `pluginContract`
// (src/legion-claim.ts), and the daemon refuses a registration naming another contract than its
// `DaemonAPIVersion()`, L plus its count of entries. The committed manifest names no number:
// `scripts/build.ts --out` and the release's release-claude-envoy-version.sh write the tree's, L,
// the greatest number of `released-contracts.json`, plus the count of entry fixtures, one for each
// entry the daemon registers (`TestEveryEntryHasOneFixture`). So a contract change is one entry,
// and neither side is bumped by hand. Run from this checkout, the plugin speaks that same number.
test("package.json names no daemon API contract, and the plugin speaks this tree's", () => {
  const manifest = json("../package.json") as PluginManifest
  expect(manifest.legion).toBeUndefined()
  const fixtures = path.resolve(import.meta.dir, "../../contracts/fixtures/daemon-api")
  const released = json(path.join(fixtures, "released-contracts.json")) as readonly number[]
  const contracts = path.join(fixtures, "contracts")
  const entries = existsSync(contracts)
    ? readdirSync(contracts).filter((name) => /^[0-9]{14}_.+\.json$/.test(name)).length
    : 0
  expect(daemonContract(manifest, import.meta.dir)).toBe(Math.max(...released) + entries)
})
