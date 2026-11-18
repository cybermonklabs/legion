import { expect, test } from "bun:test";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { stubMatchMedia } from "../margin/margin-fixture";
import { IssuePresets } from "./IssuePresets";
import { presetPath } from "./issue-address";
import { PRESET_NAMES, type Preset } from "./page-words";
import type { PresetSignal } from "./presets";

const SIGNALS: Readonly<Record<Preset, PresetSignal>> = {
  conversation: { count: 0 },
  decisions: { count: 0 },
  plan: { count: 0 },
  spec: { count: 2 },
  verification: { count: 1 },
};

/** The address the row moved to, which a chosen view changes. */
function Address(): ReactNode {
  return <output aria-label="address">{useLocation().pathname}</output>;
}

function menuItem(preset: Preset): HTMLElement {
  return screen.getByRole("menuitemradio", { name: new RegExp(`^${PRESET_NAMES[preset]}\\b`) });
}

// Below 1280 px the row is one control that opens a menu of the five, and it announces a menu
// button, so the keys are WAI-ARIA's menu button's (the tester's reading of part V at 390 px).
test("the one control is a menu button: Enter, Space, Down and Up open it on the view that is on, the arrows, Home and End move in it, Escape gives the focus back, and Tab leaves it", async () => {
  const user = userEvent.setup();
  const restoreMatchMedia = stubMatchMedia(true);
  const view = render(
    <MemoryRouter initialEntries={[presetPath("CORE-1", "spec")]}>
      <IssuePresets active="spec" issueKey="CORE-1" signals={SIGNALS} />
      <button type="button">after the row</button>
      <Address />
    </MemoryRouter>
  );
  try {
    const chooser = screen.getByRole("button", { name: /^Spec\b/ });
    for (const key of ["{Enter}", " ", "{ArrowDown}", "{ArrowUp}"]) {
      chooser.focus();
      await user.keyboard(key);
      expect(screen.queryByRole("menu"), `${key} did not open the menu`).not.toBeNull();
      expect(document.activeElement, `${key} left the focus off the view that is on`).toBe(
        menuItem("spec")
      );
      await user.keyboard("{Escape}");
      expect(screen.queryByRole("menu"), "Escape did not close the menu").toBeNull();
      expect(document.activeElement, "Escape left the focus off the control").toBe(chooser);
    }

    // Down and Up move one view, wrapping at each end; Home and End go to the ends.
    await user.keyboard("{ArrowDown}");
    for (const [key, to] of [
      ["{ArrowDown}", "verification"],
      ["{ArrowDown}", "plan"],
      ["{ArrowDown}", "decisions"],
      ["{ArrowDown}", "conversation"],
      ["{ArrowUp}", "decisions"],
      ["{Home}", "conversation"],
      ["{End}", "decisions"],
      ["{ArrowUp}", "plan"],
    ] as const) {
      await user.keyboard(key);
      expect(document.activeElement, `${key} did not move to ${PRESET_NAMES[to]}`).toBe(
        menuItem(to)
      );
    }
    // Enter on a view chooses it and gives the focus back to the control.
    await user.keyboard("{Enter}");
    expect(screen.queryByRole("menu")).toBeNull();
    expect(screen.getByRole("status", { name: "address" }).textContent).toBe(
      presetPath("CORE-1", "plan")
    );
    expect(document.activeElement, "a chosen view left the focus off the control").toBe(chooser);

    // Tab closes the menu and moves on from the control, not through the five.
    await user.keyboard("{ArrowDown}");
    await user.keyboard("{Tab}");
    expect(screen.queryByRole("menu"), "Tab left the menu open").toBeNull();
    expect(document.activeElement, "Tab did not move on from the control").toBe(
      screen.getByRole("button", { name: "after the row" })
    );
  } finally {
    view.unmount();
    restoreMatchMedia();
  }
});

