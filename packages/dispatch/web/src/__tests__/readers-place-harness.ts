/**
 * The browser a unit test of the reader's place stands in for (`features/doc/readers-place.ts`,
 * `features/doc/readers-scroll.ts`). happy-dom lays nothing out and has no ResizeObserver, so the
 * geometry is this harness: a run of blocks `BLOCK_HEIGHT` tall whose first block starts
 * `documentTop` under the top of the scrollport, a page that scrolls by `window.scrollY` or a
 * column that scrolls by its own `scrollTop`, a `ResizeObserver` the rows fire, the listeners the
 * modules register on the window held here for the rows to call, and animation frames that pass
 * when a row says so. A file that uses it calls `restoreTheBrowser` after each row.
 *
 * It also holds what both watches over the reader's place are read against - the keepers'
 * (`readers-scroll.test.ts`) and the landing correction's (`refocus-when-settled.test.ts`): one
 * input of each entry of the table of the reader's inputs (`THE_READERS_CASES`) and the page's
 * own shortcuts that table reads (`bindTheIssuePagesKeys`). Each suite delivers those inputs its
 * own way: through the listeners this harness holds (`hearTheReader`), or dispatched on the page.
 */

import { appKeymap } from "../features/shell/keymap";

export const BLOCK_HEIGHT = 100;

/** The page's own placing port, which a browser reads as `document.documentElement.clientHeight`
 *  - the window less a horizontal scrollbar - and happy-dom lays out as 0. A row that is not
 *  about that scrollbar reads the window's own height, as a page with no horizontal scrollbar
 *  does; a row that is sets `height` and has it put back after it. */
export const thePagePort = { height: window.innerHeight };
Object.defineProperty(document.documentElement, "clientHeight", {
  configurable: true,
  get: () => thePagePort.height,
});

export interface Page {
  readonly drawn: HTMLElement;
  /** Grows the document by `pixels` above block `index`, as a panel landing there does. */
  grow(index: number, pixels: number): void;
  /** What the reader does: the page scrolls and tells whoever is listening. */
  scrollTo(top: number): void;
  /** Fires every live `ResizeObserver`, as the browser does after it has laid the page out. */
  resized(): void;
  /** The reader turns the wheel: the page hears of it before the scroll it starts. */
  wheel(): void;
  /** An animation frame passes: every callback asked for before it runs. */
  frame(): void;
  readonly scrollY: () => number;
}

/** A document drawn on its own page, which can also be drawn with no box at all. */
export interface DocumentPage extends Page {
  /** Takes the document's box away, or gives it back, as the issue page does when the reader
   *  switches to another tab and back: the panel the document is drawn in is `hidden` while
   *  its editor stays bound. Drawn with no box, the document and every block in it report no
   *  client rects and an all-zero rectangle, as a browser reports for `display: none`. Nothing
   *  is told: the row fires what the change would set off. */
  hide(hidden: boolean): void;
}

/** The rectangle a browser reports for an element it draws no box for. */
const NO_BOX = { bottom: 0, height: 0, top: 0 } as DOMRect;

const observers = new Set<() => void>();
let restoreObserver: (() => void) | undefined;
let scrollListeners: ((event: Event) => void)[] = [];
/** Every other listener the modules register on the window, by event type. */
let windowListeners = new Map<string, ((event: Event) => void)[]>();
/** The animation frame callbacks asked for and not yet run. */
let frames = new Map<number, FrameRequestCallback>();
/** What a row put on the page, taken off it again when the row ends: the drawn document on a
 *  document page, the column carrying it in a reading pane. */
let mounted: HTMLElement | undefined;

