/**
 * A preload for a hook process a row starts (`bun --preload <this file> …`): a census of the timed
 * waits the hook makes. It wraps `setTimeout` and `clearTimeout`, `setInterval`, `Bun.sleep`,
 * `Bun.sleepSync`, `AbortSignal.timeout` and the `timeout` of `Bun.spawnSync`, and when the process
 * exits it writes, to the file `HOOK_TIMED_WAITS` names, one entry per wait: its kind, its declared
 * bound, when it was armed on the hook's own clock, whether it ran to its bound, and the line that
 * armed it. A row reads it with `timedWaits`.
 *
 * The census counts what the hook asks for, not how long anything took, so a loaded or starved
 * machine changes no entry's bound. What it cannot see: a wait made without one of these (a child
 * or a socket read with no timeout, `node:timers/promises`), and the hook's own CPU work.
 */
import { writeFileSync } from "node:fs"

/** One timed wait of the hook. */
export interface TimedWait {
  readonly kind: string
  /** Its declared bound, in milliseconds; -1 for a `Bun.spawnSync` with no timeout. */
  readonly ms: number
  /** When it was armed, on the hook's `performance.now()`. */
  readonly armedAt: number
  /** "fired": it ran to its bound; "cleared" or "ended": it ended sooner; "armed": the hook exited
   *  first; "repeating": a `setInterval`, which fired at least once. */
  outcome: "armed" | "fired" | "cleared" | "ended" | "repeating"
  /** The line that armed it, the first stack frame outside this file. */
  readonly at: string
}

const record = process.env["HOOK_TIMED_WAITS"]
if (record !== undefined && record !== "") {
  const waits: TimedWait[] = []
  const armed = (kind: string, ms: number): TimedWait => {
    const at = (new Error().stack ?? "").split("\n")[3]?.trim() ?? "unknown"
    const wait: TimedWait = { kind, ms, armedAt: performance.now(), outcome: "armed", at }
    waits.push(wait)
    return wait
  }

  const realSetTimeout = globalThis.setTimeout
  const realClearTimeout = globalThis.clearTimeout
  const realSetInterval = globalThis.setInterval
  const byTimer = new Map<unknown, TimedWait>()
  globalThis.setTimeout = ((
    handler: (...args: unknown[]) => void,
    ms?: number,
    ...rest: unknown[]
  ) => {
    const wait = armed("setTimeout", Number(ms ?? 0))
    const timer = realSetTimeout(() => {
      wait.outcome = "fired"
      handler(...rest)
    }, ms)
    byTimer.set(timer, wait)
    return timer
  }) as unknown as typeof setTimeout
  globalThis.clearTimeout = ((timer: Parameters<typeof clearTimeout>[0]) => {
    const wait = byTimer.get(timer)
    if (wait?.outcome === "armed") wait.outcome = "cleared"
    realClearTimeout(timer)
  }) as typeof clearTimeout
  globalThis.setInterval = ((
    handler: (...args: unknown[]) => void,
    ms?: number,
    ...rest: unknown[]
  ) => {
    const wait = armed("setInterval", Number(ms ?? 0))
    return realSetInterval(() => {
      wait.outcome = "repeating"
      handler(...rest)
    }, ms)
  }) as unknown as typeof setInterval

  const realSleep = Bun.sleep
  Bun.sleep = ((ms: number | Date) => {
    const wait = armed("Bun.sleep", typeof ms === "number" ? ms : ms.getTime() - Date.now())
    return realSleep(ms).then(() => {
      wait.outcome = "fired"
    })
  }) as typeof Bun.sleep
  const realSleepSync = Bun.sleepSync
  Bun.sleepSync = ((ms: number) => {
    const wait = armed("Bun.sleepSync", ms)
    realSleepSync(ms)
    wait.outcome = "fired"
  }) as typeof Bun.sleepSync

  const realAbortTimeout = AbortSignal.timeout
  AbortSignal.timeout = (ms: number) => {
    const wait = armed("AbortSignal.timeout", ms)
    const signal = realAbortTimeout.call(AbortSignal, ms)
    signal.addEventListener("abort", () => {
      wait.outcome = "fired"
    })
    return signal
  }

  const realSpawnSync = Bun.spawnSync
  Bun.spawnSync = ((...args: unknown[]) => {
    const options = (Array.isArray(args[0]) ? args[1] : args[0]) as { timeout?: number } | undefined
    const wait = armed("Bun.spawnSync", options?.timeout ?? -1)
    const result = (realSpawnSync as (...a: unknown[]) => { exitedDueToTimeout?: boolean })(...args)
    wait.outcome = result.exitedDueToTimeout === true ? "fired" : "ended"
    return result
  }) as typeof Bun.spawnSync

  process.on("exit", () => {
    writeFileSync(record, JSON.stringify(waits))
  })
}
