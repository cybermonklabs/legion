import { expect, test } from "bun:test";

import type { IssueSummary, LegionBoardResponse } from "../../api/types";
import {
  type BoardBy,
  boardColumns,
  boardLines,
  cardDropId,
  cellId,
  DAEMON_SETS_PHASE,
  dropPlace,
  keyRefusal,
  laneKey,
  laneLabel,
  type MoveRules,
  openIssueStatuses,
  planMove,
  priorityLanes,
  rankAfter,
  rankRefusal,
} from "./board-model";
import { LEGION_PHASES } from "./issues-legion";
import { buildItems, type Item } from "./issues-table-model";

function issue(key: string, overrides: Partial<IssueSummary> = {}): IssueSummary {
  return {
    advances: [],
    advances_revision: 0,
    route: null,
    repo: null,
    resolved_repo: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
    key,
    title: `Work ${key}`,
    status: "todo",
    priority: null,
    rank: key,
    labels: [],
    parent: null,
    blocked_by: [],
    primary_artifact_id: "artifact-1",
    assignee: null,
    claim: null,
    components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
    updated_at: "2026-09-12T00:00:00Z",
    closed_at: overrides.status === "done" ? "2026-09-12T00:00:00Z" : null,
    created_by: null,
    created_at: "2026-09-12T00:00:00Z",
    last_seq: 1,
    open_asks: 0,
    progress: { tasks: null, children: null },
    ...overrides,
  };
}

/** The rows a view lists, as the table builds them; with `board`, a project with a daemon. */
function rows(issues: IssueSummary[], board?: LegionBoardResponse): Item[] {
  return buildItems(
    issues,
    () => "someone",
    () => true,
    board === undefined
      ? {}
      : {
          legion: { board, boardStatus: "success", items: undefined, itemsStatus: "success" },
        }
  );
}

/** A board whose columns hold `cards`, each by its key, with the place it names. */
function legionBoard(
  cards: Partial<Record<string, readonly { key: string; place?: number }[]>>
): LegionBoardResponse {
  return {
    columns: LEGION_PHASES.map((phase) => ({
      id: phase.id,
      label: phase.title,
      cards: (cards[phase.id] ?? []).map(({ key, place }) => ({
        blocked_by: null,
        children: 0,
        key,
        phase: phase.id,
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
        waiting_place: place ?? null,
      })),
    })),
    computed_at: "2026-11-17T08:00:00Z",
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
    issues: {},
    project: "CORE",
    settings: null,
  };
}

/** The Board's lines over `items`, by Status with Open's statuses unless `by` says otherwise. */
function lines(
  items: readonly Item[],
  { by = "status", lanes = false }: { by?: BoardBy; lanes?: boolean } = {}
) {
  const columns = boardColumns({
    by,
    listsDone: false,
    phaseFilterPicks: [],
    phases: LEGION_PHASES,
    rows: items,
    showIcebox: true,
    statusPicks: by === "status" ? openIssueStatuses : [],
  });
  return boardLines(items, columns, by, lanes);
}

const PLACE: MoveRules = { by: "status", lanes: false, rankRefusal: undefined };
const SORTED: MoveRules = {
  by: "status",
  lanes: false,
  rankRefusal: rankRefusal({ id: "created", desc: true }, "Created"),
};

const ids = (columns: readonly { id: string; rail: boolean }[]) =>
  columns.map(({ id, rail }) => (rail ? `${id} rail` : id));

