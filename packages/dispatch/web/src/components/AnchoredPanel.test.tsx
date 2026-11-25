import { expect, spyOn, test } from "bun:test";
import { readFileSync } from "node:fs";
// biome-ignore lint/style/noRestrictedImports: the outside press below is a pointerdown with no release, and the Escape that ends an IME composition is a key marked as the input method's; user-event has no form for either.
import { act, fireEvent, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useRef, useState } from "react";

import { layOutPanel, stacked } from "../__tests__/scroll-box";
import { KeymapProvider } from "../features/shell/KeymapProvider";
import { useKeymap, useKeymapScope } from "../features/shell/keymap";
import { AnchoredPanel, noteOpenedFrom, type PanelDismissal } from "./AnchoredPanel";
import { DisplayMenu } from "./DisplayMenu";
import { MultiSelect } from "./MultiSelect";

/** A page with keys of its own - `j` and ArrowDown, as the Inbox's next row takes them - and its
 *  display menu, a real panel. Every key the page's bindings take is noted in `taken`. */
function Page({ taken }: { taken: string[] }) {
  const [open, setOpen] = useState(false);
  const [sort, setSort] = useState("newest");
  useKeymapScope("inbox");
  useKeymap("inbox", [
    { action: "inbox.next", run: (event) => taken.push(event.key) },
    { action: "inbox.arrows", run: (event) => taken.push(event.key) },
  ]);
  return (
    <>
      <button type="button">On the page</button>
      <DisplayMenu
        label="View"
        onOpenChange={setOpen}
        open={open}
        sections={[
          {
            id: "sort",
            label: "Sort by",
            onChange: setSort,
            options: [
              { label: "Newest first", value: "newest" },
              { label: "Urgency", value: "urgency" },
            ],
            value: sort,
          },
        ]}
      />
    </>
  );
}

// INFRA-3135's review round 1, follow-up F5: the display menu replaced a select, where a page's
// single keys did nothing, with radios, which take no text; with the menu open, `j` on its radio
// walked the table behind it.
test("with the display menu open and its radio focused, the page's keys fire nothing, the arrows walk the radios, Escape closes it, and a key on the page with it open still fires", async () => {
  const user = userEvent.setup();
  const taken: string[] = [];
  const view = render(
    <KeymapProvider>
      <Page taken={taken} />
    </KeymapProvider>
  );
  try {
    const trigger = screen.getByRole("button", { name: "View" });
    await user.click(trigger);
    const newest = screen.getByRole("radio", { name: "Newest first" });
    expect(newest).toHaveFocus();

    await user.keyboard("j");
    expect(taken, "j on the menu's radio").toEqual([]);
    expect(newest).toHaveFocus();
    expect(screen.getByRole("dialog", { name: "View" })).toBeInTheDocument();

    // The arrows are the radio group's own: the next choice takes the focus and is chosen.
    await user.keyboard("{ArrowDown}");
    expect(taken, "ArrowDown on the menu's radio").toEqual([]);
    const urgency = screen.getByRole("radio", { name: "Urgency" });
    expect(urgency).toHaveFocus();
    expect(urgency).toBeChecked();

    await user.keyboard("{Escape}");
    expect(screen.queryByRole("dialog", { name: "View" })).not.toBeInTheDocument();
    expect(trigger).toHaveFocus();

    // Open, with the focus put back on the page: only a key pressed inside the panel is masked.
    await user.click(trigger);
    act(() => screen.getByRole("button", { name: "On the page" }).focus());
    await user.keyboard("j");
    expect(taken, "j on the page with the menu open").toEqual(["j"]);
    expect(screen.getByRole("dialog", { name: "View" })).toBeInTheDocument();
  } finally {
    view.unmount();
  }
});

/** The highest z-index `@legion/proof-editor`'s own stylesheet gives anything it draws over a
 *  document: its selection bar with Comment and Suggest, its mark popovers, its link card. */
function editorTopLayer(): number {
  const css = readFileSync(
    Bun.resolveSync("@legion/proof-editor/style.css", import.meta.dir),
    "utf8"
  );
  return Math.max(...[...css.matchAll(/z-index:\s*(\d+)/g)].map((layer) => Number(layer[1])));
}

