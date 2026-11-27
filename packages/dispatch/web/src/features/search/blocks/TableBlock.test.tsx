import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { renderWithQueries } from "../../../__tests__/query-provider";
import { api } from "../../../api/client";
import { CONTROL } from "../../../components/control-names";
import { PriorityEditor } from "../../issue/PriorityControl";
import type { BlockOf } from "./block-cells";
import { TableBlock } from "./TableBlock";

// Each issue chip reads its issue's summary, the one read every reference to the issue shares.
let summaries: { mockRestore: () => void } | undefined;
beforeEach(() => {
  summaries = spyOn(api, "getIssueSummary").mockImplementation(async (key: string) => ({
    documents: [],
    key,
    project: "DEMO",
    status: "todo",
    title: `The issue ${key}`,
  }));
});
afterEach(() => summaries?.mockRestore());

/** The words of each body row's cells. */
function cellWords(): readonly (readonly (string | null)[])[] {
  return within(screen.getByTestId("going-block-table"))
    .getAllByRole("row")
    .slice(1)
    .map((row) =>
      within(row)
        .getAllByRole("cell")
        .map((cell) => cell.textContent)
    );
}

/** Waits until each of `count` issue chips shows its issue's title, read from its summary, so
 *  no read lands after the row has ended. */
async function chipsRead(count: number): Promise<void> {
  await waitFor(() => expect(screen.getAllByText(/^The issue DEMO-\d+$/u)).toHaveLength(count));
}

// A table the command bar's agent draws of issues counted by priority, as the server's chain
// answers it: each priority as the number the issue reads carry, and the issues with none in the
// group "none". The person reads each priority as Dispatch writes it everywhere else, and a set
// one in the form the issue page draws it, the badge of its priority menu (review a5).
test("a table of issues by priority shows each priority as Dispatch writes it", () => {
  const block: BlockOf<"table"> = {
    kind: "table",
    title: "Open bugs by priority",
    spec: { source: "run", params: {}, table: "the chain's answer", columns: ["priority", "bugs"] },
    rows: [
      { priority: 1, bugs: 5 },
      { priority: 2, bugs: 2 },
      { priority: "none", bugs: 1 },
    ],
  };
  render(<TableBlock block={block} />);
  expect(cellWords()).toEqual([
    ["P1", "5"],
    ["P2", "2"],
    ["No priority", "1"],
  ]);
  // The columns are headed as the issue page names its fields, never by their stored names.
  expect(
    within(screen.getByTestId("going-block-table"))
      .getAllByRole("columnheader")
      .map((head) => head.textContent)
  ).toEqual(["Priority", "Bugs"]);
  const issuePage = render(
    <PriorityEditor
      issueKey="DEMO-1"
      write={{
        failed: false,
        pending: false,
        retry: undefined,
        shown: 1,
        stopped: undefined,
        submit: () => {},
      }}
    />
  );
  const pageBadge = within(issuePage.container).getByText("P1", { selector: "span" });
  expect(within(screen.getByTestId("going-block-table")).getByText("P1").className).toBe(
    pageBadge.className
  );
});

// A table of a run's saved results may name any field of their rows (going/blocks.go
// `RunBlock.Draw`), and an issue's labels and the issues that block it are lists of words. The
// person reads each list as its items, never as JSON, and an empty list as an empty cell.
test("a table shows a list of words as its items, joined by commas", () => {
  const block: BlockOf<"table"> = {
    kind: "table",
    title: "Dome work",
    spec: {
      source: "run",
      params: {},
      table: "the chain's answer",
      columns: ["title", "labels", "blocked_by"],
    },
    rows: [
      { title: "Fit a motor to the dome", labels: ["dome", "motor"], blocked_by: ["DEMO-2"] },
      { title: "Polish the eyepiece", labels: [], blocked_by: null },
    ],
  };
  render(<TableBlock block={block} />);
  expect(cellWords()).toEqual([
    ["Fit a motor to the dome", "dome, motor", "DEMO-2"],
    ["Polish the eyepiece", "", ""],
  ]);
});

