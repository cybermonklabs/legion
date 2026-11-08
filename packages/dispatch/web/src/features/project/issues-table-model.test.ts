import { expect, test } from "bun:test";
import type { IssueSummary, LegionBoardCard, LegionBoardResponse } from "../../api/types";
import {
  buildItems,
  countIssueStates,
  defaultView,
  facetCounts,
  formatPhaseTime,
  matchesFilters,
  matchesSearch,
  mergeMembers,
  readUrl,
  remainingStartNextWrites,
  startNextWrites,
  writeUrl,
} from "./issues-table-model";

function issue(key: string, overrides: Partial<IssueSummary> = {}): IssueSummary {
  return {
    key,
    title: key,
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
    ...overrides,
  };
}
const items = buildItems(
  [
    issue("CORE-1", {
      title: "Implement searchable filters",
      labels: ["FrontEnd", "backend", "legion"],
    }),
    issue("CORE-2", { title: "Search backend", status: "testing", labels: ["backend"] }),
    issue("CORE-3", { title: "Keyboard navigation", labels: ["frontend"] }),
  ],
  (actor) => actor.id
);

test("labels require every picked value case-insensitively, alongside a Title filter", () => {
  const filters = [
    { id: "labels", value: ["frontend", "backend"] },
    { id: "title", value: "SEARCH" },
  ];
  expect(
    items.filter((item) => matchesFilters(item, filters)).map((item) => item.issue.key)
  ).toEqual(["CORE-1"]);
});

test("fuzzy search requires every word, tolerates one edit only for long words", () => {
  const searchable = items[0];
  if (!searchable) throw new Error("Search fixture has no rows");
  expect(matchesSearch(searchable, "filters implement")).toBe(true);
  expect(matchesSearch(searchable, "searchble filters")).toBe(true);
  expect(matchesSearch(searchable, "searchxble filters")).toBe(true);
  expect(matchesSearch(searchable, "searchxxble filters")).toBe(false);
  expect(matchesSearch(searchable, "filters missing")).toBe(false);
  expect(matchesSearch(searchable, "CORF")).toBe(false);
});

test("facet counts apply search and every other filter, not their own", () => {
  const filters = [
    { id: "status", value: ["todo"] },
    { id: "labels", value: ["backend"] },
  ];
  expect([...facetCounts(items, "labels", filters, "search")]).toEqual([
    ["frontend", 1],
    ["backend", 1],
    ["legion", 1],
  ]);
  expect([...facetCounts(items, "status", filters, "search")]).toEqual([
    ["todo", 1],
    ["testing", 1],
  ]);
  expect(
    readUrl(new URLSearchParams("label=FrontEnd")).filters.find(({ id }) => id === "labels")?.value
  ).toEqual(["frontend"]);
});

test("URL round trip preserves Board status and label params plus every table control", () => {
  const state = {
    ...defaultView,
    filters: [
      { id: "status", value: ["todo", "testing"] },
      { id: "labels", value: ["frontend", "backend"] },
      { id: "title", value: "a & b" },
    ],
    sorting: [
      { id: "priority", desc: true },
      { id: "key", desc: false },
    ],
    grouping: ["epic"],
    search: "two words",
    page: 3,
    pageSize: 25,
  };
  const params = writeUrl(new URLSearchParams("unread=1"), state);
  expect(params.getAll("status")).toEqual(["todo", "testing"]);
  expect(params.getAll("label")).toEqual(["frontend", "backend"]);
  expect(params.get("unread")).toBe("1");
  expect(params.has("f.status")).toBe(false);
  expect(readUrl(params)).toEqual(state);
  expect(readUrl(new URLSearchParams("page=NaN&size=-1&sort=bogus&group=bogus&f.bogus=x"))).toEqual(
    defaultView
  );
  expect(readUrl(writeUrl(new URLSearchParams(), { ...defaultView, filters: [] })).filters).toEqual(
    []
  );
});

test("Start next writes before keys in reverse order to retain the selected queue order", () => {
  const queue = buildItems(
    ["A", "B", "C", "D"].map((key) => issue(key, { labels: ["legion"] })),
    (actor) => actor.id
  );
  expect(startNextWrites(queue, ["C", "B"])).toEqual([
    { key: "C", input: { rank: { before: "A" } } },
    { key: "B", input: { rank: { before: "C" } } },
  ]);
  expect(startNextWrites(queue, ["A", "B", "C", "D"])).toEqual([]);
});

test("Finish preserves the selected order and sends only ranks still behind the queue head", () => {
  const rows = (keys: string[]) =>
    buildItems(
      keys.map((key, index) => issue(key, { rank: String(index), labels: ["legion"] })),
      (actor) => actor.id
    );
  expect(remainingStartNextWrites(rows(["C", "A", "B", "D"]), ["B", "C"])).toEqual([
    { key: "B", input: { rank: { before: "C" } } },
  ]);
  expect(remainingStartNextWrites(rows(["B", "C", "A", "D"]), ["B", "C"])).toEqual([]);
  expect(remainingStartNextWrites(rows(["C", "B"]), ["B", "C"])).toEqual([
    { key: "B", input: { rank: { before: "C" } } },
  ]);
  expect(remainingStartNextWrites(rows(["A", "C"]), ["B", "C"])).toEqual([
    { key: "C", input: { rank: { before: "A" } } },
  ]);
});

test("an epic is the open root, and equal titles never merge distinct epic groups", () => {
  const rows = buildItems(
    [
      issue("A", { labels: ["epic"], title: "Same" }),
      issue("B", { labels: ["epic"], title: "Same" }),
      issue("C", { parent: "A" }),
      issue("D", { parent: "C" }),
      issue("E", { parent: "B" }),
    ],
    (actor) => actor.id
  );
  expect(rows.find((row) => row.issue.key === "A")).toBeUndefined();
  expect(rows.find((row) => row.issue.key === "B")).toBeUndefined();
  expect(rows.find((row) => row.issue.key === "D")?.epicKey).toBe("A");
  expect(rows.find((row) => row.issue.key === "D")?.epic).not.toBe(
    rows.find((row) => row.issue.key === "E")?.epic
  );
});

