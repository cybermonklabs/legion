// The Codex plugin under Codex's shared app server: what its review found it must refuse.

import { describe, expect, test } from "bun:test"
import type { ChildProcess } from "node:child_process"
import { chmodSync, mkdirSync, readFileSync, writeFileSync } from "node:fs"
import { dirname, join } from "node:path"
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories"
import { sharedToolCall } from "../src/codex/codex-server"
import { readCodexSessionRecord, readSharedCodexState } from "../src/codex/codex-session-record"
import { codexStandbyLogFile, spawnCodexEnvoyStandby } from "../src/codex/codex-standby"
import { ThreadStartBackoff } from "../src/codex/codex-start-backoff"
import { callSharedCodexTool, serveSharedCodexTools } from "../src/codex/codex-tool-link"

const root = temporaryDirectory("codex-shared-faults")
/** Whether this process can read a file whatever its mode (root can), so a refused read cannot be
 *  made here. */
const readsAnything = process.getuid?.() === 0

describe("a thread's record that cannot be read", () => {
  test("is no record when it is missing or not JSON, and an error otherwise", async () => {
    const directory = join(root, "record")
    mkdirSync(directory, { recursive: true })
    expect(await readCodexSessionRecord(join(directory, "missing.json"))).toBeUndefined()
    const garbled = join(directory, "garbled.json")
    writeFileSync(garbled, "{not json")
    expect(await readCodexSessionRecord(garbled)).toBeUndefined()
    if (readsAnything) return
    const unreadable = join(directory, "unreadable.json")
    writeFileSync(unreadable, "{}")
    chmodSync(unreadable, 0o000)
    // Read as "no record", it would end that thread's live session.
    await expect(readCodexSessionRecord(unreadable)).rejects.toThrow()
  })

  test("fails the shared app server's whole read, so the owner keeps every runtime for that look", async () => {
    if (readsAnything) return
    const state = join(root, "state")
    const pid = 424242
    const threads = join(state, "codex-sessions", String(pid))
    mkdirSync(threads, { recursive: true })
    const file = join(threads, "01a1-thread.json")
    writeFileSync(file, "{}")
    chmodSync(file, 0o000)
    await expect(readSharedCodexState(state, pid, "start")).rejects.toThrow()
  })
})

describe("an Envoy tool call under the shared app server", () => {
  test("is refused when Codex names no calling thread", () => {
    expect(() => sharedToolCall(undefined, "envoy_send", {})).toThrow(
      "Codex did not identify the calling thread",
    )
    expect(sharedToolCall("01a1-thread", "envoy_send", { a: 1 })).toEqual({
      args: { a: 1 },
      name: "envoy_send",
      thread: "01a1-thread",
    })
  })

  test("is refused at the owner's socket when it names no thread or a malformed one", async () => {
    const directory = join(root, "servers")
    mkdirSync(directory, { recursive: true })
    const owner = { pid: process.pid, seq: 1, started: null }
    writeFileSync(join(directory, "1.json"), JSON.stringify(owner))
    const handled: unknown[] = []
    const close = await serveSharedCodexTools(directory, owner, async (call) => {
      handled.push(call)
      return "ok"
    })
    try {
      const signal = AbortSignal.timeout(5_000)
      expect(
        await callSharedCodexTool(
          directory,
          { args: {}, name: "envoy_send", thread: "01a1-t" },
          signal,
        ),
      ).toBe("ok")
      // The socket checks every call itself, whatever the server forwarding it checked.
      for (const thread of [undefined, "", "../escape"]) {
        await expect(
          callSharedCodexTool(directory, { args: {}, name: "envoy_send", thread } as never, signal),
        ).rejects.toThrow()
      }
      expect(handled).toEqual([{ args: {}, name: "envoy_send", thread: "01a1-t" }])
    } finally {
      await close()
    }
  })
})

