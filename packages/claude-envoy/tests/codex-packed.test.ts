import { afterAll, beforeAll, expect, test } from "bun:test"
import {
  chmod,
  cp,
  mkdir,
  mkdtemp,
  readdir,
  readFile,
  rm,
  stat,
  symlink,
  writeFile,
} from "node:fs/promises"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"

// The Codex plugin's package, `@sjawhar/codex-envoy`, built and packed as release.yaml's
// claude_envoy_build builds and packs it (scripts/build.ts --codex-out, then `bun pm pack` of that
// directory), and read back. Its `dispatch` tool ends a command after 120 s, which
// `dispatch login --device` outlasts while it waits for a person's approval, so the package ships
// the `dispatch` command a terminal runs: bin/dispatch, the installed plugin's own, which is also
// the package's npm `bin`. The release's check of the packed tarball,
// .github/scripts/release-codex-envoy-check-packed.sh, runs here over that tarball and over
// copies of it each lacking one thing that check holds it to.
const packageRoot = resolve(import.meta.dir, "..")
const repoRoot = resolve(packageRoot, "..", "..")

type Run = { readonly exitCode: number; readonly stdout: string; readonly stderr: string }

async function run(
  command: string[],
  cwd: string,
  env: Record<string, string | undefined> = process.env,
): Promise<Run> {
  const child = Bun.spawn(command, { cwd, env, stdout: "pipe", stderr: "pipe" })
  const [exitCode, stdout, stderr] = await Promise.all([
    child.exited,
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
  ])
  return { exitCode, stdout, stderr }
}

async function ran(command: string[], cwd: string): Promise<Run> {
  const result = await run(command, cwd)
  if (result.exitCode !== 0) {
    throw new Error(`${command.join(" ")} exited ${result.exitCode}: ${result.stderr.trim()}`)
  }
  return result
}

let scratch = ""
/** The tarball `bun pm pack` made of the build, as the release hands it to npm. */
let tarball = ""
/** The version both of its manifests carry, which the release's check holds them to. */
let version = ""

beforeAll(async () => {
  scratch = await mkdtemp(join(tmpdir(), "codex-envoy-packed-"))
  const out = join(scratch, "codex-envoy")
  // The build in a process of its own, as tests/build-out.test.ts runs it: a second Bun.build in
  // a test process that has already run other test files fails reading its sources.
  const build = await run(
    [process.execPath, join(packageRoot, "scripts", "build.ts"), "--codex-out", out],
    scratch,
  )
  expect({ exitCode: build.exitCode, stderr: build.stderr }).toEqual({ exitCode: 0, stderr: "" })
  const packed = join(scratch, "packed")
  await mkdir(packed)
  await ran([process.execPath, "pm", "pack", "--destination", packed], out)
  const tarballs = (await readdir(packed)).filter((entry) => entry.endsWith(".tgz"))
  expect(tarballs).toHaveLength(1)
  tarball = join(packed, tarballs[0] as string)
  version = JSON.parse(await readFile(join(out, "package.json"), "utf8")).version
}, 180_000)

// Its verdict is the rm's own success; on a loaded host the rm of the unpacked copies takes
// seconds, past Bun's 5 s default for a hook.
afterAll(async () => {
  await rm(scratch, { recursive: true, force: true })
}, 60_000)

/** The packed tarball unpacked into <scratch>/<name>, changed by `edit`, and packed again. */
async function variant(name: string, edit: (packageDir: string) => Promise<void>): Promise<string> {
  const root = join(scratch, name)
  await mkdir(root)
  await ran(["tar", "-xzpf", tarball, "-C", root], scratch)
  await edit(join(root, "package"))
  const tar = join(scratch, `${name}.tgz`)
  await ran(["tar", "-czf", tar, "-C", root, "package"], scratch)
  return tar
}

