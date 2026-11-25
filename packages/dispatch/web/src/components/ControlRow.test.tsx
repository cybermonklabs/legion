import { expect, mock, spyOn, test } from "bun:test";
import { type ColumnDef, getCoreRowModel, useReactTable } from "@tanstack/react-table";
import { act, render, screen, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { type ReactNode, useState } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { controlBar, layOutBar, layOutLine, withCss, wordsOf } from "../__tests__/chips-line";
import { MOUSE_CSS, SIZE_CSS, sizeUnder } from "../__tests__/control-size";
import { afterFrameAndTasks } from "../__tests__/frames";
import { installResizeObserver, layOutPanel, stacked } from "../__tests__/scroll-box";
import { primaryButtonBg, tableFilterActive } from "../theme/classes";
import type { BarActionProps } from "./BarAction";
import { ControlRow, type ControlRowProps } from "./ControlRow";
import type { DisplayMenuSection } from "./DisplayMenu";
import type { FilterChip } from "./filter-chips";

/**
 * Tailwind's own definitions of the utilities the bar's height rule is written in. happy-dom
 * applies a stylesheet it is given but loads none of the application's generated CSS, so the rule
 * is read from the computed style with these in the document: a bar that stopped holding one
 * line, or a chips strip that started wrapping, computes something else here whatever class it
 * carries.
 */
const HEIGHT_RULE_CSS =
  ".flex{display:flex}.flex-col{flex-direction:column}.flex-wrap{flex-wrap:wrap}.flex-nowrap{flex-wrap:nowrap}.shrink-0{flex-shrink:0}.flex-1{flex:1 1 0%}.min-w-0{min-width:0}.gap-2{gap:0.5rem}.overflow-x-auto{overflow-x:auto}" +
  String.raw`.\[\&\>\*\]\:shrink-0>*{flex-shrink:0}`;

/** A router's future flags, as the app's own router sets them. */
const FUTURE = { v7_relativeSplatPath: true, v7_startTransition: true } as const;

function row(): HTMLElement {
  return controlBar("the page");
}

/** A chip of the bar, by the name of its remove control (`filterChips`). */
function chip(name: string): HTMLElement {
  return screen.getByRole("button", { name: `Remove ${name} filter` });
}

function queryChip(name: string): HTMLElement | null {
  return screen.queryByRole("button", { name: `Remove ${name} filter` });
}

/** The words of each chip the line draws, in its order. */
function lineChips(): string[] {
  const strip = document.querySelector("[data-chip-strip]");
  return Array.from(strip?.children ?? [], wordsOf);
}

/** The words of each chip the open panel of `+N more` lists, in its order. */
function panelChips(name: string): string[] {
  return within(screen.getByRole("dialog", { name })).getAllByRole("button").map(wordsOf);
}

/** A row of a table that is not an issue, for the columns menu's slot. */
interface Deploy {
  readonly id: string;
  readonly service: string;
}

const DEPLOY_COLUMNS: ColumnDef<Deploy>[] = [
  { accessorFn: (deploy) => deploy.service, header: "Service", id: "service" },
];

const NO_DEPLOYS: Deploy[] = [];

const SORT: DisplayMenuSection = {
  id: "sort",
  label: "Sort by",
  onChange: () => undefined,
  options: [
    { label: "Newest", value: "newest" },
    { label: "Oldest", value: "oldest" },
  ],
  value: "newest",
};

/** The glyph a page hands an action. */
const ICON = <svg aria-hidden="true" />;

/** The placeholder of `Bar`'s search box. */
const PLACEHOLDER = "Issue, title, question or author";

/** An action a press does at once, named `label`. */
function pressAction(label: string, more: Partial<BarActionProps> = {}): BarActionProps {
  return { icon: ICON, label, onPress: () => undefined, ...more };
}

/** **Save view**: an action with a panel of its own. */
function saveView(onPress: () => void = () => undefined): BarActionProps {
  return {
    icon: ICON,
    label: "Save view",
    onPress,
    panel: (close) => (
      <button onClick={close} type="button">
        Save
      </button>
    ),
  };
}

/**
 * A bar with every slot filled from props, as a page on the slots draws it: the Inbox's search
 * box and switch, **Filter** counting the narrowings that are on (`narrowings`, one by default),
 * their chips, which a press takes off, **Clear filters** while any is on, **Save view**, **New
 * document**, **Mark all read** kept in **More**, the (i) hint, a table's columns menu, **View**
 * and **New message**. `props` replace any slot, `undefined` included.
 */
function Bar({
  narrowings = ["Type: Question"],
  ...props
}: Partial<ControlRowProps<Deploy>> & { readonly narrowings?: readonly string[] }): ReactNode {
  const [on, setOn] = useState(narrowings);
  const [filterOpen, setFilterOpen] = useState(false);
  const [displayOpen, setDisplayOpen] = useState(false);
  const [hintOpen, setHintOpen] = useState(false);
  const table = useReactTable({
    columns: DEPLOY_COLUMNS,
    data: NO_DEPLOYS,
    getCoreRowModel: getCoreRowModel(),
  });
  return (
    <MemoryRouter future={FUTURE}>
      <ControlRow<Deploy>
        actions={[
          saveView(),
          pressAction("New document"),
          pressAction("Mark all read", { more: true }),
        ]}
        chips={on.map((label) => ({
          label,
          remove: () => setOn((left) => left.filter((other) => other !== label)),
        }))}
        clear={on.length === 0 ? undefined : () => setOn([])}
        columns={{ columns: table.getAllLeafColumns() }}
        display={{
          label: "View",
          onOpenChange: setDisplayOpen,
          open: displayOpen,
          sections: [SORT],
        }}
        hint={{
          children: () => <p>What this view lists.</p>,
          label: "i",
          onOpenChange: setHintOpen,
          open: hintOpen,
          title: "About this view",
        }}
        main={{ icon: ICON, label: "New message", onPress: () => undefined }}
        search={{
          label: "Search asks",
          onChange: () => undefined,
          placeholder: PLACEHOLDER,
          value: "",
        }}
        switch={{
          label: "Inbox section",
          onChange: () => undefined,
          parts: [
            { label: "All", value: "all" },
            { count: 2, label: "Blocked on you", value: "needs-you" },
          ],
          value: "all",
        }}
        trigger={{
          children: () => <p>The fields.</p>,
          count: on.length,
          label: "Filter",
          onOpenChange: setFilterOpen,
          open: filterOpen,
          stacked: true,
        }}
        {...props}
      />
    </MemoryRouter>
  );
}

/** Opens **More** and answers its menu and the names of its items, in its order. */
async function openMore(user: UserEvent): Promise<{ items: string[]; menu: HTMLElement }> {
  await user.click(within(row()).getByRole("button", { name: "More" }));
  const menu = screen.getByRole("menu", { name: "More" });
  const items = Array.from(
    menu.querySelectorAll<HTMLElement>('[role="menuitem"], [role="menuitemcheckbox"]'),
    (item) => item.getAttribute("aria-label") ?? (item.textContent ?? "").replace("✓", "")
  );
  return { items, menu };
}

/** The controls of the bar a reader reaches, in its order, each by its accessible name. */
function controlNames(): string[] {
  return Array.from(row().querySelectorAll<HTMLElement>("a[href], button, input"), (control) => {
    const label = control.getAttribute("aria-label");
    return label ?? (control.textContent ?? "").trim();
  });
}

// --- k-slots: one case for each sentence of its body -------------------------------------------

test("it draws More only while it holds an action", () => {
  const view = render(<Bar actions={[]} />);
  try {
    expect(document.querySelector('[data-control="more-menu"]')).toBeNull();
    // An action of the bar's line is no action of More.
    view.rerender(<Bar actions={[pressAction("New document")]} />);
    expect(within(row()).getByRole("button", { name: "New document" })).toHaveAttribute(
      "data-control",
      "bar-action"
    );
    expect(document.querySelector('[data-control="more-menu"]')).toBeNull();
    view.rerender(<Bar actions={[pressAction("Mark all read", { more: true })]} />);
    expect(within(row()).getByRole("button", { name: "More" })).toHaveAttribute(
      "data-control",
      "more-menu"
    );
    view.rerender(<Bar actions={[]} />);
    expect(document.querySelector('[data-control="more-menu"]')).toBeNull();
  } finally {
    view.unmount();
  }
});

test("an action the page keeps in More is an item of More at every width", async () => {
  const user = userEvent.setup();
  for (const width of [1280, 640, 390]) {
    const restore = layOutBar({ width });
    const view = render(
      <Bar actions={[pressAction("Mark all read", { more: true })]} narrowings={[]} />
    );
    try {
      expect(within(row()).queryByRole("button", { name: "Mark all read" })).toBeNull();
      expect((await openMore(user)).items).toEqual(["Mark all read"]);
    } finally {
      view.unmount();
      restore();
    }
  }
});

test("below 640 px of the bar's width every action is an item of More", async () => {
  const user = userEvent.setup();
  const sizes = installResizeObserver();
  const bar = { width: 1280 };
  const restore = layOutBar(bar);
  const view = render(
    <Bar actions={[pressAction("New document"), pressAction("Mark all read", { more: true })]} />
  );
  try {
    expect(within(row()).getByRole("button", { name: "New document" })).toBeTruthy();
    expect((await openMore(user)).items).toEqual(["Mark all read"]);
    await user.keyboard("{Escape}");
    bar.width = 639;
    sizes.resize();
    expect(within(row()).queryByRole("button", { name: "New document" })).toBeNull();
    expect((await openMore(user)).items).toEqual(["New document", "Mark all read"]);
  } finally {
    view.unmount();
    restore();
    sizes.restore();
  }
});

test("with the narrow display rule, the display menu is an item of More at 639 px and in its own slot at 640 px", async () => {
  const user = userEvent.setup();
  const sizes = installResizeObserver();
  const bar = { width: 639 };
  const restore = layOutBar(bar);
  // The Inbox's bar: Mark all read kept in More, and the rule set.
  const view = render(
    <Bar actions={[pressAction("Mark all read", { more: true })]} narrowDisplayInMore />
  );
  try {
    expect(within(row()).queryByRole("button", { name: "View" })).toBeNull();
    const { items, menu } = await openMore(user);
    expect(items).toEqual(["View", "Mark all read"]);
    const display = within(menu).getByRole("menuitem", { name: "View" });
    expect(display).toHaveAttribute("data-control", "display-menu");
    // It opens its panel over the menu, which stays open under it.
    await user.click(display);
    expect(screen.getByRole("dialog", { name: "View" })).toBeTruthy();
    expect(screen.getByRole("menu", { name: "More" })).toBeTruthy();
    await user.keyboard("{Escape}");
    await user.keyboard("{Escape}");

    bar.width = 640;
    sizes.resize();
    const own = within(row()).getByRole("button", { name: "View" });
    expect(own).toHaveAttribute("data-control", "display-menu");
    expect(own.closest('[role="menu"]')).toBeNull();
    expect((await openMore(user)).items).toEqual(["Mark all read"]);
    await user.keyboard("{Escape}");

    // More holding the display menu alone is drawn too.
    view.rerender(<Bar actions={[]} narrowDisplayInMore />);
    expect(document.querySelector('[data-control="more-menu"]')).toBeNull();
    bar.width = 639;
    sizes.resize();
    expect((await openMore(user)).items).toEqual(["View"]);
  } finally {
    view.unmount();
    restore();
    sizes.restore();
  }
});

test("with the narrow display rule, an open display menu closes as the bar narrows and hands the focus to More", async () => {
  const user = userEvent.setup();
  const sizes = installResizeObserver();
  const bar = { width: 1280 };
  const restore = layOutBar(bar);
  const view = render(
    <Bar actions={[pressAction("Mark all read", { more: true })]} narrowDisplayInMore />
  );
  try {
    await user.click(within(row()).getByRole("button", { name: "View" }));
    expect(screen.getByRole("dialog", { name: "View" })).toBeTruthy();
    bar.width = 639;
    sizes.resize();
    // Its trigger moved into More: the menu closed with it, and the focus is on More.
    expect(screen.queryByRole("dialog", { name: "View" })).toBeNull();
    expect(within(row()).queryByRole("button", { name: "View" })).toBeNull();
    expect(within(row()).getByRole("button", { name: "More" })).toHaveFocus();
    // Opened from More, the menu's item says it is shut, and opens nothing until pressed.
    const { menu } = await openMore(user);
    expect(within(menu).getByRole("menuitem", { name: "View" })).toHaveAttribute(
      "aria-expanded",
      "false"
    );
    expect(screen.queryByRole("dialog", { name: "View" })).toBeNull();
  } finally {
    view.unmount();
    restore();
    sizes.restore();
  }
});

test("below 640 px Filter, the display menu, More and the main action show their glyphs alone and keep their names, and the switch keeps its labels", () => {
  const restore = layOutBar({ width: 390 });
  const view = render(<Bar />);
  try {
    for (const [name, words] of [
      ["Filter · 1", "1"],
      ["View", ""],
      ["More", ""],
      ["New message", ""],
    ] as const) {
      const button = within(row()).getByRole("button", { name });
      expect(button.querySelector("svg")).not.toBeNull();
      expect(button.textContent).toBe(words);
    }
    const segments = screen.getByRole("group", { name: "Inbox section" });
    expect(
      within(segments)
        .getAllByRole("button")
        .map((part) => part.textContent)
    ).toEqual(["All", "Blocked on you 2"]);
  } finally {
    view.unmount();
    restore();
  }
});

test("below 640 px a part of the switch given a short label draws it, and keeps its name", () => {
  const sizes = installResizeObserver();
  const bar = { width: 1280 };
  const restore = layOutBar(bar);
  const title = "Blocked on you: 2 items, oldest 2 days";
  const view = render(
    <Bar
      switch={{
        label: "Inbox section",
        onChange: () => undefined,
        parts: [
          { label: "All", short: "All", value: "all" },
          { count: 2, label: "Blocked on you", short: "You", title, value: "needs-you" },
        ],
        value: "all",
      }}
    />
  );
  const parts = () =>
    within(screen.getByRole("group", { name: "Inbox section" }))
      .getAllByRole("button")
      .map((part) => [part.textContent, part.getAttribute("aria-label") ?? part.textContent]);
  try {
    expect(parts()).toEqual([
      ["All", "All"],
      ["Blocked on you 2", title],
    ]);
    bar.width = 639;
    sizes.resize();
    expect(parts()).toEqual([
      ["All", "All"],
      ["You · 2", title],
    ]);
  } finally {
    view.unmount();
    restore();
    sizes.restore();
  }
});

test("the main action is drawn last, as the bar's one filled action", () => {
  for (const width of [1280, 390]) {
    const restore = layOutBar({ width });
    const view = render(<Bar />);
    try {
      const main = within(row()).getByRole("button", { name: "New message" });
      expect(main).toHaveAttribute("data-control", "bar-action");
      const controls = Array.from(row().querySelectorAll<HTMLElement>("a[href], button, input"));
      expect(controls.at(-1)).toBe(main);
      const filled = controls.filter((control) =>
        primaryButtonBg.split(" ").every((name) => control.classList.contains(name))
      );
      expect(filled).toEqual([main]);
    } finally {
      view.unmount();
      restore();
    }
  }
});

test("each slot draws its shared component, with its own data-control value, from the props it is given", () => {
  const view = render(<Bar />);
  try {
    expect(
      Array.from(row().querySelectorAll("[data-control]"), (control) =>
        control.getAttribute("data-control")
      )
    ).toEqual([
      "search-box",
      "segmented-control",
      "panel-trigger",
      "chip",
      "view-panel",
      "bar-action",
      "view-panel",
      "columns-menu",
      "display-menu",
      "more-menu",
      "bar-action",
    ]);
    const search = screen.getByRole("searchbox", { name: "Search asks" });
    expect(search).toHaveAttribute("placeholder", PLACEHOLDER);
    const segments = screen.getByRole("group", { name: "Inbox section" });
    expect(segments).toHaveAttribute("data-control", "segmented-control");
    expect(within(segments).getByRole("button", { name: "All" })).toHaveAttribute(
      "aria-pressed",
      "true"
    );
    expect(screen.getByRole("button", { name: "Filter · 1" })).toHaveAttribute(
      "data-control",
      "panel-trigger"
    );
    expect(chip("Type: Question")).toHaveAttribute("data-control", "chip");
    expect(screen.getByRole("button", { name: "Save view" })).toHaveAttribute(
      "data-control",
      "view-panel"
    );
    expect(screen.getByRole("button", { name: "Columns" })).toHaveAttribute(
      "data-control",
      "columns-menu"
    );
    expect(screen.getByRole("button", { name: "View" })).toHaveAttribute(
      "data-control",
      "display-menu"
    );
    // A divider after the switch, before Filter.
    const divider = row().querySelector("[data-bar-divider]");
    expect(segments.parentElement?.nextElementSibling).toBe(divider);
    expect(
      divider?.nextElementSibling?.contains(screen.getByRole("button", { name: "Filter · 1" }))
    ).toBe(true);
  } finally {
    view.unmount();
  }
});

test("an action given a refusal is drawn refused with its refusal shown, and a press on it does nothing", async () => {
  const user = userEvent.setup();
  const onPress = mock(() => {});
  const refused = pressAction("New document", {
    onPress,
    refusal: { id: "closed-refusal", words: "This issue is closed." },
  });
  for (const width of [1280, 390]) {
    const restore = layOutBar({ width });
    // The page's own line under the bar, which says the refusal.
    const view = render(
      <>
        <Bar actions={[refused]} />
        <p id="closed-refusal">This issue is closed.</p>
      </>
    );
    try {
      let control: HTMLElement;
      if (width === 1280) {
        control = within(row()).getByRole("button", { name: "New document" });
        expect(control).toHaveAttribute("title", "This issue is closed.");
      } else {
        // Below 640 px it is an item of More, which says why under its label.
        control = within((await openMore(user)).menu).getByRole("menuitem", {
          name: "New document",
        });
        expect(control).toHaveTextContent("This issue is closed.");
      }
      expect(control).toHaveAttribute("aria-disabled", "true");
      expect(control).toHaveAccessibleDescription("This issue is closed.");
      await user.click(control);
      expect(onPress).not.toHaveBeenCalled();
    } finally {
      view.unmount();
      restore();
    }
  }
});

test('an action given a panel opens that panel, with data-control="view-panel"', async () => {
  const user = userEvent.setup();
  for (const width of [1280, 390]) {
    const restore = layOutBar({ width });
    const opened = mock(() => {});
    const view = render(<Bar actions={[saveView(opened)]} />);
    try {
      const trigger =
        width === 1280
          ? within(row()).getByRole("button", { name: "Save view" })
          : within((await openMore(user)).menu).getByRole("menuitem", { name: "Save view" });
      expect(trigger).toHaveAttribute("data-control", "view-panel");
      expect(trigger).toHaveAttribute("aria-haspopup", "dialog");
      await user.click(trigger);
      // The page hears that the panel opened.
      expect(opened).toHaveBeenCalledTimes(1);
      const panel = screen.getByRole("dialog", { name: "Save view" });
      await user.click(within(panel).getByRole("button", { name: "Save" }));
      expect(screen.queryByRole("dialog", { name: "Save view" })).toBeNull();
      expect(trigger).toHaveFocus();
    } finally {
      view.unmount();
      restore();
    }
  }
});

test("a chip given a link draws the link beside the chip's remove control and outside it", async () => {
  const user = userEvent.setup();
  const remove = mock(() => {});
  const chips: FilterChip[] = [
    {
      label: "Asks from Planner",
      link: { label: "Open live conversation with Planner", to: "/agents/planner/live" },
      remove,
    },
  ];
  const view = render(<Bar chips={chips} />);
  try {
    const own = chip("Asks from Planner");
    const link = screen.getByRole("link", { name: "Open live conversation with Planner" });
    expect(link).toHaveAttribute("href", "/agents/planner/live");
    expect(own.contains(link)).toBe(false);
    expect(link.contains(own)).toBe(false);
    // Beside it: in the same chip of the line, after its remove control.
    const lineChipOf = (element: Element) => element.closest("[data-chip-strip] > *");
    expect(lineChipOf(own)).not.toBeNull();
    expect(lineChipOf(link)).toBe(lineChipOf(own));
    expect(own.compareDocumentPosition(link) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    await user.click(own);
    expect(remove).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
  }
});

test("the hint slot draws its trigger inside the bar, as a view-panel trigger", async () => {
  const user = userEvent.setup();
  const view = render(<Bar />);
  try {
    const hint = within(row()).getByRole("button", { name: "About this view" });
    expect(hint).toHaveAttribute("data-control", "view-panel");
    expect(hint).toHaveAttribute("aria-haspopup", "dialog");
    expect(hint).toHaveClass("bg-transparent");
    await user.click(hint);
    const panel = screen.getByRole("dialog", { name: "About this view" });
    expect(within(panel).getByText("What this view lists.")).toBeTruthy();
  } finally {
    view.unmount();
  }
});

test("the display slot's trigger reads the label it is given, as Group by", () => {
  const view = render(
    <Bar
      display={{ label: "Group by", onOpenChange: () => undefined, open: false, sections: [SORT] }}
    />
  );
  try {
    const trigger = within(row()).getByRole("button", { name: "Group by" });
    expect(trigger).toHaveTextContent("Group by");
    expect(trigger).toHaveAttribute("data-control", "display-menu");
  } finally {
    view.unmount();
  }
});

/** The Board's bar, whose **More** holds **Show Icebox** and **Lanes** as check items. */
function BoardBar({ onIcebox }: { onIcebox: () => void }): ReactNode {
  const [icebox, setIcebox] = useState(false);
  return (
    <Bar
      actions={[
        pressAction("Show Icebox", {
          checked: icebox,
          onPress: () => {
            onIcebox();
            setIcebox((shown) => !shown);
          },
        }),
        pressAction("Lanes", { checked: true }),
      ]}
    />
  );
}

test("an item of More given a checked state is a check item (menuitemcheckbox) that shows its state and calls the page's handler", async () => {
  const user = userEvent.setup();
  const onIcebox = mock(() => {});
  const view = render(<BoardBar onIcebox={onIcebox} />);
  try {
    // Kept in More at every width, so no button of the bar's line is either.
    expect(within(row()).queryByRole("button", { name: "Show Icebox" })).toBeNull();
    const { menu } = await openMore(user);
    const icebox = within(menu).getByRole("menuitemcheckbox", { name: "Show Icebox" });
    expect(icebox).toHaveAttribute("aria-checked", "false");
    expect(within(menu).getByRole("menuitemcheckbox", { name: "Lanes" })).toHaveAttribute(
      "aria-checked",
      "true"
    );
    await user.click(icebox);
    expect(onIcebox).toHaveBeenCalledTimes(1);
    // The menu stays open, so the reader can flip the next.
    expect(
      within(screen.getByRole("menu", { name: "More" })).getByRole("menuitemcheckbox", {
        name: "Show Icebox",
      })
    ).toHaveAttribute("aria-checked", "true");
  } finally {
    view.unmount();
  }
});

// --- ST9: Clear filters and the trigger's tint ---------------------------------------------------

test("with narrowings that differ, the trigger's count is their number and Clear filters calls the page's handler", async () => {
  const user = userEvent.setup();
  const clear = mock(() => {});
  const view = render(
    <Bar clear={clear} narrowings={["Type: Question", "Urgency: High", "Priority: P1"]} />
  );
  try {
    const trigger = within(row()).getByRole("button", { name: "Filter · 3" });
    expect(trigger).toHaveTextContent("Filter · 3");
    const button = within(row()).getByRole("button", { name: "Clear filters" });
    // Two buttons, not one inside the other, in one tinted group.
    expect(trigger.contains(button)).toBe(false);
    expect(button.contains(trigger)).toBe(false);
    expect(button.parentElement).toBe(trigger.parentElement);
    for (const name of tableFilterActive.split(" ")) {
      expect(trigger).toHaveClass(name);
      expect(button).toHaveClass(name);
    }
    await user.click(button);
    expect(clear).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
  }
});

test("with no narrowing that differs, no Clear filters is drawn", () => {
  const view = render(<Bar narrowings={[]} />);
  try {
    expect(screen.queryByRole("button", { name: "Clear filters" })).toBeNull();
    const trigger = within(row()).getByRole("button", { name: "Filter" });
    expect(trigger.textContent).toBe("Filter");
    expect(trigger).toHaveClass("bg-transparent");
  } finally {
    view.unmount();
  }
});

test("below 640 px Clear filters is drawn at no count", () => {
  const restore = layOutBar({ width: 390 });
  const view = render(<Bar clear={() => undefined} narrowings={[]} />);
  try {
    for (const count of [0, 1, 3]) {
      const narrowings = ["Type: Question", "Urgency: High", "Priority: P1"].slice(0, count);
      view.rerender(
        <Bar
          chips={narrowings.map((label) => ({ label, remove: () => undefined }))}
          clear={() => undefined}
          trigger={{
            children: () => null,
            count,
            label: "Filter",
            onOpenChange: () => undefined,
            open: false,
          }}
        />
      );
      expect(screen.queryByRole("button", { name: "Clear filters" })).toBeNull();
      const trigger = within(row()).getByRole("button", {
        name: count === 0 ? "Filter" : `Filter · ${count}`,
      });
      // No words but the badge, on its glyph.
      expect(trigger.querySelector("svg")).not.toBeNull();
      expect(trigger.textContent).toBe(count === 0 ? "" : String(count));
    }
  } finally {
    view.unmount();
    restore();
  }
});

// --- ST8: the bar's layout by its own width -----------------------------------------------------

test("a bar with no width read draws the wide layout", () => {
  // happy-dom lays nothing out: the bar's own width reads 0.
  const view = render(<Bar />);
  try {
    expect(row().getBoundingClientRect().width).toBe(0);
    expect(lineChips()).toEqual(["Type: Question"]);
    expect(within(row()).getByRole("button", { name: "New document" })).toBeTruthy();
    expect(within(row()).getByRole("button", { name: "Clear filters" })).toBeTruthy();
    expect(within(row()).getByRole("button", { name: "Filter · 1" })).toHaveTextContent(
      "Filter · 1"
    );
    expect(within(row()).getByRole("button", { name: "New message" })).toHaveTextContent(
      "New message"
    );
  } finally {
    view.unmount();
  }
});

test("a bar that widens past 640 px takes back its chips and its actions", () => {
  const sizes = installResizeObserver();
  const bar = { width: 390 };
  const restore = layOutBar(bar);
  const view = render(<Bar />);
  try {
    expect(lineChips()).toEqual([]);
    expect(within(row()).queryByRole("button", { name: "New document" })).toBeNull();
    bar.width = 640;
    sizes.resize();
    expect(lineChips()).toEqual(["Type: Question"]);
    expect(within(row()).getByRole("button", { name: "New document" })).toBeTruthy();
    expect(within(row()).getByRole("button", { name: "Clear filters" })).toBeTruthy();
  } finally {
    view.unmount();
    restore();
    sizes.restore();
  }
});

test("a panel open when the bar crosses 640 px stays open on its trigger", async () => {
  const user = userEvent.setup();
  const sizes = installResizeObserver();
  const bar = { width: 1280 };
  const restore = layOutBar(bar);
  const view = render(<Bar />);
  try {
    const trigger = within(row()).getByRole("button", { name: "Filter · 1" });
    await user.click(trigger);
    bar.width = 390;
    sizes.resize();
    expect(screen.getByRole("dialog", { name: "Filter" })).toBeTruthy();
    expect(within(row()).getByRole("button", { name: "Filter · 1" })).toBe(trigger);
    expect(trigger).toHaveAttribute("aria-expanded", "true");
    await user.keyboard("{Escape}");
    // The display menu opened narrow stays open as the bar widens.
    const display = within(row()).getByRole("button", { name: "View" });
    await user.click(display);
    bar.width = 1280;
    sizes.resize();
    expect(screen.getByRole("dialog", { name: "View" })).toBeTruthy();
    expect(within(row()).getByRole("button", { name: "View" })).toBe(display);
    expect(display).toHaveAttribute("aria-expanded", "true");
  } finally {
    view.unmount();
    restore();
    sizes.restore();
  }
});

test("an action that moves into More while it holds the focus hands it to More", () => {
  const sizes = installResizeObserver();
  const bar = { width: 1280 };
  const restore = layOutBar(bar);
  const view = render(<Bar />);
  try {
    act(() => within(row()).getByRole("button", { name: "New document" }).focus());
    bar.width = 390;
    sizes.resize();
    expect(within(row()).queryByRole("button", { name: "New document" })).toBeNull();
    expect(within(row()).getByRole("button", { name: "More" })).toHaveFocus();
  } finally {
    view.unmount();
    restore();
    sizes.restore();
  }
});

test("a narrow bar with no trigger keeps one +N more that lists its chips", async () => {
  const user = userEvent.setup();
  const chips = ["Owner: alice", "State: live"].map((label) => ({
    label,
    remove: () => undefined,
  }));
  const restore = layOutBar({ width: 390 });
  const view = render(<Bar chips={chips} clear={undefined} trigger={undefined} />);
  try {
    // No chip but `+N more` in the line: the line shows `+N more` alone here.
    expect(lineChips()).toEqual([]);
    const more = within(row()).getByRole("button", { name: "2 more filters" });
    expect(more).toHaveTextContent("+2 more");
    await user.click(more);
    expect(panelChips("2 more filters")).toEqual(["Owner: alice", "State: live"]);
  } finally {
    view.unmount();
    restore();
  }
});

/** The Inbox's bar while **Mark all read** is offered, which a press takes away; with or
 *  without its main action. */
function MarkAllBar({ main }: { main: boolean }): ReactNode {
  const [unread, setUnread] = useState(true);
  return (
    <Bar
      actions={
        unread
          ? [pressAction("Mark all read", { more: true, onPress: () => setUnread(false) })]
          : []
      }
      main={main ? { icon: ICON, label: "New message", onPress: () => undefined } : undefined}
    />
  );
}

test("More that goes hands the focus to the main action, else the control before it", async () => {
  const user = userEvent.setup();
  const landed: Record<string, string | null> = {};
  for (const main of [true, false]) {
    const view = render(<MarkAllBar main={main} />);
    try {
      const { menu } = await openMore(user);
      await user.click(within(menu).getByRole("menuitem", { name: "Mark all read" }));
      expect(document.querySelector('[data-control="more-menu"]')).toBeNull();
      landed[main ? "with a main action" : "with none"] =
        document.activeElement?.getAttribute("aria-label") ??
        document.activeElement?.textContent ??
        null;
    } finally {
      view.unmount();
    }
  }
  expect(landed).toEqual({ "with a main action": "New message", "with none": "View" });
});

test("the trigger's panel lists each chip at its top, at every width, each with its remove control", async () => {
  const user = userEvent.setup();
  for (const width of [1280, 390]) {
    const restore = layOutBar({ width });
    const view = render(<Bar narrowings={["Type: Question", "Urgency: High"]} />);
    try {
      await user.click(within(row()).getByRole("button", { name: "Filter · 2" }));
      const panel = screen.getByRole("dialog", { name: "Filter" });
      const listed = within(panel).getAllByRole("button", { name: /^Remove / });
      expect(listed.map((each) => each.getAttribute("aria-label"))).toEqual([
        "Remove Type: Question filter",
        "Remove Urgency: High filter",
      ]);
      // At the top, above the page's own fields.
      const fields = within(panel).getByText("The fields.");
      for (const each of listed) {
        expect(
          each.compareDocumentPosition(fields) & Node.DOCUMENT_POSITION_FOLLOWING
        ).toBeTruthy();
      }
      // A press takes that narrowing off, and the panel stays open.
      await user.click(listed[0] as HTMLElement);
      const still = screen.getByRole("dialog", { name: "Filter" });
      expect(
        within(still)
          .getAllByRole("button", { name: /^Remove / })
          .map((each) => each.getAttribute("aria-label"))
      ).toEqual(["Remove Urgency: High filter"]);
      expect(within(row()).getByRole("button", { name: "Filter · 1" })).toBeTruthy();
    } finally {
      view.unmount();
      restore();
    }
  }
});

// D26 in the trigger's panel: its list of chips is a focus place of its own, as the bar's line
// is, so a chip taken off there hands the focus to the chip now at its place, else to the one
// before it - never past the list to the panel's own field after it while a chip is left. The
// list's last chip takes the list with it, and the panel hands the focus to that field.
test("a chip taken off in the trigger's panel hands the focus to the next chip, else the one before", async () => {
  const user = userEvent.setup();
  const view = render(
    <Bar
      narrowings={["Alpha", "Beta", "Gamma"]}
      trigger={{
        children: () => <button type="button">First field</button>,
        label: "Filter",
        onOpenChange: () => undefined,
        open: true,
        stacked: true,
      }}
    />
  );
  try {
    const panel = screen.getByRole("dialog", { name: "Filter" });
    const chip = (name: string) =>
      within(panel).getByRole("button", { name: `Remove ${name} filter` });
    const take = async (name: string) => {
      const taken = chip(name);
      act(() => taken.focus());
      await user.keyboard("{Enter}");
      expect(taken.isConnected).toBe(false);
    };
    await take("Alpha");
    expect(chip("Beta")).toHaveFocus();
    // The list's last chip, with the panel's field after it: the one before it takes the focus.
    await take("Gamma");
    expect(chip("Beta")).toHaveFocus();
    await take("Beta");
    expect(within(panel).queryAllByRole("button", { name: /^Remove / })).toEqual([]);
    expect(within(panel).getByRole("button", { name: "First field" })).toHaveFocus();
  } finally {
    view.unmount();
  }
});

// --- The bar's line, rewritten from the row's rules ---------------------------------------------

test("the slots are drawn in the bar's own order, whatever order the page names them in", () => {
  const view = render(<Bar />);
  try {
    expect(row()).toHaveAttribute("data-control", "control-row");
    expect(controlNames()).toEqual([
      "Search asks",
      "All",
      "Blocked on you 2",
      "Filter · 1",
      "Clear filters",
      "Remove Type: Question filter",
      "Save view",
      "New document",
      "About this view",
      "Columns",
      "View",
      "More",
      "New message",
    ]);
  } finally {
    view.unmount();
  }
});

test("a slot the page leaves out is no empty box in the bar", () => {
  const view = render(
    <ControlRow search={{ label: "Search", onChange: () => undefined, value: "" }} />
  );
  try {
    // The search box's slot, and the empty space the chips stand in: nothing else.
    const drawn = Array.from(row().children);
    expect(drawn).toHaveLength(2);
    expect(drawn[0]?.contains(screen.getByRole("searchbox", { name: "Search" }))).toBe(true);
    expect(drawn[1]?.querySelector("[data-chip-strip]")).not.toBeNull();
    view.rerender(<ControlRow actions={[pressAction("Upload")]} />);
    expect(screen.queryByRole("searchbox")).toBeNull();
    expect(Array.from(row().children)).toHaveLength(2);
  } finally {
    view.unmount();
  }
});

test("a chip is drawn in the bar's line only while one is on, and a bar with none draws no +N more", () => {
  const view = render(<Bar chips={[]} />);
  try {
    expect(lineChips()).toEqual([]);
    expect(screen.queryByRole("button", { name: /more filters?$/ })).toBeNull();
    view.rerender(<Bar chips={[{ label: "Type: Question", remove: () => undefined }]} />);
    expect(lineChips()).toEqual(["Type: Question"]);
    view.rerender(<Bar chips={undefined} />);
    expect(lineChips()).toEqual([]);
  } finally {
    view.unmount();
  }
});

test("the bar is one line, and however many chips are on they stand in it", () => {
  const removeCss = withCss(HEIGHT_RULE_CSS);
  const view = render(<Bar narrowings={["Type: Question", "Urgency: High", "Priority: P1"]} />);
  try {
    // One flex line, so its height is its tallest control's.
    expect(getComputedStyle(row()).display).toBe("flex");
    expect(getComputedStyle(row()).flexDirection).not.toMatch(/column/);
    expect(getComputedStyle(row()).flexWrap).toBe("nowrap");
    // No box in the bar may take another line at all.
    const wrapping = Array.from(row().querySelectorAll("*")).filter(
      (node) => getComputedStyle(node).flexWrap === "wrap"
    );
    expect(wrapping).toEqual([]);
    // The chips are in the bar's own line, in the box of its empty space, which takes the room
    // the rest of the line leaves and gives way rather than push a control past the edge.
    const strip = row().querySelector<HTMLElement>("[data-chip-strip]");
    if (strip === null) throw new Error("the bar draws no chips strip");
    expect(lineChips()).toHaveLength(3);
    const box = strip.parentElement;
    if (box === null) throw new Error("the strip is drawn outside a box of its own");
    expect(box.parentElement).toBe(row());
    expect(getComputedStyle(box).flexGrow).toBe("1");
    expect(getComputedStyle(box).minWidth).toBe("0");
    expect(getComputedStyle(strip).minWidth).toBe("0");
    expect(getComputedStyle(strip).overflowX).not.toMatch(/auto|scroll/);
    // A chip the line draws is never squeezed.
    for (const each of strip.children) expect(getComputedStyle(each).flexShrink).toBe("0");
    // Every other slot keeps its width but the search box, which gives way.
    const search = screen.getByRole("searchbox", { name: "Search asks" }).parentElement;
    expect(search === null ? undefined : getComputedStyle(search).flexGrow).toBe("1");
    for (const slot of row().children) {
      if (slot === box || slot === search) continue;
      expect(getComputedStyle(slot).flexShrink).toBe("0");
    }
  } finally {
    view.unmount();
    removeCss();
  }
});

test("below 640 px of the bar's width the bar is still one line, and its chips leave it", () => {
  const removeCss = withCss(HEIGHT_RULE_CSS);
  const restore = layOutBar({ width: 390 });
  const view = render(<Bar narrowings={["Type: Question", "Urgency: High"]} />);
  try {
    expect(getComputedStyle(row()).flexWrap).toBe("nowrap");
    const wrapping = Array.from(row().querySelectorAll("*")).filter(
      (node) => getComputedStyle(node).flexWrap === "wrap"
    );
    expect(wrapping).toEqual([]);
    expect(lineChips()).toEqual([]);
    expect(queryChip("Type: Question")).toBeNull();
    // Filter's badge stands for them.
    expect(within(row()).getByRole("button", { name: "Filter · 2" }).textContent).toBe("2");
  } finally {
    view.unmount();
    restore();
    removeCss();
  }
});

test("while a chip is on, Clear filters stands beside the trigger in its tinted group, outside the chips", async () => {
  const user = userEvent.setup();
  const clear = mock(() => {});
  const view = render(<Bar clear={clear} />);
  try {
    const button = screen.getByRole("button", { name: "Clear filters" });
    const trigger = screen.getByRole("button", { name: "Filter · 1" });
    expect(trigger.nextElementSibling).toBe(button);
    const strip = row().querySelector("[data-chip-strip]");
    expect(strip?.contains(button)).toBe(false);
    expect(strip?.parentElement?.contains(button)).toBe(false);
    // Before the chips in the reading order, as the trigger's group is.
    expect(
      strip === null ? 0 : button.compareDocumentPosition(strip) & Node.DOCUMENT_POSITION_FOLLOWING
    ).toBeTruthy();
    expect(button).not.toHaveAttribute("data-control");
    expect(button).toHaveAttribute("type", "button");
    await user.click(button);
    expect(clear).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
  }
});

test("with no chip on, a Clear filters given still stands right after the trigger, and the bar keeps its one line", () => {
  const removeCss = withCss(HEIGHT_RULE_CSS);
  // The project List's search text, which draws no chip and which its Clear filters clears.
  const view = render(<Bar clear={() => undefined} narrowings={[]} />);
  try {
    const button = screen.getByRole("button", { name: "Clear filters" });
    expect(screen.getByRole("button", { name: "Filter" }).nextElementSibling).toBe(button);
    expect(lineChips()).toEqual([]);
    expect(getComputedStyle(row()).flexWrap).toBe("nowrap");
  } finally {
    view.unmount();
    removeCss();
  }
});

test("the bar draws no Clear filters without clear, chips or no chips", () => {
  const view = render(<Bar clear={undefined} />);
  try {
    expect(screen.queryByRole("button", { name: "Clear filters" })).toBeNull();
    view.rerender(<Bar clear={undefined} narrowings={[]} />);
    expect(screen.queryByRole("button", { name: "Clear filters" })).toBeNull();
  } finally {
    view.unmount();
  }
});

test("Clear filters keeps the 44 px target, and gives it up from md only at the compact size", () => {
  const view = render(<Bar />);
  // Its height under `css`: a window below `md` (`SIZE_CSS` alone), or from it (with `md:`).
  const minHeight = (css: string) =>
    sizeUnder(screen.getByRole("button", { name: "Clear filters" }), css).minHeight;
  try {
    // happy-dom computes these lengths in px: 2.75rem is the 44 px target, 2rem 32 px.
    expect(minHeight(SIZE_CSS)).toBe("44px");
    expect(minHeight(`${SIZE_CSS}${MOUSE_CSS}`)).toBe("44px");
    view.rerender(<Bar size="compact" />);
    expect(minHeight(SIZE_CSS)).toBe("44px");
    expect(minHeight(`${SIZE_CSS}${MOUSE_CSS}`)).toBe("32px");
  } finally {
    view.unmount();
  }
});

// --- The chips' focus hand-off --------------------------------------------------------------------

/**
 * A bar whose chips each take their narrowing off when pressed, as every page's chips do, beside
 * a filter trigger counting them and a Clear filters that takes every one off at once. Given
 * `held`, a pressed chip is named as going at once - the page draws again with the chip still in
 * the bar - and is taken off only when the promise `held` hands it settles, as on a page whose
 * removal waits on its navigation.
 */
function ChipsBar({ held, names }: { held?: () => Promise<void>; names: readonly string[] }) {
  const [on, setOn] = useState(names);
  const [going, setGoing] = useState<string>();
  const takeOff = (name: string) => setOn((chips) => chips.filter((each) => each !== name));
  return (
    <>
      <ControlRow
        chips={on.map((label) => ({
          label,
          remove: () => {
            if (held === undefined) {
              takeOff(label);
              return;
            }
            setGoing(label);
            void held().then(() => takeOff(label));
          },
        }))}
        clear={on.length === 0 ? undefined : () => setOn([])}
        search={{ label: "Search", onChange: () => undefined, value: "" }}
        trigger={{
          children: () => null,
          count: on.length,
          label: "Filter",
          onOpenChange: () => undefined,
          open: false,
        }}
      />
      {going === undefined ? null : <span>{going} is going</span>}
    </>
  );
}

/** The chip removals `ChipsBar` holds, each committed when the test releases it. */
function heldRemovals(): { hold: () => Promise<void>; release: () => Promise<void> } {
  const waiting: (() => void)[] = [];
  return {
    hold: () => {
      const { promise, resolve } = Promise.withResolvers<void>();
      waiting.push(resolve);
      return promise;
    },
    release: () =>
      act(async () => {
        for (const resolve of waiting.splice(0)) resolve();
        await Promise.resolve();
      }),
  };
}

test("a chip taken off hands the focus to the chip now at its place", async () => {
  const user = userEvent.setup();
  const view = render(<ChipsBar names={["Alpha", "Beta", "Gamma"]} />);
  try {
    await user.click(chip("Beta"));
    expect(queryChip("Beta")).toBeNull();
    // Not the page's body, from which a keyboard reader would start again at its first link.
    expect(chip("Gamma")).toHaveFocus();
  } finally {
    view.unmount();
  }
});

test("the last chip taken off hands the focus to the chip before it", async () => {
  const user = userEvent.setup();
  const view = render(<ChipsBar names={["Alpha", "Beta", "Gamma"]} />);
  try {
    await user.click(chip("Gamma"));
    expect(queryChip("Gamma")).toBeNull();
    expect(chip("Beta")).toHaveFocus();
  } finally {
    view.unmount();
  }
});

test("the only chip taken off hands the focus to the filter trigger", async () => {
  const user = userEvent.setup();
  const view = render(<ChipsBar names={["Alpha"]} />);
  try {
    await user.click(chip("Alpha"));
    expect(lineChips()).toEqual([]);
    expect(screen.getByRole("button", { name: "Filter" })).toHaveFocus();
  } finally {
    view.unmount();
  }
});

test("Clear filters hands the focus to the filter trigger", async () => {
  const user = userEvent.setup();
  const view = render(<ChipsBar names={["Alpha", "Beta", "Gamma"]} />);
  try {
    await user.click(screen.getByRole("button", { name: "Clear filters" }));
    expect(lineChips()).toEqual([]);
    // The page's own: nothing differs now, so it gives no Clear filters.
    expect(screen.queryByRole("button", { name: "Clear filters" })).toBeNull();
    expect(screen.getByRole("button", { name: "Filter" })).toHaveFocus();
  } finally {
    view.unmount();
  }
});

test("Delete on a focused chip takes it off, as Enter and Space do, and hands the focus on", async () => {
  const user = userEvent.setup();
  const view = render(<ChipsBar names={["Alpha", "Beta"]} />);
  try {
    act(() => chip("Alpha").focus());
    await user.keyboard("{Delete}");
    expect(queryChip("Alpha")).toBeNull();
    expect(chip("Beta")).toHaveFocus();
    await user.keyboard("{Delete}");
    expect(lineChips()).toEqual([]);
    expect(screen.getByRole("button", { name: "Filter" })).toHaveFocus();
  } finally {
    view.unmount();
  }
});

/** Where the router is, for a row that reads whether a link was followed. */
function Location(): ReactNode {
  return <output data-testid="location">{useLocation().pathname}</output>;
}

test("Delete on a chip's link beside its remove control takes nothing off and follows nothing", async () => {
  const user = userEvent.setup();
  const remove = mock(() => {});
  // The Inbox's agent chip: the shared chip that clears the narrowing and, after it, the link to
  // the agent's live view.
  const view = render(
    <MemoryRouter future={FUTURE}>
      <ControlRow
        chips={[
          {
            label: "Asks from Planner",
            link: { label: "Open live conversation", to: "/agents/planner/live" },
            remove,
          },
        ]}
        trigger={{
          children: () => null,
          count: 1,
          label: "Filter",
          onOpenChange: () => undefined,
          open: false,
        }}
      />
      <Location />
    </MemoryRouter>
  );
  try {
    const live = screen.getByRole("link", { name: "Open live conversation" });
    act(() => live.focus());
    await user.keyboard("{Delete}");
    expect(screen.getByTestId("location").textContent).toBe("/");
    expect(remove).not.toHaveBeenCalled();
    expect(live).toHaveFocus();
  } finally {
    view.unmount();
  }
});

test("a removal that commits later hands the focus on then, whatever held it meanwhile, and moves nothing once the reader has moved it elsewhere", async () => {
  const user = userEvent.setup();
  const removals = heldRemovals();
  const view = render(
    <ChipsBar held={removals.hold} names={["Alpha", "Beta", "Gamma", "Delta"]} />
  );
  try {
    // The bar draws again at once with the chip still in it: the hand-off waits.
    await user.click(chip("Alpha"));
    expect(screen.getByText("Alpha is going")).toBeDefined();
    expect(chip("Alpha")).toHaveFocus();
    await removals.release();
    expect(queryChip("Alpha")).toBeNull();
    expect(chip("Beta")).toHaveFocus();
    // A press that leaves the focus on the body, as a pointer's click in Safari does: the
    // hand-off still waits for the removal rather than focus the chip that is going.
    act(() => chip("Beta").blur());
    act(() => chip("Gamma").click());
    expect(screen.getByText("Gamma is going")).toBeDefined();
    expect(document.activeElement).toBe(document.body);
    await removals.release();
    expect(queryChip("Gamma")).toBeNull();
    expect(chip("Delta")).toHaveFocus();
    // The reader moves to the search box before Beta's removal commits, and the focus stays.
    await user.click(chip("Beta"));
    const search = screen.getByRole("searchbox", { name: "Search" });
    await user.click(search);
    await removals.release();
    expect(queryChip("Beta")).toBeNull();
    expect(search).toHaveFocus();
  } finally {
    view.unmount();
  }
});

// --- +N more --------------------------------------------------------------------------------------

/** Four chips of 100 px. A 300 px line holds two of them and `+2 more`, which take 100 + 8 +
 *  100 + 8 + 60 = 276 px; a third chip would take it to 384. */
const FOUR = { Alpha: 100, Beta: 100, Delta: 100, Gamma: 100 };

test("chips past the line's room are drawn behind one +N more, named for what it opens", () => {
  const restore = layOutLine({ width: 300 }, FOUR);
  const view = render(<ChipsBar names={["Alpha", "Beta", "Gamma", "Delta"]} />);
  try {
    expect(lineChips()).toEqual(["Alpha", "Beta"]);
    const more = screen.getByRole("button", { name: "2 more filters" });
    expect(more).toHaveTextContent("+2 more");
    expect(more).toHaveAttribute("data-control", "chip");
    expect(more).toHaveAttribute("aria-haspopup", "dialog");
    expect(more).toHaveAttribute("aria-expanded", "false");
    // At the end of the chips, outside the strip, and after Clear filters, which stands in the
    // trigger's group.
    expect(document.querySelector("[data-chip-strip]")?.nextElementSibling).toBe(more);
    const button = screen.getByRole("button", { name: "Clear filters" });
    expect(button.compareDocumentPosition(more) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    // The chips left out are drawn nowhere until it opens.
    expect(queryChip("Gamma")).toBeNull();
    expect(queryChip("Delta")).toBeNull();
  } finally {
    view.unmount();
    restore();
  }
});

test("+N more opens a panel of the chips left out, with the focus on its first, and each chip is drawn once", async () => {
  const user = userEvent.setup();
  const restore = layOutLine({ width: 300 }, FOUR);
  const view = render(<ChipsBar names={["Alpha", "Beta", "Gamma", "Delta"]} />);
  try {
    const more = screen.getByRole("button", { name: "2 more filters" });
    await user.click(more);
    expect(panelChips("2 more filters")).toEqual(["Gamma", "Delta"]);
    expect(more).toHaveAttribute("aria-expanded", "true");
    expect(chip("Gamma")).toHaveFocus();
    // In the line or in the panel, never both.
    for (const name of ["Alpha", "Beta", "Gamma", "Delta"]) {
      expect(screen.getAllByRole("button", { name: `Remove ${name} filter` })).toHaveLength(1);
    }
  } finally {
    view.unmount();
    restore();
  }
});

test("Escape in the panel, and Tab past its last chip, close it and hand the focus back to +N more", async () => {
  const user = userEvent.setup();
  const restore = layOutLine({ width: 300 }, FOUR);
  const view = render(<ChipsBar names={["Alpha", "Beta", "Gamma", "Delta"]} />);
  try {
    const more = screen.getByRole("button", { name: "2 more filters" });
    await user.click(more);
    expect(chip("Gamma")).toHaveFocus();
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(more).toHaveFocus();
    expect(more).toHaveAttribute("aria-expanded", "false");
    // Opened again from the keyboard, and left by Tab past its last chip.
    await user.keyboard("{Enter}");
    expect(chip("Gamma")).toHaveFocus();
    await user.tab();
    expect(chip("Delta")).toHaveFocus();
    await user.tab();
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(more).toHaveFocus();
  } finally {
    view.unmount();
    restore();
  }
});

/** Walks the chips of the open `panel` by Tab, four forward and then four back, and answers the
 *  words of the chip each key puts the focus on, with how far the panel has scrolled then. */
async function walkChips(user: UserEvent, panel: HTMLElement): Promise<[string, number][]> {
  const walked: [string, number][] = [];
  for (const shift of [false, false, false, false, true, true, true, true]) {
    await user.tab({ shift });
    const focused = document.activeElement;
    walked.push([focused === null ? "nothing" : wordsOf(focused), panel.scrollTop]);
  }
  return walked;
}

// Here no engine scrolls the panel itself, as Firefox scrolls no chip already partly in sight.
test("a chip the focus reaches in the panel of +N more stands whole in the panel's box, which scrolls the least that shows it, and nothing else scrolls", async () => {
  const user = userEvent.setup();
  const names = ["Alpha", "Beta", "Gamma", "Delta", "Epsilon", "Zeta", "Eta"];
  const restore = layOutLine({ width: 300 }, Object.fromEntries(names.map((name) => [name, 100])));
  const view = render(<ChipsBar names={names} />);
  const scrolls = spyOn(Element.prototype, "scrollIntoView");
  try {
    await user.click(screen.getByRole("button", { name: "5 more filters" }));
    const panel = screen.getByRole("dialog", { name: "5 more filters" });
    // Five chips hold 8 + 5 × 52 px.
    const chips = within(panel).getAllByRole("button");
    layOutPanel(panel, 8 + 52 * chips.length, stacked(chips));
    expect(chip("Gamma")).toHaveFocus();
    // The chip each key puts the focus on, and how far the panel has scrolled then: the panel
    // shows 0-198 px of what it holds, and the fourth chip, Zeta, stands at 164-208.
    expect(await walkChips(user, panel)).toEqual([
      ["Delta", 0],
      ["Epsilon", 0],
      ["Zeta", 10],
      ["Eta", 62],
      // Already whole, so nothing scrolls.
      ["Zeta", 62],
      ["Epsilon", 62],
      // Back up, each chip stands at the box's top edge, so the first leaves the panel's 8 px of
      // padding above it out of sight.
      ["Delta", 60],
      ["Gamma", 8],
    ]);
    expect(scrolls).not.toHaveBeenCalled();
  } finally {
    scrolls.mockRestore();
    view.unmount();
    restore();
  }
});

// The same in the trigger's panel, which lists every chip at its top above the page's fields, and
// below 640 px of the bar's width is where a keyboard reader takes one off. That list stands in
// the panel's own content box (`contents`), not in the panel's box itself, so the box scrolled is
// the panel's, whatever stands between.
test("a chip the focus reaches at the top of the trigger's panel stands whole in the panel's box, which scrolls the least that shows it, and nothing else scrolls", async () => {
  const user = userEvent.setup();
  const restore = layOutBar({ width: 390 });
  const view = render(<Bar narrowings={["Alpha", "Beta", "Gamma", "Delta", "Epsilon"]} />);
  const scrolls = spyOn(Element.prototype, "scrollIntoView");
  try {
    await user.click(within(row()).getByRole("button", { name: "Filter · 5" }));
    const panel = screen.getByRole("dialog", { name: "Filter" });
    // Five chips hold 8 + 5 × 52 px.
    const chips = within(panel).getAllByRole("button");
    layOutPanel(panel, 8 + 52 * chips.length, stacked(chips));
    expect(chip("Alpha")).toHaveFocus();
    // The panel shows 0-198 px of what it holds, and the fourth chip, Delta, stands at 164-208.
    expect(await walkChips(user, panel)).toEqual([
      ["Beta", 0],
      ["Gamma", 0],
      ["Delta", 10],
      ["Epsilon", 62],
      // Already whole, so nothing scrolls.
      ["Delta", 62],
      ["Gamma", 62],
      // Back up, each chip stands at the box's top edge, so the first leaves the panel's 8 px of
      // padding above it out of sight.
      ["Beta", 60],
      ["Alpha", 8],
    ]);
    expect(scrolls).not.toHaveBeenCalled();
  } finally {
    scrolls.mockRestore();
    view.unmount();
    restore();
  }
});

test("a chip taken off in the panel goes, and the room it frees takes the next chip into the line as +N counts down", async () => {
  const user = userEvent.setup();
  // Two 100 px chips and `+3 more` take 276 px of a 300 px line, and the 200 px chip after them
  // does not fit. With it gone, the 16 px chip after it does: 100 + 8 + 100 + 8 + 16 + 8 + 60 is
  // 300 px.
  const restore = layOutLine(
    { width: 300 },
    { Alpha: 100, Beta: 100, Delta: 100, Gamma: 16, Wide: 200 }
  );
  const view = render(<ChipsBar names={["Alpha", "Beta", "Wide", "Gamma", "Delta"]} />);
  try {
    await user.click(screen.getByRole("button", { name: "3 more filters" }));
    expect(chip("Wide")).toHaveFocus();
    await user.keyboard("{Enter}");
    expect(queryChip("Wide")).toBeNull();
    expect(lineChips()).toEqual(["Alpha", "Beta", "Gamma"]);
    expect(screen.getByRole("button", { name: "1 more filter" })).toHaveTextContent("+1 more");
    expect(panelChips("1 more filter")).toEqual(["Delta"]);
    // The panel stands, so the focus stays in it, on the chip now at the place of the one taken
    // off.
    expect(chip("Delta")).toHaveFocus();
  } finally {
    view.unmount();
    restore();
  }
});

test("the panel's last chip taken off closes the panel, and the focus goes to the line's last chip", async () => {
  const user = userEvent.setup();
  // Two of three 100 px chips and `+1 more` fit a 300 px line; the two alone need no `+N more`.
  const restore = layOutLine({ width: 300 }, { Alpha: 100, Beta: 100, Gamma: 100 });
  const view = render(<ChipsBar names={["Alpha", "Beta", "Gamma"]} />);
  try {
    await user.click(screen.getByRole("button", { name: "1 more filter" }));
    expect(chip("Gamma")).toHaveFocus();
    // Delete takes a chip off in the panel as it does in the line.
    await user.keyboard("{Delete}");
    expect(queryChip("Gamma")).toBeNull();
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.queryByRole("button", { name: /more filters?$/ })).toBeNull();
    expect(lineChips()).toEqual(["Alpha", "Beta"]);
    // Not the page's body: the chip before the place the panel's chips stood at.
    expect(chip("Beta")).toHaveFocus();
  } finally {
    view.unmount();
    restore();
  }
});

test("a line that widens takes back the chips that now fit, and one that narrows hands them to +N more again", () => {
  const sizes = installResizeObserver();
  const room = { width: 300 };
  const restore = layOutLine(room, FOUR);
  const view = render(<ChipsBar names={["Alpha", "Beta", "Gamma", "Delta"]} />);
  try {
    expect(lineChips()).toEqual(["Alpha", "Beta"]);
    // Four 100 px chips and their three gaps take 424 px: a 440 px line holds them all, and
    // needs no `+N more`.
    room.width = 440;
    sizes.resize();
    expect(lineChips()).toEqual(["Alpha", "Beta", "Gamma", "Delta"]);
    expect(screen.queryByRole("button", { name: /more filters?$/ })).toBeNull();
    room.width = 300;
    sizes.resize();
    expect(lineChips()).toEqual(["Alpha", "Beta"]);
    expect(screen.getByRole("button", { name: "2 more filters" })).toHaveTextContent("+2 more");
  } finally {
    view.unmount();
    restore();
    sizes.restore();
  }
});

test("where the line has room for less than one whole chip it draws +N more alone, which lists every chip", async () => {
  const user = userEvent.setup();
  const long = "Label: a name that runs past the line";
  const restore = layOutLine({ width: 300 }, { Beta: 50, [long]: 400 });
  const view = render(<ChipsBar names={[long, "Beta"]} />);
  try {
    // No chip is drawn cut short: the first one is wider than the line, so none stands in it.
    expect(lineChips()).toEqual([]);
    const more = screen.getByRole("button", { name: "2 more filters" });
    expect(more).toHaveTextContent("+2 more");
    await user.click(more);
    expect(panelChips("2 more filters")).toEqual([long, "Beta"]);
  } finally {
    view.unmount();
    restore();
  }
});

test("a chip given a link is one chip of the line with its link, as the Inbox's agent chip comes before the rest", () => {
  const restore = layOutLine(
    { width: 300 },
    { "Asks from Planner": 100, "Type: Question": 100, "Urgency: High": 100 }
  );
  const view = render(
    <MemoryRouter future={FUTURE}>
      <ControlRow
        chips={[
          {
            label: "Asks from Planner",
            link: { label: "Open live conversation", to: "/agents/planner/live" },
            remove: () => undefined,
          },
          { label: "Type: Question", remove: () => undefined },
          { label: "Urgency: High", remove: () => undefined },
        ]}
      />
    </MemoryRouter>
  );
  try {
    expect(lineChips()).toEqual(["Asks from Planner", "Type: Question"]);
    // The link stands in the line with its chip, and goes behind `+N more` with it.
    expect(screen.getByRole("link", { name: "Open live conversation" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "1 more filter" })).toHaveTextContent("+1 more");
  } finally {
    view.unmount();
    restore();
  }
});

// --- The bar's other focus hand-offs ---------------------------------------------------------------

/** A bar whose actions each take themselves away when pressed, as the Inbox's Mark all read does
 *  once nothing is left unread, beside a filter trigger, with no other control after them. */
function SelfRemovingActions({ names }: { names: readonly string[] }) {
  const [on, setOn] = useState(names);
  return (
    <ControlRow
      actions={on.map((name) =>
        pressAction(name, {
          onPress: () => setOn((left) => left.filter((other) => other !== name)),
        })
      )}
      search={{ label: "Search", onChange: () => undefined, value: "" }}
      trigger={{
        children: () => null,
        count: 0,
        label: "Filter",
        onOpenChange: () => undefined,
        open: false,
      }}
    />
  );
}

test("an action that takes itself away hands the focus to the next control after it, then the one before it, then the filter trigger", async () => {
  const user = userEvent.setup();
  const view = render(
    <SelfRemovingActions names={["Mark all read", "Archive all", "Snooze all"]} />
  );
  try {
    act(() => screen.getByRole("button", { name: "Mark all read" }).focus());
    await user.keyboard("{Enter}");
    expect(screen.queryByRole("button", { name: "Mark all read" })).toBeNull();
    // Not the page's body, from which a keyboard reader would start again at its first link.
    expect(screen.getByRole("button", { name: "Archive all" })).toHaveFocus();
    // The last control has no next one: the one before it.
    act(() => screen.getByRole("button", { name: "Snooze all" }).focus());
    await user.keyboard("{Enter}");
    expect(screen.getByRole("button", { name: "Archive all" })).toHaveFocus();
    // Its only control: the slot goes with it, and the focus goes to the trigger.
    await user.keyboard("{Enter}");
    expect(screen.queryByRole("button", { name: "Archive all" })).toBeNull();
    expect(screen.getByRole("button", { name: "Filter" })).toHaveFocus();
  } finally {
    view.unmount();
  }
});

// A hand-off passes over every control that takes typed text: the reader never chose such a
// field, and their next key would be typed into it. So an action's going in a bar with no
// trigger - the priorities table's - leaves the focus out of its search box.
test("a control of the bar that takes itself away hands the focus past the search box, which takes typed text, and never into it", async () => {
  const user = userEvent.setup();
  const landed: Record<string, string | null> = {};
  for (const withTrigger of [true, false]) {
    const Page = () => {
      const [shown, setShown] = useState(true);
      return (
        <ControlRow
          actions={shown ? [pressAction("New priority", { onPress: () => setShown(false) })] : []}
          search={{ label: "Search priorities", onChange: () => undefined, value: "" }}
          trigger={
            withTrigger
              ? {
                  children: () => null,
                  count: 0,
                  label: "Filter",
                  onOpenChange: () => undefined,
                  open: false,
                }
              : undefined
          }
        />
      );
    };
    const view = render(<Page />);
    try {
      act(() => screen.getByRole("button", { name: "New priority" }).focus());
      await user.keyboard("{Enter}");
      expect(screen.queryByRole("button", { name: "New priority" })).toBeNull();
      const focused = document.activeElement;
      landed[withTrigger ? "with a trigger" : "with no trigger"] =
        focused === null || focused === document.body
          ? "the page"
          : (focused.getAttribute("aria-label") ?? focused.textContent);
    } finally {
      view.unmount();
    }
  }
  expect(landed).toEqual({ "with a trigger": "Filter", "with no trigger": "the page" });
});

/** The shell's main content holding a bar whose actions are exactly `names`, as a live update
 *  leaves them. */
function LivePage({ names }: { names: readonly string[] }) {
  return (
    <main>
      <ControlRow
        actions={names.map((name) => pressAction(name))}
        trigger={{
          children: () => null,
          count: 0,
          label: "Filter",
          onOpenChange: () => undefined,
          open: false,
        }}
      />
    </main>
  );
}

// INFRA-3135's review round 1, R10: a live update - the last unread item read in another tab -
// takes Mark all read away while it holds the focus, after the reader scrolled it out of their
// sight. The bar's hand-off hands the focus to no control - a live update is no press of the
// reader's - so neither the next action takes it nor the page is scrolled back to the bar.
test("an action the reader scrolled out of sight goes with a live update and hands the focus to nothing", async () => {
  const view = render(<LivePage names={["Mark all read", "Archive all"]} />);
  try {
    const mark = screen.getByRole("button", { name: "Mark all read" });
    act(() => mark.focus());
    // The page scrolled the bar 2,000 px above the window's top edge.
    mark.getBoundingClientRect = () => new DOMRect(0, -2_000, 120, 44);
    act(() => {
      document.dispatchEvent(new Event("scroll"));
    });
    view.rerender(<LivePage names={["Archive all"]} />);
    await afterFrameAndTasks();
    expect(screen.queryByRole("button", { name: "Mark all read" })).toBeNull();
    expect(screen.getByRole("button", { name: "Archive all" })).not.toHaveFocus();
    expect(document.activeElement).toBe(document.body);
  } finally {
    view.unmount();
  }
});

/** A bar whose hint panel holds one control, which takes itself away when pressed. */
function PanelInBar() {
  const [open, setOpen] = useState(true);
  const [deleted, setDeleted] = useState(false);
  return (
    <ControlRow
      hint={{
        children: () =>
          deleted ? (
            <p>No group.</p>
          ) : (
            <button onClick={() => setDeleted(true)} type="button">
              Delete group
            </button>
          ),
        label: "i",
        onOpenChange: setOpen,
        open,
        title: "Session groups",
      }}
      trigger={{
        children: () => null,
        count: 0,
        label: "Filter",
        onOpenChange: () => undefined,
        open: false,
      }}
    />
  );
}

// A control of a panel the bar opens is the panel's, not the bar's: its going hands the focus on
// inside the panel (`PanelTrigger`), and the bar, which hears the focus through React's tree,
// hands it to none of its own controls, so the filter trigger in sight does not take it.
test("a control of a panel the bar opens hands its focus to no control of the bar", async () => {
  const user = userEvent.setup();
  const view = render(<PanelInBar />);
  try {
    const panel = screen.getByRole("dialog", { name: "Session groups" });
    const deleteGroup = within(panel).getByRole("button", { name: "Delete group" });
    act(() => deleteGroup.focus());
    await user.keyboard("{Enter}");
    expect(screen.queryByRole("button", { name: "Delete group" })).toBeNull();
    expect(row().contains(document.activeElement)).toBe(false);
    expect(panel.contains(document.activeElement)).toBe(true);
  } finally {
    view.unmount();
  }
});

// --- The bar's looks and sizes ---------------------------------------------------------------------

test("every button of the bar is quiet, with no border and no fill at rest, but the chosen part of the switch and the main action", () => {
  const view = render(<Bar narrowings={[]} />);
  try {
    const main = screen.getByRole("button", { name: "New message" });
    const chosen = screen.getByRole("button", { name: "All" });
    const quiet = Array.from(row().querySelectorAll<HTMLElement>("button")).filter(
      (button) => button !== main && button !== chosen
    );
    expect(quiet.map((button) => button.getAttribute("aria-label") ?? button.textContent)).toEqual([
      "Blocked on you 2",
      "Filter",
      "Save view",
      "New document",
      "About this view",
      "Columns",
      "View",
      "More",
    ]);
    for (const button of quiet) {
      expect(button).toHaveClass("bg-transparent");
      expect(button.className.split(/\s+/).filter((name) => /^border(-\d+)?$/.test(name))).toEqual(
        []
      );
    }
    expect(chosen).not.toHaveClass("bg-transparent");
    expect(main).not.toHaveClass("bg-transparent");
  } finally {
    view.unmount();
  }
});

test("the search box's slot is never narrower than its placeholder, in the box's own font", () => {
  const view = render(<Bar />);
  try {
    const box = screen.getByRole("searchbox", { name: "Search asks" });
    const sizer = box.parentElement?.firstElementChild;
    if (!(sizer instanceof HTMLElement) || sizer === box) {
      throw new Error("the search box's slot holds no placeholder of its own");
    }
    expect(sizer).toHaveAttribute("aria-hidden", "true");
    expect(sizer.textContent).toBe(PLACEHOLDER);
    // The box's own font at each width - `text-sm`, and the 16 px `styles.css` gives every field
    // below 1280 px - on one line, with the box's own side padding and border, seen by nobody.
    for (const name of ["text-sm", "max-xl:text-base", "whitespace-pre", "px-3", "border-x"]) {
      expect(sizer).toHaveClass(name);
    }
    expect(box).toHaveClass("px-3");
    expect(sizer).toHaveClass("invisible");
    expect(sizer).toHaveClass("h-0");
  } finally {
    view.unmount();
  }
});

test("the bar's controls take their size from the page", () => {
  const view = render(<Bar size="compact" />);
  try {
    for (const control of [
      screen.getByRole("searchbox", { name: "Search asks" }),
      screen.getByRole("button", { name: "All" }),
      screen.getByRole("button", { name: "Filter · 1" }),
      chip("Type: Question"),
      screen.getByRole("button", { name: "Save view" }),
      screen.getByRole("button", { name: "New document" }),
      screen.getByRole("button", { name: "Columns" }),
      screen.getByRole("button", { name: "View" }),
      screen.getByRole("button", { name: "New message" }),
    ]) {
      expect(control).toHaveClass("md:min-h-8");
    }
    view.rerender(<Bar size="comfortable" />);
    expect(screen.getByRole("button", { name: "New message" })).not.toHaveClass("md:min-h-8");
  } finally {
    view.unmount();
  }
});
