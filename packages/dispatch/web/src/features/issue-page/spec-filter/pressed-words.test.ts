import { expect, spyOn, test } from "bun:test";

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
  return pressAt(html, (words) => {
    const walker = document.createTreeWalker(words, NodeFilter.SHOW_TEXT);
    let text = walker.nextNode();
    while (text !== null && text.textContent !== pressed) text = walker.nextNode();
    if (text === null) throw new Error(`the words draw no text "${pressed}"`);
    return [text, into];
  });
}

/** The end of the words' paragraph, past the last thing it draws: where a press past the end of
 *  its line falls, as the browser finds the place. */
function paragraphEnd(words: HTMLElement): readonly [Node, number] {
  const paragraph = words.firstElementChild ?? words;
  return [paragraph, paragraph.childNodes.length];
}

/** Draws `html` as a statement's drawn words and reads a press at the point `at`, (1, 2) where
 *  none is given, where the browser's `caretPositionFromPoint` finds the place `place` reads in
 *  them. */
function pressAt(
  html: string,
  place: (words: HTMLElement) => readonly [Node, number],
  [atX, atY]: readonly [number, number] = [1, 2]
): CaretAt {
  const words = document.createElement("div");
  words.setAttribute("data-node-words", "");
  words.innerHTML = html;
  document.body.append(words);
  const original = Object.getOwnPropertyDescriptor(document, "caretPositionFromPoint");
  try {
    const [offsetNode, offset] = place(words);
    Object.defineProperty(document, "caretPositionFromPoint", {
      configurable: true,
      value: (x: number, y: number) => (x === atX && y === atY ? { offsetNode, offset } : null),
    });
    return pressInWords(words, atX, atY);
  } finally {
    if (original === undefined) Reflect.deleteProperty(document, "caretPositionFromPoint");
    else Object.defineProperty(document, "caretPositionFromPoint", original);
    words.remove();
  }
}

/** What `run` answers while every element draws `boxes` as its client rects, in the order of the
 *  text, one box or several on a line, as the browser draws an atom in one box for each run of one
 *  direction. happy-dom lays nothing out and gives each element a box of no size at the page's
 *  corner, which the press at (1, 2) is past, so a case of a press inside an atom gives the atom a
 *  line that holds the press. */
function withLines<Answer>(boxes: readonly DOMRect[], run: () => Answer): Answer {
  const lines = spyOn(HTMLElement.prototype, "getClientRects").mockImplementation(
    () => boxes as unknown as DOMRectList
  );
  try {
    return run();
  } finally {
    lines.mockRestore();
  }
}

/** A line that holds the press at (1, 2). */
const AROUND_PRESS = new DOMRect(-10, -8, 60, 20);

const AFTER = " first, then the rest.";
/** Inline html the view draws as an atom: the editor holds no text of it. */
const HTML_ATOM = '<span data-type="html">&lt;name&gt;</span>';
/** Words with inline html in them, and words with a hard break in them. */
const KEPT = `<p>Kept by ${HTML_ATOM}${AFTER}</p>`;
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
  const atom = withLines([AROUND_PRESS], () => pressIn(KEPT, "<name>", 2));
  expect(atom).toEqual({ x: 1, y: 2, offset: "Kept by ".length });
  expect(atom.next).toBeUndefined();
  const start = pressIn(BROKEN, "Line one.", 0);
  expect(start).toEqual({ x: 1, y: 2, offset: 0, word: ["", "Line", " one.Line", 0] });
  expect(start.next).toBeUndefined();
});

const PICTURE = `data-dispatch-picture="dispatch://CORE-1/artifact/shot-png" data-picture-caption="shot.png" href="/CORE-1/artifact/shot-png"`;
/**
 * Each atom the view draws, with a place inside it a press can fall at: inline html, a footnote's
 * reference, a picture by its caption's link, and a picture that has loaded, which draws no text.
 */
