import { spyOn } from "bun:test";

// A product's own bound, held: the code under test arms its deadlines with `AbortSignal.timeout`,
// and a row that holds those of one length decides itself when each ends. So the row reads what
// the code did up to the bound and at it, in that order, and no verdict reads a clock: a bound
// the code did not arm at that length is never held, and a row that waits for it to be armed
// fails at its deadline naming the wait.

/** The deadlines {@link holdDeadlines} holds, in the order the code armed them. */
export interface HeldDeadlines {
  /** One controller per deadline held, in the order armed. */
  readonly held: readonly AbortController[];
  /** Ends every deadline held so far, with the reason `AbortSignal.timeout` ends one with; one of
   *  the held length armed after it is held too. */
  end(): void;
  /** Ends every deadline held so far, as `end` does, and holds no more: one of the held length
   *  armed after it is the real one, so the code's later bounds of that length (a later call's
   *  share, say) run as they would. */
  letGo(): void;
  /** Puts the real `AbortSignal.timeout` back; a row's `afterEach(() => mock.restore())` does
   *  the same. */
  restore(): void;
}

/**
 * Holds every deadline of `ms` the code arms with `AbortSignal.timeout` on a signal the row ends
 * itself ({@link HeldDeadlines.end}); a deadline of any other length, or one armed once the row
 * let the hold go ({@link HeldDeadlines.letGo}), is the real one.
 */
export function holdDeadlines(ms: number): HeldDeadlines {
  const real = AbortSignal.timeout.bind(AbortSignal);
  const held: AbortController[] = [];
  let holding = true;
  const spy = spyOn(AbortSignal, "timeout").mockImplementation((delay: number) => {
    if (delay !== ms || !holding) return real(delay);
    const controller = new AbortController();
    held.push(controller);
    return controller.signal;
  });
  const end = (): void => {
    for (const controller of held) {
      if (!controller.signal.aborted) {
        controller.abort(new DOMException("The operation timed out.", "TimeoutError"));
      }
    }
  };
  return {
    held,
    end,
    letGo: () => {
      holding = false;
      end();
    },
    restore: () => spy.mockRestore(),
  };
}
