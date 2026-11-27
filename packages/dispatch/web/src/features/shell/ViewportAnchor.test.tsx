import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
// fireEvent also sends a box's own scroll event, which user-event has no form for, and a pointer
// press that gives no focus, where user-event's press focuses what it goes down on.
// biome-ignore lint/style/noRestrictedImports: this file sends the pointer's own clientX/clientY and a pointerout's relatedTarget, neither of which user-event's hover and unhover can set.
import { fireEvent, render } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createRef, type ReactNode, type RefObject } from "react";

import { type AnchorRow, ViewportAnchor } from "./ViewportAnchor";

const ROW_HEIGHT = 50;
const LIST_TOP = 100;
const VIEWPORT_HEIGHT = 600;

const anchor = createRef<ViewportAnchor>();
const scrollerRef = createRef<HTMLDivElement>();

/** The row the next commit will anchor to, as `getSnapshotBeforeUpdate` will choose it. */
function chosen(): AnchorRow | null {
  const current = anchor.current?.current() ?? null;
  return current === null ? null : { id: current.id, via: current.via };
}

function List({
  enabled,
  groups,
  ownsScrollAnchoring,
  rows,
  scroller,
}: {
  enabled?: () => boolean;
  /** A row's group; rows default to one shared group. */
  groups?: Readonly<Record<string, string>>;
  /** Whether the anchor alone keeps the box's scroll while a place is kept there. */
  ownsScrollAnchoring?: boolean;
  rows: string[];
  /** When given, the rows scroll in this element instead of the document. */
  scroller?: RefObject<HTMLDivElement | null>;
}): ReactNode {
  const list = (
    <ViewportAnchor
      enabled={enabled}
      group="data-group"
      item="data-row"
      ownsScrollAnchoring={ownsScrollAnchoring}
      ref={anchor}
      scroller={scroller}
    >
      {rows.map((id) => (
        <div data-group={groups?.[id] ?? "one"} data-row={id} key={id} tabIndex={-1}>
          <button type="button">{id}</button>
        </div>
      ))}
    </ViewportAnchor>
  );
  return scroller === undefined ? (
    list
  ) : (
    <div data-scroller="" ref={scroller}>
      {list}
    </div>
  );
}

// A fixed geometry: rows are ROW_HEIGHT tall, stacked from `listTop` in DOM order, in a
// VIEWPORT_HEIGHT window that scrolls through `window.scrollY` - or, where the list is rendered
// in a scroller, through that element's `scrollTop`, the scroller itself filling the window and
// never moving. `listTop` starts at LIST_TOP; a test lowers it to shrink content above the rows
// without moving what scrolls them, which is what a card collapsing in a commit of its own does.
// A case may draw a row at another height (`heights`), which moves the rows below it, and a
// row's control is CONTROL_HEIGHT tall and stands at the row's foot: `drawnApart` measures
// both, and `rowAtDepth` finds a row of a list that holds other heights.
const CONTROL_HEIGHT = 20;
const rows = () => [...document.querySelectorAll<HTMLElement>("[data-row]")];
const viewportTop = (id: string) =>
  document.querySelector(`[data-row="${id}"]`)?.getBoundingClientRect().top;
const controlTop = (id: string) =>
  document.querySelector(`[data-row="${id}"] button`)?.getBoundingClientRect().top;

let listTop = LIST_TOP;
let contentLeft = 0;
let contentWidth = 100;
/** The rows a case draws at another height than ROW_HEIGHT, by id. */
let heights: Record<string, number> = {};
let scrollYDescriptor: PropertyDescriptor | undefined;
let originalScrollBy: typeof window.scrollBy;
let restoreSpies: (() => void)[] = [];

const heightOf = (row: Element) => heights[row.getAttribute("data-row") ?? ""] ?? ROW_HEIGHT;
const listHeight = () => rows().reduce((sum, row) => sum + heightOf(row), 0);
/** How far below the list's top `row` starts: the heights of the rows before it. */
function rowOffset(row: Element): number {
  let offset = 0;
  for (const one of rows()) {
    if (one === row) break;
    offset += heightOf(one);
  }
  return offset;
}

/** The row whose span holds the point `depth` px below the list's top. */
function rowAtDepth(depth: number): HTMLElement | null {
  let top = 0;
  for (const row of rows()) {
    if (depth >= top && depth < top + heightOf(row)) return row;
    top += heightOf(row);
  }
  return null;
}

/** The box of `element` where the fixed geometry does not draw it: a row's control, and, while
 *  the case draws a row at another height, every row and the list. Null for the rest. */
function drawnApart(element: Element): DOMRect | null {
  if (element.hasAttribute("data-scroller")) return null;
  const row = element.closest("[data-row]");
  const control = row !== null && row !== element;
  if (!control && Object.keys(heights).length === 0) return null;
  let top = listTop - (scrollerRef.current?.scrollTop ?? window.scrollY);
  let height = listHeight();
  if (row !== null) {
    top += rowOffset(row);
    height = heightOf(row);
  }
  if (control) {
    top += height - CONTROL_HEIGHT;
    height = CONTROL_HEIGHT;
  }
  return {
    bottom: top + height,
    height,
    left: contentLeft,
    right: contentLeft + contentWidth,
    toJSON: () => ({}),
    top,
    width: contentWidth,
    x: contentLeft,
    y: top,
  };
}

function setScrollY(value: number): void {
  Object.defineProperty(window, "scrollY", { configurable: true, value, writable: true });
}

beforeEach(() => {
  scrollYDescriptor = Object.getOwnPropertyDescriptor(window, "scrollY");
  originalScrollBy = window.scrollBy;
  listTop = LIST_TOP;
  contentLeft = 0;
  contentWidth = 100;
  heights = {};
  setScrollY(0);
  window.scrollBy = ((x: number | ScrollToOptions, y?: number) => {
    const top = typeof x === "number" ? y : x.top;
    if (top !== undefined) setScrollY(window.scrollY + top);
  }) as typeof window.scrollBy;
  Object.defineProperty(window, "innerHeight", {
    configurable: true,
    value: VIEWPORT_HEIGHT,
    writable: true,
  });
  const rect = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ) {
    const apart = drawnApart(this);
    if (apart !== null) return apart;
    const all = rows();
    // Whatever scrolls the rows carries them: the window, or the scroller when one is rendered.
    const offset = scrollerRef.current?.scrollTop ?? window.scrollY;
    const isScroller = this.hasAttribute("data-scroller");
    const isRow = this.hasAttribute("data-row");
    const top = isScroller
      ? 0
      : isRow
        ? listTop + all.indexOf(this as HTMLElement) * ROW_HEIGHT - offset
        : listTop - offset;
    const height = isScroller ? VIEWPORT_HEIGHT : isRow ? ROW_HEIGHT : all.length * ROW_HEIGHT;
    return {
      bottom: top + height,
      height,
      left: isScroller ? 0 : contentLeft,
      right: isScroller ? 100 : contentLeft + contentWidth,
      toJSON: () => ({}),
      top,
      width: isScroller ? 100 : contentWidth,
      x: isScroller ? 0 : contentLeft,
      y: top,
    };
  });
  const point = spyOn(document, "elementFromPoint").mockImplementation((_x, y) => {
    if (_x < 0 || _x > 100) return null;
    const offset = scrollerRef.current?.scrollTop ?? window.scrollY;
    if (Object.keys(heights).length > 0) return rowAtDepth(offset + y - listTop);
    const index = Math.floor((offset + y - listTop) / ROW_HEIGHT);
    return rows()[index] ?? null;
  });
  restoreSpies = [() => rect.mockRestore(), () => point.mockRestore()];
});

afterEach(() => {
  for (const restore of restoreSpies) restore();
  window.scrollBy = originalScrollBy;
  if (scrollYDescriptor === undefined) {
    Reflect.deleteProperty(window, "scrollY");
  } else {
    Object.defineProperty(window, "scrollY", scrollYDescriptor);
  }
});

const ids = (count: number, from = 1) => Array.from({ length: count }, (_, i) => `r${from + i}`);

test("a scrolled reader keeps the row at the viewport centre in place when rows are inserted above", () => {
  const view = render(<List rows={ids(12)} />);
  try {
    setScrollY(100);
    // Visible centre: (LIST_TOP - 100 + 600) / 2 = 300 → document y 400 → row index 6 → r7.
    const before = viewportTop("r7");
    expect(chosen()).toEqual({ id: "r7", via: "center" });
    view.rerender(<List rows={["new-a", "new-b", ...ids(12)]} />);
    expect(viewportTop("r7")).toBe(before);
    expect(window.scrollY).toBe(200);
  } finally {
    view.unmount();
  }
});

test("a reader at the top sees new rows arrive without the view following the centre row", () => {
  const view = render(<List rows={ids(12)} />);
  try {
    expect(chosen()).toBeNull();
    view.rerender(<List rows={["new-a", ...ids(12)]} />);
    expect(window.scrollY).toBe(0);
    expect(viewportTop("new-a")).toBe(LIST_TOP);
  } finally {
    view.unmount();
  }
});

test("the row with focus within is the current node even at the top, ahead of the pointer and the centre", async () => {
  const user = userEvent.setup();
  const view = render(<List rows={ids(12)} />);
  try {
    const focused = document.querySelector<HTMLElement>('[data-row="r3"] button');
    if (focused === null) throw new Error("r3 button missing");
    focused.focus();
    const hovered = document.querySelector<HTMLElement>('[data-row="r5"]');
    if (hovered === null) throw new Error("r5 missing");
    await user.hover(hovered);
    const focusedTop = viewportTop("r3");
    const hoveredTop = viewportTop("r5");

    // Two rows land above r3 and one between r3 and r5.
    expect(chosen()).toEqual({ id: "r3", via: "focus" });
    view.rerender(<List rows={["a", "b", "r1", "r2", "r3", "r4", "between", ...ids(8, 5)]} />);

    expect(viewportTop("r3")).toBe(focusedTop);
    expect(viewportTop("r5")).toBe((hoveredTop ?? 0) + ROW_HEIGHT);
    expect(focused).toHaveFocus();
  } finally {
    view.unmount();
  }
});

test("the row under the pointer is the current node ahead of the centre, and is released when the pointer leaves it", async () => {
  const user = userEvent.setup();
  const view = render(<List rows={ids(12)} />);
  try {
    setScrollY(100);
    const hovered = document.querySelector<HTMLElement>('[data-row="r2"]');
    if (hovered === null) throw new Error("r2 missing");
    await user.hover(hovered);
    const hoveredTop = viewportTop("r2");
    const centreTop = viewportTop("r7");

    // One row above r2, one between r2 and the centre row r7.
    expect(chosen()).toEqual({ id: "r2", via: "pointer" });
    view.rerender(<List rows={["a", "r1", "r2", "r3", "between", ...ids(9, 4)]} />);
    expect(viewportTop("r2")).toBe(hoveredTop);
    expect(viewportTop("r7")).toBe((centreTop ?? 0) + ROW_HEIGHT);

    await user.unhover(hovered);
    expect(chosen()?.via).toBe("center");
  } finally {
    view.unmount();
  }
});

test("the pointer moving onto the card a row opened keeps that row the current one, until the card closes", async () => {
  const user = userEvent.setup();
  const view = render(<List rows={ids(12)} />);
  // The one hover card (`features/refs/RefPreview.tsx`): `fixed` over the page and portaled into
  // the body, so it is never inside the list whose row opened it.
  const card = document.createElement("div");
  card.id = "ref-preview";
  document.body.append(card);
  try {
    setScrollY(100);
    const hovered = document.querySelector<HTMLElement>('[data-row="r2"]');
    const reference = hovered?.querySelector("button") ?? null;
    if (hovered === null || reference === null) throw new Error("r2 missing");
    await user.hover(reference);
    expect(chosen()).toEqual({ id: "r2", via: "pointer" });

    // The card opened over the resting pointer. `RefPreview` names its reference by
    // `aria-describedby` for as long as it is shown, which is how the row is known to own it.
    reference.setAttribute("aria-describedby", "ref-preview");
    const hoveredTop = viewportTop("r2");
    // user-event's unhover can set no relatedTarget, which is what names the card the pointer went to.
    fireEvent.pointerOut(reference, { relatedTarget: card });

    // A row arriving above r2 while the pointer reads r2's card leaves r2 where it was.
    expect(chosen()).toEqual({ id: "r2", via: "pointer" });
    view.rerender(<List rows={["a", ...ids(12)]} />);
    expect(viewportTop("r2")).toBe(hoveredTop);

    // The pointer left the card for somewhere else, and the card closed itself: the hand is off
    // the row, and the centre has it again.
    reference.removeAttribute("aria-describedby");
    card.remove();
    expect(chosen()?.via).toBe("center");
  } finally {
    view.unmount();
    card.remove();
  }
});