export function pageOf(blocks: number, documentTop: number): DocumentPage {
  let scrollY = 0;
  let hidden = false;
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
      hidden
        ? NO_BOX
        : ({
            bottom: topOf(index) + BLOCK_HEIGHT + (grown.get(index) ?? 0),
            top: topOf(index),
          } as DOMRect);
    block.getClientRects = () =>
      (hidden ? [] : [block.getBoundingClientRect()]) as unknown as DOMRectList;
    drawn.append(block);
  }
  // The document's own box: from its first block's top to its last block's bottom.
  drawn.getBoundingClientRect = () =>
    hidden ? NO_BOX : ({ bottom: topOf(blocks), top: topOf(0) } as DOMRect);
  drawn.getClientRects = () =>
    (hidden ? [] : [drawn.getBoundingClientRect()]) as unknown as DOMRectList;
  document.body.append(drawn);
  // How tall the page stands, which is what tells a scroll made to answer for a growth from
  // one the reader made (`pageHeight`).
  Object.defineProperty(document.documentElement, "scrollHeight", {
    configurable: true,
    get: () => topOf(blocks) + scrollY,
  });
  const page: DocumentPage = {
    drawn,
    frame,
    grow(index, pixels) {
      grown.set(index, (grown.get(index) ?? 0) + pixels);
    },
    hide(next) {
      hidden = next;
    },
    resized() {
      for (const fire of [...observers]) fire();
    },
    scrollTo(top) {
      scrollY = top;
      for (const listener of scrollListeners) listener(new Event("scroll"));
    },
    scrollY: () => scrollY,
    wheel,
  };
  installTheBrowser(page);
  mounted = page.drawn;
  return page;
}

/** Where the Inbox's reading column stands on the page, and how tall it is: the column is a box
 *  of its own, so the first line of the document a reader can see is its top edge and not the
 *  window's. */
export const COLUMN_TOP = 200;
export const COLUMN_HEIGHT = 600;

export interface Column extends Page {
  /** How far down the column the reader is, which is what the column scrolls by. */
  readonly scrollTop: () => number;
}

/** The same run of blocks drawn in that column. happy-dom lays nothing out, computes no
 *  overflow and scrolls nothing, so what `whatScrolls` reads to find the box - its overflow and
 *  its content standing taller than it does - is stated here, as are the rectangles. */
export function columnOf(blocks: number, documentTop: number): Column {
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
    frame,
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
    wheel,
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

/** The reader does `event`, which every listener the modules hold for its type hears - a
 *  scroll's among them, which this harness holds apart for the page's own `scrollTo`. */
export function hear(event: Event): void {
  const listeners = event.type === "scroll" ? scrollListeners : windowListeners.get(event.type);
  for (const listener of listeners ?? []) listener(event);
}

/** The reader turns the wheel, which every listener the modules hold for it hears. */
export function wheel(): void {
  hear(new Event("wheel"));
}

/** An animation frame passes: what was asked for before it runs, and what that asks for waits
 *  for the next one. */
export function frame(): void {
  const due = [...frames.values()];
  frames = new Map();
  for (const callback of due) callback(performance.now());
}

/** The browser's own pieces the modules reach for: `ResizeObserver`, the page's scroll offset,
 *  `window.scrollBy`, the listeners they register and the animation frames they ask for. */
export function installTheBrowser(page: Page): void {
  const original = globalThis.ResizeObserver;
  const scrollBy = window.scrollBy;
  const scrollY = Object.getOwnPropertyDescriptor(window, "scrollY");
  const addListener = window.addEventListener;
  const removeListener = window.removeEventListener;
  const requestFrame = window.requestAnimationFrame;
  const cancelFrame = window.cancelAnimationFrame;
  scrollListeners = [];
  windowListeners = new Map();
  frames = new Map();
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
    if (type === "scroll") {
      scrollListeners.push(listener);
      return;
    }
    windowListeners.set(type, [...(windowListeners.get(type) ?? []), listener]);
  }) as never;
  window.removeEventListener = ((type: string, listener: (event: Event) => void) => {
    if (type === "scroll") {
      scrollListeners = scrollListeners.filter((kept) => kept !== listener);
      return;
    }
    windowListeners.set(
      type,
      (windowListeners.get(type) ?? []).filter((kept) => kept !== listener)
    );
  }) as never;
  let lastFrame = 0;
  window.requestAnimationFrame = (callback: FrameRequestCallback) => {
    lastFrame += 1;
    frames.set(lastFrame, callback);
    return lastFrame;
  };
  window.cancelAnimationFrame = (handle: number) => {
    frames.delete(handle);
  };
  restoreObserver = () => {
    globalThis.ResizeObserver = original;
    window.scrollBy = scrollBy;
    window.addEventListener = addListener;
    window.removeEventListener = removeListener;
    window.requestAnimationFrame = requestFrame;
    window.cancelAnimationFrame = cancelFrame;
    if (scrollY !== undefined) Object.defineProperty(window, "scrollY", scrollY);
    (mounted ?? page.drawn).remove();
    mounted = undefined;
  };
}