const ATOMS: readonly (readonly [
  atom: string,
  inside: (atom: Element) => readonly [Node, number],
])[] = [
  [HTML_ATOM, (atom) => [atom.firstChild ?? atom, 2]],
  ['<sup data-type="footnote_reference">1</sup>', (atom) => [atom.firstChild ?? atom, 0]],
  [`<a ${PICTURE}><span>shot.png</span></a>`, (atom) => [atom.firstChild?.firstChild ?? atom, 3]],
  [`<a ${PICTURE}><img alt="shot.png"></a>`, (atom) => [atom, 1]],
];
const NOTE = "Read the note";
/** The press at the end of `NOTE`, before an atom after it. */
const AT_NOTE_END: CaretAt = { x: 1, y: 2, offset: NOTE.length, word: ["the ", "note", "", 4] };
/** `NOTE` and then inline HTML that ends the words, in a paragraph that runs `direction`. */
function noteAndHtml(direction?: "ltr" | "rtl"): string {
  const style = direction === undefined ? "" : ` style="direction: ${direction}"`;
  return `<p${style}>${NOTE}${HTML_ATOM}</p>`;
}

/** A press at `at`, (1, 2) where none is given, that the browser answers `offset` characters into
 *  the text of the last atom `paragraph` draws. */
function pressInLastAtom(
  paragraph: string,
  offset: number,
  at?: readonly [number, number]
): CaretAt {
  return pressAt(
    paragraph,
    (words) => {
      const atom = words.firstElementChild?.lastElementChild ?? words;
      return [atom.firstChild ?? atom, offset];
    },
    at
  );
}

// An atom that ends the words holds no text, so the count past it is also the end of the text
// before it, as at the start of the text after one: the press says it was past the atom, for the
// editor to put the caret after it, where the block ends. The editor holds a reference as text,
// so the count past one that ends the words is past it already, and says nothing more.
test("a press past an atom that ends the words says the caret goes after it", () => {
  for (const [atom] of ATOMS) {
    expect(pressAt(`<p>${NOTE}${atom}</p>`, paragraphEnd), atom).toEqual({
      ...AT_NOTE_END,
      next: true,
    });
  }
  const cited = "dispatch://CORE-2";
  const reference = `<a data-row-reference="${cited}" data-row-written="${cited}" href="/CORE-2"><span>CORE-2</span> The other issue</a>`;
  const past = pressAt(`<p>Read ${reference}</p>`, paragraphEnd);
  expect(past).toEqual({
    x: 1,
    y: 2,
    offset: "Read ".length + cited.length,
    word: ["Read ", cited, "", cited.length],
  });
  expect(past.next).toBeUndefined();
});

test("a press inside an atom that ends the words goes before it", () => {
  for (const [atom, inside] of ATOMS) {
    const at = withLines([AROUND_PRESS], () =>
      pressAt(`<p>${NOTE}${atom}</p>`, (words) =>
        inside(words.firstElementChild?.lastElementChild ?? words)
      )
    );
    expect(at, atom).toEqual(AT_NOTE_END);
    expect(at.next, atom).toBeUndefined();
  }
});

