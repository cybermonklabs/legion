import { expect, spyOn, test } from "bun:test";
// fireEvent also gives one row's press below a click with no pointer press before it, and a turn
// of the wheel over a list below, which user-event has no form for either.
// biome-ignore lint/style/noRestrictedImports: the outside-press sites below are a pointerdown with no release, and the keys that end or move an IME composition are keys marked as the input method's; user-event has no form for either.
import { act, fireEvent, render, screen, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { useState } from "react";
import { flushSync } from "react-dom";

import {
  highlightedRow,
  onCoarsePointer,
  panelOf,
  pickNamed,
  pressesTriggerOn,
} from "../__tests__/list-picks";
import { installResizeObserver } from "../__tests__/scroll-box";
import { NoTooltips } from "../features/issue-page/NoTooltips";
import { KeymapProvider } from "../features/shell/KeymapProvider";
import { MENU_TRIGGER_SELECTOR } from "./key-targets";
import { type ListMarks, type ListMenu, MultiSelect } from "./MultiSelect";
import { PanelTrigger } from "./PanelTrigger";

function Host({ onCreate, options }: { onCreate?: (value: string) => void; options: string[] }) {
  const [open, setOpen] = useState(false);
  const [selected, setSelected] = useState<string[]>([]);
  return (
    <>
      <MultiSelect
        emptyMessage="Nothing here."
        label="Labels"
        onChange={setSelected}
        onCreate={onCreate}
        onOpenChange={setOpen}
        open={open}
        options={options}
        searchLabel="Search labels"
        selected={selected}
      />
      <output data-testid="selected">{selected.join(",")}</output>
    </>
  );
}

/** A single pick as a host draws one: the value it holds, every value the list handed it, what
 *  closed it each time, and a control outside the list to press, whose own action closes it. */
function SingleHost({
  ariaDisabled,
  disabled,
  focusOnOpen,
  initial,
  keysOnTrigger,
  marks,
  onCreate,
  onQueryChange,
  onTriggerFocus,
  optionLabel,
  options,
}: {
  ariaDisabled?: boolean;
  disabled?: boolean;
  focusOnOpen?: boolean;
  initial?: string;
  keysOnTrigger?: boolean;
  marks?: ListMarks;
  onCreate?: (value: string) => boolean;
  onQueryChange?: (query: string) => void;
  onTriggerFocus?: (from: Element | null) => void;
  optionLabel?: (value: string) => string | undefined;
  options: string[];
}) {
  const [open, setOpen] = useState(false);
  const [value, setValue] = useState(initial);
  const [handed, setHanded] = useState<string[]>([]);
  const [closings, setClosings] = useState<string[]>([]);
  return (
    <>
      <MultiSelect
        ariaDisabled={ariaDisabled}
        createLabel={(typed) => `Use "${typed}"`}
        disabled={disabled}
        emptyMessage="Nothing here."
        focusOnOpen={focusOnOpen}
        keysOnTrigger={keysOnTrigger}
        label="Epic"
        marks={marks}
        mode="single"
        onCreate={onCreate}
        onOpenChange={(next, closedBy) => {
          setOpen(next);
          if (!next) setClosings((before) => [...before, closedBy ?? "nothing"]);
        }}
        onPick={(picked) => {
          setValue(picked);
          setHanded((before) => [...before, picked]);
        }}
        onQueryChange={onQueryChange}
        onTriggerFocus={onTriggerFocus}
        open={open}
        optionLabel={optionLabel}
        options={options}
        searchLabel="Search epics"
        value={value}
      />
      <output data-testid="handed">{handed.join(",")}</output>
      <output data-testid="closings">{closings.join(",")}</output>
      <button onClick={() => setOpen(false)} type="button">
        Elsewhere
      </button>
    </>
  );
}

test("clicking rows toggles a multi-selection, the trigger counts it and Clear empties it", async () => {
  const user = userEvent.setup();
  const view = render(<Host options={["api", "docs", "frontend"]} />);
  try {
    const trigger = screen.getByRole("button", { name: "Labels" });
    expect(trigger).toHaveAttribute("aria-expanded", "false");
    await user.click(trigger);
    const listbox = screen.getByRole("listbox", { name: "Labels options" });
    expect(listbox).toHaveAttribute("aria-multiselectable", "true");
    expect(screen.getByRole("combobox", { name: "Search labels" })).toHaveFocus();

    await user.click(screen.getByRole("option", { name: "docs" }));
    await user.click(screen.getByRole("option", { name: "api" }));
    expect(screen.getByTestId("selected").textContent).toBe("docs,api");
    expect(screen.getByRole("option", { name: "docs" })).toHaveAttribute("aria-selected", "true");
    expect(screen.getByRole("option", { name: "frontend" })).toHaveAttribute(
      "aria-selected",
      "false"
    );
    expect(screen.getByRole("button", { name: "Labels · 2" })).toHaveAttribute(
      "aria-expanded",
      "true"
    );

    await user.click(screen.getByRole("option", { name: "docs" }));
    expect(screen.getByTestId("selected").textContent).toBe("api");

    await user.click(screen.getByRole("button", { name: "Clear labels" }));
    expect(screen.getByTestId("selected").textContent).toBe("");
    expect(screen.queryByRole("button", { name: "Clear labels" })).not.toBeInTheDocument();
  } finally {
    view.unmount();
  }
});

test("the search narrows the rows and arrows, Enter and Escape drive them from the input", async () => {
  const user = userEvent.setup();
  const view = render(<Host options={["api", "docs", "frontend"]} />);
  try {
    const trigger = screen.getByRole("button", { name: "Labels" });
    await user.click(trigger);
    const search = screen.getByRole("combobox", { name: "Search labels" });

    await user.clear(search);
    await user.click(search);
    await user.paste("DOC");
    expect(screen.getAllByRole("option").map((option) => option.textContent)).toEqual(["docs"]);
    await user.clear(search);

    // The first row is active until an arrow moves it; Enter toggles the active row.
    expect(search).toHaveAttribute(
      "aria-activedescendant",
      screen.getByRole("option", { name: "api" }).id
    );
    await user.keyboard("{ArrowDown}");
    await user.keyboard("{ArrowDown}");
    expect(search).toHaveAttribute(
      "aria-activedescendant",
      screen.getByRole("option", { name: "frontend" }).id
    );
    await user.keyboard("{Enter}");
    expect(screen.getByTestId("selected").textContent).toBe("frontend");
    // ArrowUp wraps from the first row to the last.
    await user.keyboard("{ArrowUp}");
    await user.keyboard("{ArrowUp}");
    await user.keyboard("{ArrowUp}");
    expect(search).toHaveAttribute(
      "aria-activedescendant",
      screen.getByRole("option", { name: "frontend" }).id
    );

    await user.clear(search);
    await user.click(search);
    await user.paste("zzz");
    expect(screen.getByText("Nothing here.")).toBeTruthy();
    await user.keyboard("{Enter}");
    expect(screen.getByTestId("selected").textContent).toBe("frontend");

    await user.keyboard("{Escape}");
    expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
    expect(trigger).toHaveAttribute("aria-expanded", "false");
    expect(trigger).toHaveFocus();
  } finally {
    view.unmount();
  }
});

// A key held down acts once (R8, the class of the tester's defect 3), by the page's rule, which
// `KeymapProvider` binds (`Keymap.captureKeyDown`): a held Enter in a multi-select toggles its
// option once, where each repeat would toggle it again, and the list ends with the option picked or
// not by the press alone.
test("a held Enter toggles the active option once", async () => {
  const user = userEvent.setup();
  const view = render(
    <KeymapProvider>
      <Host options={["api", "docs", "frontend"]} />
    </KeymapProvider>
  );
  try {
    await user.click(screen.getByRole("button", { name: "Labels" }));
    await user.keyboard("{Enter>4}");
    expect(screen.getByTestId("selected").textContent, "a held Enter picks api once").toBe("api");
    await user.keyboard("{Enter>5}");
    expect(screen.getByTestId("selected").textContent, "a held Enter takes api back once").toBe("");
  } finally {
    view.unmount();
  }
});

/**
 * Lays the open list out as a browser would, which happy-dom does not: its panel, the box that
 * scrolls, is 288 px tall at 100 px down the window inside a 1 px border, so it shows 286 px of
 * what it holds. The text box stands stuck to its top, 61 px tall, and so does the Clear foot or a
 * menu's line of keys, 61 px tall too, to its bottom while it is drawn - but for a menu's line on
 * a coarse pointer, which follows the options (`ListMenu`); each option is 44 px tall, or as tall
 * as `height` makes it as it is measured, under the text box at its place among the options the
 * list draws when it is measured, moved up by the panel's `scrollTop`, which stops at both ends of
 * the scroll, as a browser's does. A case that cuts the panel's box shorter sets its
 * `clientHeight`, which the foot that sticks and the end of the scroll follow. `trigger` stands
 * above the panel, so the panel does not close as one hanging off nothing (`AnchoredPanel`).
 * Answers the panel.
 */
function layOutList(
  trigger: HTMLElement,
  height: (option: Element) => number = () => 44
): HTMLElement {
  const listbox = screen.getByRole("listbox");
  const box = listbox.parentElement;
  const head = screen.getByRole("combobox").parentElement;
  if (box === null || head === null) throw new Error("the list is drawn outside its panel");
  const foot =
    screen.queryByRole("button", { name: /^Clear / })?.parentElement ??
    within(box).queryByText(/to choose · Esc to close$/);
  const sticks = foot?.classList.contains("sticky") === true;
  const drawn = () => [...listbox.querySelectorAll('[role="option"]')];
  const tall = (options: readonly Element[]) =>
    options.reduce((sum, option) => sum + height(option), 0);
  let scrolled = 0;
  Object.defineProperties(box, {
    clientHeight: { configurable: true, get: () => 286 },
    clientTop: { configurable: true, get: () => 1 },
    scrollTop: {
      configurable: true,
      get: () => scrolled,
      set: (value: number) => {
        const end = 61 + tall(drawn()) + (foot === null ? 0 : 61) - box.clientHeight;
        scrolled = Math.min(Math.max(value, 0), Math.max(end, 0));
      },
    },
  });
  trigger.getBoundingClientRect = () => new DOMRect(0, 52, 120, 44);
  box.getBoundingClientRect = () => new DOMRect(0, 100, 256, 288);
  head.getBoundingClientRect = () => new DOMRect(1, 101, 254, 61);
  if (foot !== null) {
    foot.getBoundingClientRect = () =>
      new DOMRect(
        1,
        sticks ? 101 + box.clientHeight - 61 : 162 + tall(drawn()) - scrolled,
        254,
        61
      );
  }
  for (const option of drawn()) {
    option.getBoundingClientRect = () => {
      const options = drawn();
      const above = tall(options.slice(0, options.indexOf(option)));
      return new DOMRect(1, 162 + above - scrolled, 254, height(option));
    };
  }
  return box;
}

/** The words of the option the list marks active: the one its text box names. */
function activeName(search: HTMLElement): string {
  const option = document.getElementById(search.getAttribute("aria-activedescendant") ?? "");
  return option?.textContent?.replace("✓", "") ?? "no option";
}

/** The issue header's Status list's eight statuses, in its order. */
const STATUSES = [
  "Triage",
  "Backlog",
  "Todo",
  "In progress",
  "Blocked",
  "Testing",
  "Needs review",
  "Retro",
];

// The issue header's Status list in a browser: eight statuses under the text box of a box that
// shows five, where the arrows marked Testing, Needs review and Retro active out of sight.
test("ArrowDown and ArrowUp scroll the list the least that shows the option they mark active whole below its text box, and scroll nothing else", async () => {
  const user = userEvent.setup();
  const view = render(<SingleHost initial="Triage" options={STATUSES} />);
  const scrolls = spyOn(Element.prototype, "scrollIntoView");
  try {
    const trigger = screen.getByRole("button", { name: pickNamed("Epic") });
    await user.click(trigger);
    const box = layOutList(trigger);
    const search = screen.getByRole("combobox", { name: "Search epics" });
    expect(activeName(search)).toBe("Triage");
    // The option each key marks active, and how far the list has scrolled then: the box shows
    // 61-286 px of its content, and the sixth option, Testing, stands at 281-325.
    const walked: [string, number][] = [];
    const down = Array<string>(7).fill("ArrowDown");
    for (const key of [...down, "ArrowUp", "ArrowDown", "ArrowDown", "ArrowUp"]) {
      await user.keyboard(`{${key}}`);
      walked.push([activeName(search), box.scrollTop]);
    }
    expect(walked).toEqual([
      ["Backlog", 0],
      ["Todo", 0],
      ["In progress", 0],
      ["Blocked", 0],
      ["Testing", 39],
      ["Needs review", 83],
      ["Retro", 127],
      // Already whole, so nothing scrolls.
      ["Needs review", 127],
      ["Retro", 127],
      // ArrowDown wraps to the first option, back under the text box, and ArrowUp to the last.
      ["Triage", 0],
      ["Retro", 127],
    ]);
    expect(scrolls).not.toHaveBeenCalled();
  } finally {
    scrolls.mockRestore();
    view.unmount();
  }
});

test("the arrows scroll a multi-select's list the least that shows the option they mark active whole above its Clear foot", async () => {
  const user = userEvent.setup();
  const labels = ["api", "docs", "frontend", "infra", "mobile", "ops", "qa", "web"];
  const view = render(<Host options={labels} />);
  const scrolls = spyOn(Element.prototype, "scrollIntoView");
  try {
    const trigger = screen.getByRole("button", { name: "Labels" });
    await user.click(trigger);
    // A pick draws the Clear foot, which covers the box's last 61 px.
    await user.click(screen.getByRole("option", { name: "api" }));
    const box = layOutList(trigger);
    const search = screen.getByRole("combobox", { name: "Search labels" });
    expect(search).toHaveFocus();
    const walked: [string, number][] = [];
    for (const key of ["ArrowDown", "ArrowDown", "ArrowDown", "ArrowUp", "ArrowUp", "ArrowUp"]) {
      await user.keyboard(`{${key}}`);
      walked.push([activeName(search), box.scrollTop]);
    }
    // The box shows 61-225 px of its content between the text box and the foot, and the fourth
    // option, infra, stands at 193-237.
    expect(walked).toEqual([
      ["docs", 0],
      ["frontend", 0],
      ["infra", 12],
      ["frontend", 12],
      ["docs", 12],
      ["api", 0],
    ]);
    expect(scrolls).not.toHaveBeenCalled();
  } finally {
    scrolls.mockRestore();
    view.unmount();
  }
});

/**
 * Lays each list an open draws out as `layOutList` does, its options as tall as `height` makes
 * them, the moment anything first measures it: the open measures its list in the commit that
 * draws it, before a case could lay it out after the open. Answers the box the last list scrolls
 * in, and what puts the measurements back.
 */
function layOutListsAsOpened(
  trigger: HTMLElement,
  height?: (option: Element) => number
): {
  box: () => HTMLElement | null;
  restore: () => void;
} {
  let listbox: Element | null = null;
  let box: HTMLElement | null = null;
  const measure = Element.prototype.getBoundingClientRect;
  const spy = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ) {
    const drawn = document.querySelector('[role="listbox"]');
    if (drawn !== null && drawn !== listbox) {
      listbox = drawn;
      box = layOutList(trigger, height);
      // The element measured may be one the layout has just given a box of its own.
      if (Object.hasOwn(this, "getBoundingClientRect")) return this.getBoundingClientRect();
    }
    return measure.call(this);
  });
  return { box: () => box, restore: () => spy.mockRestore() };
}

