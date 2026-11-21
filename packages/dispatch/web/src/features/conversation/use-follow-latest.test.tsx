import { expect, spyOn, test } from "bun:test";
import { act, render } from "@testing-library/react";
import type { ReactNode } from "react";

import { thePageLeftItThere } from "../doc/readers-place";
import { useFollowLatest } from "./use-follow-latest";

// The composer sits at document top 0; the newest card (topmost `[data-turn]`) sits below it.
// `useFollowLatest` reads both elements' `getBoundingClientRect()`, so the harness renders
// stand-ins for them rather than a real layout engine.
const COMPOSER_HEIGHT = 140;
const NEWEST_TURN_DOC_TOP = 300;
const RESTING_SCROLL_Y = NEWEST_TURN_DOC_TOP - COMPOSER_HEIGHT;

function installScrollLayoutMocks(): { restore: () => void } {
  const rectSpy = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ) {
    const isComposer = this.getAttribute("aria-label") === "Comment composer";
    const isTurn = this.hasAttribute("data-turn");
    const docTop = isComposer ? 0 : isTurn ? NEWEST_TURN_DOC_TOP : 0;
    const height = isComposer ? COMPOSER_HEIGHT : isTurn ? 50 : 0;
    const top = docTop - window.scrollY;
    return {
      bottom: top + height,
      height,
      left: 0,
      right: 100,
      toJSON: () => ({}),
      top,
      width: 100,
      x: 0,
      y: top,
    };
  });
  const originalScrollTo = window.scrollTo;
  window.scrollTo = (options?: ScrollToOptions | number, y?: number) => {
    const top = typeof options === "number" ? y : options?.top;
    if (top !== undefined) {
      Object.defineProperty(window, "scrollY", { configurable: true, value: top, writable: true });
    }
  };
  return {
    restore: () => {
      rectSpy.mockRestore();
      window.scrollTo = originalScrollTo;
    },
  };
}

function Harness({
  enabled = true,
  itemSeqs,
  onFirstPlacement,
  ownSendCount,
}: {
  enabled?: boolean;
  itemSeqs: number[];
  onFirstPlacement?: () => void;
  ownSendCount: number;
}): ReactNode {
  useFollowLatest({ enabled, latestItemSeq: itemSeqs.at(0), onFirstPlacement, ownSendCount });
  return (
    <div>
      <form aria-label="Comment composer" />
      {itemSeqs.map((seq) => (
        <div data-turn={`message:${seq}`} key={seq} />
      ))}
    </div>
  );
}

test("scrolls to the newest turn on mount", () => {
  const scrollY = Object.getOwnPropertyDescriptor(window, "scrollY");
  const layout = installScrollLayoutMocks();
  let unmount: (() => void) | undefined;

  try {
    Object.defineProperty(window, "scrollY", { configurable: true, value: 4_500, writable: true });
    unmount = render(<Harness itemSeqs={[1]} ownSendCount={0} />).unmount;

    expect(window.scrollY).toBe(RESTING_SCROLL_Y);
  } finally {
    unmount?.();
    layout.restore();
    if (scrollY === undefined) {
      Reflect.deleteProperty(window, "scrollY");
    } else {
      Object.defineProperty(window, "scrollY", scrollY);
    }
  }
});

