/**
 * The Codex plugin's MCP server (`bin/envoy-channel.ts --host codex`) in a process of its own,
 * started the way the `codex` process starts it, with this test process standing in for that
 * `codex` process: the server is its child, so its parent is the process whose session record it
 * waits for (`codexSessionRecordFile(<state>, <parent pid>)`), and `recordFor` writes that record
 * as the plugin's `SessionStart` hook does. Its environment is only what a test hands it, as Codex
 * hands a plugin's server only its manifest's `env_vars`, with `PATH` and `bunCacheOff` added.
 *
 * Used by the rows that run the Codex server's own NATS dial against a stand-in hub and broker:
 * tests/codex-connection-proof.test.ts and tests/codex-prints-no-secret.test.ts.
 */
import { mkdirSync, writeFileSync } from "node:fs"
import { join, resolve } from "node:path"
import { bunCacheOff } from "@legion/test-fixtures/bun-child-environment"
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
 * Starts the server with `env` as its whole environment (PATH and `bunCacheOff` are added: a test
 * hands it a fresh `HOME`, where bun would write its transpiler cache) in `pluginRoot`, the
 * directory Codex starts it in, and keeps its stdin open, as Codex keeps the MCP pipe.
 */
export function startCodexServer(env: Record<string, string>, pluginRoot: string): CodexServer {
  let stderr = ""
  const child = Bun.spawn([process.execPath, channelEntry, "--host", "codex"], {
    cwd: pluginRoot,
    env: {
      PATH: process.env["PATH"] ?? "",
      ...bunCacheOff,
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
