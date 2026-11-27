import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { SortingState, Table } from "@tanstack/react-table";
import { act, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useState } from "react";
import { MemoryRouter } from "react-router-dom";
import { api } from "../../api/client";
import { whoAmIQuery } from "../../api/queries";
import type {
  IssueSummary,
  LegionBoardCard,
  LegionBoardFacts,
  LegionBoardResponse,
} from "../../api/types";
import { ColumnsMenu } from "../../components/ColumnsMenu";
import {
  DataTable,
  DataTableCells,
  type DataTableNames,
  type RowNoun,
  useDataTable,
} from "../../components/DataTable";
import { type LayoutColumns, useTableLayout } from "../../components/table-layout";
import { LEGION_PHASES, type LegionFacts, type LegionPhase } from "./issues-legion";
import { useIssueTableColumns } from "./issues-table-columns";
import {
  buildItems,
  COLUMNS,
  ISSUE_GROUP_ATTRIBUTE,
  ISSUE_LINK_ATTRIBUTE,
  ISSUE_ROW_ATTRIBUTE,
  ISSUE_TABLE_COLUMNS,
  type Item,
  type ItemContext,
  isBoardFedColumn,
  type LegionReads,
  MEASURE_METHODS,
  viewLayoutColumns,
} from "./issues-table-model";

/** The columns the board route feeds, in the registry's order. */
const BOARD_FED = COLUMNS.flatMap(({ id }) => (isBoardFedColumn(id) ? [id] : []));
const HOUR = 3_600_000;
const ago = (ms: number) => new Date(Date.now() - ms).toISOString();

function issue(key: string, overrides: Partial<IssueSummary> = {}): IssueSummary {
  return {
    key,
    advances: [],
    advances_revision: 0,
    title: `Work ${key}`,
    status: "todo",
    priority: null,
    rank: key,
    parent: null,
    blocked_by: [],
    assignee: null,
    claim: null,
    components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
    route: null,
    repo: null,
    resolved_repo: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
    labels: [],
    open_asks: 0,
    progress: { tasks: null, children: null },
    last_seq: 1,
    primary_artifact_id: "spec",
    created_by: null,
    created_at: "2026-09-10T00:00:00Z",
    updated_at: "2026-09-10T00:00:00Z",
    closed_at: null,
    ...overrides,
  };
}

function card(key: string, overrides: Partial<LegionBoardCard> = {}): LegionBoardCard {
  return {
    blocked_by: null,
    children: 0,
    key,
    phase: "",
    phase_since: null,
    priority: null,
    roles: null,
    slot: null,
    slot_since: null,
    slot_use: null,
    title: key,
    tree: key,
    tree_slot: null,
    waiting: null,
    waiting_on: null,
    waiting_place: null,
    ...overrides,
  };
}

function facts(overrides: Partial<LegionBoardFacts> = {}): LegionBoardFacts {
  return {
    as_of: "2026-11-17T00:00:00Z",
    closed_at: null,
    growth: { hours_by_phase: {}, slot_hours: 0, work_hours: 0, work_last_at: 0 },
    hours_by_phase: null,
    opened_at: "2026-11-01T00:00:00Z",
    review_rounds: 0,
    root_key: "CORE-5",
    slot_hours: 0,
    tree_depth: 0,
    updated_at: "2026-11-17T00:00:00Z",
    waiting_reason: "",
    work_first_at: "2026-11-01T00:00:00Z",
    work_hours: 0,
    work_last_at: null,
    ...overrides,
  };
}

function board(
  cards: Partial<Record<LegionPhase, LegionBoardCard[]>>,
  issues: Record<string, LegionBoardFacts>
): LegionBoardResponse {
  return {
    columns: LEGION_PHASES.map(({ id, title }) => ({ id, label: title, cards: cards[id] ?? [] })),
    computed_at: "2026-11-17T12:00:00Z",
    header: {
      facts_cursor_age_seconds: null,
      facts_updated_at: null,
      p0_holders: null,
      queue_length: 0,
      read_at: null,
      read_error: null,
      read_ok: true,
      slot_cap: null,
      slots_in_use: 0,
      trees: 0,
    },
    issues,
    project: "CORE",
    settings: null,
  };
}

