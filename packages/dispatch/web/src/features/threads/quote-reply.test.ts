import { expect, test } from "bun:test";
import { hashKey } from "@tanstack/react-query";

import {
  ownWords,
  quoteBlocks,
  quoteInto,
  quoteWaiting,
  takeQuote,
  withQuote,
} from "./quote-reply";

test("a one-block selection is one quote line, a blank line after it, and the cursor under that", () => {
  const quoted = withQuote("", ["go with B per the train"]);
  expect(quoted).toEqual({
    caret: "> go with B per the train\n\n".length,
    text: "> go with B per the train\n\n",
  });
});

test("each block of a selection is its own quoted block, a bare `>` between two", () => {
  const blocks = ["The R3 audit decides it.", "**Recommended: ship it as is.**", "Ship as is"];
  expect(quoteBlocks(blocks)).toBe(
    "> The R3 audit decides it.\n>\n> **Recommended: ship it as is.**\n>\n> Ship as is"
  );
  const quoted = withQuote("", blocks);
  expect(quoted?.text).toBe(
    "> The R3 audit decides it.\n>\n> **Recommended: ship it as is.**\n>\n> Ship as is\n\n"
  );
  expect(quoted?.caret).toBe(quoted?.text.length);
});

test("every line of a block is quoted: a code block's empty line is a bare `>`, a nested quote two marks", () => {
  expect(quoteBlocks(["```ts\nconst a = 1;\n\nconst b = 2;\n```", "> said before", "- one"])).toBe(
    "> ```ts\n> const a = 1;\n>\n> const b = 2;\n> ```\n>\n> > said before\n>\n> - one"
  );
});

test("a quote goes at the end of what the box holds, after a blank line", () => {
  expect(withQuote("Ship it.", ["It writes nothing"])?.text).toBe(
    "Ship it.\n\n> It writes nothing\n\n"
  );
  // The box's own trailing lines and spaces are the one blank line the quote stands after.
  expect(withQuote("Ship it.\n\n\n  ", ["It writes nothing"])?.text).toBe(
    "Ship it.\n\n> It writes nothing\n\n"
  );
  // A box holding only blank space is an empty one.
  expect(withQuote(" \n", ["It writes nothing"])?.text).toBe("> It writes nothing\n\n");
});

test("a second quote follows the first, and the cursor is under the newest", () => {
  const first = withQuote("", ["the ledger moves first"]);
  const second = withQuote(`${first?.text}Agreed.`, ["the mirror replays"]);
  expect(second?.text).toBe("> the ledger moves first\n\nAgreed.\n\n> the mirror replays\n\n");
  expect(second?.caret).toBe(second?.text.length);
  expect(second?.text.slice(0, second.caret).endsWith("> the mirror replays\n\n")).toBe(true);
});

test("a selection of no words quotes nothing", () => {
  expect(quoteBlocks([])).toBe("");
  expect(quoteBlocks(["  ", ""])).toBe("");
  expect(withQuote("Ship it.", [" "])).toBeNull();
});

test("the reader's own words are the lines that are not a quote's", () => {
  const box = "Flip. One thing I can't read here:\n\n> go with B per the train?\n>\n> Keep\n\n";
  expect(ownWords(box).trim()).toBe("Flip. One thing I can't read here:");
  // A quote alone holds none of the reader's words, whatever it asks.
  expect(ownWords("> Should it ship?\n\n").trim()).toBe("");
  // Markdown reads a quote after up to three spaces; four make it the reader's own code.
  expect(ownWords("   > quoted\n    > typed").trim()).toBe("> typed");
  // A `>` inside a line the reader typed is theirs.
  expect(ownWords("5 > 3, so yes").trim()).toBe("5 > 3, so yes");
});

// A quote waits for its box by the name of the box's send, which rows share: a thread's send is
// named by the thread's id. The row hands a quote to a box that never mounts, so the quote is
// left waiting, as a row that ends before its box takes the quote leaves it. A store the preload
// does not empty after each row keeps it, and the row run again (`--rerun-each`), or a later row
// whose box of that name mounts, finds a quote nobody handed it.
test("a box takes only the quote handed to it, from a store the row starts empty", () => {
  const send = ["thread-send", "thread-unmounted"];
  const name = hashKey(send);
  expect(quoteWaiting(name), "a quote waited before any was handed").toBe(false);
  quoteInto(send, ["the ledger moves first"]);
  expect(quoteWaiting(name), "the handed quote is not waiting").toBe(true);
  // A box of another name takes nothing of it.
  expect(takeQuote(hashKey(["thread-send", "thread-other"]))).toBeUndefined();
});