/** Puts back everything a row's page replaced, and the page's port: each file that uses this
 *  harness runs it after every row. */
export function restoreTheBrowser(): void {
  restoreObserver?.();
  restoreObserver = undefined;
  observers.clear();
  scrollListeners = [];
  windowListeners = new Map();
  frames = new Map();
  thePagePort.height = window.innerHeight;
}

/** The page's own shortcuts, as the issue page registers them (`IssuePage.tsx`): `s` focuses the
 *  issue's Status, which scrolls the page to it, `1` writes the priority, and `t c` switches to
 *  the Conversation tab. Registered in the application's registry, which is the one the table of
 *  the reader's inputs reads (`effectOf`, `features/shell/keymap.ts`); no binding runs anything,
 *  as no row needs one to. Returns the unregister, which a suite calls after its last row. */
export function bindTheIssuePagesKeys(): () => void {
  return appKeymap.register("global", [
    { effect: "moves the reader", id: "status", keys: "s", label: "Focus the status", run() {} },
    { effect: "moves nothing", id: "set-priority", keys: "1", label: "Set priority", run() {} },
    {
      effect: "moves the reader",
      id: "tab-conversation",
      keys: "t c",
      label: "Conversation tab",
      run() {},
    },
  ]);
}

/**
 * One event of a reader's input, as a browser reports it.
 *
 * A key names the box it is pressed into: the page itself where `into` names none, and `into`
 * is given the suite's own editor - a box that takes what is typed into it - for a key typed
 * there. A key pressed `onThePage` is heard by the page's key registry after the watches, as the
 * browser hands a key on (`KeymapProvider`): the registry takes a shortcut's key and moves a
 * chord on, which is what makes the second key of a chord its binding's. A `keydown` that names
 * no `key` is one no keyboard made, which a script dispatched with no key to name.
 */
export type ReadersStep =
  | {
      readonly held?: KeyboardEventInit;
      readonly into?: (editor: HTMLElement) => HTMLElement;
      readonly key?: string;
      readonly onThePage?: true;
      readonly type: "keydown";
    }
  | { readonly pointerType: "mouse" | "pen" | "touch"; readonly type: "pointerdown" }
  | { readonly type: "scroll" | "touchmove" | "touchstart" | "wheel" };

/** One input of an entry of the table of the reader's inputs (`THE_READERS_INPUTS`,
 *  `features/doc/readers-scroll.ts`), named by the entry it is, and whether it moves the reader -
 *  stated here rather than read from the table, so an entry reverted to the other effect turns
 *  every suite's rows for it red. An entry whose effect is unknown until it moves is read as
 *  moving nothing at its start, which is the whole of what that effect asks of either watch: a
 *  press or a finger that has gone down and not moved leaves them at work. */
export interface ReadersCase {
  readonly entry: string;
  readonly input: readonly ReadersStep[];
  readonly moves: boolean;
  readonly name: string;
}

/** A box that takes what is typed into it, as the editor does. */
export function anEditor(): HTMLElement {
  const editor = document.createElement("div");
  editor.contentEditable = "true";
  return editor;
}

/** An `<input>` of `type`: a decision block's options are checkboxes and radios
 *  (`features/inbox/AskOptionRow.tsx`). */
export function anInput(type: string): HTMLInputElement {
  const input = document.createElement("input");
  input.type = type;
  return input;
}

