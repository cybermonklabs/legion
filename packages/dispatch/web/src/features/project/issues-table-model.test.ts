import { expect, test } from "bun:test";
import type { IssueSummary } from "../../api/types";
import {
  buildItems,
  clearPicked,
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
  readSelection,
  readUrl,
  reorder,
  selectIssues,
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

test("a narrowing names its issues in the URL, lifts the status filter, and is read back", () => {
  const picked = selectIssues(
    new URLSearchParams("status=todo&page=4&q=rank"),
    ["CORE-1", "CORE-2"],
    "Finished last week"
  );
  expect(picked.get("only")).toBe("CORE-1,CORE-2");
  expect(picked.get("onlyLabel")).toBe("Finished last week");
  // Finished work shows: the status filter goes, `nofilter=1` keeps the empty pick, and the
  // reader lands on the first page. Every other control stays as it was.
  expect(picked.has("status")).toBe(false);
  expect(picked.get("nofilter")).toBe("1");
  expect(picked.has("page")).toBe(false);
  expect(picked.get("q")).toBe("rank");
  const selection = readSelection(picked);
  expect([...(selection?.keys ?? [])]).toEqual(["CORE-1", "CORE-2"]);
  expect(selection?.label).toBe("Finished last week");
  expect(readSelection(new URLSearchParams("only=CORE-1"))?.label).toBe("Selected issues");
  expect(readSelection(new URLSearchParams("only="))?.keys.size).toBe(0);
  expect(readSelection(new URLSearchParams("q=rank"))).toBeNull();
});

test("removing the chip gives the whole table back and leaves the other controls alone", () => {
  const cleared = clearPicked(new URLSearchParams("only=CORE-1&onlyLabel=Two&q=rank&page=4"));
  expect(cleared.has("only")).toBe(false);
  expect(cleared.has("onlyLabel")).toBe(false);
  expect(cleared.has("page")).toBe(false);
  expect(cleared.get("q")).toBe("rank");
  expect(readSelection(cleared)).toBeNull();
  expect(readUrl(cleared).selection).toBeNull();
});

test("the view carries the narrowing through a read and a write of the URL", () => {
  const view = readUrl(new URLSearchParams("only=CORE-1,CORE-2&onlyLabel=Finished&nofilter=1"));
  expect(view.selection?.label).toBe("Finished");
  expect([...(view.selection?.keys ?? [])]).toEqual(["CORE-1", "CORE-2"]);
  // A control the reader moves while the table is narrowed keeps the narrowing.
  const written = writeUrl(new URLSearchParams("only=CORE-1,CORE-2&onlyLabel=Finished"), {
    ...view,
    search: "rank",
  });
  expect(written.get("only")).toBe("CORE-1,CORE-2");
  expect(written.get("onlyLabel")).toBe("Finished");
  expect(written.get("q")).toBe("rank");
  // A narrowing with no words of its own writes no `onlyLabel`, and reads back as the default.
  const unnamed = writeUrl(new URLSearchParams(), readUrl(new URLSearchParams("only=CORE-1")));
  expect(unnamed.get("only")).toBe("CORE-1");
  expect(unnamed.has("onlyLabel")).toBe(false);
  expect(readUrl(unnamed).selection?.label).toBe("Selected issues");
});

test("a stateKey prefixes the table's own controls and leaves the bare ones to the other table", () => {
  const state = {
    ...defaultView,
    sorting: [{ id: "key", desc: true }],
    grouping: ["epic"],
    search: "rank",
    page: 2,
    pageSize: 25,
    filters: [{ id: "labels", value: ["frontend"] }],
  };
  const scoped = writeUrl(new URLSearchParams("sort=-priority&q=other"), state, "advances");
  expect(scoped.get("advances.sort")).toBe("-key");
  expect(scoped.get("advances.group")).toBe("epic");
  expect(scoped.get("advances.q")).toBe("rank");
  expect(scoped.get("advances.page")).toBe("3");
  expect(scoped.get("advances.size")).toBe("25");
  expect(scoped.get("advances.nofilter")).toBe("1");
  expect(scoped.get("advances.label")).toBe("frontend");
  // The page's other table is written bare, and neither table touches the other's keys.
  expect(scoped.get("sort")).toBe("-priority");
  expect(scoped.get("q")).toBe("other");
  expect(readUrl(scoped, "advances")).toEqual(state);
  expect(readUrl(scoped).sorting).toEqual([{ id: "priority", desc: true }]);
  expect(readUrl(scoped).search).toBe("other");
  expect(readUrl(scoped).filters).toEqual(defaultView.filters);
  // A table with no stateKey writes what this page has always published.
  const bare = writeUrl(new URLSearchParams(), state);
  expect(bare.get("sort")).toBe("-key");
  expect([...bare.keys()].some((key) => key.includes("advances."))).toBe(false);
});