test("by Status the columns are the picked statuses in order, Icebox a rail until shown, and Done last", () => {
  const items = rows([issue("CORE-1", { status: "todo" })]);
  const statusColumns = (statusPicks: readonly string[], showIcebox: boolean, listsDone: boolean) =>
    ids(
      boardColumns({
        by: "status",
        listsDone,
        phaseFilterPicks: [],
        phases: LEGION_PHASES,
        rows: items,
        showIcebox,
        statusPicks,
      })
    );
  expect(statusColumns(openIssueStatuses, false, false)).toEqual([
    "triage",
    "icebox rail",
    "backlog",
    "todo",
    "in_progress",
    "testing",
    "needs_review",
    "retro",
    "done rail",
  ]);
  expect(statusColumns(openIssueStatuses, true, false)[1]).toBe("icebox");
  expect(statusColumns(["triage"], false, false)).toEqual(["triage", "done rail"]);
  // No pick: each status a row is in, and Done a column, since such a view lists done issues.
  expect(statusColumns([], false, true)).toEqual(["todo", "done"]);
  expect(statusColumns(["done"], false, true)).toEqual(["done"]);
});

test("by Legion phase the columns are the twelve phases in the board's order and the None rail, kept by the filter's picks", () => {
  const phaseColumns = (phaseFilterPicks: readonly string[]) =>
    ids(
      boardColumns({
        by: "legion_phase",
        listsDone: true,
        phaseFilterPicks,
        phases: LEGION_PHASES,
        rows: [],
        showIcebox: true,
        statusPicks: ["todo"],
      })
    );
  expect(phaseColumns([])).toEqual([...LEGION_PHASES.map((phase) => phase.id), "none rail"]);
  expect(phaseColumns(["implementing"])).toEqual(["implementing"]);
  expect(phaseColumns(["queue", "none"])).toEqual(["queue", "none rail"]);
});

test("the lines keep the view's order in each cell, and lanes split them by priority", () => {
  const items = rows([
    issue("CORE-3", { status: "todo", priority: 0 }),
    issue("CORE-1", { status: "todo" }),
    issue("CORE-2", { status: "in_progress", priority: 0 }),
    issue("CORE-9", { status: "done" }),
  ]);
  const flat = lines(items);
  expect(flat).toHaveLength(1);
  const cells = flat[0]?.cells ?? [];
  const keysOf = (column: string) =>
    cells.find((cell) => cell.column.id === column)?.items.map((item) => item.issue.key);
  expect(keysOf("todo")).toEqual(["CORE-3", "CORE-1"]);
  expect(keysOf("in_progress")).toEqual(["CORE-2"]);
  // Open lists no done issue: Done is a rail, and holds what the view gives it.
  expect(keysOf("done")).toEqual(["CORE-9"]);

  const laned = lines(items, { lanes: true });
  expect(laned.map((line) => line.lane)).toEqual([...priorityLanes]);
  const todoIn = (lane: number | null) =>
    laned
      .find((line) => line.lane === lane)
      ?.cells.find((cell) => cell.column.id === "todo")
      ?.items.map((item) => item.issue.key);
  expect(todoIn(0)).toEqual(["CORE-3"]);
  expect(todoIn(null)).toEqual(["CORE-1"]);
});

test("by Legion phase a row stands in its card's column, and a row with no card in None", () => {
  const board = legionBoard({
    queue: [{ key: "CORE-1", place: 1 }],
    implementing: [{ key: "CORE-2" }],
  });
  const items = rows([issue("CORE-1"), issue("CORE-2"), issue("CORE-3")], board);
  const cells = lines(items, { by: "legion_phase" })[0]?.cells ?? [];
  const keysOf = (column: string) =>
    cells.find((cell) => cell.column.id === column)?.items.map((item) => item.issue.key);
  expect(keysOf("queue")).toEqual(["CORE-1"]);
  expect(keysOf("implementing")).toEqual(["CORE-2"]);
  expect(keysOf("none")).toEqual(["CORE-3"]);
});

const TODO = () =>
  rows([
    issue("CORE-1", { rank: "a" }),
    issue("CORE-2", { rank: "b" }),
    issue("CORE-3", { rank: "c" }),
    issue("CORE-4", { rank: "d", status: "in_progress" }),
  ]);