// A press on a view control leaves the focus where it is (decision 93). Focus taken on
// mousedown would leave the outline, whose row then closes its body under the pointer, and on an
// address that names a node (`#b-`) the press would end elsewhere and do nothing.
test("a press on a view control leaves the focus where it is, and moves to the view's address", async () => {
  const user = userEvent.setup();
  const restoreMatchMedia = stubMatchMedia(false);
  const view = render(
    <MemoryRouter initialEntries={["/issues/CORE-1#b-r1"]}>
      <button type="button">a row of the outline</button>
      <IssuePresets active="conversation" issueKey="CORE-1" signals={SIGNALS} />
      <Address />
    </MemoryRouter>
  );
  try {
    const row = screen.getByRole("button", { name: "a row of the outline" });
    row.focus();
    const spec = screen.getByRole("button", { name: "Spec" });
    await user.pointer({ keys: "[MouseLeft>]", target: spec });
    expect(document.activeElement, "the press's mousedown took the focus").toBe(row);
    await user.pointer({ keys: "[/MouseLeft]", target: spec });
    expect(screen.getByRole("status", { name: "address" }).textContent).toBe(
      presetPath("CORE-1", "spec")
    );
  } finally {
    view.unmount();
    restoreMatchMedia();
  }
});

// A press on a control's signal, the `<label>` of its button, leaves the focus where it is too
// (decision 93): a label takes no focus, so its mousedown would move the focus to the body, and
// its own activation would focus the button at the click.
test("a press on a view control's count leaves the focus where it is, and moves to the view's address", async () => {
  const user = userEvent.setup();
  const restoreMatchMedia = stubMatchMedia(false);
  const view = render(
    <MemoryRouter initialEntries={["/issues/CORE-1#b-r1"]}>
      <button type="button">a row of the outline</button>
      <IssuePresets active="conversation" issueKey="CORE-1" signals={SIGNALS} />
      <Address />
    </MemoryRouter>
  );
  try {
    const row = screen.getByRole("button", { name: "a row of the outline" });
    row.focus();
    const count = view.container.querySelector<HTMLElement>('[data-preset-count="spec"]');
    if (count === null) throw new Error("Spec draws no count");
    await user.pointer({ keys: "[MouseLeft>]", target: count });
    expect(document.activeElement, "the press's mousedown on the count took the focus").toBe(row);
    await user.pointer({ keys: "[/MouseLeft]", target: count });
    expect(document.activeElement, "the press's click on the count took the focus").toBe(row);
    expect(screen.getByRole("status", { name: "address" }).textContent).toBe(
      presetPath("CORE-1", "spec")
    );
  } finally {
    view.unmount();
    restoreMatchMedia();
  }
});

// Below 1280 px a press on the one control leaves the focus where it is until its menu opens
// and takes it (decision 93), so the outline keeps its row open under the pointer.
test("a press on the one control leaves the focus where it is, then opens its menu on the view that is on", async () => {
  const user = userEvent.setup();
  const restoreMatchMedia = stubMatchMedia(true);
  const view = render(
    <MemoryRouter initialEntries={[presetPath("CORE-1", "spec")]}>
      <button type="button">a row of the outline</button>
      <IssuePresets active="spec" issueKey="CORE-1" signals={SIGNALS} />
    </MemoryRouter>
  );
  try {
    const row = screen.getByRole("button", { name: "a row of the outline" });
    row.focus();
    const chooser = screen.getByRole("button", { name: /^Spec\b/ });
    await user.pointer({ keys: "[MouseLeft>]", target: chooser });
    expect(document.activeElement, "the press's mousedown on the one control took the focus").toBe(
      row
    );
    await user.pointer({ keys: "[/MouseLeft]", target: chooser });
    expect(screen.queryByRole("menu"), "the press did not open the menu").not.toBeNull();
    expect(document.activeElement, "the menu opened with the focus off the view that is on").toBe(
      menuItem("spec")
    );
  } finally {
    view.unmount();
    restoreMatchMedia();
  }
});