// The same Status list on an issue in Testing, Needs review or Retro opened with that status
// active below the five rows its box shows, and Enter took it unseen. However the list opens - a
// click, Enter or ArrowDown on its trigger - it opens with the option its value picks shown whole
// below its text box, the list's own box alone scrolled, in the commit that draws it. A list that
// closed on a search opens on every option again, and shows the picked one whole among them, not
// at the place it held among the options that search left.
test("a single pick opens with its picked option shown whole below its text box, however it opens, and scrolls nothing else", async () => {
  const user = userEvent.setup();
  const view = render(<SingleHost initial="Testing" options={STATUSES} />);
  const trigger = screen.getByRole("button", { name: pickNamed("Epic") });
  const lists = layOutListsAsOpened(trigger);
  const scrolls = spyOn(Element.prototype, "scrollIntoView");
  try {
    // The option each open marks active, and how far the list has scrolled then: the box shows
    // 61-286 px of its content, and the sixth option, Testing, stands at 281-325.
    const opened: [string, string, number | undefined][] = [];
    const shown = (way: string) =>
      opened.push([
        way,
        activeName(screen.getByRole("combobox", { name: "Search epics" })),
        lists.box()?.scrollTop,
      ]);
    const opens: Readonly<Record<string, () => Promise<void>>> = {
      "a click": () => user.click(trigger),
      ArrowDown: () => user.keyboard("{ArrowDown}"),
      Enter: () => user.keyboard("{Enter}"),
    };
    for (const [way, open] of Object.entries(opens)) {
      await open();
      shown(way);
      // Escape closes the list and puts the focus back on its trigger, for the next open.
      await user.keyboard("{Escape}");
    }
    // A search leaves Testing alone in the list, and Enter picks it, which closes the list.
    await user.click(trigger);
    await user.keyboard("test{Enter}");
    await user.click(trigger);
    shown("a click after a search");
    expect(opened).toEqual([
      ["a click", "Testing", 39],
      ["ArrowDown", "Testing", 39],
      ["Enter", "Testing", 39],
      ["a click after a search", "Testing", 39],
    ]);
    expect(scrolls).not.toHaveBeenCalled();
  } finally {
    scrolls.mockRestore();
    lists.restore();
    view.unmount();
  }
});

// The issue's Assignee list reads its people only once it is opened: it opened on Unassigned and
// the assignee, and when the people came the assignee moved to its place among everyone, still
// the active option, below the five rows its box shows, where Enter took it unseen. Each change of
// the options shows the option the list opened on whole again, the list's own box alone
// scrolled, and so does a change of the box's own size: where the window has too little room, the
// panel cuts its box in a commit after the one that showed the option. That lasts until the reader
// moves on in the list: after an arrow, a word typed in the text box or a scroll of their own, a
// change of the options or of the box leaves the list where it is.
test("a single pick keeps the option it opened on shown whole as its options and its box change, until the reader moves on in the list", async () => {
  const user = userEvent.setup();
  const sizes = installResizeObserver();
  const before = ["Triage", "Needs review"];
  const host = (options: string[]) => <SingleHost initial="Needs review" options={options} />;
  const view = render(host(before));
  const trigger = screen.getByRole("button", { name: pickNamed("Epic") });
  const lists = layOutListsAsOpened(trigger);
  const scrolls = spyOn(Element.prototype, "scrollIntoView");
  try {
    // The option the list marks active at each moment, and how far it has scrolled then: the box
    // shows 61-286 px of its content, Needs review stands at 325-369 among the eight statuses,
    // and at 369-413 with one more option before it.
    const seen: [string, string, number | undefined][] = [];
    const shown = (moment: string) =>
      seen.push([
        moment,
        activeName(screen.getByRole("combobox", { name: "Search epics" })),
        lists.box()?.scrollTop,
      ]);
    // Opens the list on `options`, the two options unless named, after closing it and taking its
    // options to them: the layout gives a box to the rows the open draws alone.
    const reopen = async (options = before) => {
      await user.keyboard("{Escape}");
      view.rerender(host(options));
      await user.click(trigger);
    };
    await user.click(trigger);
    shown("opened on two options");
    view.rerender(host(STATUSES));
    shown("the eight statuses come");
    view.rerender(host(["Draft", ...STATUSES]));
    shown("one more comes before it");

    // An arrow marks Triage active, and the statuses come.
    await reopen();
    await user.keyboard("{ArrowUp}");
    view.rerender(host(STATUSES));
    shown("an arrow, then the statuses");

    // A word typed and erased leaves the search as it began, and the statuses come. Both options
    // hold the word, so the rows the list laid out stay drawn under it.
    await reopen();
    await user.keyboard("e{Backspace}");
    view.rerender(host(STATUSES));
    shown("a word typed, then the statuses");

    // The statuses come; the reader turns the wheel over the list, back to its top, and one
    // more option comes before Needs review.
    await reopen();
    view.rerender(host(STATUSES));
    shown("opened again, the statuses come");
    const box = lists.box();
    if (box === null) throw new Error("the list was never laid out");
    // A turn of the wheel, which user-event has no form for.
    fireEvent.wheel(box, { deltaY: -100 });
    box.scrollTop = 0;
    view.rerender(host(["Draft", ...STATUSES]));
    shown("the reader's scroll, then one more");

    // The window leaves the panel too little room, so it cuts the list's box from 286 px of what
    // it holds to 264 (`AnchoredPanel`), and what watches the box hears of it.
    const cut = () => {
      const panel = lists.box();
      if (panel === null) throw new Error("the list was never laid out");
      Object.defineProperty(panel, "clientHeight", { configurable: true, get: () => 264 });
      panel.getBoundingClientRect = () => new DOMRect(0, 100, 256, 266);
      sizes.resize(panel);
    };
    // The statuses come, and the panel is cut: at 83 the cut box shows 144-347 px of its content
    // below its text box, and Needs review, at 325-369, ends below that.
    await reopen();
    view.rerender(host(STATUSES));
    cut();
    shown("the statuses, then the panel cut");

    // The list opens on the statuses with Needs review shown whole at 83, an arrow marks Testing
    // active, whole at 281-325, and the panel is cut.
    await reopen(STATUSES);
    await user.keyboard("{ArrowUp}");
    cut();
    shown("the statuses, an arrow, then the panel cut");

    expect(seen).toEqual([
      ["opened on two options", "Needs review", 0],
      ["the eight statuses come", "Needs review", 83],
      ["one more comes before it", "Needs review", 127],
      ["an arrow, then the statuses", "Triage", 0],
      ["a word typed, then the statuses", "Triage", 0],
      ["opened again, the statuses come", "Needs review", 83],
      ["the reader's scroll, then one more", "Needs review", 0],
      ["the statuses, then the panel cut", "Needs review", 105],
      ["the statuses, an arrow, then the panel cut", "Testing", 83],
    ]);
    expect(scrolls).not.toHaveBeenCalled();
  } finally {
    scrolls.mockRestore();
    lists.restore();
    view.unmount();
    sizes.restore();
  }
});

// The rows can stay the same values while the text one draws changes: a host's label for a value
// can answer otherwise once a read has come, as a name read after the list opened on a login.
// Inside `NoTooltips` a label wraps rather than ending in an ellipsis, so a longer one makes its
// row taller while the list's box keeps its size, and the option the list opened on can end below
// what the box shows. That change shows it whole again, the list's own box alone scrolled.
test("a single pick keeps the option it opened on shown whole as its label changes over the same options and makes its row taller", async () => {
  const user = userEvent.setup();
  const host = (labels: Readonly<Record<string, string>>) => (
    <NoTooltips>
      <SingleHost initial="Testing" optionLabel={(value) => labels[value]} options={STATUSES} />
    </NoTooltips>
  );
  const view = render(host({}));
  const trigger = screen.getByRole("button", { name: pickNamed("Epic") });
  // A row whose text runs past 26 characters wraps onto a second line of 20 px: 8 px above and
  // below its two lines make it 56 px tall, where one line keeps it at its 44 px least.
  const lists = layOutListsAsOpened(trigger, (option) =>
    (option.textContent ?? "").length > 26 ? 56 : 44
  );
  const scrolls = spyOn(Element.prototype, "scrollIntoView");
  try {
    // The box shows 61-286 px of its content: at 39, the sixth option, Testing, stands whole at
    // 281-325; its longer label makes it 281-337, which ends below what the box shows.
    await user.click(trigger);
    const opened = lists.box()?.scrollTop;
    view.rerender(host({ Testing: "Testing on staging, waiting on its release" }));
    expect(activeName(screen.getByRole("combobox", { name: "Search epics" }))).toBe(
      "Testing on staging, waiting on its release"
    );
    expect([opened, lists.box()?.scrollTop]).toEqual([39, 51]);
    expect(scrolls).not.toHaveBeenCalled();
  } finally {
    scrolls.mockRestore();
    lists.restore();
    view.unmount();
  }
});

test("with onCreate, a search that matches no option offers a Create row that Enter takes", async () => {
  const user = userEvent.setup();
  const created: string[] = [];
  const view = render(<Host onCreate={(value) => created.push(value)} options={["docs"]} />);
  try {
    await user.click(screen.getByRole("button", { name: "Labels" }));
    const search = screen.getByRole("combobox", { name: "Search labels" });
    await user.clear(search);
    await user.click(search);
    await user.paste("Docs");
    expect(screen.queryByRole("option", { name: /Create/ })).not.toBeInTheDocument();
    await user.clear(search);
    await user.click(search);
    await user.paste(" urgent ");
    expect(screen.getByRole("option", { name: 'Create "urgent"' })).toBeTruthy();
    await user.keyboard("{ArrowDown}");
    await user.keyboard("{Enter}");
    expect(created).toEqual(["urgent"]);
    expect((search as HTMLInputElement).value).toBe("");
  } finally {
    view.unmount();
  }
});

test("an outside pointer press closes the popover", async () => {
  const user = userEvent.setup();
  const view = render(
    <>
      <Host options={["docs"]} />
      <button type="button">Elsewhere</button>
    </>
  );
  try {
    await user.click(screen.getByRole("button", { name: "Labels" }));
    expect(screen.getByRole("listbox")).toBeTruthy();
    // A press with no release: user-event's click would add the release and the click the
    // popover's pointerdown listener is deliberately tested without.
    fireEvent.pointerDown(screen.getByRole("option", { name: "docs" }));
    expect(screen.getByRole("listbox")).toBeTruthy();
    // The same press, landing outside: user-event has no press-only form for it either.
    fireEvent.pointerDown(screen.getByRole("button", { name: "Elsewhere" }));
    expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
  } finally {
    view.unmount();
  }
});

test("optionLabel renders the row text and the search matches it while the value stays the key", async () => {
  const user = userEvent.setup();
  const changes: string[][] = [];
  const view = render(
    <MultiSelect
      emptyMessage="Nothing here."
      label="Status"
      onChange={(next) => changes.push(next)}
      onOpenChange={() => undefined}
      open
      optionLabel={(value) => (value === "in_progress" ? "In progress" : value)}
      options={["todo", "in_progress"]}
      searchLabel="Search statuses"
      selected={[]}
    />
  );
  try {
    const search = screen.getByRole("combobox", { name: "Search statuses" });
    await user.clear(search);
    await user.click(search);
    await user.paste("in prog");
    expect(screen.getAllByRole("option").map((option) => option.textContent)).toEqual([
      "In progress",
    ]);
    await user.click(screen.getByRole("option", { name: "In progress" }));
    expect(changes).toEqual([["in_progress"]]);
  } finally {
    view.unmount();
  }
});

test("a row's detail reads after its label and a comma, spoken with its own words when given", () => {
  const view = render(
    <MultiSelect
      emptyMessage="Nothing here."
      label="Labels"
      onChange={() => undefined}
      onOpenChange={() => undefined}
      open
      optionDetail={(value) => (value === "api" ? "3" : "1")}
      optionDetailLabel={(value) => (value === "api" ? "3 issues" : "1 issue")}
      options={["api", "docs"]}
      searchLabel="Search labels"
      selected={[]}
    />
  );
  try {
    // The visible row runs the count into the label; its accessible name never does.
    expect(screen.getAllByRole("option").map((option) => option.textContent)).toEqual([
      "api3",
      "docs1",
    ]);
    expect(screen.getByRole("option", { name: "api, 3 issues" })).toBeDefined();
    expect(screen.getByRole("option", { name: "docs, 1 issue" })).toBeDefined();
  } finally {
    view.unmount();
  }
});

test("the compact size changes the trigger's look alone, and the control carries no data-control", async () => {
  const user = userEvent.setup();
  const opened: boolean[] = [];
  const view = render(
    <MultiSelect
      emptyMessage="Nothing here."
      label="Labels"
      onChange={() => undefined}
      onOpenChange={(next) => opened.push(next)}
      open={false}
      options={["api", "docs"]}
      searchLabel="Search labels"
      selected={["api"]}
      size="compact"
    />
  );
  try {
    const trigger = screen.getByRole("button", { name: "Labels · 1" });
    // The vocabulary's eleven names are the controls a page draws; this one is a part of them,
    // not one of them, so nothing reads it by that attribute.
    expect(trigger).not.toHaveAttribute("data-control");
    expect(trigger).toHaveAttribute("aria-expanded", "false");
    await user.click(trigger);
    expect(opened).toEqual([true]);
  } finally {
    view.unmount();
  }
});

test("a single pick's listbox is not multiselectable, and only its current value's option is selected", async () => {
  const user = userEvent.setup();
  const view = render(<SingleHost initial="docs" options={["api", "docs", "frontend"]} />);
  try {
    const trigger = screen.getByRole("button", { name: pickNamed("Epic") });
    expect(trigger).toHaveTextContent("docs");
    await user.click(trigger);
    expect(screen.getByRole("listbox", { name: "Epic options" })).not.toHaveAttribute(
      "aria-multiselectable"
    );
    expect(screen.getByRole("option", { name: "docs" })).toHaveAttribute("aria-selected", "true");
    for (const name of ["api", "frontend"])
      expect(screen.getByRole("option", { name })).toHaveAttribute("aria-selected", "false");
    // One value is changed by picking another, so there is nothing to clear.
    expect(screen.queryByRole("button", { name: "Clear epic" })).not.toBeInTheDocument();
  } finally {
    view.unmount();
  }
});

// INFRA-3135, round 12: axe's `label-content-name-mismatch` (WCAG 2.5.3, Label in Name) failed on
// every single pick that showed a value: its trigger was named "Assignee of CORE-1" while it
// showed "alice", so the words on it were not in its name. Its name is the list's name, or the
// host's own, then what it shows, leaving out words the host hides from assistive technology,
// and no description repeats the value; a trigger that shows the list's name says it once.
test("a single pick's trigger is named for its list and then for what it shows", () => {
  const view = render(
    <>
      <SingleHost initial="docs" options={["api", "docs"]} />
      <MultiSelect
        emptyMessage="Nothing here."
        label="Issue"
        mode="single"
        onOpenChange={() => undefined}
        onPick={() => undefined}
        open={false}
        options={["CORE-1"]}
        searchLabel="Search issues"
        triggerAriaLabel="Choose issue"
        value="CORE-1"
      >
        Issue: CORE-1
      </MultiSelect>
      <MultiSelect
        chevron={false}
        emptyMessage="Nothing here."
        label="Repository"
        mode="single"
        onOpenChange={() => undefined}
        onPick={() => undefined}
        open={false}
        options={["acme/widgets"]}
        searchLabel="Search repositories"
        value="acme/widgets"
      >
        <span aria-hidden="true">acme/widgets</span>
        <span className="sr-only">acme/widgets, inherited</span>
      </MultiSelect>
      <MultiSelect
        emptyMessage="Nothing here."
        label="Project"
        mode="single"
        onOpenChange={() => undefined}
        onPick={() => undefined}
        open={false}
        options={["CORE"]}
        searchLabel="Search projects"
      />
    </>
  );
  try {
    const picked = screen.getByRole("button", { name: "Epic docs" });
    expect(picked).toHaveTextContent(/^docs$/);
    expect(picked).not.toHaveAccessibleDescription();
    expect(screen.getByRole("button", { name: "Choose issue Issue: CORE-1" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Repository acme/widgets, inherited" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Project" })).toHaveTextContent(/^Project$/);
  } finally {
    view.unmount();
  }
});