/** Seven rows in the listing's order: three Queue cards (two with a Place, CORE-3 with none), a
 *  Todo issue with no card, a card in Implementing, and two done issues, CORE-6 with facts only
 *  the items read holds and CORE-7 with no Legion work. */
const ISSUES: IssueSummary[] = [
  issue("CORE-1", { rank: "e", labels: ["legion"] }),
  issue("CORE-2", { rank: "d" }),
  issue("CORE-3", { rank: "a" }),
  issue("CORE-4", { rank: "b", labels: ["legion"] }),
  issue("CORE-5", { rank: "c", status: "in_progress" }),
  issue("CORE-6", { rank: "f", status: "done", closed_at: ago(24 * HOUR) }),
  issue("CORE-7", { rank: "g", status: "done", closed_at: ago(48 * HOUR) }),
];
const BOARD = board(
  {
    queue: [
      card("CORE-2", {
        waiting_place: 1,
        slot_use: "between_turns",
        waiting: { reason: "ready", since: ago(HOUR / 2), blocked_by: null },
      }),
      card("CORE-1", {
        waiting_place: 2,
        slot_use: "held_idle",
        waiting: { reason: "blocked", since: ago(2 * HOUR), blocked_by: ["CORE-4"] },
      }),
      card("CORE-3", { waiting: { reason: "slot", since: ago(5 * HOUR), blocked_by: null } }),
    ],
    implementing: [
      card("CORE-5", {
        roles: ["reviewer", "implementer"],
        slot_use: "working",
        phase_since: ago(3 * HOUR),
      }),
    ],
  },
  {
    "CORE-1": facts({ work_hours: 2, root_key: "CORE-1" }),
    "CORE-5": facts({ work_hours: 10, review_rounds: 2, hours_by_phase: { implementing: 10 } }),
  }
);
/** A counted zero and a missing work time: Legion worked CORE-6 and measured no work. */
const CLOSED_FACTS: LegionFacts = {
  closed_at: ago(24 * HOUR),
  hours_by_phase: {},
  review_rounds: 0,
  root_key: "CORE-5",
  tree_depth: 1,
  work_first_at: null,
  work_hours: 0,
};
const READS: LegionReads = {
  board: BOARD,
  boardStatus: "success",
  items: { "CORE-6": CLOSED_FACTS },
  itemsStatus: "success",
};
const rows = (context: ItemContext = { legion: READS }) =>
  buildItems(ISSUES, (actor) => actor.id, undefined, context);

const NAMES: DataTableNames = {
  groupAttribute: ISSUE_GROUP_ATTRIBUTE,
  keymap: {
    next: { id: "columns-next", keys: "j", label: "Next issue" },
    open: { id: "columns-open", keys: "o", label: "Open issue" },
    openOnEnter: { id: "columns-open-enter", keys: "Enter", label: "Open the focused issue" },
    previous: { id: "columns-previous", keys: "k", label: "Previous issue" },
    scope: "project",
    select: { id: "columns-select", keys: "x", label: "Select or deselect the focused issue" },
  },
  linkAttribute: ISSUE_LINK_ATTRIBUTE,
  rowAttribute: ISSUE_ROW_ATTRIBUTE,
};
const NOUN: RowNoun = {
  group: { one: "group", many: "groups" },
  line: { one: "row", many: "rows" },
  many: "issues",
  one: "issue",
};
const NO_COUNTS: ReadonlyMap<string, number> = new Map();
const noMark = () => {};
const ignore = () => {};

/** The Issues tab's table over `items`, showing `layout`'s columns, sorted by `sorting`, `pageSize`
 *  rows to a page; `tab` unset draws it as a table another page draws. `onTable` is handed the
 *  table on each draw. */
