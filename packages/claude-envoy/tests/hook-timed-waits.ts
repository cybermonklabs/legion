/**
 * A preload for a hook process a row starts (`bun --preload <this file> …`): a census of the timed
 * waits the hook makes, of the processes it starts, and of its turns of the event loop and reads of
 * the clock. It wraps `setTimeout` and `clearTimeout`, `setInterval`, `setImmediate`, `Bun.sleep`,
 * `Bun.sleepSync`, `AbortSignal.timeout`, `Atomics.wait` and `Atomics.waitAsync`; `Bun.spawn`, `Bun.spawnSync` and
 * `Bun.$`; and
 * `Date.now`, `new Date()`, `Date()`, `performance.now`, `performance.mark`, `performance.measure`,
 * `process.hrtime`, `process.uptime` and `Bun.nanoseconds`.
 * When the process exits it writes, to the file `HOOK_TIMED_WAITS` names, a `Census`: one entry per
 * wait above zero and one per start (its kind, its declared bound, when it was made, how it ended,
 * and the line of the hook's code that made it), and how many zero-length timers it armed and how
 * many times it read the clock.
 *
 * In Bun 1.4.4 `node:timers/promises` arms its timers through the global `setTimeout`, and
 * `node:child_process` starts its children through the global `Bun.spawn` and `Bun.spawnSync`, so
 * both are counted here (`tests/open-asks-hook.test.ts`'s census rows rely on it).
 *
 * The census counts what the hook asks for, not how long anything took, so a loaded or starved
 * machine changes no entry and no count. What it cannot see: a wait made through none of these (a
 * socket, a stream read), which `hook-wait-sites.ts` finds in the code instead, and the hook's own
 * CPU work that reads no clock.
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
  /** When it took its outcome, on the hook's `performance.now()` (a `setInterval`: when it first
   *  fired, or was cleared); undefined while it is armed. */
  endedAt?: number
  /** The frame of the hook's code that made it: the first one outside this file and Bun's own. */
  readonly at: string
}

/** What the census file holds. */
export interface Census {
  readonly waits: TimedWait[]
  /** `setTimeout` or `setInterval` with no delay above zero, and `setImmediate`. */
  readonly zeroLengthTimers: number
  readonly clockReads: number
  /** When the process exited, on the hook's `performance.now()`. */
  readonly exitedAt: number
  /** The CPU time the hook's process had used at its exit, over all its threads, user and system
   *  together (`process.cpuUsage()`), in milliseconds. */
  readonly cpuMs: number
}

