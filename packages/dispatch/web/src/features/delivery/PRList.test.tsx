import { afterEach, expect, spyOn, test } from "bun:test";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import type { DeliveryPR } from "../../api/types";
import { SORT_MARK_WIDTH } from "../../components/DataTable";
import { tableStorageKey } from "../project/issues-table-storage";
import { PRList } from "./PRList";

const base: DeliveryPR = {
  additions: 1,
  author: "octocat",
  components: [],
  created_at: "2024-06-01T00:00:00Z",
  deletions: 1,
  deploy_run: null,
  deployed_at: null,
  deployed_status: "waiting",
  first_commit_at: null,
  id: "acme/widgets#1",
  issue: null,
  issue_title: null,
  merged_at: "2024-06-01T01:00:00Z",
  number: 1,
  parent_agent: null,
  partial: false,
  priority: null,
  repo: "acme/widgets",
  rework: false,
  sessions: [],
  title: "feat: widget 1",
  unfetchable_reason: null,
  url: "https://github.com/acme/widgets/pull/1",
};

/** Where the list keeps the reader's widths, order and visibility of its columns. */
const LAYOUT_KEY = tableStorageKey("delivery-pr-list");

const restores: (() => void)[] = [];
afterEach(() => {
  cleanup();
  for (const restore of restores.splice(0)) restore();
  localStorage.removeItem(LAYOUT_KEY);
});

/** `count` pull requests, oldest merge first, each a minute after the one before. */
function prs(count: number): DeliveryPR[] {
  return Array.from({ length: count }, (_, i) => ({
    ...base,
    id: `acme/widgets#${i + 1}`,
    merged_at: new Date(Date.parse("2024-06-01T00:00:00Z") + (i + 1) * 60_000).toISOString(),
    number: i + 1,
    title: `feat: widget ${i + 1}`,
    url: `https://github.com/acme/widgets/pull/${i + 1}`,
  }));
}

function renderList(
  rows: DeliveryPR[],
  onSelect = (_id: string) => {},
  onSelectRun = (_id: number) => {},
  selectedId: string | undefined = undefined
) {
  return render(
    <PRList
      allRepos={["acme/widgets"]}
      colorBy="repo"
      colorScale={new Map()}
      sessionGroups={[]}
      onSelect={onSelect}
      onSelectRun={onSelectRun}
      prs={rows}
      selectedId={selectedId}
    />
  );
}

/** The rows the list draws, by their pull request, in order. */
function listed(): string[] {
  const table = screen.getByRole("table", { name: "Merged pull requests" });
  return [...table.querySelectorAll("tbody [data-row]")].map(
    (row) => row.getAttribute("data-row") ?? ""
  );
}

test("the list is a table of the one table core: a row per PR, newest merge first, and a header sorts it", async () => {
  const user = userEvent.setup();
  renderList(prs(3));
  expect(listed()).toEqual(["acme/widgets#3", "acme/widgets#2", "acme/widgets#1"]);
  // No row of the list is an issue's.
  expect(document.querySelector("[data-issue-row]")).toBeNull();
  const merged = screen.getByRole("columnheader", { name: /^Merged/ });
  expect(merged).toHaveAttribute("aria-sort", "descending");

  await user.click(within(merged).getByRole("button"));
  expect(listed()).toEqual(["acme/widgets#1", "acme/widgets#2", "acme/widgets#3"]);
});

test("a list with no PR says so", () => {
  renderList([]);
  expect(screen.getByText("No PRs in the current filter/window.")).toBeInTheDocument();
});

test("the rows are one keyboard stop: arrows move between them, Home and End jump to the ends, and Enter opens the focused row", async () => {
  const user = userEvent.setup();
  const opened: string[] = [];
  renderList(prs(5), (id) => opened.push(id));
  const rows = () => [...document.querySelectorAll<HTMLElement>("tbody [data-row]")];
  expect(rows().filter((row) => row.tabIndex === 0)).toEqual([rows()[0]]);

  rows()[0]?.focus();
  await user.keyboard("{ArrowDown}{ArrowDown}");
  expect(document.activeElement?.getAttribute("data-row")).toBe("acme/widgets#3");
  expect(rows().filter((row) => row.tabIndex === 0)).toEqual([rows()[2]]);
  await user.keyboard("{End}");
  expect(document.activeElement?.getAttribute("data-row")).toBe("acme/widgets#1");
  await user.keyboard("{ArrowDown}");
  expect(document.activeElement?.getAttribute("data-row")).toBe("acme/widgets#1");
  await user.keyboard("{Home}");
  expect(document.activeElement?.getAttribute("data-row")).toBe("acme/widgets#5");

  await user.keyboard("{Enter}");
  expect(opened).toEqual(["acme/widgets#5"]);
});

test("a deployed row's 'deployed' opens its deploy run, the row's own click opens the PR, and the open PR's row is selected", async () => {
  const user = userEvent.setup();
  const opened: string[] = [];
  const runs: number[] = [];
  renderList(
    [
      {
        ...base,
        deploy_run: 500,
        deployed_at: "2024-06-01T01:40:00Z",
        deployed_status: "deployed",
      },
    ],
    (id) => opened.push(id),
    (id) => runs.push(id),
    "acme/widgets#1"
  );
  await user.click(screen.getByRole("button", { name: "Open deploy run #500" }));
  expect(runs).toEqual([500]);
  expect(opened).toEqual([]);

  await user.click(screen.getByText("feat: widget 1"));
  expect(opened).toEqual(["acme/widgets#1"]);
  expect(document.querySelector('[data-row="acme/widgets#1"]')).toHaveAttribute(
    "aria-selected",
    "true"
  );
});

// dispatch://INFRA-2918/spec#b-r7: on 2026-11-17 the list cut three headers even unsorted, drawn
// at 60, 68 and 20 px against their natural 92, 71 and 55. Here the reader's stored widths are
// those cut ones (Rework's at 36, the narrowest a layout stores), and each header still draws its
// label and its sort mark's slot whole, sorted or not. happy-dom lays nothing out: each label is
// given its natural width where the header reads it, a range over what it holds.
test("its Parent agent, Lead time and Rework headers draw at their natural width, sorted and unsorted", async () => {
  const user = userEvent.setup();
  const natural: Readonly<Record<string, number>> = {
    "Lead time": 71,
    "Parent agent": 92,
    Rework: 55,
  };
  const range = spyOn(Range.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Range
  ) {
    return new DOMRect(0, 0, natural[this.startContainer.textContent ?? ""] ?? 0, 16);
  });
  restores.push(() => range.mockRestore());
  localStorage.setItem(
    LAYOUT_KEY,
    JSON.stringify({
      order: [],
      sizing: { leadTime: 68, parentAgent: 60, rework: 36 },
      visibility: {},
    })
  );
  renderList(prs(2));

  const widths = () =>
    Object.keys(natural).map(
      (label) => screen.getByRole("columnheader", { name: new RegExp(`^${label}`) }).style.width
    );
  const whole = Object.values(natural).map((width) => `${width + SORT_MARK_WIDTH}px`);
  expect(widths()).toEqual(whole);
  for (const label of Object.keys(natural)) {
    const header = screen.getByRole("columnheader", { name: new RegExp(`^${label}`) });
    await user.click(within(header).getByRole("button"));
    expect(header).toHaveAttribute("aria-sort");
    expect(widths()).toEqual(whole);
  }
});
