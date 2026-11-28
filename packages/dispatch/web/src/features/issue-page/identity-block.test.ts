import { expect, test } from "bun:test";
import { act, renderHook } from "@testing-library/react";

import { HEADER_SCROLLS_QUERY, useIdentityBlock } from "./identity-block";

/** Leaves the page `y` pixels down, as a scroll leaves `window.scrollY`. */
function scrolledTo(y: number): void {
  Object.defineProperty(window, "scrollY", { configurable: true, value: y, writable: true });
}

// R11: the identity block draws its shadow once the page has scrolled more than 40 px, as the
// prototype's `#hdr-sticky.stuck` does (`scrollY > 40`), and not at the first pixel; a place
// the page restores past 40 px draws it in the commit that restores it.
test("the header's shadow appears only after 40 px of scroll", () => {
  const scrollY = Object.getOwnPropertyDescriptor(window, "scrollY");
  const original = globalThis.ResizeObserver;
  // happy-dom has no `ResizeObserver`, and the block's height is no part of this case.
  globalThis.ResizeObserver = class {
    observe() {}
    unobserve() {}
    disconnect() {}
  } as unknown as typeof ResizeObserver;
  const block = { current: document.createElement("header") };
  try {
    scrolledTo(0);
    const scrolled = renderHook(() => useIdentityBlock(block));
    expect(scrolled.result.current.condensed).toBe(false);
    act(() => {
      scrolledTo(40);
      window.dispatchEvent(new Event("scroll"));
    });
    expect(scrolled.result.current.condensed).toBe(false);
    act(() => {
      scrolledTo(41);
      window.dispatchEvent(new Event("scroll"));
    });
    expect(scrolled.result.current.condensed).toBe(true);
    scrolled.unmount();

    scrolledTo(0);
    const restored = renderHook(() => useIdentityBlock(block, 41));
    expect(restored.result.current.condensed).toBe(true);
    restored.unmount();
  } finally {
    globalThis.ResizeObserver = original;
    if (scrollY === undefined) {
      Reflect.deleteProperty(window, "scrollY");
    } else {
      Object.defineProperty(window, "scrollY", scrollY);
    }
  }
});

// At 760 px and below the header scrolls away with the page and the filter row is the band that
// stays, so the header is never stuck there and draws no shadow, however far the page has
// scrolled; once the width passes 760 px, the same place draws it.
test("the header draws no shadow where it scrolls away", () => {
  const scrollY = Object.getOwnPropertyDescriptor(window, "scrollY");
  const original = globalThis.ResizeObserver;
  const originalMatchMedia = window.matchMedia;
  // happy-dom has no `ResizeObserver`, and the block's height is no part of this case.
  globalThis.ResizeObserver = class {
    observe() {}
    unobserve() {}
    disconnect() {}
  } as unknown as typeof ResizeObserver;
  // A width the test moves while the hook is mounted: happy-dom delivers no `change` to a list
  // made before its viewport moves, so each list answers the one width and hears it move.
  const changes: ((event: Event) => void)[] = [];
  let narrow = true;
  window.matchMedia = ((query: string) =>
    ({
      addEventListener: (type: string, listener: EventListenerOrEventListenerObject | null) => {
        if (type === "change" && typeof listener === "function") changes.push(listener);
      },
      addListener: () => {},
      dispatchEvent: () => true,
      get matches() {
        return query === HEADER_SCROLLS_QUERY && narrow;
      },
      media: query,
      onchange: null,
      removeEventListener: () => {},
      removeListener: () => {},
    }) as MediaQueryList) as typeof window.matchMedia;
  const block = { current: document.createElement("header") };
  try {
    scrolledTo(0);
    const scrolled = renderHook(() => useIdentityBlock(block));
    act(() => {
      scrolledTo(41);
      window.dispatchEvent(new Event("scroll"));
    });
    expect(scrolled.result.current.condensed).toBe(false);
    act(() => {
      narrow = false;
      for (const change of changes) change(new Event("change"));
    });
    expect(scrolled.result.current.condensed).toBe(true);
    scrolled.unmount();
  } finally {
    globalThis.ResizeObserver = original;
    window.matchMedia = originalMatchMedia;
    if (scrollY === undefined) {
      Reflect.deleteProperty(window, "scrollY");
    } else {
      Object.defineProperty(window, "scrollY", scrollY);
    }
  }
});