function Columns({
  items,
  layout: layoutColumns,
  onTable,
  pageSize = 50,
  sorting = [],
  tab = { hasLegion: true },
}: {
  items: Item[];
  layout: LayoutColumns;
  onTable?: (table: Table<Item>) => void;
  pageSize?: number;
  sorting?: SortingState;
  tab?: { hasLegion: boolean } | null;
}): ReactNode {
  const layout = useTableLayout("columns-test", layoutColumns, sorting);
  const columns = useIssueTableColumns(
    undefined,
    false,
    noMark,
    layoutColumns,
    sorting,
    tab === null
      ? { kind: "embedded" }
      : { kind: "tab", hasLegion: tab.hasLegion, phases: LEGION_PHASES }
  );
  const [openFilter, setOpenFilter] = useState<string>();
  const model = useDataTable<Item>({
    rows: items,
    columns,
    getRowId: (item) => item.issue.key,
    layout,
    names: NAMES,
    state: {
      sorting,
      grouping: [],
      columnFilters: [],
      globalFilter: "",
      pagination: { pageIndex: 0, pageSize },
    },
    onSortingChange: ignore,
    onGroupingChange: ignore,
    onColumnFiltersChange: ignore,
    onGlobalFilterChange: ignore,
    onPaginationChange: ignore,
    viewKey: JSON.stringify(sorting),
  });
  onTable?.(model.table);
  return (
    <>
      <ColumnsMenu columns={model.table.getAllLeafColumns()} />
      <DataTable
        empty="No issues match these filters."
        filterCounts={NO_COUNTS}
        label="Issues"
        model={model}
        noun={NOUN}
        onOpenFilterChange={setOpenFilter}
        openFilter={openFilter}
        renderGroup={() => null}
        renderRow={({ row, index, measure }) => (
          <tr data-index={index} data-issue-row={row.id} ref={measure}>
            <DataTableCells row={row} />
          </tr>
        )}
        scrollTestId="columns-scroll"
      />
    </>
  );
}

const restores: (() => void)[] = [];
/** Takes the drawn table away with the layout it stored, so the next draw opens on its own. */
function undraw() {
  for (const restore of restores.splice(0)) restore();
  localStorage.clear();
}
afterEach(undraw);

/** Who the reader is: the browser keeps their choices under their login. */
const VIEWER = { event_head: "0", kind: "user", login: "alice" } as const;

/** Draws the table, with the box the height a window gives it: the unit DOM lays nothing out. */
function draw(props: Parameters<typeof Columns>[0]) {
  const descriptor = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "clientHeight");
  Object.defineProperty(HTMLElement.prototype, "clientHeight", {
    configurable: true,
    get() {
      return this.dataset.testid === "columns-scroll" ? 600 : (descriptor?.get?.call(this) ?? 0);
    },
  });
  const viewer = spyOn(api, "whoAmI").mockResolvedValue(VIEWER);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(whoAmIQuery().queryKey, VIEWER);
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <Columns {...props} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  restores.push(() => {
    view.unmount();
    client.clear();
    viewer.mockRestore();
    if (descriptor) Object.defineProperty(HTMLElement.prototype, "clientHeight", descriptor);
    else Reflect.deleteProperty(HTMLElement.prototype, "clientHeight");
  });
  return view;
}

/** The tab's layout showing Select and `shown`. */
const showing = (...shown: string[]) => viewLayoutColumns(shown, false);

const listed = () =>
  [...document.querySelectorAll(`[${ISSUE_ROW_ATTRIBUTE}]`)].map((row) =>
    row.getAttribute(ISSUE_ROW_ATTRIBUTE)
  );

/** The text of each cell of `key`'s row after Select, left to right. */
const cellsOf = (key: string) => {
  const row = document.querySelector(`[${ISSUE_ROW_ATTRIBUTE}="${key}"]`);
  return [...(row?.querySelectorAll("td") ?? [])].slice(1).map((cell) => cell.textContent ?? "");
};

/** The text of `key`'s cell under the header whose sort button reads `header`. */
const cellUnder = (key: string, header: string) => {
  const place = [...document.querySelectorAll("thead th")].findIndex(
    (cell) => cell.querySelector("button")?.textContent === header
  );
  const row = document.querySelector(`[${ISSUE_ROW_ATTRIBUTE}="${key}"]`);
  return row?.querySelectorAll("td")[place]?.textContent ?? "";
};

