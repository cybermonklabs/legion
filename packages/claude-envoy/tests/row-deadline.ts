/**
 * A wait of a row that starts a child process, which ends on what the child does and fails by name
 * when that never comes, before the row's hang guard does.
 *
 * A guard is a hang guard and decides no verdict. Each file that uses one sets it as its rows'
 * default (`setDefaultTimeout`: a row's own limit wins over it, and it wins over the package
 * script's `--timeout`). There are two:
 *
 * - `ROW_GUARD_MS`, 120 s, the value #4924's `HANG_GUARD_MS` holds. It is the default of
 *   `channel-fatal-exit.test.ts`, `envoy-channel-lifecycle.test.ts`,
 *   `envoy-channel-server.test.ts`, `envoy-send-url.test.ts` and `legion-pane-mcp.test.ts`. #4955
 *   recorded 39.7 s as the slowest row of the files that used this module then, each run whole on
 *   one CPU that 200 busy loops share. These five files' comments give no later reading. One of
 *   their rows states a limit of its own: `channel-fatal-exit.test.ts`'s late parent, this guard
 *   plus the wait it makes before its stop (`DIAL_TIMEOUT_MS` and 1 s).
 * - `JOB_ROOM_GUARD_MS`, 240 s, below. It is the default of `open-asks-hook.test.ts` and
 *   `envoy-channel-mcp.test.ts`, whose rows a loaded host stretched past a sixth of
 *   `ROW_GUARD_MS`. Their worst rows read 59 101 ms (open-asks, at 82306880) and 82 600 ms (mcp,
 *   at 0faaee16), each over 20 runs on two CPUs, with each file's comment naming its runs. mcp's
 *   two broker-token rows state this guard as their limit. mcp's dial row states `ROW_TIMEOUT_MS`
 *   (600 s, `./reopen-server.ts`), 7.1 times its 84 395 ms.
 *
 * A wait here gives up `NAMED_BEFORE_GUARD_MS` before the row's guard, counted from the row's
 * start (`rowStarted`, which each file that waits here calls from a `beforeEach` of its own). It
 * then ends the child and fails naming what it waited for and what the child had written. A row
 * whose guard is not `ROW_GUARD_MS` hands its guard to each wait (`guardMs`), and to
 * `./mcp-connect.ts`'s `connectedToServer`. A child that a loaded machine merely slows takes what
 * it takes up to there.
 */
import type { Subprocess } from "bun"

/** The hang guard the header names its files for, which each sets as its rows' default. */
export const ROW_GUARD_MS = 120_000
/**
 * The hang guard of the files whose rows a loaded host stretches past a sixth of `ROW_GUARD_MS`
 * (`open-asks-hook.test.ts` and `envoy-channel-mcp.test.ts`, each with its readings): not six
 * times a reading, which is past what the claude-envoy job leaves one hang, but that room. The
 * job's `timeout-minutes: 10` less its test step's 285 s and its other steps' 51 s at 82306880
 * (CI run 45394586867) leaves 264 s, and both files run in that one job, so this room is for one
 * hang in it, whichever file holds it. A row that uses it states it as its limit and hands it to
 * each wait here (`untilTheGuard`, `beforeTheGuard`, `exitBeforeTheGuard`).
 */
export const JOB_ROOM_GUARD_MS = 240_000
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
 * row is waiting on. `guardMs` is the limit the row states, as for `untilTheGuard`.
 */
export async function beforeTheGuard<T>(
  promise: Promise<T>,
  what: string,
  state: () => string,
  onLate?: () => void,
  guardMs: number = ROW_GUARD_MS,
): Promise<T> {
  const left = untilTheGuard(guardMs)
  const late = Promise.withResolvers<{ readonly late: true }>()
  const timer = setTimeout(() => late.resolve({ late: true }), left)
  try {
    const first = await Promise.race([promise.then((value) => ({ value })), late.promise])
    if ("value" in first) return first.value
    onLate?.()
    throw new Error(
      `${what} had not happened ${guardMs - NAMED_BEFORE_GUARD_MS} ms into the row: ${state()}`,
    )
  } finally {
    clearTimeout(timer)
  }
}

/** `child`'s exit code once it exits, or a failure naming it (`what`) and what it wrote, with the
 *  child ended, when it has not exited by the time `beforeTheGuard` gives up under `guardMs`. */
export function exitBeforeTheGuard(
  child: Subprocess,
  what: string,
  wrote: () => string,
  guardMs: number = ROW_GUARD_MS,
): Promise<number> {
  return beforeTheGuard(child.exited, `${what}'s exit`, wrote, () => child.kill("SIGKILL"), guardMs)
}
