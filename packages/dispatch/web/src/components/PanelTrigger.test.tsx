import { expect, test } from "bun:test";
import { act, render, screen, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { type ReactNode, useState } from "react";

import { disarm } from "../__tests__/armed-press";
import { ControlRow } from "./ControlRow";
import { filterChips } from "./filter-chips";
import { holdHandOff } from "./focus-place";
import { MultiSelect } from "./MultiSelect";
import { PanelTrigger } from "./PanelTrigger";

function Host({ count, held = false }: { count?: number; held?: boolean }) {
  const [open, setOpen] = useState(false);
  return (
    <>
      <PanelTrigger
        ariaDisabled={held}
        count={count}
        label="Columns"
        onOpenChange={setOpen}
        open={open}
      >
        {(close) => (
          <button onClick={close} type="button">
            Done
          </button>
        )}
      </PanelTrigger>
      <button type="button">Elsewhere</button>
    </>
  );
}

test("the panel is on the page only while it is open, and the trigger names what it opens", async () => {
  const user = userEvent.setup();
  const view = render(<Host />);
  try {
    const trigger = screen.getByRole("button", { name: "Columns" });
    expect(trigger).toHaveAttribute("data-control", "panel-trigger");
    expect(trigger).toHaveAttribute("aria-haspopup", "dialog");
    expect(trigger).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();

    await user.click(trigger);
    expect(screen.getByRole("dialog", { name: "Columns" })).toBeTruthy();
    expect(trigger).toHaveAttribute("aria-expanded", "true");
  } finally {
    view.unmount();
  }
});

test("a press tells the host to open, and the next one to close: the host owns which is open", async () => {
  const user = userEvent.setup();
  const presses: boolean[] = [];
  const view = render(
    <PanelTrigger label="Columns" onOpenChange={(next) => presses.push(next)} open={false}>
      {() => <span>Nothing here.</span>}
    </PanelTrigger>
  );
  try {
    await user.click(screen.getByRole("button", { name: "Columns" }));
    expect(presses).toEqual([true]);

    view.rerender(
      <PanelTrigger label="Columns" onOpenChange={(next) => presses.push(next)} open>
        {() => <span>Nothing here.</span>}
      </PanelTrigger>
    );
    expect(screen.getByRole("button", { name: "Columns" })).toHaveAttribute(
      "aria-expanded",
      "true"
    );
    await user.click(screen.getByRole("button", { name: "Columns" }));
    expect(presses).toEqual([true, false]);
  } finally {
    view.unmount();
  }
});

test("the close the panel is handed shuts it and leaves the reader on the trigger", async () => {
  const user = userEvent.setup();
  const view = render(<Host />);
  try {
    const trigger = screen.getByRole("button", { name: "Columns" });
    await user.click(trigger);
    await user.click(screen.getByRole("button", { name: "Done" }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(trigger).toHaveFocus();
  } finally {
    view.unmount();
  }
});

// A held trigger (`ariaDisabled`, a write it leads to out) refuses only an open: a press on it
// still closes the panel it has open, so a hold that comes while the panel is open never leaves the
// reader unable to shut it, and a press on it while the panel is closed opens nothing. It keeps
// the focus throughout.
test("a held trigger closes the panel it has open, and opens none while closed", async () => {
  const user = userEvent.setup();
  const view = render(<Host />);
  try {
    const trigger = screen.getByRole("button", { name: "Columns" });
    await user.click(trigger);
    expect(screen.getByRole("dialog", { name: "Columns" })).toBeTruthy();

    view.rerender(<Host held />);
    expect(trigger).toHaveAttribute("aria-disabled", "true");
    await user.click(trigger);
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(trigger).toHaveAttribute("aria-expanded", "false");

    await user.click(trigger);
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(trigger).toHaveAttribute("aria-expanded", "false");
    expect(trigger).toHaveFocus();
  } finally {
    view.unmount();
  }
});

test("a count given is read on the trigger, the way a multi-select counts its picks", () => {
  const view = render(<Host count={2} />);
  try {
    expect(screen.getByRole("button", { name: "Columns · 2" })).toBeTruthy();
  } finally {
    view.unmount();
  }
});

test("a title is the accessible name of both the trigger and the panel it opens", async () => {
  const user = userEvent.setup();
  const view = render(
    <PanelTrigger
      label={<span aria-hidden="true">▾</span>}
      onOpenChange={() => undefined}
      open={false}
      title="Filter Status"
    >
      {() => <span>Nothing here.</span>}
    </PanelTrigger>
  );
  try {
    expect(screen.getByRole("button", { name: "Filter Status" })).toBeTruthy();
    view.rerender(
      <PanelTrigger
        label={<span aria-hidden="true">▾</span>}
        onOpenChange={() => undefined}
        open
        title="Filter Status"
      >
        {() => <span>Nothing here.</span>}
      </PanelTrigger>
    );
    await user.click(screen.getByRole("button", { name: "Filter Status" }));
    expect(screen.getByRole("dialog", { name: "Filter Status" })).toBeTruthy();
  } finally {
    view.unmount();
  }
});

test("a trigger given a control of its own carries that name, and the panel stays this one", () => {
  const view = render(
    <PanelTrigger
      control="column-filter"
      label="Status"
      onOpenChange={() => undefined}
      open={false}
    >
      {() => <span>Nothing here.</span>}
    </PanelTrigger>
  );
  try {
    // A column filter, a columns menu and a display menu are each this button under a name of
    // their own, so a row finds the one it means without a second definition of the button.
    expect(screen.getByRole("button", { name: "Status" })).toHaveAttribute(
      "data-control",
      "column-filter"
    );
    expect(document.querySelector('[data-control="panel-trigger"]')).toBeNull();
  } finally {
    view.unmount();
  }
});

/** A panel of controls that each take themselves out of it when pressed, as the search bar's
 *  Author box goes once an author is taken. */
function ShrinkingPanel({ names }: { names: readonly string[] }) {
  const [open, setOpen] = useState(false);
  const [left, setLeft] = useState(names);
  return (
    <PanelTrigger label="Filter" onOpenChange={setOpen} open={open}>
      {() =>
        left.map((name) => (
          <button
            key={name}
            onClick={() => setLeft((all) => all.filter((other) => other !== name))}
            type="button"
          >
            {name}
          </button>
        ))
      }
    </PanelTrigger>
  );
}

test("a control that takes itself out of an open panel hands the focus to the next control in it, then the one before it, then the panel itself", async () => {
  const user = userEvent.setup();
  const view = render(<ShrinkingPanel names={["Project", "Status", "Author"]} />);
  try {
    await user.click(screen.getByRole("button", { name: "Filter" }));
    const panel = screen.getByRole("dialog", { name: "Filter" });
    act(() => within(panel).getByRole("button", { name: "Project" }).focus());
    await user.keyboard("{Enter}");
    expect(within(panel).queryByRole("button", { name: "Project" })).toBeNull();
    // Not the page's body, from which a keyboard reader would start again at its first link.
    expect(within(panel).getByRole("button", { name: "Status" })).toHaveFocus();
    // The panel's last control has no next one: the one before it.
    act(() => within(panel).getByRole("button", { name: "Author" }).focus());
    await user.keyboard("{Enter}");
    expect(within(panel).getByRole("button", { name: "Status" })).toHaveFocus();
    // Its only control: the panel itself, which is still open and still answers Escape.
    await user.keyboard("{Enter}");
    expect(panel).toHaveFocus();
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Filter" })).toHaveFocus();
  } finally {
    view.unmount();
  }
});

/** A panel whose last control's press takes it and the control before it away in one commit,
 *  as a block of a panel's controls goes together. */
function BlockPanel() {
  const [open, setOpen] = useState(false);
  const [left, setLeft] = useState(["Project", "Status", "Author", "Owner"]);
  return (
    <PanelTrigger label="Filter" onOpenChange={setOpen} open={open}>
      {() =>
        left.map((name) => (
          <button
            key={name}
            onClick={() =>
              setLeft((all) =>
                name === "Owner" ? all.slice(0, -2) : all.filter((o) => o !== name)
              )
            }
            type="button"
          >
            {name}
          </button>
        ))
      }
    </PanelTrigger>
  );
}

// The tree's architect's ruling of 2026-11-15 (the nearest earlier control wins) holds in the
// panel's own hand-off: a block of two taken away at the panel's end lands the focus on the
// control before the block, not on the panel itself.
test("a block of controls taken out of an open panel hands the focus to the control before the block", async () => {
  const user = userEvent.setup();
  const view = render(<BlockPanel />);
  try {
    await user.click(screen.getByRole("button", { name: "Filter" }));
    const panel = screen.getByRole("dialog", { name: "Filter" });
    act(() => within(panel).getByRole("button", { name: "Owner" }).focus());
    await user.keyboard("{Enter}");
    expect(within(panel).queryByRole("button", { name: "Author" })).toBeNull();
    expect(within(panel).getByRole("button", { name: "Status" })).toHaveFocus();
  } finally {
    view.unmount();
  }
});

/** A panel holding a menu of its own, as the Delivery page's facets each are, ahead of one
 *  more control: picking the menu's one value takes the whole menu away, its trigger and the
 *  list holding the focus with it. */
function PanelWithMenu() {
  const [open, setOpen] = useState(false);
  const [menuOpen, setMenuOpen] = useState(false);
  const [picked, setPicked] = useState(false);
  return (
    <PanelTrigger label="Filter" onOpenChange={setOpen} open={open}>
      {() => (
        <>
          {picked ? null : (
            <PanelTrigger label="Project" onOpenChange={setMenuOpen} open={menuOpen}>
              {() => (
                <button onClick={() => setPicked(true)} type="button">
                  CORE
                </button>
              )}
            </PanelTrigger>
          )}
          <button type="button">Author</button>
        </>
      )}
    </PanelTrigger>
  );
}

test("a menu inside the panel that goes with the focus in its own list hands the focus to the control now at its trigger's place", async () => {
  const user = userEvent.setup();
  const view = render(<PanelWithMenu />);
  try {
    await user.click(screen.getByRole("button", { name: "Filter" }));
    const panel = screen.getByRole("dialog", { name: "Filter" });
    act(() => within(panel).getByRole("button", { name: "Project" }).focus());
    await user.keyboard("{Enter}");
    // The menu's list is a panel of its own, portalled outside this one, and holds the focus.
    const menu = screen.getByRole("dialog", { name: "Project" });
    expect(within(menu).getByRole("button", { name: "CORE" })).toHaveFocus();
    await user.keyboard("{Enter}");
    expect(screen.queryByRole("dialog", { name: "Project" })).not.toBeInTheDocument();
    expect(within(panel).getByRole("button", { name: "Author" })).toHaveFocus();
  } finally {
    view.unmount();
  }
});

/** The Inbox's control row under `?project=PANE,ZZZ`: a chip for each project the filter picks,
 *  and the Filter panel, holding one control and then the Project list (`MultiSelect`), whose list
 *  is portalled out of the panel. No row carries ZZZ, so the list draws ZZZ's row only while ZZZ
 *  is picked, as the Inbox lists a value the address names (`fieldOptions`). Each pick, from the
 *  list or a chip, waits in `answers` until the case applies it in a commit of its own, as the
 *  Inbox applies a pick through the address after the press's own commit, and holds the press
 *  until then (`holdHandOff`), as the address answers the press in a browser before it ends. */
function ProjectFilterRow({ answers }: { readonly answers: (() => void)[] }) {
  const [open, setOpen] = useState(false);
  const [listOpen, setListOpen] = useState(false);
  const [picked, setPicked] = useState<readonly string[]>(["PANE", "ZZZ"]);
  const pick = (next: readonly string[]) => {
    const answered = holdHandOff();
    answers.push(() => {
      setPicked(next);
      answered();
    });
  };
  return (
    <ControlRow
      chips={filterChips(
        picked.map((project) => ({
          label: `Project: ${project}`,
          remove: () => pick(picked.filter((other) => other !== project)),
        }))
      )}
      trigger={
        <PanelTrigger label="Filter" onOpenChange={setOpen} open={open} stacked>
          {() => (
            <>
              <button type="button">Blocked on you</button>
              <MultiSelect
                emptyMessage="No matching project."
                label="Project"
                onChange={pick}
                onOpenChange={setListOpen}
                open={listOpen}
                options={["PANE", ...picked.filter((project) => project !== "PANE")]}
                searchLabel="Search project"
                selected={picked}
                triggerAriaLabel="Filter by project"
              />
            </>
          )}
        </PanelTrigger>
      }
    />
  );
}

/** Opens `ProjectFilterRow`'s Filter panel and its Project list, with the pointer. */
async function openProjectList(user: UserEvent): Promise<void> {
  await user.click(screen.getByRole("button", { name: "Filter" }));
  const panel = screen.getByRole("dialog", { name: "Filter" });
  await user.click(within(panel).getByRole("button", { name: "Filter by project" }));
}

/** Records the name of each element the focus moves to from now on (`focusin`), its
 *  `aria-label` else its text, and answers them when read, which ends the record. */
function recordFocusMoves(): () => string[] {
  const moves: string[] = [];
  const note = (event: FocusEvent) => {
    const { target } = event;
    if (target instanceof Element) {
      moves.push(target.getAttribute("aria-label") ?? target.textContent ?? "");
    }
  };
  document.addEventListener("focusin", note);
  return () => {
    document.removeEventListener("focusin", note);
    return moves;
  };
}

// The focus on ZZZ's row of the open Project list, as a screen reader's cursor puts it there, and
// Enter or Space unpicks ZZZ: the press's own commit leaves the row, and its focus, where they
// were, and the address's answer takes the row away while the list stays open. The list hands
// that focus to its own search box in the commit of that answer (`MultiSelect`), and the panel,
// which the list stands outside of, leaves the loss to the list (`useFocusPlace`): the focus
// lands on neither the list's trigger, at the list's place in the panel, nor the page's body.
test("a focused option its unpick takes out of a list the panel portals out hands the focus to the list's search box in the commit that takes it away", async () => {
  const user = userEvent.setup();
  const landed: Record<string, readonly string[]> = {};
  for (const key of ["{Enter}", " "]) {
    const answers: (() => void)[] = [];
    const view = render(<ProjectFilterRow answers={answers} />);
    try {
      await openProjectList(user);
      const search = screen.getByRole("combobox", { name: "Search project" });
      const zzz = screen.getByRole("option", { name: "ZZZ" });
      act(() => zzz.focus());
      await user.keyboard(key);
      expect(answers.length, "the unpick of ZZZ was not handed to the host").toBe(1);
      expect(zzz, "the press's own commit moved the focus off the row it kept").toHaveFocus();
      const moves = recordFocusMoves();
      act(() => answers.shift()?.());
      landed[key] = moves();
      expect(zzz.isConnected, "the answer did not take ZZZ's row away").toBe(false);
      expect(screen.getByRole("listbox", { name: "Project options" })).toBeInTheDocument();
      expect(search, "the focus did not end in the list's search box").toHaveFocus();
    } finally {
      view.unmount();
      disarm();
    }
  }
  expect(landed).toEqual({ " ": ["Search project"], "{Enter}": ["Search project"] });
});

// The list's Clear foot is the other way a reader takes ZZZ off from inside the list: a keyboard
// press of it hands the focus to the search box at the press itself (`MultiSelect`), in a panel as
// outside one, and the address's answer, which empties the pick and takes ZZZ's row away, leaves
// the focus there. Clear stays, held and saying that nothing is picked.
test("a keyboard press of the Clear foot of a list the panel portals out hands the focus to the list's search box, where the answer leaves it", async () => {
  const user = userEvent.setup();
  const answers: (() => void)[] = [];
  const view = render(<ProjectFilterRow answers={answers} />);
  try {
    await openProjectList(user);
    const search = screen.getByRole("combobox", { name: "Search project" });
    const clear = screen.getByRole("button", { name: "Clear project" });
    act(() => clear.focus());
    const moves = recordFocusMoves();
    await user.keyboard("{Enter}");
    expect(search, "the press of Clear did not hand the focus to the search box").toHaveFocus();
    act(() => answers.shift()?.());
    expect(screen.queryByRole("option", { name: "ZZZ" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "No project picked" })).toHaveAttribute(
      "aria-disabled",
      "true"
    );
    expect(moves()).toEqual(["Search project"]);
    expect(search).toHaveFocus();
  } finally {
    view.unmount();
    disarm();
  }
});

// ZZZ's chip in the row is the third way: its removal is the row's to hand on (`ControlRow`), and
// the address's answer that takes the chip away hands the focus to the chip before it, PANE's, as
// the row's last chip does. So it does where the reader's pointer takes the chip off while the
// panel's Project list holds the focus on ZZZ's row, which that press closes with the panel.
test("a chip taken off the row hands the focus to the chip before it, with the panel shut or its list holding the focus", async () => {
  const user = userEvent.setup();
  const landed: Record<string, readonly string[]> = {};
  for (const way of ["the panel shut", "the panel's list on ZZZ's row"] as const) {
    const answers: (() => void)[] = [];
    const view = render(<ProjectFilterRow answers={answers} />);
    try {
      const chip = screen.getByRole("button", { name: "Remove Project: ZZZ filter" });
      if (way === "the panel shut") {
        act(() => chip.focus());
        await user.keyboard("{Enter}");
      } else {
        await openProjectList(user);
        const zzz = screen.getByRole("option", { name: "ZZZ" });
        act(() => zzz.focus());
        await user.click(chip);
        expect(screen.queryByRole("dialog", { name: "Filter" })).not.toBeInTheDocument();
      }
      expect(answers.length, "the chip's removal was not handed to the host").toBe(1);
      const moves = recordFocusMoves();
      act(() => answers.shift()?.());
      landed[way] = moves();
      expect(chip.isConnected, "the answer did not take ZZZ's chip away").toBe(false);
      expect(screen.getByRole("button", { name: "Remove Project: PANE filter" })).toHaveFocus();
    } finally {
      view.unmount();
      disarm();
    }
  }
  expect(landed).toEqual({
    "the panel shut": ["Remove Project: PANE filter"],
    "the panel's list on ZZZ's row": ["Remove Project: PANE filter"],
  });
});

/** A panel opened from inside a modal dialog, as the search bar's Filter panel and the Inbox
 *  drawer's View menu are: the panel is portalled to the body, outside the dialog, and a control
 *  of the page stands before the dialog. */
function PanelInDialog({ children }: { children: ReactNode }) {
  const [open, setOpen] = useState(false);
  return (
    <>
      <button type="button">Behind the dialog</button>
      <div aria-label="Search" aria-modal="true" role="dialog">
        <PanelTrigger label="Filter" onOpenChange={setOpen} open={open}>
          {() => children}
        </PanelTrigger>
        <button type="button">After the trigger</button>
      </div>
    </>
  );
}

test("Tab past the panel's last control closes it and puts the reader on its trigger, whose next Tab goes on in the dialog", async () => {
  const user = userEvent.setup();
  const view = render(
    <PanelInDialog>
      <button type="button">Project</button>
      <button type="button">Updated since</button>
    </PanelInDialog>
  );
  try {
    const trigger = screen.getByRole("button", { name: "Filter" });
    await user.click(trigger);
    const panel = screen.getByRole("dialog", { name: "Filter" });
    expect(within(panel).getByRole("button", { name: "Project" })).toHaveFocus();
    // Between the panel's own controls Tab moves as ever.
    await user.tab();
    expect(within(panel).getByRole("button", { name: "Updated since" })).toHaveFocus();
    // Past the last, never to whatever the body holds after the panel or the page behind.
    await user.tab();
    expect(screen.queryByRole("dialog", { name: "Filter" })).not.toBeInTheDocument();
    expect(trigger).toHaveAttribute("aria-expanded", "false");
    expect(trigger).toHaveFocus();
    await user.tab();
    expect(screen.getByRole("button", { name: "After the trigger" })).toHaveFocus();
  } finally {
    view.unmount();
  }
});

test("Shift+Tab past the panel's first control closes it and puts the reader on its trigger", async () => {
  const user = userEvent.setup();
  const view = render(
    <PanelInDialog>
      <button type="button">Project</button>
      <button type="button">Updated since</button>
    </PanelInDialog>
  );
  try {
    const trigger = screen.getByRole("button", { name: "Filter" });
    await user.click(trigger);
    const panel = screen.getByRole("dialog", { name: "Filter" });
    expect(within(panel).getByRole("button", { name: "Project" })).toHaveFocus();
    await user.tab({ shift: true });
    expect(screen.queryByRole("dialog", { name: "Filter" })).not.toBeInTheDocument();
    expect(trigger).toHaveFocus();
  } finally {
    view.unmount();
  }
});

test("a group of radios is one Tab stop: Tab from the chosen radio of the panel's last group closes it", async () => {
  const user = userEvent.setup();
  const view = render(
    <PanelInDialog>
      <fieldset>
        <legend>Thread order</legend>
        <label>
          <input defaultChecked name="order" type="radio" value="oldest" />
          Oldest first
        </label>
        <label>
          <input name="order" type="radio" value="newest" />
          Newest first
        </label>
      </fieldset>
    </PanelInDialog>
  );
  try {
    const trigger = screen.getByRole("button", { name: "Filter" });
    await user.click(trigger);
    const oldest = screen.getByRole("radio", { name: "Oldest first" });
    act(() => oldest.focus());
    await user.tab();
    expect(screen.queryByRole("dialog", { name: "Filter" })).not.toBeInTheDocument();
    expect(trigger).toHaveFocus();
  } finally {
    view.unmount();
  }
});

// INFRA-3135's review round 1, R10: opening a panel puts the focus on its first control by the
// rule its Tab stops and every dialog's trap follow (`focusableIn`): a disabled button takes no
// focus, so the focus goes on to the first control that does - a link included - and a panel
// with no control takes the focus itself, so the reader is in the panel they opened either way.
test("opening a panel puts the focus on its first control that takes the focus, else on the panel", async () => {
  const user = userEvent.setup();
  const view = render(
    <PanelInDialog>
      <button disabled type="button">
        Saved views
      </button>
      <a href="/help">How filters work</a>
    </PanelInDialog>
  );
  try {
    await user.click(screen.getByRole("button", { name: "Filter" }));
    expect(screen.getByRole("link", { name: "How filters work" })).toHaveFocus();
  } finally {
    view.unmount();
  }
  const prose = render(
    <PanelInDialog>
      <p>No filter applies to this list.</p>
    </PanelInDialog>
  );
  try {
    await user.click(screen.getByRole("button", { name: "Filter" }));
    expect(screen.getByRole("dialog", { name: "Filter" })).toHaveFocus();
  } finally {
    prose.unmount();
  }
});

/** Lays out what happy-dom does not: `trigger` drawn 28 px tall from `top`, and every panel
 *  holding `panelHeight` px of content, which is what the placement reads of it (`scrollHeight`,
 *  with no border). Answers what puts happy-dom's own layout back. */
function layOut(trigger: HTMLElement, top: number, panelHeight: number): () => void {
  const own = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "scrollHeight");
  Object.defineProperty(HTMLElement.prototype, "scrollHeight", {
    configurable: true,
    get(this: HTMLElement) {
      return this.getAttribute("role") === "dialog" ? panelHeight : 0;
    },
  });
  trigger.getBoundingClientRect = () =>
    ({
      bottom: top + 28,
      height: 28,
      left: 10,
      right: 100,
      top,
      width: 90,
      x: 10,
      y: top,
    }) as DOMRect;
  return () => {
    if (own === undefined) Reflect.deleteProperty(HTMLElement.prototype, "scrollHeight");
    else Object.defineProperty(HTMLElement.prototype, "scrollHeight", own);
  };
}

// INFRA-3135, review round 1: a list low in the Delivery page's Filter panel was pushed up over
// its own trigger to fit the window, so a press on the trigger landed on the list's search box
// and the list could not be closed by the control that opened it.
test("a panel with no room under its trigger opens over it, never on it", async () => {
  const user = userEvent.setup();
  const view = render(<Host />);
  const trigger = screen.getByRole("button", { name: "Columns" });
  const triggerTop = window.innerHeight - 68;
  const restore = layOut(trigger, triggerTop, 200);
  try {
    await user.click(trigger);
    const panel = screen.getByRole("dialog", { name: "Columns" });
    expect(panel.style.top).toBe(`${triggerTop - 4 - 200}px`);
    expect(panel.style.maxHeight).toBe("");
  } finally {
    restore();
    view.unmount();
  }
});

test("a panel with room on neither side of its trigger is cut to the roomier side, never laid on it", async () => {
  const user = userEvent.setup();
  const view = render(<Host />);
  const trigger = screen.getByRole("button", { name: "Columns" });
  const triggerTop = window.innerHeight - 200;
  const restore = layOut(trigger, triggerTop, window.innerHeight * 2);
  try {
    await user.click(trigger);
    const panel = screen.getByRole("dialog", { name: "Columns" });
    // Over the trigger, from the window's 8 px margin to 4 px above the trigger.
    expect(panel.style.top).toBe("8px");
    expect(panel.style.maxHeight).toBe(`${triggerTop - 4 - 8}px`);
  } finally {
    restore();
    view.unmount();
  }
});
