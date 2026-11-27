import { expect, test } from "bun:test";

import { rangeOf } from "./selection-range";

/** Six rows as the list draws them: asks and threads alike, across two bands. */
const ORDER = ["ask-1", "reply:c1", "ask-2", "reply:c2", "agent:m1", "ask-3"];

test("a row clicked after the anchor runs from the anchor down to it", () => {
  expect(rangeOf(ORDER, "reply:c1", "agent:m1")).toEqual([
    "reply:c1",
    "ask-2",
    "reply:c2",
    "agent:m1",
  ]);
});

test("a row clicked before the anchor runs from it down to the anchor, in the list's order", () => {
  expect(rangeOf(ORDER, "agent:m1", "reply:c1")).toEqual([
    "reply:c1",
    "ask-2",
    "reply:c2",
    "agent:m1",
  ]);
});

test("the anchor clicked again is a run of that row alone", () => {
  expect(rangeOf(ORDER, "ask-2", "ask-2")).toEqual(["ask-2"]);
});

test("an anchor the order does not hold makes a run of the clicked row alone", () => {
  expect(rangeOf(ORDER, "reply:gone", "reply:c2")).toEqual(["reply:c2"]);
});
