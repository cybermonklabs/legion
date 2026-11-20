// The Codex hooks as Codex runs them (`codex/hooks/hooks.json`: `bun <hook>`), their stdout a
// pipe Codex reads as the hook's output. Bun drops what a full pipe has not taken when a program
// calls `process.exit`, so each case hands the hook a one-page pipe whose reader starts late, the
// reader a loaded machine gives Codex, and holds the output to every byte the hook wrote. For the
// PostToolUse hook it holds the inbox too: a message leaves `waiting/` for `taken/` only once Codex
// has the output that carries it.
import { afterEach, describe, expect, test } from "bun:test"
import { existsSync, readdirSync } from "node:fs"
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret"
import { skipWithout } from "@legion/test-fixtures/host-tools"
import { readThroughLatePipe } from "@legion/test-fixtures/late-pipe"
import { CODEX_DISPATCH_NOTE } from "../hooks/codex-hook"
import { codexInboxDirectory } from "../src/codex/codex-inbox"

const toolHook = resolve(import.meta.dir, "..", "hooks", "codex-tool-hook.ts")
const sessionHook = resolve(import.meta.dir, "..", "hooks", "codex-hook.ts")
const SESSION = "s-1"
const noPython = skipWithout("python3")

const cleanup: (() => Promise<void>)[] = []
afterEach(async () => {
  for (const step of cleanup.splice(0).reverse()) await step()
})

async function scratch(): Promise<string> {
  const root = await mkdtemp(join(tmpdir(), "claude-envoy-codex-pipe-"))
  cleanup.push(() => rm(root, { recursive: true, force: true }))
  return root
}

/** Six messages of twenty lines each, waiting in `pluginData`'s inbox of SESSION: 9 KiB of JSON
 *  once quoted, over two pipe pages, and under the hook's cap together. */
async function sixWaiting(pluginData: string): Promise<string> {
  const inbox = codexInboxDirectory(pluginData, SESSION)
  await mkdir(join(inbox, "waiting"), { recursive: true })
  for (let message = 1; message <= 6; message++) {
    const lines = Array.from(
      { length: 20 },
      (_, line) => `message ${message} line ${String(line).padStart(2, "0")} ${'"x" '.repeat(10)}`,
    )
    await writeFile(
      join(inbox, "waiting", `00000000000000${message}-aaaa.json`),
      JSON.stringify({ content: lines.join("\n") }),
    )
  }
  return inbox
}

/** The `.json` files of `directory`, none when it does not exist. */
function messagesIn(directory: string): string[] {
  return existsSync(directory)
    ? readdirSync(directory).filter((name) => name.endsWith(".json"))
    : []
}

/** The hook's `additionalContext`, from output that must be its whole JSON. */
function contextOf(stdout: Uint8Array): string {
  return JSON.parse(new TextDecoder().decode(stdout)).hookSpecificOutput.additionalContext
}

