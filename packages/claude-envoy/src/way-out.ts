import { SHUTDOWN_SETTLE_MS } from "@legion/envoy-client/envoy-settings"
import type { ChannelRecord, ShutdownWait } from "./channel-record"

/**
 * The channel server's way out, from the signal or the end of stdin to the process's exit, in one
 * place: each step waits for its fact, and only the exit bound below ends what is still running.
 *
 * A step waits for its fact unless the product decides to go on without it: the bounds of
 * `SHUTDOWN_BOUNDS`, each a wait whose end the next step makes moot (a drain the close ends, the
 * registration writes the DELETE goes out past). The last state and turn writes, the DELETE, the
 * connection's close, the mod link's bind and close and the MCP server's close have no bound of
 * their own: a write still running there is one a correct shutdown completes, so the way waits
 * for it, and a write that never ends is ended by `SHUTDOWN_EXIT_MS` alone, which says so on
 * stderr, naming the steps it drops, and exits 1.
 */

/**
 * How long an MCP client gives a stdio server from the end of its stdin to SIGKILL: the MCP SDK's
 * close ends stdin, waits 2 s, sends SIGTERM, waits 2 s more, then SIGKILL
 * (`StdioClientTransport.close`, @modelcontextprotocol/sdk 1.30.0, the version in this tree;
 * Claude Code's client is taken to close the same way, which this tree cannot read). The pod's
 * worker shim gives the agent 5 s (AGENTS.md, `legion worker-shim`), inside the pod's
 * `terminationGracePeriodSeconds` (10 s by default, 9 s at least), so this one is the shortest.
 */
export const CLIENT_KILL_MS = 4_000

/** How long an exit on the bound gives stdout and stderr to take what was written, the line that
 *  says why among it, before it exits whatever is still queued (`exitProcess`). */
export const EXIT_FLUSH_MS = 500

/** What is left before the client's SIGKILL once the exit's flush has run out: the room a loaded
 *  machine's late timer takes. */
const KILL_MARGIN_MS = 500

/**
 * The most the way out takes: past it the process exits 1, whatever await, timer or handle is
 * still running, before the client's SIGKILL with the flush and the margin to spare. The slowest
 * correct way out read 185 ms from SIGTERM to the process's exit under 8 busy loops on 2 CPUs
 * (envoy-channel-stall.test.ts, builds Job d5-stall-8, 2026-11-19).
 */
export const SHUTDOWN_EXIT_MS = CLIENT_KILL_MS - EXIT_FLUSH_MS - KILL_MARGIN_MS

/** The bounds the way out sets itself, by step: each the most it waits before it goes on without
 *  the fact, since the next step makes that fact moot. */
export const SHUTDOWN_BOUNDS = {
  /** The NATS drain of a session's connection: past it the close ends the connection. */
  drain: 1_000,
  /** The registration writes still in flight, the session's chain and one on the wire, together:
   *  past it the DELETE goes out anyway (`SHUTDOWN_SETTLE_MS`, which pi-envoy shares). */
  settle: SHUTDOWN_SETTLE_MS,
  /** A failed start's drain of a connection that may still be redialing, which may never finish:
   *  the close after it ends the connection for good. */
  "start-drain": 2_000,
} as const satisfies Partial<Record<ShutdownWait, number>>

/**
 * The most the bounds above take on the way out's longest path: a session's drain and settle, or
 * a failed start's drain, which has no session to settle. Every other step waits for its fact, so
 * what this leaves under `SHUTDOWN_EXIT_MS` is theirs (channel-way-out.test.ts holds it there).
 */
export const SHUTDOWN_BOUNDS_WORST_MS = Math.max(
  SHUTDOWN_BOUNDS.drain + SHUTDOWN_BOUNDS.settle,
  SHUTDOWN_BOUNDS["start-drain"],
)

/** The way out's steps as they run: each waits, is recorded, and is named by the exit bound while
 *  it runs. */
export interface WayOut {
  /**
   * Waits for `work` to settle, fulfilled or rejected, and records the step with what ended it;
   * where `boundMs` is given, it goes on once that has passed instead, naming the bound. Never
   * rejects.
   */
  step(wait: ShutdownWait, work: Promise<unknown>, boundMs?: number): Promise<"fact" | "bound">
  /** The steps started and not yet ended, in the order they started: what an exit now drops. */
  running(): readonly ShutdownWait[]
}

/** A way out that records each step to `record` (the tests' record, unset in production). */
export function wayOut(record: ChannelRecord | undefined): WayOut {
  const running: ShutdownWait[] = []
  return {
    async step(wait, work, boundMs) {
      running.push(wait)
      try {
        const endedBy =
          boundMs === undefined
            ? await work.then(
                () => "fact" as const,
                () => "fact" as const,
              )
            : await settledOrGivenUp(work, boundMs)
        record?.(boundMs === undefined ? { wait, endedBy } : { wait, armedMs: boundMs, endedBy })
        return endedBy
      } finally {
        running.splice(running.indexOf(wait), 1)
      }
    },
    running: () => [...running],
  }
}

/**
 * Resolves when `work` settles, fulfilled or rejected, or once `ms` have passed, naming which came
 * first; never rejects. The timer is cleared either way, so a wait that ended on its fact holds
 * nothing open.
 */
export async function settledOrGivenUp(
  work: Promise<unknown>,
  ms: number,
): Promise<"fact" | "bound"> {
  const { promise, resolve } = Promise.withResolvers<"bound">()
  const timer = setTimeout(() => resolve("bound"), ms)
  try {
    return await Promise.race([
      work.then(
        () => "fact" as const,
        () => "fact" as const,
      ),
      promise,
    ])
  } finally {
    clearTimeout(timer)
  }
}