describe("the standby a shared app server's owner or hook starts", () => {
  /** A stand-in bundle that writes what it was started with, and one line to its stderr. */
  function fakeBundle(name: string): { bundle: string; out: string; log: string } {
    const directory = join(root, name)
    mkdirSync(join(directory, "dist"), { recursive: true })
    const out = join(directory, "started.json")
    const bundle = join(directory, "dist", "envoy-channel.js")
    writeFileSync(
      bundle,
      `require("node:fs").writeFileSync(${JSON.stringify(out)}, JSON.stringify({ argv: process.argv.slice(2), cwd: process.cwd(), env: process.env }))\nprocess.stderr.write("standby failed: the reason\\n")\nprocess.exit(3)\n`,
    )
    return { bundle, log: codexStandbyLogFile(join(directory, "servers")), out }
  }

  async function started(child: ChildProcess, out: string) {
    await new Promise((done) => child.once("exit", done))
    return JSON.parse(readFileSync(out, "utf8")) as {
      argv: string[]
      cwd: string
      env: Record<string, string>
    }
  }

  test("is started from the hook with the plugin's state and environment alone, and writes its stderr to its log", async () => {
    const { bundle, out, log } = fakeBundle("from-hook")
    process.env["CODEX_ENVOY_TEST_UNRELATED"] = "kept out"
    try {
      const child = spawnCodexEnvoyStandby(bundle, 4242, "start-time", log, "/state/of/plugin")
      const seen = await started(child, out)
      expect(seen.argv).toEqual(["--codex-standby", "4242", "start-time", "--from-hook"])
      expect(seen.cwd).toBe(join(root, "from-hook"))
      expect(seen.env["PLUGIN_DATA"]).toBe("/state/of/plugin")
      expect(seen.env["CODEX_ENVOY_TEST_UNRELATED"]).toBeUndefined()
      expect(readFileSync(log, "utf8")).toBe("standby failed: the reason\n")
    } finally {
      delete process.env["CODEX_ENVOY_TEST_UNRELATED"]
    }
  })

  test("started by the owner inherits its environment, and a log at its limit starts again", async () => {
    const { bundle, out, log } = fakeBundle("from-owner")
    mkdirSync(dirname(log), { recursive: true })
    writeFileSync(log, "x".repeat(1024 * 1024))
    process.env["CODEX_ENVOY_TEST_INHERITED"] = "inherited"
    try {
      const seen = await started(spawnCodexEnvoyStandby(bundle, 4242, "start-time", log), out)
      expect(seen.argv).toEqual(["--codex-standby", "4242", "start-time"])
      expect(seen.env["CODEX_ENVOY_TEST_INHERITED"]).toBe("inherited")
      expect(readFileSync(log, "utf8")).toBe("standby failed: the reason\n")
      // Under the limit, the next standby's lines follow it.
      await started(spawnCodexEnvoyStandby(bundle, 4242, "start-time", log), out)
      expect(readFileSync(log, "utf8")).toBe("standby failed: the reason\n".repeat(2))
    } finally {
      delete process.env["CODEX_ENVOY_TEST_INHERITED"]
    }
  })
})

describe("a thread whose runtime ended in failure", () => {
  test("starts again after 1 s, then 2, 4 and so on to a minute, and afresh at a new load or after a long run", () => {
    let now = 0
    const backoff = new ThreadStartBackoff(() => now)
    const live = (generation: string) => new Map([["t", { generation }]])
    expect(backoff.mayStart("t")).toBe(true)
    const waits = [1, 2, 3, 4, 5, 6, 7, 8].map(() => {
      const wait = backoff.failed("t", "g1", now)
      expect(backoff.mayStart("t")).toBe(false)
      now += wait - 1
      expect(backoff.mayStart("t")).toBe(false)
      now += 1
      expect(backoff.mayStart("t")).toBe(true)
      return wait
    })
    expect(waits).toEqual([1_000, 2_000, 4_000, 8_000, 16_000, 32_000, 60_000, 60_000])
    // The same load keeps its count; a new load starts it again.
    backoff.forgetChanged(live("g1"))
    expect(backoff.failed("t", "g1", now)).toBe(60_000)
    backoff.forgetChanged(live("g2"))
    expect(backoff.mayStart("t")).toBe(true)
    expect(backoff.failed("t", "g2", now)).toBe(1_000)
    // A runtime that ran past the longest wait failed afresh.
    const startedAt = now
    now += 60_001
    expect(backoff.failed("t", "g2", startedAt)).toBe(1_000)
    // One that ended without failing clears the count.
    backoff.ended("t")
    expect(backoff.mayStart("t")).toBe(true)
    expect(backoff.failed("t", "g2", now)).toBe(1_000)
  })
})
