import { afterEach, expect, test } from "bun:test";

import {
  keepTheReadersPlace,
  keepWhatStandsUnder,
  placeInView,
  putTheBoxAt,
  readersBlock,
  scrollportHeight,
  scrollportTop,
  thePageLeftItThere,
  whatScrolls,
  whereTheReaderIs,
} from "./readers-place";

/** The page's own placing port, which a browser reads as `document.documentElement.clientHeight`
 *  - the window less a horizontal scrollbar - and happy-dom lays out as 0. The rows that are not
 *  about that scrollbar read the window's own height, as a page with no horizontal scrollbar
 *  does. */
let pagePort = window.innerHeight;
Object.defineProperty(document.documentElement, "clientHeight", {
  configurable: true,
  get: () => pagePort,
});

// happy-dom lays nothing out and has no ResizeObserver, so the geometry is this harness: a run
// of blocks `BLOCK_HEIGHT` tall whose first block starts `documentTop` under the top of the
// scrollport, a page that scrolls by `window.scrollY` and a `ResizeObserver` the rows fire.
const BLOCK_HEIGHT = 100;

interface Page {
  readonly drawn: HTMLElement;
  /** Grows the document by `pixels` above block `index`, as a panel landing there does. */
  grow(index: number, pixels: number): void;
  /** What the reader does: the page scrolls and tells whoever is listening. */
  scrollTo(top: number): void;
  /** Fires every live `ResizeObserver`, as the browser does after it has laid the page out. */
  resized(): void;
  readonly scrollY: () => number;
}

const observers = new Set<() => void>();
let restoreObserver: (() => void) | undefined;
let scrollListeners: ((event: Event) => void)[] = [];
/** What a row put on the page, taken off it again when the row ends: the drawn document on a
 *  document page, the column carrying it in a reading pane. */
let mounted: HTMLElement | undefined;

function pageOf(blocks: number, documentTop: number): Page {
  let scrollY = 0;
  // How much taller than `BLOCK_HEIGHT` each block is, which is what a panel landing in it adds.
  const grown = new Map<number, number>();
  const topOf = (index: number) => {
    let top = documentTop;
    for (let above = 0; above < index; above += 1) {
      top += BLOCK_HEIGHT + (grown.get(above) ?? 0);
    }
    return top - scrollY;
  };
  const drawn = document.createElement("div");
  for (let index = 0; index < blocks; index += 1) {
    const block = document.createElement("section");
    block.getBoundingClientRect = () =>
      ({
        bottom: topOf(index) + BLOCK_HEIGHT + (grown.get(index) ?? 0),
        top: topOf(index),
      }) as DOMRect;
    drawn.append(block);
  }
  document.body.append(drawn);
  // How tall the page stands, which is what tells a scroll made to answer for a growth from
  // one the reader made (`pageHeight`).
  Object.defineProperty(document.documentElement, "scrollHeight", {
    configurable: true,
    get: () => topOf(blocks) + scrollY,
  });
  const page: Page = {
    drawn,
    grow(index, pixels) {
      grown.set(index, (grown.get(index) ?? 0) + pixels);
    },
    resized() {
      for (const fire of [...observers]) fire();
    },
    scrollTo(top) {
      scrollY = top;
      for (const listener of scrollListeners) listener(new Event("scroll"));
    },
    scrollY: () => scrollY,
  };
  installTheBrowser(page);
  mounted = page.drawn;
  return page;
}

/** Where the Inbox's reading column stands on the page, and how tall it is: the column is a box
 *  of its own, so the first line of the document a reader can see is its top edge and not the
 *  window's. */
const COLUMN_TOP = 200;
const COLUMN_HEIGHT = 600;

interface Column extends Page {
  /** How far down the column the reader is, which is what the column scrolls by. */
  readonly scrollTop: () => number;
}

/** The same run of blocks drawn in that column. happy-dom lays nothing out, computes no
 *  overflow and scrolls nothing, so what `whatScrolls` reads to find the box - its overflow and
 *  its content standing taller than it does - is stated here, as are the rectangles. */
