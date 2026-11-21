import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
// biome-ignore lint/style/noRestrictedImports: the outside press below is a pointerdown with no release, which user-event has no form for.
import { act, fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useRef, useState } from "react";

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