const record = process.env["HOOK_TIMED_WAITS"]
if (record !== undefined && record !== "") {
  const waits: TimedWait[] = []
  // The hook's turns of the event loop and its reads of the clock, which no entry above holds: a
  // wait made of many zero-length timers, or of a loop on the clock, shows as a count far above
  // the path's own. The census's own times come from the real clock, read before it is wrapped.
  const counts = { zeroLengthTimers: 0, clockReads: 0 }
  const now = performance.now.bind(performance)
  const realDateNow = Date.now
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
  const ended = (wait: TimedWait, outcome: TimedWait["outcome"]): void => {
    wait.outcome = outcome
    wait.endedAt ??= now()
  }
  const made = (kind: string, ms: number, command?: string): TimedWait => {
    const wait: TimedWait = {
      kind,
      ms,
      ...(command === undefined ? {} : { command }),
      armedAt: now(),
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
    const delay = Number(ms ?? 0)
    if (!(delay > 0)) {
      counts.zeroLengthTimers++
      return realSetTimeout(handler, ms, ...rest)
    }
    const wait = made("setTimeout", delay)
    const timer = realSetTimeout(() => {
      ended(wait, "fired")
      handler(...rest)
    }, ms)
    byTimer.set(timer, wait)
    return timer
  }) as unknown as typeof setTimeout
  globalThis.clearTimeout = ((timer: Parameters<typeof clearTimeout>[0]) => {
    const wait = byTimer.get(timer)
    if (wait !== undefined && (wait.outcome === "armed" || wait.outcome === "repeating")) {
      wait.outcome = "cleared"
      wait.endedAt = now()
    }
    realClearTimeout(timer)
  }) as typeof clearTimeout
  globalThis.setInterval = ((
    handler: (...args: unknown[]) => void,
    ms?: number,
    ...rest: unknown[]
  ) => {
    const delay = Number(ms ?? 0)
    if (!(delay > 0)) counts.zeroLengthTimers++
    const wait = made("setInterval", delay)
    const interval = realSetInterval(() => {
      // A repeating timer ends when it is cleared, not when it first fires.
      if (wait.outcome === "armed") wait.outcome = "repeating"
      handler(...rest)
    }, ms)
    byTimer.set(interval, wait)
    return interval
  }) as unknown as typeof setInterval
  const realClearInterval = globalThis.clearInterval
  globalThis.clearInterval = ((timer: Parameters<typeof clearInterval>[0]) => {
    const wait = byTimer.get(timer)
    if (wait !== undefined && (wait.outcome === "armed" || wait.outcome === "repeating")) {
      wait.outcome = "cleared"
      wait.endedAt = now()
    }
    realClearInterval(timer)
  }) as typeof clearInterval
  const realSetImmediate = globalThis.setImmediate
  globalThis.setImmediate = ((...args: Parameters<typeof setImmediate>) => {
    counts.zeroLengthTimers++
    return realSetImmediate(...args)
  }) as unknown as typeof setImmediate

  // Every read of the clock the hook's code can make through the process's own clocks: Date.now, a
  // Date made with no argument or Date() called, performance.now, performance.mark and
  // performance.measure, process.hrtime and its bigint, process.uptime, Bun.nanoseconds, and each
  // function of Temporal.Now (Bun 1.4.4 has six, `instant` to `zonedDateTimeISO`; `timeZoneId`
  // reads no time, and is counted with them). What reads time some other way (os.uptime, an Intl
  // format given no date, a file's times) is not counted: a poll on it shows as the CPU it burns
  // (`cpuMs`) or the timers it arms.
  const RealDate = Date
  const countedNow = (): number => {
    counts.clockReads++
    return realDateNow()
  }
  globalThis.Date = new Proxy(RealDate, {
    construct(target, args, newTarget) {
      if (args.length === 0) counts.clockReads++
      return Reflect.construct(target, args, newTarget)
    },
    apply(target, thisArg, args) {
      counts.clockReads++
      return Reflect.apply(target, thisArg, args)
    },
    get(target, property, receiver) {
      return property === "now" ? countedNow : Reflect.get(target, property, receiver)
    },
  })
  performance.now = () => {
    counts.clockReads++
    return now()
  }
  const realMark = performance.mark.bind(performance)
  performance.mark = ((...args: Parameters<typeof performance.mark>) => {
    counts.clockReads++
    return realMark(...args)
  }) as typeof performance.mark
  const realMeasure = performance.measure.bind(performance)
  performance.measure = ((...args: Parameters<typeof performance.measure>) => {
    counts.clockReads++
    return realMeasure(...args)
  }) as typeof performance.measure
  const realUptime = process.uptime.bind(process)
  process.uptime = () => {
    counts.clockReads++
    return realUptime()
  }
  const realHrtime = process.hrtime
  process.hrtime = Object.assign(
    (time?: [number, number]) => {
      counts.clockReads++
      return realHrtime(time)
    },
    {
      bigint: () => {
        counts.clockReads++
        return realHrtime.bigint()
      },
    },
  )
  // Temporal.Now's functions are its own properties, not enumerable ones.
  const temporalNow = (globalThis as { Temporal?: { Now?: Record<string, unknown> } }).Temporal?.Now
  for (const name of temporalNow === undefined ? [] : Object.getOwnPropertyNames(temporalNow)) {
    const read = temporalNow?.[name]
    if (typeof read !== "function" || temporalNow === undefined) continue
    temporalNow[name] = (...args: unknown[]) => {
      counts.clockReads++
      return Reflect.apply(read, temporalNow, args)
    }
  }
  const realNanoseconds = Bun.nanoseconds
  Bun.nanoseconds = () => {
    counts.clockReads++
    return realNanoseconds()
  }

  const realSleep = Bun.sleep
  Bun.sleep = ((ms: number | Date) => {
    const wait = made("Bun.sleep", typeof ms === "number" ? ms : ms.getTime() - realDateNow())
    return realSleep(ms).then(() => {
      ended(wait, "fired")
    })
  }) as typeof Bun.sleep
  const realSleepSync = Bun.sleepSync
  Bun.sleepSync = ((ms: number) => {
    const wait = made("Bun.sleepSync", ms)
    realSleepSync(ms)
    ended(wait, "fired")
  }) as typeof Bun.sleepSync

  const realAbortTimeout = AbortSignal.timeout
  AbortSignal.timeout = (ms: number) => {
    const wait = made("AbortSignal.timeout", ms)
    const signal = realAbortTimeout.call(AbortSignal, ms)
    signal.addEventListener("abort", () => {
      ended(wait, "fired")
    })
    return signal
  }

  // A thread's block on shared memory: a timed wait of its own, -1 where it has no timeout.
  const realAtomicsWait = Atomics.wait
  Atomics.wait = ((...args: unknown[]) => {
    const timeout = args[3]
    const bounded = typeof timeout === "number" && Number.isFinite(timeout) && timeout > 0
    const wait = made("Atomics.wait", bounded ? timeout : -1)
    const answer: string = Reflect.apply(realAtomicsWait, Atomics, args)
    ended(wait, answer === "timed-out" ? "fired" : "ended")
    return answer
  }) as typeof Atomics.wait

  const realAtomicsWaitAsync = Atomics.waitAsync
  Atomics.waitAsync = ((...args: unknown[]) => {
    const timeout = args[3]
    const bounded = typeof timeout === "number" && Number.isFinite(timeout) && timeout > 0
    const wait = made("Atomics.waitAsync", bounded ? timeout : -1)
    const answer: { async: boolean; value: Promise<string> | string } = Reflect.apply(
      realAtomicsWaitAsync,
      Atomics,
      args,
    )
    void Promise.resolve(answer.value).then((settled) => {
      ended(wait, settled === "timed-out" ? "fired" : "ended")
    })
    return answer
  }) as typeof Atomics.waitAsync

  const realSpawn = Bun.spawn
  Bun.spawn = ((...args: unknown[]) => {
    const { command, timeout } = startOf(args)
    const wait = made("Bun.spawn", timeout, command)
    const child: Subprocess = Reflect.apply(realSpawn, Bun, args)
    void child.exited.then(() => {
      ended(wait, timeout > 0 && child.signalCode !== null ? "fired" : "ended")
    })
    return child
  }) as typeof Bun.spawn
  const realSpawnSync = Bun.spawnSync
  Bun.spawnSync = ((...args: unknown[]) => {
    const { command, timeout } = startOf(args)
    const wait = made("Bun.spawnSync", timeout, command)
    const result: SyncSubprocess = Reflect.apply(realSpawnSync, Bun, args)
    ended(wait, result.exitedDueToTimeout === true ? "fired" : "ended")
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
      ended(made("Bun.$", -1, text.trim().split(/\s+/)[0] ?? "unknown"), "ended")
      return Reflect.apply(target, thisArg, args)
    },
  })

  process.on("exit", () => {
    const cpu = process.cpuUsage()
    const census: Census = {
      waits,
      ...counts,
      exitedAt: now(),
      cpuMs: Math.round((cpu.user + cpu.system) / 1000),
    }
    writeFileSync(record, JSON.stringify(census))
  })
}
