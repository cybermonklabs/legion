import { onTestFinished } from "bun:test";

/**
 * withinRow(promise) is `promise` as the row that calls it sees it: it settles as `promise` does
 * while the row runs, and never once the row has ended. Call it from a row's body, never from a
 * hook or a module's scope: it registers the row's `onTestFinished`.
 *
 * A row that `bun test`'s --timeout ends is not stopped: Bun 1.4.4 marks it failed, kills the
 * processes it started and goes on to the next row, and the row's body still waits at its
 * `await`. The killed child's exit then resolves that wait, and what the body asserts on it throws
 * where no row runs, as "Unhandled error between tests", or inside a later row, which then fails
 * for a cause not its own. Measured on Bun 1.4.4: the row's `onTestFinished` runs when the guard
 * ends the row, before the killed child's exit is read, so a wait that stops settling there leaves
 * the body at its `await` for good and no line after it runs.
 */
export function withinRow<T>(promise: Promise<T>): Promise<T> {
  let ended = false;
  onTestFinished(() => {
    ended = true;
  });
  const { promise: seen, resolve, reject } = Promise.withResolvers<T>();
  promise.then(
    (value) => {
      if (!ended) resolve(value);
    },
    (error: unknown) => {
      if (!ended) reject(error);
    }
  );
  return seen;
}