test("a disabled anchor measures nothing and leaves the scroll alone", () => {
  const view = render(<List enabled={() => false} rows={ids(12)} />);
  try {
    setScrollY(100);
    const rect = spyOn(Element.prototype, "getBoundingClientRect");
    view.rerender(<List enabled={() => false} rows={["new-a", ...ids(12)]} />);
    expect(rect.mock.calls.length).toBe(0);
    expect(window.scrollY).toBe(100);
  } finally {
    view.unmount();
  }
});

test("a current node that left the list scrolls nothing", () => {
  const view = render(<List rows={ids(12)} />);
  try {
    setScrollY(100);
    expect(chosen()).toEqual({ id: "r7", via: "center" });
    view.rerender(<List rows={ids(12).filter((id) => id !== "r7")} />);
    expect(window.scrollY).toBe(100);
  } finally {
    view.unmount();
  }
});

test("a current node that changed group was relocated, not shifted: it is not followed and the view stays", () => {
  const view = render(<List rows={ids(12)} />);
  try {
    setScrollY(100);
    expect(chosen()).toEqual({ id: "r7", via: "center" });
    // r7 leaves its section for the end of the list.
    view.rerender(
      <List groups={{ r7: "other" }} rows={[...ids(12).filter((id) => id !== "r7"), "r7"]} />
    );
    expect(window.scrollY).toBe(100);
    expect(viewportTop("r8")).toBe(LIST_TOP + 6 * ROW_HEIGHT - 100);
  } finally {
    view.unmount();
  }
});

test("a current node reordered within its group is still followed", () => {
  const view = render(<List rows={ids(12)} />);
  try {
    setScrollY(100);
    const before = viewportTop("r7");
    expect(chosen()).toEqual({ id: "r7", via: "center" });
    // r7 is promoted two places up its section.
    view.rerender(<List rows={[...ids(4), "r7", "r5", "r6", ...ids(5, 8)]} />);
    expect(viewportTop("r7")).toBe(before);
    expect(window.scrollY).toBe(0);
  } finally {
    view.unmount();
  }
});

test("rows scrolling in an element are pinned by that element's scrollTop, and the window stays", () => {
  const view = render(<List rows={ids(12)} scroller={scrollerRef} />);
  try {
    const scroller = scrollerRef.current;
    if (scroller === null) throw new Error("scroller missing");
    scroller.scrollTop = 100;
    // The same geometry as the window case: the centre of the scroller's box is r7.
    const before = viewportTop("r7");
    expect(chosen()).toEqual({ id: "r7", via: "center" });
    view.rerender(<List rows={["new-a", "new-b", ...ids(12)]} scroller={scrollerRef} />);
    expect(viewportTop("r7")).toBe(before);
    expect(scroller.scrollTop).toBe(200);
    expect(window.scrollY).toBe(0);
  } finally {
    view.unmount();
  }
});

// The Inbox drawer's defect: the row above the hand shrank inside its own card, a commit this
// anchor never sees, and the browser re-dispatched the boundary events at the pointer's own
// unchanged position. Reading the row's place again there would take the drift for the place
// to keep, and the next commit would pin the row where it had already slid to.
test("a row moving under a pointer that has not moved does not become the place it is kept at", () => {
  const view = render(<List rows={ids(12)} scroller={scrollerRef} />);
  try {
    const scroller = scrollerRef.current;
    if (scroller === null) throw new Error("scroller missing");
    scroller.scrollTop = 100;
    const hovered = document.querySelector<HTMLElement>('[data-row="r4"]');
    if (hovered === null) throw new Error("r4 missing");
    // user-event's hover sends no clientX/clientY of its own, and the pointer's place is what this row measures.
    fireEvent.pointerOver(hovered, { clientX: 20, clientY: 250 });
    const held = viewportTop("r4");

    // Content above the hand's row loses 50 px with no commit of this anchor's, so the row
    // slides up under a pointer that has not moved, and the browser says so again. The box's
    // own scrollTop does not move: the content shrank, the reader did not scroll.
    listTop -= ROW_HEIGHT;
    expect(viewportTop("r4")).toBe((held ?? 0) - ROW_HEIGHT);
    // The browser's own re-dispatch at the unchanged position, which user-event has no form for.
    fireEvent.pointerOver(hovered, { clientX: 20, clientY: 250 });

    view.rerender(<List rows={["a", ...ids(12)]} scroller={scrollerRef} />);
    expect(viewportTop("r4")).toBe(held);
  } finally {
    view.unmount();
  }
});

// A commit landing between a scroll and the scroll event it raises: following a reference from
// a Conversation card scrolls the page, and the thread-open commit arrives before the event
// does. Where the document scrolls the rows nothing is kept for that commit to restore, so the
// reader stays where they put themselves.
test("a window scroll whose event has not arrived yet is not undone by the next commit", () => {
  const view = render(<List rows={ids(12)} />);
  try {
    setScrollY(100);
    const hovered = document.querySelector<HTMLElement>('[data-row="r4"]');
    if (hovered === null) throw new Error("r4 missing");
    // user-event's hover sends no clientX/clientY of its own, and the pointer's place is what this row measures.
    fireEvent.pointerOver(hovered, { clientX: 20, clientY: 250 });

    // The page moved to the reference's target and the `scroll` event has not run yet.
    setScrollY(250);
    const at = viewportTop("r4");

    // A row arrives below the hand's row, which leaves that row where it is.
    view.rerender(<List rows={[...ids(4), "new", ...ids(8, 5)]} />);
    expect(window.scrollY).toBe(250);
    expect(viewportTop("r4")).toBe(at);
  } finally {
    view.unmount();
  }
});

// The reader gave the row under their hand another priority while the Inbox is grouped by it:
// the row left its section in an earlier commit, and the place kept for it is where it used to
// sit. A row that changed group was relocated, not shifted, so the view does not follow it.
test("a held row that changed group is not followed to the section it moved to", () => {
  const view = render(<List rows={ids(12)} scroller={scrollerRef} />);
  try {
    const scroller = scrollerRef.current;
    if (scroller === null) throw new Error("scroller missing");
    scroller.scrollTop = 100;
    const hovered = document.querySelector<HTMLElement>('[data-row="r4"]');
    if (hovered === null) throw new Error("r4 missing");
    // user-event's hover sends no clientX/clientY of its own, and the pointer's place is what this row measures.
    fireEvent.pointerOver(hovered, { clientX: 20, clientY: 250 });

    // r4 leaves its section for the end of the list. The commit that moves it finds it in a
    // group it was not measured in, and leaves the view alone.
    const moved = [...ids(12).filter((id) => id !== "r4"), "r4"];
    view.rerender(<List groups={{ r4: "other" }} rows={moved} scroller={scrollerRef} />);
    expect(scroller.scrollTop).toBe(100);

    // The next commit finds it in its new section, where the kept place is no longer its own.
    view.rerender(
      <List groups={{ r4: "other" }} rows={[...moved, "r13"]} scroller={scrollerRef} />
    );
    expect(scroller.scrollTop).toBe(100);
    expect(viewportTop("r4")).toBe(LIST_TOP + 11 * ROW_HEIGHT - 100);
  } finally {
    view.unmount();
  }
});

// A long entry above the reader is cut, in no commit of this anchor's, by more than the box
// holds below the view. The browser pulls the box's scrollTop back to its new end and says so
// with a scroll event. That is not the reader scrolling: keeping the row's place there would
// leave the row as far out of place as the room that was below the view.
test("a box the browser pulls back to its end after its content shrank keeps the held row in place", () => {
  // happy-dom's ResizeObserver never calls back: this one is called back where the case says
  // the content changed height.
  const RealResizeObserver = globalThis.ResizeObserver;
  const observers: ResizeObserverCallback[] = [];
  globalThis.ResizeObserver = class {
    constructor(callback: ResizeObserverCallback) {
      observers.push(callback);
    }
    disconnect() {}
    observe() {}
    unobserve() {}
  } as unknown as typeof ResizeObserver;
  const resized = () => {
    for (const callback of observers) callback([], undefined as unknown as ResizeObserver);
  };
  // The long entry, still whole, is the content above the rows.
  listTop = 400;
  const view = render(<List rows={ids(40)} scroller={scrollerRef} />);
  try {
    const scroller = scrollerRef.current;
    if (scroller === null) throw new Error("scroller missing");
    // happy-dom lays nothing out: the box is the window's height, and its content runs from the
    // box's top to the last row's foot.
    Object.defineProperty(scroller, "clientHeight", {
      configurable: true,
      value: VIEWPORT_HEIGHT,
    });
    Object.defineProperty(scroller, "scrollHeight", {
      configurable: true,
      get: () => listTop + rows().length * ROW_HEIGHT,
    });
    const focused = controlOf("r34");
    focused.focus();
    // The reader scrolls to 50 px short of the box's end (2,400 - 600 = 1,800).
    scroller.scrollTop = 1750;
    fireEvent.scroll(scroller);
    const held = viewportTop("r34");
    expect(held).toBe(300);

    // The entry is cut by 300 px, six times the room below the view, and the browser pulls the
    // box back to its new end. That frame's scroll event runs before its resize observations.
    listTop -= 300;
    scroller.scrollTop = scroller.scrollHeight - scroller.clientHeight;
    expect(scroller.scrollTop).toBe(1500);
    fireEvent.scroll(scroller);
    resized();
    expect(viewportTop("r34")).toBe(held);
    // The scroll event of the anchor's own put-back.
    fireEvent.scroll(scroller);

    // A later change of the content's height still holds the row there.
    listTop -= ROW_HEIGHT;
    resized();
    expect(viewportTop("r34")).toBe(held);
  } finally {
    view.unmount();
    globalThis.ResizeObserver = RealResizeObserver;
  }
});

/**
 * Draws `drawn` in a scroller that keeps its offset as the browser's box does: VIEWPORT_HEIGHT
 * tall, its content running from its top to the last row's foot, and a `scrollTop` that stays
 * between its top and its end. happy-dom keeps any offset it is given, lays nothing out, and
 * never calls a ResizeObserver back, so `settle` stands in for the browser's frames after a
 * change. In each frame the box is pulled back to its end where its content no longer reaches
 * that far, one scroll event is raised for whatever moved it since the frame before, and the
 * root's resize is observed where the list's height changed.
 * With `ownsScrollAnchoring`, the host takes the box over.
 */
function drawInBox(drawn: string[], { ownsScrollAnchoring = false } = {}) {
  const RealResizeObserver = globalThis.ResizeObserver;
  const observers: ResizeObserverCallback[] = [];
  globalThis.ResizeObserver = class {
    constructor(callback: ResizeObserverCallback) {
      observers.push(callback);
    }
    disconnect() {}
    observe() {}
    unobserve() {}
  } as unknown as typeof ResizeObserver;
  const view = render(
    <List ownsScrollAnchoring={ownsScrollAnchoring} rows={drawn} scroller={scrollerRef} />
  );
  const scroller = scrollerRef.current;
  if (scroller === null) throw new Error("scroller missing");
  const end = () => Math.max(0, listTop + listHeight() - VIEWPORT_HEIGHT);
  let offset = 0;
  let moved = false;
  let observed = listHeight();
  Object.defineProperty(scroller, "clientHeight", { configurable: true, value: VIEWPORT_HEIGHT });
  Object.defineProperty(scroller, "scrollHeight", {
    configurable: true,
    get: () => listTop + listHeight(),
  });
  Object.defineProperty(scroller, "scrollTop", {
    configurable: true,
    get: () => offset,
    set: (value: number) => {
      const next = Math.max(0, Math.min(value, end()));
      if (next !== offset) moved = true;
      offset = next;
    },
  });
  const frame = () => {
    // The pull-back: the setter keeps the offset within the content as it is now.
    scroller.scrollTop = offset;
    if (moved) {
      moved = false;
      fireEvent.scroll(scroller);
    }
    if (listHeight() !== observed) {
      observed = listHeight();
      for (const callback of observers) callback([], undefined as unknown as ResizeObserver);
    }
  };
  return {
    rerender: (next: string[]) =>
      view.rerender(
        <List ownsScrollAnchoring={ownsScrollAnchoring} rows={next} scroller={scrollerRef} />
      ),
    scroller,
    settle: () => {
      frame();
      // The scroll event of a put-back the anchor made in that frame.
      if (moved) frame();
    },
    unmount: () => {
      view.unmount();
      globalThis.ResizeObserver = RealResizeObserver;
    },
  };
}

