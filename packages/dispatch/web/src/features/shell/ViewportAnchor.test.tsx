import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
// biome-ignore lint/style/noRestrictedImports: this file sends the pointer's own clientX/clientY and a pointerout's relatedTarget, neither of which user-event's hover and unhover can set.
import { fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import {
  createRef,
  Fragment,
  type ReactNode,
  type RefObject,
  useLayoutEffect,
  useRef,
  useState,
} from "react";

import { disarm } from "../../__tests__/armed-press";
import { nextFrame } from "../../__tests__/frames";
import { installResizeObserver } from "../../__tests__/scroll-box";
import { ControlRow } from "../../components/ControlRow";
import { CONTROL } from "../../components/control-names";
import { ARM_ON_PRESS } from "../../components/focus-place";
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

/** The inline `overflow-anchor` of the box the rows scroll in - the scroller where one is
 *  rendered, else the document's root - as each commit of the list saw it, read by `CommitProbe`. */
const anchoringSeen: string[] = [];

/** The box the rows scroll in now: the scroller where one is rendered, else the document's root,
 *  whose `overflow-anchor` the window takes. */
const scrollBox = (): HTMLElement => scrollerRef.current ?? document.documentElement;

/** Reads the scroll box's inline `overflow-anchor` in every commit of the list, once React has
 *  changed the rows and before the anchor keeps its place: a child's layout effect runs ahead of
 *  its parent's `componentDidUpdate`. */
function CommitProbe(): null {
  useLayoutEffect(() => {
    anchoringSeen.push(scrollBox().style.getPropertyValue("overflow-anchor"));
  });
  return null;
}

function List({
  banner = false,
  children,
  enabled,
  groups,
  headings,
  holdAtTop,
  leaving,
  rows,
  scroller,
}: {
  /** Whether a banner BANNER_HEIGHT tall stands above the list, as the Inbox's header stands
   *  above its rows. */
  banner?: boolean;
  /** Drawn inside the anchor after the rows, so its layout effects run in each of the list's
   *  commits before the anchor keeps its place. */
  children?: ReactNode;
  enabled?: () => boolean;
  /** A row's group; rows default to one shared group. */
  groups?: Readonly<Record<string, string>>;
  /** The rows a heading is drawn right before, as a group's heading is drawn before its first
   *  row. */
  headings?: readonly string[];
  holdAtTop?: boolean;
  /** The rows a press of the reader's is taking away, as the host names them. */
  leaving?: (id: string) => boolean;
  rows: string[];
  /** When given, the rows scroll in this element instead of the document. */
  scroller?: RefObject<HTMLDivElement | null>;
}): ReactNode {
  const list = (
    <ViewportAnchor
      enabled={enabled}
      group="data-group"
      holdAtTop={holdAtTop}
      item="data-row"
      leaving={leaving}
      ref={anchor}
      scroller={scroller}
    >
      {rows.map((id) => (
        <Fragment key={id}>
          {headings?.includes(id) === true ? <div data-heading={id} /> : null}
          <div data-group={groups?.[id] ?? "one"} data-row={id} tabIndex={-1}>
            <button type="button">{id}</button>
          </div>
        </Fragment>
      ))}
      <CommitProbe />
      {children}
    </ViewportAnchor>
  );
  const page = (
    <>
      {banner ? <div data-banner="" /> : null}
      {list}
    </>
  );
  return scroller === undefined ? (
    page
  ) : (
    <div data-scroller="" ref={scroller}>
      {page}
    </div>
  );
}

// A fixed geometry: rows, and the headings between them, are ROW_HEIGHT tall, stacked from the
// list's top in DOM order, in a VIEWPORT_HEIGHT window that scrolls through `window.scrollY` - or,
// where the list is rendered in a scroller, through that element's `scrollTop`, the scroller
// itself filling the window and never moving. A row's own button is drawn in the row's box. The
// list's top is `listTop`, BANNER_HEIGHT lower while the banner is drawn. `listTop` starts at
// LIST_TOP; a test lowers it to shrink content above the rows without moving what scrolls them,
// which is what a card collapsing in a commit of its own does.
const laidOut = () => [...document.querySelectorAll<HTMLElement>("[data-row], [data-heading]")];
const viewportTop = (id: string) =>
  document.querySelector(`[data-row="${id}"]`)?.getBoundingClientRect().top;
const headingTop = (id: string) =>
  document.querySelector(`[data-heading="${id}"]`)?.getBoundingClientRect().top;
const BANNER_HEIGHT = 52;
const listStart = () =>
  listTop + (document.querySelector("[data-banner]") === null ? 0 : BANNER_HEIGHT);

let listTop = LIST_TOP;
let contentLeft = 0;
let contentWidth = 100;
let scrollYDescriptor: PropertyDescriptor | undefined;
let originalScrollBy: typeof window.scrollBy;
let restoreSpies: (() => void)[] = [];

function setScrollY(value: number): void {
  Object.defineProperty(window, "scrollY", { configurable: true, value, writable: true });
}

beforeEach(() => {
  scrollYDescriptor = Object.getOwnPropertyDescriptor(window, "scrollY");
  originalScrollBy = window.scrollBy;
  listTop = LIST_TOP;
  contentLeft = 0;
  contentWidth = 100;
  anchoringSeen.length = 0;
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
    const all = laidOut();
    // Whatever scrolls the rows carries them: the window, or the scroller when one is rendered.
    const offset = scrollerRef.current?.scrollTop ?? window.scrollY;
    const isScroller = this.hasAttribute("data-scroller");
    const item = this.closest("[data-row], [data-heading]");
    const top = isScroller
      ? 0
      : item !== null
        ? listStart() + all.indexOf(item as HTMLElement) * ROW_HEIGHT - offset
        : listStart() - offset;
    const height = isScroller
      ? VIEWPORT_HEIGHT
      : item !== null
        ? ROW_HEIGHT
        : all.length * ROW_HEIGHT;
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
    const index = Math.floor((offset + y - listStart()) / ROW_HEIGHT);
    return laidOut()[index] ?? null;
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

/** Waits until the frame the last commit went into is painted: a frame's callbacks, then a task. */
function painted(): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  requestAnimationFrame(() => setTimeout(resolve, 0));
  return promise;
}

/** A scroll of the reader's own, as the browser reports it: the window, or the box they scroll. */
function scrollTo(top: number, box?: HTMLElement): void {
  if (box === undefined) {
    setScrollY(top);
    document.dispatchEvent(new Event("scroll"));
    return;
  }
  box.scrollTop = top;
  box.dispatchEvent(new Event("scroll"));
}

// A host that holds a reader at the top too (`holdAtTop`: the Issues and priorities tables, the
// Board) keeps the first row on screen there once the reader has seen it: a row added above it
// lands out of view above, and the rows on screen keep their tops.
test("a host that holds a reader at the top keeps the first row on screen it painted as rows arrive above", async () => {
  const view = render(<List holdAtTop rows={ids(12)} />);
  try {
    await painted();
    expect(chosen()).toEqual({ id: "r1", via: "top" });
    const before = viewportTop("r1");
    view.rerender(<List holdAtTop rows={["new-a", ...ids(12)]} />);
    expect(viewportTop("r1")).toBe(before);
    expect(window.scrollY).toBe(ROW_HEIGHT);
    expect(viewportTop("new-a")).toBe(LIST_TOP - ROW_HEIGHT);
  } finally {
    view.unmount();
  }
});

test("a host that holds a reader at the top keeps the first row on screen in its own scroller", async () => {
  const view = render(<List holdAtTop rows={ids(12)} scroller={scrollerRef} />);
  try {
    const scroller = scrollerRef.current;
    if (scroller === null) throw new Error("no scroller");
    await painted();
    expect(chosen()).toEqual({ id: "r1", via: "top" });
    const before = viewportTop("r1");
    view.rerender(<List holdAtTop rows={["new-a", "new-b", ...ids(12)]} scroller={scrollerRef} />);
    expect(viewportTop("r1")).toBe(before);
    expect(scroller.scrollTop).toBe(2 * ROW_HEIGHT);
    expect(window.scrollY).toBe(0);
  } finally {
    view.unmount();
  }
});

// Where the middle of the list holds no row (the gap between two Board columns, an empty column)
// the first row of the column nearest the middle is the one kept.
test("a host that holds a reader at the top keeps the first row of the column nearest the middle", async () => {
  // The middle of a 300 px list is at 150, and rows answer the hit test only from 0 to 100.
  contentWidth = 300;
  const view = render(<List holdAtTop rows={ids(12)} />);
  try {
    await painted();
    expect(chosen()).toEqual({ id: "r1", via: "top" });
  } finally {
    view.unmount();
  }
});

// Rows drawn but not yet painted are rows no reader has seen: a draw before the first frame
// (the table's second draw at the width it measured) holds nothing, and the rows start at the top.
test("a host that holds a reader at the top holds nothing before the rows are first painted", async () => {
  const view = render(<List holdAtTop rows={ids(12)} />);
  try {
    expect(chosen()).toBeNull();
    view.rerender(<List holdAtTop rows={["new-a", ...ids(12)]} />);
    expect(window.scrollY).toBe(0);
    expect(viewportTop("new-a")).toBe(LIST_TOP);
    await painted();
    expect(chosen()).toEqual({ id: "new-a", via: "top" });
  } finally {
    view.unmount();
  }
});

// The Board draws nothing when the window scrolls, so a reader who scrolled down, saw a change
// land there and scrolled back to the top has no commit at the top to record: the scroll that
// ends there records the first row on screen once it is painted.
test("a scroll of the reader's own that ends at the top records the first row on screen there", async () => {
  const view = render(<List holdAtTop rows={ids(12)} />);
  try {
    await painted();
    scrollTo(100);
    expect(chosen()).toEqual({ id: "r7", via: "center" });
    // A change lands while the reader is scrolled, and is painted there.
    view.rerender(<List holdAtTop rows={[...ids(12), "r13"]} />);
    await painted();
    scrollTo(0);
    await painted();
    expect(chosen()).toEqual({ id: "r1", via: "top" });
    const before = viewportTop("r1");
    view.rerender(<List holdAtTop rows={["new-a", ...ids(13)]} />);
    expect(viewportTop("r1")).toBe(before);
    expect(window.scrollY).toBe(ROW_HEIGHT);
  } finally {
    view.unmount();
  }
});

test("a scroll of the reader's own that ends at the top of its own scroller records the first row there", async () => {
  const view = render(<List holdAtTop rows={ids(12)} scroller={scrollerRef} />);
  try {
    const scroller = scrollerRef.current;
    if (scroller === null) throw new Error("no scroller");
    await painted();
    scrollTo(100, scroller);
    view.rerender(<List holdAtTop rows={[...ids(12), "r13"]} scroller={scrollerRef} />);
    await painted();
    scrollTo(0, scroller);
    await painted();
    expect(chosen()).toEqual({ id: "r1", via: "top" });
    view.rerender(<List holdAtTop rows={["new-a", ...ids(13)]} scroller={scrollerRef} />);
    expect(scroller.scrollTop).toBe(ROW_HEIGHT);
  } finally {
    view.unmount();
  }
});

// In a project with a Legion daemon the board's first snapshot is part of a view's first draw:
// Open draws its rows before the board answers, and the snapshot sorts them again. The host holds
// nothing for that commit (the table's view key), and the row it puts first is the one held once
// it is painted.
test("a host that holds a reader at the top holds the row the board's first snapshot puts first, once it is painted", async () => {
  const view = render(<List holdAtTop rows={ids(12)} />);
  try {
    await painted();
    expect(chosen()).toEqual({ id: "r1", via: "top" });
    const sorted = ["r5", ...ids(12).filter((id) => id !== "r5")];
    view.rerender(<List enabled={() => false} holdAtTop rows={sorted} />);
    expect(window.scrollY).toBe(0);
    expect(viewportTop("r5")).toBe(LIST_TOP);
    // Drawn but not yet painted: the row that was first is not the first row on screen.
    expect(chosen()).toBeNull();
    await painted();
    expect(chosen()).toEqual({ id: "r5", via: "top" });
    view.rerender(<List holdAtTop rows={["new-a", ...sorted]} />);
    expect(viewportTop("r5")).toBe(LIST_TOP);
    expect(window.scrollY).toBe(ROW_HEIGHT);
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

/** The button in row `id`, which takes the focus as a row's mark takes it from the selection
 *  bar's hand-off. */
function buttonOf(id: string): HTMLElement {
  const button = document.querySelector<HTMLElement>(`[data-row="${id}"] button`);
  if (button === null) throw new Error(`${id} button missing`);
  return button;
}

/** Every element brought into view from now on, named by its row and its tag, with the options
 *  it was asked with, until `restore`. The view itself does not move. A name rather than the
 *  element, so a failed match prints a line rather than the element's whole tree. */
function watchScrollsIntoView(): {
  restore: () => void;
  scrolled: { options: unknown; target: string }[];
} {
  const scrolled: { options: unknown; target: string }[] = [];
  const spy = spyOn(Element.prototype, "scrollIntoView").mockImplementation(function (
    this: Element,
    options?: unknown
  ) {
    const row = this.closest("[data-row]")?.getAttribute("data-row") ?? "no row";
    scrolled.push({ options, target: `${row} ${this.tagName.toLowerCase()}` });
  });
  return { restore: () => spy.mockRestore(), scrolled };
}

// A Dismiss over two marked rows: the selection bar's hand-off has put the focus in the row after
// them while they are still drawn. They go, and the rows after them move up into their place, as
// the reader asked. The view stays where the reader put it. For that commit the browser's own
// scroll anchoring is off on the document's root, whose value the window takes, and it stays off
// through the rendering update after it, since WebKit adjusts at a layout after the commit: it is
// back on two frames later.
test("rows the host names as leaving let the focused row below them move up into their place, with no scroll", async () => {
  const going = (id: string) => id === "r3" || id === "r4";
  const view = render(<List leaving={going} rows={ids(12)} />);
  try {
    setScrollY(100);
    const focused = buttonOf("r6");
    focused.focus();
    const place = viewportTop("r3");
    expect(chosen()).toEqual({ id: "r6", via: "focus" });

    view.rerender(<List leaving={going} rows={ids(12).filter((id) => !going(id))} />);
    expect(viewportTop("r5")).toBe(place);
    expect(viewportTop("r6")).toBe((place ?? 0) + ROW_HEIGHT);
    expect(window.scrollY).toBe(100);
    expect(focused).toHaveFocus();
    expect(anchoringSeen.at(-1)).toBe("none");
    expect(document.documentElement.style.getPropertyValue("overflow-anchor")).toBe("none");
    await nextFrame();
    expect(document.documentElement.style.getPropertyValue("overflow-anchor")).toBe("none");
    await nextFrame();
    expect(document.documentElement.style.getPropertyValue("overflow-anchor")).toBe("");
  } finally {
    view.unmount();
  }
});

// The same Dismiss while a dialog's scroll lock puts `overflow: hidden` on the body, as the
// Inbox's Decline dialog does at a phone's width until the decline lands and the dialog goes with
// its row. Chromium then takes the window's scroll anchoring from the body, so the browser's own
// anchoring is off on the body for that commit as well as on the document's root, and back on
// with the root's two frames later.
test("rows the host names as leaving hold the browser's scroll anchoring off on the body too, while a dialog locks the page's scroll", async () => {
  const going = (id: string) => id === "r3" || id === "r4";
  const view = render(<List leaving={going} rows={ids(12)} />);
  document.body.style.overflow = "hidden";
  try {
    setScrollY(100);
    buttonOf("r6").focus();
    view.rerender(<List leaving={going} rows={ids(12).filter((id) => !going(id))} />);
    expect(window.scrollY).toBe(100);
    expect(document.body.style.getPropertyValue("overflow-anchor")).toBe("none");
    await nextFrame();
    await nextFrame();
    expect(document.body.style.getPropertyValue("overflow-anchor")).toBe("");
  } finally {
    document.body.style.removeProperty("overflow");
    view.unmount();
  }
});

/** Moves the window by `by` in the layout effects of the commit that draws it, as Firefox's own
 *  scroll anchoring moves it at the layout after a commit, ahead of the anchor's own pass. */
function AnchorsAnyway({ by }: { by: number }): null {
  useLayoutEffect(() => {
    setScrollY(window.scrollY + by);
  }, [by]);
  return null;
}

// A press that takes away the row the reader's hand is on, as the Inbox's Decline dialog goes with
// its row once the decline lands, while the browser moves the window by that row all the same, as
// Firefox does while a dialog's scroll lock is on, whatever `overflow-anchor` the root and the body
// carry. The row has gone, so no row is left to measure the view by: the anchor puts the window
// back where the press found it, the row after it moves up into its place, and the view does not
// scroll.
test("a row the host names as leaving, taken from under the reader's hand, leaves the window where the press found it, though the browser moved it", () => {
  const going = (id: string) => id === "r6";
  const view = render(<List leaving={going} rows={ids(12)} />);
  try {
    setScrollY(100);
    buttonOf("r6").focus();
    const place = viewportTop("r6");
    view.rerender(
      <List leaving={going} rows={ids(12).filter((id) => !going(id))}>
        <AnchorsAnyway by={-ROW_HEIGHT} />
      </List>
    );
    expect(window.scrollY).toBe(100);
    expect(viewportTop("r7")).toBe(place);
  } finally {
    view.unmount();
  }
});

// A live update takes two rows away above the view while the reader's hand is on a row in sight,
// and a press of theirs is still taking a row below that one away. Nobody pressed on the rows that
// went, so the anchor keeps the rows in view where they were, and the browser's own scroll
// anchoring is left on.
test("a live update that takes rows away above the view keeps the rows in view where they were, with the browser's scroll anchoring left on", () => {
  const going = (id: string) => id === "r9";
  const view = render(<List leaving={going} rows={ids(12)} />);
  try {
    setScrollY(275);
    buttonOf("r8").focus();
    const inView = () => ["r6", "r7", "r8", "r9"].map((id) => viewportTop(id));
    const seen = inView();
    // r1 and r2 stand wholly above the window.
    expect((viewportTop("r2") ?? 0) + ROW_HEIGHT).toBeLessThan(0);

    const updated = ids(12).filter((id) => id !== "r1" && id !== "r2");
    view.rerender(<List leaving={going} rows={updated} />);
    expect(inView()).toEqual(seen);
    expect(window.scrollY).toBe(275 - 2 * ROW_HEIGHT);
    expect(anchoringSeen.at(-1)).toBe("");
  } finally {
    view.unmount();
  }
});

// The reader's own Dismiss takes away the row their hand is on, not one above it: the row the
// host names as leaving is the current row. The browser's own scroll anchoring would hold the
// view against that row's going, which is the shift this component lets happen - measured in the
// Inbox at 390x664, a thread's Dismiss moved the window from 437 px to 0 in Chromium, Firefox
// and WebKit - so it is turned off for that commit as it is for a row above, and back on two
// frames later. Nothing else changed above the row's place, so the row after it moves up into
// that place and nothing scrolls.
test("the row the host names as leaving being the focused row itself turns the browser's scroll anchoring off for that commit", async () => {
  const going = (id: string) => id === "r6";
  const view = render(<List leaving={going} rows={ids(12)} />);
  try {
    setScrollY(100);
    buttonOf("r6").focus();
    expect(chosen()).toEqual({ id: "r6", via: "focus" });

    view.rerender(<List leaving={going} rows={ids(12).filter((id) => !going(id))} />);
    expect(window.scrollY).toBe(100);
    expect(anchoringSeen.at(-1)).toBe("none");
    expect(document.documentElement.style.getPropertyValue("overflow-anchor")).toBe("none");
    await nextFrame();
    expect(document.documentElement.style.getPropertyValue("overflow-anchor")).toBe("none");
    await nextFrame();
    expect(document.documentElement.style.getPropertyValue("overflow-anchor")).toBe("");
  } finally {
    view.unmount();
  }
});

// The reader's press takes the last rows of a kind away, and a control the page offers only while
// such rows are listed goes with them: below `sm` the Inbox's header loses the line its Mark all
// read stood on. The rows free their space, and the line that closed above the list is made up for
// as any other change above it is: the row after the rows moves up into their place, and the
// window scrolls up by the line.
test("content above the list that shrinks in the commit in which named rows leave is made up for, and the row after them stands at their place", () => {
  const going = (id: string) => id === "r3" || id === "r4";
  const view = render(<List banner leaving={going} rows={ids(12)} />);
  try {
    setScrollY(100);
    buttonOf("r6").focus();
    const place = viewportTop("r3");
    const before = viewportTop("r6") ?? 0;

    view.rerender(<List leaving={going} rows={ids(12).filter((id) => !going(id))} />);
    expect(viewportTop("r5")).toBe(place);
    expect(viewportTop("r6")).toBe(before - 2 * ROW_HEIGHT);
    expect(window.scrollY).toBe(100 - BANNER_HEIGHT);
  } finally {
    view.unmount();
  }
});

// The reader's own Dismiss takes away the row their hand is on, and the line above the list goes
// with it, as Mark all read's goes with the last unread thread. The row after it moves up into its
// place, and the line is made up for: that row stands where the row the press took away stood.
test("a focused row named as leaving, taken in a commit in which content above the list shrinks, leaves the row after it standing at its place", () => {
  const going = (id: string) => id === "r6";
  const view = render(<List banner leaving={going} rows={ids(12)} />);
  try {
    setScrollY(100);
    buttonOf("r6").focus();
    const place = viewportTop("r6");

    view.rerender(<List leaving={going} rows={ids(12).filter((id) => !going(id))} />);
    expect(viewportTop("r7")).toBe(place);
    expect(window.scrollY).toBe(100 - BANNER_HEIGHT);
  } finally {
    view.unmount();
  }
});

// The same on the list's last row: no row moves up into its place, so the row before it keeps
// where it stood, and the line above the list is made up for.
test("the last row, focused and named as leaving, taken in a commit in which content above the list shrinks, leaves the row before it where it stood", () => {
  const going = (id: string) => id === "r8";
  const view = render(<List banner leaving={going} rows={ids(8)} />);
  try {
    setScrollY(100);
    buttonOf("r8").focus();
    const stood = viewportTop("r7");

    view.rerender(<List leaving={going} rows={ids(7)} />);
    expect(viewportTop("r7")).toBe(stood);
    expect(window.scrollY).toBe(100 - BANNER_HEIGHT);
  } finally {
    view.unmount();
  }
});

// The focused row the press takes away is the last of its group, whose heading goes with it: the
// next group's heading and rows move up into the place the heading and the row held, and nothing
// scrolls.
test("a focused row named as leaving, the last of its group, lets the next group's heading and rows move up into the place it and its heading held", () => {
  const going = (id: string) => id === "r5";
  const view = render(<List headings={["r5", "r6"]} leaving={going} rows={ids(12)} />);
  try {
    setScrollY(100);
    buttonOf("r5").focus();
    const place = headingTop("r5");

    const kept = ids(12).filter((id) => !going(id));
    view.rerender(<List headings={["r6"]} leaving={going} rows={kept} />);
    expect(headingTop("r6")).toBe(place);
    expect(viewportTop("r6")).toBe((place ?? 0) + ROW_HEIGHT);
    expect(window.scrollY).toBe(100);
  } finally {
    view.unmount();
  }
});

// The same banner goes with no row of the reader's press leaving above the focused row: a live
// update took it. The rows in view stay where they were.
test("content above the list that shrinks with no named row leaving above the focused row is compensated, and the rows in view stay", () => {
  const going = (id: string) => id === "r9";
  const view = render(<List banner leaving={going} rows={ids(12)} />);
  try {
    setScrollY(100);
    buttonOf("r6").focus();
    const inView = () => ["r5", "r6", "r7"].map((id) => viewportTop(id));
    const seen = inView();

    view.rerender(<List leaving={going} rows={ids(12)} />);
    expect(inView()).toEqual(seen);
    expect(window.scrollY).toBe(100 - BANNER_HEIGHT);
  } finally {
    view.unmount();
  }
});

// Rows above the focused row are named as leaving for the whole time the press's write is out, so
// a commit can name them while none of them goes. In such a commit the banner goes: the shrink is
// compensated as any change above the list is, and the rows in view stay where they were.
test("content above the list that shrinks while named rows above the focused row stay is compensated, and the rows in view stay", () => {
  const going = (id: string) => id === "r3" || id === "r4";
  const view = render(<List banner leaving={going} rows={ids(12)} />);
  try {
    setScrollY(100);
    buttonOf("r6").focus();
    const inView = () => ["r5", "r6", "r7"].map((id) => viewportTop(id));
    const seen = inView();

    view.rerender(<List leaving={going} rows={ids(12)} />);
    expect(inView()).toEqual(seen);
    expect(window.scrollY).toBe(100 - BANNER_HEIGHT);
  } finally {
    view.unmount();
  }
});

// Content that grows above the list in the commit in which named rows leave is compensated, as a
// shrink is: the rows move up by the named rows' space alone.
test("content above the list that grows in the commit in which named rows leave is still compensated", () => {
  const going = (id: string) => id === "r3" || id === "r4";
  const view = render(<List leaving={going} rows={ids(12)} />);
  try {
    setScrollY(100);
    buttonOf("r6").focus();
    const before = viewportTop("r6") ?? 0;

    view.rerender(<List banner leaving={going} rows={ids(12).filter((id) => !going(id))} />);
    expect(viewportTop("r6")).toBe(before - 2 * ROW_HEIGHT);
    expect(window.scrollY).toBe(100 + BANNER_HEIGHT);
  } finally {
    view.unmount();
  }
});

// A refetch drops a row the reader did not press on, in the same commit as the row their press
// takes away. That row's going is compensated as any shift they did not cause is, and the
// focused row moves up by the row they pressed on alone.
test("a row the host does not name, going in the same commit as a row it names, is still compensated", () => {
  const going = (id: string) => id === "r3";
  const view = render(<List leaving={going} rows={ids(12)} />);
  try {
    setScrollY(100);
    buttonOf("r8").focus();
    const before = viewportTop("r8") ?? 0;

    // r3 goes by the reader's press, r5 by the refetch.
    const kept = ids(12).filter((id) => id !== "r3" && id !== "r5");
    view.rerender(<List leaving={going} rows={kept} />);
    expect(viewportTop("r8")).toBe(before - ROW_HEIGHT);
    expect(window.scrollY).toBe(100 - ROW_HEIGHT);
  } finally {
    view.unmount();
  }
});

// The bar's Snooze: the host names its rows while the write is out and holds them in their group,
// then moves them into Later, after the focused row, in the commit in which it stops naming them.
test("named rows that move to a group after the focused row, in a commit that no longer names them, let it move up all the same", () => {
  const going = (id: string) => id === "r3" || id === "r4";
  const view = render(<List leaving={going} rows={ids(12)} />);
  try {
    setScrollY(100);
    buttonOf("r6").focus();
    const place = viewportTop("r3");

    const moved = [...ids(12).filter((id) => !going(id)), "r3", "r4"];
    view.rerender(<List groups={{ r3: "later", r4: "later" }} rows={moved} />);
    expect(viewportTop("r5")).toBe(place);
    expect(window.scrollY).toBe(100);
  } finally {
    view.unmount();
  }
});

// The rows the press takes away are the last of their group, whose heading goes with them: the
// next group's heading and rows move up into the place the heading and the rows held.
test("a group's heading that goes with the last named rows under it lets the focused row move up by its place too", () => {
  const going = (id: string) => id === "r3" || id === "r4";
  const view = render(<List headings={["r3", "r5"]} leaving={going} rows={ids(12)} />);
  try {
    setScrollY(100);
    buttonOf("r6").focus();
    const place = headingTop("r3");

    const kept = ids(12).filter((id) => !going(id));
    view.rerender(<List headings={["r5"]} leaving={going} rows={kept} />);
    expect(headingTop("r5")).toBe(place);
    expect(viewportTop("r5")).toBe((place ?? 0) + ROW_HEIGHT);
    expect(window.scrollY).toBe(100);
  } finally {
    view.unmount();
  }
});

// A press over two rows far apart: one the reader scrolled above the view, one in sight. Each
// frees its whole space, the part above the view included, so the rows in view move up by the
// row above it, and the focused row after both moves up by the two and ends in sight. Nothing
// scrolls: not the window, and not the focused control into view.
test("named rows that stood above the view free their whole space: the focused row moves up by all of it, ends in sight, and nothing scrolls", () => {
  const going = (id: string) => id === "r2" || id === "r7";
  const moves = watchScrollsIntoView();
  const view = render(<List leaving={going} rows={ids(12)} />);
  try {
    setScrollY(275);
    buttonOf("r8").focus();
    const before = viewportTop("r8") ?? 0;
    const inView = viewportTop("r5") ?? 0;
    // r2 stands wholly above the window.
    expect((viewportTop("r2") ?? 0) + ROW_HEIGHT).toBeLessThan(0);

    view.rerender(<List leaving={going} rows={ids(12).filter((id) => !going(id))} />);
    expect(viewportTop("r5")).toBe(inView - ROW_HEIGHT);
    expect(viewportTop("r8")).toBe(before - 2 * ROW_HEIGHT);
    expect(before - 2 * ROW_HEIGHT).toBeGreaterThanOrEqual(0);
    expect(window.scrollY).toBe(275);
    expect(moves.scrolled).toEqual([]);
  } finally {
    view.unmount();
    moves.restore();
  }
});

// The rows the press takes away, one above the view and one just below it, free their whole
// space. The focused row after them moves up across the window's bottom edge and is left partly
// in sight: `scrollToNearest` scrolls the window by the exact cut, without `scrollIntoView` and
// without the root's 5rem scroll-padding-bottom below 1280 px pushing it further.
test("a focused row the named rows leave partly in sight is scrolled by the exact cut, without root scroll padding", () => {
  Object.defineProperty(window, "innerHeight", { configurable: true, value: 580, writable: true });
  const style = document.documentElement.style;
  const padding = style.scrollPaddingBottom;
  style.scrollPaddingBottom = "5rem";
  const going = (id: string) => id === "r2" || id === "r16";
  const moves = watchScrollsIntoView();
  const view = render(<List leaving={going} rows={ids(20)} />);
  try {
    setScrollY(245);
    buttonOf("r17").focus();
    const before = viewportTop("r17") ?? 0;
    const after = before - 2 * ROW_HEIGHT;
    expect(after).toBeLessThan(580);
    expect(after + ROW_HEIGHT).toBeGreaterThan(580);
    const cut = Math.ceil(after + ROW_HEIGHT - 580);

    view.rerender(<List leaving={going} rows={ids(20).filter((id) => !going(id))} />);
    expect(window.scrollY).toBe(245 + cut);
    expect((viewportTop("r17") ?? 0) + ROW_HEIGHT).toBe(580);
    expect(moves.scrolled).toEqual([]);
  } finally {
    style.scrollPaddingBottom = padding;
    view.unmount();
    moves.restore();
  }
});

// In a named box the anchor keeps the place it last saw the hand's row at. Once the rows the
// press took away have let that row move up, the kept place is where the row stands now, so the
// next commit keeps it there rather than putting it back where it stood before. The box's own
// scroll anchoring is off from the press's commit until the next commit no press takes rows away
// in, which gives it back the value it had.
test("in a named box the kept place moves up with the focused row, and the next commit keeps the row there", async () => {
  const user = userEvent.setup();
  const going = (id: string) => id === "r3" || id === "r4";
  const kept = ids(12).filter((id) => !going(id));
  const view = render(<List leaving={going} rows={ids(12)} scroller={scrollerRef} />);
  try {
    const scroller = scrollerRef.current;
    if (scroller === null) throw new Error("scroller missing");
    scroller.style.setProperty("overflow-anchor", "auto");
    scroller.scrollTop = 100;
    buttonOf("r6").focus();
    const row = document.querySelector<HTMLElement>('[data-row="r6"]');
    if (row === null) throw new Error("r6 missing");
    // The pointer comes to rest on the row, which keeps its place.
    await user.hover(row);
    const place = viewportTop("r4");

    view.rerender(<List leaving={going} rows={kept} scroller={scrollerRef} />);
    expect(viewportTop("r6")).toBe(place);
    expect(scroller.scrollTop).toBe(100);
    expect(anchoringSeen.at(-1)).toBe("none");
    // Still off: no rendering update has run since. The next commit no press takes rows away in
    // gives it back, with the value the box had.
    expect(scroller.style.getPropertyValue("overflow-anchor")).toBe("none");

    // A row arrives at the end of the list, below the hand's row.
    view.rerender(<List leaving={going} rows={[...kept, "r13"]} scroller={scrollerRef} />);
    expect(viewportTop("r6")).toBe(place);
    expect(scroller.scrollTop).toBe(100);
    expect(anchoringSeen.at(-1)).toBe("auto");
    expect(scroller.style.getPropertyValue("overflow-anchor")).toBe("auto");
  } finally {
    view.unmount();
  }
});

// INFRA-3135, round 31 (the tester's red row of round 29, builds Job infra-3135-i-h5-g6tnd): in
// the Inbox at 1440x900 the pointer rests on a row below the rows the bar's Snooze took, while the
// focus stands on the mark of the row marked last, where the bar's hand-off parked it. The commit
// that takes those rows away lets the rows after them move up, the row under the pointer with
// them, and scrolls nothing. The anchor kept a place for that row from before, and the resize the
// commit makes of the list put the row back there, scrolling the whole list back by the rows'
// height after the hand-off had landed. The place kept for a row the commit did not put back is
// let go with the rows, so the resize moves nothing.
test("a resize after the rows a press took away have gone puts back no row under the pointer while the focus stood in another", () => {
  const sizes = installResizeObserver();
  const going = (id: string) => id === "r3" || id === "r4";
  const view = render(<List leaving={going} rows={ids(12)} scroller={scrollerRef} />);
  try {
    const scroller = scrollerRef.current;
    if (scroller === null) throw new Error("scroller missing");
    scroller.scrollTop = 100;
    // The pointer comes to rest on r8, 350 px down the box: the anchor keeps its place.
    const hovered = document.querySelector<HTMLElement>('[data-row="r8"]');
    if (hovered === null) throw new Error("r8 missing");
    fireEvent.pointerOver(hovered, { clientX: 20, clientY: 370 });
    const before = viewportTop("r8") ?? 0;
    // The hand-off parks the focus on r3's own control, with no move of the pointer's or scroll.
    buttonOf("r3").focus();

    view.rerender(
      <List leaving={going} rows={ids(12).filter((id) => !going(id))} scroller={scrollerRef} />
    );
    expect(viewportTop("r8")).toBe(before - 2 * ROW_HEIGHT);
    expect(scroller.scrollTop).toBe(100);
    // The list's own box is shorter by the two rows, which the engine reports after the commit.
    sizes.resize();
    expect(viewportTop("r8")).toBe(before - 2 * ROW_HEIGHT);
    expect(scroller.scrollTop).toBe(100);
  } finally {
    view.unmount();
    sizes.restore();
  }
});

/** A page's control row above the list, drawn as `components/ControlRow.tsx` draws its own: it
 *  arms the reader's press at its root (`ARM_ON_PRESS`), and in the commit that takes its chip
 *  away it hands the focus to its Filter, ahead of the list's own commit work, as the Inbox draws
 *  the row before the list. */
function Controls({ chip, onTakeOff }: { chip: boolean; onTakeOff?: () => void }): ReactNode {
  const filter = useRef<HTMLButtonElement>(null);
  useLayoutEffect(() => {
    if (!chip) filter.current?.focus();
  }, [chip]);
  return (
    <div data-control={CONTROL.controlRow} {...ARM_ON_PRESS}>
      <button ref={filter} type="button">
        Filter
      </button>
      {chip ? (
        <button onClick={onTakeOff} type="button">
          Remove Type: question filter
        </button>
      ) : null}
    </div>
  );
}

/** The list under that row, narrowed by its chip to the rows from r4 on: taking the chip off
 *  draws r1 to r3 again at the list's head in the same commit, as the Inbox draws again the
 *  threads its narrowing to questions left out. */
function Narrowed({ scroller }: { scroller?: RefObject<HTMLDivElement | null> }): ReactNode {
  const [narrowed, setNarrowed] = useState(true);
  return (
    <>
      <Controls chip={narrowed} onTakeOff={() => setNarrowed(false)} />
      <List rows={narrowed ? ids(9, 4) : ids(12)} scroller={scroller} />
    </>
  );
}

// The reader takes the list's only narrowing off from the keyboard with the window scrolled, and
// the rows it left out come back at the list's head, above the row nearest the list's centre.
// Their place is the control they pressed: the control row stays where it stood, the rows come
// back below it, and the Filter the row hands the focus to stands where it stood. Measured in the
// Inbox at 390x664 while the anchor held the centre row instead: the window went from 30 px to
// 1,269 px, and Filter stood at -1,140 px.
test("a press of the reader's in the control row above the list that draws rows back above the list's centre keeps that row where it stood, and the window does not scroll", async () => {
  const user = userEvent.setup();
  const view = render(<Narrowed />);
  try {
    setScrollY(100);
    const filter = screen.getByRole("button", { name: "Filter" });
    screen.getByRole("button", { name: "Remove Type: question filter" }).focus();
    const stood = filter.getBoundingClientRect().top;
    // With no press, a commit now would hold the row at the list's centre.
    expect(chosen()).toEqual({ id: "r8", via: "center" });

    await user.keyboard("{Enter}");
    expect(viewportTop("r1")).toBeDefined();
    expect(filter).toHaveFocus();
    expect(filter.getBoundingClientRect().top).toBe(stood);
    expect(window.scrollY).toBe(100);
  } finally {
    disarm();
    view.unmount();
  }
});

// WebKit leaves the focus on the body after a click on a button, so no focused element says the
// press was made in the control row: the anchor reads the press from the row itself.
test("a click in the control row that moves no focus, as WebKit's does, keeps that row where it stood while the narrowing it takes off draws rows back", () => {
  const view = render(<Narrowed />);
  try {
    setScrollY(100);
    const filter = screen.getByRole("button", { name: "Filter" });
    const chip = screen.getByRole("button", { name: "Remove Type: question filter" });
    expect(document.activeElement).toBe(document.body);
    const stood = filter.getBoundingClientRect().top;
    expect(chosen()).toEqual({ id: "r8", via: "center" });

    // The click alone, with no focus moved by it, as WebKit's on a button is: user-event focuses
    // the button it clicks.
    fireEvent.click(chip);
    expect(viewportTop("r1")).toBeDefined();
    expect(filter.getBoundingClientRect().top).toBe(stood);
    expect(window.scrollY).toBe(100);
  } finally {
    disarm();
    view.unmount();
  }
});

// The same rows drawn back by a live update while the focus rests on the row's Filter, with no
// press of the reader's: a change they did not make is made up for around the row at the list's
// centre, as any other is, so the rows in view stay where they were.
test("rows a live update draws above the list's centre while the focus rests in the control row are made up for, and the rows in view stay", () => {
  const page = (rows: string[]) => (
    <>
      <Controls chip />
      <List rows={rows} />
    </>
  );
  const view = render(page(ids(9, 4)));
  try {
    setScrollY(100);
    screen.getByRole("button", { name: "Filter" }).focus();
    const centre = viewportTop("r8");

    view.rerender(page(ids(12)));
    expect(viewportTop("r8")).toBe(centre);
    expect(window.scrollY).toBe(100 + 3 * ROW_HEIGHT);
  } finally {
    view.unmount();
  }
});

// The control row stands outside the box the rows scroll in, as the Inbox's stands above the list
// column beside its reading pane: the box's scroll moves nothing of the row, so the press leaves
// the box to hold the row at its centre, and the window does not move.
test("a press in a control row outside the box the rows scroll in leaves that box holding the row at its centre", async () => {
  const user = userEvent.setup();
  const view = render(<Narrowed scroller={scrollerRef} />);
  try {
    const scroller = scrollerRef.current;
    if (scroller === null) throw new Error("scroller missing");
    scroller.scrollTop = 100;
    screen.getByRole("button", { name: "Remove Type: question filter" }).focus();
    const centre = viewportTop("r8");

    await user.keyboard("{Enter}");
    expect(viewportTop("r8")).toBe(centre);
    expect(scroller.scrollTop).toBe(100 + 3 * ROW_HEIGHT);
    expect(window.scrollY).toBe(0);
  } finally {
    disarm();
    view.unmount();
  }
});

/** The page's own control row (`components/ControlRow.tsx`) above the list: its Filter trigger,
 *  and the one chip of the narrowing to the rows from r4 on, which the reader's press takes off,
 *  drawing r1 to r3 again at the list's head in the same commit, as `Narrowed` does. */
function NarrowedByControlRow(): ReactNode {
  const [narrowed, setNarrowed] = useState(true);
  return (
    <>
      <ControlRow
        chips={
          narrowed ? (
            <button key="type" onClick={() => setNarrowed(false)} type="button">
              Remove Type: question filter
            </button>
          ) : undefined
        }
        trigger={
          <button data-control={CONTROL.panelTrigger} type="button">
            Filter
          </button>
        }
      />
      <List rows={narrowed ? ids(9, 4) : ids(12)} />
    </>
  );
}

/**
 * The reader takes the only narrowing of `NarrowedByControlRow` off from the keyboard, with the
 * control row 60 px down the page and 104 px tall, its Filter trigger on its first line, 44 px
 * tall, and the window scrolled to `scrolled`. Expects the focus on Filter, the scrolls into view
 * and of the window to be `moves`, in order, and Filter's top at the window's top once the press
 * is done.
 */
async function expectChipsHandOff({
  moves: expected,
  scrolled,
}: {
  readonly moves: unknown[];
  readonly scrolled: number;
}): Promise<void> {
  const user = userEvent.setup();
  const view = render(<NarrowedByControlRow />);
  // Every scroll into view and every scroll of the window, in order: a scroll into view moves the
  // window the least that shows the element whole, as `{ block: "nearest" }` does.
  const moves: unknown[] = [];
  const scrolls = spyOn(Element.prototype, "scrollIntoView").mockImplementation(function (
    this: Element,
    options?: boolean | ScrollIntoViewOptions
  ) {
    moves.push({ of: this.textContent, options, scroll: "into view" });
    const box = this.getBoundingClientRect();
    if (box.top < 0) setScrollY(window.scrollY + box.top);
    else if (box.bottom > VIEWPORT_HEIGHT)
      setScrollY(window.scrollY + box.bottom - VIEWPORT_HEIGHT);
  });
  const scrollBy = window.scrollBy;
  window.scrollBy = ((x: number | ScrollToOptions, y?: number) => {
    moves.push({ by: typeof x === "number" ? y : x.top, scroll: "window" });
    scrollBy(x as number, y as number);
  }) as typeof window.scrollBy;
  try {
    const row = document.querySelector(`[data-control="${CONTROL.controlRow}"]`);
    const filter = screen.getByRole("button", { name: "Filter" });
    if (row === null) throw new Error("no control row is drawn");
    row.getBoundingClientRect = () => new DOMRect(0, 60 - window.scrollY, 100, 104);
    filter.getBoundingClientRect = () => new DOMRect(0, 60 - window.scrollY, 100, 44);
    setScrollY(scrolled);
    screen.getByRole("button", { name: "Remove Type: question filter" }).focus();

    await user.keyboard("{Enter}");
    expect(filter).toHaveFocus();
    expect(moves).toEqual(expected);
    expect(filter.getBoundingClientRect().top).toBe(0);
  } finally {
    scrolls.mockRestore();
    disarm();
    view.unmount();
  }
}

// The reader takes the list's only narrowing off from the keyboard with the window scrolled so the
// control row's Filter trigger, 44 px tall, stands 20 px above the window's top. The row hands the
// focus to Filter and lands it with `landWhole` (`components/focus-place.ts`): no scroll of the
// engine's own, and a control not wholly in sight scrolled to its nearest edge, one wholly out of
// sight as well, where a component's own hand-off (`landFirstInSight`) lands only a control in
// sight and shows only one partly cut. This is the partly cut case. The anchor then puts the row
// back where the commit found it, which leaves Filter as cut as before, so the landing shows it
// whole again once the commit is done. A plain `focus()` scrolled it in Chromium and Firefox only
// to have the anchor undo that scroll.
test("the control row's chips hand-off shows its landing whole again after the anchor puts the row back where the commit found it", async () => {
  await expectChipsHandOff({
    moves: [
      { of: "Filter", options: { block: "nearest", inline: "nearest" }, scroll: "into view" },
      // The anchor puts the row back where the commit found it, 20 px above the window's top.
      { by: 20, scroll: "window" },
      { of: "Filter", options: { block: "nearest", inline: "nearest" }, scroll: "into view" },
    ],
    scrolled: 80,
  });
});

// The same press with the window scrolled so far that the Filter trigger stands wholly above the
// window's top, 140 px above it. A keyboard focus the reader cannot see is lost to them, so the
// hand-off shows its landing whole at its nearest edge here too, as the plain `focus()` it
// replaced scrolled it in: at once, and again once the anchor has put the row back where the
// commit found it, which takes Filter out of sight again. The landing showed only a control partly
// in sight, and left this one where it stood, out of sight.
test("the control row's chips hand-off shows a landing wholly out of sight at its nearest edge, again after the anchor puts the row back", async () => {
  await expectChipsHandOff({
    moves: [
      { of: "Filter", options: { block: "nearest", inline: "nearest" }, scroll: "into view" },
      // The anchor puts the row back where the commit found it, 140 px above the window's top.
      { by: 140, scroll: "window" },
      { of: "Filter", options: { block: "nearest", inline: "nearest" }, scroll: "into view" },
    ],
    scrolled: 200,
  });
});
