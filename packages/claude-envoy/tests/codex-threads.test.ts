// The parts of the Codex plugin that serve the threads of Codex's shared app server: the records the
// `SessionStart` hook writes per thread and the owner reads (`../src/codex/codex-session-record`),
// the owner's sessions, one per live thread (`../src/codex/codex-threads`), and the socket a
// server standing by sends its thread's Envoy tool calls to (`../src/codex/codex-tool-route`).
import { describe, expect, setDefaultTimeout, test } from "bun:test"
import { existsSync, readdirSync, writeFileSync } from "node:fs"
import { mkdir } from "node:fs/promises"
import { join } from "node:path"
import { HANG_GUARD_MS } from "@legion/test-fixtures/hang-guard"
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories"
import type { CodexEnvoySession } from "../src/codex/codex-envoy-session"
import {
  type CodexSessionRecord,
  codexThreadEndedFile,
  codexThreadRecordFile,
  codexThreadsDirectory,
  markThreadEnded,
  processStarted,
  pruneCodexSessionRecords,
  readLiveThreads,
  watchThreads,
  writeCodexSessionRecord,
} from "../src/codex/codex-session-record"
import { createCodexThreads, UNREGISTERED_THREAD } from "../src/codex/codex-threads"
import { forwardToolCall, serveToolRoute, toolRouteFile } from "../src/codex/codex-tool-route"
import type { ChannelToolRuntime } from "../src/envoy-channel-server"
import { waitFor } from "./channel-session-harness"

// The rows wait on files and local sockets, which a loaded machine slows. Their bound is a hang
// guard, not a verdict, for every row of this file that states none of its own.
setDefaultTimeout(HANG_GUARD_MS)

const makeDirectory = temporaryDirectories("codex-threads")

const A = "019a0f3c-5b1e-7d2a-9c4f-0b1d2e3f4a5b"
const B = "019a0f3c-6c2f-7e3b-8d5a-1c2e3f4a5b6c"

/** One turn of the event loop: every promise callback queued before it has run. */
function turn(): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>()
  setImmediate(resolve)
  return promise
}

/** The record the hook writes for thread `thread` of the process `pid`, started `started`. */
function threadRecord(thread: string, started: string | null): CodexSessionRecord {
  return {
    codex_executable: null,
    codex_home: "/h/.codex",
    codex_started: started,
    cwd: "/work",
    session_id: thread,
    source: "startup",
    transcript_path: `/h/.codex/sessions/rollout-${thread}.jsonl`,
  }
}

describe("the thread records of Codex's shared app server", () => {
  test("live are the records of the running process with no ended marker beside them", async () => {
    const state = makeDirectory()
    const started = processStarted(process.pid) ?? null
    expect(started).not.toBeNull()
    const pid = process.pid
    await writeCodexSessionRecord(codexThreadRecordFile(state, pid, A), threadRecord(A, started))
    await writeCodexSessionRecord(codexThreadRecordFile(state, pid, B), threadRecord(B, started))
    // An earlier process that had this pid, a record under another thread's name, a half-written
    // temporary file and a stray file: none of them is a live thread.
    await writeCodexSessionRecord(
      codexThreadRecordFile(state, pid, "old"),
      threadRecord("old", "1"),
    )
    await writeCodexSessionRecord(
      codexThreadRecordFile(state, pid, "moved"),
      threadRecord(A, started),
    )
    writeFileSync(join(codexThreadsDirectory(state, pid), `${B}.json.4242.tmp`), "{")
    writeFileSync(join(codexThreadsDirectory(state, pid), "notes.txt"), "")
    expect([...(await readLiveThreads(state, pid)).keys()].sort()).toEqual([A, B])

    await markThreadEnded(state, pid, A)
    expect(existsSync(codexThreadEndedFile(state, pid, A))).toBe(true)
    const live = await readLiveThreads(state, pid)
    expect([...live.keys()]).toEqual([B])
    expect(live.get(B)?.transcript_path).toBe(`/h/.codex/sessions/rollout-${B}.jsonl`)
    // A process with no records yet has no live thread.
    expect((await readLiveThreads(state, pid + 1_000_000)).size).toBe(0)
  })

  test("a watch sees a new thread, its end and its load again", async () => {
    const state = makeDirectory()
    const started = processStarted(process.pid) ?? null
    const seen: string[][] = []
    const warnings: string[] = []
    const stop = watchThreads(
      state,
      process.pid,
      (live) => seen.push([...live.keys()].sort()),
      20,
      (text) => warnings.push(text),
    )
    const last = (): string => JSON.stringify(seen.at(-1))
    try {
      await waitFor(async () => last() === "[]", "the first look, with no thread yet", last)
      await writeCodexSessionRecord(
        codexThreadRecordFile(state, process.pid, A),
        threadRecord(A, started),
      )
      await waitFor(async () => last() === JSON.stringify([A]), "thread A recorded", last)
      await markThreadEnded(state, process.pid, A)
      await waitFor(async () => last() === "[]", "thread A ended", last)
      // A resume writes the record again and removes the marker, as the hook does.
      await writeCodexSessionRecord(
        codexThreadRecordFile(state, process.pid, A),
        threadRecord(A, started),
      )
      await Bun.file(codexThreadEndedFile(state, process.pid, A)).delete()
      await waitFor(async () => last() === JSON.stringify([A]), "thread A loaded again", last)
    } finally {
      stop()
    }
    expect(warnings).toEqual([])
  })

  test("the thread record directory of an exited codex process is pruned, and a live one's kept", async () => {
    const state = makeDirectory()
    const exited = Bun.spawnSync(["true"]).pid
    for (const pid of [exited, process.pid]) {
      await writeCodexSessionRecord(codexThreadRecordFile(state, pid, A), threadRecord(A, null))
    }
    await pruneCodexSessionRecords(state)
    expect(readdirSync(join(state, "codex-sessions"))).toEqual([String(process.pid)])
  })
})