// INFRA-3135, round 12: below 1280 px every button is a flex box (`styles.css`), whose own text
// ends in no ellipsis, so a trigger cut a long value mid-word; and no trigger showed that it
// opens a list. In both modes the trigger's text is an element of its own that truncates - a
// single pick's carries its whole value as its title and is still what the trigger's name ends
// in - with a hidden chevron after it; a host whose children are a look of their own, a badge,
// draws them alone. `e2e/table-controls.e2e.ts` reads the ellipsis and the chevron in a browser.
test("a trigger's text truncates in its own element before a hidden chevron, unless its host draws a look of its own", () => {
  const view = render(
    <>
      <SingleHost initial="docs" options={["api", "docs"]} />
      <Host options={["api"]} />
      <MultiSelect
        chevron={false}
        emptyMessage="Nothing here."
        label="Assignee"
        mode="single"
        onOpenChange={() => undefined}
        onPick={() => undefined}
        open={false}
        options={["bob"]}
        searchLabel="Search people"
        value="bob"
      >
        <span data-testid="badge">bob</span>
      </MultiSelect>
    </>
  );
  try {
    const single = screen.getByRole("button", { name: pickNamed("Epic") });
    for (const trigger of [single, screen.getByRole("button", { name: "Labels" })]) {
      const chevron = trigger.querySelector("svg");
      expect(chevron).toHaveAttribute("aria-hidden", "true");
      expect(chevron?.previousElementSibling).toHaveClass("truncate");
    }
    expect(single.querySelector(".truncate")).toHaveAttribute("title", "docs");

    const badge = screen.getByRole("button", { name: pickNamed("Assignee") });
    expect(badge.querySelector("svg")).toBeNull();
    expect(badge.querySelector(".truncate")).toBeNull();
    expect(screen.getByTestId("badge").parentElement?.parentElement).toBe(badge);
  } finally {
    view.unmount();
  }
});

test("a single pick closes the list, puts the focus on its trigger and hands over one value", async () => {
  const user = userEvent.setup();
  const view = render(<SingleHost options={["api", "docs", "frontend"]} />);
  try {
    const trigger = screen.getByRole("button", { name: "Epic" });
    await user.click(trigger);
    await user.click(screen.getByRole("option", { name: "docs" }));
    expect(screen.getByTestId("handed").textContent).toBe("docs");
    expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
    expect(trigger).toHaveAttribute("aria-expanded", "false");
    expect(trigger).toHaveFocus();
    expect(trigger).toHaveTextContent("docs");

    // From the keyboard: the list opens on the value picked, the arrows move the active option
    // on from it, and Enter picks it.
    await user.keyboard("{Enter}");
    const search = screen.getByRole("combobox", { name: "Search epics" });
    expect(search).toHaveFocus();
    await user.keyboard("{ArrowDown}");
    expect(search).toHaveAttribute(
      "aria-activedescendant",
      screen.getByRole("option", { name: "frontend" }).id
    );
    await user.keyboard("{Enter}");
    expect(screen.getByTestId("handed").textContent).toBe("docs,frontend");
    expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
    expect(trigger).toHaveFocus();
    expect(trigger).toHaveTextContent("frontend");
  } finally {
    view.unmount();
  }
});

test("Escape, Tab and an outside press close a single pick with nothing picked and its value kept", async () => {
  const user = userEvent.setup();
  const view = render(<SingleHost initial="api" options={["api", "docs"]} />);
  try {
    const trigger = screen.getByRole("button", { name: pickNamed("Epic") });
    await user.click(trigger);
    await user.keyboard("{ArrowDown}");
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
    expect(trigger).toHaveFocus();

    // Tab and Shift+Tab leave the reader on the trigger rather than carrying them past it.
    for (const shift of [false, true]) {
      await user.click(trigger);
      await user.keyboard("{ArrowDown}");
      await user.tab({ shift });
      expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
      expect(trigger).toHaveFocus();
    }

    await user.click(trigger);
    // A press with no release, as the multi-select's outside press above is.
    fireEvent.pointerDown(screen.getByRole("button", { name: "Elsewhere" }));
    expect(screen.queryByRole("listbox")).not.toBeInTheDocument();

    expect(screen.getByTestId("handed").textContent).toBe("");
    expect(trigger).toHaveTextContent("api");
  } finally {
    view.unmount();
  }
});

// An input method composes text in the list's text box from several keys and ends with Enter,
// which commits what it composed, or Escape, which cancels it: neither is the reader's pick or
// close. user-event has no form for a composing key, so each is fired with `isComposing`.
test("Enter, Escape and Tab that end or move an IME composition leave a single pick open with nothing picked", async () => {
  const user = userEvent.setup();
  const view = render(<SingleHost initial="api" options={["api", "docs"]} />);
  try {
    const trigger = screen.getByRole("button", { name: pickNamed("Epic") });
    await user.click(trigger);
    const box = screen.getByRole("combobox", { name: "Search epics" });
    await user.keyboard("{ArrowDown}");
    for (const key of ["Enter", "Escape", "Tab"]) {
      // user-event cannot mark a key as part of an IME composition, so the key is fired with
      // `isComposing` set.
      fireEvent.keyDown(box, { isComposing: true, key });
      expect(screen.queryByRole("listbox"), `${key} composing`).toBeInTheDocument();
      expect(box, `${key} composing`).toHaveFocus();
    }
    expect(screen.getByTestId("handed").textContent).toBe("");

    // The same keys with no composition are the reader's: Enter picks the active row.
    await user.keyboard("{Enter}");
    expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
    expect(screen.getByTestId("handed").textContent).toBe("docs");
  } finally {
    view.unmount();
  }
});

// Safari sends the key that ends an input method's composition after the composition's end,
// unmarked - `isComposing` false - with keyCode 229 (WebKit bug 165004): that Enter, Escape or Tab
// is the input method's as well, and neither picks nor closes. The plain Enter after it picks.
test("Enter, Escape and Tab that end an IME composition in Safari, at keyCode 229, leave a single pick open with nothing picked", async () => {
  const user = userEvent.setup();
  const view = render(<SingleHost initial="api" options={["api", "docs"]} />);
  try {
    await user.click(screen.getByRole("button", { name: pickNamed("Epic") }));
    const box = screen.getByRole("combobox", { name: "Search epics" });
    await user.keyboard("{ArrowDown}");
    for (const key of ["Enter", "Escape", "Tab"]) {
      // user-event cannot mark a key as an input method's, so the key is fired with keyCode 229.
      fireEvent.keyDown(box, { isComposing: false, key, keyCode: 229 });
      expect(screen.queryByRole("listbox"), `${key} at keyCode 229`).not.toBeNull();
      expect(box, `${key} at keyCode 229`).toHaveFocus();
    }
    expect(screen.getByTestId("handed").textContent).toBe("");

    await user.keyboard("{Enter}");
    expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
    expect(screen.getByTestId("handed").textContent).toBe("docs");
  } finally {
    view.unmount();
  }
});

/** Makes the document keep naming a focused element once it has left the page: an engine that
 *  keeps a removed element active, which `focusLost`'s `!isConnected` check is for. None of
 *  Playwright's Chromium, Firefox and WebKit does so (`focusLost`), and happy-dom names the body
 *  at once, as they do. Answers what puts the document back. */
function keepRemovedFocus(): () => void {
  let prototype: object | null = Object.getPrototypeOf(document);
  let accessor: PropertyDescriptor | undefined;
  while (prototype !== null && accessor === undefined) {
    accessor = Object.getOwnPropertyDescriptor(prototype, "activeElement");
    prototype = Object.getPrototypeOf(prototype);
  }
  const read = accessor?.get;
  if (read === undefined) throw new Error("expected document.activeElement to be an accessor");
  let last: Element | null = null;
  const note = (event: Event) => {
    if (event.target instanceof Element) last = event.target;
  };
  document.addEventListener("focus", note, true);
  Object.defineProperty(document, "activeElement", {
    configurable: true,
    get() {
      const focused: Element | null = read.call(document);
      return focused === document.body && last !== null && !last.isConnected ? last : focused;
    },
  });
  return () => {
    Reflect.deleteProperty(document, "activeElement");
    document.removeEventListener("focus", note, true);
  };
}

// INFRA-3135, round 12: the close returned the focus to the trigger only where it had fallen to
// the body, so in an engine that keeps naming a removed element focused, as `keepRemovedFocus`
// makes happy-dom do, Tab or Shift+Tab out of the text box the close took away left the focus to
// fall onto the page. None of the three engines measured does so (`focusLost`); a close the list
// makes itself puts the reader on the trigger either way, and a press outside still leaves them
// where it put them.
test("where the engine still names a removed text box focused, Tab out of it puts the focus on the trigger and a press outside leaves it where it landed", async () => {
  const user = userEvent.setup();
  const restore = keepRemovedFocus();
  const view = render(<SingleHost initial="api" options={["api", "docs"]} />);
  try {
    const trigger = screen.getByRole("button", { name: pickNamed("Epic") });
    for (const shift of [false, true]) {
      await user.click(trigger);
      expect(screen.getByRole("combobox", { name: "Search epics" })).toHaveFocus();
      await user.tab({ shift });
      expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
      expect(
        trigger,
        `${shift ? "Shift+Tab" : "Tab"} left the focus off the trigger`
      ).toHaveFocus();
    }

    await user.click(trigger);
    const elsewhere = screen.getByRole("button", { name: "Elsewhere" });
    await user.click(elsewhere);
    expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
    expect(elsewhere).toHaveFocus();
    expect(screen.getByTestId("handed").textContent).toBe("");
  } finally {
    view.unmount();
    restore();
  }
});

test("a single pick's text box narrows its options, and Enter over none picks nothing", async () => {
  const user = userEvent.setup();
  const view = render(<SingleHost options={["api", "docs", "frontend"]} />);
  try {
    await user.click(screen.getByRole("button", { name: "Epic" }));
    const search = screen.getByRole("combobox", { name: "Search epics" });
    await user.click(search);
    await user.paste("FRONT");
    expect(screen.getAllByRole("option").map((option) => option.textContent)).toEqual(["frontend"]);
    await user.clear(search);
    await user.click(search);
    await user.paste("zzz");
    expect(screen.queryAllByRole("option")).toHaveLength(0);
    expect(screen.getByText("Nothing here.")).toBeTruthy();
    await user.keyboard("{Enter}");
    expect(screen.getByTestId("handed").textContent).toBe("");
    expect(screen.getByRole("listbox", { name: "Epic options" })).toBeTruthy();
  } finally {
    view.unmount();
  }
});

// A single pick whose host takes values its options lack - the issue header's Repository takes
// any `owner/name` - offers a typed text no option holds as a row of its own, in the host's words,
// and hands the text over trimmed. A text that names an option offers no such row; one an option
// merely contains does. The host's `true` closes the list as a pick does.
test("a single pick's onCreate row offers a text no option holds, and a taken one closes the list on the trigger", async () => {
  const user = userEvent.setup();
  const created: string[] = [];
  const view = render(
    <SingleHost
      onCreate={(value) => {
        created.push(value);
        return true;
      }}
      options={["api", "docs"]}
    />
  );
  try {
    const trigger = screen.getByRole("button", { name: "Epic" });
    await user.click(trigger);
    const search = screen.getByRole("combobox", { name: "Search epics" });
    await user.click(search);
    await user.paste("DOCS");
    expect(screen.getAllByRole("option").map((option) => option.textContent)).toEqual(["docs"]);
    await user.clear(search);
    await user.click(search);
    await user.paste(" urgent ");
    // No option holds it: the empty message says so, and the row under it takes the text.
    expect(screen.getByText("Nothing here.")).toBeTruthy();
    expect(screen.getAllByRole("option").map((option) => option.textContent)).toEqual([
      '+Use "urgent"',
    ]);
    await user.click(screen.getByRole("option", { name: 'Use "urgent"' }));
    expect(created).toEqual(["urgent"]);
    expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
    expect(trigger).toHaveFocus();
    expect(screen.getByTestId("closings").textContent).toBe("pick");
    expect(screen.getByTestId("handed").textContent).toBe("");

    // From the keyboard: Enter takes the row while it is the active one.
    await user.keyboard("{Enter}");
    await user.paste("ap");
    expect(screen.getAllByRole("option").map((option) => option.textContent)).toEqual([
      "api",
      '+Use "ap"',
    ]);
    await user.keyboard("{ArrowDown}");
    expect(screen.getByRole("combobox", { name: "Search epics" })).toHaveAttribute(
      "aria-activedescendant",
      screen.getByRole("option", { name: 'Use "ap"' }).id
    );
    await user.keyboard("{Enter}");
    expect(created).toEqual(["urgent", "ap"]);
    expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
    expect(trigger).toHaveFocus();
    expect(screen.getByTestId("handed").textContent).toBe("");
  } finally {
    view.unmount();
  }
});

// The host's `false` refuses the text: the list stays open with the text in its box and the
// focus in it, for the reader to correct.
test("a single pick's onCreate row the host refuses leaves the list open with the text in its box", async () => {
  const user = userEvent.setup();
  const created: string[] = [];
  const view = render(
    <SingleHost
      onCreate={(value) => {
        created.push(value);
        return false;
      }}
      options={["api"]}
    />
  );
  try {
    await user.click(screen.getByRole("button", { name: "Epic" }));
    const search = screen.getByRole("combobox", { name: "Search epics" }) as HTMLInputElement;
    await user.click(search);
    await user.paste("not one");
    await user.keyboard("{Enter}");
    await user.click(screen.getByRole("option", { name: 'Use "not one"' }));
    expect(created).toEqual(["not one", "not one"]);
    expect(screen.getByRole("listbox", { name: "Epic options" })).toBeTruthy();
    expect(search.value).toBe("not one");
    expect(search).toHaveFocus();
    expect(screen.getByTestId("closings").textContent).toBe("");
    expect(screen.getByTestId("handed").textContent).toBe("");
  } finally {
    view.unmount();
  }
});

// INFRA-3135, round 12: the Agents composer's Issue list. Enter on the list opened over the issue
// already picked has to keep that issue, as Enter on a `<select>` does, so the list opens with its
// picked value active rather than its first option.
test("a single pick opens with its picked value active, so Enter keeps it and the arrows move on from it", async () => {
  const user = userEvent.setup();
  const view = render(<SingleHost initial="docs" options={["api", "docs", "frontend"]} />);
  try {
    const trigger = screen.getByRole("button", { name: pickNamed("Epic") });
    await user.click(trigger);
    expect(screen.getByRole("combobox", { name: "Search epics" })).toHaveAttribute(
      "aria-activedescendant",
      screen.getByRole("option", { name: "docs" }).id
    );
    await user.keyboard("{Enter}");
    expect(screen.getByTestId("handed").textContent).toBe("docs");
    expect(trigger).toHaveTextContent("docs");

    await user.click(trigger);
    await user.keyboard("{ArrowDown}");
    expect(screen.getByRole("combobox", { name: "Search epics" })).toHaveAttribute(
      "aria-activedescendant",
      screen.getByRole("option", { name: "frontend" }).id
    );
  } finally {
    view.unmount();
  }
});

/** Checks that the single pick stands open as a click on its trigger opens it: the focus in its
 *  text box, the search empty, and the picked value `active` the active option. */
function openedAsClicked(active: string): void {
  const search = screen.getByRole("combobox", { name: "Search epics" });
  expect(search).toHaveFocus();
  expect((search as HTMLInputElement).value).toBe("");
  expect(search).toHaveAttribute(
    "aria-activedescendant",
    screen.getByRole("option", { name: active }).id
  );
}