test("a drop onto a card lands at its place, one step down below it and one step up above it; onto a column, at its end", () => {
  const board = lines(TODO());
  expect(dropPlace(board, "CORE-1", cardDropId("CORE-2"))).toEqual({
    column: "todo",
    lane: undefined,
    index: 1,
  });
  expect(dropPlace(board, "CORE-3", cardDropId("CORE-2"))).toEqual({
    column: "todo",
    lane: undefined,
    index: 1,
  });
  expect(dropPlace(board, "CORE-1", cardDropId("CORE-4"))).toEqual({
    column: "in_progress",
    lane: undefined,
    index: 0,
  });
  expect(dropPlace(board, "CORE-4", cellId("todo", undefined))).toEqual({
    column: "todo",
    lane: undefined,
    index: 3,
  });
  expect(dropPlace(board, "CORE-1", cellId("todo", undefined))).toEqual({
    column: "todo",
    lane: undefined,
    index: 2,
  });
  expect(dropPlace(board, "CORE-1", cardDropId("CORE-1"))).toBeUndefined();
  expect(dropPlace(board, "CORE-1", "column:nowhere")).toBeUndefined();
  expect(dropPlace(board, "CORE-1", cardDropId("CORE-99"))).toBeUndefined();
});

// By Status, under the Place sort: a move within a column writes the rank before the card it
// lands above, and the card shows a rank between its new neighbours until the server answers.
test("under the Place sort a move within a column writes the rank and names where the card lands", () => {
  const board = lines(TODO());
  expect(planMove(board, PLACE, "CORE-1", { column: "todo", lane: undefined, index: 1 })).toEqual({
    kind: "move",
    key: "CORE-1",
    change: { input: { rank: { before: "CORE-3" } }, rank: rankAfter("b") },
    position: { at: 2, of: 3 },
  });
  // Last in the column: after the card above it.
  expect(
    planMove(board, PLACE, "CORE-1", { column: "todo", lane: undefined, index: 2 })
  ).toMatchObject({
    kind: "move",
    change: { input: { rank: { after: "CORE-3" } }, rank: rankAfter("c") },
    position: { at: 3, of: 3 },
  });
  // A move that leaves the card where it is changes nothing.
  expect(
    planMove(board, PLACE, "CORE-1", { column: "todo", lane: undefined, index: 0 })
  ).toBeUndefined();
});

// INFRA-3221's tester: the daemon gives Places in an order of its own, so two cards next to each
// other by Place need not be in rank order, and the server refuses a rank between two that are
// not. A drop between them names one neighbour, the card it lands above, by Status and in Queue.
test("a drop between two placed cards whose ranks run against their Places names only the card it lands above", () => {
  const board = legionBoard({
    queue: [
      { key: "CORE-4", place: 1 },
      { key: "CORE-3", place: 2 },
      { key: "CORE-5", place: 3 },
    ],
  });
  // In the view's order, by Place: CORE-4 holds Place 1 and ranks after CORE-3.
  const items = rows(
    [
      issue("CORE-4", { rank: "x" }),
      issue("CORE-3", { rank: "w" }),
      issue("CORE-5", { rank: "y" }),
    ],
    board
  );
  const queue: MoveRules = { by: "legion_phase", lanes: false, rankRefusal: undefined };
  for (const [rules, column, by] of [
    [PLACE, "todo", "status"],
    [queue, "queue", "legion_phase"],
  ] as const) {
    const drawn = lines(items, { by });
    const cell = drawn[0]?.cells.find((candidate) => candidate.column.id === column);
    expect(cell?.items.map((item) => item.issue.key)).toEqual(["CORE-4", "CORE-3", "CORE-5"]);
    expect(planMove(drawn, rules, "CORE-5", { column, lane: undefined, index: 1 })).toMatchObject({
      kind: "move",
      change: { input: { rank: { before: "CORE-3" } } },
      position: { at: 2, of: 3 },
    });
  }
});

