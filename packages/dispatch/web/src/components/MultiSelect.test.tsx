import { expect, spyOn, test } from "bun:test";
// fireEvent also gives one row's press below a click with no pointer press before it, and a turn
// of the wheel over a list below, which user-event has no form for either.
// biome-ignore lint/style/noRestrictedImports: the outside-press sites below are a pointerdown with no release, and the keys that end or move an IME composition are keys marked as the input method's; user-event has no form for either.
import { act, fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";

import { panelOf, pickNamed } from "../__tests__/list-picks";
import { installResizeObserver } from "../__tests__/scroll-box";
import { NoTooltips } from "../features/issue-page/NoTooltips";
import { type ListMarks, MultiSelect } from "./MultiSelect";

/** A multi-select as a host draws one, closed until its trigger opens it. With `pickedFirst` it
 *  lists the picked values first, in the order they were picked, and then the other options in
 *  their own order, as the Delivery page's facets do (`DeliveryPage`'s `Facet`). */
function Host({
  onCreate,
  options,
  pickedFirst = false,
}: {
  onCreate?: (value: string) => void;
  options: string[];
  pickedFirst?: boolean;
}) {
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
        options={pickedFirst ? [...new Set([...selected, ...options])] : options}
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

/** The open list's Clear foot, which it draws while its pick holds a value, or null. */
function clearFoot(): HTMLElement | null {
  return screen.queryByRole("button", { name: /^Clear / })?.parentElement ?? null;
}

/** Lays `foot`, the list's Clear foot, out as `layOutList` lays out the list: 61 px tall, stuck
 *  to the bottom of what `box`, its panel, shows. */
function layOutFoot(foot: Element, box: Element): void {
  foot.getBoundingClientRect = () => {
    const bottom = box.getBoundingClientRect().top + box.clientTop + box.clientHeight;
    return new DOMRect(1, bottom - 61, 254, 61);
  };
}

/**
 * Lays the open list out as a browser would, which happy-dom does not: its panel, the box that
 * scrolls, is 288 px tall at 100 px down the window inside a 1 px border, so it shows 286 px of
 * what it holds. The text box stands stuck to its top, 61 px tall, and so does the Clear foot to
 * its bottom while it is drawn (`layOutFoot`; a foot drawn after the layout is laid out by
 * `layOutListsAsOpened`); each option is 44 px tall, or as tall as `height` makes it as it is
 * measured, under the text box at its place among the options the list draws when it is
 * measured, moved up by the panel's `scrollTop`, which stops at both ends of the scroll, as a
 * browser's does. `trigger` stands above the panel, so the panel does not close as one hanging off
 * nothing (`AnchoredPanel`). Answers the panel.
 */
function layOutList(
  trigger: HTMLElement,
  height: (option: Element) => number = () => 44
): HTMLElement {
  const listbox = screen.getByRole("listbox");
  const box = listbox.parentElement;
  const head = screen.getByRole("combobox").parentElement;
  if (box === null || head === null) throw new Error("the list is drawn outside its panel");
  const drawn = () => [...listbox.querySelectorAll('[role="option"]')];
  const tall = (options: readonly Element[]) =>
    options.reduce((sum, option) => sum + height(option), 0);
  const shown = 286;
  let scrolled = 0;
  Object.defineProperties(box, {
    clientHeight: { configurable: true, get: () => shown },
    clientTop: { configurable: true, get: () => 1 },
    scrollTop: {
      configurable: true,
      get: () => scrolled,
      set: (value: number) => {
        const end = 61 + tall(drawn()) + (clearFoot() === null ? 0 : 61) - shown;
        scrolled = Math.min(Math.max(value, 0), Math.max(end, 0));
      },
    },
  });
  trigger.getBoundingClientRect = () => new DOMRect(0, 52, 120, 44);
  box.getBoundingClientRect = () => new DOMRect(0, 100, 256, 288);
  head.getBoundingClientRect = () => new DOMRect(1, 101, 254, 61);
  const foot = clearFoot();
  if (foot !== null) layOutFoot(foot, box);
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
 * draws it, before a case could lay it out after the open. A Clear foot the list draws after
 * that, as its first value is ticked, is laid out (`layOutFoot`) the moment anything first
 * measures it too. Answers the box the last list scrolls in, and what puts the measurements back.
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
    if (box !== null && this.parentElement === box && this === clearFoot()) {
      layOutFoot(this, box);
      return this.getBoundingClientRect();
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

// INFRA-3135, round 27: a word typed in the text box marks the first option it leaves active, and
// so does the word cleared, while the box stayed where the arrows had scrolled it: in the issue
// header's Status, Triage stood 39 px above what the box showed after "e", and Enter took an
// option the reader could not see. Each change of the search shows the option it marks active
// whole below the text box, the list's own box alone scrolled.
test("a word typed in a single pick's text box and the word cleared each scroll its list the least that shows the option they mark active whole", async () => {
  const user = userEvent.setup();
  const view = render(<SingleHost initial="Triage" options={STATUSES} />);
  const scrolls = spyOn(Element.prototype, "scrollIntoView");
  try {
    const trigger = screen.getByRole("button", { name: pickNamed("Epic") });
    await user.click(trigger);
    const box = layOutList(trigger);
    const search = screen.getByRole("combobox", { name: "Search epics" });
    // The option the list marks active and how far it has scrolled, after seven ArrowDown and
    // after each key below: the box shows 61-286 px of its content.
    await user.keyboard("{ArrowDown}".repeat(7));
    const walked: [string, number][] = [[activeName(search), box.scrollTop]];
    for (const key of ["e", "{ArrowUp}", "{Backspace}"]) {
      await user.keyboard(key);
      walked.push([activeName(search), box.scrollTop]);
    }
    expect(walked).toEqual([
      // Retro, the last of the eight statuses, at 369-413.
      ["Retro", 127],
      // "e" leaves six statuses, Triage the first of them, at 61-105.
      ["Triage", 0],
      // ArrowUp wraps to the last of them, Retro, at 281-325.
      ["Retro", 39],
      // The word cleared, Triage is the first of all eight again.
      ["Triage", 0],
    ]);
    expect(scrolls).not.toHaveBeenCalled();
  } finally {
    scrolls.mockRestore();
    view.unmount();
  }
});

/** The project List's Labels filter's twenty labels, in its order. */
const LABELS = Array.from({ length: 20 }, (_, at) => `lab-${String(at + 1).padStart(2, "0")}`);

// The same in a multi-select: in the project List's Labels filter, walked to its last label, "1"
// left lab-01 the active option 259 px above what the box showed, and clearing the word left it
// above the box too.
test("a word typed in a multi-select's text box and the word cleared each scroll its list the least that shows the option they mark active whole", async () => {
  const user = userEvent.setup();
  const view = render(<Host options={LABELS} />);
  const scrolls = spyOn(Element.prototype, "scrollIntoView");
  try {
    const trigger = screen.getByRole("button", { name: "Labels" });
    await user.click(trigger);
    const box = layOutList(trigger);
    const search = screen.getByRole("combobox", { name: "Search labels" });
    // The option the list marks active and how far it has scrolled, after nineteen ArrowDown and
    // after each key below: the box shows 61-286 px of its content.
    await user.keyboard("{ArrowDown}".repeat(19));
    const walked: [string, number][] = [[activeName(search), box.scrollTop]];
    for (const key of ["1", "{ArrowUp}", "{Backspace}"]) {
      await user.keyboard(key);
      walked.push([activeName(search), box.scrollTop]);
    }
    expect(walked).toEqual([
      // lab-20, the last of the twenty labels, at 897-941.
      ["lab-20", 655],
      // "1" leaves eleven labels, lab-01 the first of them, at 61-105.
      ["lab-01", 0],
      // ArrowUp wraps to the last of them, lab-19, at 501-545.
      ["lab-19", 259],
      // The word cleared, lab-01 is the first of all twenty again.
      ["lab-01", 0],
    ]);
    expect(scrolls).not.toHaveBeenCalled();
  } finally {
    scrolls.mockRestore();
    view.unmount();
  }
});

// INFRA-3135, round 27: Enter on the option at the bottom of a multi-select's scrolled box ticked
// it, the first value ticked, and the tick drew the Clear foot, stuck to the bottom of the box,
// over it: lab-07 in the project List's Labels filter stood wholly under the foot. Once the host's
// pick holds the value, the list shows the option it marks active whole above the foot, the
// list's own box alone scrolled.
test("Enter that ticks a multi-select's first value at the bottom of its box scrolls the list the least that shows it whole above the Clear foot the tick draws", async () => {
  const user = userEvent.setup();
  const labels = ["api", "docs", "frontend", "infra", "mobile", "ops", "qa", "web"];
  const view = render(<Host options={labels} />);
  const trigger = screen.getByRole("button", { name: "Labels" });
  const lists = layOutListsAsOpened(trigger);
  const scrolls = spyOn(Element.prototype, "scrollIntoView");
  try {
    await user.click(trigger);
    const search = screen.getByRole("combobox", { name: "Search labels" });
    // The option the list marks active and how far it has scrolled, after five ArrowDown and
    // after Enter.
    await user.keyboard("{ArrowDown}".repeat(5));
    const walked: [string, number | undefined][] = [[activeName(search), lists.box()?.scrollTop]];
    await user.keyboard("{Enter}");
    walked.push([activeName(search), lists.box()?.scrollTop]);
    expect(screen.getByTestId("selected").textContent).toBe("ops");
    expect(screen.getByRole("button", { name: "Clear labels" })).toBeTruthy();
    expect(walked).toEqual([
      // ops, the sixth option, at 281-325 px of the box's content: whole at the bottom of the
      // 100-325 px the box shows.
      ["ops", 39],
      // The foot covers the box's last 61 px, so at 39 the box shows 100-264 px, and ops ends
      // below that.
      ["ops", 100],
    ]);
    expect(scrolls).not.toHaveBeenCalled();
  } finally {
    scrolls.mockRestore();
    lists.restore();
    view.unmount();
  }
});

// INFRA-3135, round 27: the Delivery page lists a facet's picked values first, so Enter that
// ticked acme/r06 took it to the top of its Repository list, above what the box the arrows had
// scrolled showed, and Enter that unticked it took it back to its own place, below what the box
// showed once it was back at its top. Once the host's pick holds the value otherwise, the list
// shows the option it marks active whole in its new place, the list's own box alone scrolled.
test("Enter in a multi-select whose host lists the picked values first scrolls the list to show the option it ticks whole at the top, and the option it unticks whole back in its place", async () => {
  const user = userEvent.setup();
  const labels = ["api", "docs", "frontend", "infra", "mobile", "ops", "qa", "web"];
  const view = render(<Host options={labels} pickedFirst />);
  const trigger = screen.getByRole("button", { name: "Labels" });
  const lists = layOutListsAsOpened(trigger);
  const scrolls = spyOn(Element.prototype, "scrollIntoView");
  try {
    await user.click(trigger);
    const search = screen.getByRole("combobox", { name: "Search labels" });
    // The option the list marks active, its place among the options, what is picked and how far
    // the list has scrolled, after seven ArrowDown and after each Enter.
    const walked: [string, number, string | null, number | undefined][] = [];
    const note = () => {
      const active = search.getAttribute("aria-activedescendant");
      walked.push([
        activeName(search),
        screen.getAllByRole("option").findIndex((option) => option.id === active),
        screen.getByTestId("selected").textContent,
        lists.box()?.scrollTop,
      ]);
    };
    await user.keyboard("{ArrowDown}".repeat(7));
    note();
    for (let enter = 0; enter < 2; enter += 1) {
      await user.keyboard("{Enter}");
      note();
    }
    expect(walked).toEqual([
      // web, the last option, at 369-413 px of the box's content, which shows 61-286 px of it.
      ["web", 7, "", 127],
      // Ticked, web is listed first, at 61-105.
      ["web", 0, "web", 0],
      // Unticked, web is back last, at 369-413, and the Clear foot is gone.
      ["web", 7, "", 127],
    ]);
    expect(scrolls).not.toHaveBeenCalled();
  } finally {
    scrolls.mockRestore();
    lists.restore();
    view.unmount();
  }
});

/** What a case below notes of the open Labels list after each step: the option it marks active,
 *  that option's place among the options it draws (-1 for none), what the host's pick holds, and
 *  how far `box`, the list's box, has scrolled. */
function activePlace(box: HTMLElement | null): [string, number, string | null, number | undefined] {
  const search = screen.getByRole("combobox", { name: "Search labels" });
  const active = search.getAttribute("aria-activedescendant");
  return [
    activeName(search),
    screen.getAllByRole("option").findIndex((option) => option.id === active),
    screen.getByTestId("selected").textContent,
    box?.scrollTop,
  ];
}

// A host that lists picked values first moves them back to their original places when Clear
// empties its selection. The active option can move below the visible part of the box.
// Clear keeps active the option it finds active: the value Enter ticked, or the first row, which a
// list opened again on a pick marks active. Once the host's pick holds nothing, the list shows that
// option whole in its own place, the list's own box alone scrolled, with the focus still in the
// text box.
test("Clear in a multi-select whose host lists the picked values first keeps the option it marks active, and scrolls the list the least that shows it whole in its own place", async () => {
  const user = userEvent.setup();
  const labels = ["api", "docs", "frontend", "infra", "mobile", "ops", "qa", "web"];
  const view = render(<Host options={labels} pickedFirst />);
  const trigger = screen.getByRole("button", { name: "Labels" });
  const lists = layOutListsAsOpened(trigger);
  const scrolls = spyOn(Element.prototype, "scrollIntoView");
  try {
    // The option the list marks active, its place among the options, what is picked and how far
    // the list has scrolled: after seven ArrowDown and Enter, after a pointer's press of Clear,
    // once Enter has ticked web again and the list is closed and opened again, and after Clear.
    await user.click(trigger);
    await user.keyboard("{ArrowDown}".repeat(7));
    await user.keyboard("{Enter}");
    const walked = [activePlace(lists.box())];
    await user.click(screen.getByRole("button", { name: "Clear labels" }));
    walked.push(activePlace(lists.box()));
    await user.keyboard("{Enter}");
    await user.keyboard("{Escape}");
    await user.click(trigger);
    walked.push(activePlace(lists.box()));
    await user.click(screen.getByRole("button", { name: "Clear labels" }));
    walked.push(activePlace(lists.box()));
    expect(walked).toEqual([
      // Ticked, web, the last option, is listed first, at 61-105 px of the box's content, which
      // shows 61-225 px of it above the Clear foot.
      ["web", 0, "web", 0],
      // Cleared, web is back last, at 369-413, and the foot is gone, so at 0 the box showed 61-286.
      ["web", 7, "", 127],
      // Ticked again and the list opened again: web is listed first, and is the first row.
      ["web", 0, "web", 0],
      // Cleared, web is back last, as above.
      ["web", 7, "", 127],
    ]);
    expect(screen.getByRole("combobox", { name: "Search labels" })).toHaveFocus();
    expect(scrolls).not.toHaveBeenCalled();
  } finally {
    scrolls.mockRestore();
    lists.restore();
    view.unmount();
  }
});

// A pointer tick in a list that shows picked values first moves the ticked option above the
// visible part of the scrolled box. The list scrolls nothing for
// it: the reader picks among the rows the box shows. Once the host's pick holds the value, the
// list marks active the first option whole between the text box and the Clear foot the tick
// draws, so the next Enter ticks an option the reader sees, not the one the tick moved out of
// sight.
test("a pointer's tick in a multi-select whose host lists the picked values first scrolls nothing, and marks active the first option whole in sight, which the next Enter ticks", async () => {
  const user = userEvent.setup();
  const labels = ["api", "docs", "frontend", "infra", "mobile", "ops", "qa", "web"];
  const view = render(<Host options={labels} pickedFirst />);
  const trigger = screen.getByRole("button", { name: "Labels" });
  const lists = layOutListsAsOpened(trigger);
  const scrolls = spyOn(Element.prototype, "scrollIntoView");
  try {
    await user.click(trigger);
    // The option the list marks active, its place among the options, what is picked and how far
    // the list has scrolled, after seven ArrowDown, after a pointer's press of ops and after Enter.
    await user.keyboard("{ArrowDown}".repeat(7));
    const walked = [activePlace(lists.box())];
    await user.click(screen.getByRole("option", { name: "ops" }));
    walked.push(activePlace(lists.box()));
    await user.keyboard("{Enter}");
    walked.push(activePlace(lists.box()));
    expect(walked).toEqual([
      // web, the last option, at 369-413 px of the box's content, which shows 188-413 px of it;
      // ops, the sixth, stands whole at 281-325.
      ["web", 7, "", 127],
      // Ticked, ops is listed first, at 61-105, above the 188-352 px the box shows over the Clear
      // foot. docs, at 149-193, is cut by the text box, and frontend, at 193-237, is the first
      // option whole in sight.
      ["frontend", 3, "ops", 127],
      // Enter ticks frontend, which is then listed second, at 105-149, and the list shows it whole.
      ["frontend", 1, "ops,frontend", 44],
    ]);
    expect(scrolls).not.toHaveBeenCalled();
  } finally {
    scrolls.mockRestore();
    lists.restore();
    view.unmount();
  }
});

// The same pointer's tick where no option stands whole between the text box and the Clear foot
// the tick draws: each option here is 170 px tall, taller than the 164 px between them, as a long
// label wrapped over many lines makes its row. The first tick draws the foot over the bottom of
// the option it ticks. The list scrolls nothing and marks no option active, so Enter ticks
// nothing, until an arrow marks an option again.
test("a pointer's tick that leaves no option whole between a multi-select's text box and the Clear foot it draws scrolls nothing and marks no option active, so Enter ticks nothing", async () => {
  const user = userEvent.setup();
  const labels = ["api", "docs", "frontend", "infra", "mobile", "ops", "qa", "web"];
  const view = render(<Host options={labels} />);
  const trigger = screen.getByRole("button", { name: "Labels" });
  const lists = layOutListsAsOpened(trigger, () => 170);
  const scrolls = spyOn(Element.prototype, "scrollIntoView");
  try {
    await user.click(trigger);
    // The option the list marks active, its place among the options, what is picked and how far
    // the list has scrolled, after ArrowDown, after a pointer's press of docs, after Enter and
    // after ArrowDown again.
    await user.keyboard("{ArrowDown}");
    const walked = [activePlace(lists.box())];
    await user.click(screen.getByRole("option", { name: "docs" }));
    walked.push(activePlace(lists.box()));
    await user.keyboard("{Enter}");
    walked.push(activePlace(lists.box()));
    await user.keyboard("{ArrowDown}");
    walked.push(activePlace(lists.box()));
    expect(walked).toEqual([
      // docs, the second option, at 231-401 px of the box's content, which shows 176-401 px of it.
      ["docs", 1, "", 115],
      // Ticked, docs stays in its place, and the Clear foot covers 340-401: the box shows 176-340,
      // where api, at 61-231, and docs are each cut.
      ["no option", -1, "docs", 115],
      // Enter ticks nothing.
      ["no option", -1, "docs", 115],
      // ArrowDown marks the first option, api, and scrolls the box to its top.
      ["api", 0, "docs", 0],
    ]);
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

/** Makes the document keep naming a focused element once it has left the page, as an engine that
 *  moves the focus off a removed element only at its next rendering update does (the HTML spec's
 *  focus fixup, `focusLost`); happy-dom names the body at once, as Chromium does. Answers what
 *  puts the document back. */
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
// the body, and an engine that moves the focus off a removed element only at its next rendering
// update still names the text box the close took away when the close runs, so there Tab or
// Shift+Tab out of the box left the focus to fall onto the page. A close the list makes itself puts
// the reader on the trigger in every engine; a press outside still leaves them where it put them.
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
