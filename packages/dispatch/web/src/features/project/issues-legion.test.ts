import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { QueryClient, QueryObserver } from "@tanstack/react-query";
import { http } from "msw";

import { answerRequests } from "../../__tests__/network";
import { ApiError } from "../../api/client";
import { legionItemsQuery } from "../../api/queries";
import { queryRetry, queryRetryDelay } from "../../api/query-client";
import type {
  EvalItemRow,
  LegionBoardColumn,
  LegionBoardFacts,
  LegionBoardResponse,
} from "../../api/types";
import {
  boardFactsAt,
  closedDaysWindows,
  formatPhaseTime,
  formatWorkTime,
  ITEMS_PAGE_SIZE,
  itemFacts,
  itemsWindows,
  LEGION_PHASES,
  type LegionFacts,
  legionFactsOf,
  legionPhasesOf,
  legionPhaseTitle,
  listedDoneRows,
  readLegionItems,
} from "./issues-legion";

function board(issues: LegionBoardResponse["issues"]): LegionBoardResponse {
  return {
    columns: null,
    computed_at: "2026-11-17T08:00:00Z",
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
    project: "ACME",
    settings: null,
  };
}

function boardFacts(rootKey: string, workHours: number): LegionBoardFacts {
  return {
    as_of: "2026-11-17T08:00:00Z",
    closed_at: null,
    growth: { hours_by_phase: {}, slot_hours: 0, work_hours: 0, work_last_at: 0 },
    hours_by_phase: { implementing: workHours },
    opened_at: "2026-11-01T00:00:00Z",
    review_rounds: 1,
    root_key: rootKey,
    slot_hours: workHours,
    tree_depth: 0,
    updated_at: "2026-11-17T08:00:00Z",
    waiting_reason: "",
    work_first_at: "2026-11-02T00:00:00Z",
    work_hours: workHours,
    work_last_at: "2026-11-16T00:00:00Z",
  };
}

function itemRow(key: string, cols: Record<string, unknown> | null): EvalItemRow {
  return {
    cols,
    href: `/issues/${key}`,
    links: [],
    measured_at: "2026-11-01T10:00:00Z",
    range: "window",
    row_key: key,
    row_kind: "issue",
    source: "legionfacts",
  };
}

/** The columns the items route serves for a closed issue (`itemColumns` in
 *  `legionfacts/items.go`), with its title and priority (`WithIssues`). */
const servedColumns = (key: string): Record<string, unknown> => ({
  opened_at: "2026-10-28T00:00:00Z",
  closed_at: "2026-11-01T10:00:00Z",
  work_hours: 3.75,
  review_rounds: 2,
  root_key: "ACME-5",
  tree_depth: 1,
  slot_hours: 4,
  waiting_reason: "",
  work_first_at: "2026-10-30T09:00:00Z",
  work_last_at: "2026-11-01T09:00:00Z",
  parked_hours: 0,
  queued_hours: 1,
  started_at: "2026-10-30T09:00:00Z",
  merged_at: null,
  slot_wait_hours: 0,
  sent_back: 0,
  implementing_after_first_review: false,
  testing_after_first_review: false,
  children: 0,
  repo: "cybermonklabs/legion",
  hours_implementing: 2.25,
  hours_reviewing: 1,
  hours_other: 0.5,
  bar_implementer: 2,
  bar_implementer_in_a_turn: 1.5,
  idle_reviewer: 0.25,
  title: `Work ${key}`,
  priority: 1,
});

test("the Legion phases are the board's columns, in its order, by their titles, with Retro between Accepting and Merging", () => {
  expect(LEGION_PHASES.map(({ id, title }) => [id, title])).toEqual([
    ["queue", "Queue"],
    ["design", "Design"],
    ["planning", "Planning"],
    ["implementing", "Implementing"],
    ["testing", "Testing"],
    ["reviewing", "Reviewing"],
    ["accepting", "Accepting"],
    ["retro", "Retro"],
    ["merging", "Merging"],
    ["awaiting_children", "Waiting on children"],
    ["awaiting_person", "Waiting on a person"],
    ["held", "Held"],
  ]);
  expect(legionPhaseTitle("retro")).toBe("Retro");
  expect(legionPhaseTitle("awaiting_person")).toBe("Waiting on a person");
  // A phase a newer daemon names, which this build does not know, shows as the daemon wrote it.
  expect(legionPhaseTitle("rebasing")).toBe("rebasing");
});