// A control at the foot of a long row holds the focus while the row's head is above the view,
// as a Reply under an agents' turn still drawn whole does. The row is then cut short by a
// commit of its own, which the anchor sees only as its root resized: that moves the control up
// by the cut and leaves the row's top where it was. The reader is on the control, so the
// control is what stays.
test("a focused control at the foot of a long row keeps its place when the row's own content above it shrinks", () => {
  heights = { r3: 1000 };
  const box = drawInBox(ids(40));
  try {
    const focused = controlOf("r3");
    focused.focus();
    // The reader scrolls to the control: r3 runs from 200 to 1,200 px down the content, and its
    // control stands at its foot.
    box.scroller.scrollTop = 1000;
    box.settle();
    expect(viewportTop("r3")).toBe(-800);
    const held = controlTop("r3");
    expect(held).toBe(180);

    // r3 is cut to 200 px, which raises its control by 800 px.
    heights = { r3: 200 };
    box.settle();
    expect(controlTop("r3")).toBe(held);
    expect(box.scroller.scrollTop).toBe(200);

    // A commit that adds a row above holds r3 at the top that puts its control back, so the
    // control stays there too.
    box.rerender(["new", ...ids(40)]);
    expect(controlTop("r3")).toBe(held);
    box.settle();
    expect(controlTop("r3")).toBe(held);
  } finally {
    box.unmount();
  }
});

// The reader is at the very end of the box when content above their row shrinks outside a
// commit: the browser pulls the box back by the whole shrink, which leaves the row where it
// was. The place is kept at the offset the box was left at, so when the content above grows
// again, with the box no longer at its end, the row still stays.
test("a box pulled back to its end by the whole shrink above the row keeps the row's place for the next change", () => {
  heights = { r2: 150 };
  const box = drawInBox(ids(40));
  try {
    // The reader scrolls to the box's end (2,200 - 600 = 1,600 px) and rests the pointer on r34.
    box.scroller.scrollTop = 1600;
    box.settle();
    const hovered = document.querySelector<HTMLElement>('[data-row="r34"]');
    if (hovered === null) throw new Error("r34 missing");
    // user-event's hover sends no clientX/clientY of its own, and the pointer's place is what this row measures.
    fireEvent.pointerOver(hovered, { clientX: 20, clientY: 270 });
    expect(viewportTop("r34")).toBe(250);

    // r2 is cut by 100 px, and the browser pulls the box back by as much, to its new end.
    heights = {};
    box.settle();
    expect(box.scroller.scrollTop).toBe(1500);
    expect(viewportTop("r34")).toBe(250);

    // r2 grows back by those 100 px, in no commit of the anchor's.
    heights = { r2: 150 };
    box.settle();
    expect(viewportTop("r34")).toBe(250);
  } finally {
    box.unmount();
  }
});

// Content below the reader's row shrinks outside a commit by more than the room below the view:
// the browser pulls the box back to its new end, which moves the row down, and no scroll can
// put it back. Where the pull-back left the row is where the reader now sees it, and the place
// kept from then on, so a later growth below, which gives the box room again, leaves it there.
// The focus is on a control at the foot of that row, and its own place is kept from there too.
test("a box pulled back past the room below the row keeps the row where the pull-back left it, and the focused control's own place from there", () => {
  heights = { r18: 200, r20: 350 };
  const box = drawInBox(ids(20));
  try {
    const focused = controlOf("r18");
    focused.focus();
    // The reader scrolls to 100 px short of the box's end (1,550 - 600 = 950 px).
    box.scroller.scrollTop = 850;
    box.settle();
    expect(viewportTop("r18")).toBe(100);
    expect(controlTop("r18")).toBe(280);

    // r20 is cut by 300 px, three times the room below the view: the browser pulls the box back
    // 200 px to its new end, which moves r18 down by as much.
    heights = { r18: 200 };
    box.settle();
    expect(box.scroller.scrollTop).toBe(650);
    expect(viewportTop("r18")).toBe(300);
    expect(controlTop("r18")).toBe(480);

    // r20 grows back, which gives the box room below the view again.
    heights = { r18: 200, r20: 350 };
    box.settle();
    expect(viewportTop("r18")).toBe(300);
    expect(controlTop("r18")).toBe(480);

    // r18's own content above its control is cut by 100 px: the control stays where it was.
    heights = { r18: 100, r20: 350 };
    box.settle();
    expect(controlTop("r18")).toBe(480);
  } finally {
    box.unmount();
  }
});

// A control that the focus is moved to inside a row, by code and with no scroll, as a page's
// hand-on or Details' focus keeper moves it, is the reader's place from then on: a later change of
// that row's height keeps the control where it was. The anchor records a focused control only when
// the reader scrolls or moves the pointer, so a focus moved by code alone is not kept, and a cut
// of the row then moves the control with it. Found by the tester on a running build of 685fbb25
// with a copy of a large real issue: Load older pressed as the focus reached it put the focus on a
// turn's Reply at 270 px. The history raises a scroll event to have that place kept, but in that
// commit the anchor's own scroll had moved the box from 29,643 back to 30,372 px, the last
// frame's offset, so the anchor took the history's event for its own and no browser scroll event
// followed; the turn's cut from 510 to 190 px left the Reply at -50 px, above the view, in three
// runs of ten at 1440 px.
test("a control the focus is moved to by code inside a row keeps its place when that row is cut", () => {
  const RealResizeObserver = globalThis.ResizeObserver;
  const observers: ResizeObserverCallback[] = [];
  globalThis.ResizeObserver = class {
    constructor(callback: ResizeObserverCallback) {
      observers.push(callback);
    }
    disconnect() {}
    observe() {}
    unobserve() {}
  } as unknown as typeof ResizeObserver;
  const resized = () => {
    for (const callback of observers) callback([], undefined as unknown as ResizeObserver);
  };
  // r20 is a long turn: from 1,050 to 2,050 px down the content, its control at its foot.
  heights = { r20: 1000 };
  const view = render(<List rows={ids(30)} scroller={scrollerRef} />);
  try {
    const scroller = scrollerRef.current;
    if (scroller === null) throw new Error("scroller missing");
    Object.defineProperty(scroller, "clientHeight", { configurable: true, value: VIEWPORT_HEIGHT });
    Object.defineProperty(scroller, "scrollHeight", {
      configurable: true,
      get: () => listTop + listHeight(),
    });
    // The reader's view: r20's head above it, its control at 400 px.
    scroller.scrollTop = 1630;
    fireEvent.scroll(scroller);
    const control = controlOf("r20");
    // The focus is moved to r20's control by code, with no scroll.
    control.focus({ preventScroll: true });
    expect(document.activeElement).toBe(control);
    const held = controlTop("r20");
    expect(held).toBe(400);

    // r20 is cut to 190 px in a commit of its own, which the anchor sees as its root resized.
    heights = { r20: 190 };
    resized();
    expect(controlTop("r20")).toBe(held);
  } finally {
    view.unmount();
    globalThis.ResizeObserver = RealResizeObserver;
  }
});

// A press of the reader's on a control in a long row is their own act on that row, as "more" at
// the foot of a cut turn draws the rest of the turn's words between its head and itself: the row
// opens below its head, which stays where the reader saw it, and the control moves down with what
// the press drew. Keeping the focused control's own place there instead pushed the head up by all
// of it, out of the view. Only a change the reader did not make keeps the control's own place.
test("a key pressed on a focused control in a long row lets the row open below its head", async () => {
  const user = userEvent.setup();
  heights = { r3: 400 };
  const box = drawInBox(ids(40));
  try {
    const focused = controlOf("r3");
    focused.focus();
    // The reader scrolls to the row: r3 runs from 200 to 600 px down the content, and its control
    // stands at its foot.
    box.scroller.scrollTop = 100;
    box.settle();
    expect(viewportTop("r3")).toBe(100);
    expect(controlTop("r3")).toBe(480);

    // Enter on the control opens the row by 800 px above the control.
    await user.keyboard("{Enter}");
    heights = { r3: 1200 };
    box.settle();
    expect(viewportTop("r3")).toBe(100);
    expect(controlTop("r3")).toBe(1280);
  } finally {
    box.unmount();
  }
});

// The same press made with the pointer, whether it goes down on the control that holds the focus
// or gives the control the focus: the focus a press gives is that press's own, and no place to
// keep the control at.
test("a pointer pressed on a control in a long row lets the row open below its head, the focus it gives included", async () => {
  const user = userEvent.setup();
  heights = { r3: 400 };
  const box = drawInBox(ids(40));
  try {
    const control = controlOf("r3");
    control.focus();
    box.scroller.scrollTop = 100;
    box.settle();
    expect(viewportTop("r3")).toBe(100);
    expect(controlTop("r3")).toBe(480);

    // A press on the control that holds the focus opens the row by 800 px above the control.
    await user.click(control);
    heights = { r3: 1200 };
    box.settle();
    expect(viewportTop("r3")).toBe(100);
    expect(controlTop("r3")).toBe(1280);

    // The row closes and the focus leaves it; the next press gives the control the focus as it
    // opens the row again.
    heights = { r3: 400 };
    box.settle();
    control.blur();
    await user.click(control);
    expect(document.activeElement).toBe(control);
    heights = { r3: 1200 };
    box.settle();
    expect(viewportTop("r3")).toBe(100);
    expect(controlTop("r3")).toBe(1280);
  } finally {
    box.unmount();
  }
});

// A pointer press that gives the root no focus, as a press in a browser that does not focus a
// pressed button does, is over once it has ended. A focus the control it went down on gets
// later, the reader tabbing to it or code handing it the focus, is not that press's own, and the
// control is the reader's place from then on. A press kept until the next focus took that focus
// for its own and let the control go, and a cut of the row's own content above the control then
// moved it.
test("a control focused after a press that gave it no focus keeps its place when its row's own content above it shrinks", async () => {
  heights = { r3: 1000 };
  const box = drawInBox(ids(40));
  try {
    const control = controlOf("r3");
    // The reader scrolls to the control: r3 runs from 200 to 1,200 px down the content, and its
    // control stands at its foot.
    box.scroller.scrollTop = 1000;
    box.settle();
    expect(viewportTop("r3")).toBe(-800);
    expect(controlTop("r3")).toBe(180);

    // A press on the control that gives it no focus, and the task queued as the press ended.
    fireEvent.pointerDown(control);
    fireEvent.pointerUp(control);
    expect(document.activeElement).not.toBe(control);
    const taskRan = Promise.withResolvers<void>();
    window.setTimeout(taskRan.resolve, 0);
    await taskRan.promise;

    // Code hands the control the focus.
    control.focus();
    expect(document.activeElement).toBe(control);
    const held = controlTop("r3");
    expect(held).toBe(180);

    // r3 is cut to 200 px in a commit of its own, which raises its control by 800 px.
    heights = { r3: 200 };
    box.settle();
    expect(controlTop("r3")).toBe(held);
  } finally {
    box.unmount();
  }
});

test("the centre anchor stays inside a horizontally scrolled table's visible box", () => {
  contentLeft = -800;
  contentWidth = 2000;
  const view = render(<List rows={ids(12)} scroller={scrollerRef} />);
  try {
    const scroller = scrollerRef.current;
    if (!scroller) throw new Error("No scroller");
    scroller.scrollTop = 100;
    expect(chosen()).toEqual({ id: "r7", via: "center" });
    const before = viewportTop("r7");
    view.rerender(<List rows={["new", ...ids(12)]} scroller={scrollerRef} />);
    expect(viewportTop("r7")).toBe(before);
  } finally {
    view.unmount();
  }
});

/**
 * Draws `drawn` in a scroller whose frames the case runs by hand, for the order of events within
 * one frame: the case moves the box and sends each scroll event the frame raises, `resized` calls
 * the root's size observers back as the frame's resize observations would, and `frameEnds` runs
 * the frames asked for so far, as the browser runs a frame's animation-frame callbacks after its
 * scroll steps. happy-dom keeps any offset it is given and raises no scroll event for it, runs
 * frame requests on a timer of its own, and never calls a ResizeObserver back. The box is
 * VIEWPORT_HEIGHT tall, its content running from its top to the last row's foot.
 * With `ownsScrollAnchoring`, the host takes the box over.
 */