// The closed trigger opens on the keys the WAI-ARIA select-only combobox opens its list on,
// beside Enter and Space: ArrowDown and ArrowUp, with Alt (Option on a Mac) or without. Each of
// ArrowDown, Alt+ArrowDown and ArrowUp opens it as a click does, and moves no option.
test.each([
  ["ArrowDown", "{ArrowDown}"],
  ["Alt+ArrowDown", "{Alt>}{ArrowDown}{/Alt}"],
  ["ArrowUp", "{ArrowUp}"],
])("%s on the closed trigger opens the list as a click does", async (_name, keys) => {
  const user = userEvent.setup();
  const view = render(<SingleHost initial="docs" options={["api", "docs", "frontend"]} />);
  try {
    const trigger = screen.getByRole("button", { name: pickNamed("Epic") });
    act(() => trigger.focus());
    await user.keyboard(keys);
    expect(trigger).toHaveAttribute("aria-expanded", "true");
    openedAsClicked("docs");
  } finally {
    view.unmount();
  }
});

// A character typed on the closed trigger opens the list with that character in its text box,
// typed once, so the options are already narrowed by it and the next character follows it. Shift
// makes a character of its own.
test("a character typed on the closed trigger opens the list with it in the search, the options narrowed by it", async () => {
  const user = userEvent.setup();
  const queries: string[] = [];
  const view = render(
    <SingleHost
      onQueryChange={(query) => queries.push(query)}
      options={["api", "docs", "frontend"]}
    />
  );
  try {
    const trigger = screen.getByRole("button", { name: "Epic" });
    act(() => trigger.focus());
    await user.keyboard("p");
    const search = screen.getByRole("combobox", { name: "Search epics" }) as HTMLInputElement;
    expect(search).toHaveFocus();
    expect(search.value).toBe("p");
    expect([search.selectionStart, search.selectionEnd]).toEqual([1, 1]);
    expect(screen.getAllByRole("option").map((option) => option.textContent)).toEqual(["api"]);
    await user.keyboard("i");
    expect(search.value).toBe("pi");
    expect(queries).toEqual(["p", "pi"]);

    await user.keyboard("{Escape}");
    expect(trigger).toHaveFocus();
    await user.keyboard("{Shift>}D{/Shift}");
    expect((screen.getByRole("combobox", { name: "Search epics" }) as HTMLInputElement).value).toBe(
      "D"
    );
    expect(screen.getAllByRole("option").map((option) => option.textContent)).toEqual([
      "docs",
      "frontend",
    ]);
    expect(screen.getByTestId("handed").textContent).toBe("");
  } finally {
    view.unmount();
  }
});

// Space presses the trigger, as a click does, and types nothing into the search, even after a
// typed character opened the list before.
test("Space on the closed trigger still opens the list with an empty search", async () => {
  const user = userEvent.setup();
  const view = render(<SingleHost initial="docs" options={["api", "docs", "frontend"]} />);
  try {
    const trigger = screen.getByRole("button", { name: pickNamed("Epic") });
    act(() => trigger.focus());
    await user.keyboard("f");
    expect((screen.getByRole("combobox", { name: "Search epics" }) as HTMLInputElement).value).toBe(
      "f"
    );
    await user.keyboard("{Escape}");
    expect(trigger).toHaveFocus();
    await user.keyboard(" ");
    openedAsClicked("docs");
    expect(screen.getAllByRole("option")).toHaveLength(3);
  } finally {
    view.unmount();
  }
});

// A busy trigger (`aria-disabled`) and a disabled one open on none of the keys, whether or not the
// engine leaves the focus on them. The same keys open the trigger once it is enabled again.
test("a disabled or busy trigger opens on none of the keys that open an enabled one", async () => {
  const user = userEvent.setup();
  const keys = ["{ArrowDown}", "{Alt>}{ArrowDown}{/Alt}", "{ArrowUp}", "d"];
  const view = render(<SingleHost options={["api", "docs"]} />);
  try {
    const trigger = screen.getByRole("button", { name: "Epic" });
    for (const state of [{ ariaDisabled: true }, { disabled: true }]) {
      view.rerender(<SingleHost {...state} options={["api", "docs"]} />);
      for (const key of keys) {
        act(() => trigger.focus());
        await user.keyboard(key);
        expect(screen.queryByRole("listbox"), key).not.toBeInTheDocument();
        expect(trigger, key).toHaveAttribute("aria-expanded", "false");
      }
    }

    view.rerender(<SingleHost options={["api", "docs"]} />);
    for (const key of keys) {
      act(() => trigger.focus());
      await user.keyboard(key);
      expect(screen.queryByRole("listbox"), key).toBeInTheDocument();
      await user.keyboard("{Escape}");
    }
  } finally {
    view.unmount();
  }
});

// A host whose next move depends on how the reader left the list - the Agents composer's, whose
// Escape goes one level out to the row - is told what closed it.
test("every close tells the host what closed the list: a pick, Escape, Tab, a press outside, the trigger", async () => {
  const user = userEvent.setup();
  const view = render(<SingleHost initial="api" options={["api", "docs"]} />);
  try {
    const trigger = screen.getByRole("button", { name: pickNamed("Epic") });
    await user.click(trigger);
    await user.click(screen.getByRole("option", { name: "docs" }));
    await user.click(trigger);
    await user.keyboard("{Escape}");
    await user.click(trigger);
    await user.tab();
    await user.click(trigger);
    // A press with no release, as the outside presses above are.
    fireEvent.pointerDown(screen.getByRole("button", { name: "Elsewhere" }));
    await user.click(trigger);
    await user.click(trigger);
    expect(screen.getByTestId("closings").textContent).toBe("pick,escape,tab,press,trigger");
  } finally {
    view.unmount();
  }
});

// The Agents composer opens its Issue list before the issues have come and hands the focus in
// itself once they have, and finds the list's text box - portalled out of the row - through the
// panel the trigger names.
test("the trigger names the open list's panel, the host's marks are on the trigger and the text box, and focusOnOpen false leaves the focus", async () => {
  const user = userEvent.setup();
  const view = render(
    <SingleHost
      focusOnOpen={false}
      marks={{ search: { "data-test-search": "" }, trigger: { "data-test-trigger": "" } }}
      options={["api", "docs"]}
    />
  );
  try {
    const trigger = screen.getByRole("button", { name: "Epic" });
    expect(trigger).toHaveAttribute("data-test-trigger", "");
    expect(trigger).not.toHaveAttribute("aria-controls");
    await user.click(trigger);
    expect(trigger).toHaveFocus();
    const panel = panelOf(trigger);
    const search = screen.getByRole("combobox", { name: "Search epics" });
    expect(search).toHaveAttribute("data-test-search", "");
    expect(panel.contains(search)).toBe(true);
    expect(panel.contains(screen.getByRole("listbox", { name: "Epic options" }))).toBe(true);
  } finally {
    view.unmount();
  }
});

// The Agents page closes a row's Issue list from the row itself (`leaveAgentComposer`), with the
// reader on the row: the close leaves them there rather than pulling them onto the trigger.
test("a close the host makes while the focus is outside the list leaves the focus there", async () => {
  const user = userEvent.setup();
  const view = render(<SingleHost options={["api", "docs"]} />);
  try {
    await user.click(screen.getByRole("button", { name: "Epic" }));
    const elsewhere = screen.getByRole("button", { name: "Elsewhere" });
    act(() => elsewhere.focus());
    await user.keyboard("{Enter}");
    expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
    expect(elsewhere).toHaveFocus();
  } finally {
    view.unmount();
  }
});

/** Every key that reached the window with nothing having handled it - what the page's keymap,
 *  which listens there and skips a handled key, would read as its own - until `stop`. */
function keysThePageReads(): { readonly keys: string[]; readonly stop: () => void } {
  const keys: string[] = [];
  const listen = (event: KeyboardEvent) => {
    if (!event.defaultPrevented) keys.push(event.key);
  };
  window.addEventListener("keydown", listen);
  return { keys, stop: () => window.removeEventListener("keydown", listen) };
}

// dispatch://INFRA-2160, R6: a host that writes the moment a value is picked - the assignee, the
// repository, an ask's hand-on - takes the keys a `<select>` takes on its closed trigger, and
// each only moves the list's highlight.
test("with keysOnTrigger, a move key on the closed trigger opens the list on the value picked and moves the highlight by that key, and the page never reads it", async () => {
  const user = userEvent.setup();
  const epics = Array.from({ length: 25 }, (_, index) => `epic-${index + 1}`);
  const view = render(<SingleHost initial="epic-13" keysOnTrigger options={epics} />);
  const page = keysThePageReads();
  try {
    const trigger = screen.getByRole("button", { name: pickNamed("Epic") });
    act(() => trigger.focus());
    for (const [key, to] of [
      ["ArrowDown", "epic-14"],
      ["ArrowUp", "epic-12"],
      ["Home", "epic-1"],
      ["End", "epic-25"],
      // Ten rows on from the value picked, the thirteenth, and ten back from it.
      ["PageDown", "epic-23"],
      ["PageUp", "epic-3"],
    ] as const) {
      await user.keyboard(`{${key}}`);
      expect(
        screen.getByRole("listbox", { name: "Epic options" }),
        `${key} opens the list`
      ).toBeTruthy();
      expect(screen.getByRole("combobox", { name: "Search epics" })).toHaveFocus();
      expect(screen.getByRole("combobox", { name: "Search epics" })).toHaveValue("");
      expect(
        highlightedRow(screen.getByRole("combobox", { name: "Search epics" })),
        `${key} highlights ${to}`
      ).toBe(to);
      await user.keyboard("{Escape}");
      expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
      expect(trigger).toHaveFocus();
      expect(trigger).toHaveTextContent("epic-13");
    }
    // Every key was the list's own: the opening keys the trigger's, the Escapes the panel's.
    expect(page.keys).toEqual([]);
    expect(screen.getByTestId("handed").textContent).toBe("");
    expect(screen.getByTestId("closings").textContent).toBe(Array(6).fill("escape").join(","));
  } finally {
    page.stop();
    view.unmount();
  }
});

/**
 * Lays out every list a key opens as `layOutList` does one, from before the list is drawn: a key
 * on the closed trigger opens the list on a row far down, and the list scrolls in the commit that
 * draws it, before a test could lay out what it drew. The box that scrolls, a listbox's parent, is
 * 288 px tall at 100 px down the window inside a 1 px border, so it shows 286 px; the text box's
 * row is 61 px, stuck to its top; each option is 44 px under it in the list's order, moved up by
 * the box's `scrollTop`, which stops at both ends of the scroll. A list's trigger stands above the
 * box, so the panel does not close as one hanging off nothing (`AnchoredPanel`), and every other
 * element keeps happy-dom's own box. Answers the undo.
 */
function layOutListsAsDrawn(): () => void {
  const scrolled = new WeakMap<Element, number>();
  const isBox = (element: Element) => element.querySelector(':scope > [role="listbox"]') !== null;
  const optionsIn = (box: Element) => [...box.querySelectorAll('[role="option"]')];
  const ownRect = Element.prototype.getBoundingClientRect;
  const rects = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ) {
    if (isBox(this)) return new DOMRect(0, 100, 256, 288);
    if (this.getAttribute("aria-haspopup") === "listbox") return new DOMRect(0, 52, 120, 44);
    if (this.querySelector(':scope > [role="combobox"]') !== null) {
      return new DOMRect(1, 101, 254, 61);
    }
    const box = this.closest('[role="listbox"]')?.parentElement ?? null;
    if (this.getAttribute("role") !== "option" || box === null) return ownRect.call(this);
    const top = 162 + 44 * optionsIn(box).indexOf(this) - (scrolled.get(box) ?? 0);
    return new DOMRect(1, top, 254, 44);
  });
  const own = ["scrollTop", "clientHeight", "clientTop"] as const;
  const before = own.map((name) => Object.getOwnPropertyDescriptor(HTMLElement.prototype, name));
  const original = (name: (typeof own)[number]) =>
    Object.getOwnPropertyDescriptor(HTMLElement.prototype, name) ??
    Object.getOwnPropertyDescriptor(Element.prototype, name);
  const scrollTop = original("scrollTop");
  const clientHeight = original("clientHeight");
  const clientTop = original("clientTop");
  Object.defineProperties(HTMLElement.prototype, {
    clientHeight: {
      configurable: true,
      get(this: HTMLElement) {
        return isBox(this) ? 286 : clientHeight?.get?.call(this);
      },
    },
    clientTop: {
      configurable: true,
      get(this: HTMLElement) {
        return isBox(this) ? 1 : clientTop?.get?.call(this);
      },
    },
    scrollTop: {
      configurable: true,
      get(this: HTMLElement) {
        return isBox(this) ? (scrolled.get(this) ?? 0) : scrollTop?.get?.call(this);
      },
      set(this: HTMLElement, value: number) {
        if (!isBox(this)) {
          scrollTop?.set?.call(this, value);
          return;
        }
        const end = 61 + optionsIn(this).length * 44 - 286;
        scrolled.set(this, Math.min(Math.max(value, 0), Math.max(end, 0)));
      },
    },
  });
  return () => {
    rects.mockRestore();
    for (const [index, name] of own.entries()) {
      const descriptor = before[index];
      if (descriptor === undefined) Reflect.deleteProperty(HTMLElement.prototype, name);
      else Object.defineProperty(HTMLElement.prototype, name, descriptor);
    }
  };
}

// The panel holds about five rows and marks the highlighted one on the row alone
// (`aria-activedescendant`), so a key that moves the highlight past its fold - End, Page Down,
// ArrowUp's wrap from a trigger holding nothing, ArrowUp's wrap in the text box - scrolls the
// list's own box the least that shows the row whole under its text box, and never the window
// (`scrollIntoView` scrolls both), before Enter can pick it.
test("a single pick scrolls its own box to show the row each key highlights, and scrolls nothing else", async () => {
  const user = userEvent.setup();
  const epics = Array.from({ length: 25 }, (_, index) => `epic-${index + 1}`);
  const undo = layOutListsAsDrawn();
  const scrolls = spyOn(Element.prototype, "scrollIntoView");
  const view = render(<SingleHost keysOnTrigger options={epics} />);
  try {
    const trigger = screen.getByRole("button", { name: pickNamed("Epic") });
    act(() => trigger.focus());
    // The box shows the window's 162-387 px under its text box; each row a key highlights must
    // stand whole there, whatever the box had scrolled to before.
    for (const [keys, to] of [
      ["{End}", "epic-25"],
      ["{PageDown}", "epic-10"],
      ["{ArrowUp}", "epic-25"],
      ["{Home}{ArrowUp}", "epic-25"],
    ] as const) {
      await user.keyboard(keys);
      const search = screen.getByRole("combobox", { name: "Search epics" });
      expect(highlightedRow(search), `${keys} highlights ${to}`).toBe(to);
      const row = document.getElementById(search.getAttribute("aria-activedescendant") ?? "");
      const { bottom, top } = row?.getBoundingClientRect() ?? new DOMRect();
      expect(top >= 162 && bottom <= 387, `${keys} shows ${to} whole in the box`).toBe(true);
      await user.keyboard("{Escape}");
    }
    expect(scrolls).not.toHaveBeenCalled();
  } finally {
    scrolls.mockRestore();
    undo();
    view.unmount();
  }
});

test("with keysOnTrigger, a printable key on the closed trigger opens the list with that key in the search box, whose first match Enter picks", async () => {
  const user = userEvent.setup();
  const view = render(
    <SingleHost initial="api" keysOnTrigger options={["api", "docs", "frontend"]} />
  );
  const page = keysThePageReads();
  try {
    const trigger = screen.getByRole("button", { name: pickNamed("Epic") });
    act(() => trigger.focus());
    await user.keyboard("d");
    const search = screen.getByRole("combobox", { name: "Search epics" });
    expect(search).toHaveFocus();
    expect(search).toHaveValue("d");
    expect(screen.getAllByRole("option").map((option) => option.textContent)).toEqual([
      "docs",
      "frontend",
    ]);
    expect(highlightedRow(screen.getByRole("combobox", { name: "Search epics" }))).toBe("docs");
    // The key is the search's first letter, and the next ones follow it there.
    await user.keyboard("o");
    expect(search).toHaveValue("do");
    await user.keyboard("{Enter}");
    expect(screen.getByTestId("handed").textContent).toBe("docs");
    expect(trigger).toHaveFocus();
    // The letter that opened the list was the trigger's own; the one typed into the text box
    // reaches the window, where the keymap reads no letter typed into a text box.
    expect(page.keys).toEqual(["o"]);

    // A chord is the page's, and Space presses the trigger: the list opens on the value picked,
    // with an empty search.
    await user.keyboard("{Control>}k{/Control}");
    expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
    expect(page.keys).toEqual(["o", "Control", "k"]);
    await user.keyboard(" ");
    expect(screen.getByRole("combobox", { name: "Search epics" })).toHaveValue("");
    expect(highlightedRow(screen.getByRole("combobox", { name: "Search epics" }))).toBe("docs");
  } finally {
    page.stop();
    view.unmount();
  }
});

