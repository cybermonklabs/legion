import { expect, spyOn, test } from "bun:test";
import { render, screen } from "@testing-library/react";

import { installResizeObserver } from "../__tests__/scroll-box";
import { pillClassName, usePillWrapped } from "./Pill";

/** A pill in the shape that wraps, as `ClaimChip`'s `wraps` and every pill inside `NoTooltips`
 *  draw it, with the line and the padding its classes give it in a browser, which the unit DOM
 *  reads from no stylesheet. */
function WrappingPill({ wraps = true }: { wraps?: boolean }) {
  const { ref, wrapped } = usePillWrapped(wraps);
  return (
    <span
      className={pillClassName("label", false, wraps, wrapped)}
      data-testid="pill"
      ref={ref}
      style={{ lineHeight: "16px", paddingBottom: "4px", paddingTop: "4px" }}
    >
      Claimed by a session whose name runs long
    </span>
  );
}

// A full radius on a box several lines tall draws an oval, which leaves the first and last lines'
// words outside its fill: a pill that wraps is a pill on one line and has 8 px corners on more,
// decided by its drawn height, and decided again each time it resizes.
test("a pill that wraps has the full radius on one line and 8 px corners on more, by its drawn height", () => {
  const sizes = installResizeObserver();
  // One line: 16 px inside its 8 px of padding.
  let height = 24;
  const rect = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(
    () => new DOMRect(0, 0, 120, height)
  );
  const view = render(<WrappingPill />);
  try {
    const pill = screen.getByTestId("pill");
    expect(pill.className).toContain("rounded-full");
    expect(pill.className).not.toContain("rounded-lg");
    // Its column narrows, and it takes two lines and the gap between them.
    height = 44;
    sizes.resize(pill);
    expect(pill.className).toContain("rounded-lg");
    expect(pill.className).not.toContain("rounded-full");
    // Widened back to one line, it is a pill again.
    height = 24;
    sizes.resize(pill);
    expect(pill.className).toContain("rounded-full");
  } finally {
    view.unmount();
    rect.mockRestore();
    sizes.restore();
  }
  // Taken out, it watches nothing.
  expect(sizes.live()).toBe(0);
});

test("a pill that does not wrap keeps the full radius and is never measured", () => {
  const sizes = installResizeObserver();
  const rect = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(
    () => new DOMRect(0, 0, 120, 44)
  );
  const view = render(<WrappingPill wraps={false} />);
  try {
    expect(screen.getByTestId("pill").className).toContain("rounded-full");
    expect(rect).not.toHaveBeenCalled();
    expect(sizes.live()).toBe(0);
  } finally {
    view.unmount();
    rect.mockRestore();
    sizes.restore();
  }
});
