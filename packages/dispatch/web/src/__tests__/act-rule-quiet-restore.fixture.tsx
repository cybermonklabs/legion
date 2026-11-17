// The rows `act-warnings.test.ts` runs in a child `bun test` of its own to prove that `setup.ts`
// puts back a spy of `quietConsoleError` that a row leaves in place, by each of its two restores:
// the `afterEach` beside its clock restore, after a row that throws and, since it runs before the
// act check, after a row whose act message makes that check throw; and the `beforeEach` of its act
// rule. The file's name matches no test pattern, so the suite never runs it. Four rows fail on
// purpose, with the errors `act-warnings.test.ts` expects; a hook of this file fails only when a
// restore is missing or runs after the act check, in any row order, with an error that says which.
import { afterAll, afterEach, describe, test } from "bun:test";
import { render } from "@testing-library/react";
import { useState } from "react";

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

/** The setter of the count `QuietActCount` shows, which its row calls with no event between. */
interface HeldCount {
  set: (count: number) => void;
}

function useHeldCount(held: HeldCount): string {
  const [count, setCount] = useState(0);
  held.set = setCount;
  return `count ${count}`;
}

function QuietActCount({ held }: { held: HeldCount }) {
  return <p>{useHeldCount(held)}</p>;
}

// This row's state set outside act draws an act message while `console.error` is quiet, so the
// preload's act check throws for the row, which skips any `afterEach` after that check. The restore
// beside the clock restore puts the spy back before this `afterAll`, which Bun runs after the row
// and before any next row, only because it runs before that check.
describe("a quiet row whose act check throws", () => {
  afterAll(() => {
    if ("mock" in console.error) {
      throw new Error(
        "console.error was still quiet after the act check failed its row: no restore ran before that check"
      );
    }
  });
  row("sets a state outside act while console.error is quiet", () => {
    const held: HeldCount = {
      set: () => {
        throw new Error("QuietActCount has not rendered");
      },
    };
    render(<QuietActCount held={held} />);
    quietConsoleError();
    held.set(1);
  });
});

row("a row that leaves console.error alone passes", () => {});

afterAll(() => {
  if (startedQuiet.length > 0) {
    const rows = startedQuiet.join("; ");
    throw new Error(`rows started quiet, so no beforeEach put the spy back: ${rows}`);
  }
});