// A character typed on a closed trigger is its list's search, punctuation as much as a letter or
// a digit, as the WAI-ARIA select-only combobox takes every printable character and as
// INFRA-3135's list-pick row presses `?` on every single pick's trigger, the assignee, the
// repository and the hand-on among them. It writes nothing (R6), and it is the trigger's own, so
// the page never reads it: the keymap reads a list's trigger as editable, so no Keyboard
// shortcuts open over it either way.
test("with keysOnTrigger, punctuation on the closed trigger opens the list with it in the search box, as a letter does, and writes nothing", async () => {
  const user = userEvent.setup();
  const view = render(
    <SingleHost initial="api" keysOnTrigger options={["api", "docs", "frontend"]} />
  );
  const page = keysThePageReads();
  try {
    const trigger = screen.getByRole("button", { name: pickNamed("Epic") });
    act(() => trigger.focus());
    for (const key of ["?", "/", "]", ".", "2"]) {
      await user.keyboard(key);
      expect(
        screen.getByRole("combobox", { name: "Search epics" }),
        `${key} did not open the list with it in the search box`
      ).toHaveValue(key);
      await user.keyboard("{Escape}");
      expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
      expect(trigger).toHaveFocus();
    }
    expect(page.keys).toEqual([]);
    expect(screen.getByTestId("handed").textContent).toBe("");
  } finally {
    page.stop();
    view.unmount();
  }
});

// Opt-in: without `keysOnTrigger` the closed trigger opens on the keys every list's trigger opens
// on - Enter, Space, ArrowDown, ArrowUp and a typed character, as the cases above read - and the
// keys a `<select>` alone takes there, Home, End, Page Down and Page Up, open nothing and stay the
// page's. ArrowDown opens the list as a click does, on the value picked, and moves nothing.
test("without keysOnTrigger, Home, End, Page Down and Page Up on the closed trigger open nothing and stay the page's", async () => {
  const user = userEvent.setup();
  const view = render(<SingleHost initial="api" options={["api", "docs"]} />);
  const page = keysThePageReads();
  try {
    const trigger = screen.getByRole("button", { name: pickNamed("Epic") });
    act(() => trigger.focus());
    for (const key of ["{Home}", "{End}", "{PageDown}", "{PageUp}"]) {
      await user.keyboard(key);
    }
    expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
    expect(trigger).toHaveFocus();
    expect(page.keys).toEqual(["Home", "End", "PageDown", "PageUp"]);
    await user.keyboard("{ArrowDown}");
    expect(highlightedRow(screen.getByRole("combobox", { name: "Search epics" }))).toBe("api");
    expect(page.keys).toEqual(["Home", "End", "PageDown", "PageUp"]);
    expect(screen.getByTestId("handed").textContent).toBe("");
  } finally {
    page.stop();
    view.unmount();
  }
});

/** A host whose list stays open: it ignores every ask of the list's to close. No key moves the
 *  focus from the search box to a row: the box's Tab closes the list in a host that lets it close,
 *  and hands the focus to the trigger. So the cases below put the focus on a row, the Create row or
 *  Clear directly, as a screen reader's cursor does: those are buttons, which it focuses. With
 *  `pickedFirst` it orders the rows as the labels' and the components' hosts do (`IssueLabels`,
 *  `ComponentPicker`): the picked ones first, each part by its name. With `dropPicked` it lists
 *  only the values not yet picked, so a row goes as it is picked. It stands for the row a press
 *  takes off the page in Details: a label only the open draft holds, which `IssueLabels` lists only
 *  while the draft holds it, goes as the reader unpicks it. */
function OpenHost({
  dropPicked = false,
  onCreate,
  options,
  pickedFirst = false,
}: {
  dropPicked?: boolean;
  onCreate?: (value: string) => void;
  options: string[];
  pickedFirst?: boolean;
}) {
  const [selected, setSelected] = useState<string[]>([]);
  const ordered = pickedFirst
    ? [...options].sort(
        (left, right) =>
          Number(selected.includes(right)) - Number(selected.includes(left)) ||
          left.localeCompare(right)
      )
    : options;
  return (
    <>
      <MultiSelect
        emptyMessage="Nothing here."
        label="Labels"
        onChange={setSelected}
        onCreate={onCreate}
        onOpenChange={() => undefined}
        open
        options={dropPicked ? ordered.filter((option) => !selected.includes(option)) : ordered}
        searchLabel="Search labels"
        selected={selected}
      />
      <output data-testid="selected">{selected.join(",")}</output>
    </>
  );
}

// Clear is drawn only while the pick holds a value, so a press of Clear takes Clear away under
// the focus. The focus goes to the search box, not to the body, where the popover's Escape no
// longer reaches it.
test("a keyboard press of Clear, by Enter or Space, empties the pick and the focus stays in the search box", async () => {
  const user = userEvent.setup();
  const view = render(<OpenHost options={["api", "docs"]} />);
  try {
    const search = screen.getByRole("combobox", { name: "Search labels" });
    expect(search).toHaveFocus();
    for (const key of ["{Enter}", " "]) {
      await user.keyboard("{Enter}");
      expect(screen.getByTestId("selected").textContent).toBe("api");
      // On Clear, as a screen reader's cursor puts the focus there.
      screen.getByRole("button", { name: "Clear labels" }).focus();
      expect(screen.getByRole("button", { name: "Clear labels" })).toHaveFocus();
      await user.keyboard(key);
      expect(screen.getByTestId("selected").textContent).toBe("");
      expect(screen.queryByRole("button", { name: "Clear labels" })).not.toBeInTheDocument();
      expect(search).toHaveFocus();
    }
  } finally {
    view.unmount();
  }
});

// The Create row is drawn only while the search names no option, so a press of it, which makes
// the value and empties the search, takes it away under the focus.
test("a keyboard press of the Create row makes the value and the focus stays in the search box", async () => {
  const user = userEvent.setup();
  const created: string[] = [];
  const view = render(<OpenHost onCreate={(value) => created.push(value)} options={["docs"]} />);
  try {
    const search = screen.getByRole("combobox", { name: "Search labels" });
    await user.click(search);
    await user.paste("urgent");
    screen.getByRole("option", { name: 'Create "urgent"' }).focus();
    expect(screen.getByRole("option", { name: 'Create "urgent"' })).toHaveFocus();
    await user.keyboard("{Enter}");
    expect(created).toEqual(["urgent"]);
    expect(screen.queryByRole("option", { name: /Create/ })).not.toBeInTheDocument();
    expect(search).toHaveFocus();
  } finally {
    view.unmount();
  }
});

// The host's options are a read that can answer again while the popover is open, as a project's
// labels can come back without the one a row draws. That takes away the row the focus is on, which
// is no press of the reader's, so the focus stays on the page's body, as for every change the
// reader did not make.
test("a row that new options drop leaves the focus on the page's body", async () => {
  const view = render(<OpenHost options={["api", "docs"]} />);
  try {
    screen.getByRole("option", { name: "docs" }).focus();
    expect(screen.getByRole("option", { name: "docs" })).toHaveFocus();
    view.rerender(<OpenHost options={["api"]} />);
    expect(screen.queryByRole("option", { name: "docs" })).not.toBeInTheDocument();
    expect(document.body).toHaveFocus();
  } finally {
    view.unmount();
  }
});

// Options that come to hold the value the Create row offers make that value an option's row, so
// they take away the Create row the focus is on. That is no press of the reader's, so the focus
// stays on the page's body.
test("a Create row that new options name leaves the focus on the page's body", async () => {
  const user = userEvent.setup();
  const view = render(<OpenHost onCreate={() => undefined} options={["api"]} />);
  try {
    await user.click(screen.getByRole("combobox", { name: "Search labels" }));
    await user.paste("urgent");
    screen.getByRole("option", { name: 'Create "urgent"' }).focus();
    expect(screen.getByRole("option", { name: 'Create "urgent"' })).toHaveFocus();
    view.rerender(<OpenHost onCreate={() => undefined} options={["api", "urgent"]} />);
    expect(screen.queryByRole("option", { name: /Create/ })).not.toBeInTheDocument();
    expect(screen.getByRole("option", { name: "urgent" })).toBeTruthy();
    expect(document.body).toHaveFocus();
  } finally {
    view.unmount();
  }
});

// A pointer press of Clear leaves the focus in the search box, so Clear goes without it. That
// press answers no later loss: a Create row a screen reader's cursor focused, which new options
// then take away, is no press of the reader's, and its focus stays on the page's body.
test("a pointer press of Clear answers no later loss: a Create row new options take away leaves the focus on the page's body", async () => {
  const user = userEvent.setup();
  const view = render(<OpenHost onCreate={() => undefined} options={["api", "docs"]} />);
  try {
    const search = screen.getByRole("combobox", { name: "Search labels" });
    await user.keyboard("{Enter}");
    expect(screen.getByTestId("selected").textContent).toBe("api");
    await user.click(screen.getByRole("button", { name: "Clear labels" }));
    expect(screen.getByTestId("selected").textContent).toBe("");
    expect(search, "a pointer press of Clear took the focus out of the search box").toHaveFocus();
    await user.paste("urgent");
    const create = screen.getByRole("option", { name: 'Create "urgent"' });
    create.focus();
    view.rerender(<OpenHost onCreate={() => undefined} options={["api", "docs", "urgent"]} />);
    expect(screen.queryByRole("option", { name: /Create/ })).not.toBeInTheDocument();
    expect(
      document.body,
      "new options took the Create row away, and the focus left the page's body though the reader pressed nothing on it"
    ).toHaveFocus();
  } finally {
    view.unmount();
  }
});

/** Makes `parent` drop the focus of a child it moves, as Chromium does: a node taken out of the
 *  document to be put back elsewhere loses the focus to the body, where happy-dom keeps it on the
 *  node. React moves a keyed child with `insertBefore`, or with `appendChild` to the end. The
 *  patch is on `parent` alone, so it goes with that element. */
function dropFocusOnMove(parent: HTMLElement): void {
  const { appendChild, insertBefore } = parent;
  const blurMoved = (node: Node) => {
    const focused = document.activeElement;
    if (node.parentNode === parent && focused instanceof HTMLElement && node.contains(focused)) {
      focused.blur();
    }
  };
  parent.insertBefore = <T extends Node>(node: T, child: Node | null): T => {
    blurMoved(node);
    return insertBefore.call(parent, node, child) as T;
  };
  parent.appendChild = <T extends Node>(node: T): T => {
    blurMoved(node);
    return appendChild.call(parent, node) as T;
  };
}

// A host that orders the rows by the pick moves a row as the reader unpicks it. Chromium drops the
// focus of a node it moves to the page's body (`dropFocusOnMove` does that here), and React gives
// it back to the row as it ends the commit, since the row is still on the page. So a row that held
// the focus as it was pressed, as a screen reader's cursor puts it there, keeps it in its new
// place, and nothing takes it to the search box.
test("a focused row that moves as the reader unpicks it keeps the focus", async () => {
  const user = userEvent.setup();
  const view = render(<OpenHost options={["api", "bug", "docs"]} pickedFirst />);
  try {
    dropFocusOnMove(screen.getByRole("listbox", { name: "Labels options" }));
    await user.keyboard("{Enter}");
    await user.keyboard("{ArrowDown}");
    await user.keyboard("{Enter}");
    expect(screen.getByTestId("selected").textContent).toBe("api,bug");
    const api = screen.getByRole("option", { name: "api" });
    expect(screen.getAllByRole("option")[0], "the picked rows are not drawn first").toBe(api);
    api.focus();
    await user.click(api);
    expect(screen.getByTestId("selected").textContent).toBe("bug");
    expect(screen.getAllByRole("option")[1], "the unpicked row did not move").toBe(api);
    expect(api, "React keeps a moved row's focus, and the row lost it").toHaveFocus();
  } finally {
    view.unmount();
  }
});

// A host that does not order the rows by the pick leaves each row in its place as it is pressed,
// so a row that holds the focus keeps it, and the reader goes on from that row rather than from
// the search box.
test("a focused row that stays in its place as it is pressed keeps the focus", async () => {
  const user = userEvent.setup();
  const view = render(<OpenHost options={["api", "bug", "docs"]} />);
  try {
    const bug = screen.getByRole("option", { name: "bug" });
    bug.focus();
    await user.keyboard("{Enter}");
    expect(screen.getByTestId("selected").textContent).toBe("bug");
    expect(screen.getAllByRole("option")[1], "the picked row left its place").toBe(bug);
    expect(bug, "the row stayed in its place, and its pick took the focus off it").toHaveFocus();
    await user.keyboard(" ");
    expect(screen.getByTestId("selected").textContent).toBe("");
    expect(bug, "the row stayed in its place, and its unpick took the focus off it").toHaveFocus();
  } finally {
    view.unmount();
  }
});

// A press can take a row off the page, as `IssueLabels` does in Details with a label only its open
// draft holds, as the reader unpicks it; `dropPicked` stands for that, taking the row off as it is
// picked. The focus on that row falls to the page's body, where Escape no longer reaches the
// popover: React has no row to give it back to. A row that held the focus as it was pressed has
// that focus given to the search box once the press is drawn.
test("a focused row that its pick takes off the page gives the focus to the search box", async () => {
  const user = userEvent.setup();
  const view = render(<OpenHost dropPicked options={["api", "bug", "docs"]} />);
  try {
    const search = screen.getByRole("combobox", { name: "Search labels" });
    const bug = screen.getByRole("option", { name: "bug" });
    bug.focus();
    await user.keyboard("{Enter}");
    expect(screen.getByTestId("selected").textContent).toBe("bug");
    expect(bug.isConnected, "the picked row is still on the page").toBe(false);
    expect(
      search,
      "the pick took the row off the page, and the focus did not go to the search box"
    ).toHaveFocus();
  } finally {
    view.unmount();
  }
});

/** A list inside a panel, as the Agents page's Filter holds its Role list (`AgentsPage.tsx`): the
 *  panel's state and the list's are the host's, above the panel, so a pick draws the panel again
 *  and the panel's hand-off for a control a press takes out of it (`useFocusPlace`) runs in the
 *  commit that answers the pick. The options are the roles live sessions hold, then each picked
 *  role none of them holds (`withPicked`), so the picked `ghost-role` is listed only while it is
 *  picked. */
function PanelListHost() {
  const [open, setOpen] = useState(false);
  const [roles, setRoles] = useState<string[]>(["ghost-role"]);
  const live = ["planner", "reviewer"];
  return (
    <PanelTrigger label="Filter" onOpenChange={() => undefined} open stacked>
      {() => (
        <MultiSelect
          emptyMessage="No role matches that."
          label="Role"
          onChange={setRoles}
          onOpenChange={setOpen}
          open={open}
          options={[...live, ...roles.filter((role) => !live.includes(role))]}
          searchLabel="Search roles"
          selected={roles}
          triggerAriaLabel="Filter by role"
        />
      )}
    </PanelTrigger>
  );
}

