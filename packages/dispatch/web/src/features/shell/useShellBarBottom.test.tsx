import { expect, spyOn, test } from "bun:test";
import { render, screen } from "@testing-library/react";

import { installResizeObserver } from "../../__tests__/scroll-box";
import { SHELL_BAR_BOTTOM_PROPERTY, useShellBarBottom } from "./useShellBarBottom";

/** The shell below `xl` as far as the hook sees it: the compact bar where `drawn`, and nothing
 *  where the shell draws none (the issue frame, or a window from `xl`). */
function Shell({ drawn }: { readonly drawn: boolean }) {
  const barRef = useShellBarBottom();
  return <div>{drawn ? <header data-testid="bar" ref={barRef} /> : null}</div>;
}

const published = () => document.documentElement.style.getPropertyValue(SHELL_BAR_BOTTOM_PROPERTY);

// The widened side panel starts where the shell's compact top bar ends (`wideInsetOf`), so the
// shell publishes that edge: when it draws the bar, when the bar's size changes (its counts wrap
// onto a second row), and as the page scrolls the bar away, until the window's top is the edge.
test("the bar publishes where it ends as it is drawn, as it grows and as the page scrolls it away, and the property goes with it", () => {
  const sizes = installResizeObserver();
  let bottom = 56;
  const rect = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(
    () => ({ bottom }) as DOMRect
  );
  const view = render(<Shell drawn />);
  try {
    expect(published()).toBe("56px");

    // The counts wrap onto a second row.
    bottom = 92;
    sizes.resize(screen.getByTestId("bar"));
    expect(published()).toBe("92px");

    // The page scrolls: the bar's edge rises with it, and stops at the window's top.
    bottom = 20;
    window.dispatchEvent(new Event("scroll"));
    expect(published()).toBe("20px");
    bottom = -36;
    window.dispatchEvent(new Event("scroll"));
    expect(published()).toBe("0px");

    // A window from `xl` draws no bar: nothing is published, nothing is watched.
    view.rerender(<Shell drawn={false} />);
    expect(published()).toBe("");
    expect(sizes.live()).toBe(0);
    bottom = 40;
    window.dispatchEvent(new Event("scroll"));
    expect(published()).toBe("");

    // Drawn again, it publishes again.
    view.rerender(<Shell drawn />);
    expect(published()).toBe("40px");
  } finally {
    view.unmount();
    rect.mockRestore();
    sizes.restore();
  }
  expect(published()).toBe("");
  expect(sizes.live()).toBe(0);
});
