import { beforeAll, expect, spyOn, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useState } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import type { IssueDetails } from "../../api/types";
import { DESKTOP_WIDTH, PHONE_WIDTH, stubViewport } from "../margin/margin-fixture";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { buildIssuePath, parseIssuePath } from "../refs/routes";
import { KeymapProvider } from "../shell/KeymapProvider";
import { useKeymap } from "../shell/keymap";
import { useDialog } from "../shell/useDialog";
import { artifact, issueDetails } from "./decisions-filter/decisions-fixtures";
import { IssuePresets, WORDS_FILTER_ATTRIBUTE } from "./IssuePresets";
import { presetPath } from "./issue-address";
import { CONTROL_LABELS, PRESET_NAMES, PRESET_ORDER, type Preset } from "./page-words";
import { type Filter, isFilter, type PresetSignal } from "./presets";
import { ProvidersSlot } from "./slots/providers";
import { AppProviders, stubReads } from "./slots/stage-fixtures";
import { ViewRowSlot } from "./slots/view-row";

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
      ["{ArrowDown}", "documents"],
      ["{ArrowDown}", "dashboard"],
      ["{ArrowUp}", "documents"],
      ["{Home}", "dashboard"],
      ["{End}", "documents"],
      ["{ArrowUp}", "decisions"],
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
// row of six.
test("with no signals the row of six draws every view's control with its name alone, and no description names a missing element", () => {
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
// it, which it keeps once the signal is in it. Dashboard, Conversation and Documents draw no
// signal and hold no room. From 1280 px the signal describes its toggle, so no toggle's name
// changes as it arrives; below 1280 px the one control and each item of its menu are named by the
// signal beside their name (WCAG 2.5.3), so each is named by its view's name alone before it and
// by that name first after it. The case draws the row of seven at 1440 px (`DESKTOP_WIDTH`) and
// the one control and its menu at 390 px (`PHONE_WIDTH`).
test.each([
  DESKTOP_WIDTH,
  PHONE_WIDTH,
])("before the counts arrive each filter control holds its signal's room, empty and hidden, in the class it keeps once they do, Dashboard, Conversation and Documents hold none, and no name changes but by the signal (width %i)", async (width) => {
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
    // row's seven toggles, each read in the box it stands in with its signal's label, or the one
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
      if (!isFilter(preset)) {
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
      if (!isFilter(preset)) {
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

/** The issue the page's own view row is drawn for below: its spec, and a document of no role,
 *  which Documents' list opens. */
const NOTES = artifact("artifact-notes", "notes-md");
const ROW_ISSUE: IssueDetails = {
  ...issueDetails(),
  artifacts: [artifact("artifact-spec", "spec", true), NOTES],
};

/** Documents' list's own address, and the notes' own, which a row of the list opens. */
const DOCUMENTS = presetPath("CORE-1", "documents");
const NOTES_ADDRESS = buildIssuePath({ key: "CORE-1", kind: "artifact", slug: NOTES.slug });

/** The page's own view row at the address the router holds, under the page's providers, as the
 *  page's root draws it, and where the router is: the address and the location's state. */
function PageRow(): ReactNode {
  const { pathname, search, state } = useLocation();
  const route = parseIssuePath(pathname, search);
  if (route === undefined) throw new Error(`${pathname} names no route`);
  return (
    <ProvidersSlot issue={ROW_ISSUE} route={route}>
      <ViewRowSlot issue={ROW_ISSUE} route={route} />
      <output aria-label="where" data-state={JSON.stringify(state ?? null)}>
        {`${pathname}${search}`}
      </output>
    </ProvidersSlot>
  );
}

/** The page's view row at `address`, with `state` in its location where one is given, at
 *  `width` px, 1440 unless given, its reads answered; `done` takes it down and puts the width
 *  and the reads back. */
function drawPageRow(
  address: string,
  { state, width = DESKTOP_WIDTH }: { state?: unknown; width?: number } = {}
): { container: HTMLElement; done: () => void } {
  const restoreMatchMedia = stubViewport(width);
  const reads = stubReads();
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter initialEntries={[{ pathname: address, state }]}>
      <AppProviders client={client}>
        <PageRow />
      </AppProviders>
    </MemoryRouter>
  );
  return {
    container: view.container,
    done: () => {
      view.unmount();
      restoreMatchMedia();
      for (const read of reads) read.mockRestore();
    },
  };
}

/** Where the page's row is, as `PageRow` draws it. */
function where(): HTMLElement {
  return screen.getByRole("status", { name: "where" });
}

/** The row of controls' toggle of `preset`, from 1280 px, named by its name alone. */
function toggle(preset: Preset): HTMLElement {
  return screen.getByRole("button", { name: PRESET_NAMES[preset] });
}

// Part N's R4: Documents is a control of the row in root R2's order - after Threads and before
// Graph of the controls `main` holds, which today is after Decisions - and where the row is one
// control, at 1024 px and below, an entry of its menu.
test("Documents is a control after Decisions, and an entry of the one control", async () => {
  const user = userEvent.setup();
  for (const width of [DESKTOP_WIDTH, 1024]) {
    const restoreMatchMedia = stubViewport(width);
    const view = render(
      <MemoryRouter initialEntries={[presetPath("CORE-1", "conversation")]}>
        <IssuePresets active="conversation" issueKey="CORE-1" signals={SIGNALS} />
        <Address />
      </MemoryRouter>
    );
    try {
      if (width === DESKTOP_WIDTH) {
        const names = within(screen.getByRole("toolbar"))
          .getAllByRole("button")
          .map((button) => button.textContent);
        expect(names.indexOf(PRESET_NAMES.documents), "Documents is not after Decisions").toBe(
          names.indexOf(PRESET_NAMES.decisions) + 1
        );
        expect(toggle("documents")).toHaveAttribute("aria-pressed", "false");
        await user.click(toggle("documents"));
      } else {
        await user.click(screen.getByRole("button", { name: PRESET_NAMES.conversation }));
        const documents = screen.getByRole("menuitemradio", { name: PRESET_NAMES.documents });
        expect(documents).toHaveAttribute("aria-checked", "false");
        expect(menuItem("decisions").nextElementSibling, "Documents is not after Decisions").toBe(
          documents
        );
        await user.click(documents);
      }
      expect(screen.getByRole("status", { name: "address" }).textContent, `${width} px`).toBe(
        DOCUMENTS
      );
    } finally {
      view.unmount();
      restoreMatchMedia();
    }
  }
});

// Part N's R4 and ST-7: a press on Documents from any other control opens its list at its own
// address and turns that control off, and hands the list the address it was pressed at, which
// the list's Escape goes back to (the location's `outline`).
test("pressing Documents opens its list and turns every other control off", async () => {
  const user = userEvent.setup();
  for (const from of PRESET_ORDER.filter((preset) => preset !== "documents")) {
    const row = drawPageRow(presetPath("CORE-1", from));
    try {
      expect(toggle(from)).toHaveAttribute("aria-pressed", "true");
      await user.click(toggle("documents"));
      await waitFor(() => expect(where().textContent).toBe(DOCUMENTS));
      expect(toggle("documents"), `from ${PRESET_NAMES[from]}`).toHaveAttribute(
        "aria-pressed",
        "true"
      );
      for (const other of PRESET_ORDER.filter((preset) => preset !== "documents")) {
        expect(toggle(other), `${PRESET_NAMES[other]} stayed on`).toHaveAttribute(
          "aria-pressed",
          "false"
        );
      }
      expect(JSON.parse(where().dataset.state ?? "null")).toEqual({
        outline: presetPath("CORE-1", from),
      });
    } finally {
      row.done();
    }
  }
});

// Part N's R4: Documents is on while its list is drawn and while a document opened from the
// list is (the location's `outline` is the list's address); the same document opened from
// anywhere else is the page with no filter on.
test("Documents stays active while a document opened from its list is drawn", async () => {
  const fromList = drawPageRow(NOTES_ADDRESS, { state: { outline: DOCUMENTS } });
  try {
    await screen.findByRole("button", { name: CONTROL_LABELS.version });
    expect(toggle("documents")).toHaveAttribute("aria-pressed", "true");
    expect(toggle("conversation")).toHaveAttribute("aria-pressed", "false");
  } finally {
    fromList.done();
  }
  const fromView = drawPageRow(NOTES_ADDRESS, { state: { outline: presetPath("CORE-1", "spec") } });
  try {
    await screen.findByRole("button", { name: CONTROL_LABELS.version });
    expect(toggle("documents")).toHaveAttribute("aria-pressed", "false");
    expect(toggle("conversation")).toHaveAttribute("aria-pressed", "true");
  } finally {
    fromView.done();
  }
});

// Part N's R4 and ST-7: a press on Documents while it is on - on its list, or on a document
// opened from it - returns to Conversation, as a press on the view that is on turns it off.
test("pressing Documents while it is active returns to Conversation", async () => {
  const user = userEvent.setup();
  for (const [address, state] of [
    [DOCUMENTS, undefined],
    [NOTES_ADDRESS, { outline: DOCUMENTS }],
  ] as const) {
    const row = drawPageRow(address, { state });
    try {
      expect(toggle("documents"), address).toHaveAttribute("aria-pressed", "true");
      await user.click(toggle("documents"));
      await waitFor(() => expect(where().textContent).toBe(presetPath("CORE-1", "conversation")));
      expect(toggle("conversation"), address).toHaveAttribute("aria-pressed", "true");
      expect(toggle("documents"), address).toHaveAttribute("aria-pressed", "false");
    } finally {
      row.done();
    }
  }
});

// The row is one tab stop that the arrows, Home and End move along, Documents among the rest,
// from Dashboard, the row's first control (dispatch://INFRA-5702/spec#b-r-new-page), to
// Documents, its last.
test("the arrows move along the row across Documents", async () => {
  const user = userEvent.setup();
  const restoreMatchMedia = stubViewport(DESKTOP_WIDTH);
  const view = render(
    <MemoryRouter initialEntries={[presetPath("CORE-1", "decisions")]}>
      <IssuePresets active="decisions" issueKey="CORE-1" signals={SIGNALS} />
    </MemoryRouter>
  );
  try {
    toggle("decisions").focus();
    for (const [key, to] of [
      ["{ArrowRight}", "documents"],
      ["{ArrowRight}", "dashboard"],
      ["{ArrowLeft}", "documents"],
      ["{ArrowLeft}", "decisions"],
      ["{Home}", "dashboard"],
      ["{End}", "documents"],
    ] as const) {
      await user.keyboard(key);
      expect(document.activeElement, `${key} did not move to ${PRESET_NAMES[to]}`).toBe(toggle(to));
    }
    expect(
      within(screen.getByRole("toolbar"))
        .getAllByRole("button")
        .filter((button) => button.tabIndex === 0),
      "the row is not one tab stop, on Documents"
    ).toEqual([toggle("documents")]);
  } finally {
    view.unmount();
    restoreMatchMedia();
  }
});

// Part N's R4 and ST-1: a control that draws no outline draws no filter by words - neither its
// Filter in the row nor the box, nor an Outline sheet to hold them - on its own address and, for
// Documents, on a document opened from its list. Of the three, Threads alone is not yet held: it
// joins the list below as its part adds its control, which is no filter.
test("Dashboard, Threads and Documents draw no Filter", async () => {
  const outlineless = PRESET_ORDER.filter(
    (preset) => preset !== "conversation" && !isFilter(preset)
  );
  for (const width of [DESKTOP_WIDTH, 1000]) {
    // Conversation draws it, in the row or behind its Outline control, so what is read below is
    // there to find.
    const conversation = drawPageRow(presetPath("CORE-1", "conversation"), { width });
    try {
      expect(
        conversation.container.querySelector(
          `[${WORDS_FILTER_ATTRIBUTE}], button[aria-haspopup="dialog"]`
        ),
        `Conversation draws no Filter at ${width} px`
      ).not.toBeNull();
    } finally {
      conversation.done();
    }
    for (const [address, state] of [
      ...outlineless.map((preset) => [presetPath("CORE-1", preset), undefined] as const),
      [NOTES_ADDRESS, { outline: DOCUMENTS }] as const,
    ]) {
      const row = drawPageRow(address, { state, width });
      try {
        await waitFor(() => expect(where().textContent).toBe(address));
        expect(
          row.container.querySelector(`[${WORDS_FILTER_ATTRIBUTE}]`),
          `${address} draws a Filter at ${width} px`
        ).toBeNull();
        expect(
          row.container.querySelector('button[aria-haspopup="dialog"]'),
          `${address} draws an Outline sheet at ${width} px`
        ).toBeNull();
      } finally {
        row.done();
      }
    }
  }
});

// Part N's R4: Documents counts nothing. Once the tree's reads are in, the filters draw their
// counts, and Documents' control draws none and is described by none, from 1280 px and in the one
// control's menu, which names it alone.
test("Documents draws no count", async () => {
  const user = userEvent.setup();
  const wide = drawPageRow(DOCUMENTS);
  try {
    await waitFor(() =>
      expect(wide.container.querySelector('[data-preset-count="spec"]')).not.toBeNull()
    );
    expect(wide.container.querySelector('[data-preset-count="documents"]')).toBeNull();
    expect(toggle("documents")).not.toHaveAttribute("aria-describedby");
  } finally {
    wide.done();
  }
  const narrow = drawPageRow(DOCUMENTS, { width: 1000 });
  try {
    await user.click(screen.getByRole("button", { name: PRESET_NAMES.documents }));
    // The menu names each filter by its name and its count once the counts are read.
    await within(screen.getByRole("menu")).findByRole("menuitemradio", { name: /^Spec \d/ });
    expect(narrow.container.querySelector('[data-preset-count="documents"]')).toBeNull();
    expect(
      screen.getByRole("menuitemradio", { name: PRESET_NAMES.documents, checked: true })
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: PRESET_NAMES.documents })).toBeInTheDocument();
  } finally {
    narrow.done();
  }
});

// dispatch://INFRA-5702/spec#b-r-new-page (R2): Dashboard is the first control of the row, in
// both its shapes, drawn and pressed as every control is: pressing it while it is on returns to
// Conversation, at Conversation's own address, and Conversation's control opens that address
// from Dashboard's, the issue's own among them. Dashboard draws no signal: what waits on the
// reader is counted in the header.
test("Dashboard is the first control, draws no count, and a press of it while it is on returns to Conversation", async () => {
  const user = userEvent.setup();
  const restoreMatchMedia = stubViewport(DESKTOP_WIDTH);
  const view = render(
    <MemoryRouter initialEntries={["/issues/CORE-1"]}>
      <IssuePresets active="dashboard" issueKey="CORE-1" signals={SIGNALS} />
      <Address />
    </MemoryRouter>
  );
  try {
    const dashboard = screen.getByRole("button", { name: PRESET_NAMES.dashboard, pressed: true });
    expect(
      within(screen.getByRole("toolbar")).getAllByRole("button")[0],
      "Dashboard is not the row's first control"
    ).toBe(dashboard);
    expect(dashboard.getAttribute("aria-describedby"), "Dashboard is described by a signal").toBe(
      null
    );
    expect(
      view.container.querySelector('[data-preset-count="dashboard"]'),
      "Dashboard drew a count"
    ).toBeNull();
    await user.click(dashboard);
    expect(screen.getByRole("status", { name: "address" }).textContent).toBe(
      presetPath("CORE-1", "conversation")
    );
  } finally {
    view.unmount();
    restoreMatchMedia();
  }

  const restorePhone = stubViewport(PHONE_WIDTH);
  const phone = render(
    <MemoryRouter initialEntries={["/issues/CORE-1"]}>
      <IssuePresets active="dashboard" issueKey="CORE-1" signals={SIGNALS} />
      <Address />
    </MemoryRouter>
  );
  try {
    await user.click(screen.getByRole("button", { name: PRESET_NAMES.dashboard }));
    expect(screen.getAllByRole("menuitemradio")[0]?.getAttribute("aria-checked")).toBe("true");
    expect(screen.getAllByRole("menuitemradio")[0]).toBe(menuItem("dashboard"));
    await user.click(menuItem("conversation"));
    expect(screen.getByRole("status", { name: "address" }).textContent).toBe(
      presetPath("CORE-1", "conversation")
    );
  } finally {
    phone.unmount();
    restorePhone();
  }
});
