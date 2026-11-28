/**
 * A sideways scroll box a test sizes, an open panel that scrolls down laid out as a browser lays
 * it out, the `ResizeObserver` that tells a box it changed, and the end of a windowed table's
 * scroll. happy-dom lays nothing out, so `scrollWidth`, `clientWidth` and `clientHeight` read 0
 * and `scrollLeft` and `scrollTop` take any number, and its `ResizeObserver` never calls back; a
 * rule that reads them only runs where a test says how big the box is and when that changed.
 */

import { act } from "@testing-library/react";

/** A box `client` px wide around `scroll` px of content. A test may change either afterwards. */
export interface ScrollBox {
  client: number;
  scroll: number;
}

/** `element` reads its widths from `box`, and its `scrollLeft` stops at both ends of the scroll,
 *  as a browser's does. */
export function layOutScrollBox(element: HTMLElement, box: ScrollBox): void {
  let left = 0;
  Object.defineProperties(element, {
    clientWidth: { configurable: true, get: () => box.client },
    scrollLeft: {
      configurable: true,
      get: () => left,
      set: (value: number) => {
        left = Math.min(Math.max(value, 0), Math.max(box.scroll - box.client, 0));
      },
    },
    scrollWidth: { configurable: true, get: () => box.scroll },
  });
}

/** How long a windowed table waits after its box's last scroll event to mark the scroll ended:
 *  `@tanstack/virtual-core`'s `isScrollingResetDelay`, 150 ms by default, which `DataTable`
 *  keeps, as a debounce on the window's own timer. */
const WINDOW_SCROLL_END_MS = 150;

/** Waits, inside `act`, past the end a windowed table marks after its box's last scroll event.
 *  That end is a state update of the table's own on a timer, which can fire outside `act` while
 *  the row reads on or before its cleanup, and React reports it: `DataTable.test.tsx`'s
 *  screen-reader row failed so once in a loaded run of 3,553 rows ("update outside act in
 *  Deploys"), and fails so every time with a 300 ms pause after its scroll. A row that scrolls a
 *  windowed table's box calls this right after its last scroll, so the end lands inside `act`
 *  whatever the load: timers fire in the order they are due. */
export async function endWindowScroll(): Promise<void> {
  await act(() => {
    const { promise, resolve } = Promise.withResolvers<void>();
    setTimeout(resolve, WINDOW_SCROLL_END_MS + 50);
    return promise;
  });
}

/** An element of an open panel, and where it stands in what the panel holds: its top, in px down
 *  from the top of what the panel holds, and its height. */
type Placed = [element: Element, top: number, height: number];

/**
 * Lays an open panel out as a browser would, which happy-dom does not: its box is 200 px tall at
 * `at` px down the window inside a 1 px border, so it shows 198 px of the `holds` px it holds.
 * Each element of `placed` stands where it says, moved up by the panel's `scrollTop`, which stops
 * at both ends of the scroll, as a browser's does.
 */
export function layOutPanel(panel: HTMLElement, holds: number, placed: Placed[], at = 100): void {
  const shown = 198;
  let scrolled = 0;
  Object.defineProperties(panel, {
    clientHeight: { configurable: true, get: () => shown },
    clientTop: { configurable: true, get: () => 1 },
    scrollTop: {
      configurable: true,
      get: () => scrolled,
      set: (value: number) => {
        scrolled = Math.min(Math.max(value, 0), holds - shown);
      },
    },
  });
  panel.getBoundingClientRect = () => new DOMRect(0, at, 256, 200);
  for (const [element, top, height] of placed) {
    element.getBoundingClientRect = () => new DOMRect(9, at + 1 + top - scrolled, 238, height);
  }
}

/** `controls` one under another from 8 px into their panel (its padding), 44 px tall and 8 px
 *  apart. */
export function stacked(controls: Element[]): Placed[] {
  return controls.map((control, index) => [control, 8 + 52 * index, 44]);
}

export interface ResizeControl {
  /** Every element a live observer watches. */
  readonly observed: () => readonly Element[];
  /** How many observers are live. */
  readonly live: () => number;
  /** Reports a change of `target` to its observers, or of every observed box when omitted. */
  readonly resize: (target?: Element) => void;
  readonly restore: () => void;
}

export function installResizeObserver(): ResizeControl {
  const original = globalThis.ResizeObserver;
  const live = new Set<ObserverStub>();

  class ObserverStub implements ResizeObserver {
    readonly targets: Element[] = [];
    readonly callback: ResizeObserverCallback;

    constructor(callback: ResizeObserverCallback) {
      this.callback = callback;
      live.add(this);
    }

    disconnect(): void {
      live.delete(this);
    }

    observe(target: Element): void {
      this.targets.push(target);
    }

    unobserve(): void {}
  }

  globalThis.ResizeObserver = ObserverStub;
  return {
    live: () => live.size,
    observed: () => [...live].flatMap((observer) => observer.targets),
    resize: (target?: Element) => {
      act(() => {
        for (const observer of [...live]) {
          if (target === undefined || observer.targets.includes(target)) {
            observer.callback([], observer);
          }
        }
      });
    },
    restore: () => {
      globalThis.ResizeObserver = original;
    },
  };
}
