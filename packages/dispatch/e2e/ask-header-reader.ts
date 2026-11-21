// The one reader of an ask header's geometry, which every row of `ask-header-sweep.e2e.ts` reads
// its headers through (dispatch://INFRA-341, R1). `readHeader` reads, in the page, what a header
// draws at the width it has; a sweep is that reading at every width from `NARROWEST` up, `STEP` px
// at a time, the way a host of that width would draw it; a switch-width row is that reading at the
// widths a layout switches at alone; and a host-width row is one reading at the width its host
// draws the header at. Each reading fails on the five checks that file's head names. What each
// place draws (`Drawn`) and what each row holds it to stay in that file, beside its rows: this
// module is what they read with, and holds no row.
import { expect, type Locator } from "@playwright/test";

/** The width the sweep starts from: just under the narrowest header a host draws, which is the
 *  Inbox's reading pane at 1280 px dragged to its 30% - about 218 px of a card's header there,
 *  and 217 px of a decision block's, where round 10's fault was. */
const NARROWEST = 216;
/** The width every header is swept to at least: past the widest layout switch (the block's
 *  `ASK_BLOCK_LINE`, `ask-header-sweep.e2e.ts`), and the width of a tablet's widest header. A
 *  header its host draws wider is swept to its own width. */
const WIDEST = 960;
const STEP = 4;
/** The fewest widths a sweep reads a header at: every `STEP` from `NARROWEST` while under
 *  `WIDEST`, and `WIDEST` itself. A header its host draws wider is read at more. */
const SWEPT_WIDTHS = Math.ceil((WIDEST - NARROWEST) / STEP) + 1;

/** How many lines a row draws, by the header's width: each band is `[from, least, most]`, read
 *  from the widest down, and the first whose `from` the width reaches decides. */
export type Lines = readonly (readonly [from: number, least: number, most: number])[];

/** What one header draws in its place, for the reader to hold at every width. */
export interface Drawn {
  /** Each field the header draws there, by name, as a selector inside the header. */
  readonly fields: Readonly<Record<string, string>>;
  /** The lines the row draws, where the place's layout decides them; a record's sentence wraps
   *  as text does, so it gives none. */
  readonly lines?: Lines;
  /** The row the copy control and the lines are read in, as a selector inside the header, or
   *  matching the header itself where the header is its own row (a decision block's). */
  readonly row: string;
}

/** A box the page draws, or the span of what a part draws, in the page's px. */
export interface Rect {
  readonly bottom: number;
  readonly left: number;
  readonly right: number;
  readonly top: number;
}

/** What one field of a header draws at one width. */
export interface FieldReading {
  /** How much of the field's text is drawn, cut where a box clips it; a field that holds no
   *  text is its box, and `whole` is then infinite. */
  readonly drawn: number;
  /** The lines of the row the field stands on, counted from 0, the row's first. */
  readonly lines: readonly number[];
  /** The span of everything the field draws, or null where it draws nothing. */
  readonly span: Rect | null;
  /** How wide the field's text is, whole. */
  readonly whole: number;
}

/** What a header draws at one width: the reading every assertion of `ask-header-sweep.e2e.ts`
 *  is made on. */
export interface Reading {
  /** Each failure of the five that file's head names. */
  readonly failures: readonly string[];
  /** Each field of `Drawn.fields` the header draws, by name. */
  readonly fields: Readonly<Record<string, FieldReading>>;
  /** The header's own box. */
  readonly head: Rect;
  /** How many lines the row draws. */
  readonly lines: number;
  /** The row's own box, which is the `@container` a card's layout reads, or null where no row
   *  matches `Drawn.row`. */
  readonly row: Rect | null;
  /** The header's width the reading was taken at, in whole px. */
  readonly width: number;
}

/** The widths a reading is taken at: a sweep's, from `from` up by `step`, to `widest` or the
 *  header's own width, whichever is wider; or the widths `at` alone. */
type ReadWidths =
  | { readonly from: number; readonly step: number; readonly widest: number }
  | { readonly at: readonly number[] };