// A search hit's owner is an object: an issue, a project or a person. The person reads it by the
// field that names it - its title, its name, its login or its key, in that order - and reads its
// JSON only where it has none of them, as an actor, which names itself by kind and id.
test("a table shows an object by the field that names it, and its JSON only where none does", () => {
  const block: BlockOf<"table"> = {
    kind: "table",
    title: "Hits",
    spec: { source: "run", params: {}, table: "the chain's answer", columns: ["snippet", "owner"] },
    rows: [
      { snippet: "a", owner: { key: "DEMO-1", kind: "issue", title: "Fit a motor to the dome" } },
      { snippet: "b", owner: { key: "OPS", kind: "project", name: "Operations" } },
      { snippet: "c", owner: { kind: "user", login: "alice" } },
      { snippet: "d", owner: { key: "DEMO-2" } },
      { snippet: "e", owner: { id: "alice", kind: "user" } },
    ],
  };
  render(<TableBlock block={block} />);
  expect(cellWords()).toEqual([
    ["a", "Fit a motor to the dome"],
    ["b", "Operations"],
    ["c", "alice"],
    ["d", "DEMO-2"],
    ["e", '{"id":"alice","kind":"user"}'],
  ]);
});

// Design v2, state 5: a status by its word, at most 8 rows with "Show all 42 ›" under them, and
// the first cell of a row that is an issue drawn as that issue's chip, a link to it. The link
// stays: the server's steps put each key there from the run's own reads, never from the model's
// words, so it is to an issue the run read, as an answer's item frame is.
const statuses = ["todo", "in_progress", "needs_review", "done"];
const issues: BlockOf<"table"> = {
  kind: "table",
  title: "DEMO issues",
  spec: { source: "run", params: {}, table: "issues", columns: ["key", "status"] },
  rows: Array.from({ length: 10 }, (_, index) => ({
    key: `DEMO-${index + 1}`,
    status: statuses[index % statuses.length],
  })),
};

test("a table shows 8 rows, its statuses in words and its issues as chips, then Show all", async () => {
  const user = userEvent.setup();
  renderWithQueries(<TableBlock block={issues} />);
  const rows = cellWords();
  expect(rows).toHaveLength(8);
  expect(rows.slice(0, 4).map((row) => row[1])).toEqual([
    "Todo",
    "In progress",
    "Needs review",
    "Done",
  ]);
  const chip = screen.getAllByTestId("going-block-item")[0] as HTMLElement;
  expect(chip).toHaveAttribute("href", "/issues/DEMO-1");
  expect(within(chip).getByTestId("item-chip")).toHaveTextContent("DEMO-1");
  // The header stays on top while the rows scroll under it, and the column of issues is headed
  // "Issue", what it holds, not the stored field "key".
  const table = screen.getByTestId("going-block-table");
  expect(table.querySelector("thead")?.className).toContain("sticky");
  expect(
    within(table)
      .getAllByRole("columnheader")
      .map((head) => head.textContent)
  ).toEqual(["Issue", "Status"]);
  // The table is the shared one (`TableShell`), in its own bordered box, named by the block's
  // title.
  expect(table.querySelector(`[data-control="${CONTROL.tableShell}"]`)).not.toBeNull();
  expect(within(table).getByRole("table", { name: "DEMO issues" })).toBeTruthy();
  // With no conversation to open it over, Show all shows every row in place.
  const all = screen.getByTestId("going-block-show-all");
  expect(all).toHaveTextContent("Show all 10 ›");
  await user.click(all);
  expect(cellWords()).toHaveLength(10);
  expect(screen.queryByTestId("going-block-show-all")).toBeNull();
  await chipsRead(10);
});

test("Show all calls the conversation's own opener where one is given, and a whole table shows every row", async () => {
  const user = userEvent.setup();
  let opened = 0;
  const { unmount } = renderWithQueries(
    <TableBlock block={issues} onShowAll={() => (opened += 1)} />
  );
  await user.click(screen.getByTestId("going-block-show-all"));
  expect(opened).toBe(1);
  expect(cellWords()).toHaveLength(8);
  unmount();
  renderWithQueries(<TableBlock block={issues} whole />);
  expect(cellWords()).toHaveLength(10);
  expect(screen.queryByTestId("going-block-show-all")).toBeNull();
  await chipsRead(10);
});
