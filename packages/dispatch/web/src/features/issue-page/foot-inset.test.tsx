import { expect, test } from "bun:test";
import { render, screen } from "@testing-library/react";
import { type CSSProperties, type ReactNode, useRef } from "react";

import { nextFrame } from "../../__tests__/frames";
import {
  CLEARANCE_PROPERTY,
  DOCUMENT_SCROLL_ATTRIBUTE,
  FOOT_LAYERS,
  MARK_ATTRIBUTE,
  useFootInset,
} from "../../hooks/useFootInset";

/** The property the page layer publishes for the floats above it, which the update banner reads
 *  (`FOOT_LAYERS`). */
const PAGE_INSET = FOOT_LAYERS[0].property;

/** The height of the float each case shows, in pixels. */
const FLOAT_HEIGHT = 40;

/**
 * A box `height` px tall whose bottom edge stands `gap` px above the screen's bottom edge, an
 * element of the foot's page layer as the issue page's box is (`slots/foot.tsx`), with `style`
 * as its position. happy-dom lays nothing out, so the box's place on screen is this harness's:
 * the element answers `getBoundingClientRect` with it. happy-dom computes no initial value for
 * `top` and `bottom`, so each box states its own, as a browser's computed style reads them.
 */
function FootBox({
  gap,
  height,
  style,
}: {
  gap: number;
  height: number;
  style: CSSProperties;
}): ReactNode {
  const box = useRef<HTMLElement | null>(null);
  useFootInset(box, true, "page");
  return (
    <section
      ref={(node) => {
        box.current = node;
        if (node === null) return;
        node.getBoundingClientRect = () =>
          new DOMRect(0, window.innerHeight - gap - height, 400, height);
      }}
      style={style}
    />
  );
}

/** A float of the banner layer, fixed and with no floor, as the update banner is
 *  (`DeploymentResilience`). With no float the page layer publishes nothing, so each case shows
 *  this one and reads where the page layer puts it. */
function Float(): ReactNode {
  const float = useRef<HTMLElement | null>(null);
  useFootInset(float, true, "banner");
  return (
    <div
      ref={(node) => {
        float.current = node;
        if (node === null) return;
        node.getBoundingClientRect = () =>
          new DOMRect(600, window.innerHeight - FLOAT_HEIGHT, 300, FLOAT_HEIGHT);
      }}
      data-testid="float"
      style={{ position: "fixed" }}
    />
  );
}

/** What the document element publishes for the floats above the page layer. */
function published(): string {
  return document.documentElement.style.getPropertyValue(PAGE_INSET);
}

/** The issue page's box: `sticky bottom-0`, which holds it at the foot of its column. */
const HELD: CSSProperties = { bottom: 0, position: "sticky", top: "auto" };

// The issue page's box is not fixed: `sticky bottom-0` holds it at the foot of its column while
// the outline scrolls under it. Standing at the screen's bottom edge it is an obstacle the floats
// keep clear of, so the float rides 1rem above its top edge; a box's edge landing on a fraction
// of a pixel moves that by as much. Once the page has moved it up from that edge with room under
// it for the float, the float sits on its floor and the page layer publishes nothing.
test("a box held at the screen's foot lifts the float above it, and one scrolled away with room under it lifts nothing", () => {
  const held = render(
    <>
      <FootBox gap={0} height={58} style={HELD} />
      <Float />
    </>
  );
  expect(published()).toBe("58px");
  held.unmount();
  expect(published()).toBe("");

  const nearlyHeld = render(
    <>
      <FootBox gap={0.5} height={58} style={HELD} />
      <Float />
    </>
  );
  expect(published()).toBe("58.5px");
  nearlyHeld.unmount();
  expect(published()).toBe("");

  const scrolledAway = render(
    <>
      <FootBox gap={120} height={58} style={HELD} />
      <Float />
    </>
  );
  expect(published()).toBe("");
  scrolledAway.unmount();
});

// Today's docked composer is sticky too, held at the top of its column from 640 px up, and keeps
// the 64 px `bottom` of its fixed look below 640 px. A box sticky to the top is in the page
// wherever it stands, even where a short window puts its bottom edge on the screen's, so it lifts
// nothing and the float sits where `main` draws it.
test("a box sticky to the top lifts nothing, even where its bottom edge meets the screen's", () => {
  const docked = render(
    <>
      <FootBox gap={0} height={58} style={{ bottom: "4rem", position: "sticky", top: 0 }} />
      <Float />
    </>
  );
  expect(published()).toBe("");
  docked.unmount();
});

/** The frame's column (`IssueOutlinePage.tsx`), a column flexbox whose content the document
 *  scrolls, holding the outline's last row and, while `box`, the box at the column's foot at
 *  `order` (`slots/foot.tsx`). */
function FrameColumn({ box, order }: { box: boolean; order: number }): ReactNode {
  return (
    <div
      data-testid="column"
      style={{ display: "flex", flexDirection: "column" }}
      {...{ [DOCUMENT_SCROLL_ATTRIBUTE]: "" }}
    >
      <p data-testid="row">The outline's last row.</p>
      {box ? <FootBox gap={0} height={56} style={{ ...HELD, order }} /> : null}
    </div>
  );
}

// The frame's column ends with the box, sticky to its foot, and the float rides above it, over the
// outline's last row. Room under the box would lift it and the float with it, so the column takes
// the room above it, as the spacer the box's `order: 1` puts above the box: 1rem above the float's
// top, less the box's top. The float stands at 72-112 px over the box's 0-56 px, so that is 72 px;
// the padding the column took before the box came, 1rem above the float on its floor at 16-56 px,
// goes for it. A box at `order: 0`, which the spacer would land under, holds the room off as
// before. happy-dom has no hit test, so every point of the float hits it and then the last row,
// as `hooks/useFootInset.test.tsx` has it, and lays out no spacer, so this case reads the room
// the column takes, and the page at its end is read in a browser (`e2e/foot-sweep.e2e.ts`).
test("the frame's column takes the room above a box sticky to its foot that its spacer lands above, and none above one it would land under", async () => {
  Object.defineProperty(document, "elementsFromPoint", {
    configurable: true,
    value: () => [screen.getByTestId("float"), screen.getByTestId("row")],
  });
  const column = () => screen.getByTestId("column");
  try {
    const view = render(
      <>
        <FrameColumn box={false} order={1} />
        <Float />
      </>
    );
    await nextFrame();
    expect(column()).toHaveAttribute(MARK_ATTRIBUTE, "padding");
    expect(column().style.paddingBottom).toBe("72px");
    view.rerender(
      <>
        <FrameColumn box order={1} />
        <Float />
      </>
    );
    await nextFrame();
    expect(column()).toHaveAttribute(MARK_ATTRIBUTE, "after");
    expect(column().style.getPropertyValue(CLEARANCE_PROPERTY)).toBe("72px");
    expect(column().style.paddingBottom).toBe("");
    view.unmount();

    const under = render(
      <>
        <FrameColumn box order={0} />
        <Float />
      </>
    );
    await nextFrame();
    expect(column()).not.toHaveAttribute(MARK_ATTRIBUTE);
    under.unmount();
  } finally {
    Reflect.deleteProperty(document, "elementsFromPoint");
  }
});
