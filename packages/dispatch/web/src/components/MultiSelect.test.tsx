import { expect, test } from "bun:test";
// fireEvent also gives one row's press below a click with no pointer press before it, which
// user-event has no form for either.
// biome-ignore lint/style/noRestrictedImports: the two outside-press sites below are a pointerdown with no release, which user-event has no form for.
import { fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";

import { MultiSelect } from "./MultiSelect";

function Host({
  error,
  onCreate,
  options,
}: {
  error?: string;
  onCreate?: (value: string) => void;
  options: string[];
}) {
  const [open, setOpen] = useState(false);
  const [selected, setSelected] = useState<string[]>([]);
  return (
    <>
      <MultiSelect
        emptyMessage="Nothing here."
        error={error}
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

// The rows scroll under a footer pinned to the popover's bottom edge, so an error is drawn in
// that footer, in view of the row just pressed, beside Clear when there is one, and announced.
test("an error is announced in the footer pinned below the rows, beside Clear", async () => {
  const user = userEvent.setup();
  const view = render(<Host error="At most one label." options={["api", "docs"]} />);
  try {
    await user.click(screen.getByRole("button", { name: "Labels" }));
    expect(screen.getByRole("alert").textContent).toBe("At most one label.");
    expect(screen.getByRole("alert").closest(".sticky")).not.toBeNull();

    await user.click(screen.getByRole("option", { name: "api" }));
    const footer = screen.getByRole("button", { name: "Clear labels" }).closest(".sticky");
    expect(footer?.contains(screen.getByRole("alert"))).toBe(true);
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

/** A host that keeps the popover open as the search box's Tab asks it to close, so Tab goes on
 *  from the search box to the rows, the Create row and Clear: buttons, which a screen reader's
 *  cursor focuses too. With `pickedFirst` it orders the rows as the labels' and the components'
 *  hosts do (`IssueLabels`, `ComponentPicker`): the picked ones first, each part by its name. With
 *  `dropPicked` it lists only the values not yet picked, so a row goes as it is picked. It stands
 *  for the row a press takes off the page in Details: a label only the open draft holds, which
 *  `IssueLabels` lists only while the draft holds it, goes as the reader unpicks it. */
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
      // From the search box past the two rows to Clear.
      await user.tab();
      await user.tab();
      await user.tab();
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
    await user.tab();
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
  const user = userEvent.setup();
  const view = render(<OpenHost options={["api", "docs"]} />);
  try {
    await user.tab();
    await user.tab();
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
    await user.tab();
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
