import { expect, test } from "bun:test";
// biome-ignore lint/style/noRestrictedImports: the two outside-press sites below are a pointerdown with no release, which user-event has no form for.
import { fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";

import { MultiSelect } from "./MultiSelect";

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
 *  cursor focuses too. */
function OpenHost({
  onCreate,
  options,
}: {
  onCreate?: (value: string) => void;
  options: string[];
}) {
  const [selected, setSelected] = useState<string[]>([]);
  return (
    <>
      <MultiSelect
        emptyMessage="Nothing here."
        label="Labels"
        onChange={setSelected}
        onCreate={onCreate}
        onOpenChange={() => undefined}
        open
        options={options}
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