test("scrolls to reveal an own send even when the seq change lands on a later render", () => {
  const scrollY = Object.getOwnPropertyDescriptor(window, "scrollY");
  const layout = installScrollLayoutMocks();
  let unmount: (() => void) | undefined;

  try {
    const view = render(<Harness itemSeqs={[1]} ownSendCount={0} />);
    unmount = view.unmount;
    expect(window.scrollY).toBe(RESTING_SCROLL_Y);

    // The composer's mutation bumps `ownSendCount` in the same tick it invalidates the events
    // query; the invalidated refetch — and the new `latestItemSeq` it produces — lands on a
    // later render. This render observes only the `ownSendCount` change; `itemSeqs` is unchanged.
    Object.defineProperty(window, "scrollY", { configurable: true, value: 4_500, writable: true });
    act(() => {
      window.dispatchEvent(new Event("scroll"));
    });
    act(() => {
      view.rerender(<Harness itemSeqs={[1]} ownSendCount={1} />);
    });

    // The own send follows regardless of scroll position, even before the refetch lands.
    expect(window.scrollY).toBe(RESTING_SCROLL_Y);
  } finally {
    unmount?.();
    layout.restore();
    if (scrollY === undefined) {
      Reflect.deleteProperty(window, "scrollY");
    } else {
      Object.defineProperty(window, "scrollY", scrollY);
    }
  }
});

test("does not mistake a later arrival for the send once ownSendCount is recorded", () => {
  const scrollY = Object.getOwnPropertyDescriptor(window, "scrollY");
  const layout = installScrollLayoutMocks();
  let unmount: (() => void) | undefined;

  try {
    const view = render(<Harness itemSeqs={[1]} ownSendCount={0} />);
    unmount = view.unmount;
    expect(window.scrollY).toBe(RESTING_SCROLL_Y);

    // The live event feed can invalidate the events query independently of the mutation's own
    // callback, sometimes landing the seq change *before* `ownSendCount` catches up. While the
    // reader is still pinned this just follows, exactly like any other arrival.
    act(() => {
      view.rerender(<Harness itemSeqs={[2, 1]} ownSendCount={0} />);
    });
    expect(window.scrollY).toBe(RESTING_SCROLL_Y);

    // The reader scrolls away, then `ownSendCount` finally catches up to the send that already
    // landed above — this must not be treated as a second, still-pending own send.
    Object.defineProperty(window, "scrollY", { configurable: true, value: 4_500, writable: true });
    act(() => {
      window.dispatchEvent(new Event("scroll"));
    });
    act(() => {
      view.rerender(<Harness itemSeqs={[2, 1]} ownSendCount={1} />);
    });
    expect(window.scrollY).toBe(RESTING_SCROLL_Y);

    // A stranger's turn arriving next must not be mistaken for the already-recorded send.
    Object.defineProperty(window, "scrollY", { configurable: true, value: 4_500, writable: true });
    act(() => {
      window.dispatchEvent(new Event("scroll"));
    });
    act(() => {
      view.rerender(<Harness itemSeqs={[3, 2, 1]} ownSendCount={1} />);
    });
    expect(window.scrollY).toBe(4_500);
  } finally {
    unmount?.();
    layout.restore();
    if (scrollY === undefined) {
      Reflect.deleteProperty(window, "scrollY");
    } else {
      Object.defineProperty(window, "scrollY", scrollY);
    }
  }
});

test("does not follow another turn arriving while the reader is away", () => {
  const scrollY = Object.getOwnPropertyDescriptor(window, "scrollY");
  const layout = installScrollLayoutMocks();
  let unmount: (() => void) | undefined;

  try {
    const view = render(<Harness itemSeqs={[1]} ownSendCount={0} />);
    unmount = view.unmount;
    expect(window.scrollY).toBe(RESTING_SCROLL_Y);

    Object.defineProperty(window, "scrollY", { configurable: true, value: 4_500, writable: true });
    act(() => {
      window.dispatchEvent(new Event("scroll"));
    });

    // A turn from someone else arrives; `ownSendCount` never changes.
    act(() => {
      view.rerender(<Harness itemSeqs={[2, 1]} ownSendCount={0} />);
    });

    expect(window.scrollY).toBe(4_500);
  } finally {
    unmount?.();
    layout.restore();
    if (scrollY === undefined) {
      Reflect.deleteProperty(window, "scrollY");
    } else {
      Object.defineProperty(window, "scrollY", scrollY);
    }
  }
});