/** A link the reader has tabbed to. */
export function aLink(): HTMLAnchorElement {
  const link = document.createElement("a");
  link.href = "#b-block";
  return link;
}

/** The event a browser reports for `step`, built afresh, since an event is heard once: a key
 *  bubbles and may be cancelled, as a browser's does, and a press names its kind of pointer. */
export function eventOf(step: ReadersStep): Event {
  switch (step.type) {
    case "keydown":
      return step.key === undefined
        ? new Event("keydown")
        : new KeyboardEvent("keydown", {
            ...step.held,
            bubbles: true,
            cancelable: true,
            key: step.key,
          });
    case "pointerdown": {
      const event = new Event("pointerdown", { bubbles: true });
      Object.defineProperty(event, "pointerType", { value: step.pointerType });
      return event;
    }
    default:
      return new Event(step.type, { bubbles: true });
  }
}

/** A key going down, pressed into `into` - the page itself where nothing has the focus - as this
 *  harness hands it to the modules' listeners: it calls them itself, so the target a browser's
 *  dispatch would give the event is stated on it. */
export function keydown(
  key: string,
  held?: KeyboardEventInit,
  into: Element = document.body
): KeyboardEvent {
  const event = eventOf({ held, key, type: "keydown" }) as KeyboardEvent;
  Object.defineProperty(event, "target", { value: into });
  return event;
}

/** A pointer going down, of the kind `pointerType` names. */
export function pointerdown(pointerType: "mouse" | "pen" | "touch"): Event {
  return eventOf({ pointerType, type: "pointerdown" });
}

/** The reader does `input`, which every listener the modules hold hears through this harness
 *  (`hear`): a key pressed into the box it names - a fresh `anEditor()` where it is typed into
 *  the editor - and handed on to the page's key registry where it is pressed on the page. */
export function hearTheReader(input: readonly ReadersStep[]): void {
  for (const step of input) {
    if (step.type !== "keydown" || step.key === undefined) {
      hear(eventOf(step));
      continue;
    }
    const event = keydown(step.key, step.held, step.into?.(anEditor()));
    hear(event);
    if (step.onThePage === true) {
      appKeymap.handleKeyDown(event);
    }
  }
}

/** The editor itself, for a key typed into it. */
const theEditor = (editor: HTMLElement): HTMLElement => editor;

/** One input of each entry of the table of the reader's inputs, in the table's own order, and
 *  some entries more than once where the box a key is pressed in decides what it does. Both
 *  watches read every one, each delivering it its own way. */
