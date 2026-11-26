import { beforeAll, expect, spyOn, test } from "bun:test";
import { render, screen, waitFor } from "@testing-library/react";
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
import type { Filter, PresetSignal } from "./presets";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

const SIGNALS: Readonly<Record<Filter, PresetSignal>> = {
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
// on the dialog itself in its first frame, or on the page after a press on the dialog's text.
// In the second case the key's target is outside the dialog, and only whether a dialog is open
// tells the menu to leave the key to it. That is `useDialog`'s registry's answer (`dialogOpen()`,
// decision 318), the one that decides which dialog the key closes and that Details' drawer asks
// too, so a dialog's markup does not change it.
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

    // The dialog holds the focus itself until its control takes it a frame after it opens; with
    // that frame held, the Escape lands on the dialog, not on the menu's item.
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

/** The room `control` holds for its signal (`data-preset-signal`): whether it is hidden from
 *  assistive technology, the words it holds, and its class, which sizes it; null where the
 *  control holds none. */
function signalRoom(
  control: Element
): { readonly className: string; readonly hidden: string | null; readonly words: string } | null {
  const room = control.querySelector("[data-preset-signal]");
  return room === null
    ? null
    : {
        className: room.className,
        hidden: room.getAttribute("aria-hidden"),
        words: room.textContent ?? "",
      };
}

// On a large issue the counts arrive seconds after the row, once every read has answered
// (`slots/view-row.tsx`), and a control that grew then moved every control after it under a press
// aimed at one of them. So each filter control holds its signal's room from its first draw: drawn
// empty and hidden from assistive technology until the signal is known, with the class that sizes
// it, which it keeps once the signal is in it. Conversation draws no signal and holds no room.
// From 1280 px the signal describes its toggle, so no toggle's name changes as it arrives; below
// 1280 px the one control and each item of its menu are named by the signal beside their name
// (WCAG 2.5.3), so each is named by its view's name alone before it and by that name first after
// it. The case draws the row of five at 1440 px (`DESKTOP_WIDTH`) and the one control and its
// menu at 390 px (`PHONE_WIDTH`).
test.each([
  DESKTOP_WIDTH,
  PHONE_WIDTH,
])("before the counts arrive each filter control holds its signal's room, empty and hidden, in the class it keeps once they do, Conversation holds none, and no name changes but by the signal (width %i)", async (width) => {
  const user = userEvent.setup();
  const restoreMatchMedia = stubViewport(width);
  const drawn = (signals?: Readonly<Record<Filter, PresetSignal>>): ReactNode => (
    <MemoryRouter initialEntries={[presetPath("CORE-1", "spec")]}>
      <IssuePresets active="spec" issueKey="CORE-1" signals={signals} />
    </MemoryRouter>
  );
  const view = render(drawn());
  try {
    const row = width === DESKTOP_WIDTH;
    if (!row) await user.click(screen.getByRole("button", { name: PRESET_NAMES.spec }));
    // Each control by what it is and the view it is drawn for, found by `named`, its name: the
    // row's five toggles, each read in the box it stands in with its signal's label, or the one
    // control, which draws the view that is on, and each item of its menu.
    const controls = (
      named: (preset: Preset) => RegExp | string
    ): (readonly [string, Preset, Element])[] =>
      row
        ? PRESET_ORDER.map((preset) => {
            const toggle = screen.getByRole("button", {
              name: named(preset),
              pressed: preset === "spec",
            });
            return [
              `the ${PRESET_NAMES[preset]} toggle`,
              preset,
              toggle.parentElement ?? toggle,
            ] as const;
          })
        : [
            ["the one control", "spec", screen.getByRole("button", { name: named("spec") })],
            ...PRESET_ORDER.map(
              (preset) =>
                [
                  `the menu's ${PRESET_NAMES[preset]}`,
                  preset,
                  screen.getByRole("menuitemradio", { name: named(preset) }),
                ] as const
            ),
          ];
    const classes = new Map<string, string>();
    for (const [what, preset, control] of controls((preset) => PRESET_NAMES[preset])) {
      const room = signalRoom(control);
      if (preset === "conversation") {
        expect(room, `${what} holds a room for a signal it never draws`).toBeNull();
        continue;
      }
      expect(room, `${what} holds no room for its signal before the counts arrive`).not.toBeNull();
      expect(room?.hidden, `${what}'s empty room is not hidden from assistive technology`).toBe(
        "true"
      );
      expect(room?.words, `${what}'s room draws words before the counts arrive`).toBe("");
      expect(room?.className, `${what}'s room reserves no width`).toMatch(/(^| )min-w-\[/);
      classes.set(what, room?.className ?? "");
    }

    view.rerender(drawn(SIGNALS));
    // From 1280 px each toggle is named by its view's name alone after the counts as before them;
    // below 1280 px each control is named by its view's name first, then its signal.
    for (const [what, preset, control] of controls((preset) =>
      row ? PRESET_NAMES[preset] : new RegExp(`^${PRESET_NAMES[preset]}\\b`)
    )) {
      const room = signalRoom(control);
      if (preset === "conversation") {
        expect(room, `${what} holds a room for a signal once the counts arrive`).toBeNull();
        continue;
      }
      expect(
        room?.className,
        `${what}'s room changed its class when the counts arrived, so its width may change`
      ).toBe(classes.get(what));
      expect(room?.hidden, `${what}'s signal is hidden from assistive technology`).toBeNull();
      expect(
        control.querySelector(`[data-preset-count="${preset}"]`)?.textContent,
        `${what}'s room does not draw its count once the counts arrive`
      ).toBe(String(SIGNALS[preset].count));
    }
    expect(missingReferences(view.container), "a control names a missing element").toEqual([]);
  } finally {
    view.unmount();
    restoreMatchMedia();
  }
});

test.each([
  DESKTOP_WIDTH,
  PHONE_WIDTH,
])("Conversation draws no count or missing ARIA reference even when filters have counts (width %i)", async (width) => {
  const user = userEvent.setup();
  const restoreMatchMedia = stubViewport(width);
  const view = render(
    <MemoryRouter initialEntries={[presetPath("CORE-1", "conversation")]}>
      <IssuePresets active="conversation" issueKey="CORE-1" signals={SIGNALS} />
    </MemoryRouter>
  );
  try {
    const conversation = screen.getByRole("button", { name: PRESET_NAMES.conversation });
    if (width === PHONE_WIDTH) {
      await user.click(conversation);
      expect(
        screen.getByRole("menuitemradio", { name: PRESET_NAMES.conversation, checked: true })
      ).toBeInTheDocument();
    }
    expect(
      view.container.querySelector('[data-preset-count="spec"]')?.textContent,
      "Spec did not draw its count"
    ).toBe(String(SIGNALS.spec.count));
    expect(
      view.container.querySelector('[data-preset-count="conversation"]'),
      "Conversation drew a count"
    ).toBeNull();
    expect(missingReferences(view.container), "a control names a missing element").toEqual([]);
  } finally {
    view.unmount();
    restoreMatchMedia();
  }
});