test("the Legion phases are each column id of the board route's contract, and no other", () => {
  // The generated `LegionBoardColumn["id"]` is made from this enum. A column that the board
  // adds and the phase list lacks fails here.
  const contract = JSON.parse(
    readFileSync(
      join(import.meta.dir, "../../../../../contracts/schemas/dispatch-openapi.json"),
      "utf8"
    )
  ) as {
    components: { schemas: { LegionBoardColumn: { properties: { id: { enum: string[] } } } } };
  };
  const columns = contract.components.schemas.LegionBoardColumn.properties.id.enum;
  expect(LEGION_PHASES.map(({ id }): string => id).sort()).toEqual([...columns].sort());
});

test("the page's Legion phases are the board route's columns in its order under its labels, today's until it answers", () => {
  expect(legionPhasesOf(undefined)).toBe(LEGION_PHASES);
  const columns = [
    ...LEGION_PHASES.map(({ id, title }) => ({ id, label: title, cards: [] })),
    // The generated column ids name today's twelve: a newer server's column is none of them.
    { id: "x_phase" as string as LegionBoardColumn["id"], label: "X phase", cards: [] },
  ];
  const phases = legionPhasesOf({ columns });
  expect(phases.map(({ id, title }) => [id, title])).toEqual([
    ...LEGION_PHASES.map(({ id, title }) => [id, title]),
    ["x_phase", "X phase"],
  ]);
  // A later frame of the same columns, whatever cards they hold, is the same list.
  expect(legionPhasesOf({ columns: columns.map((column) => ({ ...column, cards: null })) })).toBe(
    phases
  );
});

test("the time in a phase and the work time read in minutes, hours and minutes, then days", () => {
  const now = Date.parse("2026-11-17T12:00:00Z");
  expect(formatPhaseTime("2026-11-17T09:30:00Z", now)).toBe("2 h 30 m");
  expect(formatPhaseTime("2026-11-17T11:55:30Z", now)).toBe("4 m");
  expect(formatPhaseTime("2026-11-14T07:00:00Z", now)).toBe("3 d 5 h");
  // A phase entry no read recorded, or one this cannot read, is no time anybody measured.
  expect(formatPhaseTime(null, now)).toBe("—");
  expect(formatPhaseTime("yesterday", now)).toBe("—");
  // A clock behind the daemon's never writes a time below nothing.
  expect(formatPhaseTime("2026-11-17T12:05:00Z", now)).toBe("0 m");
  expect(formatWorkTime(3.75)).toBe("3 h 45 m");
  expect(formatWorkTime(0.5)).toBe("30 m");
  expect(formatWorkTime(50)).toBe("2 d 2 h");
  expect(formatWorkTime(-1)).toBe("0 m");
});

test("a row takes the board's facts first, then its items row, then no Legion work", () => {
  const now = Date.parse("2026-11-17T12:00:00Z");
  const held = boardFacts("ACME-1", 5);
  const items: Record<string, LegionFacts> = {
    "ACME-1": { ...boardFacts("ACME-1", 1), closed_at: "2026-11-01T10:00:00Z" },
    "ACME-2": { ...boardFacts("ACME-2", 2), closed_at: "2026-11-02T10:00:00Z" },
  };
  const answered = board({ "ACME-1": held });
  expect(legionFactsOf("ACME-1", answered, items, now)).toBe(held);
  expect(legionFactsOf("ACME-2", answered, items, now)).toBe(items["ACME-2"]);
  expect(legionFactsOf("ACME-3", answered, items, now)).toBeUndefined();
  // A map the board answered as null, or no board yet, leaves the items row.
  expect(legionFactsOf("ACME-1", board(null), items, now)).toBe(items["ACME-1"]);
  expect(legionFactsOf("ACME-1", undefined, items, now)).toBe(items["ACME-1"]);
  expect(legionFactsOf("ACME-1", undefined, undefined, now)).toBeUndefined();
  // A name every object answers through its prototype is no key of either.
  expect(legionFactsOf("constructor", answered, items, now)).toBeUndefined();
});

/** `boardFacts("ACME-1", 3)`, of 08:00, with the rates `rates` gives and every other rate 0. */
const growing = (rates: Partial<NonNullable<LegionBoardFacts["growth"]>>): LegionBoardFacts => ({
  ...boardFacts("ACME-1", 3),
  growth: { hours_by_phase: {}, slot_hours: 0, work_hours: 0, work_last_at: 0, ...rates },
});
/** Two hours after the `as_of` of `boardFacts`. */
const TWO_HOURS_ON = Date.parse("2026-11-17T10:00:00Z");