/** A single pick a page draws, open or shut as the reader leaves it. */
function VersionList() {
  const [open, setOpen] = useState(false);
  return (
    <MultiSelect
      emptyMessage="No version matches that."
      label="Version"
      mode="single"
      onOpenChange={setOpen}
      onPick={() => {}}
      open={open}
      options={["", "1"]}
      searchLabel="Search versions"
    />
  );
}

// INFRA-3135, round 12: with text selected in a document, the editor's selection bar (fixed, at
// z-index 190) drew over the options of the Version list opened above it, and a press meant for
// an option landed on Suggest. An open panel - a list, a menu - is the reader's topmost layer,
// over every layer the document draws.
test("an open list or menu stands above every layer a document's own floating bars take", async () => {
  const user = userEvent.setup();
  const view = render(
    <KeymapProvider>
      <Page taken={[]} />
      <VersionList />
    </KeymapProvider>
  );
  try {
    const top = editorTopLayer();
    expect(top, "the editor's stylesheet names no layer").toBeGreaterThanOrEqual(190);
    await user.click(screen.getByRole("button", { name: "View" }));
    const menu = screen.getByRole("dialog", { name: "View" });
    expect(Number(menu.style.zIndex), "the display menu's layer").toBeGreaterThan(top);
    await user.keyboard("{Escape}");
    await user.click(screen.getByRole("button", { name: "Version" }));
    const list = screen.getByRole("listbox", { name: "Version options" }).parentElement;
    expect(Number(list?.style.zIndex), "the Version list's layer").toBeGreaterThan(top);
  } finally {
    view.unmount();
  }
});

/** A panel a host opens from its own anchor, noting each dismissal it is told of. The anchor can
 *  be taken off the page with no press, and a control outside the panel can be pressed. */
function DismissedPanel({ dismissals }: { dismissals: PanelDismissal[] }) {
  const [open, setOpen] = useState(false);
  const [anchored, setAnchored] = useState(true);
  const anchor = useRef<HTMLButtonElement>(null);
  return (
    <>
      {anchored ? (
        <button onClick={() => setOpen(true)} ref={anchor} type="button">
          Open
        </button>
      ) : null}
      <button onClick={() => setAnchored(false)} type="button">
        Outside
      </button>
      {open ? (
        <AnchoredPanel
          anchorRef={anchor}
          className=""
          id="dismissed-panel"
          label="Panel"
          onDismiss={(how) => {
            dismissals.push(how);
            setOpen(false);
          }}
          role="dialog"
        >
          <button type="button">Inside</button>
        </AnchoredPanel>
      ) : null}
    </>
  );
}

// INFRA-3135, round 12: the Agents composer's Issue list hands the reader to the row on Escape
// alone, so the panel the list stands in tells its host what dismissed it, and carries the id its
// anchor names it by.
test("a panel tells its host what dismissed it: Escape, Tab past its end, a press outside, its anchor gone", async () => {
  const user = userEvent.setup();
  const dismissals: PanelDismissal[] = [];
  const view = render(<DismissedPanel dismissals={dismissals} />);
  try {
    const open = screen.getByRole("button", { name: "Open" });
    const outside = screen.getByRole("button", { name: "Outside" });
    await user.click(open);
    expect(document.getElementById("dismissed-panel")).toBe(
      screen.getByRole("dialog", { name: "Panel" })
    );
    expect(screen.getByRole("button", { name: "Inside" })).toHaveFocus();
    await user.keyboard("{Escape}");
    await user.click(open);
    await user.tab();
    await user.click(open);
    // A press with no release, so the control under it does nothing of its own.
    fireEvent.pointerDown(outside);
    await user.click(open);
    // The anchor leaves the page with no press at all.
    act(() => outside.click());
    expect(dismissals).toEqual(["escape", "tab", "press", "gone"]);
    expect(screen.queryByRole("dialog", { name: "Panel" })).toBeNull();
  } finally {
    view.unmount();
  }
});

/** A panel opened from its anchor, with a row beside the anchor that a host may name as where the
 *  focus goes back to (`returnFocus`), or that a key may note as where it opened the panel from
 *  (`noteOpenedFrom`). */