test("Queue draws the Place, or In Legion queue for a Queue card with no Place", async () => {
  draw({ items: rows(), layout: showing("queue") });
  await screen.findByRole("table", { name: "Issues" });
  expect(ISSUES.map(({ key }) => cellsOf(key)[0])).toEqual([
    "#2",
    "#1",
    "In Legion queue",
    "",
    "",
    "",
    "",
  ]);
  // With no daemon it is today's number: the Todo issues labelled `legion`, in rank order.
  undraw();
  draw({ items: rows({}), layout: showing("queue"), tab: { hasLegion: false } });
  await screen.findByRole("table", { name: "Issues" });
  expect(ISSUES.map(({ key }) => cellsOf(key)[0])).toEqual(["#2", "", "", "#1", "", "", ""]);
});

test("Queue sorts by Place, then the rows with no Place by rank, the Queue card among them", async () => {
  draw({ items: rows(), layout: showing("queue"), sorting: [{ id: "queue", desc: false }] });
  await screen.findByRole("table", { name: "Issues" });
  expect(listed()).toEqual(["CORE-2", "CORE-1", "CORE-3", "CORE-4", "CORE-5", "CORE-6", "CORE-7"]);
});

test("Legion phase sorts in the board's order, the rows with no card last either way", async () => {
  draw({
    items: rows(),
    layout: showing("legion_phase"),
    sorting: [{ id: "legion_phase", desc: false }],
  });
  await screen.findByRole("table", { name: "Issues" });
  expect(listed()).toEqual(["CORE-1", "CORE-2", "CORE-3", "CORE-5", "CORE-4", "CORE-6", "CORE-7"]);
  // The board's own label for the card's column, blank with no card.
  expect(cellsOf("CORE-5")).toEqual(["Implementing"]);
  expect(cellsOf("CORE-2")).toEqual(["Queue"]);
  expect(cellsOf("CORE-4")).toEqual([""]);
  undraw();
  draw({
    items: rows(),
    layout: showing("legion_phase"),
    sorting: [{ id: "legion_phase", desc: true }],
  });
  await screen.findByRole("table", { name: "Issues" });
  expect(listed()).toEqual(["CORE-5", "CORE-1", "CORE-2", "CORE-3", "CORE-4", "CORE-6", "CORE-7"]);
  undraw();
  // A Retro card sorts between an Accepting card and a Merging card.
  const late = board(
    { merging: [card("CORE-1")], retro: [card("CORE-2")], accepting: [card("CORE-3")] },
    {}
  );
  draw({
    items: rows({ legion: { ...READS, board: late } }),
    layout: showing("legion_phase"),
    sorting: [{ id: "legion_phase", desc: false }],
  });
  await screen.findByRole("table", { name: "Issues" });
  expect(listed().slice(0, 3)).toEqual(["CORE-3", "CORE-2", "CORE-1"]);
  expect(cellsOf("CORE-2")).toEqual(["Retro"]);
});

test("Worker state and Why it waits sort in their values' order and read their words", async () => {
  draw({
    items: rows(),
    layout: showing("activity", "waiting_reason", "roles"),
    sorting: [{ id: "activity", desc: false }],
  });
  await screen.findByRole("table", { name: "Issues" });
  expect(listed().slice(0, 3)).toEqual(["CORE-5", "CORE-2", "CORE-1"]);
  expect(cellsOf("CORE-5")).toEqual(["Working", "", "Implementer, Reviewer"]);
  expect(cellsOf("CORE-2")).toEqual(["Between turns", "Ready to start", ""]);
  // After A blocker, the keys of the issues that block it.
  expect(cellsOf("CORE-1")).toEqual(["Idle", "A blockerCORE-4", ""]);
  expect(cellsOf("CORE-3")).toEqual(["", "Capacity to go on", ""]);
  const blocker = document.querySelector(`[${ISSUE_ROW_ATTRIBUTE}="CORE-1"]`);
  expect(within(blocker as HTMLElement).getByRole("link", { name: "CORE-4" })).toHaveAttribute(
    "href",
    "/issues/CORE-4"
  );
  undraw();
  draw({
    items: rows(),
    layout: showing("waiting_reason"),
    sorting: [{ id: "waiting_reason", desc: false }],
  });
  await screen.findByRole("table", { name: "Issues" });
  expect(listed().slice(0, 3)).toEqual(["CORE-2", "CORE-3", "CORE-1"]);
});

