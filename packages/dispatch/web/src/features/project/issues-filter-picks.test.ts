import { expect, test } from "bun:test";

import type { Actor } from "../../api/types";
import { copyAddress } from "../../lib/page-address";
import {
  ASKS_PICKS,
  activityPick,
  asksPick,
  createdByReader,
  DURATION_PICKS,
  durationPick,
  LEGION_TREE_PICKS,
  legionPhasePicks,
  legionPick,
  phaseColumnPick,
  phaseHoursPicks,
  QUEUE_PICKS,
  REVIEW_ROUND_PICKS,
  ROLE_PICKS,
  readAsksPick,
  reviewRoundsPick,
  sinceDurationPick,
  WAITING_REASON_PICKS,
  WORKER_STATE_PICKS,
  waitingReasonPick,
  workerPicks,
} from "./issues-filter-picks";
import { LEGION_PHASES } from "./issues-legion";
import { expandView } from "./issues-views";

const at = (query: string) => copyAddress(query);

const HOUR = 3_600_000;
const DAY = 24 * HOUR;
const NOW = Date.parse("2026-11-17T12:00:00Z");

test("a duration's picks are under-1h, 1h-1d, 1d-7d, over-7d and none, each with its lower bound", () => {
  expect(DURATION_PICKS).toEqual(["under-1h", "1h-1d", "1d-7d", "over-7d", "none"]);
  expect(
    [0, HOUR - 1, HOUR, DAY - 1, DAY, 7 * DAY - 1, 7 * DAY, undefined].map(durationPick)
  ).toEqual(["under-1h", "under-1h", "1h-1d", "1h-1d", "1d-7d", "1d-7d", "over-7d", "none"]);
  expect(sinceDurationPick(new Date(NOW - 2 * HOUR).toISOString(), NOW)).toBe("1h-1d");
  expect(sinceDurationPick(new Date(NOW - 8 * DAY).toISOString(), NOW)).toBe("over-7d");
  expect(sinceDurationPick(null, NOW)).toBe("none");
  expect(sinceDurationPick(undefined, NOW)).toBe("none");
});

test("Review rounds' picks are 0, 1, 2, 3-or-more and none", () => {
  expect(REVIEW_ROUND_PICKS).toEqual(["0", "1", "2", "3-or-more", "none"]);
  expect([0, 1, 2, 3, 7, undefined].map(reviewRoundsPick)).toEqual([
    "0",
    "1",
    "2",
    "3-or-more",
    "3-or-more",
    "none",
  ]);
});

test("Workers' picks are the roles' ids and none", () => {
  expect(ROLE_PICKS).toEqual([
    "architect",
    "planner",
    "implementer",
    "tester",
    "reviewer",
    "merger",
    "none",
  ]);
  expect(workerPicks(["implementer", "tester"])).toEqual(["implementer", "tester"]);
  expect(workerPicks([])).toEqual(["none"]);
  expect(workerPicks(null)).toEqual(["none"]);
});

test("Why it waits' picks are ready, slot, blocked, person and none", () => {
  expect(WAITING_REASON_PICKS).toEqual(["ready", "slot", "blocked", "person", "none"]);
  const blocked = { blocked_by: ["CORE-2"], reason: "blocked", since: "2026-11-17T00:00:00Z" };
  expect(waitingReasonPick(blocked)).toBe("blocked");
  expect(waitingReasonPick({ reason: "ready" })).toBe("ready");
  expect(waitingReasonPick(null)).toBe("none");
  expect(waitingReasonPick({ reason: "" })).toBe("none");
});

test("Worker state's picks are the board's slot uses and none", () => {
  expect(WORKER_STATE_PICKS).toEqual([
    "working",
    "between_turns",
    "held_children",
    "held_person",
    "held_idle",
    "none",
  ]);
  expect(activityPick("between_turns")).toBe("between_turns");
  expect(activityPick(null)).toBe("none");
});

