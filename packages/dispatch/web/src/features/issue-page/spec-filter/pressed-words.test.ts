import { expect, test } from "bun:test";

import type { CaretAt } from "../node-caret";
import { pressInWords } from "./pressed-words";

// A press on a statement's words is read as the count of the characters its editor holds before
// it, and the word pressed with its neighbours, from the words the view drew: each inline kind the
// view draws in place of what the editor holds counts as the editor holds it. Each case reads the
// count and the word whole, so a count off by the kind's characters fails here even where the
// editor would find the word pressed again (`node-caret.ts`'s `placeAt`).

/**
 * Draws `html` as a statement's drawn words and reads a press `into` characters into its text
 * `pressed`, at the point (1, 2), where the browser's `caretPositionFromPoint` finds that place.
 */
function pressIn(html: string, pressed: string, into: number): CaretAt {
  const words = document.createElement("div");
  words.setAttribute("data-node-words", "");
  words.innerHTML = html;
  document.body.append(words);
  const walker = document.createTreeWalker(words, NodeFilter.SHOW_TEXT);
  let text = walker.nextNode();
  while (text !== null && text.textContent !== pressed) text = walker.nextNode();
  if (text === null) throw new Error(`the words draw no text "${pressed}"`);
  const original = Object.getOwnPropertyDescriptor(document, "caretPositionFromPoint");
  Object.defineProperty(document, "caretPositionFromPoint", {
    configurable: true,
    value: (x: number, y: number) =>
      x === 1 && y === 2 ? { offsetNode: text, offset: into } : null,
  });
  try {
    return pressInWords(words, 1, 2);
  } finally {
    if (original === undefined) Reflect.deleteProperty(document, "caretPositionFromPoint");
    else Object.defineProperty(document, "caretPositionFromPoint", original);
    words.remove();
  }
}

const AFTER = " first, then the rest.";
/** Words with inline html in them, and words with a hard break in them. */
const KEPT = `<p>Kept by <span data-type="html">&lt;name&gt;</span>${AFTER}</p>`;
const BROKEN = "<p>Line one.<br>Line two, then the rest.</p>";

test("a press after a reference counts it as its text wrote it, once for all its words", () => {
  const cited = "dispatch://CORE-2";
  const html = `<p>Read <a data-row-reference="${cited}" data-row-written="${cited}" href="/CORE-2"><span>CORE-2</span> The other issue</a>${AFTER}</p>`;
  expect(pressIn(html, AFTER, 3)).toEqual({
    x: 1,
    y: 2,
    offset: "Read ".length + cited.length + 3,
    word: [`${cited} `, "first,", " then", 2],
  });
});

test("a press after a mention counts it as its text wrote it", () => {
  const cited = "dispatch://CORE-1/spec#b-r2";
  const html = `<p>Read <a data-row-chip="" data-row-reference="${cited}" data-row-written="${cited}" href="/CORE-1/spec#b-r2">The other statement</a>${AFTER}</p>`;
  expect(pressIn(html, AFTER, 3)).toEqual({
    x: 1,
    y: 2,
    offset: "Read ".length + cited.length + 3,
    word: [`${cited} `, "first,", " then", 2],
  });
});

test("a press after inline html counts the html as nothing", () => {
  expect(pressIn(KEPT, AFTER, 3)).toEqual({
    x: 1,
    y: 2,
    offset: "Kept by ".length + 3,
    word: ["by  ", "first,", " then", 2],
  });
});

test("a press after a footnote's reference counts the reference as nothing", () => {
  const html = `<p>Read the note<sup data-type="footnote_reference">1</sup>${AFTER}</p>`;
  expect(pressIn(html, AFTER, 3)).toEqual({
    x: 1,
    y: 2,
    offset: "Read the note".length + 3,
    word: ["note ", "first,", " then", 2],
  });
});

test("a press after a picture counts the picture as nothing", () => {
  const html = `<p>See <a data-dispatch-picture="dispatch://CORE-1/artifact/shot-png" data-picture-caption="shot.png" href="/CORE-1/artifact/shot-png"><span>shot.png</span></a>${AFTER}</p>`;
  expect(pressIn(html, AFTER, 3)).toEqual({
    x: 1,
    y: 2,
    offset: "See ".length + 3,
    word: ["See  ", "first,", " then", 2],
  });
});

// A hard break holds no text in the view or the editor, so the words on each side of it read as
// one there, as they do in the editor's text.
test("a press after a hard break counts the break as nothing", () => {
  expect(pressIn(BROKEN, "Line two, then the rest.", 6)).toEqual({
    x: 1,
    y: 2,
    offset: "Line one.".length + 6,
    word: ["one.Line ", "two,", " then", 1],
  });
});

// The count at the start of the text after a break or an atom is also the end of the text before
// it, so the press says which of the two it was at.
test("a press at the start of the text after a hard break or an atom says the caret goes into that text", () => {
  expect(pressIn(BROKEN, "Line two, then the rest.", 0)).toEqual({
    x: 1,
    y: 2,
    offset: "Line one.".length,
    word: ["Line ", "one.Line", " two,", 4],
    next: true,
  });
  expect(pressIn(KEPT, AFTER, 0)).toEqual({ x: 1, y: 2, offset: "Kept by ".length, next: true });
});

test("a press at the end of the text before a break, inside an atom or at the words' start goes into no next text", () => {
  const end = pressIn(BROKEN, "Line one.", "Line one.".length);
  expect(end).toEqual({ x: 1, y: 2, offset: 9, word: ["Line ", "one.Line", " two,", 4] });
  expect(end.next).toBeUndefined();
  const atom = pressIn(KEPT, "<name>", 2);
  expect(atom).toEqual({ x: 1, y: 2, offset: "Kept by ".length });
  expect(atom.next).toBeUndefined();
  const start = pressIn(BROKEN, "Line one.", 0);
  expect(start).toEqual({ x: 1, y: 2, offset: 0, word: ["", "Line", " one.Line", 0] });
  expect(start.next).toBeUndefined();
});