function columnOf(blocks: number, documentTop: number): Column {
  let scrolled = 0;
  const grown = new Map<number, number>();
  const topOf = (index: number) => {
    let top = COLUMN_TOP + documentTop;
    for (let above = 0; above < index; above += 1) {
      top += BLOCK_HEIGHT + (grown.get(above) ?? 0);
    }
    return top - scrolled;
  };
  const column = document.createElement("div");
  column.style.overflowY = "auto";
  column.getBoundingClientRect = () =>
    ({ bottom: COLUMN_TOP + COLUMN_HEIGHT, height: COLUMN_HEIGHT, top: COLUMN_TOP }) as DOMRect;
  Object.defineProperty(column, "clientHeight", { configurable: true, get: () => COLUMN_HEIGHT });
  Object.defineProperty(column, "scrollHeight", {
    configurable: true,
    get: () => topOf(blocks) + scrolled - COLUMN_TOP,
  });
  const drawn = document.createElement("div");
  for (let index = 0; index < blocks; index += 1) {
    const block = document.createElement("section");
    block.getBoundingClientRect = () =>
      ({
        bottom: topOf(index) + BLOCK_HEIGHT + (grown.get(index) ?? 0),
        top: topOf(index),
      }) as DOMRect;
    drawn.append(block);
  }
  column.append(drawn);
  document.body.append(column);
  const page: Column = {
    drawn,
    grow(index, pixels) {
      grown.set(index, (grown.get(index) ?? 0) + pixels);
    },
    resized() {
      for (const fire of [...observers]) fire();
    },
    scrollTo(top) {
      scrolled = top;
      for (const listener of scrollListeners) listener(new Event("scroll"));
    },
    scrollTop: () => scrolled,
    scrollY: () => 0,
  };
  // The column's own scroll, which is what the keeper writes (`moveTheBox`): the browser tells
  // the listeners after the write, and this harness tells them as the page's own does.
  Object.defineProperty(column, "scrollTop", {
    configurable: true,
    get: () => scrolled,
    set: (next: number) => page.scrollTo(next),
  });
  installTheBrowser(page);
  mounted = column;
  return page;
}

/** The browser's own pieces this module reaches for: `ResizeObserver`, the page's scroll
 *  offset, `window.scrollBy` and the scroll listener it registers. */
function installTheBrowser(page: Page): void {
  const original = globalThis.ResizeObserver;
  const scrollBy = window.scrollBy;
  const scrollY = Object.getOwnPropertyDescriptor(window, "scrollY");
  const addListener = window.addEventListener;
  const removeListener = window.removeEventListener;
  scrollListeners = [];
  class ObserverStub {
    private readonly fire: () => void;
    constructor(callback: ResizeObserverCallback) {
      this.fire = () => callback([], this as unknown as ResizeObserver);
      observers.add(this.fire);
    }
    disconnect(): void {
      observers.delete(this.fire);
    }
    observe(): void {}
    unobserve(): void {}
  }
  globalThis.ResizeObserver = ObserverStub as unknown as typeof ResizeObserver;
  Object.defineProperty(window, "scrollY", { configurable: true, get: page.scrollY });
  window.scrollBy = ((_x: number, y: number) => page.scrollTo(page.scrollY() + y)) as never;
  window.addEventListener = ((type: string, listener: (event: Event) => void) => {
    if (type === "scroll") scrollListeners.push(listener);
  }) as never;
  window.removeEventListener = ((type: string, listener: (event: Event) => void) => {
    if (type === "scroll") scrollListeners = scrollListeners.filter((kept) => kept !== listener);
  }) as never;
  restoreObserver = () => {
    globalThis.ResizeObserver = original;
    window.scrollBy = scrollBy;
    window.addEventListener = addListener;
    window.removeEventListener = removeListener;
    if (scrollY !== undefined) Object.defineProperty(window, "scrollY", scrollY);
    (mounted ?? page.drawn).remove();
    mounted = undefined;
  };
}

afterEach(() => {
  restoreObserver?.();
  restoreObserver = undefined;
  observers.clear();
  scrollListeners = [];
  pagePort = window.innerHeight;
});

test("the reader's block is the first one the top of the scrollport is in or above", () => {
  const page = pageOf(40, 0);
  page.scrollTo(1_250);

  expect(readersBlock(page.drawn)).toEqual({ index: 12, top: -50 });
});

