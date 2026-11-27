/** A thread's runtime that ended in failure starts again after this, doubled at each failure in a
 *  row up to `RETRY_MAX_MS`, so a failing start is not tried again at every look of the owner. */
const RETRY_FIRST_MS = 1_000
const RETRY_MAX_MS = 60_000

interface Failed {
  readonly generation: string
  readonly failures: number
  readonly retryAt: number
}

/** When each thread of a shared app server whose runtime ended in failure may start again. */
export class ThreadStartBackoff {
  readonly #failed = new Map<string, Failed>()
  readonly #now: () => number

  constructor(now: () => number = () => performance.now()) {
    this.#now = now
  }

  /** Forgets each thread that is no longer live at the load that failed: a new load, or its end,
   *  starts its count again. */
  forgetChanged(live: ReadonlyMap<string, { readonly generation: string }>): void {
    for (const [thread, failed] of this.#failed) {
      if (live.get(thread)?.generation !== failed.generation) this.#failed.delete(thread)
    }
  }

  /** Whether `thread`'s runtime may start now. */
  mayStart(thread: string): boolean {
    const failed = this.#failed.get(thread)
    return failed === undefined || this.#now() >= failed.retryAt
  }

  /** `thread`'s runtime ended without failing. */
  ended(thread: string): void {
    this.#failed.delete(thread)
  }

  /** `thread`'s runtime for load `generation`, started at `startedAt`, ended in failure: answers
   *  the wait before it starts again. One that ran longer than the longest wait failed afresh. */
  failed(thread: string, generation: string, startedAt: number): number {
    const now = this.#now()
    const before = this.#failed.get(thread)
    const failures =
      before === undefined || before.generation !== generation || now - startedAt > RETRY_MAX_MS
        ? 1
        : before.failures + 1
    const wait = Math.min(RETRY_FIRST_MS * 2 ** (failures - 1), RETRY_MAX_MS)
    this.#failed.set(thread, { failures, generation, retryAt: now + wait })
    return wait
  }
}