function drawFrameByFrame(drawn: string[], { ownsScrollAnchoring = false } = {}) {
  const RealResizeObserver = globalThis.ResizeObserver;
  const observers: ResizeObserverCallback[] = [];
  globalThis.ResizeObserver = class {
    constructor(callback: ResizeObserverCallback) {
      observers.push(callback);
    }
    disconnect() {}
    observe() {}
    unobserve() {}
  } as unknown as typeof ResizeObserver;
  const asked = new Map<number, FrameRequestCallback>();
  let last = 0;
  const request = spyOn(window, "requestAnimationFrame").mockImplementation((callback) => {
    last += 1;
    asked.set(last, callback);
    return last;
  });
  const cancel = spyOn(window, "cancelAnimationFrame").mockImplementation((frame) => {
    asked.delete(frame);
  });
  restoreSpies.push(
    () => request.mockRestore(),
    () => cancel.mockRestore()
  );
  const view = render(
    <List ownsScrollAnchoring={ownsScrollAnchoring} rows={drawn} scroller={scrollerRef} />
  );
  const scroller = scrollerRef.current;
  if (scroller === null) throw new Error("scroller missing");
  Object.defineProperty(scroller, "clientHeight", { configurable: true, value: VIEWPORT_HEIGHT });
  Object.defineProperty(scroller, "scrollHeight", {
    configurable: true,
    get: () => listTop + listHeight(),
  });
  return {
    frameEnds: () => {
      const due = [...asked.values()];
      asked.clear();
      for (const callback of due) callback(performance.now());
    },
    rerender: (next: string[]) =>
      view.rerender(
        <List ownsScrollAnchoring={ownsScrollAnchoring} rows={next} scroller={scrollerRef} />
      ),
    resized: () => {
      for (const callback of observers) callback([], undefined as unknown as ResizeObserver);
    },
    scroller,
    unmount: () => {
      view.unmount();
      globalThis.ResizeObserver = RealResizeObserver;
    },
  };
}

/** The reader's view in the cases below: the box scrolled to 300 px by the reader, and the focus
 *  on r10's control, r10 standing 250 px down the view and its control 280 px down. */
function readerOnR10(scroller: HTMLElement): void {
  scroller.scrollTop = 300;
  fireEvent.scroll(scroller);
  const control = controlOf("r10");
  control.focus();
  expect(viewportTop("r10")).toBe(250);
  expect(controlTop("r10")).toBe(280);
}

// The focus is on a control in the view, and a commit adds a row above it, which the anchor
// answers by scrolling the box down by the row. In the same frame another write puts the box back
// where it was, so the frame moves nothing and the browser raises no scroll event for the anchor's
// scroll. The reader then scrolls. A mark the anchor kept for "the next scroll event" took the
// reader's event for its own, and the place they scrolled to was never recorded: content above
// cut afterwards moved the control away from where they saw it.
test("the anchor's own scroll put back in its frame leaves the reader's next scroll the reader's", () => {
  const box = drawFrameByFrame(ids(30));
  try {
    readerOnR10(box.scroller);
    // A row arrives above r10, and the anchor scrolls the box down by it.
    box.rerender(["new", ...ids(30)]);
    expect(box.scroller.scrollTop).toBe(350);
    expect(viewportTop("r10")).toBe(250);
    // Another write in the same frame puts the box back: the frame moves nothing, raises no scroll
    // event, and ends.
    box.scroller.scrollTop = 300;
    box.frameEnds();

    // The reader scrolls down, and the browser says so.
    box.scroller.scrollTop = 500;
    fireEvent.scroll(box.scroller);
    const seen = controlTop("r10");
    expect(seen).toBe(130);

    // Content above the rows is cut by a row's height in no commit of the anchor's.
    listTop -= ROW_HEIGHT;
    box.resized();
    expect(controlTop("r10")).toBe(seen);
  } finally {
    box.unmount();
  }
});

// Two scrolls of the anchor's own reach one frame: a put-back in the resize observations at the
// end of one frame, and a commit's before the next. That next frame raises one scroll event for
// the box, at the offset the second scroll left, and the event is the anchor's own: it records
// nothing. Here the reader has pressed a key on the focused control, which lets the control go and
// keeps the row's head; recording the place at that event would keep the control again, and the
// row then opening below its head would push the head up out of the view.
test("two scrolls of the anchor's own in one frame raise one event at the last offset, which records nothing", async () => {
  const user = userEvent.setup();
  const box = drawFrameByFrame(ids(30));
  try {
    readerOnR10(box.scroller);
    await user.keyboard("{Enter}");

    // Content above the rows grows by 30 px in no commit of the anchor's, and the anchor scrolls
    // the box down by as much.
    listTop += 30;
    box.resized();
    expect(box.scroller.scrollTop).toBe(330);
    // A row arrives above r10 before the next frame, and the anchor scrolls the box down by it.
    box.rerender(["new", ...ids(30)]);
    expect(box.scroller.scrollTop).toBe(380);
    // The next frame raises one scroll event for both, at the offset the second left, and ends.
    fireEvent.scroll(box.scroller);
    box.frameEnds();
    expect(viewportTop("r10")).toBe(250);

    // r10 opens by 400 px above its control, as "more" pressed on it draws a turn's words.
    heights = { r10: 450 };
    box.resized();
    expect(viewportTop("r10")).toBe(250);
    expect(controlTop("r10")).toBe(680);
  } finally {
    box.unmount();
  }
});

// The reader scrolls in the frame that reports the anchor's own scroll, before the browser raises
// that frame's scroll event: the frame raises one event, at the offset the reader left the box at.
// That event is the reader's, and the place they scrolled to is recorded.
test("the reader scrolling after the anchor's own scroll and before its event makes that event the reader's", () => {
  const box = drawFrameByFrame(ids(30));
  try {
    readerOnR10(box.scroller);
    // A row arrives above r10, and the anchor scrolls the box down by it.
    box.rerender(["new", ...ids(30)]);
    expect(box.scroller.scrollTop).toBe(350);
    // The reader scrolls down before the frame's scroll event, which comes at their offset.
    box.scroller.scrollTop = 500;
    fireEvent.scroll(box.scroller);
    box.frameEnds();
    const seen = controlTop("r10");
    expect(seen).toBe(130);

    // Content above the rows is cut by a row's height in no commit of the anchor's.
    listTop -= ROW_HEIGHT;
    box.resized();
    expect(controlTop("r10")).toBe(seen);
  } finally {
    box.unmount();
  }
});

// The anchor's own scroll is put back in its frame, which raises no event, and the frame ends.
// Content above the rows is then cut while the box is off the place kept for the control, so the
// cut moves the control. The reader then scrolls to the very offset the anchor's scroll had left:
// that scroll is the reader's, and the place kept from then on is where they see the control now,
// not the one kept before the cut.
test("once the frame of the anchor's own scroll has ended, the reader scrolling to the offset it left is the reader's", () => {
  const box = drawFrameByFrame(ids(30));
  try {
    readerOnR10(box.scroller);
    // A row arrives above r10, and the anchor scrolls the box down by it.
    box.rerender(["new", ...ids(30)]);
    expect(box.scroller.scrollTop).toBe(350);
    // Another write in the same frame puts the box back: the frame moves nothing, raises no scroll
    // event, and ends.
    box.scroller.scrollTop = 300;
    box.frameEnds();

    // Content above the rows is cut by a row's height while the box is off the kept place.
    listTop -= ROW_HEIGHT;
    box.resized();
    expect(controlTop("r10")).toBe(280);

    // The reader scrolls to the offset the anchor's own scroll left, and the browser says so.
    box.scroller.scrollTop = 350;
    fireEvent.scroll(box.scroller);
    const seen = controlTop("r10");
    expect(seen).toBe(230);

    // Content above the rows is cut by a row's height again.
    listTop -= ROW_HEIGHT;
    box.resized();
    expect(controlTop("r10")).toBe(seen);
  } finally {
    box.unmount();
  }
});

// While a place is kept in the box the host named, the anchor alone keeps that box's scroll: the
// browser's own scroll anchoring is turned off there, and comes back as the box's own style had
// it once no place is kept. The browser anchors on a node of its own choosing near the top of the
// view, not on the reader's row. As the page's typeface arrived after Load older landed, the turns
// laid out again in it moved that node and the focused control apart: the browser scrolled
// Details by 39,180 px to keep its node, the control the page had handed the focus to went
// 1,492 px up, and the anchor, which had not made that scroll, put nothing back. A pull-back of
// the box to its end then left the control 364 px above the view.
test("a place kept in a named box turns the box's own scroll anchoring off until no place is kept", () => {
  const view = render(<List ownsScrollAnchoring rows={ids(12)} scroller={scrollerRef} />);
  try {
    const scroller = scrollerRef.current;
    if (scroller === null) throw new Error("scroller missing");
    scroller.style.setProperty("overflow-anchor", "auto");
    const row = document.querySelector<HTMLElement>('[data-row="r3"]');
    if (row === null) throw new Error("r3 missing");
    // user-event's hover sends no clientX/clientY of its own, and the pointer's place is what this row measures.
    fireEvent.pointerOver(row, { clientX: 20, clientY: 220 });
    // The hand's own move onto the row, which the browser raises after its boundary event and
    // which is what records the place where the host takes its box over; user-event sets no point.
    fireEvent.pointerMove(row, { clientX: 20, clientY: 220 });
    expect(chosen()).toEqual({ id: "r3", via: "pointer" });
    expect(scroller.style.getPropertyValue("overflow-anchor")).toBe("none");

    // The pointer leaves the rows, and with it the place.
    fireEvent.pointerOut(row, { clientX: 20, clientY: 900, relatedTarget: document.body });
    expect(chosen()).toBeNull();
    expect(scroller.style.getPropertyValue("overflow-anchor")).toBe("auto");
  } finally {
    view.unmount();
  }
});

// With its own anchoring off, the browser keeps the view in that box for no change, and content of
// the box outside the rows, a section above them, can change height too: while a place is kept,
// the anchor watches the box's other children as it watches its own root, and stops once none is.
test("while a place is kept in a named box, the anchor watches the box's other children change height", () => {
  const RealResizeObserver = globalThis.ResizeObserver;
  const watched = new Set<Element>();
  globalThis.ResizeObserver = class {
    disconnect() {
      watched.clear();
    }
    observe(target: Element) {
      watched.add(target);
    }
    unobserve(target: Element) {
      watched.delete(target);
    }
  } as unknown as typeof ResizeObserver;
  const view = render(
    <div data-scroller="" ref={scrollerRef}>
      <section data-above="">Above the rows</section>
      <ViewportAnchor item="data-row" ownsScrollAnchoring ref={anchor} scroller={scrollerRef}>
        {ids(12).map((id) => (
          <div data-row={id} key={id}>
            {id}
          </div>
        ))}
      </ViewportAnchor>
    </div>
  );
  try {
    const above = document.querySelector("[data-above]");
    const root = scrollerRef.current?.lastElementChild ?? null;
    const row = document.querySelector<HTMLElement>('[data-row="r3"]');
    if (above === null || root === null || row === null) throw new Error("box missing");
    expect(watched.has(root)).toBe(true);
    expect(watched.has(above)).toBe(false);

    // user-event's hover sends no clientX/clientY of its own, and the pointer's place is what this row measures.
    fireEvent.pointerOver(row, { clientX: 20, clientY: 220 });
    // The hand's own move onto the row, which the browser raises after its boundary event and
    // which is what records the place where the host takes its box over; user-event sets no point.
    fireEvent.pointerMove(row, { clientX: 20, clientY: 220 });
    expect(watched.has(above)).toBe(true);

    // A pointerout to a relatedTarget outside the rows, at a place of the pointer's own, neither of which user-event's unhover can set.
    fireEvent.pointerOut(row, { clientX: 20, clientY: 900, relatedTarget: document.body });
    expect(watched.has(above)).toBe(false);
    expect(watched.has(root)).toBe(true);
  } finally {
    view.unmount();
    globalThis.ResizeObserver = RealResizeObserver;
  }
});

