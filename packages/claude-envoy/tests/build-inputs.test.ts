import { expect, test } from "bun:test"
import { existsSync } from "node:fs"
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { dirname, isAbsolute, join, relative, resolve, sep } from "node:path"
import { gitBinary } from "@legion/test-fixtures/version-control-tools"

const packageRoot = resolve(import.meta.dir, "..")
const repoRoot = resolve(packageRoot, "..", "..")

/** The row's own limit, and the only bound of the build and the git commands it runs: each waits
 *  for its command to exit until the row's deadline, and no command has a span of its own, so no
 *  guard ends a command the row would still have time for. The build is the slow command, and how
 *  long it takes depends on the machine: 1.4 to 9.1 s at a load of about 1,300 on a 288-CPU box,
 *  and 4.7 to 9.4 s pinned to two CPUs beside 16 busy loops (8 runnable per CPU, that box's ratio
 *  at a load of 2,300), each run to its end; 0.27 s in CI. The one run at a load of about 2,300
 *  was cut at 120,028 ms by the 120 s limit this row had, so that reading is a lower bound, and
 *  CPU alone did not slow the build that far. The limit is sized from CI's deadline, the claude-
 *  envoy job's `timeout-minutes: 10`: in job 137305559238 this row began 76 s into the job, the
 *  rows after it took 159 s and the steps after the test step 3 s, which leaves 362 s for a row
 *  that hangs to be named before the job is cut; 300 s leaves 62 s of that. */
const ROW_LIMIT_MS = 300_000

/** What Bun gives the row past `ROW_LIMIT_MS`: the time for a command killed at the deadline to
 *  fail the row by its own name before Bun's timer fails it unnamed. It decides no verdict: every
 *  command `run` starts has ended or been killed by the deadline. */
const REPORT_MS = 10_000

/** `command` run in `cwd`; its stdout, or an error naming the command and its stderr when it fails,
 *  or naming it, killed, when it has not exited by `deadline` (epoch ms), the row's own. It runs in
 *  a process group of its own, and the kill takes the whole group: a child of the command that
 *  holds its output pipes open would otherwise keep the read waiting past the kill, to Bun's timer.
 *  A command due to start once the deadline has passed is not started: the error says the row's
 *  time was spent before it, so it names no command that did not spend it. */
async function run(
  command: string[],
  cwd: string,
  deadline: number,
  env?: Record<string, string>,
): Promise<string> {
  if (Date.now() >= deadline) {
    throw new Error(
      `the row's ${ROW_LIMIT_MS / 1000} s were spent before ${command.join(" ")} started, so it did not start`,
    )
  }
  const child = Bun.spawn(command, {
    cwd,
    ...(env === undefined ? {} : { env }),
    stdin: "ignore",
    stdout: "pipe",
    stderr: "pipe",
    detached: true,
  })
  let killed = false
  const guard = setTimeout(
    () => {
      killed = true
      try {
        process.kill(-child.pid, "SIGKILL")
      } catch {
        // The group already exited.
      }
    },
    Math.max(0, deadline - Date.now()),
  )
  const [exitCode, stdout, stderr] = await Promise.all([
    child.exited,
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
  ]).finally(() => clearTimeout(guard))
  if (killed) {
    throw new Error(
      `${command.join(" ")} had not exited when the row's ${ROW_LIMIT_MS / 1000} s were spent, so it was killed: ${stderr.trim()}`,
    )
  }
  if (exitCode !== 0) {
    throw new Error(`${command.join(" ")} exited ${exitCode}: ${stderr.trim()}`)
  }
  return stdout
}

/** Every file the build reads, repository-relative, by running it: both halves of the build report
 *  what they read, so a source either of them starts reading is covered here without this test
 *  listing any of them itself. The build runs in a process of its own because a second Bun.build in
 *  a test process that has already run other test files fails reading its sources with EISDIR
 *  (tests/dist-fresh.test.ts builds the same way). */
