import { expect, test } from "bun:test";
import type { IssueSummary } from "../../api/types";
import {
  buildItems,
  columnFilterSpec,
  compareGroups,
  defaultView,
  facetCounts,
  isEpic,
  isQueued,
  matchesFilters,
  matchesSearch,
  mergeMembers,
  queuedInOrder,
  readUrl,
  reorder,
  startNextWrites,
  UNCLAIMED_FACET,
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
  // The project's own spelling of a label is the facet, in the table as on the Board.
  expect([...facetCounts(items, "labels", filters, "search")]).toEqual([
    ["FrontEnd", 1],
    ["backend", 1],
    ["legion", 1],
  ]);
  expect([...facetCounts(items, "status", filters, "search")]).toEqual([
    ["todo", 1],
    ["testing", 1],
  ]);
  // A URL keeps the casing it carries — one spelling for the table and the Board — and the
  // matching is what is case-insensitive.
  expect(
    readUrl(new URLSearchParams("label=FrontEnd")).filters.find(({ id }) => id === "labels")?.value
  ).toEqual(["FrontEnd"]);
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

test("Start next ranks the selected rows in queue order and skips the ones already ahead", () => {
  const rows = (keys: string[]) =>
    buildItems(
      keys.map((key, index) => issue(key, { rank: String(index), labels: ["legion"] })),
      (actor) => actor.id
    );
  const queue = rows(["A", "B", "C", "D"]);
  expect(queuedInOrder(queue, ["C", "B"])).toEqual(["B", "C"]);
  expect(startNextWrites(queue, queuedInOrder(queue, ["C", "B"]))).toEqual([
    { key: "C", input: { rank: { before: "A" } } },
    { key: "B", input: { rank: { before: "C" } } },
  ]);
  expect(startNextWrites(queue, ["A", "B", "C", "D"])).toEqual([]);
  // The same function picks up an action a reload cut short: only the ranks still behind.
  expect(startNextWrites(rows(["C", "A", "B", "D"]), ["B", "C"])).toEqual([
    { key: "B", input: { rank: { before: "C" } } },
  ]);
  expect(startNextWrites(rows(["B", "C", "A", "D"]), ["B", "C"])).toEqual([]);
  expect(startNextWrites(rows(["C", "B"]), ["B", "C"])).toEqual([
    { key: "B", input: { rank: { before: "C" } } },
  ]);
  expect(startNextWrites(rows(["A", "C"]), ["B", "C"])).toEqual([
    { key: "C", input: { rank: { before: "A" } } },
  ]);
});

// The server matches a label on `lower(label)`, so an issue labelled `Legion` is queued and one
// labelled `Epic` is an epic, exactly as the lower-case spellings are.
test("the legion and epic labels are matched case-insensitively", () => {
  expect(isQueued(issue("A", { labels: ["Legion"] }))).toBe(true);
  expect(isQueued(issue("A", { labels: ["LEGION"], status: "testing" }))).toBe(false);
  expect(isEpic(issue("A", { labels: ["Epic"] }))).toBe(true);
  expect(isEpic(issue("A", { labels: ["epic"], status: "done" }))).toBe(false);
  const rows = buildItems(
    [issue("A", { labels: ["Epic"] }), issue("B", { parent: "A", labels: ["Legion"] })],
    (actor) => actor.id
  );
  expect(rows.find((row) => row.issue.key === "B")?.queue).toBe(1);
  expect(rows.find((row) => row.issue.key === "B")?.epicKey).toBe("A");
});

test("Nobody or lapsed picks unheld and takeable claims, and ?unclaimed=1 still sets it", () => {
  const held = issue("A", {
    claim: { actor: { kind: "session", id: "one" }, at: "2026-09-10T00:00:00Z" },
  });
  const rows = buildItems(
    [held, issue("B")],
    (actor) => actor.id,
    [],
    (row) => row.key === "B"
  );
  const picked = [{ id: "claim", value: [UNCLAIMED_FACET] }];
  expect(rows.filter((row) => matchesFilters(row, picked)).map((row) => row.issue.key)).toEqual([
    "B",
  ]);
  expect([...facetCounts(rows, "claim", [], "")]).toEqual([
    ["one", 1],
    ["Nobody", 1],
    [UNCLAIMED_FACET, 1],
  ]);
  const fromLink = readUrl(new URLSearchParams("unclaimed=1"));
  expect(fromLink.filters.find(({ id }) => id === "claim")?.value).toEqual([UNCLAIMED_FACET]);
  // And a view written back from it keeps the parameter the Board's own chip reads.
  expect(writeUrl(new URLSearchParams(), fromLink).get("unclaimed")).toBe("1");
});

test("an open root labelled epic is a row of its own, and equal titles never merge groups", () => {
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
  expect(rows.find((row) => row.issue.key === "A")?.epicKey).toBe("A");
  expect(rows.find((row) => row.issue.key === "B")?.epicKey).toBe("B");
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

test("a dropped column lands immediately before the one it was dropped on", () => {
  const order = ["select", "key", "title", "status", "epic"];
  expect(reorder(order, "epic", "title")).toEqual(["select", "key", "epic", "title", "status"]);
  expect(reorder(order, "key", "epic")).toEqual(["select", "title", "status", "key", "epic"]);
  expect(order).toEqual(["select", "key", "title", "status", "epic"]);
});

test("a column dropped on itself, or on one the order does not hold, stays where it is", () => {
  const order = ["select", "key", "title"];
  expect(reorder(order, "title", "title")).toEqual(order);
  expect(reorder(order, "title", "claim")).toEqual(order);
  expect(reorder(order, "claim", "title")).toEqual(order);
});

// B1: a `?status=` this bundle does not know is still the reader's filter, under its own text.
test("an unknown status keeps its own text and its place in the picks", () => {
  const show = columnFilterSpec("status");
  if (show?.kind !== "picks" || show.show === undefined) throw new Error("the status filter");
  expect(show.show("bogus")).toBe("bogus");
  expect(show.show("Done")).toBe("Done");
  expect(show.show("done")).toBe("Done");
  expect(readUrl(new URLSearchParams("status=bogus")).filters).toEqual([
    { id: "status", value: ["bogus"] },
  ]);
});

// O1: the view the table opens with says nothing in a link, and comes back when read.
test("a shared link leaves out the default status set and reads it back", () => {
  const params = writeUrl(new URLSearchParams(), { ...defaultView, search: "rank" });
  expect(params.get("status")).toBeNull();
  expect(params.get("nofilter")).toBeNull();
  expect(readUrl(params).filters).toEqual(defaultView.filters);
  // An empty selection is still expressible, and still means every status.
  expect(writeUrl(new URLSearchParams(), { ...defaultView, filters: [] }).get("nofilter")).toBe(
    "1"
  );
  expect(readUrl(new URLSearchParams("nofilter=1")).filters).toEqual([]);
});

// O2: Key filters by what the reader types, not by a list of every issue.
test("the Key column filters by typed text", () => {
  const key = columnFilterSpec("key");
  expect(key?.kind).toBe("text");
  const item = buildItems([issue("CORE-12")], () => "")[0];
  if (item === undefined) throw new Error("no item");
  expect(matchesFilters(item, [{ id: "key", value: "re-1" }])).toBe(true);
  expect(matchesFilters(item, [{ id: "key", value: "CORE-9" }])).toBe(false);
});

// I3: a key reads as a number where it is one, and the empty group reads last.
test("groups order by number inside a name, with the empty group last", () => {
  expect(compareGroups("epic", "CORE-2 · Two", "CORE-10 · Ten")).toBeLessThan(0);
  expect(compareGroups("epic", "No epic", "CORE-10 · Ten")).toBeGreaterThan(0);
  expect(compareGroups("claim", "Nobody", "zoe")).toBeGreaterThan(0);
  expect(compareGroups("assignee", "Unassigned", "alice")).toBeGreaterThan(0);
});

// F8: nothing typed matches every row, before any of the work a query needs.
test("a blank search matches every row", () => {
  const item = buildItems([issue("CORE-1")], () => "")[0];
  if (item === undefined) throw new Error("no item");
  expect(matchesSearch(item, "")).toBe(true);
  expect(matchesSearch(item, "   ")).toBe(true);
});