// A host that leaves its box to the browser's own scroll anchoring, as the Inbox's columns do,
// keeps that anchoring while a place is kept there. The reader scrolls and focuses a row in view,
// which is the kept place; content between that row and the view then changing height in a
// commit of its own is the browser's to answer, which holds the rows in view. Turned off for
// every host that names a box, the anchoring let the rows in view move by the change. Nor does
// the anchor watch the box's other children for such a host, while it still keeps the focused
// row's place as its own root changes height.
test("a host that does not take its box over keeps the box's own scroll anchoring while a focused row is kept, and watches no other child of the box", () => {
  const RealResizeObserver = globalThis.ResizeObserver;
  const watched = new Set<Element>();
  const observers: ResizeObserverCallback[] = [];
  globalThis.ResizeObserver = class {
    constructor(callback: ResizeObserverCallback) {
      observers.push(callback);
    }
    disconnect() {
      watched.clear();
    }
    observe(target: Element) {
      watched.add(target);
    }
    unobserve(target: Element) {
      watched.delete(target);
    }
  } as unknown as typeof ResizeObserver;
  const view = render(
    <div data-scroller="" ref={scrollerRef}>
      <section data-above="">Above the rows</section>
      <ViewportAnchor item="data-row" ref={anchor} scroller={scrollerRef}>
        {ids(40).map((id) => (
          <div data-row={id} key={id} tabIndex={-1}>
            {id}
          </div>
        ))}
      </ViewportAnchor>
    </div>
  );
  try {
    const scroller = scrollerRef.current;
    const above = document.querySelector("[data-above]");
    const root = scroller?.lastElementChild ?? null;
    if (scroller === null || above === null || root === null) throw new Error("box missing");
    scroller.style.setProperty("overflow-anchor", "auto");
    // The reader scrolls, and the browser says so with the box's scroll event, which user-event
    // has no form for; then they focus a row in the view.
    scroller.scrollTop = 1000;
    fireEvent.scroll(scroller);
    rowOf("r23").focus();
    expect(chosen()).toEqual({ id: "r23", via: "focus" });
    const kept = viewportTop("r23");
    expect(kept).toBe(200);
    expect(scroller.style.getPropertyValue("overflow-anchor")).toBe("auto");
    expect(watched.has(above)).toBe(false);
    expect(watched.has(root)).toBe(true);

    // r2 grows by a row's height in a commit of its own, which the anchor sees as its root
    // resized: the place is kept, and r23 goes back where the reader last saw it.
    heights = { r2: 2 * ROW_HEIGHT };
    for (const callback of observers) callback([], undefined as unknown as ResizeObserver);
    expect(viewportTop("r23")).toBe(kept);
    expect(scroller.style.getPropertyValue("overflow-anchor")).toBe("auto");
  } finally {
    view.unmount();
    globalThis.ResizeObserver = RealResizeObserver;
  }
});

// The box raises a scroll event for a change of height before the frame observes that change, and
// the anchor makes the change good at the event itself, before it reads it (`ownedScrolled`). As
// the page's typeface arrived after Load older landed, with the browser's own anchoring off in
// Details, the browser raised a scroll event for the box at the offset the place was kept at,
// before the frame observed the rows laid out again in it. Recording the place at that event kept
// the focused control where the new typeface had moved it, 38,600 px down, and the observation
// that followed found nothing to put back.
test("a scroll event raised before the frame observes a change of height makes the change good at that event, and the place holds", () => {
  const box = drawFrameByFrame(ids(30), { ownsScrollAnchoring: true });
  try {
    readerOnR10(box.scroller);
    // Content above the rows grows by 400 px in no commit of the anchor's, and the box raises a
    // scroll event at the offset it stood at, before the frame observes the root's new size.
    listTop += 400;
    expect(controlTop("r10")).toBe(680);
    fireEvent.scroll(box.scroller);
    expect(controlTop("r10"), "the scroll event left the change for a later observation").toBe(280);
    expect(box.scroller.scrollTop).toBe(700);
    box.resized();
    expect(controlTop("r10")).toBe(280);
    expect(box.scroller.scrollTop).toBe(700);
  } finally {
    box.unmount();
  }
});

/** The row `id` of the list drawn, which the case hands to `keepRow`. */
function rowOf(id: string): HTMLElement {
  const row = document.querySelector<HTMLElement>(`[data-row="${id}"]`);
  if (row === null) throw new Error(`${id} missing`);
  return row;
}

/** The control drawn inside the row `id` of the list. */
function controlOf(id: string): HTMLButtonElement {
  const control = rowOf(id).querySelector("button");
  if (control === null) throw new Error(`${id} button missing`);
  return control;
}

// A host keeps a row's place itself while a control of its own outside the list holds the focus,
// as Details' history keeps the entry next to Load older while the older page is read: no act of
// the reader's names a place then. The commit the page lands in draws rows under the kept row and
// cuts a long row above it. The row nearest the middle of the view, which the anchor would
// otherwise hold, is that long row, whose top the cut leaves where it was.
test("a row a host keeps holds its place across a commit that adds rows below it and shrinks a row above it", () => {
  // r3 is drawn 1,000 px tall, and the commit that draws r7 cuts it to 300 px.
  heights = {
    get r3() {
      return document.querySelector('[data-row="r7"]') === null ? 1000 : 300;
    },
  };
  const view = render(<List rows={ids(6)} scroller={scrollerRef} />);
  try {
    const scroller = scrollerRef.current;
    if (scroller === null) throw new Error("scroller missing");
    scroller.scrollTop = 800;
    expect(chosen()).toEqual({ id: "r3", via: "center" });
    anchor.current?.keepRow(rowOf("r5"));
    expect(chosen()).toEqual({ id: "r5", via: "kept" });
    const kept = viewportTop("r5");
    expect(kept).toBe(450);

    view.rerender(<List rows={ids(8)} scroller={scrollerRef} />);
    expect(viewportTop("r5")).toBe(kept);
    expect(scroller.scrollTop).toBe(100);
  } finally {
    view.unmount();
  }
});

// The reader's focus within a row is the reader's own place and comes before any row a host
// would keep: the host keeps none then. Once the host keeps a row, the focus moving into the list
// is an act of the reader's, which records their place instead.
test("a host keeps no row while the reader's focus is in one, and the focus moving into the list lets a kept row go", () => {
  const view = render(<List rows={ids(12)} scroller={scrollerRef} />);
  try {
    const scroller = scrollerRef.current;
    if (scroller === null) throw new Error("scroller missing");
    scroller.scrollTop = 100;
    const focused = controlOf("r4");
    const other = controlOf("r6");
    focused.focus();
    anchor.current?.keepRow(rowOf("r8"));
    expect(chosen()).toEqual({ id: "r4", via: "focus" });
    focused.blur();
    expect(chosen()).toEqual({ id: "r7", via: "center" });

    // With the focus outside the list the host keeps r8, and the focus moving into the list again
    // is the reader's place from then on: the host's goes.
    anchor.current?.keepRow(rowOf("r8"));
    expect(chosen()).toEqual({ id: "r8", via: "kept" });
    other.focus();
    expect(chosen()).toEqual({ id: "r6", via: "focus" });
    other.blur();
    expect(chosen()).toEqual({ id: "r7", via: "center" });
  } finally {
    view.unmount();
  }
});

// The focus is on a control at the foot of a long row whose head is above the view, and the
// anchor keeps that control's own place. A host asking to keep another row then keeps nothing:
// the reader's place stays, so the row's own words above the control being cut leave the control
// where it was. A keep that took the reader's place held the other row, which the cut does not
// move, and the control went up by the cut.
test("a host's keep leaves the place of the reader's focused control, which a cut above the control then keeps", () => {
  heights = { r3: 1000 };
  const box = drawInBox(ids(40));
  try {
    const focused = controlOf("r3");
    focused.focus();
    // The reader scrolls to the control: r3 runs from 200 to 1,200 px down the content, and its
    // control stands at its foot.
    box.scroller.scrollTop = 1000;
    box.settle();
    const held = controlTop("r3");
    expect(held).toBe(180);

    anchor.current?.keepRow(rowOf("r2"));
    expect(chosen()).toEqual({ id: "r3", via: "focus" });

    // r3 is cut to 200 px, which raises its control by 800 px.
    heights = { r3: 200 };
    box.settle();
    expect(controlTop("r3")).toBe(held);

    // A commit that adds a row above keeps the control there too.
    box.rerender(["new", ...ids(40)]);
    expect(controlTop("r3")).toBe(held);
  } finally {
    box.unmount();
  }
});

// A host scrolls its box to an element an address names, inside a long row whose head is above
// the view, and keeps that element's own place (`keepRow`'s `element`), with no focus on it: as
// the history lands a short reply under a long root turn drawn whole. The row's own content above
// the element is then cut by a commit of its own, which the anchor sees only as its content
// resized: that moves the element up by the cut and leaves the row's top where it was. Kept at
// the row's top, the element went up by the cut, out of the view; kept at its own, it stays where
// the host put it, and the row's top moves down by the cut instead. A host that names no element
// keeps the row's top, as the cases show ("a row a host keeps holds its place across a commit
// that adds rows below it and shrinks a row above it", "a box the browser pulls back to its end
// keeps the row a host keeps, at the offset the pull-back left").
test("an element a host keeps inside a row keeps its own place when the row's own content above it shrinks", () => {
  heights = { r3: 1000 };
  const box = drawInBox(ids(40), { ownsScrollAnchoring: true });
  try {
    const element = controlOf("r3");
    // The host scrolls the box to the element: r3 runs from 200 to 1,200 px down the content, and
    // the element stands at its foot.
    box.scroller.scrollTop = 1000;
    box.settle();
    anchor.current?.keepRow(rowOf("r3"), element);
    expect(chosen()).toEqual({ id: "r3", via: "kept" });
    expect(element).not.toHaveFocus();
    const kept = controlTop("r3");
    expect(kept).toBe(180);
    expect(viewportTop("r3")).toBe(-800);

    // r3 is cut to 200 px, which raises the element by 800 px.
    heights = { r3: 200 };
    box.settle();
    expect(controlTop("r3"), "the cut above the element the host keeps moved it").toBe(kept);
    expect(viewportTop("r3")).toBe(0);
    expect(box.scroller.scrollTop).toBe(200);

    // A commit that adds a row above holds r3 at the top that puts the element back, so the
    // element stays there too.
    box.rerender(["new", ...ids(40)]);
    expect(controlTop("r3")).toBe(kept);
    box.settle();
    expect(controlTop("r3")).toBe(kept);
  } finally {
    box.unmount();
  }
});

// The element a host keeps inside a row holds the place only until something else names it: a
// press of the reader's inside the list lets it go, as it lets a focused control go
// (`readerPressed`); the element leaving the row lets it go; and an act of the reader's records
// the reader's own place instead: the hand moving onto the row, or the focus moving onto the
// element, which then keeps the place only while it holds the focus, as any focused control
// does. The row's top is kept from then on: a cut of the row's own content above the element
// moves the element up by the cut and leaves the row's top where it was, and a row above it that
// grows in the same frame is made good by a scroll of the box, so the row stays where it was. An
// element still kept after any of these held the place, and the cut moved the row down by 800 px
// instead; with no place kept, the growth above moved the row down by 100 px.
for (const { letGo, once } of [
  {
    letGo: () => fireEvent.keyDown(rowOf("r20")),
    once: "the reader presses a key inside the list",
  },
  { letGo: () => rowOf("r4").append(controlOf("r3")), once: "the element leaves the row" },
  {
    // user-event's hover sends no clientX/clientY of its own, and the pointer's place names the row.
    letGo: () => fireEvent.pointerMove(rowOf("r3"), { clientX: 20, clientY: 100 }),
    once: "the reader's hand moves onto the row",
  },
  {
    letGo: () => {
      controlOf("r3").focus();
      controlOf("r3").blur();
    },
    once: "the reader's focus moves onto the element and leaves the list",
  },
]) {
  test(`an element a host keeps inside a row is let go once ${once}, and the row's own top is kept`, () => {
    heights = { r3: 1000 };
    const box = drawInBox(ids(40), { ownsScrollAnchoring: true });
    try {
      // As in the case above: r3 runs from 200 to 1,200 px down the content, and the host keeps
      // the control at its foot.
      box.scroller.scrollTop = 1000;
      box.settle();
      anchor.current?.keepRow(rowOf("r3"), controlOf("r3"));
      expect(controlTop("r3")).toBe(180);
      letGo();

      // r3 is cut to 200 px, which raises the control by 800 px, and in the same frame r2 grows
      // by 100 px, which moves r3 down by as much.
      heights = { r2: 150, r3: 200 };
      box.settle();
      expect(viewportTop("r3"), `once ${once}, the row's own top was not kept`).toBe(-800);
      expect(box.scroller.scrollTop).toBe(1100);
    } finally {
      box.unmount();
    }
  });
}

