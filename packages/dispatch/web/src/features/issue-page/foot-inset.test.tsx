import { expect, test } from "bun:test";
import { render } from "@testing-library/react";
import { type ReactNode, useRef } from "react";

import {
  FOOT_COMPOSER_INSET_PROPERTY,
  FOOT_HELD_ATTRIBUTE,
  useFootInset,
} from "../../hooks/useFootInset";

/**
 * A box `height` px tall whose bottom edge stands `gap` px above the screen's bottom edge, marked
 * as held at the foot where `held` says so, as the issue page's box is (`slots/foot.tsx`).
 * happy-dom lays nothing out, so the box's place on screen is this harness's: the element answers
 * `getBoundingClientRect` with it. Its style is `sticky`, as both the issue page's box and
 * today's docked composer are from 640 px up, so only the mark tells them apart.
 */
function FootBox({ gap, height, held }: { gap: number; height: number; held: boolean }): ReactNode {
  const box = useRef<HTMLElement | null>(null);
  useFootInset(box, true);
  return (
    <section
      ref={(node) => {
        box.current = node;
        if (node === null) return;
        node.getBoundingClientRect = () =>
          new DOMRect(0, window.innerHeight - gap - height, 400, height);
      }}
      style={{ position: "sticky" }}
      {...(held ? { [FOOT_HELD_ATTRIBUTE]: "" } : {})}
    />
  );
}

/** What the document element publishes as the reach of the composers held at the screen's foot. */
function published(): string {
  return document.documentElement.style.getPropertyValue(FOOT_COMPOSER_INSET_PROPERTY);
}

// The issue page's box is not fixed: `sticky bottom-0` holds it at the foot of its column while
// the outline scrolls under it, and its host marks it so. Standing at the screen's bottom edge it
// reaches up from there as a fixed composer does, so the shell's status corner rides above it;
// within a pixel of the edge is at it, since a box's edge lands on a fraction of a pixel. Once the
// page has moved it off that edge it covers nothing of the shell's and counts nothing.
test("a box held at the screen's foot publishes its reach, and one scrolled away publishes none", () => {
  const held = render(<FootBox gap={0} height={58} held />);
  expect(published()).toBe("58px");
  held.unmount();
  expect(published()).toBe("");

  const nearlyHeld = render(<FootBox gap={0.5} height={58} held />);
  expect(published()).toBe("58.5px");
  nearlyHeld.unmount();
  expect(published()).toBe("");

  const scrolledAway = render(<FootBox gap={120} height={58} held />);
  expect(published()).toBe("");
  scrolledAway.unmount();
});

// A sticky box no host marks - today's docked composer, held at the top of its column from 640 px
// up - is in the page wherever it stands, even where a short window puts its bottom edge on the
// screen's, so it publishes nothing and the status corner stays where `main` draws it.
test("a sticky box its host does not mark publishes nothing, even where its bottom edge meets the screen's", () => {
  const docked = render(<FootBox gap={0} height={58} held={false} />);
  expect(published()).toBe("");
  docked.unmount();
});