test("a reader still above the document's first block has no place to keep", () => {
  const page = pageOf(40, 80);

  expect(readersBlock(page.drawn)).toBeUndefined();
  expect(readersBlock(null)).toBeUndefined();
});

test("a panel landing above the reader leaves their words where they were", () => {
  const page = pageOf(40, 0);
  page.scrollTo(1_250);
  const release = keepTheReadersPlace(page.drawn);
  try {
    page.grow(3, 332);
    page.resized();

    expect(page.scrollY()).toBe(1_582);
    expect(readersBlock(page.drawn)).toEqual({ index: 12, top: -50 });
  } finally {
    release();
  }
});

test("a panel landing below the reader moves nothing", () => {
  const page = pageOf(40, 0);
  page.scrollTo(1_250);
  const release = keepTheReadersPlace(page.drawn);
  try {
    page.grow(20, 332);
    page.resized();

    expect(page.scrollY()).toBe(1_250);
  } finally {
    release();
  }
});

test("a reader who has scrolled is kept where they are now, never pulled back", () => {
  const page = pageOf(40, 0);
  page.scrollTo(1_250);
  const release = keepTheReadersPlace(page.drawn);
  try {
    page.scrollTo(2_500);
    page.grow(3, 332);
    page.resized();

    expect(page.scrollY()).toBe(2_832);
  } finally {
    release();
  }
});

// The browser's own scroll anchoring, the splice that takes the draw's blocks off the page as
// the editor's batches replace them, and a block link's focus all move the scroll themselves.
// Where they left the reader is where the reader is, so a scroll this module did not make is
// read as their place and nothing is answered for twice.
test("growth something else has already answered for is not answered for twice", () => {
  const page = pageOf(40, 0);
  page.scrollTo(1_250);
  const release = keepTheReadersPlace(page.drawn);
  try {
    page.grow(3, 332);
    page.scrollTo(1_582);
    page.resized();

    expect(page.scrollY()).toBe(1_582);
  } finally {
    release();
  }
});

test("a panel landing before the page got to a frame is still answered for", () => {
  const page = pageOf(40, 0);
  page.scrollTo(1_250);
  const release = keepTheReadersPlace(page.drawn);
  try {
    // The scroll is read as it is told, not in a frame of its own, so a growth that lands
    // before that frame is measured against where the reader is now.
    page.scrollTo(2_500);
    page.grow(3, 332);
    page.resized();

    expect(page.scrollY()).toBe(2_832);
  } finally {
    release();
  }
});

test("the page is left alone once the document has gone", () => {
  const page = pageOf(40, 0);
  page.scrollTo(1_250);
  keepTheReadersPlace(page.drawn)();

  page.grow(3, 332);
  page.resized();

  expect(page.scrollY()).toBe(1_250);
});

// The Inbox's reading pane draws the document in a column of its own, whose top is not the top
// of the window. Read against the window, the block a reader is on in that column is one that
// scrolled out of it above them - every block between the window's top and the column's - and a
// growth inside that block moves every word they can see while this module answers nothing.
test("the reader's block in a reading column is read from the column's own top", () => {
  const column = columnOf(40, 0);
  column.scrollTo(1_250);

  expect(readersBlock(column.drawn)).toEqual({ index: 12, top: -50 });
});

test("a card landing above the reader in a reading column leaves their words where they were", () => {
  const column = columnOf(40, 0);
  column.scrollTo(1_250);
  const release = keepTheReadersPlace(column.drawn);
  try {
    // A decision card drawn into its block's shell, which is the growth the Inbox's column
    // meets: the block the reader scrolled past grows under them as its card lands.
    column.grow(3, 332);
    column.resized();

    expect(column.scrollTop()).toBe(1_582);
    expect(readersBlock(column.drawn)).toEqual({ index: 12, top: -50 });
  } finally {
    release();
  }
});

test("a reader still above a reading column's first block has no place to keep", () => {
  const column = columnOf(40, 80);

  expect(readersBlock(column.drawn)).toBeUndefined();
});