// The host letting its place go: the next commit holds the row nearest the middle of the view,
// or, at the top, nothing, and the box gets its own scroll anchoring back. A place the reader's
// own act recorded is not the host's to let go.
test("keepRow(null) lets the row a host kept go, and leaves the reader's own place kept", () => {
  const view = render(<List ownsScrollAnchoring rows={ids(12)} scroller={scrollerRef} />);
  try {
    const scroller = scrollerRef.current;
    if (scroller === null) throw new Error("scroller missing");
    scroller.style.setProperty("overflow-anchor", "auto");
    anchor.current?.keepRow(rowOf("r3"));
    expect(chosen()).toEqual({ id: "r3", via: "kept" });
    expect(scroller.style.getPropertyValue("overflow-anchor")).toBe("none");

    anchor.current?.keepRow(null);
    expect(chosen()).toBeNull();
    expect(scroller.style.getPropertyValue("overflow-anchor")).toBe("auto");
    // At the top of the box, rows arriving above land in view.
    view.rerender(
      <List ownsScrollAnchoring rows={["a", "b", ...ids(12)]} scroller={scrollerRef} />
    );
    expect(scroller.scrollTop).toBe(0);

    // The pointer resting on a row is the reader's own place: its boundary event and the hand's
    // own move onto the row, at a point of the pointer's own, which user-event's hover cannot set.
    fireEvent.pointerOver(rowOf("r4"), { clientX: 20, clientY: 370 });
    fireEvent.pointerMove(rowOf("r4"), { clientX: 20, clientY: 370 });
    expect(chosen()).toEqual({ id: "r4", via: "pointer" });
    anchor.current?.keepRow(null);
    expect(chosen()).toEqual({ id: "r4", via: "pointer" });
    expect(scroller.style.getPropertyValue("overflow-anchor")).toBe("none");
  } finally {
    view.unmount();
  }
});

// A scroll the reader made says where they are now: the row a host kept goes, and the row
// nearest the middle of the view is the place from then on. A scroll event at the offset the
// place was kept at reports no move of the view, and leaves the kept row kept.
test("a scroll the reader made lets the row a host kept go, and one that moved nothing leaves it", () => {
  const view = render(<List ownsScrollAnchoring rows={ids(12)} scroller={scrollerRef} />);
  try {
    const scroller = scrollerRef.current;
    if (scroller === null) throw new Error("scroller missing");
    scroller.scrollTop = 100;
    anchor.current?.keepRow(rowOf("r5"));
    // A scroll event at the offset the box stands at, which user-event has no form for.
    fireEvent.scroll(scroller);
    expect(chosen()).toEqual({ id: "r5", via: "kept" });

    // The reader turns the wheel over the box, which makes the scroll that follows theirs; user-event has no wheel.
    fireEvent.wheel(scroller, { deltaY: 150 });
    // The reader scrolls the box, and the browser says so with a scroll event, which user-event
    // has no form for.
    scroller.scrollTop = 250;
    fireEvent.scroll(scroller);
    expect(chosen()).toEqual({ id: "r8", via: "center" });
  } finally {
    view.unmount();
  }
});

// The browser pulling the box back to its end after content above shrank by more than the room
// below the view is no scroll of the reader's: the row a host keeps stays kept, recorded where
// the pull-back left the box, so content above that grows again leaves the row where it was.
test("a box the browser pulls back to its end keeps the row a host keeps, at the offset the pull-back left", () => {
  heights = { r3: 1000 };
  const box = drawInBox(ids(20));
  try {
    // The reader scrolls to the box's end (2,050 - 600 = 1,450 px).
    box.scroller.scrollTop = 1450;
    box.settle();
    anchor.current?.keepRow(rowOf("r18"));
    expect(chosen()).toEqual({ id: "r18", via: "kept" });
    expect(viewportTop("r18")).toBe(450);

    // r3 is cut by 800 px, and the browser pulls the box back by as much, to its new end.
    heights = { r3: 200 };
    box.settle();
    expect(box.scroller.scrollTop).toBe(650);
    expect(chosen()).toEqual({ id: "r18", via: "kept" });
    expect(viewportTop("r18")).toBe(450);

    // r3 grows back by those 800 px, in no commit of the anchor's.
    heights = { r3: 1000 };
    box.settle();
    expect(viewportTop("r18")).toBe(450);
  } finally {
    box.unmount();
  }
});

// The same pull-back where the host names an element inside the row it keeps (`keepRow`'s
// `element`), for a host that leaves the box's own scroll anchoring on: the place is recorded
// again where the pull-back left the box, and it is still that element's own, so the row's own
// content above the element that grows afterwards leaves the element where it was. Recorded again
// at the row alone, the place kept the row's top, and the growth moved the element down.
test("a box the browser pulls back to its end keeps the element a host keeps inside a row, at the offset the pull-back left", () => {
  heights = { r3: 1000, r18: 200 };
  const box = drawInBox(ids(20));
  try {
    // The reader scrolls to the box's end (2,200 - 600 = 1,600 px), and the host keeps r18's
    // control, which stands at the foot of r18, from 300 to 500 px down the view.
    box.scroller.scrollTop = 1600;
    box.settle();
    anchor.current?.keepRow(rowOf("r18"), controlOf("r18"));
    expect(chosen()).toEqual({ id: "r18", via: "kept" });
    expect(viewportTop("r18")).toBe(300);
    const kept = controlTop("r18");
    expect(kept).toBe(480);

    // r3 is cut by 800 px, and the browser pulls the box back by as much, to its new end.
    heights = { r3: 200, r18: 200 };
    box.settle();
    expect(box.scroller.scrollTop).toBe(800);
    expect(chosen()).toEqual({ id: "r18", via: "kept" });
    expect(controlTop("r18")).toBe(kept);

    // r18's own content above its control grows by 100 px, in no commit of the anchor's.
    heights = { r3: 200, r18: 300 };
    box.settle();
    expect(
      controlTop("r18"),
      "after the pull-back, the growth above the element the host keeps moved it"
    ).toBe(kept);
    expect(viewportTop("r18")).toBe(200);
  } finally {
    box.unmount();
  }
});

// The row a host keeps leaving the list takes the place with it: the next commit holds the row
// nearest the middle of the view, and the box gets its own scroll anchoring back.
test("the row a host keeps leaving the list lets its place go", () => {
  const view = render(<List ownsScrollAnchoring rows={ids(12)} scroller={scrollerRef} />);
  try {
    const scroller = scrollerRef.current;
    if (scroller === null) throw new Error("scroller missing");
    scroller.style.setProperty("overflow-anchor", "auto");
    scroller.scrollTop = 100;
    anchor.current?.keepRow(rowOf("r5"));
    expect(chosen()).toEqual({ id: "r5", via: "kept" });

    view.rerender(
      <List ownsScrollAnchoring rows={ids(12).filter((id) => id !== "r5")} scroller={scrollerRef} />
    );
    expect(chosen()).toEqual({ id: "r7", via: "center" });
    expect(scroller.style.getPropertyValue("overflow-anchor")).toBe("auto");
  } finally {
    view.unmount();
  }
});

/** Each row any of which lies in the view, by id, with its viewport top. */
function rowsInView(): Record<string, number> {
  const seen: Record<string, number> = {};
  for (const row of rows()) {
    const { bottom, top } = row.getBoundingClientRect();
    if (bottom > 0 && top < VIEWPORT_HEIGHT) seen[row.getAttribute("data-row") ?? ""] = top;
  }
  return seen;
}

/** The reader's view in the cases below, in a box drawn with r70 3,000 px tall: from 3,550 to
 *  6,550 px down the content, its control at its foot from 6,530 px. The reader scrolls to that
 *  control, 330 px down the view, and the focus is moved to `focused`, the control or the row
 *  itself. The reader then scrolls on until r70 lies wholly below the view, its top at 650 px, or
 *  wholly above it, its foot at -50 px, and the box's scroll event records the place. Answers the
 *  row in the middle of the view. */
function scrollPastFocusedR70(
  box: { scroller: HTMLElement; settle: () => void },
  focused: HTMLElement,
  side: "above" | "below"
): string {
  box.scroller.scrollTop = 6200;
  box.settle();
  focused.focus();
  expect(controlTop("r70")).toBe(330);
  box.scroller.scrollTop = side === "below" ? 2900 : 6600;
  box.settle();
  expect(document.activeElement).toBe(focused);
  expect(viewportTop("r70")).toBe(side === "below" ? 650 : -3050);
  const middle = rowAtDepth(box.scroller.scrollTop + VIEWPORT_HEIGHT / 2 - listTop);
  if (middle === null) throw new Error("no row in the middle of the view");
  return middle.getAttribute("data-row") ?? "";
}

// The focus is on a control at the foot of a tall row, and the reader scrolls on until that row
// lies wholly below the view. The row is then cut short in a commit of its own, which the anchor
// sees only as its root resized. The reader sees none of the row, so nothing on screen moves:
// keeping the control's own place scrolled the box up by the cut, 2,800 px, and every row in view
// went down out of it.
test("a cut of the focused row wholly below the view leaves the box and the rows in view where they were", () => {
  heights = { r70: 3000 };
  const box = drawInBox(ids(140));
  try {
    const control = controlOf("r70");
    const middle = scrollPastFocusedR70(box, control, "below");
    const scrolled = box.scroller.scrollTop;
    const seen = viewportTop(middle);
    expect(seen).toBe(300);

    // r70 is cut to 200 px.
    heights = { r70: 200 };
    box.settle();
    expect(box.scroller.scrollTop).toBe(scrolled);
    expect(viewportTop(middle)).toBe(seen);
  } finally {
    box.unmount();
  }
});

// The same with the row wholly above the view, the focus on the row itself, and the host taking
// the box over, so the browser's own scroll anchoring is off there. The cut takes every row under
// r70 up by 2,800 px and leaves r70's top where it was: keeping that top let the rows in view go up
// out of it. The first row in view is kept instead, and the box follows it.
test("a cut of the focused row wholly above the view, in a box the host takes over, leaves the rows in view where they were", () => {
  heights = { r70: 3000 };
  const box = drawInBox(ids(140), { ownsScrollAnchoring: true });
  try {
    const middle = scrollPastFocusedR70(box, rowOf("r70"), "above");
    const seen = viewportTop(middle);
    expect(seen).toBe(300);
    expect(box.scroller.style.getPropertyValue("overflow-anchor")).toBe("none");

    // r70 is cut to 200 px.
    heights = { r70: 200 };
    box.settle();
    expect(viewportTop(middle)).toBe(seen);
  } finally {
    box.unmount();
  }
});

// A commit draws a new row between the focused row, wholly outside the view, and the view: under
// r70 where r70 lies above the view, over it where r70 lies below. The new row lands outside the
// view, and no row in view moves. Keeping r70's place moved every row in view by the new row's
// height: down where r70 lies above the view, up where it lies below.
test("a row a commit draws between a focused row wholly outside the view and the view moves no row in view", () => {
  for (const side of ["below", "above"] as const) {
    heights = { r70: 3000 };
    const box = drawInBox(ids(140));
    try {
      const control = controlOf("r70");
      scrollPastFocusedR70(box, control, side);
      const seen = rowsInView();

      const next = ids(140);
      next.splice(side === "below" ? 69 : 70, 0, "new");
      box.rerender(next);
      expect(rowsInView(), `r70 ${side} the view`).toEqual(seen);
      box.settle();
      expect(rowsInView(), `r70 ${side} the view, once the frame has run`).toEqual(seen);
    } finally {
      box.unmount();
    }
  }
});