export const THE_READERS_CASES: readonly ReadersCase[] = [
  {
    entry: "a press of a mouse, a pen or a finger",
    input: [{ pointerType: "touch", type: "pointerdown" }],
    moves: false,
    name: "a finger's pointerdown",
  },
  {
    entry: "a press of a mouse, a pen or a finger",
    input: [{ pointerType: "mouse", type: "pointerdown" }],
    moves: false,
    name: "a click that moves neither the place nor the focus",
  },
  {
    entry: "a scroll while a press is held",
    input: [{ pointerType: "mouse", type: "pointerdown" }, { type: "scroll" }],
    moves: true,
    name: "a press that has begun to scroll",
  },
  {
    entry: "a finger going down",
    input: [{ type: "touchstart" }],
    moves: false,
    name: "a touchstart",
  },
  {
    entry: "a finger moving",
    input: [{ type: "touchstart" }, { type: "touchmove" }],
    moves: true,
    name: "a finger that moves",
  },
  { entry: "a wheel", input: [{ type: "wheel" }], moves: true, name: "a wheel" },
  {
    entry: "a keydown no keyboard made",
    input: [{ type: "keydown" }],
    moves: false,
    name: "a keydown a script dispatched",
  },
  {
    entry: "a key the page binds to move the reader",
    input: [{ key: "s", onThePage: true, type: "keydown" }],
    moves: true,
    name: "a shortcut that focuses a header control",
  },
  {
    entry: "a key the page binds to move the reader",
    input: [
      { key: "t", onThePage: true, type: "keydown" },
      { key: "c", onThePage: true, type: "keydown" },
    ],
    moves: true,
    name: "a tab chord",
  },
  {
    entry: "a key the page binds to move nothing",
    input: [{ key: "1", onThePage: true, type: "keydown" }],
    moves: false,
    name: "a shortcut that writes the priority",
  },
  {
    // Not handed on to the registry, which would hold the chord open for the rows after it.
    entry: "a key the page binds to move nothing",
    input: [{ key: "t", type: "keydown" }],
    moves: false,
    name: "the first key of a chord",
  },
  { entry: "Tab", input: [{ key: "Tab", type: "keydown" }], moves: true, name: "Tab" },
  {
    entry: "PageUp or PageDown",
    input: [{ key: "PageDown", type: "keydown" }],
    moves: true,
    name: "a scroll key",
  },
  {
    entry: "PageUp or PageDown",
    input: [{ into: theEditor, key: "PageDown", type: "keydown" }],
    moves: true,
    name: "PageDown in the editor",
  },
  {
    entry: "Enter on a link",
    input: [{ into: aLink, key: "Enter", type: "keydown" }],
    moves: true,
    name: "Enter on a link",
  },
  {
    entry: "Space, an arrow, Home or End in a field",
    input: [{ into: theEditor, key: " ", type: "keydown" }],
    moves: false,
    name: "Space typed into the editor",
  },
  {
    entry: "Space, an arrow, Home or End in a field",
    input: [{ into: () => document.createElement("textarea"), key: "ArrowDown", type: "keydown" }],
    moves: false,
    name: "an arrow in a comment box",
  },
  {
    entry: "Space on a button, a checkbox or a radio",
    input: [{ into: () => document.createElement("button"), key: " ", type: "keydown" }],
    moves: false,
    name: "Space on a button",
  },
  {
    entry: "Space on a button, a checkbox or a radio",
    input: [{ into: () => anInput("checkbox"), key: " ", type: "keydown" }],
    moves: false,
    name: "Space on a checkbox",
  },
  {
    entry: "Space on a button, a checkbox or a radio",
    input: [{ into: () => anInput("radio"), key: " ", type: "keydown" }],
    moves: false,
    name: "Space on a radio",
  },
  {
    entry: "Space, an arrow, Home or End anywhere else",
    input: [{ key: "ArrowDown", type: "keydown" }],
    moves: true,
    name: "an arrow on the page",
  },
  {
    entry: "Space, an arrow, Home or End anywhere else",
    input: [{ into: () => anInput("checkbox"), key: "ArrowUp", type: "keydown" }],
    moves: true,
    name: "an arrow from a checkbox",
  },
  {
    entry: "Space, an arrow, Home or End anywhere else",
    input: [{ into: () => anInput("radio"), key: "Home", type: "keydown" }],
    moves: true,
    name: "Home from a radio",
  },
  {
    entry: "Space, an arrow, Home or End anywhere else",
    input: [{ into: () => anInput("button"), key: "End", type: "keydown" }],
    moves: true,
    name: "End from an input button",
  },
  {
    entry: "Space, an arrow, Home or End anywhere else",
    input: [{ into: () => document.createElement("button"), key: "ArrowDown", type: "keydown" }],
    moves: true,
    name: "an arrow from a button",
  },
  {
    entry: "every other key",
    input: [{ held: { shiftKey: true }, key: "Shift", type: "keydown" }],
    moves: false,
    name: "a modifier",
  },
  {
    entry: "every other key",
    input: [{ into: theEditor, key: "e", type: "keydown" }],
    moves: false,
    name: "a letter typed into the editor",
  },
  {
    entry: "every other key",
    input: [
      { held: { ctrlKey: true }, key: "Control", type: "keydown" },
      { held: { ctrlKey: true }, key: "c", type: "keydown" },
    ],
    moves: false,
    name: "a copy",
  },
  {
    entry: "every other key",
    input: [{ key: "Escape", type: "keydown" }],
    moves: false,
    name: "Escape",
  },
];