test("follows another turn when the reader returns to the document top", () => {
  const scrollY = Object.getOwnPropertyDescriptor(window, "scrollY");
  const layout = installScrollLayoutMocks();
  let unmount: (() => void) | undefined;

  try {
    const view = render(<Harness itemSeqs={[1]} ownSendCount={0} />);
    unmount = view.unmount;
    Object.defineProperty(window, "scrollY", { configurable: true, value: 0, writable: true });
    act(() => {
      window.dispatchEvent(new Event("scroll"));
      view.rerender(<Harness itemSeqs={[2, 1]} ownSendCount={0} />);
    });

    expect(window.scrollY).toBe(RESTING_SCROLL_Y);
  } finally {
    unmount?.();
    layout.restore();
    if (scrollY === undefined) {
      Reflect.deleteProperty(window, "scrollY");
    } else {
      Object.defineProperty(window, "scrollY", scrollY);
    }
  }
});

test("does not follow another turn after a small reader scroll", () => {
  const scrollY = Object.getOwnPropertyDescriptor(window, "scrollY");
  const layout = installScrollLayoutMocks();
  let unmount: (() => void) | undefined;

  try {
    const view = render(<Harness itemSeqs={[1]} ownSendCount={0} />);
    unmount = view.unmount;
    const readerPosition = RESTING_SCROLL_Y + 37;
    Object.defineProperty(window, "scrollY", {
      configurable: true,
      value: readerPosition,
      writable: true,
    });

    act(() => {
      view.rerender(<Harness itemSeqs={[2, 1]} ownSendCount={0} />);
    });

    expect(window.scrollY).toBe(readerPosition);
  } finally {
    unmount?.();
    layout.restore();
    if (scrollY === undefined) {
      Reflect.deleteProperty(window, "scrollY");
    } else {
      Object.defineProperty(window, "scrollY", scrollY);
    }
  }
});

// `IssuePage` forgets a tab-switch scroll recorded while the list was loading when this fires,
// so it must fire exactly once, with the reader already placed, and never again for a return.
test("tells the caller once, after the first placement, and not on later follows", () => {
  const scrollY = Object.getOwnPropertyDescriptor(window, "scrollY");
  const layout = installScrollLayoutMocks();
  let unmount: (() => void) | undefined;
  const placedAt: number[] = [];
  const onFirstPlacement = () => placedAt.push(window.scrollY);

  try {
    Object.defineProperty(window, "scrollY", { configurable: true, value: 0, writable: true });
    const view = render(
      <Harness itemSeqs={[]} onFirstPlacement={onFirstPlacement} ownSendCount={0} />
    );
    unmount = view.unmount;
    expect(placedAt).toEqual([]);

    act(() => {
      view.rerender(
        <Harness itemSeqs={[1]} onFirstPlacement={onFirstPlacement} ownSendCount={0} />
      );
    });
    expect(placedAt).toEqual([RESTING_SCROLL_Y]);

    act(() => {
      view.rerender(
        <Harness itemSeqs={[2, 1]} onFirstPlacement={onFirstPlacement} ownSendCount={0} />
      );
    });
    act(() => {
      view.rerender(
        <Harness itemSeqs={[2, 1]} onFirstPlacement={onFirstPlacement} ownSendCount={1} />
      );
    });
    act(() => {
      view.rerender(
        <Harness
          enabled={false}
          itemSeqs={[2, 1]}
          onFirstPlacement={onFirstPlacement}
          ownSendCount={1}
        />
      );
    });
    act(() => {
      view.rerender(
        <Harness itemSeqs={[3, 2, 1]} onFirstPlacement={onFirstPlacement} ownSendCount={1} />
      );
    });

    expect(placedAt).toEqual([RESTING_SCROLL_Y]);
  } finally {
    unmount?.();
    layout.restore();
    if (scrollY === undefined) {
      Reflect.deleteProperty(window, "scrollY");
    } else {
      Object.defineProperty(window, "scrollY", scrollY);
    }
  }
});