test("a move into another column sets its status, with the rank under the Place sort and none under another", () => {
  const board = lines(TODO());
  expect(
    planMove(board, PLACE, "CORE-2", { column: "in_progress", lane: undefined, index: 0 })
  ).toMatchObject({
    kind: "move",
    change: { input: { status: "in_progress", rank: { before: "CORE-4" } }, rank: "" },
    position: { at: 1, of: 2 },
  });
  const sorted = planMove(board, SORTED, "CORE-2", {
    column: "in_progress",
    lane: undefined,
    index: 0,
  });
  expect(sorted).toMatchObject({ kind: "move", change: { input: { status: "in_progress" } } });
  expect(sorted?.kind === "move" ? sorted.position : "refused").toBeUndefined();
  // Onto the Done rail of a view that lists no done issue: the status, which closes it, alone.
  expect(
    planMove(board, PLACE, "CORE-2", { column: "done", lane: undefined, index: 0 })
  ).toMatchObject({
    kind: "move",
    change: { input: { status: "done" } },
  });
});

test("under another sort a move within a column is refused with a text that names the sort", () => {
  const board = lines(TODO());
  expect(planMove(board, SORTED, "CORE-1", { column: "todo", lane: undefined, index: 2 })).toEqual({
    kind: "refused",
    text: "The view sorts by Created. Sort by Queue to move cards in a column.",
  });
});

// The Place sort puts every card with a place in the Legion queue first, so the rank decides a
// move only between cards that all have a place or all have none.
test("a move between cards with a place and cards with none is refused with why", () => {
  const board = lines(
    rows([
      issue("CORE-1", { rank: "a", labels: ["legion"] }),
      issue("CORE-2", { rank: "b", labels: ["legion"] }),
      issue("CORE-3", { rank: "c" }),
    ])
  );
  expect(planMove(board, PLACE, "CORE-3", { column: "todo", lane: undefined, index: 1 })).toEqual({
    kind: "refused",
    text: "A card with no place in the Legion queue stays after the cards with a place.",
  });
  expect(planMove(board, PLACE, "CORE-1", { column: "todo", lane: undefined, index: 1 })).toEqual({
    kind: "refused",
    text: "A card with a place in the Legion queue stays before the cards with no place.",
  });
  expect(
    planMove(board, PLACE, "CORE-2", { column: "todo", lane: undefined, index: 0 })
  ).toMatchObject({
    kind: "move",
    change: { input: { rank: { before: "CORE-1" } } },
  });
});

test("a drop into another lane writes that lane's priority under any sort", () => {
  const board = lines(
    rows([
      issue("CORE-1", { rank: "a", priority: 0 }),
      issue("CORE-2", { rank: "b", priority: 1 }),
    ]),
    { lanes: true }
  );
  const lanes: MoveRules = { ...SORTED, lanes: true };
  expect(planMove(board, lanes, "CORE-1", { column: "todo", lane: 1, index: 0 })).toMatchObject({
    kind: "move",
    change: { input: { priority: 1 } },
  });
  expect(planMove(board, lanes, "CORE-1", { column: "todo", lane: null, index: 0 })).toMatchObject({
    kind: "move",
    change: { input: { priority: null } },
  });
  expect(planMove(board, lanes, "CORE-1", { column: "backlog", lane: 1, index: 0 })).toMatchObject({
    kind: "move",
    change: { input: { status: "backlog", priority: 1 } },
  });
  // Under the Place sort it writes the rank too, beside the lane's cards.
  expect(
    planMove(board, { ...PLACE, lanes: true }, "CORE-1", { column: "todo", lane: 1, index: 1 })
  ).toMatchObject({
    kind: "move",
    change: { input: { priority: 1, rank: { after: "CORE-2" } } },
  });
});