// A host that takes its box over keeps the reader's place there, the pointer resting on a row,
// while a hold of the page's own keeps a control outside the list where a key landed the focus:
// Details' hold (`details-more.tsx`), which scrolls the box in its own resize observation, ahead
// of the anchor's, as content above both grows. The anchor finds the box off the place it kept
// and puts nothing back, and the event of that scroll records the place where the view now shows
// it. Then the focus moves off the hold's control to another outside the list, which ends the
// hold, or the reader turns the wheel at the box's end, which scrolls nothing; neither raises a
// scroll event, and content above the view grows again: the rows in view stay. Dropping that
// event as a move of nothing left a place that never held again, with the box's own anchoring
// off, and the growth moved every row in view down by as much.
for (const then of [
  "the focus moves off the hold's control",
  "a turn of the wheel scrolls nothing",
] as const) {
  test(`after another hold scrolls the box in its frame's observations and ${then}, content above the view that grows moves no row in view`, () => {
    const box = drawFrameByFrame(ids(30), { ownsScrollAnchoring: true });
    // The hold's control and another, both outside the list, as Details' Collaborators and
    // Subscribers controls stand outside its history.
    const [control, other] = [document.createElement("button"), document.createElement("button")];
    document.body.append(control, other);
    try {
      control.focus();
      box.scroller.scrollTop = 300;
      // The reader's scroll, reported by the box's own scroll event, which user-event has no form for.
      fireEvent.scroll(box.scroller);
      // The pointer comes to rest on r10 at a place of its own, its boundary event and then the
      // hand's own move onto the row, which user-event's hover cannot set.
      fireEvent.pointerOver(rowOf("r10"), { clientX: 20, clientY: 260 });
      fireEvent.pointerMove(rowOf("r10"), { clientX: 20, clientY: 260 });
      expect(chosen()).toEqual({ id: "r10", via: "pointer" });
      expect(viewportTop("r10")).toBe(250);

      // Content above the rows grows by 400 px, and in the frame's resize observations the other
      // hold scrolls the box by as much before the anchor's own observation runs.
      listTop += 400;
      box.scroller.scrollTop = 700;
      box.resized();
      expect(viewportTop("r10")).toBe(250);
      // The next frame reports that scroll with the box's own scroll event, which user-event has no form for.
      fireEvent.scroll(box.scroller);
      box.frameEnds();

      if (then === "the focus moves off the hold's control") {
        other.focus();
      } else {
        // A wheel turn over the box that scrolls nothing raises no scroll event, which user-event cannot leave out.
        fireEvent.wheel(box.scroller, { deltaY: 100 });
      }

      // Content above the view grows by 300 px more, in no commit of the anchor's.
      listTop += 300;
      box.resized();
      expect(
        viewportTop("r10"),
        `after ${then}, content above the view grew and moved the row in view`
      ).toBe(250);
      expect(box.scroller.scrollTop).toBe(1000);
    } finally {
      box.unmount();
      control.remove();
      other.remove();
    }
  });
}

// The cases below hold a host that takes its box over (`ownsScrollAnchoring`) to the rule its
// place is kept by: only an act of the reader's records another place, and each event the
// browser raises for a change of the page records nothing. One case per such event.

// The reader's pointer comes to rest on r10 near its top, moving a few pixels on inside the row
// after reaching it, which raises a `pointermove` and no boundary event. Content above the rows
// then grows by 300 px in no commit of the anchor's, which takes r10 from under the pointer, and
// Chromium raises that move's boundary events at the start of the next frame, at the point the
// pointer rests at, before the frame observes the new size. Read as the hand moving, the
// `pointerout` let the place go, the observation found nothing kept, and every row in view went
// down by the growth. Found by the tester on a running build with a copy of a large real issue:
// Blocked by, above Details' view, grew by 300 px as the titles it names landed, and the entry
// under the pointer went from 500 to 800 px in three runs of three.
test("a row moved from under a pointer resting where its last move left it keeps its place, and so does every row in view", () => {
  const box = drawFrameByFrame(ids(30), { ownsScrollAnchoring: true });
  try {
    box.scroller.scrollTop = 300;
    const row = rowOf("r10");
    // The pointer reaches r10 at a point of its own, and comes to rest a few pixels on inside
    // it: user-event's hover sets no point, and sends no move inside the row it is on.
    fireEvent.pointerOver(row, { clientX: 20, clientY: 262 });
    fireEvent.pointerMove(row, { clientX: 20, clientY: 262 });
    fireEvent.pointerMove(row, { clientX: 23, clientY: 263 });
    expect(chosen()).toEqual({ id: "r10", via: "pointer" });
    const seen = rowsInView();
    expect(seen.r10).toBe(250);

    // Content above the rows grows by 300 px, which leaves r4 under the pointer.
    listTop += 300;
    // The browser's own boundary events at the point the pointer rests at, which user-event
    // cannot raise, ahead of the frame's resize observations.
    fireEvent.pointerOut(row, { clientX: 23, clientY: 263, relatedTarget: rowOf("r4") });
    fireEvent.pointerOver(rowOf("r4"), { clientX: 23, clientY: 263 });
    expect(viewportTop("r10"), "the boundary events read the growth as the hand moving").toBe(250);
    box.resized();
    expect(rowsInView(), "the growth above the rows moved the rows in view").toEqual(seen);
    expect(chosen()).toEqual({ id: "r10", via: "pointer" });
  } finally {
    box.unmount();
  }
});

// A host keeps r8's place (`keepRow`), as Details' history keeps the entry beside Load older,
// while the reader's pointer rests over the box's content above the rows: it left r1 for that
// content and moved a pixel on. That content then shrinks by 100 px, which brings r2 under the
// resting pointer, and the browser raises a `pointerover` for r2 at the pointer's point before the
// frame observes the change. Read as the hand moving onto r2, it took r2 for the reader's place,
// in the host's stead, where the shrink had already moved r8 up by 100 px; r8 stayed there.
test("a row moved under a pointer resting where its last move left it is no place of the reader's, and the row a host keeps stays", () => {
  listTop = 400;
  const box = drawFrameByFrame(ids(30), { ownsScrollAnchoring: true });
  try {
    box.scroller.scrollTop = 300;
    // The pointer reaches r1 at a point of its own, then leaves it for the content above the
    // rows and comes to rest a pixel on: user-event's hover and unhover set no point, and send
    // no move of their own.
    fireEvent.pointerOver(rowOf("r1"), { clientX: 20, clientY: 120 });
    fireEvent.pointerMove(rowOf("r1"), { clientX: 20, clientY: 120 });
    fireEvent.pointerOut(rowOf("r1"), { clientX: 20, clientY: 60, relatedTarget: box.scroller });
    fireEvent.pointerMove(box.scroller, { clientX: 20, clientY: 60 });
    fireEvent.pointerMove(box.scroller, { clientX: 21, clientY: 61 });
    anchor.current?.keepRow(rowOf("r8"));
    expect(chosen()).toEqual({ id: "r8", via: "kept" });
    expect(viewportTop("r8")).toBe(450);

    // The content above the rows shrinks by 100 px, which leaves r2 under the pointer.
    listTop -= 100;
    // The browser's own boundary event at the point the pointer rests at, which user-event
    // cannot raise, ahead of the frame's resize observations.
    fireEvent.pointerOver(rowOf("r2"), { clientX: 21, clientY: 61 });
    box.resized();
    expect(viewportTop("r8"), "the pointer's row took the host's place").toBe(450);
    expect(chosen()).toEqual({ id: "r8", via: "kept" });

    // r5 grows by 50 px in a commit of its own.
    heights = { r5: 100 };
    box.resized();
    expect(viewportTop("r8")).toBe(450);
  } finally {
    box.unmount();
  }
});

// A host keeps r8's place (`keepRow`) while the reader's pointer rests where it rested as the list
// was drawn, over the box's content above the rows: no `pointermove` has been heard since, so no
// point says where it rests. That content shrinks by 100 px, which brings r2 under the pointer,
// and the browser raises a `pointerover` for r2 at the resting point; putting r8 back takes r2
// from under the pointer again, and the browser raises a `pointerout` for it there. Read as the
// hand moving, the `pointerover` took r2 for the reader's place in the host's stead, the
// `pointerout` let that place go, and r5 growing then moved r8 down by as much. The history's list
// is drawn when its log read lands, so a reader whose mouse rests over Details as a key opens it
// meets exactly this.
test("before the pointer's first move, the boundary events the browser raises at the point it rests at record no place, and the row a host keeps stays", () => {
  listTop = 400;
  const box = drawFrameByFrame(ids(30), { ownsScrollAnchoring: true });
  try {
    box.scroller.scrollTop = 300;
    anchor.current?.keepRow(rowOf("r8"));
    expect(chosen()).toEqual({ id: "r8", via: "kept" });
    expect(viewportTop("r8")).toBe(450);

    // The content above the rows shrinks by 100 px, which leaves r2 under the resting pointer.
    listTop -= 100;
    // The browser's own boundary event at the point the pointer rests at, which user-event
    // cannot raise, ahead of the frame's resize observations.
    fireEvent.pointerOver(rowOf("r2"), { clientX: 21, clientY: 61, relatedTarget: box.scroller });
    box.resized();
    expect(chosen(), "a pointerover at the resting pointer took its row for the reader's").toEqual({
      id: "r8",
      via: "kept",
    });
    expect(viewportTop("r8")).toBe(450);

    // r8 put back took r2 from under the pointer, and the browser's own boundary event for that
    // comes at the point the pointer rests at, which user-event cannot raise.
    fireEvent.pointerOut(rowOf("r2"), { clientX: 21, clientY: 61, relatedTarget: box.scroller });
    expect(chosen(), "a pointerout at the resting pointer let the host's place go").toEqual({
      id: "r8",
      via: "kept",
    });

    // r5 grows by 50 px in a commit of its own.
    heights = { r5: 2 * ROW_HEIGHT };
    box.resized();
    expect(viewportTop("r8"), "the growth of r5 moved the row the host keeps").toBe(450);
  } finally {
    box.unmount();
  }
});

// The focus is on a control at the foot of a tall row, and the reader presses Enter on it, which
// lets the control go: what the key opens opens below the row's head (`readerPressed`). A row
// below is then cut by more than the room below the view, and the browser pulls the box back to
// its new end, which moves the row down where no scroll can put it back. Recorded at the
// pull-back's scroll event as an act of the reader's, the place took the focused control again,
// and the row's own words above the control growing then pushed the row's head up by as much.
test("a pull-back of the box to its end records nothing: a focused control a key let go stays let go", async () => {
  const user = userEvent.setup();
  heights = { r18: 200, r20: 350 };
  const box = drawInBox(ids(20), { ownsScrollAnchoring: true });
  try {
    // The reader scrolls to 100 px short of the box's end (1,550 - 600 = 950 px), and the focus
    // moves to r18's control.
    box.scroller.scrollTop = 850;
    box.settle();
    const focused = controlOf("r18");
    focused.focus();
    expect(viewportTop("r18")).toBe(100);
    expect(controlTop("r18")).toBe(280);
    await user.keyboard("{Enter}");

    // r20 is cut by 300 px, three times the room below the view: the browser pulls the box back
    // 200 px to its new end, which moves r18 down by as much.
    heights = { r18: 200 };
    box.settle();
    expect(box.scroller.scrollTop).toBe(650);
    expect(viewportTop("r18")).toBe(300);

    // r18's own words above its control grow by 100 px.
    heights = { r18: 300 };
    box.settle();
    expect(viewportTop("r18"), "the pull-back took the control the key let go again").toBe(300);
    expect(controlTop("r18")).toBe(580);
  } finally {
    box.unmount();
  }
});

// The reader's pointer left the rows for the box's content above them, so no place is kept and
// the box's own scroll anchoring is on. That content shrinks by 50 px, which brings r1 under the
// resting pointer, and the browser scrolls the box by as much to keep a node of its own choosing.
// Neither the `pointerover` that move raises nor the scroll is an act of the reader's: recorded at
// the scroll's event, r1 became the place to keep, and the box's anchoring went off for a place
// the reader never chose.
test("the browser's own anchoring scroll records nothing, and the box keeps its own anchoring", () => {
  listTop = 400;
  const view = render(<List ownsScrollAnchoring rows={ids(12)} scroller={scrollerRef} />);
  try {
    const scroller = scrollerRef.current;
    if (scroller === null) throw new Error("scroller missing");
    scroller.style.setProperty("overflow-anchor", "auto");
    scroller.scrollTop = 300;
    // The pointer reaches r1 at a point of its own, then leaves it for the content above the
    // rows: user-event's hover and unhover set no point, and send no move of their own.
    fireEvent.pointerOver(rowOf("r1"), { clientX: 20, clientY: 120 });
    fireEvent.pointerMove(rowOf("r1"), { clientX: 20, clientY: 120 });
    fireEvent.pointerOut(rowOf("r1"), { clientX: 20, clientY: 60, relatedTarget: scroller });
    fireEvent.pointerMove(scroller, { clientX: 20, clientY: 60 });
    expect(scroller.style.getPropertyValue("overflow-anchor")).toBe("auto");

    // The content above the rows shrinks by 50 px, which leaves r1 under the pointer.
    listTop -= 50;
    // The browser's own boundary event at the point the pointer rests at, which user-event
    // cannot raise; then its anchoring's scroll and that scroll's event, which it has no form for.
    fireEvent.pointerOver(rowOf("r1"), { clientX: 20, clientY: 60 });
    scroller.scrollTop = 250;
    fireEvent.scroll(scroller);
    expect(
      scroller.style.getPropertyValue("overflow-anchor"),
      "the browser's own anchoring scroll took a place for the reader's"
    ).toBe("auto");
  } finally {
    view.unmount();
  }
});