async function filesTheBuildReads(scratch: string, deadline: number): Promise<string[]> {
  const build = JSON.stringify(join(packageRoot, "scripts", "build.ts"))
  const script = [
    `import { buildBundles, buildLegionPlugin } from ${build}`,
    "console.log(",
    "  JSON.stringify({",
    `    bundles: await buildBundles(${JSON.stringify(join(scratch, "dist"))}),`,
    `    legion: await buildLegionPlugin(${JSON.stringify(join(scratch, "legion"))}),`,
    "  }),",
    ")",
  ].join("\n")
  const read: { bundles: string[]; legion: string[] } = JSON.parse(
    await run([process.execPath, "--eval", script], packageRoot, deadline),
  )
  const bundleInputs = read.bundles.map((input) => relative(repoRoot, input))
  const legionInputs = read.legion.map((input) => relative(repoRoot, input))
  const outside = [...bundleInputs, ...legionInputs].filter(
    (input) => input.startsWith("..") || isAbsolute(input),
  )
  expect(outside).toEqual([])

  // The node_modules and package-root probes below are about how Bun resolved and transpiled a
  // bundled source, so only the bundles' inputs go through them. `buildLegionPlugin` reads the
  // skills and agents as plain files, with no resolution or notices behind them, and probing their
  // package roots would demand packages/pi-legion/package.json and the like from the list for
  // files the build never reads.
  const probes = new Set<string>(legionInputs)
  const local = bundleInputs.filter((input) => !input.split(sep).includes("node_modules"))
  for (const input of local) probes.add(input)
  // A module under node_modules is whatever `bun install` put there, which the lockfile, the root
  // manifest and the patches it applies decide.
  if (local.length < bundleInputs.length) {
    const manifest = JSON.parse(await readFile(join(repoRoot, "package.json"), "utf8")) as {
      patchedDependencies?: Record<string, string>
    }
    probes.add("bun.lock")
    probes.add("package.json")
    for (const patch of Object.values(manifest.patchedDependencies ?? {})) probes.add(patch)
  }
  // The workspace package a source sits in: its manifest and tsconfig steer how Bun resolves and
  // transpiles the source, and a license or notice file at its top puts the package in the bundle's
  // notices. "LICENSE" and "NOTICE" are names scripts/third-party-notices.ts's unexported
  // LICENSE_FILE and NOTICE_FILE patterns match; a file that does not exist yet is probed too.
  for (const input of local) {
    let directory = dirname(input)
    while (directory !== "." && !existsSync(join(repoRoot, directory, "package.json"))) {
      directory = dirname(directory)
    }
    if (directory === ".") continue
    for (const name of ["package.json", "tsconfig.json", "LICENSE", "NOTICE"]) {
      probes.add(join(directory, name))
    }
  }
  return [...probes].sort()
}

/** The pathspecs of scripts/build-inputs.txt, under the list's one rule: drop empty and # lines. */
async function buildInputPathspecs(): Promise<string[]> {
  const pathspecs = (await readFile(join(packageRoot, "scripts", "build-inputs.txt"), "utf8"))
    .split("\n")
    .filter((line) => line !== "" && !line.startsWith("#"))
  // With no pathspec at all, git would list every file and a check over them would pass on nothing.
  expect(pathspecs.length).toBeGreaterThan(0)
  return pathspecs
}

