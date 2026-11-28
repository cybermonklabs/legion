/**
 * A wait of a row that starts a child process, which ends on what the child does and fails by name
 * when that never comes, before the row's hang guard does.
 *
 * The guard, `ROW_GUARD_MS`, is a hang guard and decides no verdict: 120 s, far above any real
 * duration of these rows (the slowest, run with its whole file on one CPU that 200 busy loops
 * share, took 39.7 s), and the value #4924's `HANG_GUARD_MS` holds. Each file that uses this module
 * sets it as its rows' default (`setDefaultTimeout(ROW_GUARD_MS)`: a row's own limit wins over it,
 * and it wins over the package script's `--timeout`), so the rows state no limit of their own. A
 * wait here gives up `NAMED_BEFORE_GUARD_MS` before it, counted from the row's start (`rowStarted`,
 * which each such file calls from a `beforeEach` of its own), ends the child, and fails naming what
 * it waited for and what the child had written. A child that a loaded machine merely slows takes
 * what it takes up to there.
 */
import type { Subprocess } from "bun"

/** Each row's hang guard, which every file that uses this module sets as its rows' default. */
export const ROW_GUARD_MS = 120_000
/** How long before the guard a wait here gives up: room for the row's last steps after it and
 *  for its failure to be printed, where Bun's own timeout line would name nothing. */
export const NAMED_BEFORE_GUARD_MS = 5_000

let rowStartedAt = performance.now()

/** Marks the row's start: each file that waits here calls it from a `beforeEach` of its own, since
 *  a hook this shared module registered would take the scope of the first file that imported it. */
export function rowStarted(): void {
  rowStartedAt = performance.now()
}

/** What a stream has carried so far, read as it arrives, so a wait that gives up can say it. */
export function collected(stream: ReadableStream<Uint8Array>): {
  readonly text: () => string
  readonly ended: Promise<string>
} {
  let text = ""
  const decoder = new TextDecoder()
  const ended = (async () => {
    for await (const chunk of stream) text += decoder.decode(chunk, { stream: true })
    text += decoder.decode()
    return text
  })()
  return { text: () => text, ended }
}

/** The milliseconds left until a wait here gives up, `NAMED_BEFORE_GUARD_MS` before the row's
 *  guard: the deadline to hand a wait that names itself when it gives up (`waitFor`, an MCP
 *  `connect`'s `timeout`), so it fails by its own name where Bun's line would name nothing.
 *  `guardMs` is the limit the row states, `ROW_GUARD_MS` unless the row states another, such as
 *  the 600 s of a row that chains whole server steps (`ROW_TIMEOUT_MS`, `./reopen-server.ts`). */
export function untilTheGuard(guardMs: number = ROW_GUARD_MS): number {
  return Math.max(0, rowStartedAt + guardMs - NAMED_BEFORE_GUARD_MS - performance.now())
}

/**
 * `promise`, or a failure naming `what` and what `state` reports once the row's time up to
 * `NAMED_BEFORE_GUARD_MS` before its guard has passed; `onLate` runs first, to end the child the
 * row is waiting on.
 */
export async function beforeTheGuard<T>(
  promise: Promise<T>,
  what: string,
  state: () => string,
  onLate?: () => void,
): Promise<T> {
  const left = untilTheGuard()
  const late = Promise.withResolvers<{ readonly late: true }>()
  const timer = setTimeout(() => late.resolve({ late: true }), left)
  try {
    const first = await Promise.race([promise.then((value) => ({ value })), late.promise])
    if ("value" in first) return first.value
    onLate?.()
    throw new Error(
      `${what} had not happened ${ROW_GUARD_MS - NAMED_BEFORE_GUARD_MS} ms into the row: ${state()}`,
    )
  } finally {
    clearTimeout(timer)
  }
}

/** `child`'s exit code once it exits, or a failure naming it (`what`) and what it wrote, with the
 *  child ended, when it has not exited by the time `beforeTheGuard` gives up. */
export function exitBeforeTheGuard(
  child: Subprocess,
  what: string,
  wrote: () => string,
): Promise<number> {
  return beforeTheGuard(child.exited, `${what}'s exit`, wrote, () => child.kill("SIGKILL"))
}
