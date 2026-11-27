import { afterEach, beforeEach, expect, test } from "bun:test";
import { act, waitFor } from "@testing-library/react";

import {
  HIGHLIGHT_STYLE_ATTRIBUTE,
  highlightThreadWords,
  TEXT_COMMENT_FOCUS_HIGHLIGHT,
  TEXT_COMMENT_HIGHLIGHT,
} from "./highlights";
import {
  answerSeededReads,
  columnOf,
  drawPage,
  located,
  newClient,
  ON_CHECK,
  ON_STATEMENT,
  rowOf,
} from "./margin-fixtures";

/**
 * R2: in a row drawn read-only, the quoted words of each thread carry an underline and a tint,
 * and those of the thread in focus a stronger tint. The column puts them in the CSS Custom
 * Highlight API's registry (`highlights.ts`), which happy-dom does not hold: each case stands a
 * registry and a `Highlight` in for the browser's, as Chromium's hold the ranges, and the browser
 * suite reads the real one (c-margin (3)).
 */

/** A highlight as the API makes one: the ranges it holds. */
class StandInHighlight {
  readonly ranges: Range[];
  constructor(...ranges: Range[]) {
    this.ranges = ranges;
  }
}

/** The registry the case stands in for `CSS.highlights`. */
let registry = new Map<string, StandInHighlight>();

/** What the window held before the case stood its own in. */
let held: { readonly css?: PropertyDescriptor; readonly highlight?: PropertyDescriptor } = {};

let stubs: { mockRestore(): void }[] = [];

// happy-dom answers a new `CSS` at each read of the window's, so the case stands a `CSS` of its
// own in, which holds the registry and reads every other member from happy-dom's.
beforeEach(() => {
  registry = new Map();
  held = {
    css: Object.getOwnPropertyDescriptor(globalThis, "CSS"),
    highlight: Object.getOwnPropertyDescriptor(globalThis, "Highlight"),
  };
  const css: object = Object.create(CSS, { highlights: { value: registry } });
  Object.defineProperty(globalThis, "CSS", { configurable: true, value: css, writable: true });
  Object.defineProperty(globalThis, "Highlight", {
    configurable: true,
    value: StandInHighlight,
    writable: true,
  });
});

afterEach(() => {
  for (const stub of stubs) stub.mockRestore();
  stubs = [];
  for (const [name, descriptor] of [
    ["CSS", held.css],
    ["Highlight", held.highlight],
  ] as const) {
    if (descriptor === undefined) Reflect.deleteProperty(globalThis, name);
    else Object.defineProperty(globalThis, name, descriptor);
  }
  document.head.querySelector(`style[${HIGHLIGHT_STYLE_ATTRIBUTE}]`)?.remove();
});

/** The words each range of the highlight `name` holds, in order. */
function wordsIn(name: string): string[] {
  return (registry.get(name)?.ranges ?? []).map((range) => range.toString()).sort();
}