// INFRA-2160, review 6011312190: Enter on the focused row of a role only the pick holds unpicks it
// and takes the row away inside the panel, whose hand-off then finds the row gone and the focus
// lost by the reader's press, and puts the focus on the control at the row's place: the list's
// trigger. The list's own check runs before the panel's, so the focus goes to its search box and
// the list stays open. No key moves the focus from the search box to a row, since the search
// box's Tab closes the list, so the row is focused as a screen reader's cursor focuses it.
test("a focused row its pick takes off the page, in a list inside a panel, gives the focus to the list's search box and not to its trigger", async () => {
  const user = userEvent.setup();
  const view = render(<PanelListHost />);
  try {
    const trigger = screen.getByRole("button", { name: "Filter by role" });
    expect(trigger, "the panel did not open with the focus on the list's trigger").toHaveFocus();
    await user.keyboard("{Enter}");
    const search = screen.getByRole("combobox", { name: "Search roles" });
    expect(search, "Enter on the trigger did not open the list on its search box").toHaveFocus();
    const ghost = screen.getByRole("option", { name: "ghost-role" });
    act(() => ghost.focus());
    await user.keyboard("{Enter}");
    expect(ghost.isConnected, "the unpicked role's row is still on the page").toBe(false);
    expect(
      search,
      "the pick took the row away, and the focus did not go to the list's search box"
    ).toHaveFocus();
    expect(trigger, "the pick closed the list").toHaveAttribute("aria-expanded", "true");
  } finally {
    view.unmount();
  }
});

// As the commit that answers the press ends, an engine that names a removed element focused until
// its focus fixup still names the row its pick took off the page as focused (`keepRemovedFocus`).
// That focus is lost all the same, and it goes to the search box.
test("where the engine still names a removed row focused, a focused row that its pick takes off the page gives the focus to the search box", async () => {
  const user = userEvent.setup();
  const restore = keepRemovedFocus();
  const view = render(<OpenHost dropPicked options={["api", "bug", "docs"]} />);
  try {
    const search = screen.getByRole("combobox", { name: "Search labels" });
    const bug = screen.getByRole("option", { name: "bug" });
    bug.focus();
    await user.keyboard("{Enter}");
    expect(screen.getByTestId("selected").textContent).toBe("bug");
    expect(bug.isConnected, "the picked row is still on the page").toBe(false);
    expect(
      search,
      "the engine still named the removed row focused, and the focus did not go to the search box"
    ).toHaveFocus();
  } finally {
    view.unmount();
    restore();
  }
});

// Only a press of the row that holds the focus is noted, so a press of a row the focus is not on
// moves no focus, though its pick takes the row off the page. A pointer press focuses nothing
// (`onMouseDown`) and leaves the focus in the search box. A click with no pointer press, which the
// panel's taking of the focus on a pointer press inside it (`AnchoredPanel`) does not see, leaves
// the focus on the page's body, where a focused row that new options took away left it.
test("a press of a row the focus is not on, which its pick takes off the page, leaves the focus where it is", async () => {
  const user = userEvent.setup();
  const view = render(<OpenHost dropPicked options={["api", "bug", "docs"]} />);
  try {
    const search = screen.getByRole("combobox", { name: "Search labels" });
    expect(search).toHaveFocus();
    const bug = screen.getByRole("option", { name: "bug" });
    await user.click(bug);
    expect(screen.getByTestId("selected").textContent).toBe("bug");
    expect(bug.isConnected, "the picked row is still on the page").toBe(false);
    expect(search, "a pointer press of a row took the focus out of the search box").toHaveFocus();

    screen.getByRole("option", { name: "api" }).focus();
    view.rerender(<OpenHost dropPicked options={["bug", "docs"]} />);
    expect(
      document.body,
      "new options took the focused row away, and its focus did not fall to the page's body"
    ).toHaveFocus();
    const docs = screen.getByRole("option", { name: "docs" });
    // A click with no pointer press before it: user-event has no form for it.
    fireEvent.click(docs);
    expect(screen.getByTestId("selected").textContent).toBe("bug,docs");
    expect(docs.isConnected, "the picked row is still on the page").toBe(false);
    expect(
      document.body,
      "a press of a row the focus was not on moved the focus off the page's body"
    ).toHaveFocus();
  } finally {
    view.unmount();
  }
});

// A host can keep the pick as a row is pressed, so the press changes nothing it draws. The press
// of a focused row still asks for a commit of its own, which forgets the press, so the press
// answers no later loss: new options that then take that row away are no press of the reader's,
// and its focus stays on the page's body.
test("a focused row's press that changes nothing drawn answers no later loss: new options that drop the row leave the focus on the page's body", async () => {
  const user = userEvent.setup();
  const picker = (options: string[]) => (
    <MultiSelect
      emptyMessage="Nothing here."
      label="Labels"
      onChange={() => undefined}
      onOpenChange={() => undefined}
      open
      options={options}
      searchLabel="Search labels"
      selected={[]}
    />
  );
  const view = render(picker(["api", "bug"]));
  try {
    const bug = screen.getByRole("option", { name: "bug" });
    bug.focus();
    // The first press makes the row the active one, which the picker draws; the second changes
    // nothing drawn.
    await user.keyboard("{Enter}");
    await user.keyboard("{Enter}");
    expect(bug, "a press of a row that stays on the page took its focus").toHaveFocus();
    view.rerender(picker(["api"]));
    expect(bug.isConnected, "new options did not take the row away").toBe(false);
    expect(
      document.body,
      "new options took the row away, and the focus left the page's body though the reader's press took nothing away"
    ).toHaveFocus();
  } finally {
    view.unmount();
  }
});

// A host can keep where the reader came from onto the trigger, for a way back there, as the
// Inbox's bulk Snooze keeps the row Escape returns to. The list hands the focus back to its
// trigger as it closes - on a pick, Escape, Tab, or a press of the trigger - which is no arrival,
// so the host's record outlives the reader's use of the list.
test("a single pick tells its host where a focus arriving on its trigger came from, and not of the focus its list hands back", async () => {
  const user = userEvent.setup();
  const arrivals: string[] = [];
  const view = render(
    <SingleHost
      onTriggerFocus={(from) => arrivals.push(from?.textContent ?? "nowhere")}
      options={["Lease", "Mirror"]}
    />
  );
  try {
    const trigger = screen.getByRole("button", { name: pickNamed("Epic") });
    act(() => screen.getByRole("button", { name: "Elsewhere" }).focus());
    await user.tab({ shift: true });
    expect(trigger).toHaveFocus();
    expect(arrivals).toEqual(["Elsewhere"]);

    // Escape, Tab and a pick each hand the focus back from the list to the trigger.
    await user.keyboard("{Enter}");
    expect(screen.getByRole("combobox", { name: "Search epics" })).toHaveFocus();
    await user.keyboard("{Escape}");
    expect(trigger).toHaveFocus();
    await user.keyboard("{Enter}");
    await user.tab();
    expect(trigger).toHaveFocus();
    await user.keyboard("{Enter}");
    await user.keyboard("{ArrowDown}{Enter}");
    expect(trigger).toHaveFocus();
    expect(screen.getByTestId("handed").textContent).toBe("Mirror");
    // A press of the open list's trigger closes it.
    await user.click(trigger);
    await user.click(trigger);
    expect(trigger).toHaveAttribute("aria-expanded", "false");
    expect(trigger).toHaveFocus();
    expect(arrivals).toEqual(["Elsewhere"]);

    // A press from outside is an arrival again.
    act(() => screen.getByRole("button", { name: "Elsewhere" }).focus());
    await user.click(trigger);
    expect(arrivals).toEqual(["Elsewhere", "Elsewhere"]);
  } finally {
    view.unmount();
  }
});

// ---------------------------------------------------------------------------------------------
// D5: a single pick in a menu's place (`menu`), dispatch://INFRA-2160's table "The state table
// of a pick that opts into D5", one test to a row and named by it. "The target" is where each
// close gives the focus back: the row a key opened the list from, else the trigger a press opened
// it from. The rows that only a browser can read (the drawer's Escape) are the browser checks'.
// ---------------------------------------------------------------------------------------------

const SNOOZE_LABELS: Readonly<Record<string, string>> = {
  clear: "Until I clear it",
  later: "Later today",
  "next-week": "Next week",
  tomorrow: "Tomorrow",
  unsnooze: "Un-snooze",
};

/** The three menus D5 draws: the snooze's digits, with Un-snooze's `0` held only by a snoozed
 *  item's options; the priority's; and the status's, which has none. */
const MENUS = {
  priority: {
    keys: { "0": "P0", "1": "P1", "2": "P2", "3": "P3" },
    opensOn: "value",
  },
  snooze: {
    keys: { "0": "unsnooze", "1": "later", "2": "tomorrow", "3": "next-week", "4": "clear" },
    opensOn: "first",
  },
  status: { keys: {}, opensOn: "value" },
} as const satisfies Readonly<Record<string, ListMenu>>;

type MenuKind = keyof typeof MENUS;

const LABELS: Readonly<Record<MenuKind, string>> = {
  priority: "Priority",
  snooze: "Snooze",
  status: "Status",
};

function optionsOf(kind: MenuKind, snoozed: boolean): string[] {
  if (kind === "priority") return ["Unset", "P0", "P1", "P2", "P3"];
  // An open issue's: every status but Done, as the issue page passes them.
  if (kind === "status") {
    return [
      "Triage",
      "Icebox",
      "Backlog",
      "Todo",
      "In progress",
      "Testing",
      "Needs review",
      "Retro",
    ];
  }
  return ["later", "tomorrow", "next-week", "clear", ...(snoozed ? ["unsnooze"] : [])];
}

/** A row's `b`, which presses the list's trigger as the page's binding does. */
const pressTriggerOnB = pressesTriggerOn("b", "[data-menu-trigger]");

const MENU_MARKS: ListMarks = { trigger: { "data-menu-trigger": "" } };

/** A pick that takes D5's rules as a host draws one: a row the reader is on, whose `b` opens the
 *  list; the pick, a snooze's, a priority's or a status's, on its own options unless `options`
 *  names others; every value it handed its host; what closed it each time; another control, which
 *  counts its own presses; and words on the page, which take no focus. */
function MenuHost({
  ariaDisabled,
  kind = "snooze",
  options,
  snoozed = false,
  value,
}: {
  ariaDisabled?: boolean;
  kind?: MenuKind;
  options?: string[];
  snoozed?: boolean;
  value?: string;
}) {
  const [open, setOpen] = useState(false);
  const [handed, setHanded] = useState<string[]>([]);
  const [closings, setClosings] = useState<string[]>([]);
  const [presses, setPresses] = useState(0);
  return (
    <>
      <button onKeyDown={pressTriggerOnB} type="button">
        Row
      </button>
      <MultiSelect
        ariaDisabled={ariaDisabled}
        emptyMessage="Nothing here."
        label={LABELS[kind]}
        marks={MENU_MARKS}
        menu={MENUS[kind]}
        mode="single"
        onOpenChange={(next, closedBy) => {
          setOpen(next);
          if (!next) setClosings((before) => [...before, closedBy ?? "nothing"]);
        }}
        onPick={(picked) => setHanded((before) => [...before, picked])}
        open={open}
        optionLabel={kind === "snooze" ? (option) => SNOOZE_LABELS[option] : undefined}
        options={options ?? optionsOf(kind, snoozed)}
        searchLabel={`Search ${LABELS[kind].toLocaleLowerCase()}`}
        value={value}
      />
      <output data-testid="handed">{handed.join(",")}</output>
      <output data-testid="closings">{closings.join(",")}</output>
      <button onClick={() => setPresses((before) => before + 1)} type="button">
        Other
      </button>
      <output data-testid="presses">{presses}</output>
      <p>Words on the page</p>
    </>
  );
}

/** The open list's text box. */
function menuBox(): HTMLInputElement {
  return screen.getByRole("combobox") as HTMLInputElement;
}

function handed(): string {
  return screen.getByTestId("handed").textContent ?? "";
}

function menuTrigger(kind: MenuKind = "snooze"): HTMLElement {
  return screen.getByRole("button", { name: pickNamed(LABELS[kind]) });
}

/** Opens the list by the row's `b`, with the focus on the row; answers the row. */
async function openFromRow(user: UserEvent, name = "Row"): Promise<HTMLElement> {
  const row = screen.getByRole("button", { name });
  act(() => row.focus());
  await user.keyboard("b");
  return row;
}

/** A finger's tap on `target`. */
function tap(user: UserEvent, target: Element): Promise<void> {
  return user.pointer({ keys: "[TouchA]", target });
}

test("an opener opens the list on its first or current choice", async () => {
  const user = userEvent.setup();
  // The snooze opens on its first choice whatever the pick holds, the priority on the value it
  // holds.
  for (const [kind, value, first] of [
    ["snooze", "next-week", "Later today"],
    ["priority", "P2", "P2"],
  ] as const) {
    const view = render(<MenuHost kind={kind} value={value} />);
    try {
      const trigger = menuTrigger(kind);
      const openers: readonly (readonly [string, () => Promise<void>])[] = [
        ["a key from a row", () => openFromRow(user).then(() => undefined)],
        [
          "Enter on the trigger",
          async () => {
            act(() => trigger.focus());
            await user.keyboard("{Enter}");
          },
        ],
        [
          "Space on the trigger",
          async () => {
            act(() => trigger.focus());
            await user.keyboard(" ");
          },
        ],
        ["a click", () => user.click(trigger)],
        ["a tap", () => tap(user, trigger)],
      ];
      for (const [opener, press] of openers) {
        await press();
        expect(menuBox(), `${kind}: ${opener} puts the focus in the list's box`).toHaveFocus();
        expect(highlightedRow(menuBox()), `${kind}: ${opener} highlights ${first}`).toBe(first);
        await user.keyboard("{Escape}");
        expect(screen.queryByRole("listbox"), `${kind}: ${opener}`).not.toBeInTheDocument();
      }
      expect(handed()).toBe("");
    } finally {
      view.unmount();
    }
  }
});

// R4, R5 (the tester's round 16): a menu's trigger is a menu's button, not a select. A character
// typed on it while its menu is closed is the page's key, which the keymap runs as on the row
// (`typedOnMenuTrigger`): `b` there opens the Snooze menu of the item in hand, on its first choice
// with nothing typed, and `p` the priority menu on its value. So the trigger opens no list on it,
// types nothing into its box and leaves the key to the page. Enter, Space, ArrowDown and ArrowUp,
// with Alt or without, still open its own menu on its value.
test("a character typed on the closed trigger is the page's key: it opens no list, types nothing and reaches the page, while Enter, Space and the arrows open the list", async () => {
  const user = userEvent.setup();
  const view = render(<MenuHost kind="priority" value="P2" />);
  const page = keysThePageReads();
  try {
    const trigger = menuTrigger("priority");
    for (const key of ["p", "b", "s", "2"]) {
      act(() => trigger.focus());
      await user.keyboard(key);
      expect(screen.queryByRole("listbox"), `${key} opens no list`).not.toBeInTheDocument();
      expect(trigger, key).toHaveFocus();
    }
    expect(page.keys, "every character reached the page").toEqual(["p", "b", "s", "2"]);
    expect(trigger.matches(MENU_TRIGGER_SELECTOR), "the trigger carries a menu's mark").toBe(true);
    page.stop();
    for (const key of ["{Enter}", " ", "{ArrowDown}", "{ArrowUp}", "{Alt>}{ArrowDown}{/Alt}"]) {
      act(() => trigger.focus());
      await user.keyboard(key);
      expect(menuBox(), `${key} opens the list with nothing typed`).toHaveValue("");
      expect(highlightedRow(menuBox()), `${key} opens it on the value`).toBe("P2");
      await user.keyboard("{Escape}");
    }
    expect(handed()).toBe("");
  } finally {
    page.stop();
    view.unmount();
  }
});

