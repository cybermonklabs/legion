import { afterAll, beforeAll, expect, test } from "bun:test"
import { cp, mkdir, mkdtemp, rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import { dirname, join, resolve } from "node:path"

// release.yaml's claude_envoy_build runs .github/scripts/release-claude-envoy-manifest-repository.sh
// at its step "Point the manifest at the publishing repository", which writes the publishing
// repository's url into the manifests of this package and its Codex package. It names them, and
// .github/scripts/repo-paths.sh, which gives it the url, by their paths from the repository root,
// so it runs here from a root of its own, which holds the two scripts and the two manifests, as
// release-version.test.ts runs the step before it.
const repoRoot = resolve(import.meta.dir, "..", "..", "..")
const script = join(".github", "scripts", "release-claude-envoy-manifest-repository.sh")
const repoPaths = join(".github", "scripts", "repo-paths.sh")
/** The manifest the script rewrites first. */
const packageJson = "packages/claude-envoy/package.json"
const manifests = [packageJson, "packages/claude-envoy/codex/package.json"]
const committed = {
  name: "@sjawhar/claude-envoy",
  repository: { type: "git", url: "https://github.com/cybermonklabs/legion" },
}
const committedText = `${JSON.stringify(committed, null, 2)}\n`
/** The repository the run publishes from, which repo-paths.sh reads off $GITHUB_REPOSITORY. */
const publishing = "Acme/widgets"

type Run = { readonly exitCode: number; readonly stdout: string; readonly stderr: string }

let scratch = ""

beforeAll(async () => {
  scratch = await mkdtemp(join(tmpdir(), "claude-envoy-release-repository-"))
})

afterAll(async () => {
  await rm(scratch, { recursive: true, force: true })
})

/** A root at <scratch>/<name> holding the two scripts and the two manifests, each with the text
 *  `texts` gives it, or the committed one. */
async function releaseRoot(name: string, texts: Record<string, string>): Promise<string> {
  const root = join(scratch, name)
  for (const file of [script, repoPaths]) {
    await mkdir(join(root, dirname(file)), { recursive: true })
    await cp(join(repoRoot, file), join(root, file))
  }
  for (const manifest of manifests) {
    await mkdir(join(root, dirname(manifest)), { recursive: true })
    await Bun.write(join(root, manifest), texts[manifest] ?? committedText)
  }
  return root
}

/** The script run in `root` as the step runs it, on a runner whose repository is `publishing`. */
async function pointManifests(root: string): Promise<Run> {
  const child = Bun.spawn(["bash", script], {
    cwd: root,
    env: { ...process.env, GITHUB_REPOSITORY: publishing },
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

test("the publishing repository's url is written into both manifests", async () => {
  const root = await releaseRoot("valid", {})
  const { exitCode, stderr } = await pointManifests(root)
  const written: unknown[] = await Promise.all(
    manifests.map(async (manifest) => JSON.parse(await Bun.file(join(root, manifest)).text())),
  )
  const pointed = {
    ...committed,
    repository: { ...committed.repository, url: `https://github.com/${publishing}` },
  }
  expect({ exitCode, stderr, written }).toEqual({
    exitCode: 0,
    stderr: "",
    written: manifests.map(() => pointed),
  })
})

// A manifest jq cannot parse stops the step there. Main's body ran `jq … > tmp.json && mv …` in a
// loop over the two manifests: the failing jq was not the last command of its list, so `set -e`
// let the loop go on, the second manifest was written, and the step passed with the first one
// still naming the committed repository.
test("a manifest jq cannot parse fails the step before the next manifest is written", async () => {
  const root = await releaseRoot("unparsable", { [packageJson]: "not json\n" })
  const { exitCode, stderr } = await pointManifests(root)
  const others = await Promise.all(
    manifests.slice(1).map((manifest) => Bun.file(join(root, manifest)).text()),
  )
  expect({ failed: exitCode !== 0, parseError: /parse error/.test(stderr), others }).toEqual({
    failed: true,
    parseError: true,
    others: manifests.slice(1).map(() => committedText),
  })
})