/** What keeps `tar` from carrying a `dispatch` a terminal runs, one line per reason: none for a
 *  tarball that does. It unpacks it where npm installs a package, under
 *  <scratch>/<name>/node_modules, and runs `dispatch --help` twice: as the installed plugin's
 *  bin/dispatch, and through the link npm makes for a package bin in node_modules/.bin
 *  (`npx --package @sjawhar/codex-envoy dispatch …`), which the shim must follow to find
 *  dist/dispatch.js. */
async function dispatchProblems(tar: string, name: string): Promise<string[]> {
  const modules = join(scratch, name, "node_modules")
  const installed = join(modules, "@sjawhar", "codex-envoy")
  await mkdir(installed, { recursive: true })
  await ran(["tar", "-xzpf", tar, "-C", installed, "--strip-components=1"], scratch)
  const problems: string[] = []
  const manifest = JSON.parse(await readFile(join(installed, "package.json"), "utf8"))
  if (manifest.bin?.dispatch !== "bin/dispatch") {
    problems.push(`package.json's bin is ${JSON.stringify(manifest.bin)}, not bin/dispatch`)
  }
  const shim = join(installed, "bin", "dispatch")
  const mode = await stat(shim).then(
    (entry) => entry.mode & 0o777,
    () => null,
  )
  if (mode === null) return [...problems, "it holds no package/bin/dispatch"]
  if ((mode & 0o111) !== 0o111) {
    return [...problems, `package/bin/dispatch has mode ${mode.toString(8)}, not executable by all`]
  }
  const link = join(modules, ".bin", "dispatch")
  await mkdir(join(modules, ".bin"))
  await symlink(join("..", "@sjawhar", "codex-envoy", "bin", "dispatch"), link)
  for (const [how, path] of [
    ["bin/dispatch", shim],
    ["node_modules/.bin/dispatch", link],
  ] as const) {
    const result = await run([path, "--help"], join(scratch, name), {
      PATH: process.env["PATH"] ?? "",
      HOME: join(scratch, name),
    })
    if (
      result.exitCode !== 0 ||
      result.stderr !== "" ||
      !result.stdout.startsWith("Usage: dispatch ")
    ) {
      problems.push(`${how} --help exited ${result.exitCode}: ${result.stderr.trim()}`)
    }
  }
  return problems
}

test("the packed bin/dispatch runs the bundled CLI, as the installed plugin's and as npm's link to the package bin", async () => {
  expect(await dispatchProblems(tarball, "installed")).toEqual([])
}, 60_000)

// The control of the case above: the same check over the same tarball, less the one member.
test("a packed tarball without bin/dispatch, or with one no user can run, fails that check", async () => {
  const without = await variant("without-bin", (dir) => rm(join(dir, "bin"), { recursive: true }))
  expect(await dispatchProblems(without, "installed-without-bin")).toEqual([
    "it holds no package/bin/dispatch",
  ])
  const unrunnable = await variant("unrunnable-bin", (dir) =>
    chmod(join(dir, "bin", "dispatch"), 0o644),
  )
  expect(await dispatchProblems(unrunnable, "installed-unrunnable-bin")).toEqual([
    "package/bin/dispatch has mode 644, not executable by all",
  ])
}, 60_000)

/** release.yaml's step "Check the packed Codex tarball …" over `tar`: the script run from a root
 *  of its own holding the scripts it runs, with the tarball where the pack step leaves it,
 *  $RUNNER_TEMP/codex-envoy-tarball. GITHUB_REPOSITORY is left out, so repo-paths.sh answers the
 *  repository the committed manifests name, as it does off a runner. */