// The browser can answer a point past the end of an atom's line with a place inside the atom:
// Chromium answers a press 1 to 100 px past inline HTML that ends a line with the end of the
// HTML's text. Such a place inside an atom the editor holds no text of is read by the atom's last
// line: a point past its end, on that line, goes after the atom, as a press past the end of the
// words does, or at the start of the text after it where text follows on the next line. A point on
// that line's end, inside the line or on a line of the atom before its last stays inside the atom,
// at its start. The end is the line's right edge where the atom runs left to right, and its left
// edge where it runs right to left. A reference is text the editor holds, so a place inside one
// stays there, and neither that place nor one in the text after an atom reads a line of the atom.
// happy-dom lays nothing out, so each case gives the atom's lines, placed from the press at
// (1, 2).
test("a press the browser answers inside an atom, past the end of its last line, says the caret goes after it", () => {
  const after: CaretAt = { ...AT_NOTE_END, next: true };
  // A press the browser answers at the end of the text of the last atom `paragraph` draws, as
  // Chromium answers a point past inline HTML that ends a line.
  const pressPast = (paragraph: string) => pressInLastAtom(paragraph, "<name>".length);
  let line = new DOMRect();
  let earlier: DOMRect | undefined;
  const lines = spyOn(HTMLElement.prototype, "getClientRects").mockImplementation(
    () => (earlier === undefined ? [line] : [earlier, line]) as unknown as DOMRectList
  );
  try {
    for (const past of [1, 10, 40, 100]) {
      line = new DOMRect(1 - past - 60, -8, 60, 20);
      for (const [atom, inside] of ATOMS) {
        const at = pressAt(`<p>${NOTE}${atom}</p>`, (words) =>
          inside(words.firstElementChild?.lastElementChild ?? words)
        );
        expect(at, `${past} px past ${atom}`).toEqual(after);
      }
      expect(pressPast(noteAndHtml()), `${past} px past the end`).toEqual(after);
      expect(pressPast(KEPT), `${past} px past, before text`).toEqual({
        x: 1,
        y: 2,
        offset: "Kept by ".length,
        next: true,
      });
      line = new DOMRect(1 + past, -8, 60, 20);
      expect(pressPast(noteAndHtml("rtl")), `${past} px past, right to left`).toEqual(after);
    }
    // On the end of the atom's last line, inside the line, on the atom's line before its last,
    // and past the right edge of a line that runs right to left, whose end is its left edge.
    const stays = [
      [new DOMRect(-59, -8, 60, 20), "ltr", undefined],
      [new DOMRect(-30, -8, 60, 20), "ltr", undefined],
      [new DOMRect(-99, 20, 60, 20), "ltr", new DOMRect(-30, -8, 60, 20)],
      [new DOMRect(-60, -8, 60, 20), "rtl", undefined],
    ] as const;
    for (const [box, direction, before] of stays) {
      line = box;
      earlier = before;
      const at = pressPast(noteAndHtml(direction));
      const name = `${direction} line ${box.left} to ${box.right}, ${box.top} to ${box.bottom}`;
      expect(at, name).toEqual(AT_NOTE_END);
      expect(at.next, name).toBeUndefined();
    }
    earlier = undefined;
    // A point past the end of the line, which a place in a reference or in the text after an atom
    // reads nothing of.
    line = new DOMRect(-60, -8, 60, 20);
    lines.mockClear();
    const cited = "dispatch://CORE-2";
    const title = " The other issue";
    const reference = `<a data-row-reference="${cited}" data-row-written="${cited}" href="/CORE-2"><span>CORE-2</span>${title}</a>`;
    expect(pressIn(`<p>Read ${reference}</p>`, title, title.length)).toEqual({
      x: 1,
      y: 2,
      offset: "Read ".length,
      word: ["Read ", cited, "", 0],
    });
    expect(pressIn(KEPT, AFTER, 3)).toEqual({
      x: 1,
      y: 2,
      offset: "Kept by ".length + 3,
      word: ["by  ", "first,", " then", 2],
    });
    expect(lines, "a press off an uncounted atom laid the atom out").not.toHaveBeenCalled();
  } finally {
    lines.mockRestore();
  }
});

// Chromium answers a place inside an atom for every point past it on its line, whatever the
// point's height in that line, and an atom's box is as tall as its font, not as its line: a
// raised footnote reference stood at 1.4 to 14.4 px in a line of 0 to 24 px, and inline HTML at
// 147 to 165 px in a line of 144 to 168 px. A press past the atom under its box, or over it, is
// past it all the same, and the caret goes after the atom.
test("a press past a footnote reference that ends the words, below its raised box, says the caret goes after it", () => {
  const cases = [
    ['<sup data-type="footnote_reference">1</sup>', 1, new DOMRect(100, 1.4, 6, 13), 20],
    [HTML_ATOM, "<name>".length, new DOMRect(100, 147, 40, 18), 145],
  ] as const;
  for (const [atom, offset, drawn, y] of cases) {
    const x = drawn.right + 4;
    const at = withLines([drawn], () => pressInLastAtom(`<p>${NOTE}${atom}</p>`, offset, [x, y]));
    expect(at, atom).toEqual({ ...AT_NOTE_END, next: true, x, y });
  }
});

