import { expect, test } from "bun:test";

import type { HandoffSummary } from "../../api/types";
import handoffsBody from "./fixtures/handoffs.json";
import { roundCounts } from "./round-counts";

// Ten rows of a handoffs list production answered (`fixtures/README.md`): both `plan` rows, three
// `implement`, three `test` and two `review`.
const HANDOFFS: readonly HandoffSummary[] = handoffsBody;

test("Planning, Implementing, Testing and Reviewing each count their phase's handoffs", () => {
  expect(new Set(HANDOFFS.map((handoff) => handoff.phase))).toEqual(
    new Set(["implement", "plan", "review", "test"])
  );
  expect(roundCounts(HANDOFFS)).toEqual({
    implementing: 3,
    planning: 2,
    reviewing: 2,
    testing: 3,
  });
});

test("a counted step with no handoff shows no count, and no other step ever counts one", () => {
  const [first] = HANDOFFS;
  if (first === undefined) throw new Error("the captured handoffs list is empty");
  const noReview = HANDOFFS.filter((handoff) => handoff.phase !== "review");
  // A handoff of a phase no step takes its rounds from counts for none.
  expect(roundCounts([...noReview, { ...first, phase: "design" }])).toEqual({
    implementing: 3,
    planning: 2,
    testing: 3,
  });
});

test("no handoffs read yet count no round", () => {
  expect(roundCounts(undefined)).toEqual({});
  expect(roundCounts([])).toEqual({});
});
