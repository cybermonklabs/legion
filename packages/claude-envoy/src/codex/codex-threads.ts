import { messageFor } from "@legion/envoy-client/errors"
import type { ChannelToolRuntime } from "../envoy-channel-server"
import type { CodexEnvoySession } from "./codex-envoy-session"
import type { CodexSessionRecord } from "./codex-session-record"

/**
 * The Envoy sessions of Codex's shared app server, one per thread, which the server that owns that
 * process runs (`./codex-owner`).
 *
 * Codex runs each terminal session of plain `codex` (0.170.0 and 0.171.0), and each session of
 * `codex agents`, as one thread of a single `codex app-server` process, and starts one plugin MCP
 * server per thread there. A thread's own server cannot register its thread at the thread's first
 * prompt: Codex names no thread when it starts a server, only on each tool call (`_meta.threadId`),
 * and many turns call no plugin tool. So the `SessionStart` hook records each thread on its own
 * (`codexThreadRecordFile`), and the owning server starts an Envoy session for each recorded thread
 * that has not ended (`readLiveThreads`), each under the thread's id, with a registration, a
 * heartbeat, a connection and a live view of its own. A thread's `SessionEnd` marker shuts its
 * session down, so it leaves the Agents page while the others keep theirs, and a `SessionStart`
 * that loads the thread again starts a new one under the same id, once the last one has shut down.
 * When the owner stops (Codex stops its server when it unloads the owner's own thread) its sessions
 * go with it, and the next server to own the process starts one for each live thread again.
 */
export interface CodexThreads {
  /** Starts a session for each thread of `live` that has none, and shuts down the session of each
   *  thread no longer in it. */
  update(live: ReadonlyMap<string, CodexSessionRecord>): void
  /** What thread `thread`'s Envoy tools act through, or why they cannot act yet. */
  runtime(thread: string): ChannelToolRuntime | string
  /** Withdraws every session's waiting lines (`CodexEnvoySession.withdraw`); resolves once all are
   *  withdrawn. */
  withdraw(): Promise<void>
  /** Shuts every session down, and those already shutting down are waited for; no session starts
   *  after it. */
  shutdown(): Promise<void>
}

export interface CodexThreadsOptions {
  /** A session for the thread `record` names, not yet started. */
  readonly session: (record: CodexSessionRecord) => CodexEnvoySession
  readonly warn: (text: string) => void
}

/** Why a thread has no Envoy session to act as. */
export const UNREGISTERED_THREAD =
  "this thread has no Envoy session: the Envoy plugin registers a thread of Codex's shared app server from its first prompt, once Codex runs its trusted SessionStart hook, until Codex unloads it; a fork (/side, /btw) or an ephemeral session is never registered"

interface Running {
  readonly session: CodexEnvoySession
  /** Why its start failed, once it has. */
  failure?: string
}

export function createCodexThreads(options: CodexThreadsOptions): CodexThreads {
  const running = new Map<string, Running>()
  /** The shutdown of each thread's last session still under way, which its next start waits for:
   *  a session started beside it under the same id would race its deregistration. */
  const ending = new Map<string, Promise<void>>()
  let stopped = false

  const begin = (thread: string, record: CodexSessionRecord): void => {
    const entry: Running = { session: options.session(record) }
    running.set(thread, entry)
    void (ending.get(thread) ?? Promise.resolve())
      .then(() => (running.get(thread) === entry ? entry.session.start() : undefined))
      .catch((error: unknown) => {
        entry.failure = messageFor(error)
        options.warn(`the Envoy session of thread ${thread} could not start: ${entry.failure}`)
      })
  }

  const end = (thread: string, entry: Running): void => {
    running.delete(thread)
    const done = entry.session.shutdown().catch((error: unknown) => {
      options.warn(`the Envoy session of thread ${thread} did not shut down: ${messageFor(error)}`)
    })
    ending.set(thread, done)
    void done.then(() => {
      if (ending.get(thread) === done) ending.delete(thread)
    })
  }

  return {
    update(live) {
      if (stopped) return
      for (const [thread, entry] of [...running]) if (!live.has(thread)) end(thread, entry)
      for (const [thread, record] of live) if (!running.has(thread)) begin(thread, record)
    },
    runtime(thread) {
      const entry = running.get(thread)
      if (entry === undefined) return UNREGISTERED_THREAD
      if (entry.failure !== undefined) {
        return `this thread's Envoy session could not start: ${entry.failure}`
      }
      return (
        entry.session.runtime() ??
        entry.session.notReady() ??
        "this thread's Envoy session is starting"
      )
    },
    async withdraw() {
      await Promise.all([...running.values()].map((entry) => entry.session.withdraw()))
    },
    async shutdown() {
      stopped = true
      for (const [thread, entry] of [...running]) end(thread, entry)
      await Promise.all(ending.values())
    },
  }
}