async function releaseCheck(tar: string, name: string): Promise<Run> {
  const root = join(scratch, `release-${name}`)
  await mkdir(join(root, ".github", "scripts"), { recursive: true })
  await mkdir(join(root, "packages", "claude-envoy", "scripts"), { recursive: true })
  for (const script of ["release-codex-envoy-check-packed.sh", "repo-paths.sh"]) {
    await cp(join(repoRoot, ".github", "scripts", script), join(root, ".github", "scripts", script))
  }
  await cp(
    join(packageRoot, "scripts", "check-packed-skills.sh"),
    join(root, "packages", "claude-envoy", "scripts", "check-packed-skills.sh"),
  )
  const runnerTemp = join(root, "runner-temp")
  await mkdir(join(runnerTemp, "codex-envoy-tarball"), { recursive: true })
  await cp(tar, join(runnerTemp, "codex-envoy-tarball", `sjawhar-codex-envoy-${version}.tgz`))
  const { GITHUB_REPOSITORY: _repository, ...env } = process.env
  return await run(["bash", ".github/scripts/release-codex-envoy-check-packed.sh"], root, {
    ...env,
    VERSION: version,
    RUNNER_TEMP: runnerTemp,
  })
}

test("the release's check passes the tarball the build packed", async () => {
  const result = await releaseCheck(tarball, "packed")
  expect({
    exitCode: result.exitCode,
    stderr: result.stderr,
    error: result.stdout.includes("::error::"),
  }).toEqual({
    exitCode: 0,
    stderr: "",
    error: false,
  })
}, 60_000)

test("the release's check refuses a tarball without bin/dispatch", async () => {
  const tar = await variant("release-without-bin", (dir) =>
    rm(join(dir, "bin"), { recursive: true }),
  )
  const result = await releaseCheck(tar, "without-bin")
  expect(result.exitCode).toBe(1)
  expect(result.stdout).toContain(
    "::error::the packed Codex tarball holds no package/bin/dispatch, the dispatch command a terminal runs",
  )
}, 60_000)

test("the release's check refuses a bin/dispatch not every user can run", async () => {
  const tar = await variant("release-unrunnable-bin", (dir) =>
    chmod(join(dir, "bin", "dispatch"), 0o744),
  )
  const result = await releaseCheck(tar, "unrunnable-bin")
  expect(result.exitCode).toBe(1)
  expect(result.stdout).toContain(
    "::error::the packed Codex tarball's package/bin/dispatch is not executable by every user: -rwxr--r--",
  )
}, 60_000)

test("the release's check refuses a bin/dispatch that is a link", async () => {
  const tar = await variant("release-linked-bin", async (dir) => {
    await rm(join(dir, "bin", "dispatch"))
    await symlink(join("..", "dist", "dispatch.js"), join(dir, "bin", "dispatch"))
  })
  const result = await releaseCheck(tar, "linked-bin")
  expect(result.exitCode).toBe(1)
  expect(result.stdout).toContain(
    "::error::the packed Codex tarball's package/bin/dispatch is a link, not the file a terminal runs",
  )
}, 60_000)

test("the release's check refuses a manifest whose bin is not bin/dispatch", async () => {
  const tar = await variant("release-no-bin-field", async (dir) => {
    const manifest = JSON.parse(await readFile(join(dir, "package.json"), "utf8"))
    delete manifest.bin
    await writeFile(join(dir, "package.json"), `${JSON.stringify(manifest, null, 2)}\n`)
  })
  const result = await releaseCheck(tar, "no-bin-field")
  expect(result.exitCode).toBe(1)
  expect(result.stdout).toContain(
    "::error::the packed Codex tarball's package.json does not name bin/dispatch as its dispatch bin",
  )
}, 60_000)

test("the release's check refuses a tarball without dist/dispatch.js", async () => {
  const tar = await variant("release-without-dispatch-js", (dir) =>
    rm(join(dir, "dist", "dispatch.js")),
  )
  const result = await releaseCheck(tar, "without-dispatch-js")
  expect(result.exitCode).toBe(1)
  expect(result.stdout).toContain(
    "::error::the packed Codex tarball holds no package/dist/dispatch.js, which the installed plugin runs from",
  )
}, 60_000)
