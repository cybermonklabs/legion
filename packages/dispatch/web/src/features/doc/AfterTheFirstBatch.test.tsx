import { afterEach, beforeEach, expect, test } from "bun:test";
// biome-ignore lint/style/noRestrictedImports: the bare pointerdown the chrome's space listens for, which user-event has no form for
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";

import { AfterTheFirstBatch, ChromeAfterTheFirstBatch } from "./AfterTheFirstBatch";
import { theFirstBatchIsAwaited } from "./first-batch";

// happy-dom lays nothing out and has no `ResizeObserver`, so the geometry is this harness: one
// box, whatever is rendered, whose rectangle the rows set, a page that scrolls by
// `window.scrollBy`, and observers the rows fire. What the chrome rows read is the box's own
// height while the space stands in it and again once the chrome has replaced it.
let box = { height: 0, top: 0 };
let scrolled = 0;
const observers = new Set<() => void>();
let restore: (() => void) | undefined;

/** Fires every live `ResizeObserver`, as the browser does after it has laid the page out. */
function laidOut(): void {
  for (const fire of [...observers]) fire();
}

beforeEach(() => {
  box = { height: 0, top: 0 };
  scrolled = 0;
  observers.clear();
  const rect = Element.prototype.getBoundingClientRect;
  const scrollBy = window.scrollBy;
  const scrollY = Object.getOwnPropertyDescriptor(window, "scrollY");
  const observer = globalThis.ResizeObserver;
  Element.prototype.getBoundingClientRect = function boxOf(): DOMRect {
    return { ...box } as DOMRect;
  };
  window.scrollBy = ((_x: number, y: number) => {
    scrolled += y;
  }) as never;
  Object.defineProperty(window, "scrollY", { configurable: true, get: () => scrolled });
  globalThis.ResizeObserver = class {
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
  } as unknown as typeof ResizeObserver;
  restore = () => {
    Element.prototype.getBoundingClientRect = rect;
    window.scrollBy = scrollBy;
    globalThis.ResizeObserver = observer;
    if (scrollY !== undefined) Object.defineProperty(window, "scrollY", scrollY);
  };
});

// The gate is one page's state, so each row leaves it as it found it: nothing awaited, which is
// what every page with no document on it reads.
afterEach(() => {
  cleanup();
  theFirstBatchIsAwaited(false);
  restore?.();
  restore = undefined;
});

test("a page with no document draws the part at once, and never its space", () => {
  render(<AfterTheFirstBatch space={<p>the space</p>}>{<p>the part</p>}</AfterTheFirstBatch>);

  expect(screen.getByText("the part")).toBeTruthy();
  expect(screen.queryByText("the space")).toBeNull();
});

test("a document address keeps the part's space until the editor's first batch", () => {
  act(() => theFirstBatchIsAwaited(true));
  render(<AfterTheFirstBatch space={<p>the space</p>}>{<p>the part</p>}</AfterTheFirstBatch>);

  expect(screen.getByText("the space")).toBeTruthy();

  act(() => theFirstBatchIsAwaited(false));

  expect(screen.getByText("the part")).toBeTruthy();
  expect(screen.queryByText("the space")).toBeNull();
});

test("a reader who has reached for the part has it before the batch", () => {
  act(() => theFirstBatchIsAwaited(true));
  render(
    <AfterTheFirstBatch reached space={<p>the space</p>}>
      {<p>the part</p>}
    </AfterTheFirstBatch>
  );

  expect(screen.getByText("the part")).toBeTruthy();
});

test("chrome standing above the reader's screen moves the page by what it grew", async () => {
  act(() => theFirstBatchIsAwaited(true));
  box = { height: 52, top: -400 };
  render(
    <ChromeAfterTheFirstBatch label="Show the chrome" space={<p>the space</p>}>
      {<p>the chrome</p>}
    </ChromeAfterTheFirstBatch>
  );
  expect(screen.getByText("the space")).toBeTruthy();

  // The chrome lands in two commits, as a chunk's fallback and then the part itself do: the
  // first is the space's own height again, and only the second grows the box.
  await act(async () => {
    theFirstBatchIsAwaited(false);
  });
  laidOut();
  expect(scrolled).toBe(0);

  box = { height: 304, top: -400 };
  laidOut();

  expect(screen.getByText("the chrome")).toBeTruthy();
  expect(scrolled).toBe(252);
});