test("on a coarse pointer the list opens with no focused box", async () => {
  const user = userEvent.setup();
  const restore = onCoarsePointer();
  const view = render(<MenuHost />);
  try {
    await tap(user, menuTrigger());
    const list = screen.getByRole("listbox", { name: "Snooze options" });
    expect(menuBox(), "the tap opened the phone's keyboard over the choices").not.toHaveFocus();
    expect(list).toHaveFocus();
    expect(highlightedRow(menuBox())).toBe("Later today");
    // The list's own keys still work on it.
    await user.keyboard("{ArrowDown}");
    expect(highlightedRow(menuBox())).toBe("Tomorrow");
    await user.keyboard("{Escape}");
    expect(menuTrigger()).toHaveFocus();

    // Whatever opened it: a key from the row too.
    await openFromRow(user);
    expect(menuBox()).not.toHaveFocus();
    expect(screen.getByRole("listbox", { name: "Snooze options" })).toHaveFocus();
    expect(handed()).toBe("");
  } finally {
    view.unmount();
    restore();
  }
});

test("a tap on the box focuses it", async () => {
  const user = userEvent.setup();
  const restore = onCoarsePointer();
  const view = render(<MenuHost />);
  try {
    await tap(user, menuTrigger());
    const box = menuBox();
    expect(box).not.toHaveFocus();
    await tap(user, box);
    expect(box).toHaveFocus();
    expect(screen.getByRole("listbox", { name: "Snooze options" })).toBeInTheDocument();
    await user.keyboard("to");
    expect(screen.getAllByRole("option").map((option) => option.getAttribute("id"))).toEqual([
      screen.getByRole("option", { name: "Later today" }).id,
      screen.getByRole("option", { name: "Tomorrow" }).id,
    ]);
    expect(handed()).toBe("");
  } finally {
    view.unmount();
    restore();
  }
});

// happy-dom lays nothing out, so the row reads what places the panel and sizes its rows: the
// panel's left edge, which `AnchoredPanel` keeps 8 px inside a 390 px window for a trigger at its
// right edge, and its 256 px width (`w-64`, no wider than the window less 1rem); each row's
// 44 px floor (`min-h-11`) with no breakpoint's class over it; and what draws each choice the
// panel shows where a tap reaches it (`ListMenu`). The panel takes no height cap of its own
// (`max-h-none`), only the room `AnchoredPanel` leaves it beside its trigger; the foot line
// follows the choices rather than sticking over the last of them; and the list takes the focus
// with no scroll, so where the panel stands scrolled is the highlight's alone. Under the 384 px
// cap the status's eight choices did not fit with the text box's 61 px row and the foot line's
// two lines, and the engine's scroll of the focused list put Triage under the box and Retro under
// the foot line (the tester's round 16). The browser check k-phone measures the same rectangles
// in a phone's window, and taps the centre of each of the status's choices.
test("on a phone every choice is inside the viewport and 44 px high", async () => {
  const user = userEvent.setup();
  const restore = onCoarsePointer();
  const width = Object.getOwnPropertyDescriptor(window, "innerWidth");
  const height = Object.getOwnPropertyDescriptor(window, "innerHeight");
  Object.defineProperty(window, "innerWidth", { configurable: true, value: 390 });
  Object.defineProperty(window, "innerHeight", { configurable: true, value: 664 });
  const view = render(<MenuHost snoozed />);
  const focus = spyOn(HTMLElement.prototype, "focus");
  try {
    for (const [kind, value, count] of [
      ["snooze", undefined, 5],
      ["priority", "P2", 5],
      ["status", "Todo", 8],
    ] as const) {
      view.rerender(<MenuHost kind={kind} snoozed value={value} />);
      const trigger = menuTrigger(kind);
      trigger.getBoundingClientRect = () => new DOMRect(330, 300, 52, 44);
      const from = focus.mock.calls.length;
      await tap(user, trigger);
      const panel = panelOf(trigger);
      const left = Number.parseFloat(panel.style.left);
      expect(left, `${kind}: the panel's left edge`).toBeGreaterThanOrEqual(8);
      expect(left + 256, `${kind}: the panel's right edge`).toBeLessThanOrEqual(390 - 8);
      expect(panel.className.split(" ")).toEqual(
        expect.arrayContaining(["w-64", "max-w-[calc(100vw-1rem)]"])
      );
      const options = screen.getAllByRole("option");
      expect(options).toHaveLength(count);
      for (const option of options) {
        const classes = option.className.split(" ");
        expect(classes, `${kind}: ${option.textContent}`).toEqual(
          expect.arrayContaining(["min-h-11", "w-full"])
        );
        expect(classes.filter((token) => /^[\w-]+:(?:min-)?h-/.test(token))).toEqual([]);
      }
      expect(
        panel.className.split(" ").filter((token) => token.startsWith("max-h-")),
        `${kind}: the panel's height cap`
      ).toEqual(["max-h-none"]);
      const foot = within(panel).getByText(/to choose · Esc to close$/);
      expect(foot.className.split(" "), `${kind}: the foot line follows the choices`).not.toContain(
        "sticky"
      );
      const list = within(panel).getByRole("listbox");
      expect(list).toHaveFocus();
      const listFocus = focus.mock.calls.flatMap(([options], index) =>
        index >= from && focus.mock.contexts[index] === list ? [options] : []
      );
      expect(listFocus, `${kind}: the list takes the focus with no scroll`).toEqual([
        { preventScroll: true },
      ]);
      await user.keyboard("{Escape}");
    }
    expect(handed()).toBe("");
  } finally {
    focus.mockRestore();
    view.unmount();
    restore();
    for (const [name, descriptor] of [
      ["innerWidth", width],
      ["innerHeight", height],
    ] as const) {
      if (descriptor === undefined) Reflect.deleteProperty(window, name);
      else Object.defineProperty(window, name, descriptor);
    }
  }
});

test("the arrows, Home and End move the highlight and write nothing", async () => {
  const user = userEvent.setup();
  const view = render(<MenuHost />);
  try {
    await user.click(menuTrigger());
    for (const [key, to] of [
      ["ArrowDown", "Tomorrow"],
      ["ArrowUp", "Later today"],
      ["End", "Until I clear it"],
      ["Home", "Later today"],
      ["ArrowUp", "Until I clear it"],
    ] as const) {
      await user.keyboard(`{${key}}`);
      expect(highlightedRow(menuBox()), `${key} highlights ${to}`).toBe(to);
      expect(menuBox(), key).toHaveFocus();
    }
    // The caret is the text box's: ArrowLeft and ArrowRight move it, and Home and End, which are
    // the list's, leave it.
    await user.keyboard("ek");
    const box = menuBox();
    expect(box.value).toBe("ek");
    await user.keyboard("{ArrowLeft}");
    expect(box.selectionStart).toBe(1);
    await user.keyboard("{ArrowRight}");
    expect(box.selectionStart).toBe(2);
    await user.keyboard("{Home}");
    expect(box.selectionStart).toBe(2);
    expect(highlightedRow(menuBox())).toBe("Next week");
    expect(screen.getByRole("listbox", { name: "Snooze options" })).toBeInTheDocument();
    expect(handed()).toBe("");
  } finally {
    view.unmount();
  }
});

/** The status's choices, after five more that a change of the menu's choices puts before them. */
const MORE_CHOICES = [
  "Draft",
  "Proposed",
  "Approved",
  "Planned",
  "Ready",
  ...optionsOf("status", false),
];

/**
 * The status menu of an issue in Needs review, laid out as `layOutList` lays a list out from the
 * open's first measure (`layOutListsAsOpened`), with what watches its box. Its box shows 286 px
 * of what it holds, and the rows stand whole in it between the text box and the line of keys
 * stuck to its bottom, at 61-225 px of its content; Needs review stands at 325-369 among the
 * eight choices, so the open scrolls the box to 144. On a coarse pointer the line of keys follows
 * the choices, and the rows stand whole down to the box's bottom, at 61-286 px. `cut` cuts the
 * box of the open list to 264 px, as `AnchoredPanel` cuts a panel the window has too little room
 * for, and tells what watches it of it; `rerender` draws the menu again, on `options` where named.
 */
function keptStatusMenu(): {
  readonly box: () => HTMLElement | null;
  readonly cut: () => void;
  readonly rerender: (options?: string[]) => void;
  readonly restore: () => void;
} {
  const sizes = installResizeObserver();
  const host = (options?: string[]) => (
    <MenuHost kind="status" options={options} value="Needs review" />
  );
  const view = render(host());
  const lists = layOutListsAsOpened(menuTrigger("status"));
  return {
    box: lists.box,
    cut: () => {
      const box = lists.box();
      if (box === null) throw new Error("the list was never laid out");
      Object.defineProperty(box, "clientHeight", { configurable: true, get: () => 264 });
      box.getBoundingClientRect = () => new DOMRect(0, 100, 256, 266);
      sizes.resize(box);
    },
    rerender: (options) => view.rerender(host(options)),
    restore: () => {
      lists.restore();
      view.unmount();
      sizes.restore();
    },
  };
}

// A menu keeps the row it opened on shown whole while its box or its choices change, as every
// single pick keeps the option it opened on, until the reader moves on in the list. A digit is
// the reader's move (R5: a menu's digit writes its choice). The status has no digit, so `1` writes
// nothing and the list stays open, and a cut of its box or a change of its choices after it leaves
// the list where it is: cut at 144, the box shows the rows whole at 205-347 px of its content,
// above the line of keys, and Needs review ends below that.
test("a menu's digit ends the keep of the row it opened on, so a change of its box or its choices after it leaves the list where it is", async () => {
  const user = userEvent.setup();
  const menu = keptStatusMenu();
  try {
    // The row the menu highlights at each moment, and how far its box has scrolled then.
    const seen: [string, string | undefined, number | undefined][] = [];
    const shown = (moment: string) =>
      seen.push([moment, highlightedRow(menuBox()), menu.box()?.scrollTop]);
    await user.click(menuTrigger("status"));
    shown("opened");
    menu.cut();
    shown("the box cut");

    await user.keyboard("{Escape}");
    await user.click(menuTrigger("status"));
    await user.keyboard("1");
    menu.cut();
    shown("1, then the box cut");
    menu.rerender(MORE_CHOICES);
    shown("1, then five more choices");

    expect(seen).toEqual([
      ["opened", "Needs review", 144],
      ["the box cut", "Needs review", 166],
      ["1, then the box cut", "Needs review", 144],
      ["1, then five more choices", "Needs review", 144],
    ]);
    expect(handed()).toBe("");
  } finally {
    menu.restore();
  }
});

// Home and End move a menu's highlight (R5), and the reader on from the row it opened on: after
// either, a change of the menu's choices or of its box leaves the list where it is, even where
// that moves the row they highlighted out of the box's view. End shows Retro whole at the end of
// the scroll, 188, and Home Triage at the top.
test("Home and End end the keep of the row a menu opened on, so a change of its choices or its box after them leaves the list where it is", async () => {
  const user = userEvent.setup();
  const menu = keptStatusMenu();
  try {
    const seen: [string, string | undefined, number | undefined][] = [];
    const shown = (moment: string) =>
      seen.push([moment, highlightedRow(menuBox()), menu.box()?.scrollTop]);
    await user.click(menuTrigger("status"));
    shown("opened");
    await user.keyboard("{End}");
    shown("End");
    menu.rerender(MORE_CHOICES);
    shown("End, then five more choices");
    menu.cut();
    shown("End, then the box cut");

    await user.keyboard("{Escape}");
    menu.rerender();
    await user.click(menuTrigger("status"));
    await user.keyboard("{Home}");
    shown("Home");
    menu.rerender(MORE_CHOICES);
    shown("Home, then five more choices");

    expect(seen).toEqual([
      ["opened", "Needs review", 144],
      ["End", "Retro", 188],
      ["End, then five more choices", "Retro", 188],
      ["End, then the box cut", "Retro", 188],
      ["Home", "Triage", 0],
      ["Home, then five more choices", "Triage", 0],
    ]);
    expect(handed()).toBe("");
  } finally {
    menu.restore();
  }
});

// On a phone a tap on a choice lands on the choice it is aimed at (R15, the tester's round 16):
// the press on it ends the keep of the row the menu opened on, so a cut of the box under the
// finger or a change of the choices scrolls no other choice under it before the press is over.
// The focus the open gives the list on a coarse pointer is the open's own and no move of the
// reader's, and the keep holds through it. The open shows Needs review whole above the box's
// bottom, at 83; cut, the box shows the rows whole at 144-347 px of its content, and Needs review
// ends below that.
test("on a phone a press on a choice ends the keep of the row the menu opened on, and the focus the open gives its list does not", async () => {
  const user = userEvent.setup();
  const restore = onCoarsePointer();
  const menu = keptStatusMenu();
  try {
    const seen: [string, string | undefined, number | undefined][] = [];
    const shown = (moment: string) =>
      seen.push([moment, highlightedRow(menuBox()), menu.box()?.scrollTop]);
    await tap(user, menuTrigger("status"));
    expect(screen.getByRole("listbox", { name: "Status options" })).toHaveFocus();
    shown("opened");
    menu.cut();
    shown("the box cut");

    await user.keyboard("{Escape}");
    await tap(user, menuTrigger("status"));
    // The finger down on Testing, not lifted yet.
    await user.pointer({
      keys: "[TouchA>]",
      target: screen.getByRole("option", { name: "Testing" }),
    });
    shown("a press on Testing");
    menu.cut();
    shown("a press on Testing, then the box cut");
    menu.rerender(MORE_CHOICES);
    shown("a press on Testing, then five more choices");

    expect(seen).toEqual([
      ["opened", "Needs review", 83],
      ["the box cut", "Needs review", 105],
      ["a press on Testing", "Needs review", 83],
      ["a press on Testing, then the box cut", "Needs review", 83],
      ["a press on Testing, then five more choices", "Needs review", 83],
    ]);
    expect(handed()).toBe("");
  } finally {
    menu.restore();
    restore();
  }
});

test("a letter narrows the list", async () => {
  const user = userEvent.setup();
  const view = render(<MenuHost />);
  try {
    const row = await openFromRow(user);
    await user.keyboard("to");
    expect(menuBox().value).toBe("to");
    expect(screen.getAllByRole("option").map((option) => option.id)).toEqual([
      screen.getByRole("option", { name: "Later today" }).id,
      screen.getByRole("option", { name: "Tomorrow" }).id,
    ]);
    expect(menuBox()).toHaveFocus();
    expect(row).not.toHaveFocus();
    expect(handed()).toBe("");
  } finally {
    view.unmount();
  }
});

// R5, R8 (the tester's round 16, the class of T15), and the held-key rule the architect ruled on
// 2026-11-25T04:12Z, which `KeymapProvider` binds (`Keymap.captureKeyDown`): a held key acts once
// but for the steps that repeat and the typing in a field that had the focus before its press. The
// page's binding opens the menu on the key's press, and the key's repeats, which then reach the
// menu's box, type none of themselves there: a held `b` opens the Snooze menu with nothing typed
// and every choice shown. A held arrow walks the highlight, as a move that writes nothing, and a
// held letter typed in the box, which had the focus before its press, types on each repeat.
test("a held key in the list: the opener's repeats type nothing, a held arrow walks the highlight, and a held letter types on each repeat", async () => {
  const user = userEvent.setup();
  const view = render(
    <KeymapProvider>
      <MenuHost />
    </KeymapProvider>
  );
  try {
    const row = screen.getByRole("button", { name: "Row" });
    act(() => row.focus());
    await user.keyboard("{b>6}");
    expect(menuBox()).toHaveFocus();
    expect(menuBox(), "the held b's repeats type nothing").toHaveValue("");
    expect(screen.getAllByRole("option"), "the held b shows every choice").toHaveLength(4);
    expect(highlightedRow(menuBox())).toBe("Later today");
    await user.keyboard("{ArrowDown>3}");
    expect(highlightedRow(menuBox()), "a held arrow walks the highlight").toBe("Until I clear it");
    await user.keyboard("{w>4}");
    expect(menuBox(), "a held letter types on each repeat").toHaveValue("wwww");
    expect(handed()).toBe("");
  } finally {
    view.unmount();
  }
});