test("an open board entry two hours after its as_of gains two hours on each value at rate 1", () => {
  const entry = growing({ work_hours: 1, hours_by_phase: { implementing: 1 } });
  expect(boardFactsAt(entry, TWO_HOURS_ON)).toEqual({
    closed_at: null,
    hours_by_phase: { implementing: 5 },
    review_rounds: 1,
    root_key: "ACME-1",
    tree_depth: 0,
    work_first_at: "2026-11-02T00:00:00Z",
    work_hours: 5,
  });
  // The entry the board sent keeps its own values.
  expect(entry.work_hours).toBe(3);
  expect(entry.hours_by_phase).toEqual({ implementing: 3 });
});

test("a phase that only the growth names starts from 0 hours", () => {
  const entry = growing({ hours_by_phase: { reviewing: 0.5 } });
  const closed = boardFactsAt(entry, TWO_HOURS_ON);
  expect(closed.hours_by_phase).toEqual({ implementing: 3, reviewing: 1 });
  expect(closed.work_hours).toBe(3);
  // An entry with no hours by phase at all.
  expect(boardFactsAt({ ...entry, hours_by_phase: null }, TWO_HOURS_ON).hours_by_phase).toEqual({
    reviewing: 1,
  });
});

test("an entry that does not grow, rates of 0, a clock before as_of and an as_of this cannot read leave the values as sent", () => {
  // The route sends null for both on an entry none of whose values grows.
  const closed = { ...growing({ work_hours: 1 }), as_of: null, growth: null };
  expect(boardFactsAt(closed, TWO_HOURS_ON)).toBe(closed);
  const still = growing({ hours_by_phase: { implementing: 0 } });
  expect(boardFactsAt(still, TWO_HOURS_ON)).toBe(still);
  const empty = growing({ hours_by_phase: null });
  expect(boardFactsAt(empty, TWO_HOURS_ON)).toBe(empty);
  const entry = growing({ work_hours: 1, hours_by_phase: { implementing: 1 } });
  expect(boardFactsAt(entry, Date.parse("2026-11-17T07:00:00Z"))).toBe(entry);
  expect(boardFactsAt(entry, Date.parse("2026-11-17T08:00:00Z"))).toBe(entry);
  const unread = { ...entry, as_of: "yesterday" };
  expect(boardFactsAt(unread, TWO_HOURS_ON)).toBe(unread);
});

test("a row's board entry is closed at the clock, and its items row stays as it is", () => {
  const entry = growing({ work_hours: 1, hours_by_phase: { implementing: 1 } });
  // An items row is a closed issue's: a growth it carries is no rate it grows by.
  const items: Record<string, LegionFacts> = {
    "ACME-2": { ...entry, closed_at: "2026-11-17T08:00:00Z", root_key: "ACME-2" },
  };
  const answered = board({ "ACME-1": entry });
  const held = legionFactsOf("ACME-1", answered, items, TWO_HOURS_ON);
  expect(held).toEqual(boardFactsAt(entry, TWO_HOURS_ON));
  expect(held?.work_hours).toBe(5);
  expect(held?.hours_by_phase).toEqual({ implementing: 5 });
  expect(legionFactsOf("ACME-2", answered, items, TWO_HOURS_ON)).toBe(items["ACME-2"]);
  expect(items["ACME-2"]?.work_hours).toBe(3);
});

test("an items row reads as the facts the board's map holds for an issue", () => {
  const row = itemRow("ACME-7", servedColumns("ACME-7"));
  expect(itemFacts(row)).toEqual({
    closed_at: "2026-11-01T10:00:00Z",
    hours_by_phase: { implementing: 2.25, reviewing: 1, other: 0.5 },
    review_rounds: 2,
    root_key: "ACME-5",
    tree_depth: 1,
    work_first_at: "2026-10-30T09:00:00Z",
    work_hours: 3.75,
  });
  // An issue no worker event started has no first work moment.
  const unstarted = itemRow("ACME-8", { ...servedColumns("ACME-8"), work_first_at: null });
  expect(itemFacts(unstarted)?.work_first_at).toBeNull();
  // A row without a fact every issue row carries is no facts row.
  const filing = itemRow("ACME-9", { filed_at: "2026-11-01T10:00:00Z", root_key: "ACME-9" });
  expect(itemFacts(filing)).toBeUndefined();
  expect(itemFacts(itemRow("ACME-10", null))).toBeUndefined();
});

