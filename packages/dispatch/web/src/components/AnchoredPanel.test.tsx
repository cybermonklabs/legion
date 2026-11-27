import { expect, spyOn, test } from "bun:test";
import { readFileSync } from "node:fs";
// biome-ignore lint/style/noRestrictedImports: the outside press below is a pointerdown with no release, and the Escape that ends an IME composition is a key marked as the input method's; user-event has no form for either.
import { act, fireEvent, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useRef, useState } from "react";

import { layOutPanel, stacked } from "../__tests__/scroll-box";
import { KeymapProvider } from "../features/shell/KeymapProvider";
import { useKeymap, useKeymapScope } from "../features/shell/keymap";
import { AnchoredPanel, type PanelDismissal } from "./AnchoredPanel";
import { DisplayMenu } from "./DisplayMenu";
import { MultiSelect } from "./MultiSelect";

/** A page with keys of its own - `j` and ArrowDown, as a table's next row takes them - and its
 *  display menu, a real panel. Every key the page's binding takes is noted in `taken`. */
function Page({ taken }: { taken: string[] }) {
  const [open, setOpen] = useState(false);
  const [sort, setSort] = useState("newest");
  useKeymapScope("inbox");
  useKeymap("inbox", [
    {
      id: "next",
      keys: ["j", "ArrowDown"],
      label: "Next row",
      palette: false,
      run: (event) => taken.push(event.key),
    },
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

/** An element `selector` matches, drawn from it: a tag, classes, and attributes with or without
 *  a value, as `.mark-popover.mark-popover-sheet` and `span[data-dispatch="ask"]` name one. Null
 *  for a selector with a combinator, a pseudo-class or a pseudo-element, which names no one
 *  element. */
function drawnFor(selector: string): HTMLElement | null {
  if (/[\s>+~:()*]/.test(selector)) return null;
  const element = document.createElement(/^[a-z][\w-]*/i.exec(selector)?.[0] ?? "div");
  for (const [, name] of selector.matchAll(/\.([\w-]+)/g)) element.classList.add(name);
  for (const [, name, value] of selector.matchAll(/\[([\w-]+)(?:="([^"]*)")?\]/g)) {
    element.setAttribute(name, value ?? "");
  }
  return element.matches(selector) ? element : null;
}

/** Every style rule of `rules`, and of each rule that groups rules of its own (`@media`). */
function styleRules(rules: CSSRuleList): CSSStyleRule[] {
  return [...rules].flatMap((rule) =>
    rule instanceof CSSStyleRule
      ? [rule]
      : "cssRules" in rule
        ? styleRules((rule as CSSGroupingRule).cssRules)
        : []
  );
}

/** The layer `element` stands in, as the page computes it (`getComputedStyle`): NaN for an
 *  element it puts in no layer of its own (`auto`). */
function layerOf(element: Element): number {
  return Number.parseInt(getComputedStyle(element).zIndex, 10);
}

/**
 * The layers `@legion/proof-editor`'s own stylesheet gives what it draws over a document - its
 * selection bar with Comment and Suggest, its mark popovers, its link card - each read as the page
 * computes it, however the stylesheet writes it: with the stylesheet in the document, an element
 * is drawn for each selector of each of its rules (`drawnFor`) inside the editor's root, and that
 * element's layer is read (`layerOf`). Answers the highest, and each rule that sets a z-index no
 * element drawn for it computes - a selector this draws no element for - so no layer goes unread.
 */
function editorLayers(): { top: number; unread: string[] } {
  const sheet = document.createElement("style");
  sheet.textContent = readFileSync(
    Bun.resolveSync("@legion/proof-editor/style.css", import.meta.dir),
    "utf8"
  );
  document.head.append(sheet);
  const root = document.createElement("div");
  root.className = "proof-editor";
  document.body.append(root);
  try {
    const rules = sheet.sheet?.cssRules;
    if (rules === undefined) throw new Error("the editor's stylesheet was not read");
    let top = Number.NEGATIVE_INFINITY;
    const unread: string[] = [];
    for (const rule of styleRules(rules)) {
      let read = false;
      for (const selector of rule.selectorText.split(",")) {
        const element = drawnFor(selector.trim());
        if (element === null) continue;
        root.append(element);
        const layer = layerOf(element);
        element.remove();
        if (Number.isNaN(layer)) continue;
        read = true;
        top = Math.max(top, layer);
      }
      if (!read && rule.style.zIndex !== "") unread.push(rule.selectorText);
    }
    return { top, unread };
  } finally {
    root.remove();
    sheet.remove();
  }
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
    const editor = editorLayers();
    expect(editor.unread, "the editor's layers no element drawn here computes").toEqual([]);
    expect(editor.top, "the editor's stylesheet names no layer").toBeGreaterThanOrEqual(190);
    await user.click(screen.getByRole("button", { name: "View" }));
    const menu = screen.getByRole("dialog", { name: "View" });
    expect(layerOf(menu), "the display menu's layer").toBeGreaterThan(editor.top);
    await user.keyboard("{Escape}");
    await user.click(screen.getByRole("button", { name: "Version" }));
    const list = screen.getByRole("listbox", { name: "Version options" }).parentElement;
    if (list === null) throw new Error("the Version list is drawn outside its panel");
    expect(layerOf(list), "the Version list's layer").toBeGreaterThan(editor.top);
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

/** A panel cut to the window as the shared list's is: a text box stuck to its top, twelve rows
 *  and a foot stuck to its bottom, or with `parts` false the twelve rows alone. It opens with the
 *  focus left where it is: nothing here reads where the focus goes. */
function FlooredPanel({ parts }: { parts: boolean }) {
  const [open, setOpen] = useState(false);
  const anchor = useRef<HTMLButtonElement>(null);
  return (
    <>
      <button onClick={() => setOpen(true)} ref={anchor} type="button">
        Labels
      </button>
      {open ? (
        <AnchoredPanel
          anchorRef={anchor}
          className=""
          focusOnOpen={false}
          label="Labels"
          onDismiss={() => setOpen(false)}
          role="dialog"
        >
          {parts ? (
            <div data-testid="head" style={{ position: "sticky", top: 0 }}>
              <input aria-label="Search labels" />
            </div>
          ) : null}
          {buttons(Array.from({ length: 12 }, (_, at) => `lab-${at + 1}`))}
          {parts ? (
            <div data-testid="foot" style={{ bottom: 0, position: "sticky" }}>
              {buttons(["Clear labels"])}
            </div>
          ) : null}
        </AnchoredPanel>
      ) : null}
    </>
  );
}

/** The window `FlooredPanel` opens in, and where its trigger stands in it. */
interface WindowLayout {
  /** The window's height. */
  readonly height: number;
  /** How far the page has scrolled at first, 0 where this is not given. */
  readonly pageAt?: number;
  /** How far the page scrolls at most. */
  readonly pageEnd: number;
  /** The trigger's top, in the window, as the page stands at first. */
  readonly top: number;
}

/**
 * Lays out what happy-dom does not for `FlooredPanel`, whose trigger is `trigger`: the window and
 * the page it shows, whose scroll stops at both ends as a browser's does; the trigger, 44 px tall,
 * moved up by the page's scroll; and the panel, whose text box and foot are each 61 px tall and
 * each row 44 px, inside a 1 px border: it holds 2 + 61 + 12 × 44 + 61 = 652 px with its text box
 * and foot, 530 without them, and shows as much as it is cut to. What is measured of the panel's
 * parts is their heights. Answers how far the page has scrolled, and what puts the layout back.
 */
function layOutWindow(
  trigger: HTMLElement,
  layout: WindowLayout,
  parts: boolean
): { page: () => number; restore: () => void } {
  const start = layout.pageAt ?? 0;
  let scrolled = start;
  const holds = 12 * 44 + (parts ? 2 * 61 : 0);
  const root = document.documentElement;
  const innerHeight = Object.getOwnPropertyDescriptor(window, "innerHeight");
  Object.defineProperty(window, "innerHeight", { configurable: true, value: layout.height });
  Object.defineProperties(root, {
    clientHeight: { configurable: true, get: () => layout.height },
    scrollHeight: { configurable: true, get: () => layout.height + layout.pageEnd },
    scrollTop: {
      configurable: true,
      get: () => scrolled,
      set: (value: number) => {
        scrolled = Math.min(Math.max(value, 0), layout.pageEnd);
      },
    },
  });
  // What the panel's box shows, with its border: all it holds, or as much as it is cut to.
  const shows = (panel: HTMLElement) => {
    const cut = Number.parseFloat(panel.style.maxHeight);
    return Number.isNaN(cut) ? holds + 2 : cut;
  };
  const sizes: Record<string, (panel: HTMLElement) => number> = {
    clientHeight: (panel) => shows(panel) - 2,
    offsetHeight: shows,
    scrollHeight: () => holds,
  };
  const own = Object.keys(sizes).map(
    (name) => [name, Object.getOwnPropertyDescriptor(HTMLElement.prototype, name)] as const
  );
  for (const [name, size] of Object.entries(sizes)) {
    Object.defineProperty(HTMLElement.prototype, name, {
      configurable: true,
      get(this: HTMLElement) {
        return this.getAttribute("role") === "dialog" ? size(this) : 0;
      },
    });
  }
  const measure = Element.prototype.getBoundingClientRect;
  const spy = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ) {
    if (this === trigger) return new DOMRect(10, layout.top + start - scrolled, 90, 44);
    const part = this.getAttribute("data-testid");
    if (part === "head" || part === "foot") return new DOMRect(1, 0, 254, 61);
    if (this.parentElement?.getAttribute("role") === "dialog") return new DOMRect(1, 0, 254, 44);
    return measure.call(this);
  });
  return {
    page: () => scrolled,
    restore: () => {
      spy.mockRestore();
      for (const [name, descriptor] of own) {
        if (descriptor === undefined) Reflect.deleteProperty(HTMLElement.prototype, name);
        else Object.defineProperty(HTMLElement.prototype, name, descriptor);
      }
      for (const name of ["clientHeight", "scrollHeight", "scrollTop"]) {
        Reflect.deleteProperty(root, name);
      }
      if (innerHeight === undefined) Reflect.deleteProperty(window, "innerHeight");
      else Object.defineProperty(window, "innerHeight", innerHeight);
    },
  };
}

/** Opens `FlooredPanel` in the window `layout` lays out, and answers where its panel stands: its
 *  top, the height it is cut to ("none" where it is not cut), and how far the page scrolled. */
async function openFloored(layout: WindowLayout, parts = true): Promise<[string, string, number]> {
  const user = userEvent.setup();
  const view = render(<FlooredPanel parts={parts} />);
  const trigger = screen.getByRole("button", { name: "Labels" });
  const laid = layOutWindow(trigger, layout, parts);
  try {
    await user.click(trigger);
    const panel = screen.getByRole("dialog", { name: "Labels" });
    return [panel.style.top, panel.style.maxHeight || "none", laid.page()];
  } finally {
    laid.restore();
    view.unmount();
  }
}

// At 390x360 the issue header's Labels list, its trigger mid-window, was cut to 146 px, and at
// 390x300 to 116: its text box and its Clear foot left 22 px, and then none, for its 44 px rows,
// so it showed no row whole. A cut panel is never cut below its sticky parts and one row. Where
// the roomier side has less room than that, the panel takes that side at that height, and the
// page scrolls the least that shows it within the window.
test("a panel whose roomier side holds less than its text box, one row and its foot takes that side at that height, and the page scrolls the least that shows it", async () => {
  const placed = [
    // Mid-window at 360 px: 146 px under the trigger and 146 over it.
    await openFloored({ height: 360, pageEnd: 400, top: 158 }),
    // Mid-window at 300 px, the page scrolled 100 px: 104 px under the trigger and 128 over it.
    await openFloored({ height: 300, pageAt: 100, pageEnd: 400, top: 140 }),
    // A panel with no text box and no foot is never cut below its border and its first row,
    // 2 + 44 px: here 26 px under its trigger and 26 over it.
    await openFloored({ height: 120, pageEnd: 400, top: 38 }, false),
  ];
  expect(placed).toEqual([
    // 2 + 61 + 44 + 61 = 168 px under the trigger, which the page's scroll by 22 px took up to
    // 136-180: down to the window's 8 px margin.
    ["184px", "168px", 22],
    // 168 px over it, which the page's scroll back by 40 px took down to 180-224.
    ["8px", "168px", 60],
    // 46 px under it, which the page's scroll by 20 px took up to 18-62.
    ["66px", "46px", 20],
  ]);
});

// The least height never draws a panel past the window's edge: where the page cannot scroll so
// far, the panel is cut to the room it has then.
test("a panel the page cannot scroll far enough to show at its least height is cut to the room it then has, never past the window's edge", async () => {
  const placed = [
    // Mid-window at 360 px, the page at its end: 146 px under the trigger and 146 over it.
    await openFloored({ height: 360, pageEnd: 0, top: 158 }),
    // The same, the page 10 px from its end.
    await openFloored({ height: 360, pageEnd: 10, top: 158 }),
  ];
  expect(placed).toEqual([
    ["206px", "146px", 0],
    // The page scrolls its last 10 px, which leave 156 px under the trigger, at 148-192.
    ["196px", "156px", 10],
  ]);
});

// Where the side it takes has room for its least height, a panel is placed and cut as it always
// was, and nothing scrolls.
test("a panel with room for its text box, one row and its foot is placed and cut as before, and the page does not scroll", async () => {
  const placed = [
    // 744 px under the trigger: room for all 652 px.
    await openFloored({ height: 900, pageEnd: 400, top: 100 }),
    // 44 px under it and 788 over it: room over it for all 652 px.
    await openFloored({ height: 900, pageEnd: 400, top: 800 }),
    // 444 px under it and 88 over it.
    await openFloored({ height: 600, pageEnd: 400, top: 100 }),
    // 144 px under it and 388 over it.
    await openFloored({ height: 600, pageEnd: 400, top: 400 }),
  ];
  expect(placed).toEqual([
    ["148px", "none", 0],
    ["144px", "none", 0],
    // Cut to the room under it.
    ["148px", "444px", 0],
    // Cut to the room over it, from the window's 8 px margin.
    ["8px", "388px", 0],
  ]);
});