/**
 * The one reader of a header's geometry, run in the page: `measure` reads what the header draws
 * at the width it has, and the sweep is `measure` at each of `widths`, with the header's own
 * `style` put back afterwards. With no `widths` it is one reading at the width the header's host
 * draws it.
 */
function readHeader(
  node: Element,
  { drawn, widths }: { drawn: Drawn; widths: ReadWidths | null }
): Reading[] {
  const root = node as HTMLElement;
  /** The sub-pixel slack two boxes laid side by side meet within. */
  const SLACK = 1;
  /** How much of a truncated name keeps it visible: a letter or two and the ellipsis. */
  const LEAST_TEXT = 12;
  interface Box {
    bottom: number;
    left: number;
    right: number;
    top: number;
  }
  interface Part {
    readonly element: Element;
    readonly name: string;
    /** Placed against a frame outside the header (the urgency notch over the card's top
     *  edge), so the header's edge is not its edge. */
    readonly placed: boolean;
    readonly rects: readonly Box[];
  }

  const boxOf = ({ bottom, left, right, top }: Box): Box => ({ bottom, left, right, top });
  // A `contents` box draws nothing of its own, so whether it is shown is whether its parent
  // is: `checkVisibility` answers false for any element with no box.
  const shown = (element: Element): boolean =>
    getComputedStyle(element).display === "contents"
      ? element.parentElement === null || shown(element.parentElement)
      : element.checkVisibility({ opacityProperty: true, visibilityProperty: true });
  const drawnAtAll = (rect: Box) => rect.right - rect.left > 0.5 && rect.bottom - rect.top > 0.5;
  // What of `rect` is drawn: every box between it and the header that clips its overflow
  // cuts it to its own edges, as `truncate` does a name.
  const clippedBox = (rect: Box, from: Element | null): Box => {
    const kept = boxOf(rect);
    for (let at = from; at !== null && at !== root; at = at.parentElement) {
      const style = getComputedStyle(at);
      const box = at.getBoundingClientRect();
      if (style.overflowX !== "visible") {
        kept.left = Math.max(kept.left, box.left);
        kept.right = Math.min(kept.right, box.right);
      }
      if (style.overflowY !== "visible") {
        kept.top = Math.max(kept.top, box.top);
        kept.bottom = Math.min(kept.bottom, box.bottom);
      }
    }
    return kept;
  };
  const textRects = (text: Text): Box[] => {
    const range = document.createRange();
    range.selectNodeContents(text);
    return [...range.getClientRects()]
      .map((rect) => clippedBox(rect, text.parentElement))
      .filter(drawnAtAll);
  };
  const boxRects = (element: Element): Box[] =>
    [...element.getClientRects()]
      .map((rect) => clippedBox(rect, element.parentElement))
      .filter(drawnAtAll);
  const placed = (element: Element) => {
    for (let at: Element | null = element; at !== null && at !== root; at = at.parentElement) {
      const { position } = getComputedStyle(at);
      if (position === "absolute" || position === "fixed") return true;
    }
    return false;
  };
  // A part drawn as a box rather than as text: a control (whose whole box is where a tap
  // lands), a mark, a pill, and any box that paints a background or a border. A link inside
  // a sentence is text: its box is padded for a tap without moving the line.
  const boxed = (element: Element) => {
    if (element.matches("button, svg, [role='img'], [data-ask-state-label]")) return true;
    const style = getComputedStyle(element);
    if (element.matches("a") && style.display !== "inline") return true;
    const painted = !/^(transparent|rgba\(.*,\s*0\))$/.test(style.backgroundColor);
    const bordered = ["top", "right", "bottom", "left"].some(
      (side) =>
        Number.parseFloat(style.getPropertyValue(`border-${side}-width`)) > 0 &&
        style.getPropertyValue(`border-${side}-style`) !== "none"
    );
    return painted || bordered;
  };
  const say = (element: Element) => {
    // Run in the page by `evaluate`, this is the DOM's own read of the label it names.
    const words = (element.getAttribute("aria-label") ?? element.textContent ?? "")
      .trim()
      .replace(/\s+/g, " ");
    return `${element.tagName.toLowerCase()} "${words.length > 28 ? `${words.slice(0, 27)}…` : words}"`;
  };
  // What a boxed part draws inside its box: its text and its marks.
  const inside = (element: Element, rects: Box[]) => {
    for (const child of element.childNodes) {
      if (child instanceof Text) {
        if (child.data.trim() !== "") rects.push(...textRects(child));
      } else if (child instanceof Element && shown(child)) {
        if (child.matches("svg, [role='img']")) rects.push(...boxRects(child));
        else inside(child, rects);
      }
    }
  };
  // Every drawn part under `element`, in the order the page draws them: each text run, and
  // each boxed part with whatever it draws inside it.
  const parts = (element: Element, found: Part[] = []): Part[] => {
    for (const child of element.childNodes) {
      if (child instanceof Text) {
        const parent = child.parentElement;
        if (child.data.trim() === "" || parent === null || !shown(parent)) continue;
        const words = child.data.trim();
        found.push({
          element: parent,
          name: JSON.stringify(words.length > 28 ? `${words.slice(0, 27)}…` : words),
          placed: placed(parent),
          rects: textRects(child),
        });
      } else if (child instanceof Element && shown(child)) {
        if (boxed(child)) {
          const rects = boxRects(child);
          inside(child, rects);
          found.push({ element: child, name: say(child), placed: placed(child), rects });
        } else {
          parts(child, found);
        }
      }
    }
    return found;
  };
  // Everything `field` draws: its box where it is a boxed part, and every part inside it.
  const drawnIn = (field: Element): Box[] => {
    if (boxed(field)) {
      const rects = boxRects(field);
      inside(field, rects);
      return rects;
    }
    return parts(field).flatMap((part) => part.rects);
  };
  const overlap = (a: Box, b: Box) =>
    Math.min(a.right, b.right) - Math.max(a.left, b.left) > SLACK &&
    Math.min(a.bottom, b.bottom) - Math.max(a.top, b.top) > SLACK;
  // Whether two spans stand on one line: each starts above where the other ends, by more than
  // the slack two lines meet within.
  const sameLine = (a: Box, b: Box) =>
    Math.min(a.bottom, b.bottom) - Math.max(a.top, b.top) > SLACK;
  const spanOf = (rects: readonly Box[]): Box => ({
    bottom: Math.max(...rects.map((rect) => rect.bottom)),
    left: Math.min(...rects.map((rect) => rect.left)),
    right: Math.max(...rects.map((rect) => rect.right)),
    top: Math.min(...rects.map((rect) => rect.top)),
  });
  // How much of a field is drawn, against how much of it there is: its text, clipped where
  // it is clipped, or its box where it holds no text.
  const extent = (field: Element) => {
    let drawnWidth = 0;
    let whole = 0;
    const texts = document.createTreeWalker(field, NodeFilter.SHOW_TEXT);
    for (let text = texts.nextNode(); text !== null; text = texts.nextNode()) {
      const parent = text.parentElement;
      if (!(text instanceof Text) || text.data.trim() === "" || parent === null) continue;
      if (!shown(parent)) continue;
      const range = document.createRange();
      range.selectNodeContents(text);
      for (const rect of range.getClientRects()) {
        whole += rect.width;
        const kept = clippedBox(rect, parent);
        if (drawnAtAll(kept)) drawnWidth += kept.right - kept.left;
      }
    }
    if (whole > 0) return { drawn: drawnWidth, whole };
    const box = clippedBox(field.getBoundingClientRect(), field.parentElement);
    return {
      drawn: drawnAtAll(box) ? box.right - box.left : 0,
      whole: Number.POSITIVE_INFINITY,
    };
  };

  // One reading of the header: at `width` where one is given, else at the width its host
  // draws it.
  const measure = (width: number | null): Reading => {
    if (width !== null) {
      root.style.width = `${width}px`;
      root.style.maxWidth = "none";
      root.style.minWidth = "0px";
      root.style.flex = "none";
    }
    const failures: string[] = [];
    const fail = (message: string) => {
      failures.push(message);
    };
    const head = boxOf(root.getBoundingClientRect());
    const all = parts(root);

    for (const [index, a] of all.entries()) {
      for (const b of all.slice(index + 1)) {
        if (a.element.contains(b.element) || b.element.contains(a.element)) continue;
        if (a.rects.some((x) => b.rects.some((y) => overlap(x, y)))) {
          fail(`${a.name} overlaps ${b.name}`);
        }
      }
      if (a.placed) continue;
      for (const rect of a.rects) {
        if (rect.left < head.left - SLACK) fail(`${a.name} crosses the header's left edge`);
        if (rect.right > head.right + SLACK) fail(`${a.name} crosses the header's right edge`);
        if (rect.bottom > head.bottom + SLACK) fail(`${a.name} crosses the header's foot`);
        if (rect.top < head.top - SLACK) fail(`${a.name} crosses the header's top`);
      }
    }

    const row = root.matches(drawn.row) ? root : root.querySelector(drawn.row);
    // The row's lines, each the span of the parts on it: a part opens a line when it starts
    // below the foot of every part on the line above it.
    const lines: Box[] = [];
    if (row === null) {
      fail(`no row matches ${drawn.row}`);
    } else {
      const copy = row.querySelector("[data-copy-ref] button");
      const inRow = all.filter(
        (part) =>
          row.contains(part.element) &&
          part.element.closest("[data-copy-ref]") === null &&
          part.rects.length > 0
      );
      const first = inRow[0];
      const copyPart = all.find((part) => part.element === copy);
      if (copy !== null && first !== undefined && copyPart !== undefined) {
        if (!sameLine(spanOf(copyPart.rects), spanOf(first.rects))) {
          fail(`the copy control is not on the line of ${first.name}`);
        }
      }
      for (const span of inRow.map((part) => spanOf(part.rects)).sort((a, b) => a.top - b.top)) {
        const line = lines.at(-1);
        if (line === undefined || span.top >= line.bottom - SLACK) {
          lines.push(span);
        } else {
          line.bottom = Math.max(line.bottom, span.bottom);
          line.left = Math.min(line.left, span.left);
          line.right = Math.max(line.right, span.right);
        }
      }
      const at = width ?? head.right - head.left;
      const band = drawn.lines?.find(([least]) => at >= least);
      if (band !== undefined && (lines.length < band[1] || lines.length > band[2])) {
        fail(
          `the row draws ${lines.length} line${lines.length === 1 ? "" : "s"}, where its place draws ${band[1] === band[2] ? band[1] : `${band[1]} to ${band[2]}`}`
        );
      }
    }

    const fields: Record<string, FieldReading> = {};
    for (const [name, selector] of Object.entries(drawn.fields)) {
      const field = root.querySelector(selector);
      if (field === null) {
        fail(`no ${name} is drawn`);
        continue;
      }
      if (!shown(field)) {
        fail(`the ${name} is hidden`);
        continue;
      }
      const { drawn: kept, whole } = extent(field);
      if (kept <= 0 || kept < Math.min(whole, LEAST_TEXT)) {
        fail(`the ${name} is cut to ${Math.round(kept)} px`);
      }
      const rects = drawnIn(field);
      const span = rects.length === 0 ? null : spanOf(rects);
      fields[name] = {
        drawn: kept,
        lines:
          span === null
            ? []
            : lines.flatMap((line, index) => (sameLine(span, line) ? [index] : [])),
        span,
        whole,
      };
    }

    return {
      failures,
      fields,
      head,
      lines: lines.length,
      row: row === null ? null : boxOf(row.getBoundingClientRect()),
      width: width ?? Math.round(head.right - head.left),
    };
  };

  if (widths === null) return [measure(null)];
  const swept: number[] = [];
  if ("at" in widths) {
    swept.push(...widths.at);
  } else {
    const own = root.getBoundingClientRect().width;
    for (let width = widths.from; width < Math.max(widths.widest, own); width += widths.step) {
      swept.push(width);
    }
    swept.push(Math.max(widths.widest, Math.round(own)));
  }
  // Run in the page by `evaluate`: the root's own style, kept so the sweep can put it back.
  const style = root.getAttribute("style");
  try {
    return swept.map((width) => measure(width));
  } finally {
    if (style === null) root.removeAttribute("style");
    else root.setAttribute("style", style);
  }
}

