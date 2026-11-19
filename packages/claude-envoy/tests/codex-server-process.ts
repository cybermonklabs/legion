/**
 * The Codex plugin's MCP server (`bin/envoy-channel.ts --host codex`) in a process of its own,
 * started the way the `codex` process starts it, with this test process standing in for that
 * `codex` process: the server is its child, so its parent is the process whose session record it
 * waits for (`codexSessionRecordFile(<state>, <parent pid>)`), and `recordFor` writes that record
 * as the plugin's `SessionStart` hook does. Its environment is only what a test hands it, as Codex
 * hands a plugin's server only its manifest's `env_vars`, with `PATH` and bun's transpiler cache
 * (`transpilerCache`) added.
 *
 * Used by the rows that run the Codex server's own NATS dial against a stand-in hub and broker:
 * tests/codex-connection-proof.test.ts and tests/codex-prints-no-secret.test.ts.
 */
import { mkdirSync, writeFileSync } from "node:fs"
import { homedir } from "node:os"
import { join, resolve } from "node:path"
import type { Subprocess } from "bun"
import {
  codexSessionRecordFile,
  processStarted,
  writeCodexSessionRecord,
} from "../src/codex/codex-session-record"

const channelEntry = resolve(import.meta.dir, "..", "bin", "envoy-channel.ts")

export interface CodexServer {
  readonly process: Subprocess<"pipe", "pipe", "pipe">
  /** Everything the server has written to stderr so far. */
  stderr(): string
  /** Stops the server: SIGTERM, then SIGKILL after 5 s. */
  stop(): Promise<void>
}

/**
 * A Codex home, the plugin's state directory and a person's home under `root`, with the
 * envoy.json `envoy` names written where a Codex session reads it (`~/.config/legion/envoy.json`).
 */
export function codexHomes(root: string, envoy: Record<string, unknown>) {
  const home = join(root, "home")
  const pluginData = join(root, "plugin-data")
  const codexHome = join(root, "codex-home")
  const cwd = join(root, "cwd")
  for (const directory of [join(home, ".config", "legion"), pluginData, codexHome, cwd]) {
    mkdirSync(directory, { recursive: true })
  }
  writeFileSync(
    join(home, ".config", "legion", "envoy.json"),
    JSON.stringify({ envoy, dispatch: { enabled: false } }),
  )
  return { codexHome, cwd, home, pluginData }
}

/**
 * The transpiler cache this test process's bun reads (`BUN_RUNTIME_TRANSPILER_CACHE_PATH`, else
 * `$XDG_CACHE_HOME/bun/@t@`, else `~/.bun/install/cache/@t@`, as bun 1.4.4 places it). The server
 * runs this checkout's TypeScript, and under the fresh `HOME` a test hands it, bun would put its
 * cache in that home: measured on a loaded dev box, a start transpiled the whole graph cold in
 * 11.9 s and wrote 266 files there, against 0.27 s with this cache, so a row's CONNECT came 12 to
 * 18 s late and the removal of its directory outran its 5 s hook. A harness variable: Codex hands
 * the plugin's server no such variable, and the released plugin runs one bundled file.
 */
function transpilerCache(): string {
  const named = process.env["BUN_RUNTIME_TRANSPILER_CACHE_PATH"]
  if (named !== undefined && named !== "") return named
  const xdg = process.env["XDG_CACHE_HOME"]
  if (xdg !== undefined && xdg !== "") return join(xdg, "bun", "@t@")
  return join(homedir(), ".bun", "install", "cache", "@t@")
}

/**
 * Starts the server with `env` as its whole environment (PATH and the transpiler cache are added)
 * in `pluginRoot`, the directory Codex starts it in, and keeps its stdin open, as Codex keeps the
 * MCP pipe.
 */
export function startCodexServer(env: Record<string, string>, pluginRoot: string): CodexServer {
  let stderr = ""
  const child = Bun.spawn([process.execPath, channelEntry, "--host", "codex"], {
    cwd: pluginRoot,
    env: {
      PATH: process.env["PATH"] ?? "",
      BUN_RUNTIME_TRANSPILER_CACHE_PATH: transpilerCache(),
      ...env,
    },
    stdin: "pipe",
    stdout: "pipe",
    stderr: "pipe",
  })
  void (async () => {
    for await (const chunk of child.stderr.pipeThrough(new TextDecoderStream())) stderr += chunk
  })()
  // The MCP stream is drained so the server never blocks on a full pipe.
  void new Response(child.stdout).arrayBuffer()
  return {
    process: child,
    stderr: () => stderr,
    stop: async () => {
      child.kill("SIGTERM")
      const exited = await Promise.race([child.exited.then(() => true), Bun.sleep(5_000)])
      if (exited !== true) child.kill("SIGKILL")
      await child.exited
    },
  }
}

/** The record the plugin's `SessionStart` hook writes for session `sessionId` of this process,
 *  the server's `codex` process. */
export async function recordFor(
  pluginData: string,
  sessionId: string,
  homes: { readonly codexHome: string; readonly cwd: string },
): Promise<void> {
  const started = processStarted(process.pid)
  if (started === undefined) throw new Error("this process's start cannot be read")
  await writeCodexSessionRecord(codexSessionRecordFile(pluginData, process.pid), {
    codex_executable: null,
    codex_home: homes.codexHome,
    codex_started: started,
    cwd: homes.cwd,
    session_id: sessionId,
    source: "startup",
    transcript_path: null,
  })
}

/** Polls `condition` every 50 ms until it holds; throws naming `what` after `ms`. */
export async function until(
  condition: () => boolean,
  what: string,
  ms: number,
  state: () => string,
): Promise<void> {
  const deadline = performance.now() + ms
  while (!condition()) {
    if (performance.now() > deadline) throw new Error(`timed out waiting for ${what}: ${state()}`)
    await Bun.sleep(50)
  }
}
