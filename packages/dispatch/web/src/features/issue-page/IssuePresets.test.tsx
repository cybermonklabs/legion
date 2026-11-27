import { beforeAll, expect, spyOn, test } from "bun:test";
import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useState } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { DESKTOP_WIDTH, PHONE_WIDTH, stubViewport } from "../margin/margin-fixture";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { KeymapProvider } from "../shell/KeymapProvider";
import { useKeymap } from "../shell/keymap";
import { useDialog } from "../shell/useDialog";
import { IssuePresets } from "./IssuePresets";
import { presetPath } from "./issue-address";
import { PRESET_NAMES, PRESET_ORDER, type Preset } from "./page-words";
import type { PresetSignal } from "./presets";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

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
  const restoreMatchMedia = stubViewport(PHONE_WIDTH);
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
  const restoreMatchMedia = stubViewport(DESKTOP_WIDTH);
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
  const restoreMatchMedia = stubViewport(DESKTOP_WIDTH);
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
  const restoreMatchMedia = stubViewport(PHONE_WIDTH);
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

/** A dialog over the row that `useDialog` holds open, opened by `?` as the list of keys is. It
 *  names itself no `aria-modal`, so only `useDialog`'s registry says that it is open. */
function DialogOverTheRow(): ReactNode {
  const [open, setOpen] = useState(false);
  useKeymap("global", [
    { id: "over-the-row", keys: "?", label: "A dialog over the row", run: () => setOpen(true) },
  ]);
  const dialog = useDialog<HTMLDivElement>({ onClose: () => setOpen(false), open });
  return open ? (
    <div aria-label="Over the row" ref={dialog.containerRef} role="dialog">
      <p>Words of the dialog</p>
      <button type="button">in the dialog</button>
    </div>
  ) : null;
}

// Escape closes the innermost layer. A dialog open over the menu takes it wherever the focus is:
// on the dialog itself in its first frame, on the menu's item under it, or on the page after a
// press on the dialog's text. In the last two cases the key's target is outside the dialog, and
// only whether a dialog is open tells the menu to leave the key to it. That is `useDialog`'s
// registry's answer (`dialogOpen()`, decision 318), the one that decides which dialog the key
// closes and that Details' drawer asks too, so a dialog's markup does not change it.
test("below 1280 px, Escape with a dialog open over the menu closes the dialog alone, wherever the focus is", async () => {
  const user = userEvent.setup();
  const restoreMatchMedia = stubViewport(PHONE_WIDTH);
  const view = render(
    <MemoryRouter initialEntries={[presetPath("CORE-1", "spec")]}>
      <KeymapProvider>
        <IssuePresets active="spec" issueKey="CORE-1" signals={SIGNALS} />
        <DialogOverTheRow />
      </KeymapProvider>
    </MemoryRouter>
  );
  try {
    const chooser = screen.getByRole("button", { name: /^Spec\b/ });
    chooser.focus();
    await user.keyboard("{Enter}");
    expect(document.activeElement, "Enter left the focus off the view that is on").toBe(
      menuItem("spec")
    );
    const dialog = () => screen.queryByRole("dialog", { name: "Over the row" });

    // The dialog names no field, so it holds the focus itself until its control takes it a frame
    // after it opens (`useDialog`); with that frame held, the Escape lands on the dialog, not on
    // the menu's item.
    const frames = spyOn(window, "requestAnimationFrame").mockImplementation(() => 0);
    try {
      await user.keyboard("?");
      expect(dialog(), "? drew no dialog").not.toBeNull();
      expect(document.activeElement, "the dialog left the focus behind it as it opened").toBe(
        dialog()
      );
      await user.keyboard("{Escape}");
    } finally {
      frames.mockRestore();
    }
    expect(dialog(), "Escape in the dialog's first frame left it open").toBeNull();
    expect(
      screen.queryByRole("menu"),
      "that Escape closed the menu under the dialog"
    ).not.toBeNull();

    // The dialog takes the focus in the commit that opens it, with no frame run. The focus is
    // then put back on the menu's item, outside the dialog, so the Escape lands there. The row
    // reads that it is there before the Escape: a dialog that kept the focus inside itself would
    // take the Escape in the dialog, which closes it as well, and the row would pass without the
    // case it names, the focus outside the dialog.
    const heldFrames = spyOn(window, "requestAnimationFrame").mockImplementation(() => 0);
    try {
      await user.keyboard("?");
      expect(dialog(), "? drew no dialog").not.toBeNull();
      expect(document.activeElement, "the dialog did not take the focus as it opened").toBe(
        dialog()
      );
      act(() => menuItem("spec").focus());
      expect(document.activeElement, "the focus is not on the menu's item before the Escape").toBe(
        menuItem("spec")
      );
      await user.keyboard("{Escape}");
    } finally {
      heldFrames.mockRestore();
    }
    expect(dialog(), "Escape with the focus on the menu's item left the dialog open").toBeNull();
    expect(
      screen.queryByRole("menu"),
      "that Escape closed the menu under the dialog"
    ).not.toBeNull();

    // Once its frame has given the focus to its control, a press on the dialog's text puts the
    // focus on the page, and the Escape's target is no part of the dialog.
    await user.keyboard("?");
    await waitFor(() =>
      expect(document.activeElement, "the dialog's frame gave its control no focus").toBe(
        screen.getByRole("button", { name: "in the dialog" })
      )
    );
    await user.click(screen.getByText("Words of the dialog"));
    expect(document.activeElement, "a press on the dialog's text kept the focus off the page").toBe(
      document.body
    );
    await user.keyboard("{Escape}");
    expect(dialog(), "Escape with the focus on the page left the dialog open").toBeNull();
    expect(
      screen.queryByRole("menu"),
      "that Escape closed the menu under the dialog"
    ).not.toBeNull();

    await user.keyboard("{Escape}");
    expect(screen.queryByRole("menu"), "the next Escape left the menu open").toBeNull();
    expect(document.activeElement, "that Escape left the focus off the control").toBe(chooser);
  } finally {
    view.unmount();
    restoreMatchMedia();
  }
});

