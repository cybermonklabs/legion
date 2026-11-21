import { expect, test } from "bun:test";

import { issueAsk } from "./inbox-fixture";
import { itemAfter, type PlacedRow } from "./pane-selection";

/** The rows `ids`, in that order, as the list draws them. */
function drawn(...ids: string[]): PlacedRow[] {
  return ids.map((id) => ({
    group: "none",
    row: { ask: issueAsk({ id }), kind: "ask" },
    section: "human",
  }));
}

test("the item after an action is the row after the one acted on, else the row before it", () => {
  const list = drawn("a", "b", "c");
  expect(itemAfter(list, new Set(["a"]))).toBe("b");
  expect(itemAfter(list, new Set(["b"]))).toBe("c");
  // The last row: the one before it.
  expect(itemAfter(list, new Set(["c"]))).toBe("b");
  // The only row: none, which is the list's empty message.
  expect(itemAfter(drawn("a"), new Set(["a"]))).toBeUndefined();
});

test("after rows acted on together it is the row after the last of them, else the nearest before it not acted on", () => {
  const list = drawn("a", "b", "c", "d", "e");
  expect(itemAfter(list, new Set(["b", "c"]))).toBe("d");
  expect(itemAfter(list, new Set(["d", "e"]))).toBe("c");
  // Rows apart: the row after the last of them, and where there is none, the nearest row before
  // the last that was not acted on, never a row between them that the action took too.
  expect(itemAfter(list, new Set(["a", "c"]))).toBe("d");
  expect(itemAfter(list, new Set(["b", "e"]))).toBe("d");
  expect(itemAfter(list, new Set(["a", "b", "c", "d", "e"]))).toBeUndefined();
});

test("rows the list does not draw - a folded band's - are no place to go on from or to", () => {
  // The acted row is not among the drawn ones: there is no place it stood.
  expect(itemAfter(drawn("a", "b"), new Set(["z"]))).toBeUndefined();
  // A row acted on along with others the list does not draw is read where it stands.
  expect(itemAfter(drawn("a", "b", "c"), new Set(["b", "z"]))).toBe("c");
});
