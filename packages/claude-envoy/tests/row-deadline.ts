/**
 * A wait of a row that starts a child process, which ends on what the child does and fails by name
 * when that never comes, before the row's hang guard does.
 *
 * The guard is the package test script's own `--timeout` (`bun test --timeout=60000`,
 * package.json), `ROW_GUARD_MS`, which the rows that use this module also state as their own
 * limit, a number at each row (Biome formats a `test(…)` call whose last argument is a number as a
 * test call), so a run of the file alone has it too. It is a hang guard and decides no verdict: a
 * wait here gives up `NAMED_BEFORE_GUARD_MS` before it, counted from the row's start
 * (`rowStarted`, which each such file calls from a `beforeEach` of its own), ends the child, and
 * fails naming what it waited for and what the child had written. A child that a loaded machine
 * merely slows takes what it takes up to there.
 */
import type { Subprocess } from "bun"

/** The package test script's `--timeout`, each row's hang guard. */
export const ROW_GUARD_MS = 60_000
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
  const left = rowStartedAt + ROW_GUARD_MS - NAMED_BEFORE_GUARD_MS - performance.now()
  const late = Promise.withResolvers<{ readonly late: true }>()
  const timer = setTimeout(() => late.resolve({ late: true }), Math.max(0, left))
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