describe.skipIf(noPython)("the Codex PostToolUse hook through a one-page pipe read late", () => {
  test("hands Codex every message it took, and moves each into taken/ only then", async () => {
    const pluginData = await scratch()
    const inbox = await sixWaiting(pluginData)
    const run = await readThroughLatePipe([process.execPath, toolHook], {
      env: { PATH: process.env["PATH"], PLUGIN_DATA: pluginData },
      stdin: JSON.stringify({ session_id: SESSION }),
    })
    expect(run.pipeBytes).toBe(4_096)
    expect(run.exitCode).toBe(0)
    expect(run.stdout.length).toBeGreaterThan(2 * 4_096)
    const context = contextOf(run.stdout)
    for (let message = 1; message <= 6; message++) {
      expect(context).toContain(`message ${message} line 19 `)
    }
    expect(messagesIn(join(inbox, "taken"))).toHaveLength(6)
    expect(messagesIn(join(inbox, "waiting"))).toHaveLength(0)
    expect(existsSync(join(inbox, "claimed"))).toBe(true)
    expect(readdirSync(join(inbox, "claimed"))).toEqual([])
  }, 30_000)

  // The case above ends by itself once its write is taken, and a natural end drains the write
  // anyway, so it cannot see when the hook settled. Here Codex never reads: it kills the hook at
  // its timeout, as it does, while the pipe is full and the hook is mid-write. A hook that moved
  // what it took into taken/ before Codex had every byte would leave it there, and lose it.
  test("killed mid-write, its pipe full, leaves none of what it took in taken/, and the next hook hands Codex all of it", async () => {
    const pluginData = await scratch()
    const inbox = await sixWaiting(pluginData)
    const hook = {
      env: { PATH: process.env["PATH"], PLUGIN_DATA: pluginData },
      stdin: JSON.stringify({ session_id: SESSION }),
    }
    const killed = await readThroughLatePipe([process.execPath, toolHook], {
      ...hook,
      killWhenFullMs: 2_000,
    })
    expect(killed.killedWhenFull).toBe(true)
    expect(messagesIn(join(inbox, "taken"))).toHaveLength(0)
    expect(messagesIn(join(inbox, "waiting"))).toHaveLength(0)
    const holders = readdirSync(join(inbox, "claimed"))
    expect(holders).toHaveLength(1)
    expect(messagesIn(join(inbox, "claimed", holders[0] as string))).toHaveLength(6)
    const next = await readThroughLatePipe([process.execPath, toolHook], hook)
    expect(next.exitCode).toBe(0)
    const context = contextOf(next.stdout)
    for (let message = 1; message <= 6; message++) {
      expect(context).toContain(`message ${message} line 19 `)
    }
    expect(messagesIn(join(inbox, "taken"))).toHaveLength(6)
    expect(readdirSync(join(inbox, "claimed"))).toEqual([])
  }, 30_000)

  test("with Codex's end of the pipe closed, puts every message it took back to wait", async () => {
    const pluginData = await scratch()
    const inbox = await sixWaiting(pluginData)
    // The reader's end closed before the hook writes: its write fails with EPIPE.
    const closed = Bun.spawnSync(
      [
        "python3",
        "-c",
        "import os, subprocess, sys\nr, w = os.pipe()\nos.close(r)\n" +
          "p = subprocess.run(sys.argv[1:], input=sys.stdin.buffer.read(), stdout=w)\n" +
          "sys.exit(p.returncode)",
        process.execPath,
        toolHook,
      ],
      {
        env: { PATH: process.env["PATH"], PLUGIN_DATA: pluginData },
        stdin: new TextEncoder().encode(JSON.stringify({ session_id: SESSION })),
      },
    )
    expect(closed.exitCode).toBe(0)
    expect(messagesIn(join(inbox, "waiting"))).toHaveLength(6)
    expect(messagesIn(join(inbox, "taken"))).toHaveLength(0)
  }, 30_000)

  test("hands Codex what a hook that is gone held, as if it still waited", async () => {
    const pluginData = await scratch()
    const inbox = await sixWaiting(pluginData)
    // A hook Codex killed at its timeout held two of the messages: its pid runs no process now.
    const gone = Bun.spawnSync(["true"]).pid
    await mkdir(join(inbox, "claimed", String(gone)), { recursive: true })
    for (const name of messagesIn(join(inbox, "waiting")).sort().slice(0, 2)) {
      await Bun.write(
        join(inbox, "claimed", String(gone), name),
        Bun.file(join(inbox, "waiting", name)),
      )
      await rm(join(inbox, "waiting", name))
    }
    const run = await readThroughLatePipe([process.execPath, toolHook], {
      env: { PATH: process.env["PATH"], PLUGIN_DATA: pluginData },
      stdin: JSON.stringify({ session_id: SESSION }),
    })
    expect(run.exitCode).toBe(0)
    const context = contextOf(run.stdout)
    for (let message = 1; message <= 6; message++) {
      expect(context).toContain(`message ${message} line 19 `)
    }
    expect(messagesIn(join(inbox, "taken"))).toHaveLength(6)
    expect(existsSync(join(inbox, "claimed", String(gone)))).toBe(false)
  }, 30_000)
})

describe.skipIf(noPython)("the Codex SessionStart hook through a one-page pipe read late", () => {
  test("hands Codex the whole note and open-asks summary, past a page", async () => {
    const home = await scratch()
    const questions = Array.from(
      { length: 40 },
      (_, index) => `Question ${index}: which of the two ways should the fix take, and why?`,
    )
    const server = Bun.serve({
      port: 0,
      hostname: "127.0.0.1",
      fetch: () =>
        Response.json({
          session_id: SESSION,
          as_of: "2026-09-14T00:00:00Z",
          opened_since: false,
          count: questions.length,
          waiting_on_human: questions.length,
          waiting_on_agent: 0,
          asks: questions.map((question, index) => ({
            id: `ask-${index}`,
            ref: `/issues/LEGION-9#ask-${index}`,
            question,
            kind: "decision",
            urgency: "med",
            created_at: "2026-09-14T00:00:00Z",
            age_seconds: 90,
            priority: 1,
            owner: { issue: { key: "LEGION-9", title: "Bundle" } },
            human_replied: false,
            last_reply: null,
            waiting_on: "human",
          })),
        }),
    })
    cleanup.push(async () => {
      await server.stop(true)
    })
    const run = await readThroughLatePipe([process.execPath, sessionHook, "session-start"], {
      env: {
        PATH: process.env["PATH"],
        HOME: home,
        DISPATCH_URL: `http://127.0.0.1:${server.port}`,
        DISPATCH_TOKEN: fixtureSecret("dsp"),
      },
      stdin: JSON.stringify({ session_id: SESSION, cwd: home, source: "startup" }),
    })
    expect(run.exitCode).toBe(0)
    const text = new TextDecoder().decode(run.stdout)
    expect(run.stdout.length).toBeGreaterThan(4_096)
    expect(text).toStartWith(`${CODEX_DISPATCH_NOTE}\n\nDispatch authored-ask summary:\n`)
    for (const question of questions) expect(text).toContain(question)
    expect(text.endsWith("\n")).toBe(true)
  }, 30_000)
})
