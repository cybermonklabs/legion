import { expect, test } from "bun:test";
import type { Column } from "@tanstack/react-table";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { columnFilterChip, columnFilterChips, filterChips } from "./filter-chips";

/** A table column as the chips read one: its header, what its filter holds, and what setting
 *  that filter was asked for. */
function column(
  id: string,
  header: string,
  value: unknown,
  options: { readonly show?: (value: string) => string; readonly wrote?: unknown[] } = {}
): Column<unknown, unknown> {
  return {
    columnDef: { header, meta: options.show === undefined ? {} : { show: options.show } },
    getFilterValue: () => value,
    id,
    setFilterValue: (next: unknown) => options.wrote?.push(next),
  } as unknown as Column<unknown, unknown>;
}

test("each narrowing that is on is one chip named Remove <its text> filter, whose press takes that one off", async () => {
  const user = userEvent.setup();
  const removed: string[] = [];
  const view = render(
    <div>
      {filterChips([
        { key: "label:docs", label: "Label: docs", remove: () => removed.push("docs") },
        { label: "Author alice", remove: () => removed.push("alice") },
        { label: "Needs you", remove: () => removed.push("needs you") },
      ])}
    </div>
  );
  try {
    const chips = screen.getAllByRole("button");
    expect(chips.map((chip) => chip.getAttribute("aria-label"))).toEqual([
      "Remove Label: docs filter",
      "Remove Author alice filter",
      "Remove Needs you filter",
    ]);
    const author = screen.getByRole("button", { name: "Remove Author alice filter" });
    expect(author).toHaveAttribute("data-control", "chip");
    // A narrowing that is on: the selected fill, and its whole text where it is cut.
    expect(author).toHaveAttribute("aria-pressed", "true");
    expect(author.textContent).toContain("Author alice");
    await user.click(author);
    expect(removed).toEqual(["alice"]);
  } finally {
    view.unmount();
  }
});

test("no narrowing on is no chips at all, which the row reads as nothing to draw", () => {
  expect(filterChips([])).toBeUndefined();
});

// The List's `only=` chip says more than it is named for: its name is the narrowing, its body
// adds how many of the picked issues are listed.
test("a chip whose body differs from its name shows that text and is still named for its label", () => {
  const view = render(
    <div>
      {filterChips([
        {
          label: "Showing: Last week",
          text: "Showing: Last week · 2 of 3 issues",
          remove: () => undefined,
        },
      ])}
    </div>
  );
  try {
    const chip = screen.getByRole("button", { name: "Remove Showing: Last week filter" });
    expect(chip.textContent).toContain("Showing: Last week · 2 of 3 issues");
    // The whole of it rides the text's own `title` (`TruncatedText`), which is where a chip
    // cut by a narrow row says what it holds.
    expect(chip.querySelector("[title]")).toHaveAttribute(
      "title",
      "Showing: Last week · 2 of 3 issues"
    );
    expect(chip).toHaveAttribute("aria-pressed", "true");
  } finally {
    view.unmount();
  }
});

// A column's filter is set on its header, and the row draws its chip whether that header is on
// the page or the reader hid the column: the chip is then the one place the narrowing shows.
test("a column filter that is on is one chip, in the words its header lists, whose press clears it", async () => {
  const user = userEvent.setup();
  const wrote: unknown[] = [];
  const columns = [
    column("priority", "Priority", ["0", "1"], { show: (value) => `P${value}`, wrote }),
    column("epic", "Epic", []),
    column("key", "Key", ""),
    column("title", "Title", "rollout"),
  ];
  const view = render(<div>{filterChips(columnFilterChips(columns))}</div>);
  try {
    expect(screen.getAllByRole("button").map((chip) => chip.getAttribute("aria-label"))).toEqual([
      "Remove Priority: P0, P1 filter",
      "Remove Title: rollout filter",
    ]);
    await user.click(screen.getByRole("button", { name: "Remove Priority: P0, P1 filter" }));
    expect(wrote).toEqual([undefined]);
  } finally {
    view.unmount();
  }
});

// A page whose column means more than its own picks says what taking the chip off leaves.
test("a column chip takes the removal its page gives it", async () => {
  const user = userEvent.setup();
  const wrote: unknown[] = [];
  const status = column("status", "Status", ["done"], { wrote });
  const view = render(
    <div>
      {filterChips([columnFilterChip(status, ["done"], () => status.setFilterValue(["todo"]))])}
    </div>
  );
  try {
    await user.click(screen.getByRole("button", { name: "Remove Status: done filter" }));
    expect(wrote).toEqual([["todo"]]);
  } finally {
    view.unmount();
  }
});
