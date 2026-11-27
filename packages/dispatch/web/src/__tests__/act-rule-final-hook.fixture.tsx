// The rows `act-warnings.test.ts` runs in a child `bun test` of its own to prove that the last
// `afterEach` of `setup.ts` runs the network report and the act check together, after every reset
// and restore above it: a row that both checks fail fails with one report naming both, and the row
// after it starts with nothing that row left. The file's name matches no test pattern, so the
// suite never runs it. Two rows fail on purpose, with the error `act-warnings.test.ts` expects; a
// hook of this file fails only when a row starts with something an earlier row left, in any row
// order, with an error that says which row and what it found.
import { afterAll, describe, jest, test } from "bun:test";
import { act, render } from "@testing-library/react";
import type { ComponentType } from "react";

import { relativeTimeLabel } from "../features/refs/relative-time";
import { resetTimestampTick, TICK_INTERVAL_MS, Timestamp } from "../features/refs/Timestamp";
import { quietConsoleError } from "./act-warnings";
import { type HeldCount, heldCount, useHeldCount } from "./held-count";

/** What each row found at its start that an earlier row left, by the row's name. */
const startedWith = new Map<string, string[]>();

/**
 * Whether a `<Timestamp>` mounted now ticks on a fake clock this installs: it shows a time 45 s
 * old, the clock moves past one tick inside `act`, and it must then show the moved clock's label.
 * The label is compared, not queried, so a tick that never came is recorded rather than thrown.
 * The tick it armed is called off and the clock put back, so what the row leaves is its own.
 */
function freshTimestampTicks(): boolean {
  jest.useFakeTimers();
  const at = new Date(Date.now() - 45_000);
  const view = render(<Timestamp at={at.toISOString()} />);
  try {
    act(() => {
      jest.advanceTimersByTime(TICK_INTERVAL_MS + 1);
    });
    return view.container.textContent === relativeTimeLabel(at, Date.now());
  } finally {
    view.unmount();
    resetTimestampTick();
    jest.useRealTimers();
  }
}

/**
 * What an earlier row left that the preload puts back after every row. None of it is read by
 * waiting on a clock, since a wait on a fake clock left installed never ends: Bun 1.4.4 holds
 * `setTimeout` and `Bun.sleep` alike on it and keeps `setTimeout` the same function, so a left
 * clock is read from `jest.isFakeTimers()`, and the `<Timestamp>` check moves its clock by hand.
 */
function leftBehind(): string[] {
  const found: string[] = [];
  if (jest.isFakeTimers()) found.push("a fake clock installed");
  if ("mock" in console.error) found.push("console.error a spy");
  if (!freshTimestampTicks()) found.push("a fresh <Timestamp> that did not tick");
  return found;
}

/**
 * A row that first notes what an earlier row left, then runs `body`. A `beforeEach` could not say
 * which row it ran for (Bun 1.4.4 has no `expect.getState`), and nothing of this file runs between
 * the preload's `beforeEach` and this note, so it reads what the preload left.
 */
function row(name: string, body: () => Promise<void> | void): void {
  test(name, async () => {
    const found = leftBehind();
    if (found.length > 0) startedWith.set(name, found);
    await body();
  });
}

function CountOneRowLeaves({ held }: { held: HeldCount }) {
  return <p>{useHeldCount(held)}</p>;
}

function CountAnotherRowLeaves({ held }: { held: HeldCount }) {
  return <p>{useHeldCount(held)}</p>;
}

/**
 * Leaves what both checks and every reset and restore before them are for: a request nothing
 * answers, a state set outside act while a spy of `quietConsoleError` it never restores keeps
 * `console.error` quiet, and a fake clock it never puts back, with a `<Timestamp>` tick armed
 * on it.
 */
async function leaveEverything(
  path: string,
  Count: ComponentType<{ held: HeldCount }>
): Promise<void> {
  await fetch(path);
  const held = heldCount();
  render(<Count held={held} />);
  quietConsoleError();
  held.set(1);
  jest.useFakeTimers();
  render(<Timestamp at={new Date().toISOString()} />);
}

// Both checks throw for these rows, so the preload's last `afterEach` fails each with one report
// naming both. The rows are two so that, in any order, one of them has a row after it.
describe("rows that leave everything the preload puts back", () => {
  row("one fails on its request and its act message in one report", () =>
    leaveEverything("/api/v1/nothing-answers/one", CountOneRowLeaves)
  );
  row("another fails on its request and its act message in one report", () =>
    leaveEverything("/api/v1/nothing-answers/another", CountAnotherRowLeaves)
  );
});

row("a row that leaves nothing passes", () => {});

afterAll(() => {
  if (startedWith.size > 0) {
    const rows = [...startedWith].map(([name, found]) => `${name} (${found.join(", ")})`);
    throw new Error(`rows started with what an earlier row left: ${rows.join("; ")}`);
  }
});