// The one reading of where a reader is, and the port they read in, taken against the box that
// scrolls what they are reading. In the Inbox's reading column the window's own offset is a
// number the reader never moves, so a surface that reads it records the wrong place, measures
// the wrong distance and answers the wrong thing about whether the reader is at the top.
test("where the reader is is read down the reading column, not down the window", () => {
  const column = columnOf(40, 0);
  column.scrollTo(1_250);
  const box = whatScrolls(column.drawn);

  expect(box).toBe(column.drawn.parentElement);
  expect(whereTheReaderIs(box)).toBe(1_250);
  expect(scrollportTop(box)).toBe(COLUMN_TOP);
  expect(scrollportHeight(box)).toBe(COLUMN_HEIGHT);
  // The window stands where it stood: a reading taken from it would say the reader had never
  // moved.
  expect(window.scrollY).toBe(0);
});

test("on a document page the same readings are the page's own, and unchanged", () => {
  const page = pageOf(40, 0);
  page.scrollTo(1_250);
  const box = whatScrolls(page.drawn);

  expect(box).toBeNull();
  expect(whereTheReaderIs(box)).toBe(1_250);
  expect(whereTheReaderIs(box)).toBe(window.scrollY);
  expect(scrollportTop(box)).toBe(0);
  expect(scrollportHeight(box)).toBe(window.innerHeight);
});

/** Where a list that scrolls in a box of its own stands, and how tall it is. */
const LIST_TOP = 100;
const LIST_HEIGHT = 300;

/** That list, with one row drawn `rowTop` down its content and `rowHeight` tall: happy-dom lays
 *  nothing out, computes no overflow and clamps no scroll, so what `whatScrolls` reads to find
 *  the box, the rectangles `placeInView` measures, and how far the box may scroll are all
 *  stated here. The box clamps as a browser does, which is what a row written past its end
 *  reads back. */
function listOf(
  rowTop: number,
  rowHeight: number
): { box: HTMLElement; content: number; row: HTMLElement } {
  const content = 4_000;
  let scrolled = 0;
  const box = document.createElement("div");
  box.style.overflowY = "auto";
  Object.defineProperty(box, "clientHeight", { configurable: true, get: () => LIST_HEIGHT });
  Object.defineProperty(box, "scrollHeight", { configurable: true, get: () => content });
  Object.defineProperty(box, "scrollTop", {
    configurable: true,
    get: () => scrolled,
    set: (next: number) => {
      scrolled = Math.max(0, Math.min(next, content - LIST_HEIGHT));
    },
  });
  box.getBoundingClientRect = () =>
    ({ bottom: LIST_TOP + LIST_HEIGHT, height: LIST_HEIGHT, top: LIST_TOP }) as DOMRect;
  const row = document.createElement("div");
  row.getBoundingClientRect = () =>
    ({
      bottom: LIST_TOP + rowTop + rowHeight - scrolled,
      height: rowHeight,
      top: LIST_TOP + rowTop - scrolled,
    }) as DOMRect;
  box.append(row);
  document.body.append(box);
  return { box, content, row };
}

test("a row below the fold is brought to the bottom edge, and the page says it moved it", () => {
  const { box, row } = listOf(500, 40);

  try {
    // `nearest`, the browser's own rule for a highlighted row the arrows have reached: the row
    // is below the box, so its bottom edge comes to the box's bottom edge and no further.
    placeInView(row, { block: "nearest" });

    expect(box.scrollTop).toBe(240);
    expect(row.getBoundingClientRect().bottom).toBe(LIST_TOP + LIST_HEIGHT);
    // Which is the whole point of the wrapper: the stop rule that tells the page's own move
    // from the reader's reads this (`thePageLeftItThere`, `refocus-when-settled.ts`), and the
    // browser's own `scrollIntoView` leaves it saying nothing.
    expect(thePageLeftItThere(box)).toBe(true);
  } finally {
    box.remove();
  }
});

test("a row above the fold is brought to the top edge", () => {
  const { box, row } = listOf(500, 40);

  try {
    box.scrollTop = 600;

    placeInView(row, { block: "nearest" });

    expect(box.scrollTop).toBe(500);
    expect(row.getBoundingClientRect().top).toBe(LIST_TOP);
  } finally {
    box.remove();
  }
});

