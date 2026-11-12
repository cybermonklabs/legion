import { expect, test } from "bun:test";

import { expectNoneOf } from "../none-of";

/** The message Playwright's `expect` puts on the error a failed clause throws, with the control
 *  characters its reporter colours the diff with dropped. */
function refusal(clause: () => void): string {
  try {
    clause();
  } catch (error) {
    // biome-ignore lint/suspicious/noControlCharactersInRegex: the ANSI escapes of the diff
    return (error as Error).message.replace(/\u001B\[[\d;]*m/g, "");
  }
  throw new Error("the clause passed, and this case is about how it fails");
}

const diagrams = [
  { drawn: true, near: true },
  { drawn: true, near: true },
];

test("a clause over the items it is about passes, and names nothing", () => {
  expect(() =>
    expectNoneOf(diagrams, {
      amongAtLeast: 2,
      are: (diagram) => !diagram.drawn,
      matched: "diagrams the margin reached and left undrawn",
      measured: "diagrams within a viewport height of the reader",
    })
  ).not.toThrow();
});

// The whole of this helper: a row that measured nothing fails, where the same clause written as
// `expect(nothing.filter(…)).toEqual([])` is green.
test("a clause that measured nothing fails, naming the count it needed and what it is about", () => {
  const message = refusal(() =>
    expectNoneOf([], {
      amongAtLeast: 1,
      are: (diagram: { drawn: boolean }) => !diagram.drawn,
      matched: "diagrams the margin reached and left undrawn",
      measured: "diagrams within a viewport height of the reader",
    })
  );
  expect(message).toContain("only 0 diagrams within a viewport height of the reader");
  expect(message).toContain("under the 1 this clause needs");
  expect(message).toContain('"no diagrams the margin reached and left undrawn"');
  expect(message).toContain("met by an empty set");
});

test("a row that measured some but under the count it needs fails with both numbers", () => {
  const message = refusal(() =>
    expectNoneOf(diagrams, {
      amongAtLeast: 5,
      are: (diagram) => !diagram.drawn,
      matched: "diagrams the margin reached and left undrawn",
      measured: "diagrams within a viewport height of the reader",
    })
  );
  expect(message).toContain("only 2 diagrams");
  expect(message).toContain("under the 5 this clause needs");
});

test("an item that matches fails, with how many of how many matched and what they are", () => {
  const message = refusal(() =>
    expectNoneOf([...diagrams, { drawn: false, near: true }], {
      amongAtLeast: 1,
      are: (diagram) => !diagram.drawn,
      matched: "diagrams the margin reached and left undrawn",
      measured: "diagrams within a viewport height of the reader",
    })
  );
  expect(message).toContain(
    "1 of 3 diagrams within a viewport height of the reader: diagrams the margin reached and " +
      "left undrawn"
  );
});

test("the index is the item's own, so a clause can read each against what it was", () => {
  const was = [10, 20, 30];
  const now = [10, 20, 31];
  const message = refusal(() =>
    expectNoneOf(now, {
      amongAtLeast: 3,
      are: (top, at) => top !== was[at],
      matched: "cards the mark clearing moved",
      measured: "cards of the conversation",
    })
  );
  expect(message).toContain("1 of 3 cards of the conversation: cards the mark clearing moved");
});

// A clause that needs none of its items is the empty measurement this helper exists to refuse,
// so it is refused at the call rather than passing over nothing.
test("a clause that asks for no items at all is refused", () => {
  expect(() =>
    expectNoneOf([], {
      amongAtLeast: 0,
      are: () => false,
      matched: "cards the mark clearing moved",
      measured: "cards of the conversation",
    })
  ).toThrow(/asks for at least 0 of them/);
});