test("Legion tree's picks are root, child and none, and two of them select their union", () => {
  expect(LEGION_TREE_PICKS).toEqual(["root", "child", "none"]);
  expect(legionPick({ tree_depth: 0 })).toBe("root");
  expect(legionPick({ tree_depth: 2 })).toBe("child");
  expect(legionPick(undefined)).toBe("none");
  const picked = expandView(at("view=done&f.legion=root&f.legion=child")).getAll("f.legion");
  expect(picked).toEqual(["root", "child"]);
  const rows = [{ tree_depth: 0 }, { tree_depth: 1 }, undefined];
  expect(rows.filter((facts) => picked.includes(legionPick(facts)))).toEqual([
    { tree_depth: 0 },
    { tree_depth: 1 },
  ]);
});

test("Legion phase's picks are the board's column ids in its order, Retro between Accepting and Merging, and None last", () => {
  expect(legionPhasePicks(LEGION_PHASES)).toEqual([
    "queue",
    "design",
    "planning",
    "implementing",
    "testing",
    "reviewing",
    "accepting",
    "retro",
    "merging",
    "awaiting_children",
    "awaiting_person",
    "held",
    "none",
  ]);
  expect(phaseColumnPick("implementing")).toBe("implementing");
  expect(phaseColumnPick(undefined)).toBe("none");
});

test("Open asks' picks are any and none: f.asks=0 reads as none and f.asks=3 as any", () => {
  expect(ASKS_PICKS).toEqual(["any", "none"]);
  expect(asksPick(0)).toBe("none");
  expect(asksPick(2)).toBe("any");
  expect(readAsksPick("0")).toBe("none");
  expect(readAsksPick("3")).toBe("any");
  expect(readAsksPick("1")).toBe("any");
  expect(readAsksPick("any")).toBe("any");
  expect(readAsksPick("none")).toBe("none");
});

test("f.phase_hours picks the phases with hours, several picking the rows with hours in any", () => {
  expect(phaseHoursPicks({ implementing: 2.5, testing: 0, other: 1 })).toEqual([
    "implementing",
    "other",
  ]);
  expect(phaseHoursPicks(null)).toEqual([]);
  const picked = expandView(at("view=done&f.phase_hours=testing&f.phase_hours=other")).getAll(
    "f.phase_hours"
  );
  expect(
    phaseHoursPicks({ implementing: 2.5, other: 1 }).some((phase) => picked.includes(phase))
  ).toBe(true);
  expect(phaseHoursPicks({ implementing: 2.5 }).some((phase) => picked.includes(phase))).toBe(
    false
  );
});

test("f.phase_hours offers only the phases Time per phase draws and other: a key it does not draw is other", () => {
  expect(phaseHoursPicks({ x_phase: 2 })).toEqual(["other"]);
  expect(phaseHoursPicks({ implementing: 1, x_phase: 2, other: 1 })).toEqual([
    "implementing",
    "other",
  ]);
  const picked = expandView(at("view=done&f.phase_hours=other")).getAll("f.phase_hours");
  expect(phaseHoursPicks({ x_phase: 2 }).some((phase) => picked.includes(phase))).toBe(true);
});

test("the Queue filter keeps its address values Queued and Not queued", () => {
  expect(QUEUE_PICKS).toEqual(["Queued", "Not queued"]);
});

test("f.author=@me matches the reader's own issues and those of an agent session the reader owns", () => {
  const own: Actor = { id: "Alice", kind: "user" };
  const ownSession: Actor = { id: "session-1", kind: "session", owner: "alice" };
  const othersSession: Actor = { id: "session-2", kind: "session", owner: "bob" };
  const unowned: Actor = { id: "session-3", kind: "session" };
  const other: Actor = { id: "bob", kind: "user" };
  expect(createdByReader(own, "alice")).toBe(true);
  expect(createdByReader(ownSession, "alice")).toBe(true);
  expect(createdByReader(othersSession, "alice")).toBe(false);
  expect(createdByReader(unowned, "alice")).toBe(false);
  expect(createdByReader(other, "alice")).toBe(false);
  expect(createdByReader(null, "alice")).toBe(false);
  // A saved link with @me means the reader who opens it: with nobody signed in, nobody.
  expect(createdByReader(own, undefined)).toBe(false);
  // The address keeps the pick as it is.
  expect(expandView(at("f.author=%40me")).getAll("f.author")).toEqual(["@me"]);
});
