// The rows `act-warnings.test.ts` runs in a child `bun test` of its own to prove that `setup.ts`
// puts back a spy of `quietConsoleError` that a row leaves in place, by each of its two restores:
// the `afterEach` beside its clock restore, and the `beforeEach` of its act rule. The file's name
// matches no test pattern, so the suite never runs it. Three rows fail on purpose, with the errors
// `act-warnings.test.ts` expects; a hook of this file fails only when a restore is missing, in any
// row order, with an error that says which.
import { afterAll, afterEach, describe, test } from "bun:test";

import { quietConsoleError } from "./act-warnings";

/** The rows that started with `console.error` still a spy, which a Bun mock carries `mock` for. */
const startedQuiet: string[] = [];

/**
 * A row that first notes whether `console.error` is still a spy, then runs `body`. A `beforeEach`
 * could not say which row it ran for (Bun 1.4.4 has no `expect.getState`), and nothing of this
 * file runs between the preload's `beforeEach` and this note, so it reads what the preload left.
 */
function row(name: string, body: () => void): void {
  test(name, () => {
    if ("mock" in console.error) startedQuiet.push(name);
    body();
  });
}

// The preload's `afterEach` restore runs after the row's own `afterEach` hooks, failed row or not;
// without it the spy would last into this `afterAll`, which Bun runs before any next row.
describe("a quiet row that throws", () => {
  afterAll(() => {
    if ("mock" in console.error) {
      throw new Error("console.error was still quiet after the row: no afterEach put it back");
    }
  });
  row("fails before it restores console.error", () => {
    quietConsoleError();
    throw new Error("the row failed with console.error quiet");
  });
});

// This `afterEach` runs before the preload's and throws, so Bun skips every `afterEach` of the
// preload for these rows, the restore among them, and only the preload's `beforeEach` puts the spy
// back before the next row. The rows are two so that, in any order, the first to run has a row
// after it.
describe("quiet rows whose afterEach throws", () => {
  afterEach(() => {
    throw new Error("the row's afterEach failed, which skips the preload's afterEach hooks");
  });
  row("one leaves console.error quiet past its end", () => {
    quietConsoleError();
  });
  row("another leaves console.error quiet past its end", () => {
    quietConsoleError();
  });
});

row("a row that leaves console.error alone passes", () => {});

afterAll(() => {
  if (startedQuiet.length > 0) {
    const rows = startedQuiet.join("; ");
    throw new Error(`rows started quiet, so no beforeEach put the spy back: ${rows}`);
  }
});