// scripts/build-inputs.txt is the statement of what can change this plugin's build, and
// release.yaml's $CLAUDE_ENVOY_VERSION_ARGS carries its package and content paths, so the list
// must match every file the build reads or a release would publish a build from a source no
// version was bumped for. Git does the matching, in a scratch repository holding an empty file at
// each path, so the workspace's own git or jj state plays no part, and it is the git binary that
// `git` on PATH runs (`gitBinary`), so no wrapper a machine puts first on PATH answers instead.
// The build and both git commands run until the row's one deadline (`ROW_LIMIT_MS`); gitBinary's
// own probe of PATH's git is no command `run` starts, so Bun's timer alone bounds it, and a hang
// there fails the row as Bun's timeout rather than by the command's name.
async function buildInputsMatch(): Promise<void> {
  const deadline = Date.now() + ROW_LIMIT_MS
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-build-inputs-"))
  try {
    const probes = await filesTheBuildReads(join(scratch, "build"), deadline)
    const pathspecs = await buildInputPathspecs()
    const repository = join(scratch, "repository")
    const git = await gitBinary(scratch)
    const env = { PATH: process.env["PATH"] ?? "", HOME: scratch, GIT_CONFIG_NOSYSTEM: "1" }
    await run([git, "init", "--quiet", repository], scratch, deadline, env)
    for (const probe of probes) {
      await mkdir(join(repository, dirname(probe)), { recursive: true })
      await writeFile(join(repository, probe), "")
    }
    const listed = new Set(
      (
        await run(
          [git, "ls-files", "--others", "-z", "--", ...pathspecs],
          repository,
          deadline,
          env,
        )
      )
        .split("\0")
        .filter((path) => path !== ""),
    )
    expect(probes.filter((probe) => !listed.has(probe))).toEqual([])
  } finally {
    await rm(scratch, { recursive: true, force: true })
  }
}
test(
  "scripts/build-inputs.txt matches every file the build reads",
  buildInputsMatch,
  ROW_LIMIT_MS + REPORT_MS,
)

/** The entries of scripts/build-inputs.txt that are not release inputs, so
 *  $CLAUDE_ENVOY_VERSION_ARGS does not carry them: the toolchain block the list's own header
 *  names — the Bun pin, the lockfile and root manifest and bunfig a `bun install` reads, the two
 *  .gitattributes files, and the two .github paths the build jobs run the build through. No
 *  plugin's version arguments carry theirs, because a lockfile or runner change alone releases
 *  nothing and the next source commit publishes whatever it built. */
const NON_RELEASE_INPUTS = [
  ".bun-version",
  "bun.lock",
  "package.json",
  "bunfig.toml",
  ".gitattributes",
  "packages/.gitattributes",
  ".github/scripts/release-push.sh",
  ".github/actions/setup-bun",
]

// release.yaml's claude_envoy_build bumps the version from the commits since the newest
// claude-envoy-v tag that touch the paths $CLAUDE_ENVOY_VERSION_ARGS names, and
// scripts/build-inputs.txt is the statement of what can change the build. A path on the list and
// not in the args ships in a release with no version bumped for it; one in the args and not on
// the list releases for a file the build never reads. Both files say the two must agree
// (build-inputs.txt's header, packages/claude-envoy/AGENTS.md), so this holds them equal.
test(".github/workflows/release.yaml's $CLAUDE_ENVOY_VERSION_ARGS carries the build inputs", async () => {
  const workflow = ".github/workflows/release.yaml"
  const parsed = Bun.YAML.parse(await readFile(join(repoRoot, workflow), "utf8")) as {
    env?: Record<string, string>
  }
  const args = parsed.env?.["CLAUDE_ENVOY_VERSION_ARGS"]
  expect(typeof args).toBe("string")
  // release-bump.sh reads the paths after `--`; everything before it is an option and its value.
  const words = (args as string).split(/\s+/).filter((word) => word !== "")
  const separator = words.indexOf("--")
  expect(separator).toBeGreaterThan(-1)
  // A path is written with a trailing slash in the args and without one on the list.
  const argPaths = words.slice(separator + 1).map((path) => path.replace(/\/$/, ""))
  expect(argPaths.length).toBeGreaterThan(0)

  const pathspecs = await buildInputPathspecs()
  // Every entry of NON_RELEASE_INPUTS is on the list, so a renamed one is reported here rather
  // than quietly widening what the args must carry.
  expect(NON_RELEASE_INPUTS.filter((path) => !pathspecs.includes(path))).toEqual([])
  // An `:(exclude…)` pathspec narrows a directory already on the list rather than naming a
  // source of its own, so it is no path for the args either.
  const listed = pathspecs
    .filter((pathspec) => !pathspec.startsWith(":"))
    .filter((pathspec) => !NON_RELEASE_INPUTS.includes(pathspec))

  expect([...argPaths].sort()).toEqual([...listed].sort())
})