test("each thread's words are in the margin highlight and the focused thread's in the focus highlight", async () => {
  stubs = answerSeededReads({ threads: [ON_STATEMENT, ON_CHECK] });
  const client = newClient();
  const view = drawPage(client, "conversation");
  try {
    const column = await columnOf(view, "Comments on text");
    expect(column, "no column is drawn").not.toBeNull();
    await waitFor(() =>
      expect(wordsIn(TEXT_COMMENT_HIGHLIGHT), "the words each thread quotes").toEqual(
        [ON_STATEMENT.quote, ON_CHECK.quote].sort()
      )
    );
    expect(wordsIn(TEXT_COMMENT_FOCUS_HIGHLIGHT), "a thread is in focus").toEqual([]);
    // The highlights mark the words in their rows, not anywhere else on the page.
    const statement = rowOf(view, ON_STATEMENT.node);
    const marked = registry.get(TEXT_COMMENT_HIGHLIGHT)?.ranges ?? [];
    expect(
      marked.some((range) => statement?.contains(range.startContainer) === true),
      "the statement's words are not marked in its row"
    ).toBe(true);
    expect(
      document.head.querySelector(`style[${HIGHLIGHT_STYLE_ATTRIBUTE}]`)?.textContent,
      "the two highlights draw no underline"
    ).toContain("text-decoration: underline");

    const card = column?.querySelector<HTMLElement>(
      `[data-margin-item="${CSS.escape(ON_STATEMENT.comment.id)}"]`
    );
    const control = card?.querySelector<HTMLElement>("button") ?? null;
    expect(control, "the statement's thread draws no control").not.toBeNull();
    await act(async () => control?.focus());
    expect(wordsIn(TEXT_COMMENT_FOCUS_HIGHLIGHT), "the focused thread's words").toEqual([
      ON_STATEMENT.quote,
    ]);
    expect(wordsIn(TEXT_COMMENT_HIGHLIGHT), "every thread's words stay marked").toEqual(
      [ON_STATEMENT.quote, ON_CHECK.quote].sort()
    );
    await act(async () => control?.blur());
    expect(wordsIn(TEXT_COMMENT_FOCUS_HIGHLIGHT), "a thread stays in focus").toEqual([]);
  } finally {
    view.finish();
    client.clear();
  }
  expect([...registry.keys()], "the marks stay after the column goes").toEqual([]);
});

// R2: a row in its editor draws the marks of its comments itself, so the column marks none of its
// threads' words; the read-only caret of R6 (`read-only-caret.ts`) makes a row's words editable
// too, marked `aria-readonly`, and draws no marks, so the row's words keep the column's while it
// stands. The column measures again at each change of the stage's rows, as a read that lands or
// a composer that opens makes, which the case makes by moving the statement's row in place.
test("a row in its editor takes no marks, and a row with the read-only caret keeps its threads' marks", async () => {
  stubs = answerSeededReads({ threads: [ON_STATEMENT, ON_CHECK] });
  const client = newClient();
  const view = drawPage(client, "conversation");
  try {
    expect(await columnOf(view, "Comments on text"), "no column is drawn").not.toBeNull();
    const both = [ON_STATEMENT.quote, ON_CHECK.quote].sort();
    await waitFor(() =>
      expect(wordsIn(TEXT_COMMENT_HIGHLIGHT), "the words each thread quotes").toEqual(both)
    );
    const { row, words } = located(view, ON_STATEMENT.node);
    const measuredAgain = () => row.parentElement?.insertBefore(row, row.nextSibling);

    // An editor in the statement's row: its words take input, and it draws their marks itself.
    words.setAttribute("contenteditable", "true");
    measuredAgain();
    await waitFor(() =>
      expect(wordsIn(TEXT_COMMENT_HIGHLIGHT), "the column marks words its editor draws").toEqual([
        ON_CHECK.quote,
      ])
    );

    // The read-only caret in the same row, with the attributes `read-only-caret.ts` sets.
    words.setAttribute("inputmode", "none");
    words.setAttribute("aria-readonly", "true");
    measuredAgain();
    await waitFor(() =>
      expect(wordsIn(TEXT_COMMENT_HIGHLIGHT), "the read-only caret took the row's marks").toEqual(
        both
      )
    );
  } finally {
    view.finish();
    client.clear();
  }
});

test("where the browser has no highlights, nothing is marked and nothing fails", () => {
  // happy-dom's own `CSS`, which holds no registry of highlights.
  if (held.css !== undefined) Object.defineProperty(globalThis, "CSS", held.css);
  expect("highlights" in CSS).toBe(false);
  const range = document.createRange();
  const unmark = highlightThreadWords([{ focused: true, range }]);
  expect(registry.size).toBe(0);
  expect(document.head.querySelector(`style[${HIGHLIGHT_STYLE_ATTRIBUTE}]`)).toBeNull();
  unmark();
});