test("the jump to the newest turn is the page's own move, and the reader's scroll is not", () => {
  const scrollY = Object.getOwnPropertyDescriptor(window, "scrollY");
  const layout = installScrollLayoutMocks();
  let unmount: (() => void) | undefined;

  try {
    Object.defineProperty(window, "scrollY", { configurable: true, value: 4_500, writable: true });
    unmount = render(<Harness itemSeqs={[1]} ownSendCount={0} />).unmount;

    // Pinning the newest turn under the composer is the page moving what it draws, and it goes
    // through the one call that records it (`putTheBoxAt`, `features/doc/readers-place.ts`): a
    // landing correction watching the same page reads the placement as the page's own and goes
    // on watching, rather than as the reader taking the page over (`refocus-when-settled.ts`).
    expect(window.scrollY).toBe(RESTING_SCROLL_Y);
    expect(thePageLeftItThere(null)).toBe(true);

    // The reader scrolls: the page stands where nothing of the page's own left it, and the
    // correction stops.
    Object.defineProperty(window, "scrollY", { configurable: true, value: 900, writable: true });

    expect(thePageLeftItThere(null)).toBe(false);
  } finally {
    unmount?.();
    layout.restore();
    if (scrollY === undefined) {
      Reflect.deleteProperty(window, "scrollY");
    } else {
      Object.defineProperty(window, "scrollY", scrollY);
    }
  }
});

/** Where the Inbox's reading column stands on the page, and how tall it is: a conversation
 *  drawn in a pane of its own scrolls in that column, and the window's own offset is then a
 *  number the reader never moves. */
const COLUMN_TOP = 120;
const COLUMN_HEIGHT = 600;

/** The same composer and turn, drawn in that column. happy-dom lays nothing out, computes no
 *  overflow and scrolls nothing, so what `whatScrolls` reads to find the box - its overflow and
 *  its content standing taller than it does - is stated here, as are the rectangles, which are
 *  read from the column's own top and move with the column's own scroll. */
function installColumnLayoutMocks(): {
  column: HTMLElement;
  container: HTMLElement;
  restore: () => void;
  scrolls(top: number): void;
} {
  let scrolled = 0;
  const column = document.createElement("div");
  column.style.overflowY = "auto";
  Object.defineProperty(column, "clientHeight", { configurable: true, get: () => COLUMN_HEIGHT });
  Object.defineProperty(column, "scrollHeight", { configurable: true, get: () => 10_000 });
  Object.defineProperty(column, "scrollTop", {
    configurable: true,
    get: () => scrolled,
    set: (next: number) => {
      scrolled = next;
    },
  });
  column.scrollTo = ((options?: ScrollToOptions | number, y?: number) => {
    const top = typeof options === "number" ? y : options?.top;
    if (top !== undefined) {
      scrolled = top;
    }
  }) as typeof column.scrollTo;
  const container = document.createElement("div");
  column.append(container);
  document.body.append(column);
  const rectSpy = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ) {
    const isColumn = this === column;
    const isComposer = this.getAttribute("aria-label") === "Comment composer";
    const isTurn = this.hasAttribute("data-turn");
    const height = isColumn ? COLUMN_HEIGHT : isComposer ? COMPOSER_HEIGHT : isTurn ? 50 : 0;
    const top = isColumn ? COLUMN_TOP : COLUMN_TOP + (isTurn ? NEWEST_TURN_DOC_TOP : 0) - scrolled;
    return {
      bottom: top + height,
      height,
      left: 0,
      right: 100,
      toJSON: () => ({}),
      top,
      width: 100,
      x: 0,
      y: top,
    } as DOMRect;
  });
  return {
    column,
    container,
    restore: () => {
      rectSpy.mockRestore();
      column.remove();
    },
    // What the reader does: the column scrolls and tells whoever is listening, as the browser
    // does - a column's scroll event does not bubble, so only a capturing listener hears it.
    scrolls(top: number) {
      scrolled = top;
      column.dispatchEvent(new Event("scroll"));
    },
  };
}