test("a missing measure draws an empty cell, never 0, and sorts last either way", async () => {
  draw({
    items: rows(),
    layout: showing("work_hours", "review_rounds", "legion"),
    sorting: [{ id: "work_hours", desc: false }],
  });
  await screen.findByRole("table", { name: "Issues" });
  expect(listed().slice(0, 2)).toEqual(["CORE-1", "CORE-5"]);
  expect(cellsOf("CORE-5")).toEqual(["10 h 0 m", "2", "Root"]);
  // A counted zero reads 0; a work time Legion measured none of is empty, as is every fact of
  // an issue with no Legion work.
  expect(cellsOf("CORE-6")).toEqual(["", "0", "CORE-5"]);
  expect(cellsOf("CORE-7")).toEqual(["", "", ""]);
  expect(cellsOf("CORE-4")).toEqual(["", "", ""]);
  undraw();
  draw({
    items: rows(),
    layout: showing("work_hours"),
    sorting: [{ id: "work_hours", desc: true }],
  });
  await screen.findByRole("table", { name: "Issues" });
  expect(listed()).toEqual(["CORE-5", "CORE-1", "CORE-2", "CORE-3", "CORE-4", "CORE-6", "CORE-7"]);
});

// INFRA-3221's tester: the rows that tie on the first sort because they have no value there follow
// the second sort, whichever way the first runs, and still come after the rows with a value. A
// row with no value in the second sort comes last among them, either way that one runs.
test("rows with no value in the first sort follow the second sort, after the rows with a value", async () => {
  const made = (key: string, created: string, overrides: Partial<IssueSummary> = {}) =>
    issue(key, { created_at: `2026-09-${created}T00:00:00Z`, ...overrides });
  const items = buildItems(
    [
      made("CORE-1", "04", { priority: 2 }),
      made("CORE-2", "03", { priority: 0 }),
      made("CORE-3", "01", { priority: 3 }),
      made("CORE-4", "02", { priority: 1 }),
      made("CORE-5", "06", { status: "in_progress" }),
      made("CORE-6", "05"),
    ],
    (actor) => actor.id,
    undefined,
    {
      legion: {
        board: board({ implementing: [card("CORE-5")] }, { "CORE-5": facts({ work_hours: 10 }) }),
        boardStatus: "success",
        items: {},
        itemsStatus: "success",
      },
    }
  );
  for (const [sorting, order] of [
    [
      [
        { id: "legion_phase", desc: false },
        { id: "priority", desc: false },
      ],
      ["CORE-5", "CORE-2", "CORE-4", "CORE-1", "CORE-3", "CORE-6"],
    ],
    [
      [
        { id: "legion_phase", desc: true },
        { id: "priority", desc: false },
      ],
      ["CORE-5", "CORE-2", "CORE-4", "CORE-1", "CORE-3", "CORE-6"],
    ],
    [
      [
        { id: "legion_phase", desc: false },
        { id: "priority", desc: true },
      ],
      ["CORE-5", "CORE-3", "CORE-1", "CORE-4", "CORE-2", "CORE-6"],
    ],
    // Work time, then a shift-click on Created: oldest first among the rows with no work time.
    [
      [
        { id: "work_hours", desc: true },
        { id: "created", desc: false },
      ],
      ["CORE-5", "CORE-3", "CORE-4", "CORE-2", "CORE-1", "CORE-6"],
    ],
  ] as const) {
    draw({
      items,
      layout: showing("legion_phase", "priority", "work_hours", "created"),
      sorting: [...sorting],
    });
    await screen.findByRole("table", { name: "Issues" });
    expect(listed(), JSON.stringify(sorting)).toEqual([...order]);
    undraw();
  }
});