test("a row already in view is left where it is, and nothing is recorded", () => {
  const { box, row } = listOf(500, 40);

  try {
    box.scrollTop = 450;

    placeInView(row, { block: "nearest" });

    expect(box.scrollTop).toBe(450);
    // The reader put it here, so the page must not claim it: a box the page is standing by is
    // one the stop rule would go on watching through a move the reader made next.
    expect(thePageLeftItThere(box)).toBe(false);
  } finally {
    box.remove();
  }
});

test("a row taller than the box is brought to the top edge rather than its own end", () => {
  const { box, row } = listOf(500, 500);

  try {
    // The browser's `nearest` aligns the far edge of a row that is off the far edge - except
    // where the row is taller than the box, when bringing its end up would carry its first
    // line out of sight.
    placeInView(row, { block: "nearest" });

    expect(box.scrollTop).toBe(500);
    expect(row.getBoundingClientRect().top).toBe(LIST_TOP);
  } finally {
    box.remove();
  }
});

test("a row taller than the box that starts above it is brought to its own end", () => {
  const { box, row } = listOf(300, 500);

  try {
    // The other half of that rule, and the browser's own (CSSOM View, "scroll a target into
    // view": edge A outside and the element taller aligns edge B). The row starts above the
    // box and ends inside it, so bringing its top down to the box's top would carry the end
    // the reader is reading out of sight below; its own end comes to the box's end instead.
    box.scrollTop = 600;

    placeInView(row, { block: "nearest" });

    expect(box.scrollTop).toBe(500);
    expect(row.getBoundingClientRect().bottom).toBe(LIST_TOP + LIST_HEIGHT);

    // And a row whose both edges are outside the box covers it: the reader is inside the row
    // already, and neither edge is brought anywhere.
    box.scrollTop = 450;

    placeInView(row, { block: "nearest" });

    expect(box.scrollTop).toBe(450);
  } finally {
    box.remove();
  }
});

test("a card asked for the middle of the box lands in the middle of it", () => {
  const { box, row } = listOf(500, 40);

  try {
    placeInView(row, { block: "center" });

    const card = row.getBoundingClientRect();
    expect((card.top + card.bottom) / 2).toBe(LIST_TOP + LIST_HEIGHT / 2);
    expect(thePageLeftItThere(box)).toBe(true);
  } finally {
    box.remove();
  }
});

test("a message asked for the start of the box lands on its top edge", () => {
  const { box, row } = listOf(500, 40);

  try {
    placeInView(row, { block: "start" });

    expect(row.getBoundingClientRect().top).toBe(LIST_TOP);
    expect(thePageLeftItThere(box)).toBe(true);
  } finally {
    box.remove();
  }
});

/** How thick the next row's scroller draws its top border: the slash list of the agent thread
 *  and the palette's dialog each carry one (`features/agent-view/AgentThread.tsx`,
 *  `features/search/SearchPalette.tsx`). */
const LIST_BORDER = 10;

/** The same list, drawn with a border around it. The border box is what
 *  `getBoundingClientRect` answers and the padding box is what a scroll lands content in, so
 *  the two differ by `clientTop` - which happy-dom lays out as 0 and is stated here, as every
 *  other rectangle in this file is. */
function borderedListOf(rowTop: number, rowHeight: number): { box: HTMLElement; row: HTMLElement } {
  const content = 4_000;
  let scrolled = 0;
  const box = document.createElement("div");
  box.style.overflowY = "auto";
  Object.defineProperty(box, "clientHeight", { configurable: true, get: () => LIST_HEIGHT });
  Object.defineProperty(box, "clientTop", { configurable: true, get: () => LIST_BORDER });
  Object.defineProperty(box, "scrollHeight", { configurable: true, get: () => content });
  Object.defineProperty(box, "scrollTop", {
    configurable: true,
    get: () => scrolled,
    set: (next: number) => {
      scrolled = Math.max(0, Math.min(next, content - LIST_HEIGHT));
    },
  });
  // The border box: the padding box starts `LIST_BORDER` under its top and is `LIST_HEIGHT`
  // tall, so the border box stands `2 * LIST_BORDER` taller than the port.
  box.getBoundingClientRect = () =>
    ({
      bottom: LIST_TOP + LIST_BORDER * 2 + LIST_HEIGHT,
      height: LIST_BORDER * 2 + LIST_HEIGHT,
      top: LIST_TOP,
    }) as DOMRect;
  const row = document.createElement("div");
  row.getBoundingClientRect = () =>
    ({
      bottom: LIST_TOP + LIST_BORDER + rowTop + rowHeight - scrolled,
      height: rowHeight,
      top: LIST_TOP + LIST_BORDER + rowTop - scrolled,
    }) as DOMRect;
  box.append(row);
  document.body.append(box);
  return { box, row };
}