/** The ids an `aria-describedby` or `aria-labelledby` under `within` names that no element of
 *  the document carries. */
function missingReferences(within: HTMLElement): string[] {
  return [...within.querySelectorAll("[aria-describedby], [aria-labelledby]")].flatMap((element) =>
    ["aria-describedby", "aria-labelledby"]
      .flatMap((attribute) => (element.getAttribute(attribute) ?? "").split(/\s+/))
      .filter((id) => id !== "" && document.getElementById(id) === null)
  );
}

// The view row has no signals to give while the reads its counts come from are out
// (`slots/view-row.tsx`), since a count drawn then reads as the real one. The row is drawn all
// the same, each control with its name alone, and no control points at a signal that is not
// there. The case draws it at 1440 px (`DESKTOP_WIDTH`), from 1280 px up, where the row is the
// row of five.
test("with no signals the row of five draws every view's control with its name alone, and no description names a missing element", () => {
  const restoreMatchMedia = stubViewport(DESKTOP_WIDTH);
  const view = render(
    <MemoryRouter initialEntries={[presetPath("CORE-1", "spec")]}>
      <IssuePresets active="spec" issueKey="CORE-1" />
    </MemoryRouter>
  );
  try {
    for (const preset of PRESET_ORDER) {
      expect(
        screen.getByRole("button", { name: PRESET_NAMES[preset], pressed: preset === "spec" }),
        `${PRESET_NAMES[preset]} drew no control`
      ).toBeInTheDocument();
    }
    expect(
      view.container.querySelector("[data-preset-count]"),
      "a control drew a count with no signals"
    ).toBeNull();
    expect(
      screen.getByRole("toolbar").textContent,
      "a control drew more than its name with no signals"
    ).toBe(PRESET_ORDER.map((preset) => PRESET_NAMES[preset]).join(""));
    expect(
      missingReferences(view.container),
      "a control points at an element that is not drawn"
    ).toEqual([]);
  } finally {
    view.unmount();
    restoreMatchMedia();
  }
});

// The same below 1280 px, where the one control and each item of its menu are named by their
// signal beside their name: with no signals, each is named by its name alone. The case draws it
// at 390 px (`PHONE_WIDTH`), where the row is the one control.
test("with no signals the one control and its menu draw every view named alone, and no name points at a missing element", async () => {
  const user = userEvent.setup();
  const restoreMatchMedia = stubViewport(PHONE_WIDTH);
  const view = render(
    <MemoryRouter initialEntries={[presetPath("CORE-1", "spec")]}>
      <IssuePresets active="spec" issueKey="CORE-1" />
    </MemoryRouter>
  );
  try {
    await user.click(screen.getByRole("button", { name: PRESET_NAMES.spec }));
    for (const preset of PRESET_ORDER) {
      expect(
        screen.getByRole("menuitemradio", { name: PRESET_NAMES[preset] }),
        `the menu drew no item for ${PRESET_NAMES[preset]}`
      ).toBeInTheDocument();
    }
    expect(
      view.container.querySelector("[data-preset-count]"),
      "the one control or its menu drew a count with no signals"
    ).toBeNull();
    expect(
      missingReferences(view.container),
      "the one control or an item of its menu points at an element that is not drawn"
    ).toEqual([]);
  } finally {
    view.unmount();
    restoreMatchMedia();
  }
});