/** Reads `header` at the width its host draws it: the one reading of a host-width row. */
export async function readAtHostWidth(header: Locator, drawn: Drawn): Promise<Reading> {
  // With no widths `readHeader` returns the one reading at the host's width, so this is never
  // undefined and the check below only narrows the type.
  const [reading] = await header.evaluate(readHeader, { drawn, widths: null });
  if (reading === undefined) throw new Error("the header was read at no width");
  return reading;
}

/** One failure, at the width it was read at. */
interface Failure {
  readonly message: string;
  readonly width: number;
}

/** Every failure of the five `ask-header-sweep.e2e.ts`'s head names in `readings`, with the
 *  width it was read at. */
function failuresOf(readings: readonly Reading[]): Failure[] {
  return readings.flatMap(({ failures, width }) => failures.map((message) => ({ message, width })));
}

/** A sweep's failures as the row reports them: each failure once, with the widths it was read
 *  at as runs, so a control's report says where it reddened. */
function byWidth(failures: readonly Failure[]): string[] {
  const widths = new Map<string, number[]>();
  for (const { message, width } of failures) {
    const read = widths.get(message) ?? [];
    if (read.at(-1) !== width) read.push(width);
    widths.set(message, read);
  }
  return [...widths].map(([message, read]) => {
    const runs: string[] = [];
    let start = read[0] ?? 0;
    for (const [index, width] of read.entries()) {
      const next = read[index + 1];
      if (next === undefined || next - width > STEP) {
        runs.push(start === width ? `${width}` : `${start}-${width}`);
        if (next !== undefined) start = next;
      }
    }
    return `${runs.join(", ")} px: ${message}`;
  });
}