test("a facts cell whose read is out shows a loading mark, and one whose read failed an error mark", async () => {
  const shown = showing("work_hours", "review_rounds", "legion", "phase_hours", "waited");
  draw({
    items: rows({ legion: { ...READS, items: undefined, itemsStatus: "pending" } }),
    layout: shown,
  });
  await screen.findByRole("table", { name: "Issues" });
  // The closed rows the board's map does not hold wait on the items read; the open ones do not.
  for (const key of ["CORE-6", "CORE-7"])
    expect(cellsOf(key)).toEqual(["Loading", "Loading", "Loading", "Loading", ""]);
  // Time per phase opens on Share, which draws CORE-5's bar and writes no total.
  expect(cellsOf("CORE-5")).toEqual(["10 h 0 m", "2", "Root", "", ""]);
  expect(
    document.querySelector(`[${ISSUE_ROW_ATTRIBUTE}="CORE-5"] [data-phase-hours]`)
  ).not.toBeNull();
  expect(cellsOf("CORE-4")).toEqual(["", "", "", "", ""]);
  const row = document.querySelector(`[${ISSUE_ROW_ATTRIBUTE}="CORE-7"]`);
  expect(row?.querySelectorAll('[data-facts-read="pending"]')).toHaveLength(4);
  undraw();
  draw({
    items: rows({ legion: { ...READS, items: undefined, itemsStatus: "error" } }),
    layout: shown,
  });
  await screen.findByRole("table", { name: "Issues" });
  expect(cellsOf("CORE-7")).toEqual([
    "!Could not load",
    "!Could not load",
    "!Could not load",
    "!Could not load",
    "",
  ]);
  // While the board is out, every row's facts wait on it.
  undraw();
  draw({
    items: rows({
      legion: {
        board: undefined,
        boardStatus: "pending",
        items: undefined,
        itemsStatus: "pending",
      },
    }),
    layout: showing("work_hours"),
  });
  await screen.findByRole("table", { name: "Issues" });
  expect(ISSUES.map(({ key }) => cellsOf(key)[0])).toEqual(ISSUES.map(() => "Loading"));
});

test("every column sorts, and no header is over 20 characters", async () => {
  let table: Table<Item> | undefined;
  draw({ items: rows(), layout: showing("key"), onTable: (drawn) => (table = drawn) });
  await screen.findByRole("table", { name: "Issues" });
  const columns = table?.getAllLeafColumns() ?? [];
  expect(columns.filter((column) => !column.getCanSort()).map(({ id }) => id)).toEqual(["select"]);
  expect(columns.filter((column) => !column.getCanFilter()).map(({ id }) => id)).toEqual([
    "select",
  ]);
  const headers = columns.flatMap(({ id, columnDef }) =>
    id === "select" ? [] : [String(columnDef.header)]
  );
  expect(headers.filter((header) => header.length > 20)).toEqual([]);
  expect(BOARD_FED.map((id) => String(table?.getColumn(id)?.columnDef.header))).toEqual([
    "Legion phase",
    "Workers",
    "Worker state",
    "Time waiting",
    "Why it waits",
    "Time in Legion phase",
    "Work time",
    "Review rounds",
    "Legion tree",
    "Time per phase",
  ]);
});

test("with no Legion daemon the board-fed columns are in neither Columns nor Group by", async () => {
  const user = userEvent.setup();
  let table: Table<Item> | undefined;
  draw({
    items: rows({}),
    layout: showing("key"),
    onTable: (drawn) => (table = drawn),
    tab: { hasLegion: false },
  });
  await screen.findByRole("table", { name: "Issues" });
  const leaves = table?.getAllLeafColumns() ?? [];
  const hideable = leaves.filter((column) => column.getCanHide()).map(({ id }) => id);
  for (const id of BOARD_FED) expect(hideable).not.toContain(id);
  expect(hideable).toContain("queue");
  expect(hideable).toContain("closed");
  expect(leaves.filter((column) => column.getCanGroup()).map(({ id }) => id)).toEqual([
    "priority",
    "status",
    "assignee",
    "author",
    "claim",
    "created",
    "updated",
  ]);
  await user.click(screen.getByRole("button", { name: "Columns" }));
  const menu = screen.getByRole("dialog", { name: "Columns" });
  expect(within(menu).queryByRole("checkbox", { name: "Work time" })).toBeNull();
  expect(within(menu).getByRole("checkbox", { name: "Queue" })).toBeDefined();
  expect(within(menu).getByRole("checkbox", { name: "Closed" })).toBeDefined();
});