/** A session the owner would start for a thread, recording what is asked of it: its start and its
 *  shutdown each settle when the row says so. */
interface HeldSession {
  readonly session: CodexEnvoySession
  /** Lets its start finish. */
  start(): void
  /** Lets its shutdown finish. */
  shut(): void
}

function heldSession(thread: string, log: string[]): HeldSession {
  const started = Promise.withResolvers<void>()
  const shut = Promise.withResolvers<void>()
  let runtime: ChannelToolRuntime | undefined
  const session: CodexEnvoySession = {
    async start() {
      log.push(`start ${thread}`)
      await started.promise
      runtime = { identity: { id: thread } } as unknown as ChannelToolRuntime
    },
    runtime: () => runtime,
    notReady: () => undefined,
    ended: () => undefined,
    moveTo: () => undefined,
    withdraw: async () => {
      log.push(`withdraw ${thread}`)
    },
    async shutdown() {
      log.push(`shutdown ${thread}`)
      await shut.promise
    },
  }
  return { session, start: () => started.resolve(), shut: () => shut.resolve() }
}

describe("the owner's sessions of the shared app server's threads", () => {
  function owner() {
    const log: string[] = []
    const sessions: HeldSession[] = []
    const warnings: string[] = []
    const threads = createCodexThreads({
      session: (record) => {
        const held = heldSession(record.session_id, log)
        sessions.push(held)
        return held.session
      },
      warn: (text) => warnings.push(text),
    })
    return { log, sessions, threads, warnings }
  }
  const live = (...ids: string[]) => new Map(ids.map((id) => [id, threadRecord(id, "1")]))

  test("starts one session per live thread, and each tool call finds its own thread's", async () => {
    const { log, sessions, threads } = owner()
    threads.update(live(A, B))
    await waitFor(
      async () => log.length === 2,
      "both starts",
      () => log.join(", "),
    )
    expect(log.sort()).toEqual([`start ${A}`, `start ${B}`])
    expect(threads.runtime(A)).toBe("this thread's Envoy session is starting")
    for (const held of sessions) held.start()
    await waitFor(async () => typeof threads.runtime(B) !== "string", "B's session to run")
    expect((threads.runtime(A) as ChannelToolRuntime).identity.id).toBe(A)
    expect((threads.runtime(B) as ChannelToolRuntime).identity.id).toBe(B)
    expect(threads.runtime("019a0f3c-0000-7000-8000-000000000000")).toBe(UNREGISTERED_THREAD)
    // The same threads again start nothing more: a session is made at the update itself.
    threads.update(live(A, B))
    expect(sessions.length).toBe(2)
  })

  test("a thread that ends is shut down alone, and one loaded again starts once that shutdown ended", async () => {
    const { log, sessions, threads } = owner()
    threads.update(live(A, B))
    await waitFor(
      async () => log.length === 2,
      "both starts",
      () => log.join(", "),
    )
    for (const held of sessions) held.start()
    await waitFor(async () => typeof threads.runtime(A) !== "string", "A's session to run")

    threads.update(live(B))
    expect(log.slice(2)).toEqual([`shutdown ${A}`])
    expect(threads.runtime(A)).toBe(UNREGISTERED_THREAD)
    expect((threads.runtime(B) as ChannelToolRuntime).identity.id).toBe(B)

    // Loaded again while its last session still shuts down: the new one waits for it, so two
    // sessions under one id never race a registration against a deregistration.
    threads.update(live(A, B))
    expect(sessions.length).toBe(3)
    // Held on its shutdown, not on the clock: the start would otherwise run within this turn.
    await turn()
    expect(log.slice(2)).toEqual([`shutdown ${A}`])
    sessions[0]?.shut()
    await waitFor(
      async () => log.length === 4,
      "A's second start",
      () => log.join(", "),
    )
    expect(log.slice(2)).toEqual([`shutdown ${A}`, `start ${A}`])
  })

  test("a session that cannot start says why to its thread's tool calls, and the others run", async () => {
    const warnings: string[] = []
    const threads = createCodexThreads({
      session: (record) => {
        const held = heldSession(record.session_id, [])
        if (record.session_id === A) {
          return { ...held.session, start: async () => Promise.reject(new Error("no listener")) }
        }
        held.start()
        return held.session
      },
      warn: (text) => warnings.push(text),
    })
    threads.update(live(A, B))
    await waitFor(
      async () => warnings.length === 1,
      "A's failed start",
      () => warnings.join(", "),
    )
    expect(threads.runtime(A)).toBe("this thread's Envoy session could not start: no listener")
    expect(warnings).toEqual([`the Envoy session of thread ${A} could not start: no listener`])
    await waitFor(async () => typeof threads.runtime(B) !== "string", "B's session to run")
  })

  test("a shutdown stops every session, waits for them, and starts none after it", async () => {
    const { log, sessions, threads } = owner()
    threads.update(live(A, B))
    await waitFor(
      async () => log.length === 2,
      "both starts",
      () => log.join(", "),
    )
    await threads.withdraw()
    expect(log.slice(2).sort()).toEqual([`withdraw ${A}`, `withdraw ${B}`])
    let done = false
    const shutdown = threads.shutdown().then(() => {
      done = true
    })
    await turn()
    expect(done).toBe(false)
    for (const held of sessions) held.shut()
    await shutdown
    expect(log.slice(4).sort()).toEqual([`shutdown ${A}`, `shutdown ${B}`])
    threads.update(live(A, B))
    await turn()
    expect(sessions.length).toBe(2)
    expect(threads.runtime(A)).toBe(UNREGISTERED_THREAD)
  })
})