test("chrome the reader can see lands under their eyes and moves nothing", async () => {
  act(() => theFirstBatchIsAwaited(true));
  box = { height: 52, top: 69 };
  render(
    <ChromeAfterTheFirstBatch label="Show the chrome" space={<p>the space</p>}>
      {<p>the chrome</p>}
    </ChromeAfterTheFirstBatch>
  );

  await act(async () => {
    theFirstBatchIsAwaited(false);
  });
  box = { height: 304, top: 69 };
  laidOut();

  expect(screen.getByText("the chrome")).toBeTruthy();
  expect(scrolled).toBe(0);
});

test("a growth something else has already answered for is not answered for twice", async () => {
  act(() => theFirstBatchIsAwaited(true));
  box = { height: 52, top: -400 };
  render(
    <ChromeAfterTheFirstBatch label="Show the chrome" space={<p>the space</p>}>
      {<p>the chrome</p>}
    </ChromeAfterTheFirstBatch>
  );

  await act(async () => {
    theFirstBatchIsAwaited(false);
  });
  // The document's own keeper answered this growth first, so the scroll is not this module's
  // to make again.
  box = { height: 304, top: -400 };
  scrolled = 252;
  laidOut();

  expect(scrolled).toBe(252);
});

test("a reader who presses the chrome's space has the chrome then", () => {
  act(() => theFirstBatchIsAwaited(true));
  const { container } = render(
    <ChromeAfterTheFirstBatch label="Show the chrome" space={<p>the space</p>}>
      {<p>the chrome</p>}
    </ChromeAfterTheFirstBatch>
  );
  const space = container.firstElementChild;
  if (space === null) throw new Error("the chrome kept no space");

  // A bare `pointerdown` on the space, which is the one event this release listens for:
  // user-event has no form that stops at the press without the pointer-up after it.
  act(() => {
    fireEvent.pointerDown(space);
  });

  expect(screen.getByText("the chrome")).toBeTruthy();
});

// The one Tab stop of a held box, whatever the space it keeps holds: the issue's tab strip
// keeps a row of `aria-hidden` spans, and a reader with no pointer tabbed past it into the
// document with no way to ask for the tabs at all (dispatch://INFRA-341).
test("the space a reader cannot press names the chrome it is keeping, and one Tab brings it", () => {
  act(() => theFirstBatchIsAwaited(true));
  render(
    <ChromeAfterTheFirstBatch label="Show the chrome" space={<p>the space</p>}>
      {<p>the chrome</p>}
    </ChromeAfterTheFirstBatch>
  );
  const reach = screen.getByRole("button", { name: "Show the chrome" });

  act(() => {
    reach.focus();
  });

  expect(screen.getByText("the chrome")).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Show the chrome" })).toBeNull();
});

// The handover takes the control the reader is standing on off the page - the space and the
// chrome are different elements in the same slot - and nothing put their focus anywhere, so it
// fell to `<body>` and their next Tab started again at the top of the page
// (dispatch://INFRA-341).
test("the handover leaves the reader's focus in the box the chrome landed in", () => {
  act(() => theFirstBatchIsAwaited(true));
  // A space holding a control of its own, as the issue header's does (its `aria-disabled` Pin):
  // whatever a reader stands on while the box is held goes off the page with the swap.
  const { container } = render(
    <ChromeAfterTheFirstBatch
      label="Show the chrome"
      space={
        <button aria-disabled="true" type="button">
          the held control
        </button>
      }
    >
      {<p>the chrome</p>}
    </ChromeAfterTheFirstBatch>
  );
  const held = container.firstElementChild;
  if (held === null) throw new Error("the chrome kept no box");

  act(() => {
    screen.getByRole("button", { name: "the held control" }).focus();
  });

  expect(screen.getByText("the chrome")).toBeTruthy();
  expect(document.activeElement).not.toBe(document.body);
  expect(held).toHaveFocus();
});
