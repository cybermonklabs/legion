import { expect, test } from "bun:test";
import { SEARCH_QUERY_MAX } from "@legion/contracts/dispatch-tools";
import type {
  IssueSummary,
  LegionBoardCard,
  LegionBoardFacts,
  LegionBoardResponse,
} from "../../api/types";
import { LEGION_PHASES, type LegionFacts, type LegionPhase } from "./issues-legion";
import {
  buildItems,
  COLUMNS,
  canGroupBy,
  clearPicked,
  columnFilterSpec,
  columnMinimums,
  compareGroups,
  defaultView,
  facetCounts,
  fitViewWidths,
  groupValue,
  ISSUE_TABLE_COLUMNS,
  type ItemContext,
  isQueued,
  legionPhaseFilter,
  MEASURE_METHODS,
  matchesFilters,
  matchesSearch,
  phoneMin,
  queuedInOrder,
  readSelection,
  readUrl,
  selectIssues,
  startNextWrites,
  UNCLAIMED_FACET,
  viewLayoutColumns,
  writeUrl,
} from "./issues-table-model";
import { viewColumns, viewFor, viewsGrammar } from "./issues-views";

function issue(key: string, overrides: Partial<IssueSummary> = {}): IssueSummary {
  return {
    key,
    advances: [],
    advances_revision: 0,
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
    closed_at: overrides.status === "done" ? "2026-09-10T00:00:00Z" : null,
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

// R14: a pick in another case than a value the column knows is that value, so `?status=Done`
// picks the done issues and the filter draws Done once, ticked (`ColumnFilter`).
test("a status in another case is read as the status of that name", () => {
  const status = (search: string) =>
    readUrl(new URLSearchParams(search)).filters.find(({ id }) => id === "status")?.value;
  expect(status("status=Done")).toEqual(["done"]);
  expect(status("status=Done&status=done&status=TODO")).toEqual(["done", "todo"]);
  // A status this build does not know stays as the address spells it.
  expect(status("status=in-progress")).toEqual(["in-progress"]);
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
    grouping: ["assignee"],
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

/** The array searches that cost a pass over the array they run on. */
const SCANS = ["includes", "indexOf", "splice"] as const;
/**
 * Runs `run` with `Array.prototype.includes`, `indexOf` and `splice` wrapped to add up the length
 * of each array they run on, and answers what `run` answered with that sum. Nothing else is
 * wrapped, and every method is put back before the answer, so no assertion runs while they are.
 */
function lengthsScanned<T>(run: () => T): { value: T; scanned: number } {
  type Scan = (this: unknown[], ...args: unknown[]) => unknown;
  const methods = Array.prototype as unknown as Record<(typeof SCANS)[number], Scan>;
  const originals = SCANS.map((name) => [name, methods[name]] as const);
  let scanned = 0;
  for (const [name, original] of originals)
    methods[name] = function (this: unknown[], ...args: unknown[]) {
      scanned += this.length;
      return original.apply(this, args);
    };
  try {
    const value = run();
    return { value, scanned };
  } finally {
    for (const [name, original] of originals) methods[name] = original;
  }
}

// k18 One (R14): the work Start next does after `Select all N matching` and before its first
// write is linear. Counted by the lengths the array searches run on, not by a clock: a search
// of the queue per selected row adds up to more than N²/2.
test("Start next over 10,000 queued rows searches no array once per selected row", () => {
  const N = 10_000;
  const queue = buildItems(
    Array.from({ length: N }, (_, index) =>
      issue(`Q-${index}`, { rank: String(index).padStart(5, "0"), labels: ["legion"] })
    ),
    (actor) => actor.id
  );
  const every = queue.map((item) => item.issue.key);

  const all = lengthsScanned(() => startNextWrites(queue, queuedInOrder(queue, every)));
  expect(all.value).toEqual([]);
  expect(all.scanned).toBeLessThan(20 * N);

  // Every row but the first: each selected row moves ahead of the first, in queue order.
  const moving = lengthsScanned(() => startNextWrites(queue, queuedInOrder(queue, every.slice(1))));
  expect(moving.value).toHaveLength(N - 1);
  expect(moving.value[0]).toEqual({ key: `Q-${N - 1}`, input: { rank: { before: "Q-0" } } });
  expect(moving.value.at(-1)).toEqual({ key: "Q-1", input: { rank: { before: "Q-2" } } });
  expect(moving.scanned).toBeLessThan(20 * N);
});

// The server matches a label on `lower(label)`, so an issue labelled `Legion` is queued, exactly
// as the lower-case spelling is.
test("the legion label is matched case-insensitively", () => {
  expect(isQueued(issue("A", { labels: ["Legion"] }))).toBe(true);
  expect(isQueued(issue("A", { labels: ["LEGION"], status: "testing" }))).toBe(false);
  const rows = buildItems([issue("B", { labels: ["Legion"] })], (actor) => actor.id);
  expect(rows.find((row) => row.issue.key === "B")?.queue).toBe(1);
});

test("Nobody or lapsed picks unheld and takeable claims, and ?unclaimed=1 still sets it", () => {
  const held = issue("A", {
    claim: { actor: { kind: "session", id: "one" }, at: "2026-09-10T00:00:00Z" },
  });
  const rows = buildItems(
    [held, issue("B")],
    (actor) => actor.id,
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

// A pick named like a member of `Object.prototype` is a value this build does not know, so it
// keeps its own text: `?f.waited=constructor` draws "constructor", never a function's source,
// which the filter's list could not fold.
test("a pick named like an Object member keeps its own text", () => {
  for (const id of [
    "queue",
    "asks",
    "activity",
    "waited",
    "waiting_reason",
    "review_rounds",
    "legion",
  ]) {
    for (const value of ["constructor", "toString", "__proto__"])
      expect(showOf(id)(value)).toBe(value);
  }
  expect(readUrl(new URLSearchParams("f.waited=constructor")).filters).toContainEqual({
    id: "waited",
    value: ["constructor"],
  });
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

// I3: a number inside a name reads as a number, and the empty group reads last.
test("groups order by number inside a name, with the empty group last", () => {
  // Neither column lists its values in an order of its own, so its groups read by name.
  expect(compareGroups("assignee", "agent-2", "agent-10", undefined)).toBeLessThan(0);
  expect(compareGroups("claim", "Nobody", "zoe", undefined)).toBeGreaterThan(0);
  expect(compareGroups("assignee", "Unassigned", "alice", undefined)).toBeGreaterThan(0);
});

// R5: an address of the build before still loads. The table has no Epic column and no grouping
// by it, so `f.epic` narrows nothing and `group=epic` groups nothing, and `q=epic` matches only
// what a row's own text holds: no row holds the words No epic or Unknown epic now.
test("f.epic, group=epic and q=epic name no column, grouping or text of a row", () => {
  expect(readUrl(new URLSearchParams("f.epic=No+epic&group=epic&sort=epic"))).toEqual(defaultView);
  const [plain, labelled] = buildItems(
    [issue("CORE-1", { parent: "CORE-9" }), issue("CORE-2", { labels: ["epic"] })],
    () => ""
  );
  if (plain === undefined || labelled === undefined) throw new Error("no items");
  expect(matchesSearch(plain, "epic")).toBe(false);
  expect(matchesSearch(labelled, "epic")).toBe(true);
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
    grouping: ["assignee"],
    search: "rank",
    page: 2,
    pageSize: 25,
    filters: [{ id: "labels", value: ["frontend"] }],
  };
  const scoped = writeUrl(new URLSearchParams("sort=-priority&q=other"), state, "advances");
  expect(scoped.get("advances.sort")).toBe("-key");
  expect(scoped.get("advances.group")).toBe("assignee");
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

// An unrelated write must leave the address's own value whole: the search box caps what the
// reader types (`useTableAddress`'s `setSearch`), and the table rewrites every field on any
// sort, group, filter or page change, so a cut on the way in would shorten a longer `?q=` a
// link carried as soon as the reader pressed a header.
test("a sort or a page step leaves a longer ?q= a link carried whole", () => {
  const long = "x".repeat(SEARCH_QUERY_MAX + 200);
  const linked = new URLSearchParams(`q=${long}`);
  expect(readUrl(linked).search).toBe(long);
  const sorted = writeUrl(linked, { ...readUrl(linked), sorting: [{ id: "key", desc: true }] });
  expect(sorted.get("q")).toBe(long);
  const paged = writeUrl(sorted, { ...readUrl(sorted), page: 2 });
  expect(paged.get("q")).toBe(long);
  // The same under a prefix, which is what the priorities page's work tables write.
  const prefixed = new URLSearchParams(`w.q=${long}`);
  expect(readUrl(prefixed, "w").search).toBe(long);
  expect(writeUrl(prefixed, { ...readUrl(prefixed, "w"), page: 1 }, "w").get("w.q")).toBe(long);
});

// --- The Legion columns ------------------------------------------------------------------------

const NOW = Date.parse("2026-11-17T12:00:00Z");
const HOUR = 3_600_000;
const DAY = 24 * HOUR;
const ago = (ms: number) => new Date(NOW - ms).toISOString();

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
    as_of: "2026-11-17T12:00:00Z",
    closed_at: null,
    growth: { hours_by_phase: {}, slot_hours: 0, work_hours: 0, work_last_at: 0 },
    hours_by_phase: null,
    opened_at: "2026-11-01T00:00:00Z",
    review_rounds: 0,
    root_key: "CORE-1",
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

/** The board route's answer: its columns in the board's order, each with the cards `cards`
 *  gives it, and the `issues` map. */
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

const alice = { kind: "user", id: "alice" };
const bob = { kind: "user", id: "bob" };

/** Nine rows of a project with a Legion daemon: cards in three columns, facts from the board's
 *  map and from the items read, and rows with no Legion work. */
const LEGION_ISSUES: IssueSummary[] = [
  issue("CORE-1", { created_by: alice }),
  issue("CORE-2", {
    labels: ["legion"],
    rank: "b",
    created_by: { kind: "session", id: "s-1", owner: "alice" },
  }),
  issue("CORE-3", { rank: "c", created_by: bob }),
  issue("CORE-4", { labels: ["legion"], rank: "a", created_by: bob }),
  issue("CORE-5", { status: "in_progress", created_by: bob }),
  issue("CORE-6", { rank: "d", created_by: bob }),
  issue("CORE-7", { status: "done", closed_at: ago(DAY), created_by: bob }),
  issue("CORE-8", { status: "done", closed_at: ago(40 * DAY), created_by: bob }),
  issue("CORE-9", { status: "backlog", open_asks: 2, created_by: bob }),
];
const LEGION_BOARD = board(
  {
    implementing: [
      card("CORE-1", {
        roles: ["reviewer", "implementer"],
        slot_use: "working",
        phase_since: ago(2 * HOUR),
      }),
    ],
    queue: [
      card("CORE-2", {
        waiting_place: 1,
        waiting: { reason: "ready", since: ago(30 * 60_000), blocked_by: null },
      }),
      card("CORE-6", {
        waiting_place: 2,
        waiting: { reason: "blocked", since: ago(2 * DAY), blocked_by: ["CORE-9"] },
      }),
      // A Queue card the daemon's line gives no place.
      card("CORE-3", { waiting: { reason: "slot", since: ago(3 * DAY), blocked_by: null } }),
    ],
    awaiting_person: [
      card("CORE-5", {
        roles: ["architect"],
        slot_use: "held_person",
        phase_since: ago(10 * DAY),
        waiting: { reason: "person", since: ago(10 * DAY), blocked_by: null },
      }),
    ],
  },
  {
    "CORE-1": facts({
      work_hours: 30,
      review_rounds: 3,
      hours_by_phase: { implementing: 20, reviewing: 10 },
    }),
    // Legion worked it and measured no work and no review round.
    "CORE-5": facts({ tree_depth: 1, work_first_at: null, work_hours: 0, hours_by_phase: {} }),
  }
);
/** CORE-7's facts, which only the items read of `legion-work-time` holds. */
const CLOSED_FACTS: LegionFacts = {
  closed_at: ago(DAY),
  hours_by_phase: { design: 1, other: 4 },
  review_rounds: 1,
  root_key: "CORE-7",
  tree_depth: 0,
  work_first_at: "2026-11-10T00:00:00Z",
  work_hours: 5,
};
const ANSWERED: ItemContext = {
  legion: {
    board: LEGION_BOARD,
    boardStatus: "success",
    items: { "CORE-7": CLOSED_FACTS },
    itemsStatus: "success",
  },
  viewer: "alice",
};
/** The rows of `context`, built at NOW: each length of time a row files under counts to it. */
const legionRows = (context: ItemContext = ANSWERED) =>
  buildItems(LEGION_ISSUES, (actor) => actor.id, undefined, { nowMs: NOW, ...context });
/** Each value of column `id` and how many rows it would list, with no other filter. */
const countsOf = (id: string, context?: ItemContext) =>
  Object.fromEntries(facetCounts(legionRows(context), id, [], "", NOW));
const showOf = (id: string) => {
  const filter = columnFilterSpec(id);
  if (filter?.kind !== "picks" || filter.show === undefined) throw new Error(`${id} shows none`);
  return filter.show;
};
const keysWhere = (filters: { id: string; value: unknown }[], context?: ItemContext) =>
  legionRows(context)
    .filter((item) => matchesFilters(item, filters, undefined, NOW))
    .map((item) => item.issue.key);

test("each card column picks what the rows' cards hold, in the board's words and order", () => {
  expect(countsOf("legion_phase")).toEqual({
    implementing: 1,
    queue: 3,
    awaiting_person: 1,
    none: 4,
  });
  const phase = columnFilterSpec("legion_phase");
  if (phase?.kind !== "picks") throw new Error("Legion phase picks");
  // Every Legion phase in the board's order, then None, whatever the rows hold.
  expect(phase.always).toEqual([...LEGION_PHASES.map(({ id }) => id), "none"]);
  expect(phase.always?.map(showOf("legion_phase"))).toEqual([
    "Queue",
    "Design",
    "Planning",
    "Implementing",
    "Testing",
    "Reviewing",
    "Accepting",
    "Retro",
    "Merging",
    "Waiting on children",
    "Waiting on a person",
    "Held",
    "None",
  ]);
  expect(keysWhere([{ id: "legion_phase", value: ["none"] }])).toEqual([
    "CORE-4",
    "CORE-7",
    "CORE-8",
    "CORE-9",
  ]);

  expect(countsOf("roles")).toEqual({ reviewer: 1, implementer: 1, architect: 1, none: 7 });
  expect(columnFilterSpec("roles")).toMatchObject({
    order: ["architect", "planner", "implementer", "tester", "reviewer", "merger", "none"],
  });
  expect(["implementer", "none"].map(showOf("roles"))).toEqual(["Implementer", "None"]);

  expect(countsOf("activity")).toEqual({ working: 1, held_person: 1, none: 7 });
  expect(
    ["working", "between_turns", "held_children", "held_person", "held_idle", "none"].map(
      showOf("activity")
    )
  ).toEqual([
    "Working",
    "Between turns",
    "Waiting on child issues",
    "Waiting on a person",
    "Idle",
    "None",
  ]);

  expect(countsOf("waiting_reason")).toEqual({ ready: 1, blocked: 1, slot: 1, person: 1, none: 5 });
  expect(["ready", "slot", "blocked", "person", "none"].map(showOf("waiting_reason"))).toEqual([
    "Ready to start",
    "Capacity to go on",
    "A blocker",
    "A person",
    "None",
  ]);
  // Workers, Worker state and Why it waits offer the values the rows hold, not every pick.
  for (const id of ["roles", "activity", "waiting_reason"]) {
    const filter = columnFilterSpec(id);
    expect(filter?.kind === "picks" ? filter.always : "no picks").toBeUndefined();
  }
});

test("the duration columns pick each length to now, and a missing measure is None", () => {
  expect(countsOf("waited")).toEqual({ "under-1h": 1, "1d-7d": 2, "over-7d": 1, none: 5 });
  expect(countsOf("time_in_phase")).toEqual({ "1h-1d": 1, "over-7d": 1, none: 7 });
  // CORE-5's facts hold no measured work (`work_first_at` null): its Work time is None, not 0.
  expect(countsOf("work_hours")).toEqual({ "1d-7d": 1, "1h-1d": 1, none: 7 });
  for (const id of ["waited", "time_in_phase", "work_hours"]) {
    const filter = columnFilterSpec(id);
    if (filter?.kind !== "picks") throw new Error(`${id} picks`);
    expect(filter.always).toEqual(["under-1h", "1h-1d", "1d-7d", "over-7d", "none"]);
    expect(filter.always?.map(showOf(id))).toEqual([
      "Under 1 hour",
      "1 hour to 1 day",
      "1 to 7 days",
      "Over 7 days",
      "None",
    ]);
  }
  expect(keysWhere([{ id: "work_hours", value: ["none"] }])).toContain("CORE-5");
});

test("the facts columns pick counted zeros, Root or Child, and only the phases with hours", () => {
  // CORE-5 is a counted zero; CORE-8 and the open rows the map does not hold have no Legion work.
  expect(countsOf("review_rounds")).toEqual({ "3-or-more": 1, "0": 1, "1": 1, none: 6 });
  expect(["0", "3-or-more", "none"].map(showOf("review_rounds"))).toEqual([
    "0",
    "3 or more",
    "None",
  ]);
  expect(countsOf("legion")).toEqual({ root: 2, child: 1, none: 6 });
  expect(["root", "child", "none"].map(showOf("legion"))).toEqual(["Root", "Child", "None"]);
  // Several phases select the rows with hours in any of them, and no row is None.
  expect(countsOf("phase_hours")).toEqual({ implementing: 1, reviewing: 1, design: 1, other: 1 });
  expect(["design", "held", "other"].map(showOf("phase_hours"))).toEqual([
    "Design",
    "Held",
    "Other",
  ]);
  expect(keysWhere([{ id: "phase_hours", value: ["reviewing", "design"] }]).sort()).toEqual([
    "CORE-1",
    "CORE-7",
  ]);
  for (const id of ["review_rounds", "legion"]) {
    const filter = columnFilterSpec(id);
    expect(filter?.kind === "picks" ? filter.always : []).toContain("none");
  }
  const hours = columnFilterSpec("phase_hours");
  expect(hours?.kind === "picks" ? hours.always : "no picks").toBeUndefined();
  expect(hours?.kind === "picks" ? hours.order : []).not.toContain("none");
});

// Time per phase draws no phase a newer server adds: such a key's hours are Other's in its filter
// as in its cell, so it offers no pick for the key, and its Other pick lists the row.
test("Time per phase's filter counts a key it does not draw as Other, and its Other pick lists that row", () => {
  const added: ItemContext = {
    legion: {
      board: {
        ...LEGION_BOARD,
        issues: { ...LEGION_BOARD.issues, "CORE-6": facts({ hours_by_phase: { x_phase: 2 } }) },
      },
      boardStatus: "success",
      items: { "CORE-7": CLOSED_FACTS },
      itemsStatus: "success",
    },
  };
  expect(countsOf("phase_hours", added)).toEqual({
    implementing: 1,
    reviewing: 1,
    design: 1,
    other: 2,
  });
  expect(keysWhere([{ id: "phase_hours", value: ["other"] }], added).sort()).toEqual([
    "CORE-6",
    "CORE-7",
  ]);
});

test("a facts read that is out or failed is no missing measure: its rows are in no pick", () => {
  const boardOut: ItemContext = {
    legion: { board: undefined, boardStatus: "pending", items: undefined, itemsStatus: "pending" },
  };
  expect(legionRows(boardOut).map((item) => item.factsRead)).toEqual(
    LEGION_ISSUES.map(() => "pending")
  );
  expect(countsOf("work_hours", boardOut)).toEqual({});
  expect(keysWhere([{ id: "legion", value: ["none"] }], boardOut)).toEqual([]);
  const boardFailed: ItemContext = {
    legion: { board: undefined, boardStatus: "error", items: undefined, itemsStatus: "pending" },
  };
  expect(new Set(legionRows(boardFailed).map((item) => item.factsRead))).toEqual(
    new Set(["error"])
  );
  // The board answered: an open row the map does not hold has no Legion work, and a closed one
  // waits on the items read.
  for (const itemsStatus of ["pending", "error"] as const) {
    const rows = legionRows({
      legion: { board: LEGION_BOARD, boardStatus: "success", items: undefined, itemsStatus },
    });
    expect(Object.fromEntries(rows.map((item) => [item.issue.key, item.factsRead]))).toEqual({
      "CORE-1": "success",
      "CORE-2": "success",
      "CORE-3": "success",
      "CORE-4": "success",
      "CORE-5": "success",
      "CORE-6": "success",
      "CORE-7": itemsStatus,
      "CORE-8": itemsStatus,
      "CORE-9": "success",
    });
  }
  const answered = legionRows();
  expect(answered.find((item) => item.issue.key === "CORE-7")?.facts).toEqual(CLOSED_FACTS);
  expect(answered.find((item) => item.issue.key === "CORE-8")?.facts).toBeUndefined();
});

test("the Legion queue is the Place or a Queue card with a daemon, and today's set without", () => {
  const rows = legionRows();
  expect(rows.filter((item) => item.queued).map((item) => item.issue.key)).toEqual([
    "CORE-2",
    "CORE-3",
    "CORE-6",
  ]);
  expect(rows.map((item) => item.place ?? null)).toEqual([
    null,
    1,
    null,
    null,
    null,
    2,
    null,
    null,
    null,
  ]);
  expect(countsOf("queue")).toEqual({ Queued: 3, "Not queued": 6 });
  expect(["Queued", "Not queued"].map(showOf("queue"))).toEqual([
    "In Legion queue",
    "Not in Legion queue",
  ]);
  // Start next keeps its own set: the Todo issues labelled `legion`, in rank order.
  expect(rows.filter((item) => item.queue !== undefined).map((item) => item.issue.key)).toEqual([
    "CORE-2",
    "CORE-4",
  ]);
  // With no daemon no row has a card or facts, and the Queue is today's number.
  const plain = legionRows({});
  expect(plain.filter((item) => item.queued).map((item) => [item.issue.key, item.place])).toEqual([
    ["CORE-2", 2],
    ["CORE-4", 1],
  ]);
  expect(plain.some((item) => item.card !== undefined || item.facts !== undefined)).toBe(false);
  expect(new Set(plain.map((item) => item.factsRead))).toEqual(new Set(["success"]));
  expect(keysWhere([{ id: "queue", value: ["Queued"] }], {})).toEqual(["CORE-2", "CORE-4"]);
});

test("Open asks picks With and No open asks, and a link that picked a count keeps its rows", () => {
  expect(countsOf("asks")).toEqual({ any: 1, none: 8 });
  expect(["any", "none"].map(showOf("asks"))).toEqual(["With open asks", "No open asks"]);
  expect(readUrl(new URLSearchParams("f.asks=0")).filters).toContainEqual({
    id: "asks",
    value: ["none"],
  });
  const counted = readUrl(new URLSearchParams("f.asks=2")).filters;
  expect(counted).toContainEqual({ id: "asks", value: ["any"] });
  expect(keysWhere(counted.filter(({ id }) => id === "asks"))).toEqual(["CORE-9"]);
});

test("Created by's Me is the reader, or an agent session the reader owns", () => {
  expect(countsOf("author")).toEqual({ alice: 1, "s-1": 1, bob: 7, "@me": 2 });
  expect(showOf("author")("@me")).toBe("Me");
  const mine = readUrl(new URLSearchParams("f.author=%40me")).filters;
  expect(keysWhere(mine.filter(({ id }) => id === "author"))).toEqual(["CORE-1", "CORE-2"]);
  // A reader the page does not know is nobody's Me.
  expect(countsOf("author", { ...ANSWERED, viewer: undefined })["@me"]).toBeUndefined();
});

test("Closed filters by its window: the last N days, a range of UTC days, or all", () => {
  expect(columnFilterSpec("closed")?.kind).toBe("range");
  expect(readUrl(new URLSearchParams("f.closed=7d")).filters).toContainEqual({
    id: "closed",
    value: "7d",
  });
  expect(keysWhere([{ id: "closed", value: "7d" }])).toEqual(["CORE-7"]);
  expect(keysWhere([{ id: "closed", value: "90d" }])).toEqual(["CORE-7", "CORE-8"]);
  // `from` included and `to` left out.
  const day = ago(DAY).slice(0, 10);
  expect(keysWhere([{ id: "closed", value: `${day}..` }])).toEqual(["CORE-7"]);
  expect(keysWhere([{ id: "closed", value: `..${day}` }])).toEqual(["CORE-8"]);
  expect(keysWhere([{ id: "closed", value: "all" }])).toHaveLength(LEGION_ISSUES.length);
});

// T4 found that `readUrl` dropped a sort of a column the table did not have, so Done's own sort
// read as Open's. Closed is a column now, and Done's address round-trips with its sort.
test("view=done reads its sort, -closed, and a write of it leaves view=done alone", () => {
  const grammar = viewsGrammar({
    read: (params) => readUrl(params),
    write: (params, view) => writeUrl(params, view),
  });
  for (const link of ["view=done", "view=done&sort=-closed"]) {
    const view = grammar.read(new URLSearchParams(link));
    expect(view.sorting).toEqual([{ id: "closed", desc: true }]);
    expect(view.filters).toEqual([{ id: "status", value: ["done"] }]);
    expect(grammar.write(new URLSearchParams(link), view).toString()).toBe("view=done");
  }
  const done = grammar.read(new URLSearchParams("view=done"));
  const resorted = grammar.write(new URLSearchParams("view=done"), {
    ...done,
    sorting: [{ id: "work_hours", desc: true }],
  });
  expect(resorted.toString()).toBe("view=done&sort=-work_hours");
  expect(grammar.read(resorted).sorting).toEqual([{ id: "work_hours", desc: true }]);
  // Every Legion column is a sort and a filter the address keeps.
  for (const { id } of COLUMNS.filter((column) => column.boardFed))
    expect(readUrl(new URLSearchParams(`sort=${id}`)).sorting).toEqual([{ id, desc: false }]);
});

test("Group by offers main's groupings and Legion phase, in the board's order with None last", () => {
  expect(COLUMNS.filter(({ id }) => canGroupBy(id)).map(({ id }) => id)).toEqual([
    "priority",
    "status",
    "legion_phase",
    "assignee",
    "author",
    "claim",
    "created",
    "updated",
  ]);
  const heads = [...new Set(legionRows().map((item) => groupValue("legion_phase", item)))];
  // The drawn column's order is its filter's (`legionPhaseFilter`), here over today's phases.
  const { order } = legionPhaseFilter(LEGION_PHASES);
  expect(heads.sort((a, b) => compareGroups("legion_phase", a, b, order))).toEqual([
    "queue",
    "implementing",
    "awaiting_person",
    "none",
  ]);
  // Retro heads its group between Accepting and Merging.
  expect(
    ["none", "merging", "retro", "accepting"].sort((a, b) =>
      compareGroups("legion_phase", a, b, order)
    )
  ).toEqual(["accepting", "retro", "merging", "none"]);
  expect(readUrl(new URLSearchParams("group=legion_phase")).grouping).toEqual(["legion_phase"]);
});

test("each measure names its method word for word as the Design's table gives it", () => {
  expect(MEASURE_METHODS).toEqual({
    work_hours:
      "Dispatch counts the hours from the first Legion worker event until the issue closes, or until Dispatch last read it. It does not count Backlog or Icebox hours, or hours when Legion has admitted the issue but not started it.",
    phase_hours:
      "Each hour goes to Waiting on a person while the issue's ask or root waits, else to that hour's phase. Other holds the rest, and a root's hours waiting on its children do not count.",
    review_rounds:
      "Dispatch counts each start of the Reviewing phase. For an issue with no recorded Legion phases, it counts each move to Needs review.",
    waited:
      "Dispatch shows the time since Legion asked a person, when the issue waits on a person. If it does not wait on a person, it shows the time since its Legion phase started.",
    time_in_phase:
      "Dispatch shows the time since Legion first recorded the issue in its current Legion phase.",
  });
});

test("a view's layout shows its own columns in its order, with each header and its button whole", () => {
  const open = viewLayoutColumns(viewColumns(viewFor("open"), true), false);
  expect(open.ids.slice(0, 13)).toEqual([
    "select",
    "priority",
    "key",
    "title",
    "queue",
    "status",
    "author",
    "asks",
    "legion_phase",
    "assignee",
    "labels",
    "claim",
    "created",
  ]);
  expect(open.ids).toHaveLength(COLUMNS.length);
  expect(open.hiddenByDefault).toMatchObject({ updated: false, closed: false, work_hours: false });
  expect(open.hiddenByDefault).not.toHaveProperty("asks");
  expect(open.pins?.wide.columns).toEqual(["select", "priority", "key", "title"]);
  const done = viewLayoutColumns(viewColumns(viewFor("done"), true), true);
  expect(done.ids.slice(0, 10)).toEqual([
    "select",
    "priority",
    "key",
    "title",
    "closed",
    "phase_hours",
    "work_hours",
    "assignee",
    "author",
    "labels",
  ]);
  // A project with no daemon opens with no Queue and no board-fed column.
  const plain = viewLayoutColumns(viewColumns(viewFor("open"), false), false);
  expect(plain.hiddenByDefault).toMatchObject({ queue: false, legion_phase: false });
  // 44 px buttons below 1280 px, 32 px from it; a minimum its cell sets is the same at both.
  expect(columnMinimums(true).assignee).toBe(140);
  expect(columnMinimums(false).assignee).toBe(128);
  for (const id of ["title", "key", "status", "claim"])
    expect(columnMinimums(false)[id]).toBe(columnMinimums(true)[id]);
  expect(columnMinimums(false).title).toBe(240);
  expect(columnMinimums(true).claim).toBe(324);
  expect(phoneMin("title")).toBe(120);
  // The same object for the same columns and buttons, since the stored layout is read again
  // whenever it changes.
  expect(viewLayoutColumns(viewColumns(viewFor("open"), true), false)).toBe(open);
});

test("Time per phase keeps a minimum that holds its label, its Share and Hours control and its filter on one line, and on a phone the two lines of its stacked header", () => {
  // Measured in a browser, sorted: the label and its mark, the Share and Hours control and the
  // filter fill 268 px with 44 px buttons and 256 px with 32 px ones. A cell adds 16 px of
  // padding from `md`, 12 px below it.
  expect(columnMinimums(true).phase_hours).toBe(268 + 16);
  expect(columnMinimums(false).phase_hours).toBe(256 + 16);
  // A phone stacks the label (118 px with its mark) over the control and the filter, so the
  // wider line decides: the 94 px control with 44 px buttons, a 4 px gap and the 44 px filter.
  expect(phoneMin("phase_hours")).toBe(94 + 4 + 44 + 16);
});

test("a view's widths give way at Title first, down to 240 px, then at the other columns", () => {
  const ids = ["select", "title", "author", "status"];
  const widths = { select: 44, title: 380, author: 200, status: 150 };
  const minimums = { select: 44, title: 240, author: 100, status: 150 };
  expect(fitViewWidths(ids, widths, minimums, 1000, 0)).toEqual(widths);
  expect(fitViewWidths(ids, widths, minimums, 700, 0)).toEqual({ ...widths, title: 304 });
  expect(fitViewWidths(ids, widths, minimums, 600, 0)).toEqual({
    ...widths,
    title: 240,
    author: 164,
  });
  // What does not fit at every minimum scrolls sideways.
  expect(fitViewWidths(ids, widths, minimums, 300, 104)).toEqual({
    ...widths,
    title: 240,
    author: 100,
  });
});

test("an issue table another page draws keeps today's columns and widths, with Closed hidden", () => {
  expect(ISSUE_TABLE_COLUMNS.ids).toEqual([
    "select",
    "priority",
    "queue",
    "key",
    "title",
    "status",
    "assignee",
    "author",
    "labels",
    "claim",
    "asks",
    "created",
    "updated",
    "closed",
  ]);
  expect(ISSUE_TABLE_COLUMNS.hiddenByDefault).toEqual({ updated: false, closed: false });
  expect(ISSUE_TABLE_COLUMNS.minimums).toMatchObject({ key: 108, title: 116, status: 132 });
  expect(ISSUE_TABLE_COLUMNS.sizes).toMatchObject({ key: 104, queue: 96, claim: 220 });
});