/** The count a row's "no failure at any width" is read over: `readings` holds a reading at each
 *  of the `least` widths, at the fewest, the row reads the header named `what` at. A header read
 *  at no width draws no failure, so a row whose widths came to nothing would go green holding
 *  its header to nothing; a row that asks for no width is refused, as `expectNoneOf` refuses
 *  one (`e2e/none-of.ts`). */
function readAtEach(readings: readonly Reading[], least: number, what: string): void {
  if (least < 1) {
    throw new Error(
      `${what} is read at no width, which holds its header to nothing: name the widths it is ` +
        "read at."
    );
  }
  expect
    .soft(readings.length, `${what}: the widths its header was read at`)
    .toBeGreaterThanOrEqual(least);
}

/** Sweeps one header and holds it to its place, as one reported check of the row. */
export async function holds(header: Locator, drawn: Drawn, what: string): Promise<void> {
  await expect(header).toBeVisible();
  const readings = await header.evaluate(readHeader, {
    drawn,
    widths: { from: NARROWEST, step: STEP, widest: WIDEST },
  });
  readAtEach(readings, SWEPT_WIDTHS, what);
  expect.soft(byWidth(failuresOf(readings)), what).toEqual([]);
}

/** Reads `header` at each of `widths` alone, as a sweep reads it at each of its own, and holds it
 *  to its place there, as one reported check of the row: a switch-width row's reading, which
 *  names the widths a layout switches at rather than every width. */
export async function holdsAt(
  header: Locator,
  drawn: Drawn,
  widths: readonly number[],
  what: string
): Promise<void> {
  await expect(header).toBeVisible();
  const readings = await header.evaluate(readHeader, { drawn, widths: { at: widths } });
  readAtEach(readings, widths.length, what);
  expect.soft(byWidth(failuresOf(readings)), what).toEqual([]);
}