// The conversation drawn in the Inbox's reading column: every reading of where the reader is
// is taken down that column (`whereTheReaderIs`, `features/doc/readers-place.ts`). Read from
// the window instead, the placement would be written to a page the column does not move, and
// the window's own offset - a number this reader never moves - would say they were still at
// the newest turn whatever they had scrolled past.
test("the newest turn is pinned in the reading column, and the reader's own scroll there is kept", () => {
  const scrollY = Object.getOwnPropertyDescriptor(window, "scrollY");
  const layout = installColumnLayoutMocks();
  let unmount: (() => void) | undefined;

  try {
    Object.defineProperty(window, "scrollY", { configurable: true, value: 0, writable: true });
    layout.column.scrollTop = 4_500;
    const view = render(<Harness itemSeqs={[1]} ownSendCount={0} />, {
      container: layout.container,
    });
    unmount = view.unmount;

    // The column is put where the newest turn sits under the composer, and the page the column
    // does not move stands where it stood.
    expect(layout.column.scrollTop).toBe(RESTING_SCROLL_Y);
    expect(window.scrollY).toBe(0);
    expect(thePageLeftItThere(layout.column)).toBe(true);

    // The reader scrolls the column away from the newest turn: an arrival from someone else
    // leaves them where they are, which is read from the column and not from the window.
    act(() => {
      layout.scrolls(4_500);
      view.rerender(<Harness itemSeqs={[2, 1]} ownSendCount={0} />);
    });

    expect(layout.column.scrollTop).toBe(4_500);

    // Back at the top of the column, the next arrival is followed again.
    act(() => {
      layout.scrolls(0);
      view.rerender(<Harness itemSeqs={[3, 2, 1]} ownSendCount={0} />);
    });

    expect(layout.column.scrollTop).toBe(RESTING_SCROLL_Y);
  } finally {
    unmount?.();
    layout.restore();
    if (scrollY === undefined) {
      Reflect.deleteProperty(window, "scrollY");
    } else {
      Object.defineProperty(window, "scrollY", scrollY);
    }
  }
});

// The same conversation on its own page, where `whatScrolls` finds no column and the page is
// the box: the numbers are the ones this surface has always kept.
test("on a document page the newest turn is pinned by the page's own scroll, unchanged", () => {
  const scrollY = Object.getOwnPropertyDescriptor(window, "scrollY");
  const layout = installScrollLayoutMocks();
  let unmount: (() => void) | undefined;

  try {
    Object.defineProperty(window, "scrollY", { configurable: true, value: 4_500, writable: true });
    const view = render(<Harness itemSeqs={[1]} ownSendCount={0} />);
    unmount = view.unmount;

    expect(window.scrollY).toBe(RESTING_SCROLL_Y);
    expect(thePageLeftItThere(null)).toBe(true);

    // The reader scrolls the page away, and an arrival from someone else leaves them there.
    Object.defineProperty(window, "scrollY", { configurable: true, value: 900, writable: true });
    act(() => {
      window.dispatchEvent(new Event("scroll"));
      view.rerender(<Harness itemSeqs={[2, 1]} ownSendCount={0} />);
    });

    expect(window.scrollY).toBe(900);

    // Back at the page's top, the next arrival is followed again.
    Object.defineProperty(window, "scrollY", { configurable: true, value: 0, writable: true });
    act(() => {
      window.dispatchEvent(new Event("scroll"));
      view.rerender(<Harness itemSeqs={[3, 2, 1]} ownSendCount={0} />);
    });

    expect(window.scrollY).toBe(RESTING_SCROLL_Y);
  } finally {
    unmount?.();
    layout.restore();
    if (scrollY === undefined) {
      Reflect.deleteProperty(window, "scrollY");
    } else {
      Object.defineProperty(window, "scrollY", scrollY);
    }
  }
});