test("a row is placed in the port a bordered scroller shows, not in its border box", () => {
  for (const [block, lands] of [
    ["start", 500],
    ["end", 500 + 40 - LIST_HEIGHT],
    ["center", 500 + 20 - LIST_HEIGHT / 2],
  ] as const) {
    const { box, row } = borderedListOf(500, 40);

    try {
      // What the browser aligns is the scrollport - the padding box - and a border read as
      // part of it places every row one border-width off (`clientTop`, `scrollportTop` reads
      // the border box's own top).
      placeInView(row, { block });

      expect({ block, lands: box.scrollTop }).toEqual({ block, lands });
    } finally {
      box.remove();
    }
  }
});

/** Where the clipping dialog of the next row stands, how much of its own column it shows, and
 *  how far down that column the list inside it is drawn. Its column is that list and what
 *  stands above it, so what the dialog can scroll is what the list hangs past it by. */
const DIALOG_HEIGHT = 200;
const LIST_IN_DIALOG = 220;
const DIALOG_CONTENT = LIST_IN_DIALOG + LIST_HEIGHT;

/** The search palette's shape at a short window: a dialog that clips its own column
 *  (`overflow: hidden`, a box the reader cannot drag and the page can still scroll) with the
 *  list that scrolls inside it, drawn so far down that column that the list's own port starts
 *  below what the dialog shows. Both boxes are stated, as `listOf` states its one: happy-dom
 *  lays nothing out and clamps no scroll. */
function clippedListOf(
  rowTop: number,
  rowHeight: number
): { dialog: HTMLElement; list: HTMLElement; row: HTMLElement } {
  const content = 4_000;
  let dialogScrolled = 0;
  let listScrolled = 0;
  const dialog = document.createElement("div");
  dialog.style.overflowY = "hidden";
  Object.defineProperty(dialog, "clientHeight", {
    configurable: true,
    get: () => DIALOG_HEIGHT,
  });
  Object.defineProperty(dialog, "scrollHeight", {
    configurable: true,
    get: () => DIALOG_CONTENT,
  });
  Object.defineProperty(dialog, "scrollTop", {
    configurable: true,
    get: () => dialogScrolled,
    set: (next: number) => {
      dialogScrolled = Math.max(0, Math.min(next, DIALOG_CONTENT - DIALOG_HEIGHT));
    },
  });
  dialog.getBoundingClientRect = () =>
    ({ bottom: DIALOG_HEIGHT, height: DIALOG_HEIGHT, top: 0 }) as DOMRect;
  const list = document.createElement("div");
  list.style.overflowY = "auto";
  Object.defineProperty(list, "clientHeight", { configurable: true, get: () => LIST_HEIGHT });
  Object.defineProperty(list, "scrollHeight", { configurable: true, get: () => content });
  Object.defineProperty(list, "scrollTop", {
    configurable: true,
    get: () => listScrolled,
    set: (next: number) => {
      listScrolled = Math.max(0, Math.min(next, content - LIST_HEIGHT));
    },
  });
  const listTop = () => LIST_IN_DIALOG - dialogScrolled;
  list.getBoundingClientRect = () =>
    ({ bottom: listTop() + LIST_HEIGHT, height: LIST_HEIGHT, top: listTop() }) as DOMRect;
  const row = document.createElement("div");
  row.getBoundingClientRect = () =>
    ({
      bottom: listTop() + rowTop + rowHeight - listScrolled,
      height: rowHeight,
      top: listTop() + rowTop - listScrolled,
    }) as DOMRect;
  list.append(row);
  dialog.append(list);
  document.body.append(dialog);
  return { dialog, list, row };
}