test("a closed intermediate parent does not hide an open root epic", () => {
  const rows = buildItems(
    [issue("A", { labels: ["epic"] }), issue("C", { parent: "B" })],
    (actor) => actor.id,
    [issue("B", { parent: "A", status: "done" })]
  );
  expect(rows.find((row) => row.issue.key === "C")?.epicKey).toBe("A");
});

test("a merge crosses closed intermediates but does not flatten an open subtree", () => {
  const rows = [
    issue("A", { labels: ["epic"] }),
    issue("B", { parent: "A", status: "done" }),
    issue("C", { parent: "B" }),
    issue("D", { parent: "C" }),
    issue("E", { parent: "A" }),
    issue("F", { parent: "B", status: "done" }),
    issue("G", { parent: "F" }),
  ];
  expect(mergeMembers(rows, "A")).toEqual(["C", "E", "G"]);
});

function card(key: string, phase: string): LegionBoardCard {
  return {
    key,
    title: key,
    priority: null,
    phase,
    phase_since: "2026-09-10T00:00:00Z",
    slot: null,
    roles: [],
    checks_verdict: null,
    blocked_by: [],
    waiting_on: null,
    tree: key,
    children: 0,
  };
}
const legionBoard: LegionBoardResponse = {
  project: "CORE",
  header: {
    slots_in_use: 1,
    slot_cap: 4,
    trees: 1,
    queue_length: 2,
    read_at: "2026-09-10T00:00:00Z",
    read_ok: true,
    read_error: null,
  },
  columns: [
    {
      id: "queue",
      label: "Queue",
      cards: [card("CORE-3", "admitted"), card("CORE-1", "admitted")],
    },
    { id: "planning", label: "Planning", cards: [card("CORE-4", "planning")] },
    {
      id: "implementing",
      label: "Implementing",
      cards: [
        {
          ...card("CORE-2", "implementing"),
          slot: 0,
          roles: ["implementer"],
          checks_verdict: "failing",
        },
      ],
    },
  ],
  settings: {},
  setting_sources: {},
  computed_at: "2026-09-10T00:00:00Z",
};

test("the daemon queue replaces Dispatch rank, including membership, without a derived fallback", () => {
  const issues = [1, 2, 3, 4, 5].map((n) => issue(`CORE-${n}`, { labels: ["legion"] }));
  const rows = buildItems(issues, (actor) => actor.id, [], legionBoard);
  expect(rows.map((row) => row.queue)).toEqual([2, undefined, 1, undefined, undefined]);
  expect(buildItems(issues, (actor) => actor.id, [], null).map((row) => row.queue)).toEqual([
    undefined,
    undefined,
    undefined,
    undefined,
    undefined,
  ]);
  expect(buildItems(issues, (actor) => actor.id).map((row) => row.queue)).toEqual([1, 2, 3, 4, 5]);
});

test("phase facets and workflow ordering come from the board, not alphabetical or issue order", () => {
  const rows = buildItems(
    [2, 4, 3].map((n) => issue(`CORE-${n}`)),
    (actor) => actor.id,
    [],
    legionBoard
  );
  expect(
    rows
      .slice()
      .sort((a, b) => (a.legion?.order ?? 99) - (b.legion?.order ?? 99))
      .map((row) => row.issue.key)
  ).toEqual(["CORE-3", "CORE-4", "CORE-2"]);
  expect(
    rows
      .filter((row) => matchesFilters(row, [{ id: "phase", value: ["implementing"] }]))
      .map((row) => row.issue.key)
  ).toEqual(["CORE-2"]);
  expect(
    rows
      .filter((row) =>
        matchesFilters(row, [
          { id: "slot", value: ["0"] },
          { id: "roles", value: ["implementer"] },
          { id: "ci", value: ["failing"] },
        ])
      )
      .map((row) => row.issue.key)
  ).toEqual(["CORE-2"]);
  const view = readUrl(new URLSearchParams("group=phase&sort=phase&f.phase=planning"));
  expect(view.grouping).toEqual(["phase"]);
  expect(view.sorting).toEqual([{ id: "phase", desc: false }]);
});

test("the bar counts each issue once, under its Legion phase or its Dispatch status", () => {
  const rows = buildItems(
    [1, 2, 3, 4, 5].map((n) => issue(`CORE-${n}`, { status: "todo" })),
    (actor) => actor.id,
    [],
    legionBoard
  );
  expect(countIssueStates(rows)).toEqual([
    { id: "queue", label: "Queue", column: "phase", count: 2 },
    { id: "planning", label: "Planning", column: "phase", count: 1 },
    { id: "implementing", label: "Implementing", column: "phase", count: 1 },
    { id: "todo", label: "Todo", column: "status", count: 1 },
  ]);
});

test("time in phase preserves elapsed units and unknown or future entry times", () => {
  const now = Date.now();
  for (const [minutes, expected] of [
    [59, "59 m"],
    [60, "1 h 0 m"],
    [1439, "23 h 59 m"],
    [1440, "1 d 0 h"],
  ] as const) {
    expect(formatPhaseTime(new Date(now - minutes * 60_000).toISOString(), now)).toBe(expected);
  }
  expect(formatPhaseTime(null, now)).toBe("—");
  expect(formatPhaseTime("not a date", now)).toBe("—");
  expect(formatPhaseTime(new Date(now + 60_000).toISOString(), now)).toBe("0 m");
});
