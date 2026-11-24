import { mkdtemp, rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import { dirname, join, resolve } from "node:path"

/** The package root, whose `scripts/build.ts` builds the plugin. */
const PACKAGE = resolve(import.meta.dir, "..")

let scratch: string | undefined
let build: Promise<string> | undefined

/**
 * Where code Bun runs may live: `tmpdir()`, unless its path holds a `?`, which Bun 1.4.4 reads as
 * the start of a query in a module path, so `bun <file>` under it fails "Module not found" (seen
 * for the built `dist/session-hook.js`, and for a `file:` URL with the `?` as `%3F`); then the
 * nearest directory above it whose path holds none. A plugin built there runs: its hooks and its
 * MCP server are each `bun <file>` under the plugin's directory.
 */
function runnableTmpdir(): string {
  let directory = tmpdir()
  while (directory.includes("?")) directory = dirname(directory)
  return directory
}

/**
 * The plugin the real-binary rows load into Claude Code: a fresh `scripts/build.ts --out` build of
 * this tree, whose `legion/` is the generated plugin tree and whose `dist/` holds the built hooks
 * and MCP server. The first case that asks builds it, once for every file of the run, and
 * `removeBuiltPlugin` removes it after the last (`built-plugin-preload.ts`). Nothing commits
 * `dist/`, and a pull request's tree carries main's last build of `legion/`, never its own: a row
 * run on the package directory fails in a checkout with no `dist/` and runs old hooks in one with
 * an old `dist/`, and a hand-written copy of either would prove nothing. The build writes outside
 * the repository, as `build.ts` requires, under `runnableTmpdir()`.
 */
export function builtPlugin(): Promise<string> {
  build ??= (async () => {
    scratch = await mkdtemp(join(runnableTmpdir(), "claude-envoy-plugin-"))
    const out = join(scratch, "plugin")
    const script = join(PACKAGE, "scripts", "build.ts")
    const child = Bun.spawn([process.execPath, script, "--out", out], {
      cwd: runnableTmpdir(),
      stdout: "ignore",
      stderr: "pipe",
    })
    const [exitCode, stderr] = await Promise.all([child.exited, new Response(child.stderr).text()])
    if (exitCode !== 0) throw new Error(`scripts/build.ts --out exited ${exitCode}: ${stderr}`)
    return out
  })()
  return build
}

/** Removes the build `builtPlugin` made, if any; the next `builtPlugin` builds afresh. */
export async function removeBuiltPlugin(): Promise<void> {
  const made = scratch
  scratch = undefined
  build = undefined
  if (made !== undefined) await rm(made, { recursive: true, force: true })
}
