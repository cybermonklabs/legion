/**
 * A generic serial queue of tasks (`createQueue`, `TaskQueue`). It runs one task at a time, in the
 * order the tasks were queued, and gives each task a deadline timer that abandons the task when it
 * fires. This module imports nothing, and it knows nothing of the registration state, roles, the
 * listener or the hand-back ledger.
 *
 * The caller gives `createQueue` a handle builder, which makes each task's handle and refusal
 * record. The handle decides which steps it refuses, and refuses a step with {@link refuseStep}.
 * The queue reads only the outcome: a deadline that fired ({@link taskWasAbandoned}), or a step
 * the handle refused ({@link stepWasRefused}).
 */

/** A queued task abandoned at its deadline: the queue has moved on, and the task's own
 *  continuation writes nothing when its await finally resolves. Module-private, so
 *  {@link taskWasAbandoned} is the one way to ask: the extension reads the rejection without
 *  naming the class. */
class AbandonedTaskError extends Error {}

/** Whether `error` is a queued task's deadline rejection, which a caller reads as a stall rather
 *  than as a failure of the work the task was doing. */
export function taskWasAbandoned(error: unknown): boolean {
  return error instanceof AbandonedTaskError;
}

/**
 * A step the task's handle refused. Module-private: a handle throws it only through
 * {@link refuseStep}, and `run` turns it into the task's outcome. The queue swallows it on an
 * abandoned task's late settlement. Before the deadline, the task resolves `undefined`.
 */
class StepRefused extends Error {}

/** Refuses a step of a queued task: throws the error {@link stepWasRefused} recognizes, which ends
 *  the task. A task's handle calls it before the step acts. */
export function refuseStep(message: string): never {
  throw new StepRefused(message);
}

/** Whether `error` is a step the task's handle refused ({@link refuseStep}), which a caller reads
 *  as the end of the task rather than as a failure of the work the task was doing. */
export function stepWasRefused(error: unknown): boolean {
  return error instanceof StepRefused;
}

/** What the handle builder answers for one task: the handle the task acts through, and the record
 *  the queue gives `onRefused` when a refused step ended the task. */
export interface RunningTask<Handle, Refusal> {
  readonly handle: Handle;
  readonly refusal: Refusal;
}

/** A serial queue whose tasks act through a `Handle`, and leave a `Refusal` record behind when a
 *  refused step ends them. */
export interface TaskQueue<Handle, Refusal> {
  /**
   * Runs tasks one at a time, in the order they were queued; a task that fails does not stop the
   * ones after it. Each task gets `deadlineMs()` milliseconds, read when that task starts, after
   * which the caller's promise rejects with the deadline rejection ({@link taskWasAbandoned}),
   * the queue starts the next task at once, and the abandoned task's own settlement is swallowed.
   * A task that a refused step ended before its deadline ({@link stepWasRefused}) resolves
   * `undefined`, and `onRefused` gets the refusal record of that task.
   */
  readonly run: <T>(
    task: (handle: Handle) => Promise<T>,
    onRefused?: (refusal: Refusal) => void
  ) => Promise<T | undefined>;
  /** Resolves once every task queued so far has finished. */
  readonly settled: () => Promise<unknown>;
}

/**
 * One serial queue. `deadlineMs`: the deadline of each task, read when that task starts.
 * `buildHandle`: the handle builder, which the queue calls once per task, as the task starts. It
 * gets what the handle needs from the queue: `live`, which answers false from the moment the
 * task's deadline fires, and `deadlineAt`, that moment as a `performance.now()` instant. It
 * answers the task's handle and its refusal record.
 */
export const createQueue = <Handle, Refusal>(
  deadlineMs: () => number,
  buildHandle: (live: () => boolean, deadlineAt: number) => RunningTask<Handle, Refusal>
): TaskQueue<Handle, Refusal> => {
  let tail: Promise<unknown> = Promise.resolve();
  return {
    run<T>(
      task: (handle: Handle) => Promise<T>,
      onRefused?: (refusal: Refusal) => void
    ): Promise<T | undefined> {
      // `tail` never rejects (it is always `next.catch(…)`), so a failed task never stops the
      // next.
      const next = tail.then(
        () =>
          new Promise<T | undefined>((resolve, reject) => {
            let abandoned = false;
            const taskDeadlineMs = deadlineMs();
            const deadlineAt = performance.now() + taskDeadlineMs;
            const timer = setTimeout(() => {
              abandoned = true;
              reject(
                new AbandonedTaskError(
                  `a queued task did not settle within ${taskDeadlineMs}ms and was abandoned; the queue moved on`
                )
              );
            }, taskDeadlineMs);
            const running = buildHandle(() => !abandoned, deadlineAt);
            // The task runs inside this promise so a synchronous throw is its rejection too.
            void (async () => task(running.handle))().then(
              (value) => {
                // Clear first: a healthy task must not leave a pending timer holding the process
                // open for a whole deadline.
                clearTimeout(timer);
                if (!abandoned) resolve(value);
              },
              (error: unknown) => {
                clearTimeout(timer);
                // An abandoned task's settlement is swallowed: the caller's promise rejected at
                // the deadline, and an unhandled rejection is fatal to the host.
                if (abandoned) return;
                // The task's handle refused a step, which ended the task. That is not a failure
                // to report. The caller gets the task's refusal record, because what the task
                // left behind is not always nothing.
                if (error instanceof StepRefused) {
                  onRefused?.(running.refusal);
                  resolve(undefined);
                } else reject(error);
              }
            );
          })
      );
      tail = next.catch(() => undefined);
      return next;
    },
    settled: (): Promise<unknown> => tail,
  };
};