function ReturningPanel({ toRow }: { toRow: boolean }) {
  const [open, setOpen] = useState(false);
  const anchor = useRef<HTMLButtonElement>(null);
  const row = useRef<HTMLButtonElement>(null);
  return (
    <>
      <button ref={row} type="button">
        Row
      </button>
      <button onClick={() => setOpen((was) => !was)} ref={anchor} type="button">
        Open
      </button>
      {open ? (
        <AnchoredPanel
          anchorRef={anchor}
          className=""
          label="Panel"
          onDismiss={() => setOpen(false)}
          returnFocus={toRow ? () => row.current : undefined}
          role="menu"
        >
          <button type="button">Inside</button>
        </AnchoredPanel>
      ) : null}
    </>
  );
}

test("Escape and Tab out of a panel put the focus where its host says, else on its anchor", async () => {
  const user = userEvent.setup();
  const view = render(<ReturningPanel toRow />);
  try {
    const open = screen.getByRole("button", { name: "Open" });
    const row = screen.getByRole("button", { name: "Row" });
    await user.click(open);
    expect(screen.getByRole("menu", { name: "Panel" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Inside" })).toHaveFocus();
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("menu", { name: "Panel" })).toBeNull();
    expect(row).toHaveFocus();
    await user.click(open);
    await user.tab();
    expect(screen.queryByRole("menu", { name: "Panel" })).toBeNull();
    expect(row).toHaveFocus();
  } finally {
    view.unmount();
  }
  const anchored = render(<ReturningPanel toRow={false} />);
  try {
    const open = screen.getByRole("button", { name: "Open" });
    await user.click(open);
    await user.keyboard("{Escape}");
    expect(open).toHaveFocus();
  } finally {
    anchored.unmount();
  }
});

test("Escape goes back to where a key noted it opened the panel from, and the note goes with the panel", async () => {
  const user = userEvent.setup();
  const view = render(<ReturningPanel toRow={false} />);
  try {
    const open = screen.getByRole("button", { name: "Open" });
    const row = screen.getByRole("button", { name: "Row" });
    // A key's binding on the row: it notes the row, then presses the anchor.
    act(() => row.focus());
    act(() => {
      noteOpenedFrom(open);
      open.click();
    });
    expect(screen.getByRole("button", { name: "Inside" })).toHaveFocus();
    await user.keyboard("{Escape}");
    expect(row).toHaveFocus();

    // Tab is the page's own order, so it leaves the reader on the anchor whatever was noted.
    act(() => {
      noteOpenedFrom(open);
      open.click();
    });
    await user.tab();
    expect(open).toHaveFocus();

    // That panel's closing forgot the note: a press on the anchor itself is one from the anchor.
    act(() => row.focus());
    await user.click(open);
    await user.keyboard("{Escape}");
    expect(open).toHaveFocus();
  } finally {
    view.unmount();
  }
});

// dispatch://INFRA-2160, R24: a key pressed on the page itself, with nothing focused, opens a
// panel from there, and Escape puts the reader back on the page rather than on the anchor.
test("Escape in a panel a key opened with nothing focused leaves the focus on the page, and Tab still goes to the anchor", async () => {
  const user = userEvent.setup();
  const view = render(<ReturningPanel toRow={false} />);
  try {
    const open = screen.getByRole("button", { name: "Open" });
    const fromPage = () =>
      act(() => {
        if (document.activeElement instanceof HTMLElement) document.activeElement.blur();
        noteOpenedFrom(open);
        open.click();
      });
    fromPage();
    expect(screen.getByRole("button", { name: "Inside" })).toHaveFocus();
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("menu", { name: "Panel" })).toBeNull();
    expect(document.activeElement).toBe(document.body);

    fromPage();
    await user.tab();
    expect(screen.queryByRole("menu", { name: "Panel" })).toBeNull();
    expect(open).toHaveFocus();
  } finally {
    view.unmount();
  }
});

// Safari sends the Escape that ends an input method's composition after the composition's end,
// unmarked - `isComposing` false - with keyCode 229 (WebKit bug 165004): that Escape cancels what
// the reader was composing and is the input method's, so the panel stays open and its host is
// told of no dismissal. A plain Escape after it closes the panel.
test("an Escape that ends an IME composition in Safari, at keyCode 229, leaves the panel open", async () => {
  const user = userEvent.setup();
  const dismissals: PanelDismissal[] = [];
  const view = render(<DismissedPanel dismissals={dismissals} />);
  try {
    await user.click(screen.getByRole("button", { name: "Open" }));
    const inside = screen.getByRole("button", { name: "Inside" });
    // user-event has no form for this: it cannot mark a key as an IME composition.
    fireEvent.keyDown(inside, { isComposing: false, key: "Escape", keyCode: 229 });
    expect(screen.queryByRole("dialog", { name: "Panel" }), "Escape at keyCode 229").not.toBeNull();
    expect(dismissals).toEqual([]);

    await user.keyboard("{Escape}");
    expect(screen.queryByRole("dialog", { name: "Panel" })).toBeNull();
    expect(dismissals).toEqual(["escape"]);
  } finally {
    view.unmount();
  }
});

/** A panel beside a control that keeps its own press from propagating, as an ask card's hand-on
 *  keeps its press from the row it sits in. */
function PanelBesideAStoppedPress({ dismissals }: { dismissals: PanelDismissal[] }) {
  const [open, setOpen] = useState(false);
  const anchor = useRef<HTMLButtonElement>(null);
  return (
    <>
      <button onClick={() => setOpen(true)} ref={anchor} type="button">
        Open
      </button>
      <button onPointerDown={(event) => event.stopPropagation()} type="button">
        Stopped
      </button>
      {open ? (
        <AnchoredPanel
          anchorRef={anchor}
          className=""
          label="Panel"
          onDismiss={(how) => {
            dismissals.push(how);
            setOpen(false);
          }}
          role="dialog"
        >
          <button type="button">Inside</button>
        </AnchoredPanel>
      ) : null}
    </>
  );
}

// INFRA-3135, round 12: the panel listened for a press outside it as the press bubbled up to the
// document, so a control that stopped its press on the way - the ask card's hand-on, keeping it
// from the row under it - kept every open panel open.
test("a press outside on a control that stops it from propagating still closes the panel", async () => {
  const user = userEvent.setup();
  const dismissals: PanelDismissal[] = [];
  const view = render(<PanelBesideAStoppedPress dismissals={dismissals} />);
  try {
    await user.click(screen.getByRole("button", { name: "Open" }));
    expect(screen.getByRole("dialog", { name: "Panel" })).toBeTruthy();
    // A press with no release, as the outside press above is.
    fireEvent.pointerDown(screen.getByRole("button", { name: "Stopped" }));
    expect(screen.queryByRole("dialog", { name: "Panel" })).toBeNull();
    expect(dismissals).toEqual(["press"]);
  } finally {
    view.unmount();
  }
});

/** A panel a host opens from its own anchor, both named `label`. */
function OpenPanel({ children, label }: { children: ReactNode; label: string }) {
  const [open, setOpen] = useState(false);
  const anchor = useRef<HTMLButtonElement>(null);
  return (
    <>
      <button onClick={() => setOpen(true)} ref={anchor} type="button">
        {label}
      </button>
      {open ? (
        <AnchoredPanel
          anchorRef={anchor}
          className=""
          label={label}
          onDismiss={() => setOpen(false)}
          role="dialog"
        >
          {children}
        </AnchoredPanel>
      ) : null}
    </>
  );
}

/** A button for each of `names`. */
function buttons(names: string[]): ReactNode {
  return names.map((name) => (
    <button key={name} type="button">
      {name}
    </button>
  ));
}

// INFRA-3135, round 26: Firefox left a control the Tab key reached partly outside a scrolled
// panel's box - Remove Assignee: everyone filter in the Inbox's Filter panel, 19 px below it -
// where Chromium and WebKit scroll it in themselves. Here no engine scrolls the panel itself.
test("a control the focus reaches in a panel stands whole in the panel's box, which scrolls the least that shows it, and nothing else scrolls", async () => {
  const user = userEvent.setup();
  const view = render(
    <OpenPanel label="Filters">
      {buttons(["Alpha", "Beta", "Gamma", "Delta", "Epsilon", "Zeta"])}
    </OpenPanel>
  );
  const scrolls = spyOn(Element.prototype, "scrollIntoView");
  try {
    await user.click(screen.getByRole("button", { name: "Filters" }));
    const panel = screen.getByRole("dialog", { name: "Filters" });
    // Six controls hold 8 + 6 × 52 px.
    layOutPanel(panel, 320, stacked(within(panel).getAllByRole("button")));
    expect(screen.getByRole("button", { name: "Alpha" })).toHaveFocus();
    // The control each key puts the focus on, and how far the panel has scrolled then: the panel
    // shows 0-198 px of what it holds, and the fourth control, Delta, stands at 164-208.
    const walked: [string, number][] = [];
    for (const shift of [false, false, false, false, false, true, true, true, true, true]) {
      await user.tab({ shift });
      walked.push([document.activeElement?.textContent ?? "nothing", panel.scrollTop]);
    }
    expect(walked).toEqual([
      ["Beta", 0],
      ["Gamma", 0],
      ["Delta", 10],
      ["Epsilon", 62],
      ["Zeta", 114],
      // Already whole, so nothing scrolls.
      ["Epsilon", 114],
      ["Delta", 114],
      // Gamma, at 112-156, starts 2 px above the box. Back up, each control stands at the box's
      // top edge, so the first leaves the panel's 8 px of padding above it out of sight.
      ["Gamma", 112],
      ["Beta", 60],
      ["Alpha", 8],
    ]);
    expect(scrolls).not.toHaveBeenCalled();
  } finally {
    scrolls.mockRestore();
    view.unmount();
  }
});

// INFRA-3135, round 26's goal red team: a pointer press focuses its control as the pointer goes
// down, and a scroll then moves the control under the pointer before it comes up - 24 px on a chip
// cut by the bottom of the Inbox's +N more panel, in Chromium, Firefox and WebKit (builds Job
// infra-3135-probe1-8lssx). So the focus a press makes scrolls nothing, and the next control a key
// reaches is still shown whole.
test("a control a pointer press focuses in a panel scrolls nothing while the press is down, and a key's focus after it still shows its control whole", async () => {
  const user = userEvent.setup();
  const view = render(
    <OpenPanel label="Filters">
      {buttons(["Alpha", "Beta", "Gamma", "Delta", "Epsilon", "Zeta"])}
    </OpenPanel>
  );
  try {
    await user.click(screen.getByRole("button", { name: "Filters" }));
    const panel = screen.getByRole("dialog", { name: "Filters" });
    // Six controls hold 8 + 6 × 52 px; the panel shows 0-198 px of them, and Delta stands at
    // 164-208, 10 px below what the panel shows.
    layOutPanel(panel, 320, stacked(within(panel).getAllByRole("button")));
    const delta = screen.getByRole("button", { name: "Delta" });
    // The press as a browser makes it: the pointer goes down on Delta, and its mousedown focuses
    // Delta, with the button still held.
    await user.pointer({ keys: "[MouseLeft>]", target: delta });
    expect([document.activeElement?.textContent, panel.scrollTop]).toEqual(["Delta", 0]);
    await user.pointer({ keys: "[/MouseLeft]", target: delta });
    // Epsilon, at 216-260, ends 62 px below what the panel shows.
    await user.tab();
    expect([document.activeElement?.textContent, panel.scrollTop]).toEqual(["Epsilon", 62]);
  } finally {
    view.unmount();
  }
});

// A press can end with no release: the browser cancels a press it turns into a scroll or a drag,
// a context menu opened as the button goes down takes the pointerup itself, a drag ends at its
// dragend, and the window can lose the focus mid-press. Each ends the press, as `hearPresses`
// reads one, so a key's focus after it still shows its control.
for (const [ending, end] of [
  // user-event has no form for a press the browser cancels.
  ["its pointercancel", (target: HTMLElement) => fireEvent.pointerCancel(target)],
  // user-event has no form for a context menu that takes the press's release.
  ["its context menu", (target: HTMLElement) => fireEvent.contextMenu(target)],
  // user-event has no form for a drag, and so none for a drag's end.
  ["its drag's end", (target: HTMLElement) => fireEvent.dragEnd(target)],
  // user-event has no form for the window losing the focus.
  ["the window's blur", () => fireEvent.blur(window)],
] as const) {
  test(`a pointer press in a panel that ends at ${ending} lets a key's focus after it show its control whole`, async () => {
    const user = userEvent.setup();
    const view = render(
      <OpenPanel label="Filters">
        {buttons(["Alpha", "Beta", "Gamma", "Delta", "Epsilon", "Zeta"])}
      </OpenPanel>
    );
    try {
      await user.click(screen.getByRole("button", { name: "Filters" }));
      const panel = screen.getByRole("dialog", { name: "Filters" });
      layOutPanel(panel, 320, stacked(within(panel).getAllByRole("button")));
      const delta = screen.getByRole("button", { name: "Delta" });
      // The pointer goes down on Delta and focuses it, and no release follows.
      await user.pointer({ keys: "[MouseLeft>]", target: delta });
      act(() => end(delta));
      await user.tab();
      expect([document.activeElement?.textContent, panel.scrollTop]).toEqual(["Epsilon", 62]);
    } finally {
      view.unmount();
    }
  });
}

// What a box inside the panel cuts off is drawn nowhere, so no scroll of the panel shows it: the
// panel shows whole what that box draws of the control.
test("a control in a scroll box of its own inside the panel: the panel scrolls the least that shows whole what that box shows of it, and nothing for one that box shows none of", async () => {
  const user = userEvent.setup();
  const view = render(
    <OpenPanel label="Filters">
      {buttons(["Before", "Middle"])}
      <div data-testid="scroll-box" style={{ overflowY: "auto" }}>
        {buttons(["First", "Second", "Third", "Fourth"])}
      </div>
      {buttons(["After"])}
    </OpenPanel>
  );
  try {
    await user.click(screen.getByRole("button", { name: "Filters" }));
    const panel = screen.getByRole("dialog", { name: "Filters" });
    const box = screen.getByTestId("scroll-box");
    const [before, middle, first, second, third, fourth, after] =
      within(panel).getAllByRole("button");
    // The box stands at 112-232 px of what the panel holds and shows 120 px of its controls,
    // scrolled to its top: it cuts Third, at 216-260, at 232, and draws nothing of Fourth, at
    // 268-312. After stands under the box, at 240-284, and the panel holds 292 px.
    Object.defineProperty(box, "clientHeight", { configurable: true, get: () => 120 });
    layOutPanel(panel, 292, [
      [before, 8, 44],
      [middle, 60, 44],
      [box, 112, 120],
      [first, 112, 44],
      [second, 164, 44],
      [third, 216, 44],
      [fourth, 268, 44],
      [after, 240, 44],
    ]);
    expect(before).toHaveFocus();
    const walked: [string, number][] = [];
    for (let press = 0; press < 6; press += 1) {
      await user.tab();
      walked.push([document.activeElement?.textContent ?? "nothing", panel.scrollTop]);
    }
    expect(walked).toEqual([
      ["Middle", 0],
      ["First", 0],
      ["Second", 10],
      // What the box draws of it, 216-232, stands at the panel's bottom edge.
      ["Third", 34],
      // The box draws nothing of it, so nothing scrolls.
      ["Fourth", 34],
      ["After", 86],
    ]);
  } finally {
    view.unmount();
  }
});

// INFRA-3135, round 26: the shared list's text box and its Clear foot are children of the panel's
// own box, stuck to its top and its bottom, and its rows scroll under both; a row that took the
// focus under either, as a screen reader's cursor puts it there, stood covered, since the panel
// counted its whole box as in view. A control the focus reaches stands whole in the part of the
// box neither covers, and a control inside one of them, which stands where it sticks whatever the
// panel's scroll, scrolls nothing.
test("a control the focus reaches in a panel stands whole between the panel's sticky top and bottom children, and one inside such a child scrolls nothing", async () => {
  const user = userEvent.setup();
  const view = render(
    <OpenPanel label="Status">
      <div data-testid="head" style={{ position: "sticky", top: 0 }}>
        Statuses
      </div>
      {buttons(["Alpha", "Beta", "Gamma"])}
      <div data-testid="foot" style={{ bottom: 0, position: "sticky" }}>
        {buttons(["Clear"])}
      </div>
    </OpenPanel>
  );
  try {
    await user.click(screen.getByRole("button", { name: "Status" }));
    const panel = screen.getByRole("dialog", { name: "Status" });
    const [alpha, beta, gamma, clear] = within(panel).getAllByRole("button");
    // The panel shows 0-198 px of the 300 px it holds. Its 50 px top child covers 0-50 of that,
    // and its 60 px bottom child 138-198, each where it sticks whatever the panel's scroll, so the
    // part neither covers is 50-138. Alpha stands at 50-90, Beta at 98-138, and Gamma at 148-188,
    // inside the box but under the bottom child, which holds Clear.
    layOutPanel(panel, 300, [
      [alpha, 50, 40],
      [beta, 98, 40],
      [gamma, 148, 40],
    ]);
    screen.getByTestId("head").getBoundingClientRect = () => new DOMRect(1, 101, 254, 50);
    screen.getByTestId("foot").getBoundingClientRect = () => new DOMRect(1, 239, 254, 60);
    clear.getBoundingClientRect = () => new DOMRect(9, 247, 238, 44);
    expect(alpha).toHaveFocus();
    // The control each key puts the focus on, and how far the panel has scrolled then.
    const walked: [string, number][] = [];
    for (const shift of [false, false, false, true, true, true]) {
      await user.tab({ shift });
      walked.push([document.activeElement?.textContent ?? "nothing", panel.scrollTop]);
    }
    expect(walked).toEqual([
      // Its bottom edge on the bottom child's top edge: whole, so nothing scrolls.
      ["Beta", 0],
      // Down to the bottom child's top edge.
      ["Gamma", 50],
      // Inside the bottom child, which no scroll moves.
      ["Clear", 50],
      ["Gamma", 50],
      // Beta, now at 48-88, starts 2 px under the top child: up to its bottom edge.
      ["Beta", 48],
      ["Alpha", 0],
    ]);
  } finally {
    view.unmount();
  }
});

// A panel opened from inside another - a filter panel's list - is portalled out of it to the body,
// so a focus there is that panel's own and none of the panel it was opened from.
test("a control the focus reaches in a panel opened from inside a panel stands whole in its own panel's box, and the panel it was opened from scrolls nothing", async () => {
  const user = userEvent.setup();
  const view = render(
    <OpenPanel label="Filters">
      <OpenPanel label="Assignee">{buttons(["alice", "bob", "carol", "dave", "erin"])}</OpenPanel>
      {buttons(["Labels", "Status", "Project", "Priority", "Epic"])}
    </OpenPanel>
  );
  try {
    await user.click(screen.getByRole("button", { name: "Filters" }));
    const filters = screen.getByRole("dialog", { name: "Filters" });
    // Six controls hold 8 + 6 × 52 px: Assignee, which opens the inner panel, and five more.
    layOutPanel(filters, 320, stacked(within(filters).getAllByRole("button")));
    await user.click(screen.getByRole("button", { name: "Assignee" }));
    const assignee = screen.getByRole("dialog", { name: "Assignee" });
    expect(screen.getByRole("button", { name: "alice" })).toHaveFocus();
    // Five controls hold 8 + 5 × 52 px, in a box 400 px down the window: each stands below the
    // Filters panel's box, which would scroll to its end to show any of them.
    layOutPanel(assignee, 268, stacked(within(assignee).getAllByRole("button")), 400);
    // The control each Tab puts the focus on, and how far each panel has scrolled then.
    const walked: [string, number, number][] = [];
    for (let press = 0; press < 3; press += 1) {
      await user.tab();
      walked.push([
        document.activeElement?.textContent ?? "nothing",
        assignee.scrollTop,
        filters.scrollTop,
      ]);
    }
    expect(walked).toEqual([
      ["bob", 0, 0],
      ["carol", 0, 0],
      ["dave", 10, 0],
    ]);
  } finally {
    view.unmount();
  }
});