test("a row in a list the dialog around it clips is placed by moving both boxes", () => {
  const { dialog, list, row } = clippedListOf(500, 40);

  try {
    // The search palette at a short window: the row is below its own list's port, and that port
    // is below what the dialog shows. Bringing the row to the list's own bottom edge leaves it
    // where nobody can see it, so the dialog is scrolled as well - which is what the browser's
    // own call does, walking every box that scrolls the element rather than the nearest one
    // (`e2e/keyboard-palette.stub.e2e.ts`, the short-window row).
    placeInView(row, { block: "nearest" });

    expect(list.scrollTop).toBe(500 + 40 - LIST_HEIGHT);
    expect(dialog.scrollTop).toBe(DIALOG_CONTENT - DIALOG_HEIGHT);
    // Which is the row inside the part of the dialog that is drawn, at its far edge.
    expect(row.getBoundingClientRect().bottom).toBe(DIALOG_HEIGHT);
    expect(thePageLeftItThere(list)).toBe(true);
    expect(thePageLeftItThere(dialog)).toBe(true);
  } finally {
    dialog.remove();
  }
});

test("a reader who scrolls after a placement leaves the box standing somewhere else", () => {
  const { box, row } = listOf(500, 40);

  try {
    placeInView(row, { block: "nearest" });
    expect(thePageLeftItThere(box)).toBe(true);

    // A scrollbar drag, a find-in-page jump, an assistive tool: the box stands where none of
    // this module's calls left it, and the landing correction watching it stops.
    box.scrollTop = 900;

    expect(thePageLeftItThere(box)).toBe(false);
  } finally {
    box.remove();
  }
});

test("a list put back at its own start says the page put it there", () => {
  const { box, row } = listOf(500, 40);

  try {
    placeInView(row, { block: "nearest" });

    putTheBoxAt(box, 0);

    expect(box.scrollTop).toBe(0);
    expect(thePageLeftItThere(box)).toBe(true);
  } finally {
    box.remove();
  }
});

test("a transcript written to its end is recorded where the box clamped it", () => {
  const { box, content } = listOf(500, 40);

  try {
    // How the live view asks for its own end: a write past the end of the box, which the
    // browser clamps. The record is taken after the write, so it is the place the box clamped
    // the write to rather than the number that was asked for.
    putTheBoxAt(box, content);

    expect(box.scrollTop).toBe(content - LIST_HEIGHT);
    expect(thePageLeftItThere(box)).toBe(true);
  } finally {
    box.remove();
  }
});

test("a card the page itself scrolls is placed by moving the page, which says it moved", () => {
  const page = pageOf(1, 0);
  const card = document.createElement("div");
  card.getBoundingClientRect = () =>
    ({ bottom: 1_050 - page.scrollY(), height: 50, top: 1_000 - page.scrollY() }) as DOMRect;
  document.body.append(card);

  try {
    // A comment a link names on an issue page: no column carries it, so the box is the page
    // itself and what the document element shows is the port.
    placeInView(card, { block: "nearest" });

    expect(page.scrollY()).toBe(1_050 - pagePort);
    expect(thePageLeftItThere(null)).toBe(true);
  } finally {
    card.remove();
  }
});

test("the page's port is what its document element shows, not the window a scrollbar crosses", () => {
  const page = pageOf(1, 0);
  // A page with a horizontal scrollbar: the bar stands inside the window and under the
  // document element, so `window.innerHeight` counts it and `documentElement.clientHeight`,
  // which is what the browser places against, does not.
  pagePort = window.innerHeight - 15;
  const card = document.createElement("div");
  card.getBoundingClientRect = () =>
    ({ bottom: 1_050 - page.scrollY(), height: 50, top: 1_000 - page.scrollY() }) as DOMRect;
  document.body.append(card);

  try {
    placeInView(card, { block: "nearest" });

    // The card's end on the last line the page shows, rather than 15 px under it, behind the
    // scrollbar.
    expect(page.scrollY()).toBe(1_050 - (window.innerHeight - 15));
  } finally {
    card.remove();
  }
});