// A host keeps r8's place (`keepRow`). Content above the rows grows by 100 px, and the anchor
// puts r8 back in its frame's resize observation; another hold of the page's, Details' hold of
// the place a key landed the focus at (`details-more.tsx`), scrolls the box 60 px more in its own
// observation after the anchor's. The next frame's scroll event reports that scroll, which no act
// of the reader's made: taken for the reader's, it let the host's place go with no row under the
// hand, and the next growth above moved r8 and every row in view down by as much.
test("a scroll another script made records nothing, and the row a host keeps stays kept where it left it", () => {
  const box = drawFrameByFrame(ids(30), { ownsScrollAnchoring: true });
  try {
    box.scroller.scrollTop = 100;
    anchor.current?.keepRow(rowOf("r8"));
    expect(viewportTop("r8")).toBe(350);
    listTop += 100;
    box.resized();
    expect(viewportTop("r8")).toBe(350);
    // The other hold's scroll, in its observation after the anchor's.
    box.scroller.scrollTop = 260;
    // The next frame reports it with the box's own scroll event, which user-event has no form for.
    fireEvent.scroll(box.scroller);
    box.frameEnds();
    expect(chosen(), "another script's scroll let the host's place go").toEqual({
      id: "r8",
      via: "kept",
    });
    const seen = rowsInView();
    expect(seen.r8).toBe(290);

    // Content above the rows grows by 100 px more, in no commit of the anchor's.
    listTop += 100;
    box.resized();
    expect(rowsInView(), "the growth above the rows moved the rows in view").toEqual(seen);
  } finally {
    box.unmount();
  }
});

// A host scrolls the box it named itself through the anchor (`scrollBox`), as Details' history
// brings the control a press of its Show more focuses into view. The anchor marks that scroll as
// its own: its event, which the reader's key just before it would make the reader's scroll,
// records nothing, and the row the host keeps stays kept. Read as the reader's, the event let the
// host's place go for the row in the middle of the view. The window never moves.
test("a host's scroll of its box through the anchor records nothing at its event, and moves no window", async () => {
  const user = userEvent.setup();
  const box = drawFrameByFrame(ids(30), { ownsScrollAnchoring: true });
  try {
    box.scroller.scrollTop = 100;
    anchor.current?.keepRow(rowOf("r8"));
    // The reader's key, which makes a scroll heard soon after it the reader's own.
    await user.keyboard("{ArrowDown}");
    anchor.current?.scrollBox(60);
    expect(box.scroller.scrollTop).toBe(160);
    expect(window.scrollY, "the host's scroll of its box moved the window").toBe(0);
    // The next frame reports it with the box's own scroll event, which user-event has no form for.
    fireEvent.scroll(box.scroller);
    box.frameEnds();
    expect(chosen(), "the host's own scroll let the host's place go").toEqual({
      id: "r8",
      via: "kept",
    });
  } finally {
    box.unmount();
  }
});

// A host that takes its box over draws its rows under 1,000 px of other content, as Details draws
// its history under its sections. The reader's focus is on r10's control, in view. A key then
// moves the focus out of the list, to a control in the content above it, and the page's own
// script scrolls the box to that control, as `t o` lands on Collaborators: the rows now lie wholly
// below the view. Content between the view and the rows then grows, as the collaborators' list
// that `t o` opens lands: nothing in view moves. Keeping r10's place scrolled the box down by the
// growth, and the control the focus was on went out of the view above. Found by the tester on a
// running build at 390 px: after Load older and `t o`, Collaborators went from 77 to -1,265 px in
// four runs of seven.
test("a scroll another script made that leaves the kept row wholly outside the view lets content above the rows grow and moves nothing in view", () => {
  listTop = 1000;
  const box = drawFrameByFrame(ids(30), { ownsScrollAnchoring: true });
  const outside = document.createElement("button");
  document.body.append(outside);
  try {
    box.scroller.scrollTop = 1200;
    // The reader's scroll, reported by the box's own scroll event, which user-event has no form for.
    fireEvent.scroll(box.scroller);
    const control = controlOf("r10");
    control.focus();
    expect(controlTop("r10")).toBe(280);

    // A key moves the focus to the control above the rows, and the page's script scrolls to it.
    outside.focus();
    box.scroller.scrollTop = 0;
    // The next frame reports that scroll with the box's own scroll event, which user-event has no form for.
    fireEvent.scroll(box.scroller);
    box.frameEnds();
    expect(viewportTop("r1")).toBe(1000);

    // Content between the view and the rows grows by 300 px, in no commit of the anchor's.
    listTop += 300;
    box.resized();
    expect(
      box.scroller.scrollTop,
      "content that grew below the view scrolled the box to a row the reader cannot see"
    ).toBe(0);
  } finally {
    box.unmount();
    outside.remove();
  }
});

/** `List` with the host taking its box over, every row's control drawn but `bare`'s. */
function ListWithout({ bare, rows: drawn }: { bare: string; rows: string[] }): ReactNode {
  return (
    <div data-scroller="" ref={scrollerRef}>
      <ViewportAnchor
        group="data-group"
        item="data-row"
        ownsScrollAnchoring
        ref={anchor}
        scroller={scrollerRef}
      >
        {drawn.map((id) => (
          <div data-group="one" data-row={id} key={id} tabIndex={-1}>
            {id === bare ? id : <button type="button">{id}</button>}
          </div>
        ))}
      </ViewportAnchor>
    </div>
  );
}

// The focus is on r10's control, which a commit then takes away, as a turn drawn whole drops its
// "more": Chromium raises `focusout` on the control as it removes it, and the focus goes to the
// page. That is no act of the reader's, who still reads r10. A commit then draws a row between
// r10 and the row at the middle of the view: keeping that middle row, as with no place kept,
// moved r10 up by the new row's height.
test("a focusout because the focused node went records nothing, and the next commit keeps the place it left", () => {
  const view = render(<ListWithout bare="" rows={ids(30)} />);
  try {
    const scroller = scrollerRef.current;
    if (scroller === null) throw new Error("scroller missing");
    scroller.scrollTop = 300;
    const control = controlOf("r10");
    control.focus();
    expect(viewportTop("r10")).toBe(250);

    // Chromium's `focusout` as it removes the focused control, raised while the control is still
    // in the list, which user-event has no form for; then the commit that removes it.
    fireEvent.focusOut(control, { relatedTarget: null });
    view.rerender(<ListWithout bare="r10" rows={ids(30)} />);
    expect(document.activeElement).not.toBe(control);
    expect(viewportTop("r10")).toBe(250);

    // A row arrives between r10 and the row at the middle of the view.
    view.rerender(<ListWithout bare="r10" rows={[...ids(10), "new", ...ids(20, 11)]} />);
    expect(viewportTop("r10"), "the focus's loss let the place go to the middle row").toBe(250);
    expect(chosen()).toEqual({ id: "r10", via: "kept" });
  } finally {
    view.unmount();
  }
});

// Where the box's one child holds the content above and below the rows as well as the rows, as
// Details' one child holds its sections and the history, a section above that grows while another
// shrinks by as much in the same frame leaves that child's height as it was, and moves the rows.
// While a place is kept, the anchor watches each element between its root and the box and each
// one beside them, and lets them go once none is.
test("while a place is kept in a named box, the anchor watches the content around its root at every depth", () => {
  const RealResizeObserver = globalThis.ResizeObserver;
  const watched = new Set<Element>();
  globalThis.ResizeObserver = class {
    disconnect() {
      watched.clear();
    }
    observe(target: Element) {
      watched.add(target);
    }
    unobserve(target: Element) {
      watched.delete(target);
    }
  } as unknown as typeof ResizeObserver;
  const view = render(
    <div data-scroller="" ref={scrollerRef}>
      <div data-content="">
        <section data-above="">Above the rows</section>
        <ViewportAnchor item="data-row" ownsScrollAnchoring ref={anchor} scroller={scrollerRef}>
          {ids(12).map((id) => (
            <div data-row={id} key={id}>
              {id}
            </div>
          ))}
        </ViewportAnchor>
        <section data-below="">Below the rows</section>
      </div>
      <footer data-foot="">At the box's foot</footer>
    </div>
  );
  try {
    const around = ["[data-content]", "[data-above]", "[data-below]", "[data-foot]"];
    /** The content around the root, by selector, that is watched (`true`) or not (`false`). */
    const watching = (is: boolean) =>
      around.filter((selector) => {
        const element = document.querySelector(selector);
        if (element === null) throw new Error(`${selector} missing`);
        return watched.has(element) === is;
      });
    const root = document.querySelector("[data-above]")?.nextElementSibling ?? null;
    if (root === null) throw new Error("the anchor's root missing");
    expect(watching(true)).toEqual([]);

    // The pointer comes to rest on r3 at a point of its own, its boundary event and then the
    // hand's own move onto the row, which user-event's hover cannot set.
    fireEvent.pointerOver(rowOf("r3"), { clientX: 20, clientY: 220 });
    fireEvent.pointerMove(rowOf("r3"), { clientX: 20, clientY: 220 });
    expect(watching(false), "content around the root left unwatched").toEqual([]);
    expect(watched.has(root)).toBe(true);

    // A pointerout to a relatedTarget outside the rows, at a place of the pointer's own, neither of which user-event's unhover can set.
    fireEvent.pointerOut(rowOf("r3"), { clientX: 20, clientY: 900, relatedTarget: document.body });
    expect(watching(true)).toEqual([]);
    expect(watched.has(root)).toBe(true);
  } finally {
    view.unmount();
    globalThis.ResizeObserver = RealResizeObserver;
  }
});

/**
 * `List` in a box the host takes over, whose resize observers are called back as the browser's
 * are: each only where an element it watches has changed height since it was last called back
 * for that element, or since it began to watch it (`resized`). happy-dom never calls one back,
 * and the other cases' observers are called back whatever changed.
 */
function drawObservingHeights(drawn: string[]) {
  const RealResizeObserver = globalThis.ResizeObserver;
  const observers: {
    callback: ResizeObserverCallback;
    heights: Map<Element, number>;
    observer: ResizeObserver;
  }[] = [];
  globalThis.ResizeObserver = class {
    readonly #heights = new Map<Element, number>();
    constructor(callback: ResizeObserverCallback) {
      observers.push({
        callback,
        heights: this.#heights,
        observer: this as unknown as ResizeObserver,
      });
    }
    disconnect() {
      this.#heights.clear();
    }
    observe(target: Element) {
      this.#heights.set(target, target.getBoundingClientRect().height);
    }
    unobserve(target: Element) {
      this.#heights.delete(target);
    }
  } as unknown as typeof ResizeObserver;
  const view = render(<List ownsScrollAnchoring rows={drawn} scroller={scrollerRef} />);
  const scroller = scrollerRef.current;
  if (scroller === null) throw new Error("scroller missing");
  Object.defineProperty(scroller, "clientHeight", { configurable: true, value: VIEWPORT_HEIGHT });
  Object.defineProperty(scroller, "scrollHeight", {
    configurable: true,
    get: () => listTop + listHeight(),
  });
  return {
    resized: () => {
      for (const { callback, heights: watched, observer } of observers) {
        const changed: ResizeObserverEntry[] = [];
        for (const [target, height] of watched) {
          const now = target.getBoundingClientRect().height;
          if (now === height) continue;
          watched.set(target, now);
          changed.push({ target } as ResizeObserverEntry);
        }
        if (changed.length > 0) callback(changed, observer);
      }
    },
    scroller,
    unmount: () => {
      view.unmount();
      globalThis.ResizeObserver = RealResizeObserver;
    },
  };
}

// The reader's focus is on r10's control. In one frame r3, above it, grows by 50 px while r20,
// below it, shrinks by as much, as a turn's words drawn whole above the place and a turn cut short
// below it can land together: the list's height is as it was, and so is the height of all the
// content around the list. The anchor watches the content around the place kept, the rows beside
// the kept row included, and puts the control back. Watching only the root and the content around
// it, the anchor heard of no change, and the growth moved the control down by as much.
test("a row above the kept place that grows while a row below shrinks by as much in the same frame leaves the place where it was", () => {
  heights = { r20: 2 * ROW_HEIGHT };
  const box = drawObservingHeights(ids(30));
  try {
    box.scroller.scrollTop = 300;
    const control = controlOf("r10");
    control.focus();
    expect(controlTop("r10")).toBe(280);

    heights = { r3: 2 * ROW_HEIGHT, r20: ROW_HEIGHT };
    box.resized();
    expect(controlTop("r10"), "the growth of r3 moved the focused control").toBe(280);
    expect(box.scroller.scrollTop).toBe(350);
  } finally {
    box.unmount();
  }
});
