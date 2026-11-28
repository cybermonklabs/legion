import { expect, test } from "bun:test";

import { SHELL_BAR_BOTTOM_PROPERTY } from "../shell/useShellBarBottom";
import { panelIsWide, type ShellLayout, type WideInset, wideInsetOf } from "./wide-inset";

// Where a widened side panel starts (spec R1, A6): at the edge of the navigation that stands beside
// the page, and, below `xl`, where the navigation is a header and a drawer, under the shell's top
// bar and across the whole width. Below `sm` the panel fills the window already.

/** The shell's layout at a width: each test names what differs from a wide window with the
 *  sidebar open and no icon rail. */
function layout(changes: Partial<ShellLayout>): ShellLayout {
  return { compact: false, iconRail: false, sidebarHidden: false, sm: true, ...changes };
}

const BELOW_THE_BAR: WideInset = { left: "0px", top: `var(${SHELL_BAR_BOTTOM_PROPERTY}, 0px)` };

test("from xl the panel widens to the sidebar's edge, or to the rail's where the sidebar is folded or the issue frame draws its icon rail", () => {
  expect(wideInsetOf(layout({}))).toEqual({ left: "20rem", top: "0px" });
  expect(wideInsetOf(layout({ sidebarHidden: true }))).toEqual({ left: "3.5rem", top: "0px" });
  expect(wideInsetOf(layout({ iconRail: true }))).toEqual({ left: "3.5rem", top: "0px" });
  expect(wideInsetOf(layout({ iconRail: true, sidebarHidden: true }))).toEqual({
    left: "3.5rem",
    top: "0px",
  });
});

test("below xl, from sm, the panel widens across the whole width under the shell's top bar", () => {
  const inset = wideInsetOf(layout({ compact: true }));
  expect(inset).toEqual(BELOW_THE_BAR);
  // The bar's edge is the shell's to publish, and the window's top where it draws no bar.
  expect(inset?.top).toBe("var(--shell-bar-bottom, 0px)");
});

test("below xl the folded sidebar is inert, and the panel still stands under the top bar", () => {
  expect(wideInsetOf(layout({ compact: true, sidebarHidden: true }))).toEqual(BELOW_THE_BAR);
});

test("below xl the issue frame's icon rail keeps its edge, as it does from xl", () => {
  expect(wideInsetOf(layout({ compact: true, iconRail: true }))).toEqual({
    left: "3.5rem",
    top: "0px",
  });
});

test("below sm the panel is the whole width already and offers no widening", () => {
  expect(wideInsetOf(layout({ compact: true, sm: false }))).toBeUndefined();
  expect(wideInsetOf(layout({ compact: true, sidebarHidden: true, sm: false }))).toBeUndefined();
});

// The panel and the shell read one answer to whether the panel is wide (`panelIsWide`): wide, the
// shell keeps no 420 px for it, so a top bar above it spans the whole window and no empty band
// stands between the bar and the panel (review R1 below `xl`); docked, the shell keeps them again.
test("the panel is wide while the person has widened it where the shell leaves it room, so the shell keeps no room beside the page for it, and docked again it is not", () => {
  for (const room of [
    wideInsetOf(layout({})),
    wideInsetOf(layout({ sidebarHidden: true })),
    wideInsetOf(layout({ compact: true })),
    wideInsetOf(layout({ compact: true, iconRail: true })),
  ]) {
    expect(room).toBeDefined();
    expect(panelIsWide(true, room)).toBe(true);
    expect(panelIsWide(false, room)).toBe(false);
  }
  // Below `sm` the shell leaves no room to widen into, and a stored wide state leaves the panel
  // docked at the window's whole width.
  expect(panelIsWide(true, wideInsetOf(layout({ compact: true, sm: false })))).toBe(false);
});
