import { afterEach, beforeEach, expect, test } from "bun:test";
// biome-ignore lint/style/noRestrictedImports: the strip's own scroll events below, which user-event has no form for
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { useCallback } from "react";

import { settle } from "../__tests__/fake-clock";
import {
  installResizeObserver,
  layOutScrollBox,
  type ResizeControl,
  type ScrollBox,
} from "../__tests__/scroll-box";
import { scrollFadeTrailing } from "../theme/classes";
import { useScrollFadeTrailing } from "./useScrollFadeTrailing";

// Each strip here is sized by its test, and the observer stub is the box changing: what the strip
// observes, and what a narrower viewport fires.
let sizes: ResizeControl;

beforeEach(() => {
  sizes = installResizeObserver();
});

afterEach(() => {
  sizes.restore();
});

/** A strip laid out before the hook first measures it, as a browser lays out before the layout
 *  effect runs. */
function Strip({ box }: { box: ScrollBox }) {
  const { fade, ref } = useScrollFadeTrailing();
  const sized = useCallback(
    (element: HTMLDivElement | null) => {
      if (element !== null) {
        layOutScrollBox(element, box);
      }
      ref(element);
    },
    [box, ref]
  );
  return (
    <div className={`overflow-x-auto ${fade}`} data-testid="strip" ref={sized}>
      <span>first</span>
    </div>
  );
}

test("a strip with content past its trailing edge wears the fade from its first measure, and a strip that fits does not", () => {
  const view = render(
    <>
      <Strip box={{ client: 300, scroll: 420 }} />
      <Strip box={{ client: 300, scroll: 300 }} />
    </>
  );

  try {
    const [overflowing, fitting] = screen.getAllByTestId("strip");
    expect(overflowing).toHaveClass(scrollFadeTrailing);
    expect(fitting).not.toHaveClass(scrollFadeTrailing);
    expect(fitting).toHaveClass("overflow-x-auto");
  } finally {
    view.unmount();
  }
});

test("the fade is off at the end of the scroll, a fractional pixel short of it included, and back once the reader scrolls away", () => {
  const view = render(<Strip box={{ client: 300, scroll: 420 }} />);

  try {
    const strip = screen.getByTestId("strip");
    // The reader scrolls to the end: the strip's own scroll event, which user-event cannot send.
    strip.scrollLeft = 120;
    fireEvent.scroll(strip);
    expect(strip).not.toHaveClass(scrollFadeTrailing);

    // A fractional layout stops half a pixel short of the end (a scroll event, no user-event form).
    strip.scrollLeft = 119.5;
    fireEvent.scroll(strip);
    expect(strip).not.toHaveClass(scrollFadeTrailing);

    // The reader scrolls back away from the end: a scroll event, which user-event has no form for.
    strip.scrollLeft = 40;
    fireEvent.scroll(strip);
    expect(strip).toHaveClass(scrollFadeTrailing);
  } finally {
    view.unmount();
  }
});

test("a box that narrows, and content that arrives, are measured with no render of the strip's owner", async () => {
  const box = { client: 300, scroll: 300 };
  const view = render(<Strip box={box} />);

  try {
    const strip = screen.getByTestId("strip");
    expect(strip).not.toHaveClass(scrollFadeTrailing);
    expect(sizes.observed()).toContain(strip);

    box.client = 200;
    sizes.resize();
    expect(strip).toHaveClass(scrollFadeTrailing);

    box.client = 300;
    sizes.resize();
    expect(strip).not.toHaveClass(scrollFadeTrailing);

    // Content a child component draws later, which this owner never renders for.
    box.scroll = 480;
    const later = document.createElement("span");
    later.textContent = "a label that arrived";
    strip.append(later);
    await waitFor(() => expect(strip).toHaveClass(scrollFadeTrailing));
    expect(sizes.observed()).toContain(later);
  } finally {
    view.unmount();
  }
});

// A strip that is gone keeps no part of the hook alive: its size observer is let go, and neither
// a scroll of the detached strip nor a child added to it is measured. Each measure reads the
// strip's scroll width, so the reads are counted; the same two events, while it is mounted, show
// the count does see them.
test("an unmounted strip leaves no observer or listener behind", async () => {
  const view = render(<Strip box={{ client: 300, scroll: 420 }} />);
  const strip = screen.getByTestId("strip");
  let measures = 0;
  Object.defineProperty(strip, "scrollWidth", {
    configurable: true,
    get: () => {
      measures += 1;
      return 420;
    },
  });
  // The reader's scroll is the strip's own scroll event, which user-event has no form for.
  fireEvent.scroll(strip);
  strip.append(document.createElement("span"));
  await settle();
  expect(measures).toBe(2);
  expect(sizes.live()).toBe(1);

  view.unmount();
  measures = 0;
  // A scroll the detached strip still receives: its own scroll event, no user-event form.
  fireEvent.scroll(strip);
  strip.append(document.createElement("span"));
  await settle();
  expect(measures).toBe(0);
  expect(sizes.live()).toBe(0);
});
