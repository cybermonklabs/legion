import { afterEach, beforeAll, beforeEach, jest } from "bun:test";
import { markRow } from "./deadline";

// The row-deadline preload. Every `bun test` command in this repository loads it
// (`--preload @legion/test-fixtures/row-deadline`), and each wait of `@legion/test-fixtures/wait`
// ends at the newest mark it took plus the command's `--timeout`, less a margin.
//
// Bun's `--timeout` arms a clock of its own for each row and for each hook. Measured on Bun 1.4.4,
// a preload's hooks run where a mark is wanted: its `beforeEach` before each row's own
// `beforeEach`, its `afterEach` after each row's own `afterEach` hooks and so right before a
// `describe`'s `beforeAll` that follows the row, and its `beforeAll` before the first file of the
// process, which under `--isolate` (and so `--parallel`) is each file. An `afterEach` that the
// preload's `beforeEach` registers belongs to that row alone, and runs right after the row's body
// and before every `afterEach` of the row's file and of its describes, whether the row passed,
// failed or its guard ended it; measured on Bun 1.4.4, with and without `--isolate` and under
// `--parallel`, where an `onTestFinished` registered there ran only after all of them. So the
// first of a row's own `afterEach` hooks counts from its own start, not from the row's. A hook a
// mark does not directly precede (a row's second `afterEach`, an `afterAll`, the `beforeAll` of
// the second file of a run without `--isolate`) counts from an earlier mark, so its waits end
// earlier than its own clock would allow, never later.
//
// Under `jest.useFakeTimers()` no mark is taken: `performance.now()` then reads the fake clock,
// which starts at 0 and moves only as the test advances it (measured on Bun 1.4.4), so a mark
// taken then could lie anywhere before or after Bun's own; the last real mark stays.
//
// Bun refuses a hook registered inside a concurrent row (`describe.concurrent`, `test.concurrent`,
// `--concurrent`): "Cannot call afterEach() here. It cannot be called inside a concurrent test."
// Measured on Bun 1.4.4, the refusal is thrown at the call, and caught it fails nothing. Such a
// row runs beside its siblings, so no mark of its own could tell its body's end from theirs: it
// keeps the mark its `beforeEach` took, and its first `afterEach` counts from the row's start. A
// mark is one per process, so a concurrent sibling that starts later moves it on: a wait of the
// earlier row can then end after that row's guard, which Bun's own timeout then reports in its
// place.

function mark(): void {
  if (!jest.isFakeTimers()) markRow();
}

/**
 * Whether error is Bun's refusal of a hook registered inside a concurrent row, read by Bun 1.4.4's
 * wording of it. A Bun that words it otherwise has its refusal thrown again, so every concurrent
 * row fails loudly rather than running unmarked.
 */
function concurrentRefusal(error: unknown): boolean {
  return error instanceof Error && error.message.includes("inside a concurrent test");
}

beforeAll(mark);
beforeEach(() => {
  mark();
  try {
    afterEach(mark);
  } catch (error) {
    if (!concurrentRefusal(error)) throw error;
  }
});
afterEach(mark);