test("a long span splits into windows of at most 84 days, each from one UTC midnight to another", () => {
  const windows = closedDaysWindows(
    Date.parse("2026-01-01T00:00:00Z"),
    Date.parse("2026-07-01T12:00:00Z")
  );
  // The route leaves `to` out, so the last window ends at the midnight after the last close.
  expect(windows).toEqual([
    { from: "2026-01-01T00:00:00Z", to: "2026-03-26T00:00:00Z" },
    { from: "2026-03-26T00:00:00Z", to: "2026-06-18T00:00:00Z" },
    { from: "2026-06-18T00:00:00Z", to: "2026-07-02T00:00:00Z" },
  ]);
  const days = ({ from, to }: { from: string; to: string }) =>
    (Date.parse(to) - Date.parse(from)) / 86_400_000;
  expect(windows.map(days)).toEqual([84, 84, 14]);
  // 84 whole days are one window; a close on the 85th day takes a second.
  expect(
    closedDaysWindows(Date.parse("2026-01-01T00:00:00Z"), Date.parse("2026-03-25T23:59:59Z"))
  ).toEqual([{ from: "2026-01-01T00:00:00Z", to: "2026-03-26T00:00:00Z" }]);
  expect(
    closedDaysWindows(Date.parse("2026-01-01T00:00:00Z"), Date.parse("2026-03-26T00:00:00Z"))
  ).toHaveLength(2);
});

test("a span that starts off midnight starts at that day's midnight, and spans 84 days, not 85", () => {
  expect(
    closedDaysWindows(Date.parse("2026-11-03T15:20:00Z"), Date.parse("2026-11-05T01:00:00Z"))
  ).toEqual([{ from: "2026-11-03T00:00:00Z", to: "2026-11-06T00:00:00Z" }]);
  const [first] = closedDaysWindows(
    Date.parse("2026-01-01T15:20:00Z"),
    Date.parse("2026-06-01T08:00:00Z")
  );
  expect(first).toEqual({ from: "2026-01-01T00:00:00Z", to: "2026-03-26T00:00:00Z" });
});

test("the items read spans the closes of the listed rows the board's map does not hold", () => {
  const rows = [
    { key: "ACME-1", closed_at: "2026-11-14T22:00:00Z" },
    { key: "ACME-2", closed_at: "2026-11-12T03:00:00Z" },
    { key: "ACME-3", closed_at: "2026-11-10T23:59:59Z" },
    { key: "ACME-4", closed_at: null },
  ];
  const answered = board({ "ACME-1": boardFacts("ACME-1", 1) });
  // Done's page: from the oldest close the map does not hold to the newest.
  expect(itemsWindows(rows, answered)).toEqual([
    { from: "2026-11-10T00:00:00Z", to: "2026-11-13T00:00:00Z" },
  ]);
  // A read of every done row: from the oldest such close to now.
  expect(itemsWindows(rows, answered, Date.parse("2026-11-17T08:00:00Z"))).toEqual([
    { from: "2026-11-10T00:00:00Z", to: "2026-11-18T00:00:00Z" },
  ]);
  expect(itemsWindows(rows, undefined)).toEqual([
    { from: "2026-11-10T00:00:00Z", to: "2026-11-15T00:00:00Z" },
  ]);
  // Where the map holds every closed row, nothing is read.
  expect(itemsWindows(rows.slice(0, 1), answered)).toEqual([]);
  expect(itemsWindows(rows.slice(3), answered)).toEqual([]);
});