describe("an Envoy tool call another server of the shared app server sends to the owner", () => {
  test("is run as the thread it names, and answered with the tool's value or its failure", async () => {
    const servers = makeDirectory()
    const calls: unknown[] = []
    const route = await serveToolRoute(servers, async (call) => {
      calls.push(call)
      if (call.thread === B) throw new Error(`Envoy is not ready: ${UNREGISTERED_THREAD}`)
      return { session_id: call.thread }
    })
    try {
      expect(
        await forwardToolCall(servers, { arguments: {}, name: "envoy_whoami", thread: A }),
      ).toEqual({ session_id: A })
      await expect(
        forwardToolCall(servers, { arguments: { x: 1 }, name: "envoy_send", thread: B }),
      ).rejects.toThrow(UNREGISTERED_THREAD)
      expect(calls).toEqual([
        { arguments: {}, name: "envoy_whoami", thread: A },
        { arguments: { x: 1 }, name: "envoy_send", thread: B },
      ])
    } finally {
      await route.stop()
    }
    // Stopped: the route file is gone, and a call says that no server serves the threads now.
    expect(existsSync(toolRouteFile(servers))).toBe(false)
    await expect(
      forwardToolCall(servers, { arguments: {}, name: "envoy_whoami", thread: A }),
    ).rejects.toThrow("no server has taken them over yet")
  })

  test("goes to the newest owner, and an earlier owner's stop leaves that owner's route file", async () => {
    const servers = makeDirectory()
    await mkdir(servers, { recursive: true })
    const first = await serveToolRoute(servers, async () => "first")
    const second = await serveToolRoute(servers, async () => "second")
    try {
      expect(await forwardToolCall(servers, { arguments: {}, name: "x", thread: A })).toBe("second")
      await first.stop()
      expect(await forwardToolCall(servers, { arguments: {}, name: "x", thread: A })).toBe("second")
    } finally {
      await second.stop()
    }
  })

  test("to a socket nothing serves any more says the owner cannot be reached", async () => {
    const servers = makeDirectory()
    const route = await serveToolRoute(servers, async () => "answer")
    const file = Bun.file(toolRouteFile(servers))
    const left = await file.text()
    await route.stop()
    // A route file an owner that was killed left behind.
    await Bun.write(file, left)
    await expect(forwardToolCall(servers, { arguments: {}, name: "x", thread: A })).rejects.toThrow(
      "cannot be reached",
    )
  })
})