// By Legion phase the daemon sets each card's column: only a move within Queue is taken.
test("by Legion phase only a move within Queue is taken, and every other drop is refused", () => {
  const board = legionBoard({
    queue: [
      { key: "CORE-1", place: 1 },
      { key: "CORE-2", place: 2 },
    ],
    implementing: [{ key: "CORE-3" }, { key: "CORE-4" }],
  });
  const items = rows(
    [
      issue("CORE-1", { priority: 0 }),
      issue("CORE-2", { priority: 0 }),
      issue("CORE-3"),
      issue("CORE-4"),
      issue("CORE-5"),
    ],
    board
  );
  const rules: MoveRules = { by: "legion_phase", lanes: false, rankRefusal: undefined };
  const phases = lines(items, { by: "legion_phase" });
  expect(
    planMove(phases, rules, "CORE-1", { column: "queue", lane: undefined, index: 1 })
  ).toMatchObject({
    kind: "move",
    change: { input: { rank: { after: "CORE-2" } } },
    position: { at: 2, of: 2 },
  });
  const refused = { kind: "refused", text: DAEMON_SETS_PHASE } as const;
  expect(
    planMove(phases, rules, "CORE-3", { column: "implementing", lane: undefined, index: 1 })
  ).toEqual(refused);
  expect(
    planMove(phases, rules, "CORE-1", { column: "design", lane: undefined, index: 0 })
  ).toEqual(refused);
  expect(planMove(phases, rules, "CORE-3", { column: "queue", lane: undefined, index: 0 })).toEqual(
    refused
  );
  expect(planMove(phases, rules, "CORE-1", { column: "none", lane: undefined, index: 0 })).toEqual(
    refused
  );
  // Within Queue under another sort, the sort's refusal.
  expect(
    planMove(phases, { ...rules, rankRefusal: "Sorted." }, "CORE-1", {
      column: "queue",
      lane: undefined,
      index: 1,
    })
  ).toEqual({ kind: "refused", text: "Sorted." });
  // Into another lane of Queue.
  const laned = lines(items, { by: "legion_phase", lanes: true });
  expect(
    planMove(laned, { ...rules, lanes: true }, "CORE-1", { column: "queue", lane: 1, index: 0 })
  ).toEqual(refused);
});

test("a key move is refused before its place is looked at, by Legion phase but within Queue", () => {
  const phase: MoveRules = { by: "legion_phase", lanes: false, rankRefusal: undefined };
  expect(keyRefusal(phase, "implementing", "within")).toBe(DAEMON_SETS_PHASE);
  expect(keyRefusal(phase, "queue", "across")).toBe(DAEMON_SETS_PHASE);
  expect(keyRefusal(phase, "queue", "within")).toBeUndefined();
  expect(keyRefusal(SORTED, "todo", "within")).toBe(SORTED.rankRefusal);
  expect(keyRefusal(SORTED, "todo", "across")).toBeUndefined();
});

test("the rank decides a move only under the Place sort, first to last", () => {
  expect(rankRefusal({ id: "queue", desc: false }, "Queue")).toBeUndefined();
  expect(rankRefusal({ id: "queue", desc: true }, "Queue")).toBe(
    "The view sorts by Queue from last to first. Sort from first to last to move cards in a column."
  );
  expect(rankRefusal({ id: "priority", desc: false }, "Priority")).toBe(
    "The view sorts by Priority. Sort by Queue to move cards in a column."
  );
  expect(rankRefusal(undefined, "")).toBe(
    "The view has no sort. Sort by Queue to move cards in a column."
  );
});

test("the rank a moved card shows sorts just after the card above it, or first", () => {
  expect(rankAfter("b") > "b" && rankAfter("b") < "c").toBe(true);
  expect(rankAfter("b") < "ba").toBe(true);
  expect(rankAfter(undefined) < "0").toBe(true);
});

test("laneLabel and laneKey name every priority lane", () => {
  expect(priorityLanes.map(laneLabel)).toEqual(["P0", "P1", "P2", "P3", "No priority"]);
  expect(priorityLanes.map(laneKey)).toEqual(["0", "1", "2", "3", "none"]);
  expect(cellId("todo", 0)).toBe("lane:0|column:todo");
  expect(cellId("todo", null)).toBe("lane:none|column:todo");
});
