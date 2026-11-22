/**
 * A preload for a hook process a row starts (`bun --preload <this file> …`): a census of the timed
 * waits the hook makes and of the processes it starts. It wraps `setTimeout` and `clearTimeout`,
 * `setInterval`, `Bun.sleep`, `Bun.sleepSync` and `AbortSignal.timeout`, and `Bun.spawn`,
 * `Bun.spawnSync` and `Bun.$`; when the process exits it writes, to the file `HOOK_TIMED_WAITS`
 * names, one entry per wait and one per start: its kind, its declared bound, when it was made on
 * the hook's own clock, how it ended, and the line of the hook's code that made it.
 *
 * In Bun 1.4.4 `node:timers/promises` arms its timers through the global `setTimeout`, and
 * `node:child_process` starts its children through the global `Bun.spawn` and `Bun.spawnSync`, so
 * both are counted here (`tests/open-asks-hook.test.ts`'s census rows rely on it).
 *
 * The census counts what the hook asks for, not how long anything took, so a loaded or starved
 * machine changes no entry. What it cannot see: a wait made through none of these (a socket, a
 * stream read), which `hook-wait-sites.ts` finds in the code instead, and the hook's own CPU work.
 */
import { writeFileSync } from "node:fs"
import type { Subprocess, SyncSubprocess } from "bun"

/** One timed wait or process start of the hook. */
export interface TimedWait {
  /** `setTimeout`, `setInterval`, `Bun.sleep`, `Bun.sleepSync`, `AbortSignal.timeout`,
   *  `Bun.spawn`, `Bun.spawnSync` or `Bun.$`. */
  readonly kind: string
  /** Its declared bound, in milliseconds: a timer's delay, a start's `timeout`, or -1 for a start
   *  with none. */
  readonly ms: number
  /** For a start, the name of the program it started (`Bun.$`: its command's first word). */
  readonly command?: string
  /** When it was made, on the hook's `performance.now()`. */
  readonly armedAt: number
  /** "fired": it ran to its bound; "cleared" or "ended": it ended sooner; "armed": the hook exited
   *  first; "repeating": a `setInterval`, which fired at least once. */
  outcome: "armed" | "fired" | "cleared" | "ended" | "repeating"
  /** The frame of the hook's code that made it: the first one outside this file and Bun's own. */
  readonly at: string
}

const record = process.env["HOOK_TIMED_WAITS"]
if (record !== undefined && record !== "") {
  const waits: TimedWait[] = []
  const here = import.meta.path
  const caller = (): string => {
    const frames = (new Error().stack ?? "").split("\n").slice(1)
    // The first frame in a file of the hook's: not this file, and not Bun's own (`node:` modules,
    // native code, or a frame with no file, as `Bun.$`'s Proxy leaves above its caller).
    const own = frames.find(
      (frame) => !frame.includes(here) && /\(?\/[^():]+:\d+:\d+\)?$/.test(frame.trim()),
    )
    return own?.trim() ?? "unknown"
  }
  const made = (kind: string, ms: number, command?: string): TimedWait => {
    const wait: TimedWait = {
      kind,
      ms,
      ...(command === undefined ? {} : { command }),
      armedAt: performance.now(),
      outcome: "armed",
      at: caller(),
    }
    waits.push(wait)
    return wait
  }
  /** The program and the `timeout` of a `Bun.spawn` or `Bun.spawnSync` call, in either form:
   *  `(argv, options)` or `({ cmd, …options })`. */
  const startOf = (args: unknown[]): { command: string; timeout: number } => {
    const [first, second] = args
    const isObject = (value: unknown): value is object =>
      typeof value === "object" && value !== null
    const argv = Array.isArray(first)
      ? first
      : isObject(first) && "cmd" in first
        ? first.cmd
        : undefined
    const options = Array.isArray(first) ? second : first
    const command = Array.isArray(argv) && typeof argv[0] === "string" ? argv[0] : "unknown"
    const declared = isObject(options) && "timeout" in options ? options.timeout : undefined
    return { command, timeout: typeof declared === "number" && declared > 0 ? declared : -1 }
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
    const wait = made("setTimeout", Number(ms ?? 0))
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
    const wait = made("setInterval", Number(ms ?? 0))
    return realSetInterval(() => {
      wait.outcome = "repeating"
      handler(...rest)
    }, ms)
  }) as unknown as typeof setInterval

  const realSleep = Bun.sleep
  Bun.sleep = ((ms: number | Date) => {
    const wait = made("Bun.sleep", typeof ms === "number" ? ms : ms.getTime() - Date.now())
    return realSleep(ms).then(() => {
      wait.outcome = "fired"
    })
  }) as typeof Bun.sleep
  const realSleepSync = Bun.sleepSync
  Bun.sleepSync = ((ms: number) => {
    const wait = made("Bun.sleepSync", ms)
    realSleepSync(ms)
    wait.outcome = "fired"
  }) as typeof Bun.sleepSync

  const realAbortTimeout = AbortSignal.timeout
  AbortSignal.timeout = (ms: number) => {
    const wait = made("AbortSignal.timeout", ms)
    const signal = realAbortTimeout.call(AbortSignal, ms)
    signal.addEventListener("abort", () => {
      wait.outcome = "fired"
    })
    return signal
  }

  const realSpawn = Bun.spawn
  Bun.spawn = ((...args: unknown[]) => {
    const { command, timeout } = startOf(args)
    const wait = made("Bun.spawn", timeout, command)
    const child: Subprocess = Reflect.apply(realSpawn, Bun, args)
    void child.exited.then(() => {
      wait.outcome = timeout > 0 && child.signalCode !== null ? "fired" : "ended"
    })
    return child
  }) as typeof Bun.spawn
  const realSpawnSync = Bun.spawnSync
  Bun.spawnSync = ((...args: unknown[]) => {
    const { command, timeout } = startOf(args)
    const wait = made("Bun.spawnSync", timeout, command)
    const result: SyncSubprocess = Reflect.apply(realSpawnSync, Bun, args)
    wait.outcome = result.exitedDueToTimeout === true ? "fired" : "ended"
    return result
  }) as typeof Bun.spawnSync
  Bun.$ = new Proxy(Bun.$, {
    apply(target, thisArg, args: unknown[]) {
      const strings = args[0]
      const text =
        typeof strings === "string"
          ? strings
          : Array.isArray(strings) && typeof strings[0] === "string"
            ? strings[0]
            : ""
      made("Bun.$", -1, text.trim().split(/\s+/)[0] ?? "unknown").outcome = "ended"
      return Reflect.apply(target, thisArg, args)
    },
  })

  process.on("exit", () => {
    writeFileSync(record, JSON.stringify(waits))
  })
}
