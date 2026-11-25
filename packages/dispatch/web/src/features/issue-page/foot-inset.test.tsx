import { expect, test } from "bun:test";
import { render } from "@testing-library/react";
import { type CSSProperties, type ReactNode, useRef } from "react";

import { FOOT_LAYERS, useFootInset } from "../../hooks/useFootInset";

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