test("the items read pages 200 rows at a time until the total, every window at once", async () => {
  const windows = closedDaysWindows(
    Date.parse("2026-01-01T00:00:00Z"),
    Date.parse("2026-04-01T00:00:00Z")
  );
  expect(windows).toHaveLength(2);
  const rowsOf = (from: string): EvalItemRow[] =>
    from === windows[0]?.from
      ? Array.from({ length: 450 }, (_, index) =>
          itemRow(`ACME-${index + 1}`, servedColumns(`ACME-${index + 1}`))
        )
      : [itemRow("ACME-999", servedColumns("ACME-999"))];
  const asked: Record<string, string>[] = [];
  answerRequests(
    http.get("/api/v1/projects/:key/evals/:slug/items", ({ request, params }) => {
      const query = new URL(request.url).searchParams;
      asked.push({
        key: String(params.key),
        slug: String(params.slug),
        ...Object.fromEntries(query),
      });
      const rows = rowsOf(query.get("from") ?? "");
      const offset = Number(query.get("offset"));
      const limit = Number(query.get("limit"));
      return Response.json({
        compare: null,
        items: rows.slice(offset, offset + limit),
        limit,
        offset,
        total: rows.length,
        window: { days: 84, from: query.get("from"), to: query.get("to") },
      });
    })
  );
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: queryRetry, retryDelay: queryRetryDelay } },
  });
  const facts = await queryClient.fetchQuery(legionItemsQuery("ACME", windows));
  const page = (
    window: (typeof windows)[number] | undefined,
    offset: number
  ): Record<string, string> => ({
    key: "ACME",
    slug: "legion-work-time",
    from: window?.from ?? "",
    to: window?.to ?? "",
    row_kind: "issue",
    limit: String(ITEMS_PAGE_SIZE),
    offset: String(offset),
  });
  // Each window is read page by page, in order, and the windows are read at once: the second
  // window's page is asked for before the first window's last.
  const askedOf = (window: (typeof windows)[number] | undefined) =>
    asked.filter((query) => query.from === window?.from);
  expect(askedOf(windows[0])).toEqual([
    page(windows[0], 0),
    page(windows[0], 200),
    page(windows[0], 400),
  ]);
  expect(askedOf(windows[1])).toEqual([page(windows[1], 0)]);
  expect(asked).toHaveLength(4);
  expect(asked.indexOf(askedOf(windows[1])[0] ?? {})).toBeLessThan(
    asked.indexOf(askedOf(windows[0])[2] ?? {})
  );
  expect(Object.keys(facts)).toHaveLength(451);
  expect(facts["ACME-450"]?.work_hours).toBe(3.75);
  expect(facts["ACME-999"]?.root_key).toBe("ACME-5");
});

test("an items page with no rows ends its window, whatever its total says", async () => {
  const asked: number[] = [];
  const facts = await readLegionItems(
    [{ from: "2026-11-01T00:00:00Z", to: "2026-11-02T00:00:00Z" }],
    async (query) => {
      asked.push(query.offset);
      return query.offset === 0
        ? { items: [itemRow("ACME-1", servedColumns("ACME-1"))], total: 5 }
        : { items: null, total: 5 };
    }
  );
  expect(asked).toEqual([0, 1]);
  expect(Object.keys(facts)).toEqual(["ACME-1"]);
});

test("a windowed Done read none of whose rows carries closed_at lists no row", () => {
  // A server older than `closed_at` writes none and ignores the window.
  const older: { key: string; closed_at?: string | null }[] = [
    { key: "INFRA-2" },
    { key: "INFRA-3" },
  ];
  const current = [
    { key: "INFRA-7", closed_at: "2026-11-14T10:00:00Z" },
    { key: "INFRA-3", closed_at: null },
  ];
  expect(listedDoneRows(older, true)).toEqual([]);
  // A read of every done issue with no window lists what it read.
  expect(listedDoneRows(older, false)).toBe(older);
  // One row that carries `closed_at` is a server that writes it.
  expect(listedDoneRows(current, true)).toBe(current);
});

test("a 404 EVAL_NOT_FOUND from the items route is a failed read, not an issue with no Legion work", async () => {
  let asked = 0;
  answerRequests(
    http.get("/api/v1/projects/:key/evals/:slug/items", () => {
      asked += 1;
      return Response.json(
        { code: "EVAL_NOT_FOUND", error: "no eval legion-work-time in ACME" },
        { status: 404 }
      );
    })
  );
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: queryRetry, retryDelay: queryRetryDelay } },
  });
  const observer = new QueryObserver(
    queryClient,
    legionItemsQuery("ACME", [{ from: "2026-11-01T00:00:00Z", to: "2026-11-02T00:00:00Z" }])
  );
  const stop = observer.subscribe(() => {});
  try {
    const result = await observer.refetch();
    expect(result.status).toBe("error");
    expect(result.data).toBeUndefined();
    expect(result.error).toBeInstanceOf(ApiError);
    expect(result.error instanceof ApiError ? result.error.code : undefined).toBe("EVAL_NOT_FOUND");
    // The read keeps the app's shared retry rule (`queryRetry`), which tries a refusal it does
    // not hold definitive three times, and only then answers the error.
    expect(asked).toBe(3);
  } finally {
    stop();
  }
});
