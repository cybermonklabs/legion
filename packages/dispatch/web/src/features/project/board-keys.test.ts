import { expect, test } from "bun:test";

import { announceMove, focusTarget, type KeyColumn, keyboardMove } from "./board-keys";

/** Open's Board by Status with two cards in Todo and one in In progress: Icebox and Done rails,
 *  and the empty columns between. */
const COLUMNS: readonly KeyColumn[] = [
  { id: "triage", rail: false, size: 0 },
  { id: "icebox", rail: true, size: 3 },
  { id: "backlog", rail: false, size: 0 },
  { id: "todo", rail: false, size: 2 },
  { id: "in_progress", rail: false, size: 1 },
  { id: "done", rail: true, size: 0 },
];

test("j and k rove within a column and clamp at both ends", () => {
  expect(focusTarget(COLUMNS, { column: "todo", index: 0 }, "j")).toEqual({
    column: "todo",
    index: 1,
  });
  expect(focusTarget(COLUMNS, { column: "todo", index: 1 }, "j")).toEqual({
    column: "todo",
    index: 1,
  });
  expect(focusTarget(COLUMNS, { column: "todo", index: 0 }, "k")).toEqual({
    column: "todo",
    index: 0,
  });
  // On a column itself, j and k stay there.
  expect(focusTarget(COLUMNS, { column: "backlog", index: null }, "j")).toEqual({
    column: "backlog",
    index: null,
  });
});

test("h and l step to the next column in the Board's order, rails included, clamping the index", () => {
  expect(focusTarget(COLUMNS, { column: "todo", index: 1 }, "l")).toEqual({
    column: "in_progress",
    index: 0,
  });
  expect(focusTarget(COLUMNS, { column: "todo", index: 1 }, "h")).toEqual({
    column: "backlog",
    index: null,
  });
  // A rail draws no card: the focus lands on the rail itself.
  expect(focusTarget(COLUMNS, { column: "triage", index: null }, "l")).toEqual({
    column: "icebox",
    index: null,
  });
  expect(focusTarget(COLUMNS, { column: "in_progress", index: 0 }, "l")).toEqual({
    column: "done",
    index: null,
  });
  // At the Board's ends the focus stays.
  expect(focusTarget(COLUMNS, { column: "done", index: null }, "l")).toEqual({
    column: "done",
    index: null,
  });
  expect(focusTarget(COLUMNS, { column: "triage", index: null }, "h")).toEqual({
    column: "triage",
    index: null,
  });
});

test("with nothing focused, j and l pick the first card and k and h the last, past rails", () => {
  expect(focusTarget(COLUMNS, null, "j")).toEqual({ column: "todo", index: 0 });
  expect(focusTarget(COLUMNS, null, "k")).toEqual({ column: "in_progress", index: 0 });
  const empty: KeyColumn[] = [
    { id: "queue", rail: false, size: 0 },
    { id: "none", rail: true, size: 4 },
  ];
  expect(focusTarget(empty, null, "l")).toEqual({ column: "queue", index: null });
  expect(focusTarget(empty, null, "h")).toEqual({ column: "none", index: null });
  expect(focusTarget([], null, "j")).toBeUndefined();
});

test("keyboardMove shifts a card one place within its column and stops at its ends", () => {
  expect(keyboardMove(COLUMNS, { column: "todo", index: 0 }, "down")).toEqual({
    column: "todo",
    index: 1,
  });
  expect(keyboardMove(COLUMNS, { column: "todo", index: 1 }, "down")).toBeUndefined();
  expect(keyboardMove(COLUMNS, { column: "todo", index: 1 }, "up")).toEqual({
    column: "todo",
    index: 0,
  });
  expect(keyboardMove(COLUMNS, { column: "todo", index: 0 }, "up")).toBeUndefined();
});

test("keyboardMove puts a card at the top of the next column, rails included, and stops at the Board's ends", () => {
  expect(keyboardMove(COLUMNS, { column: "todo", index: 1 }, "next")).toEqual({
    column: "in_progress",
    index: 0,
  });
  expect(keyboardMove(COLUMNS, { column: "in_progress", index: 0 }, "next")).toEqual({
    column: "done",
    index: 0,
  });
  expect(keyboardMove(COLUMNS, { column: "backlog", index: 0 }, "prev")).toEqual({
    column: "icebox",
    index: 0,
  });
  expect(keyboardMove(COLUMNS, { column: "done", index: 0 }, "next")).toBeUndefined();
  expect(keyboardMove(COLUMNS, { column: "triage", index: 0 }, "prev")).toBeUndefined();
});

test("announceMove names the card, its column and its place, or that Done closed it", () => {
  expect(announceMove("CORE-12", "todo", "Todo", { at: 2, of: 5 })).toBe(
    "CORE-12 → Todo, position 2 of 5"
  );
  expect(announceMove("CORE-12", "done", "Done", { at: 1, of: 1 })).toBe("CORE-12 → Done, closed");
  expect(announceMove("CORE-12", "done", "Done", undefined)).toBe("CORE-12 → Done, closed");
  // A card that left the view has no place in it.
  expect(announceMove("CORE-12", "backlog", "Backlog", undefined)).toBe("CORE-12 → Backlog");
});
