import { afterAll, beforeAll, expect, test } from "bun:test"
import { cp, mkdir, mkdtemp, rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import { dirname, join, resolve } from "node:path"

// release.yaml's claude_envoy_build runs .github/scripts/release-claude-envoy-version.sh at its
// step "Set release version", which writes the version the run releases into all five manifests
// of this package and its Codex package, and the tree's daemon API contract
// (scripts/daemon-contract.sh) into package.json. It names them by their paths from the
// repository root, so it runs here from a root of its own, which holds the two scripts, the five
// manifests and the contract fixtures, as packed-skills.test.ts runs the release's check of the
// packed tarball.
const repoRoot = resolve(import.meta.dir, "..", "..", "..")
const script = join(".github", "scripts", "release-claude-envoy-version.sh")
const contractScript = join("scripts", "daemon-contract.sh")
const daemonApiFixtures = join("packages", "contracts", "fixtures", "daemon-api")
/** The root's contract: L, the greatest number of its `released-contracts.json`, 232, plus its one
 *  entry fixture. */
const contract = 233
/** The manifest the script rewrites first. */
const packageJson = "packages/claude-envoy/package.json"
const manifests = [
  packageJson,
  "packages/claude-envoy/.claude-plugin/plugin.json",
  "packages/claude-envoy/.omp-plugin/plugin.json",
  "packages/claude-envoy/codex/package.json",
  "packages/claude-envoy/codex/.codex-plugin/plugin.json",
]
const committed = { name: "@sjawhar/claude-envoy", version: "1.0.0" }
const committedText = `${JSON.stringify(committed, null, 2)}\n`

type Run = { readonly exitCode: number; readonly stdout: string; readonly stderr: string }

let scratch = ""

beforeAll(async () => {
  scratch = await mkdtemp(join(tmpdir(), "claude-envoy-release-version-"))
})

afterAll(async () => {
  await rm(scratch, { recursive: true, force: true })
})

/** A root at <scratch>/<name> holding the two scripts, the contract fixtures of a tree of one
 *  entry over L, and the five manifests, each with the text `texts` gives it, or the committed
 *  one. */
async function releaseRoot(name: string, texts: Record<string, string>): Promise<string> {
  const root = join(scratch, name)
  for (const file of [script, contractScript]) {
    await mkdir(join(root, dirname(file)), { recursive: true })
    await cp(join(repoRoot, file), join(root, file))
  }
  await mkdir(join(root, daemonApiFixtures, "contracts"), { recursive: true })
  await Bun.write(join(root, daemonApiFixtures, "released-contracts.json"), "[46, 48, 232]\n")
  await Bun.write(join(root, daemonApiFixtures, "contracts", "20261125010101_probe.json"), "{}\n")
  for (const manifest of manifests) {
    await mkdir(join(root, dirname(manifest)), { recursive: true })
    await Bun.write(join(root, manifest), texts[manifest] ?? committedText)
  }
  return root
}

/** The script run in `root` as the step runs it, with VERSION in its environment. */
async function setVersion(root: string, version: string): Promise<Run> {
  const child = Bun.spawn(["bash", script], {
    cwd: root,
    env: { ...process.env, VERSION: version },
    stdout: "pipe",
    stderr: "pipe",
  })
  const [exitCode, stdout, stderr] = await Promise.all([
    child.exited,
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
  ])
  return { exitCode, stdout, stderr }
}

test("the release's version is written into all five manifests", async () => {
  const root = await releaseRoot("valid", {})
  const { exitCode, stderr } = await setVersion(root, "9.9.9")
  const written: unknown[] = await Promise.all(
    manifests.map(async (manifest) => JSON.parse(await Bun.file(join(root, manifest)).text())),
  )
  const released = { ...committed, version: "9.9.9" }
  expect({ exitCode, stderr, written }).toEqual({
    exitCode: 0,
    stderr: "",
    written: manifests.map((manifest) =>
      manifest === packageJson ? { ...released, legion: { daemonApiVersion: contract } } : released,
    ),
  })
})

// A manifest jq cannot parse stops the step there. Written as `jq … > tmp.json && mv …`, the
// failing jq was not the last command of its list, so `set -e` let the loop go on: the step
// passed with that manifest unversioned, and the four after it were written.
test("a manifest jq cannot parse fails the step before the next manifest is written", async () => {
  const root = await releaseRoot("unparsable", { [packageJson]: "not json\n" })
  const { exitCode, stderr } = await setVersion(root, "9.9.9")
  const others = await Promise.all(
    manifests.slice(1).map((manifest) => Bun.file(join(root, manifest)).text()),
  )
  expect({ failed: exitCode !== 0, parseError: /parse error/.test(stderr), others }).toEqual({
    failed: true,
    parseError: true,
    others: manifests.slice(1).map(() => committedText),
  })
})