test("with a daemon the Columns menu offers every column, a measure with its method under it", async () => {
  const user = userEvent.setup();
  let table: Table<Item> | undefined;
  draw({ items: rows(), layout: showing("key"), onTable: (drawn) => (table = drawn) });
  await screen.findByRole("table", { name: "Issues" });
  expect(
    (table?.getAllLeafColumns() ?? []).filter((column) => column.getCanGroup()).map(({ id }) => id)
  ).toContain("legion_phase");
  await user.click(screen.getByRole("button", { name: "Columns" }));
  const menu = screen.getByRole("dialog", { name: "Columns" });
  for (const [header, id] of [
    ["Work time", "work_hours"],
    ["Time per phase", "phase_hours"],
    ["Review rounds", "review_rounds"],
    ["Time waiting", "waited"],
    ["Time in Legion phase", "time_in_phase"],
  ] as const) {
    const box = within(menu).getByRole("checkbox", { name: header });
    expect(box).toHaveAccessibleDescription(MEASURE_METHODS[id] ?? "");
  }
  expect(within(menu).getByRole("checkbox", { name: "Legion phase" })).not.toHaveAttribute(
    "aria-describedby"
  );
});

test("Closed filters by a date range, and a table another page draws offers it hidden", async () => {
  const user = userEvent.setup();
  draw({ items: rows(), layout: showing("closed") });
  await screen.findByRole("table", { name: "Issues" });
  await user.click(screen.getByRole("button", { name: "Filter Closed" }));
  expect(screen.getByText("Or from and to (UTC)")).toBeDefined();
  expect(screen.getByRole("button", { name: "90 days" })).toBeDefined();
  undraw();
  let table: Table<Item> | undefined;
  draw({
    items: rows({}),
    layout: ISSUE_TABLE_COLUMNS,
    onTable: (drawn) => (table = drawn),
    tab: null,
  });
  await screen.findByRole("table", { name: "Issues" });
  const leaves = table?.getAllLeafColumns() ?? [];
  expect(leaves.map(({ id }) => id)).toEqual([...ISSUE_TABLE_COLUMNS.ids]);
  expect(table?.getColumn("closed")?.getIsVisible()).toBe(false);
  expect(table?.getColumn("closed")?.columnDef.meta?.range).toBe(true);
  // Today's Queue: Start next's number, its filter in the words it has always had.
  expect(table?.getColumn("queue")?.columnDef.meta?.show).toBeUndefined();
  expect(ISSUES.map(({ key }) => cellUnder(key, "Queue"))).toEqual([
    "#2",
    "",
    "",
    "#1",
    "",
    "",
    "",
  ]);
});

const SORT_HINT = "Sort (shift-click to add a second sort); drag to move the column";

// INFRA-3221's R9: the Queue number's help moved from its native title into the shared tooltip,
// which a keyboard reaches, on the Issues tab and in the priority pages' tables.
test("a Queue number says what it is in the tooltip, on hover and on focus, and carries no native title", async () => {
  const user = userEvent.setup();
  draw({ items: rows(), layout: showing("queue") });
  await screen.findByRole("table", { name: "Issues" });
  const number = screen.getByText("#1");
  expect(number).toHaveAttribute("tabindex", "0");
  await user.hover(number);
  expect(screen.getByRole("tooltip").textContent).toBe("Legion starts issues in this order");
  expect(number).toHaveAccessibleDescription("Legion starts issues in this order");
  await user.unhover(number);
  expect(screen.queryByRole("tooltip")).toBeNull();
  act(() => number.focus());
  expect(screen.getByRole("tooltip").textContent).toBe("Legion starts issues in this order");
  await user.keyboard("{Escape}");
  expect(screen.queryByRole("tooltip")).toBeNull();
  // A Queue card with no number has nothing to explain.
  expect(screen.getByText("In Legion queue")).not.toHaveAttribute("tabindex");
  expect(document.querySelectorAll("tbody [title]")).toHaveLength(0);
  undraw();
  draw({ items: rows({}), layout: ISSUE_TABLE_COLUMNS, tab: null });
  await screen.findByRole("table", { name: "Issues" });
  const today = document.querySelector<HTMLElement>(
    `[${ISSUE_ROW_ATTRIBUTE}="CORE-4"] [data-queue-place]`
  );
  expect(today?.textContent).toBe("#1");
  expect(today).toHaveAttribute("tabindex", "0");
  expect(today).not.toHaveAttribute("title");
  await user.hover(today as HTMLElement);
  expect(screen.getByRole("tooltip").textContent).toBe("Legion starts issues in this order");
});