/** Where the next row's fixed list stands: a box the page's own scroll does not move, holding
 *  a list that ends on the window's last line - the search palette's shape
 *  (`fixed inset-x-0 top-12 bottom-0`, `features/search/SearchPalette.tsx`). */
const FIXED_LIST_TOP = window.innerHeight - LIST_HEIGHT;

/** How much of the page's own bottom a scroll may not land content in, below 1280px
 *  (`html { scroll-padding-bottom: 5rem }`, `web/src/styles.css`), which holds the fixed review
 *  sheet's height clear of whatever the page scrolls to. */
const PAGE_SCROLL_PADDING = 80;

test("a row in a list a fixed box carries is placed without scrolling the page behind it", () => {
  const page = pageOf(1, 0);
  document.documentElement.style.scrollPaddingBottom = `${PAGE_SCROLL_PADDING}px`;
  let scrolled = 0;
  const fixed = document.createElement("div");
  fixed.style.position = "fixed";
  const list = document.createElement("div");
  list.style.overflowY = "auto";
  Object.defineProperty(list, "clientHeight", { configurable: true, get: () => LIST_HEIGHT });
  Object.defineProperty(list, "scrollHeight", { configurable: true, get: () => 4_000 });
  Object.defineProperty(list, "scrollTop", {
    configurable: true,
    get: () => scrolled,
    set: (next: number) => {
      scrolled = Math.max(0, Math.min(next, 4_000 - LIST_HEIGHT));
    },
  });
  // A fixed box stands where the window puts it, whatever the page is scrolled to, and so does
  // everything in it: that is why the page's own scroll cannot place a row of this list.
  list.getBoundingClientRect = () =>
    ({
      bottom: FIXED_LIST_TOP + LIST_HEIGHT,
      height: LIST_HEIGHT,
      top: FIXED_LIST_TOP,
    }) as DOMRect;
  const row = document.createElement("div");
  row.getBoundingClientRect = () =>
    ({
      bottom: FIXED_LIST_TOP + 500 + 40 - scrolled,
      height: 40,
      top: FIXED_LIST_TOP + 500 - scrolled,
    }) as DOMRect;
  list.append(row);
  fixed.append(list);
  document.body.append(fixed);
  page.scrollTo(1_000);

  try {
    // The palette's highlighted row, arrowed down to the foot of its list: the list brings it
    // to its own bottom edge, which is the window's last line and so inside the page's bottom
    // `scroll-padding`. The page's own delta for it is not zero - and the page is not a box
    // that scrolls this row at all, since no scroll of the page moves a fixed box. Scrolling
    // the page would leave the row exactly where it stands and ask for the same move again,
    // on every arrow press and every `ResizeObserver` call the palette makes.
    placeInView(row, { block: "nearest" });

    expect(list.scrollTop).toBe(500 + 40 - LIST_HEIGHT);
    expect(row.getBoundingClientRect().bottom).toBe(FIXED_LIST_TOP + LIST_HEIGHT);
    expect(page.scrollY()).toBe(1_000);
    expect(thePageLeftItThere(null)).toBe(false);
  } finally {
    document.documentElement.style.scrollPaddingBottom = "";
    fixed.remove();
  }
});

test("a box kept above a reading column's top is answered, though it stands under the window's", () => {
  const column = columnOf(40, 0);
  column.scrollTo(1_250);
  let height = 120;
  // The space the Inbox's reading column holds for its chrome, which lands in it taller than
  // what it replaced (`features/doc/AfterTheFirstBatch.tsx`). It stands above the top of the
  // column the reader reads in and under the top of the window, which the column's own top is
  // `COLUMN_TOP` below: read against the window it is on the reader's screen and its growth is
  // theirs to watch land, and read against the column it is out of sight above them and the
  // growth moves every word they can see.
  const held = document.createElement("div");
  held.getBoundingClientRect = () =>
    ({ bottom: COLUMN_TOP - 100 + height, height, top: COLUMN_TOP - 100 }) as DOMRect;
  (column.drawn.parentElement as HTMLElement).append(held);
  const release = keepWhatStandsUnder(held);

  try {
    height = 180;
    column.resized();

    expect(column.scrollTop()).toBe(1_250 + 60);
  } finally {
    release();
    held.remove();
  }
});
