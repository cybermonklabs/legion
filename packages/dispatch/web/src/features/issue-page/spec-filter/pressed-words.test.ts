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

/** What `run` answers while every element draws `boxes` as its client rects: one box or several
 *  on a line, each line's boxes left to right on the screen, as Chromium gives them. happy-dom
 *  lays nothing out and gives each element a box of no size at the page's corner, which the press
 *  at (1, 2) is past, so a case of a press inside an atom gives the atom a line that holds the
 *  press. */
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
const HTML_ATOM = '<span data-row-written="" data-type="html">&lt;name&gt;</span>';
/** Words with inline html in them, and words with a hard break in them. */
const KEPT = `<p>Kept by ${HTML_ATOM}${AFTER}</p>`;
const BROKEN = "<p>Line one.<br>Line two, then the rest.</p>";

const CITED = "dispatch://CORE-2";
/** A reference drawn by its target's key and title, and words that it ends. */
const REFERENCE = `<a data-row-reference="${CITED}" data-row-written="${CITED}" href="/CORE-2"><span>CORE-2</span> The other issue</a>`;
const ENDS_IN_REFERENCE = `<p>Read ${REFERENCE}</p>`;
const MENTIONED = "dispatch://CORE-1/spec#b-r2";
/** A mention drawn as a chip by its block's words, and words that it ends. */
const MENTION = `<a data-row-chip="" data-row-reference="${MENTIONED}" data-row-written="${MENTIONED}" href="/CORE-1/spec#b-r2">The other statement</a>`;
const ENDS_IN_MENTION = `<p>Read ${MENTION}</p>`;

test("a press after a reference counts it as its text wrote it, once for all its words", () => {
  const html = `<p>Read ${REFERENCE}${AFTER}</p>`;
  expect(pressIn(html, AFTER, 3)).toEqual({
    x: 1,
    y: 2,
    offset: "Read ".length + CITED.length + 3,
    word: [`${CITED} `, "first,", " then", 2],
  });
});