// An atom's box is as tall as its font, so the line before an atom's last runs on below that box
// by its half-leading: an atom drawn over two lines stood at 147 to 165 px in a line of 144 to
// 168 px, and at 171 to 189 px on the next. A press in that band, right of the last line's short
// end, is on the earlier line, so it stays inside the atom, at its start, and does not go after
// the atom, a line below the press. The two lines meet half way between the two boxes.
test("a press below an atom's box on the line before its last stays at the atom's start", () => {
  const boxes = [new DOMRect(-30, -18, 60, 18), new DOMRect(-99, 6, 60, 18)];
  const at = withLines(boxes, () => pressInLastAtom(noteAndHtml(), 2));
  expect(at).toEqual(AT_NOTE_END);
  expect(at.next).toBeUndefined();
});

// An atom the browser draws in two boxes on one line, as it draws one that a run of another
// direction splits, has no line before its last, so a press past the two boxes goes after it.
test("a press past an atom drawn in two boxes on one line says the caret goes after it", () => {
  const boxes = [new DOMRect(-90, -8, 30, 20), new DOMRect(-60, -8, 30, 20)];
  const at = withLines(boxes, () => pressInLastAtom(noteAndHtml(), "<name>".length));
  expect(at).toEqual({ ...AT_NOTE_END, next: true });
});

// Chromium draws an atom in one box for each run of one direction, so an atom whose last line
// holds right-to-left text draws that line in several boxes: inline HTML ending in Hebrew drew its
// last line as two. The box before the last then stands on the last line, and the line before it
// is the last box above that line. A press on that earlier line, right of the last line's end,
// stays inside the atom, at its start.
test("a press on the line before an atom's last, where the last line holds two boxes, stays at the atom's start", () => {
  const boxes = [
    new DOMRect(300, 147, 300, 18),
    new DOMRect(0, 171, 50, 18),
    new DOMRect(50, 171, 50, 18),
  ];
  const at = withLines(boxes, () => pressInLastAtom(noteAndHtml(), 2, [400, 150]));
  expect(at).toEqual({ ...AT_NOTE_END, x: 400, y: 150 });
  expect(at.next).toBeUndefined();
});

// Chromium gives the boxes of one line left to right on the screen, so the last box ends a line
// that runs left to right and starts one that runs right to left. The line ends at its greatest
// right where it runs left to right, and at its least left where it runs right to left, whichever
// box that is: a press past the last box, short of that end, is inside the line and stays at the
// atom's start. The left-to-right case draws boxes in an order Chromium does not, as a guard.
test("a press past an atom's last box, short of the end of its line, stays at the atom's start", () => {
  const cases = [
    ["ltr", [new DOMRect(-20, -8, 40, 20), new DOMRect(-60, -8, 40, 20)]],
    ["rtl", [new DOMRect(-20, -8, 40, 20), new DOMRect(20, -8, 40, 20)]],
  ] as const;
  for (const [direction, boxes] of cases) {
    const at = withLines(boxes, () => pressInLastAtom(noteAndHtml(direction), "<name>".length));
    expect(at, direction).toEqual(AT_NOTE_END);
    expect(at.next, direction).toBeUndefined();
  }
});

// A padded inline atom draws boxes taller than its line, so the boxes of two lines overlap: a
// picture's caption link, which `.prose a` pads to its line and which is no inline-block before its
// picture loads, drew 25 px boxes on 24 px lines in Chromium, at 791 to 816 px and 815 to 840 px.
// Which line a box is on is read from its middle, so a press past the last line's end goes after
// the atom, and a press on the line above, right of the last line's end, stays at its start.
test("a press beside a wrapped atom whose boxes overlap the line above reads the line it is on", () => {
  const boxes = [new DOMRect(149.75, 791, 99.8, 25), new DOMRect(20, 815, 74.42, 25)];
  const past = withLines(boxes, () =>
    pressInLastAtom(noteAndHtml(), "<name>".length, [124.42, 828])
  );
  expect(past, "past the last line's end").toEqual({
    ...AT_NOTE_END,
    next: true,
    x: 124.42,
    y: 828,
  });
  const above = withLines(boxes, () => pressInLastAtom(noteAndHtml(), 2, [278, 800]));
  expect(above, "on the line above").toEqual({ ...AT_NOTE_END, x: 278, y: 800 });
  expect(above.next, "on the line above").toBeUndefined();
});