test("a digit writes its own choice whatever the narrowing", async () => {
  const user = userEvent.setup();
  const view = render(<MenuHost />);
  try {
    // Narrowed to Next week, `2` still writes Tomorrow, which the narrowing hides.
    const row = await openFromRow(user);
    await user.keyboard("next");
    expect(screen.getAllByRole("option")).toHaveLength(1);
    await user.keyboard("2");
    expect(handed()).toBe("tomorrow");
    expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
    expect(row).toHaveFocus();

    await openFromRow(user);
    await user.keyboard("4");
    expect(handed()).toBe("tomorrow,clear");
    expect(row).toHaveFocus();

    // A snoozed item's list holds Un-snooze, which `0` writes.
    view.rerender(<MenuHost snoozed />);
    await openFromRow(user);
    await user.keyboard("0");
    expect(handed()).toBe("tomorrow,clear,unsnooze");
    expect(row).toHaveFocus();
    expect(screen.getByTestId("closings").textContent).toBe("pick,pick,pick");
  } finally {
    view.unmount();
  }
});

// Safari sends a key typed while an input method composes after the composition's end, unmarked,
// at keyCode 229 (WebKit bug 165004): a digit typed so is the input method's text, not a choice.
test("a digit that ends an IME composition in Safari, at keyCode 229, writes no choice", async () => {
  const user = userEvent.setup();
  const view = render(<MenuHost />);
  try {
    await openFromRow(user);
    // user-event cannot mark a key as an input method's, so the key is fired with keyCode 229.
    fireEvent.keyDown(menuBox(), { isComposing: false, key: "2", keyCode: 229 });
    expect(handed()).toBe("");
    expect(screen.queryByRole("listbox")).not.toBeNull();
    expect(menuBox()).toHaveFocus();

    await user.keyboard("2");
    expect(handed()).toBe("tomorrow");
  } finally {
    view.unmount();
  }
});

test("a digit with no choice writes nothing", async () => {
  const user = userEvent.setup();
  const view = render(<MenuHost />);
  try {
    await user.click(menuTrigger());
    // `0` is Un-snooze's, which the list of an item not snoozed does not hold; `7` names no
    // choice. Neither is typed into the box either.
    for (const digit of ["0", "7"]) {
      await user.keyboard(digit);
      expect(screen.getByRole("listbox", { name: "Snooze options" }), digit).toBeInTheDocument();
      expect(menuBox(), digit).toHaveFocus();
      expect(menuBox().value, digit).toBe("");
      expect(screen.getAllByRole("option"), digit).toHaveLength(4);
    }
    expect(handed()).toBe("");
  } finally {
    view.unmount();
  }
});

test("Enter writes the highlighted choice", async () => {
  const user = userEvent.setup();
  const view = render(<MenuHost kind="priority" value="P2" />);
  try {
    const row = await openFromRow(user);
    await user.keyboard("{ArrowUp}{Enter}");
    expect(handed()).toBe("P1");
    expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
    expect(row).toHaveFocus();
  } finally {
    view.unmount();
  }
});

test("a click on a choice writes it", async () => {
  const user = userEvent.setup();
  const view = render(<MenuHost />);
  try {
    const row = await openFromRow(user);
    await user.click(screen.getByRole("option", { name: "Next week" }));
    expect(handed()).toBe("next-week");
    expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
    expect(row).toHaveFocus();

    // A tap writes the choice it hits as a click does.
    await openFromRow(user);
    await tap(user, screen.getByRole("option", { name: "Tomorrow" }));
    expect(handed()).toBe("next-week,tomorrow");
    expect(row).toHaveFocus();

    // From a press of the trigger the target is the trigger.
    await user.click(menuTrigger());
    await user.click(screen.getByRole("option", { name: "Later today" }));
    expect(handed()).toBe("next-week,tomorrow,later");
    expect(menuTrigger()).toHaveFocus();
  } finally {
    view.unmount();
  }
});

test("Escape writes nothing and gives the focus back", async () => {
  const user = userEvent.setup();
  const view = render(<MenuHost />);
  try {
    const row = await openFromRow(user);
    await user.keyboard("{ArrowDown}{Escape}");
    expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
    expect(row).toHaveFocus();

    await user.click(menuTrigger());
    await user.keyboard("{Escape}");
    expect(menuTrigger()).toHaveFocus();

    // A key pressed with nothing focused opened it from the page itself, which gets it back.
    act(() => (document.activeElement as HTMLElement | null)?.blur());
    act(() => menuTrigger().click());
    expect(menuBox()).toHaveFocus();
    await user.keyboard("{Escape}");
    expect(document.body).toHaveFocus();

    expect(handed()).toBe("");
    expect(screen.getByTestId("closings").textContent).toBe("escape,escape,escape");
  } finally {
    view.unmount();
  }
});

test("Tab and Shift+Tab write nothing and give the focus back", async () => {
  const user = userEvent.setup();
  const view = render(<MenuHost />);
  try {
    for (const shift of [false, true]) {
      const row = await openFromRow(user);
      await user.keyboard("{ArrowDown}");
      await user.tab({ shift });
      expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
      expect(row, shift ? "Shift+Tab" : "Tab").toHaveFocus();
    }
    expect(handed()).toBe("");
    expect(screen.getByTestId("closings").textContent).toBe("tab,tab");
  } finally {
    view.unmount();
  }
});

test("a press on another control closes the list and focuses that control", async () => {
  const user = userEvent.setup();
  const view = render(<MenuHost />);
  const other = screen.getByRole("button", { name: "Other" });
  // Where the focus is as the browser's own focus on the press comes, at its `mousedown`.
  const seen: { atMouseDown: Element | null } = { atMouseDown: null };
  const read = () => {
    seen.atMouseDown = document.activeElement;
  };
  other.addEventListener("mousedown", read);
  try {
    const row = await openFromRow(user);
    await user.click(other);
    expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
    expect(seen.atMouseDown, "the close gave the focus back before the press's own").toBe(row);
    expect(other).toHaveFocus();
    expect(screen.getByTestId("presses").textContent).toBe("1");
    expect(handed()).toBe("");
    expect(screen.getByTestId("closings").textContent).toBe("press");
  } finally {
    other.removeEventListener("mousedown", read);
    view.unmount();
  }
});

test("a press on the page gives the focus back", async () => {
  const user = userEvent.setup();
  const view = render(<MenuHost />);
  try {
    const words = screen.getByText("Words on the page");
    const row = await openFromRow(user);
    await user.click(words);
    expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
    expect(row).toHaveFocus();

    await openFromRow(user);
    await tap(user, words);
    expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
    expect(row).toHaveFocus();

    await user.click(menuTrigger());
    await user.click(words);
    expect(menuTrigger()).toHaveFocus();
    expect(handed()).toBe("");
    expect(screen.getByTestId("closings").textContent).toBe("press,press,press");
  } finally {
    view.unmount();
  }
});

// Plan v15 D5 and the decision "a press on a text box, a button or a link then focuses that
// control and acts on one press" (the tester's round 16): the focus a press outside gives back, at
// its `pointerdown` and again once it is over, comes with no scroll. A scroll then would move the
// page under the press between its `mousedown` and its `mouseup`, so in card mode a press far down
// the page, below the card whose `b` opened the list, would release on another element and not
// act. Escape and Tab give the focus back as the engine focuses, which brings the element into
// view where it is out of sight.
test("a press outside gives the focus back with no scroll, and Escape and Tab as the engine focuses", async () => {
  const user = userEvent.setup();
  const view = render(<MenuHost />);
  const row = screen.getByRole("button", { name: "Row" });
  const focus = spyOn(row, "focus");
  // How each focus given to the row since the list opened asked to scroll.
  const givenBack = async (close: () => Promise<void>) => {
    await openFromRow(user);
    const from = focus.mock.calls.length;
    await close();
    return focus.mock.calls.slice(from).map(([options]) => options);
  };
  try {
    const noScroll = { preventScroll: true };
    expect(
      await givenBack(() => user.click(screen.getByRole("button", { name: "Other" })))
    ).toEqual([noScroll]);
    expect(await givenBack(() => user.click(screen.getByText("Words on the page")))).toEqual([
      noScroll,
      noScroll,
    ]);
    for (const keys of ["{Escape}", "{Tab}"]) {
      expect(await givenBack(() => user.keyboard(keys)), keys).toEqual([undefined]);
      expect(row, keys).toHaveFocus();
    }
    expect(handed()).toBe("");
  } finally {
    focus.mockRestore();
    view.unmount();
  }
});

test("a pick while a write is out writes nothing", async () => {
  const user = userEvent.setup();
  const view = render(<MenuHost />);
  try {
    await user.click(menuTrigger());
    // The item's write goes out from elsewhere while its list stands open.
    view.rerender(<MenuHost ariaDisabled />);
    for (const [pick, press] of [
      ["a digit", () => user.keyboard("2")],
      ["Enter", () => user.keyboard("{Enter}")],
      ["a click", () => user.click(screen.getByRole("option", { name: "Next week" }))],
    ] as const) {
      await press();
      expect(screen.getByRole("listbox", { name: "Snooze options" }), pick).toBeInTheDocument();
      expect(menuBox(), pick).toHaveFocus();
    }
    expect(handed()).toBe("");
  } finally {
    view.unmount();
  }
});

test("a trigger whose write is out opens nothing", async () => {
  const user = userEvent.setup();
  const view = render(<MenuHost ariaDisabled />);
  try {
    const row = screen.getByRole("button", { name: "Row" });
    const trigger = menuTrigger();
    const onTrigger = (keys: string) => async () => {
      act(() => trigger.focus());
      await user.keyboard(keys);
    };
    for (const [opener, press, stays] of [
      ["a key from a row", () => openFromRow(user), row],
      ["a click", () => user.click(trigger), trigger],
      ["a tap", () => tap(user, trigger), trigger],
      ["Enter", onTrigger("{Enter}"), trigger],
      ["Space", onTrigger(" "), trigger],
      ["ArrowDown", onTrigger("{ArrowDown}"), trigger],
      ["a letter", onTrigger("t"), trigger],
    ] as const) {
      await press();
      expect(screen.queryByRole("listbox"), opener).not.toBeInTheDocument();
      expect(trigger, opener).toHaveAttribute("aria-expanded", "false");
      expect(stays, opener).toHaveFocus();
    }
    expect(handed()).toBe("");
  } finally {
    view.unmount();
  }
});

/** A host whose pick is a write, out until the test settles it (`writes`, the settle of each
 *  write, `true` where the server takes it), as the Inbox's and the issue page's are: a snooze
 *  that lands takes the row it was for out of the list and hands the focus to the row after it,
 *  R8's hand-off; a priority that lands is drawn as the value; a refused write shows its refusal.
 *  The list's trigger stands outside the rows, as the reading pane's head's does, and each row's
 *  `b` opens it. */
function WriteHost({
  kind,
  writes,
}: {
  kind: "priority" | "snooze";
  writes: ((taken: boolean) => void)[];
}) {
  const [rows, setRows] = useState(["ask-a", "ask-b"]);
  const [value, setValue] = useState<string | undefined>(kind === "priority" ? "P2" : undefined);
  const [refusal, setRefusal] = useState<string | null>(null);
  const [open, setOpen] = useState(false);
  return (
    <>
      {rows.map((row) => (
        <button data-row={row} key={row} onKeyDown={pressTriggerOnB} type="button">
          {row}
        </button>
      ))}
      <MultiSelect
        emptyMessage="Nothing here."
        label={LABELS[kind]}
        marks={MENU_MARKS}
        menu={MENUS[kind]}
        mode="single"
        onOpenChange={setOpen}
        onPick={(picked) => {
          writes.push((taken) => {
            if (!taken) {
              setRefusal(`The server refused ${picked}.`);
            } else if (kind === "priority") {
              setValue(picked);
            } else {
              flushSync(() => setRows(["ask-b"]));
              document.querySelector<HTMLElement>('[data-row="ask-b"]')?.focus();
            }
          });
        }}
        open={open}
        optionLabel={kind === "snooze" ? (option) => SNOOZE_LABELS[option] : undefined}
        options={optionsOf(kind, false)}
        searchLabel={`Search ${LABELS[kind].toLocaleLowerCase()}`}
        value={value}
      />
      {refusal === null ? null : <p role="alert">{refusal}</p>}
    </>
  );
}

test("a snooze that lands hands the focus to the next row", async () => {
  const user = userEvent.setup();
  const writes: ((taken: boolean) => void)[] = [];
  const view = render(<WriteHost kind="snooze" writes={writes} />);
  try {
    const row = await openFromRow(user, "ask-a");
    await user.keyboard("1");
    expect(writes).toHaveLength(1);
    // While the write is out the reader is back on the row.
    expect(row).toHaveFocus();
    act(() => writes[0]?.(true));
    expect(row.isConnected).toBe(false);
    expect(screen.getByRole("button", { name: "ask-b" })).toHaveFocus();
    expect(writes).toHaveLength(1);
  } finally {
    view.unmount();
  }
});

test("a priority that lands keeps the focus on the target", async () => {
  const user = userEvent.setup();
  const writes: ((taken: boolean) => void)[] = [];
  const view = render(<WriteHost kind="priority" writes={writes} />);
  try {
    const row = await openFromRow(user, "ask-a");
    await user.keyboard("1");
    expect(row).toHaveFocus();
    act(() => writes[0]?.(true));
    expect(menuTrigger("priority")).toHaveTextContent("P1");
    expect(row).toHaveFocus();
    expect(writes).toHaveLength(1);
  } finally {
    view.unmount();
  }
});

test("a refused pick keeps the focus and shows the refusal", async () => {
  const user = userEvent.setup();
  const writes: ((taken: boolean) => void)[] = [];
  const view = render(<WriteHost kind="priority" writes={writes} />);
  try {
    const row = await openFromRow(user, "ask-a");
    await user.keyboard("{ArrowDown}{Enter}");
    expect(row).toHaveFocus();
    act(() => writes[0]?.(false));
    expect(screen.getByRole("alert")).toHaveTextContent("The server refused P3.");
    expect(menuTrigger("priority")).toHaveTextContent("P2");
    expect(row).toHaveFocus();
    expect(writes).toHaveLength(1);
  } finally {
    view.unmount();
  }
});

test("the list shows its keys", async () => {
  const user = userEvent.setup();
  const view = render(<MenuHost />);
  try {
    for (const [props, keys, digits] of [
      [{ kind: "snooze" }, "1–4", { "Later today": "1", Tomorrow: "2", "Until I clear it": "4" }],
      [{ kind: "snooze", snoozed: true }, "0–4", { "Un-snooze": "0", "Next week": "3" }],
      [{ kind: "priority", value: "P2" }, "0–3", { P0: "0", P3: "3", Unset: undefined }],
      [{ kind: "status", value: "Todo" }, undefined, { Retro: undefined, Todo: undefined }],
    ] as const) {
      view.rerender(<MenuHost {...props} />);
      await user.click(menuTrigger(props.kind));
      const foot = screen.getByText(
        `↑ ↓ Home End to move · Enter${keys === undefined ? "" : ` or ${keys}`} to choose · Esc to close`
      );
      expect(foot, `${props.kind}'s foot line`).toHaveAttribute("aria-hidden", "true");
      for (const [name, digit] of Object.entries(digits)) {
        // The option's name stays its words; its digit is drawn for the eye alone and named
        // to assistive technology as the option's key.
        const option = screen.getByRole("option", { name });
        const drawn = option.querySelector("kbd");
        if (digit === undefined) {
          expect(drawn, `${name} draws no digit`).toBeNull();
          expect(option).not.toHaveAttribute("aria-keyshortcuts");
          continue;
        }
        expect(drawn?.textContent, `${name}'s digit`).toBe(digit);
        expect(drawn?.closest('[aria-hidden="true"]'), `${name}'s digit is hidden`).not.toBeNull();
        expect(option).toHaveAttribute("aria-keyshortcuts", digit);
      }
      await user.keyboard("{Escape}");
    }
  } finally {
    view.unmount();
  }
});