test("a press after a mention counts it as its text wrote it", () => {
  const html = `<p>Read ${MENTION}${AFTER}</p>`;
  expect(pressIn(html, AFTER, 3)).toEqual({
    x: 1,
    y: 2,
    offset: "Read ".length + MENTIONED.length + 3,
    word: [`${MENTIONED} `, "first,", " then", 2],
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
  const html = `<p>Read the note<sup data-row-written="" data-type="footnote_reference">1</sup>${AFTER}</p>`;
  expect(pressIn(html, AFTER, 3)).toEqual({
    x: 1,
    y: 2,
    offset: "Read the note".length + 3,
    word: ["note ", "first,", " then", 2],
  });
});

test("a press after a picture counts the picture as nothing", () => {
  const html = `<p>See <a data-row-written="" data-dispatch-picture="dispatch://CORE-1/artifact/shot-png" data-picture-caption="shot.png" href="/CORE-1/artifact/shot-png"><span>shot.png</span></a>${AFTER}</p>`;
  expect(pressIn(html, AFTER, 3)).toEqual({
    x: 1,
    y: 2,
    offset: "See ".length + 3,
    word: ["See  ", "first,", " then", 2],
  });
});

// A hard break holds no text in the view or the editor, and each reading counts it as one
// character, "\n", so the count after it is never the count before it, and the words on each side
// of it read as two words there, as they do in the editor's reading.
test("a press after a hard break counts the break as one character", () => {
  expect(pressIn(BROKEN, "Line two, then the rest.", 6)).toEqual({
    x: 1,
    y: 2,
    offset: "Line one.\n".length + 6,
    word: ["Line ", "two,", " then", 1],
  });
});

// The count at the start of the text after an atom is also the end of the text before it, so the
// press says which of the two it was at; at the start of the text after a break it says so too.
test("a press at the start of the text after a hard break or an atom says the caret goes into that text", () => {
  expect(pressIn(BROKEN, "Line two, then the rest.", 0)).toEqual({
    x: 1,
    y: 2,
    offset: "Line one.\n".length,
    word: ["one.\n", "Line", " two,", 0],
    next: true,
  });
  expect(pressIn(KEPT, AFTER, 0)).toEqual({ x: 1, y: 2, offset: "Kept by ".length, next: true });
});

test("a press at the end of the text before a break, inside an atom or at the words' start goes into no next text", () => {
  const end = pressIn(BROKEN, "Line one.", "Line one.".length);
  expect(end).toEqual({ x: 1, y: 2, offset: 9, word: ["Line ", "one.", "\nLine", 4] });
  expect(end.next).toBeUndefined();
  const atom = withLines([AROUND_PRESS], () => pressIn(KEPT, "<name>", 2));
  expect(atom).toEqual({ x: 1, y: 2, offset: "Kept by ".length });
  expect(atom.next).toBeUndefined();
  const start = pressIn(BROKEN, "Line one.", 0);
  expect(start).toEqual({ x: 1, y: 2, offset: 0, word: ["", "Line", " one.", 0] });
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
  [
    '<sup data-row-written="" data-type="footnote_reference">1</sup>',
    (atom) => [atom.firstChild ?? atom, 0],
  ],
  [
    `<a data-row-written="" ${PICTURE}><span>shot.png</span></a>`,
    (atom) => [atom.firstChild?.firstChild ?? atom, 3],
  ],
  [`<a data-row-written="" ${PICTURE}><img alt="shot.png"></a>`, (atom) => [atom, 1]],
];
const NOTE = "Read the note";
/** The press at the end of `NOTE`, before an atom after it. */
const AT_NOTE_END: CaretAt = { x: 1, y: 2, offset: NOTE.length, word: ["the ", "note", "", 4] };
/** `NOTE` and then inline HTML that ends the words, in a paragraph that runs `direction`. */
function noteAndHtml(direction?: "ltr" | "rtl"): string {
  const style = direction === undefined ? "" : ` style="direction: ${direction}"`;
  return `<p${style}>${NOTE}${HTML_ATOM}</p>`;
}

/** A press at `at`, (1, 2) where none is given, that the browser answers in the last marked
 *  element `paragraph` draws, at `offset` into its last text: a place inside an atom, or inside a
 *  reference's link. */
function pressInLastMark(
  paragraph: string,
  offset: number,
  at?: readonly [number, number]
): CaretAt {
  return pressAt(
    paragraph,
    (words) => {
      const marks = words.querySelectorAll("[data-row-written]");
      const mark = marks[marks.length - 1] ?? words;
      const walker = document.createTreeWalker(mark, NodeFilter.SHOW_TEXT);
      let text: Node | null = null;
      for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) text = node;
      return [text ?? mark, offset];
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
      pastAtom: true,
    });
  }
  const past = pressAt(ENDS_IN_REFERENCE, paragraphEnd);
  expect(past).toEqual({
    x: 1,
    y: 2,
    offset: "Read ".length + CITED.length,
    word: ["Read ", CITED, "", CITED.length],
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
// edge where it runs right to left. A reference is text the editor holds, and a place inside one
// is read by its last line too: a point past the end of that line goes after the reference's
// words as the editor holds them. A place in the text after an atom reads no line of the atom.
// happy-dom lays nothing out, so each case gives the atom's lines, placed from the press at
// (1, 2).
test("a press the browser answers inside an atom, past the end of its last line, says the caret goes after it", () => {
  const after: CaretAt = { ...AT_NOTE_END, pastAtom: true };
  // A press the browser answers at the end of the text of the last atom `paragraph` draws, as
  // Chromium answers a point past inline HTML that ends a line.
  const pressPast = (paragraph: string) => pressInLastMark(paragraph, "<name>".length);
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
    // A point past the end of the line a reference ends, which the browser answers inside the
    // reference's link, goes after the reference's words as the editor holds them; a place in the
    // text after an atom reads no line of the atom.
    line = new DOMRect(-60, -8, 60, 20);
    const title = " The other issue";
    expect(pressIn(ENDS_IN_REFERENCE, title, title.length)).toEqual({
      x: 1,
      y: 2,
      offset: "Read ".length + CITED.length,
      word: ["Read ", CITED, "", CITED.length],
    });
    lines.mockClear();
    expect(pressIn(KEPT, AFTER, 3)).toEqual({
      x: 1,
      y: 2,
      offset: "Kept by ".length + 3,
      word: ["by  ", "first,", " then", 2],
    });
    expect(lines, "a press off an atom laid the atom out").not.toHaveBeenCalled();
  } finally {
    lines.mockRestore();
  }
});

/** A line's last text, which ends with a space the line's end collapses. */
const LINE_END = "the second line ends with ";
/** A picture whose reference names no artifact, as the view draws it: a link with no words. */
const EMPTY_LINK = '<a data-row-written="" href="/CORE-1"></a>';

/** A press past the end of the line `LINE_END` begins, in a paragraph of `LINE_END` and then
 *  `rest`, which Chromium answers in `LINE_END`, before its last space. */
function pressBeforeLastSpace(rest: string): CaretAt {
  return pressIn(`<p>${LINE_END}${rest}</p>`, LINE_END, LINE_END.length - 1);
}

// Chromium answers a point past the end of a line whose last text ends with a space with a place
// in that text before the space, which the line's end collapses, and so before what follows the
// text on the line: a picture whose reference names no artifact, drawn as a link with no words in
// one box of no width. Such a place stands at the end of its line, so the press counts the space
// and goes past the atom, to the hard break that ends the line or to the words' end, and says it
// is past the atom. An element drawn in a box after the place keeps the press where the browser
// answered it, before that element.
test("a press the browser answers before a line's last space, with an empty atom after it, goes to the end of the line", () => {
  const empty = [new DOMRect(718, 495, 0, 19)];
  expect(withLines(empty, () => pressBeforeLastSpace(`${EMPTY_LINK}<br>Next line.`))).toEqual({
    x: 1,
    y: 2,
    offset: LINE_END.length,
    pastAtom: true,
  });
});

test("a press the browser answers before the words' last space, with an empty atom that ends them, goes to their end", () => {
  const empty = [new DOMRect(718, 519, 0, 19)];
  expect(withLines(empty, () => pressBeforeLastSpace(EMPTY_LINK))).toEqual({
    x: 1,
    y: 2,
    offset: LINE_END.length,
    pastAtom: true,
  });
});

test("a press the browser answers before a line's last space stays there where an element in a box follows it", () => {
  const drawn = [new DOMRect(678, 495, 58, 19)];
  expect(withLines(drawn, () => pressBeforeLastSpace(`${HTML_ATOM}<br>Next line.`))).toEqual({
    x: 1,
    y: 2,
    offset: LINE_END.length - 1,
    word: ["ends ", "with", " \nNext", 4],
  });
});

/** The press at the end of `NOTE` at the point (`x`, `y`): it stays inside the atom after the
 *  note, at its start, or with `pastAtom`, goes after it. */
function noteEnd(x: number, y: number, pastAtom?: true): CaretAt {
  return pastAtom === undefined ? { ...AT_NOTE_END, x, y } : { ...AT_NOTE_END, pastAtom, x, y };
}

/** A press past a reference or a mention that ends the words: after its words as written. */
function afterWritten(written: string, x: number, y: number): CaretAt {
  return {
    x,
    y,
    offset: "Read ".length + written.length,
    word: ["Read ", written, "", written.length],
  };
}

// The boxes the real Spec view draws for each inline kind, one row each, as the row "a press on
// each inline kind of a Spec statement, at each place" of `e2e/issue-page-j.e2e.ts` measured them
// in Chromium (its `E2E-BOXES` lines) at 1440 and 390 px, pressed where that row presses, with the
// place the caret goes. A rule that reads one of them right and another wrong fails here. An atom
// stands on one line as one box as tall as its font, 19 px on a 24 px line, and a picture's caption
// 25 px. A reference is drawn in two boxes on one line at 1440 px, its key and its title, and its
// link wraps at 390 px into boxes 25 px tall on 24 px lines, which overlap the line above by 1 px:
// so a box's line is read from its middle.
test("a press around each inline kind is read by the line it is on, in the boxes the Spec view draws", () => {
  const end = "<name>".length;
  const html = noteAndHtml();
  const picture = `<p>${NOTE}<a data-row-written="" ${PICTURE}><span>shot.png</span></a></p>`;
  const note = `<p>${NOTE}<span data-row-written="">[^k5]</span></p>`;
  const rows: readonly (readonly [
    name: string,
    boxes: readonly DOMRect[],
    press: () => CaretAt,
    caret: CaretAt,
  ])[] = [
    [
      "inline HTML, inside it, at 1440 px",
      [new DOMRect(578, 447, 58, 19)],
      () => pressInLastMark(html, 2, [607, 457]),
      noteEnd(607, 457),
    ],
    [
      "inline HTML, past the end of the words, at 1440 px",
      [new DOMRect(678, 495, 58, 19)],
      () => pressInLastMark(html, end, [756, 505]),
      noteEnd(756, 505, true),
    ],
    [
      "inline HTML, past the end of the words, at 390 px",
      [new DOMRect(170, 489, 58, 19)],
      () => pressInLastMark(html, end, [248, 499]),
      noteEnd(248, 499, true),
    ],
    [
      "a picture's caption, past the end of the words, at 390 px",
      [new DOMRect(170, 486, 60, 25)],
      () => pressInLastMark(picture, "shot.png".length, [250, 499]),
      noteEnd(250, 499, true),
    ],
    [
      "a footnote's reference drawn as its written text, inside it, at 390 px",
      [new DOMRect(70, 441, 36, 19)],
      () => pressInLastMark(note, 2, [88, 451]),
      noteEnd(88, 451),
    ],
    [
      "a footnote's reference drawn as its written text, past the end of the words, at 1440 px",
      [new DOMRect(678, 495, 36, 19)],
      () => pressInLastMark(note, "[^k5]".length, [734, 505]),
      noteEnd(734, 505, true),
    ],
    [
      "a reference in two boxes on one line, past the end of the words, at 1440 px",
      [new DOMRect(673, 507, 45, 19), new DOMRect(718, 507, 462, 19)],
      () => pressInLastMark(ENDS_IN_REFERENCE, " The other issue".length, [1200, 517]),
      afterWritten(CITED, 1200, 517),
    ],
    [
      "a reference wrapped in three overlapping boxes, past the end of the words, at 390 px",
      [new DOMRect(165, 510, 128, 25), new DOMRect(26, 534, 297, 25), new DOMRect(26, 558, 74, 25)],
      () => pressInLastMark(ENDS_IN_REFERENCE, " The other issue".length, [120, 571]),
      afterWritten(CITED, 120, 571),
    ],
    [
      "a mention, past the end of the words, at 390 px",
      [new DOMRect(169, 512.203125, 174, 24.59375)],
      () => pressInLastMark(ENDS_IN_MENTION, "The other statement".length, [363, 525]),
      afterWritten(MENTIONED, 363, 525),
    ],
    [
      "a mention wrapped onto two lines, past the end of the line it ends, at 390 px",
      [new DOMRect(213, 462.203125, 128, 24.59375), new DOMRect(26, 487.203125, 43, 24.59375)],
      () => pressInLastMark(ENDS_IN_MENTION, "The other statement".length, [89, 500]),
      afterWritten(MENTIONED, 89, 500),
    ],
  ];
  for (const [name, boxes, press, caret] of rows) {
    const at = withLines(boxes, press);
    expect(at, name).toEqual(caret);
    expect(at.pastAtom, name).toBe(caret.pastAtom);
  }
});
