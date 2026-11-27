/**
 * A sideways scroll box a test sizes, and the `ResizeObserver` that tells it the box changed.
 * happy-dom lays nothing out, so `scrollWidth` and `clientWidth` read 0 and `scrollLeft` takes any
 * number, and its `ResizeObserver` never calls back; a strip whose rule reads them only runs where
 * a test says how wide it is and when that changed.
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