test("each measure header's sort tooltip gives its method, then the sort hint on a muted second line", async () => {
  const user = userEvent.setup();
  draw({
    items: rows(),
    layout: showing(
      "status",
      "work_hours",
      "phase_hours",
      "review_rounds",
      "waited",
      "time_in_phase"
    ),
  });
  const table = await screen.findByRole("table", { name: "Issues" });
  const lines = () =>
    [...screen.getByRole("tooltip").querySelectorAll("p")].map((line) => line.textContent);
  for (const [header, id] of [
    ["Work time", "work_hours"],
    ["Time per phase", "phase_hours"],
    ["Review rounds", "review_rounds"],
    ["Time waiting", "waited"],
    ["Time in Legion phase", "time_in_phase"],
  ] as const) {
    const sort = within(table).getByRole("button", { name: header });
    const method = MEASURE_METHODS[id];
    if (method === undefined) throw new Error(`${header} has no method`);
    expect(sort).not.toHaveAttribute("title");
    await user.hover(sort);
    expect(lines()).toEqual([method, SORT_HINT]);
    await user.unhover(sort);
    act(() => sort.focus());
    expect(lines()).toEqual([method, SORT_HINT]);
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("tooltip")).toBeNull();
  }
  // Any other header has the sort hint alone.
  await user.hover(within(table).getByRole("button", { name: "Status" }));
  expect(lines()).toEqual([SORT_HINT]);
});

/** CORE-1 with 5 hours by phase beside CORE-5's 10. */
const PHASE_READS: LegionReads = {
  ...READS,
  board: {
    ...BOARD,
    issues: {
      ...BOARD.issues,
      "CORE-1": facts({
        work_hours: 5,
        root_key: "CORE-1",
        hours_by_phase: { design: 2, implementing: 3 },
      }),
    },
  },
};

test("Time per phase opens on Share, its header's control switches to Hours against the page's longest total, and a remount keeps Hours", async () => {
  const user = userEvent.setup();
  const bar = (key: string) =>
    document.querySelector<HTMLElement>(`[${ISSUE_ROW_ATTRIBUTE}="${key}"] [data-phase-hours-bar]`);
  draw({ items: rows({ legion: PHASE_READS }), layout: showing("phase_hours") });
  const table = await screen.findByRole("table", { name: "Issues" });
  const control = within(table).getByRole("group", { name: "Time per phase as" });
  // In the column's header, outside its sort button.
  const cell: HTMLElement | null = control.closest("th");
  expect(cell).toBe(within(table).getByRole("columnheader", { name: /Time per phase/ }));
  expect(within(table).getByRole("button", { name: "Time per phase" })).not.toContainElement(
    control
  );
  expect(within(control).getByRole("button", { name: "Share" })).toHaveAttribute(
    "aria-pressed",
    "true"
  );
  expect([bar("CORE-1")?.style.width, bar("CORE-5")?.style.width]).toEqual(["100%", "100%"]);
  expect([cellsOf("CORE-1"), cellsOf("CORE-5")]).toEqual([[""], [""]]);

  await user.click(within(control).getByRole("button", { name: "Hours" }));
  expect([bar("CORE-1")?.style.width, bar("CORE-5")?.style.width]).toEqual(["50%", "100%"]);
  expect([cellsOf("CORE-1"), cellsOf("CORE-5")]).toEqual([["5.0 h"], ["10 h"]]);
  expect(localStorage.getItem("dispatch.project.phase-hours:alice")).toBe("hours");

  // A remount, as a reload makes one, keeps Hours: the browser kept it for the reader.
  for (const restore of restores.splice(0)) restore();
  draw({ items: rows({ legion: PHASE_READS }), layout: showing("phase_hours"), pageSize: 2 });
  await screen.findByRole("table", { name: "Issues" });
  expect(
    within(screen.getByRole("group", { name: "Time per phase as" })).getByRole("button", {
      name: "Hours",
    })
  ).toHaveAttribute("aria-pressed", "true");
  // A page of two rows: CORE-1 holds the longest total of its page, whatever the next holds.
  expect(listed()).toEqual(["CORE-1", "CORE-2"]);
  expect(bar("CORE-1")?.style.width).toBe("100%");
  expect(cellsOf("CORE-1")).toEqual(["5.0 h"]);
});
