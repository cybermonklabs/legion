import { expect, test } from "bun:test";
import blockAgentStatus from "../fixtures/going/block_agent_status.json";
import blockChart from "../fixtures/going/block_chart.json";
import blockIssueList from "../fixtures/going/block_issue_list.json";
import blockMetric from "../fixtures/going/block_metric.json";
import blockProgress from "../fixtures/going/block_progress.json";
import blockTable from "../fixtures/going/block_table.json";
import blockTimeline from "../fixtures/going/block_timeline.json";
import decision from "../fixtures/going/decision.json";
import roster from "../fixtures/going/roster.json";
import type {
  GoingBlockData,
  GoingBlockKind,
  GoingBlockSource,
  GoingChartSpec,
  GoingDecidedContext,
  GoingDecisionEvent,
  GoingLine,
  GoingMetricSpec,
  GoingProgressSpec,
  GoingRosterEvent,
  GoingTableSpec,
} from "./going";

// The frame data in fixtures/going is written by the Dispatch server's own Go types
// (TestTheGoingFrameFixturesAreWhatTheServerWrites, packages/envoy/internal/dispatch/api), so each
// is what a run's stream carries. These tests hold it to the hand-written frames of going.ts key
// for key: each list below must name every key or member of its type, or the type check fails,
// so a key the frames gain or lose on either side fails here until the other side matches it.

/** `list`, which must hold every member of U. */
function allOf<U extends string>() {
  return <const L extends readonly U[]>(
    list: L & ([Exclude<U, L[number]>] extends [never] ? unknown : never)
  ): L => list;
}

type Decision = GoingDecisionEvent["data"];
type Roster = GoingRosterEvent["data"];

const DECISION_KEYS = allOf<keyof Decision>()(["action", "line", "context"]);
const LINES = allOf<GoingLine>()(["ask", "do", "file"]);
const CONTEXTS = allOf<GoingDecidedContext>()(["page", "named", "none"]);
const BLOCK_KEYS = allOf<keyof GoingBlockData>()(["kind", "title", "spec", "rows"]);
const SOURCE_KEYS = allOf<keyof GoingBlockSource>()(["source", "params", "table"]);
/** Every kind's spec, by kind: a kind without a row, or a row missing a key of its spec's type or
 *  naming one it lacks, fails the type check. */
const SPEC_KEYS = {
  agent_status: SOURCE_KEYS,
  chart: allOf<keyof GoingChartSpec>()(["source", "params", "table", "x", "y", "mark"]),
  issue_list: SOURCE_KEYS,
  metric: allOf<keyof GoingMetricSpec>()([
    "source",
    "params",
    "table",
    "field",
    "label",
    "compare",
    "compare_label",
  ]),
  progress: allOf<keyof GoingProgressSpec>()([
    "source",
    "params",
    "table",
    "label",
    "done",
    "total",
  ]),
  table: allOf<keyof GoingTableSpec>()(["source", "params", "table", "columns"]),
  timeline: SOURCE_KEYS,
} satisfies Record<GoingBlockKind, readonly string[]>;
const METRIC_OPTIONAL = ["compare", "compare_label"] as const;
const MARKS = allOf<GoingChartSpec["mark"]>()(["bar", "line"]);
/** The fields the rows of a kind with a fixed table carry, as going.ts's GoingBlockData says. */
const FIXED_ROW_KEYS: Partial<Record<GoingBlockKind, readonly string[]>> = {
  agent_status: ["session_id", "title", "state", "open_asks", "last_activity"],
  issue_list: ["key", "title", "status"],
  timeline: ["at", "lane", "label"],
};
const ROSTER_KEYS = allOf<keyof Roster>()(["age_ms", "source"]);
const ROSTER_SOURCES = allOf<Roster["source"]>()(["live", "stored"]);

/** The fixture of each kind of block. */
const BLOCKS: Record<GoingBlockKind, Readonly<Record<string, unknown>>> = {
  agent_status: blockAgentStatus,
  chart: blockChart,
  issue_list: blockIssueList,
  metric: blockMetric,
  progress: blockProgress,
  table: blockTable,
  timeline: blockTimeline,
};

const sorted = (keys: readonly string[]) => [...keys].sort();
const keysOf = (value: object) => sorted(Object.keys(value));

/** `value` carries every key of `all` but the `optional` ones, and none outside `all`. */
function expectKeys(value: object, all: readonly string[], optional: readonly string[] = []) {
  const keys = keysOf(value);
  for (const key of keys) expect(all).toContain(key);
  for (const key of all) if (!optional.includes(key)) expect(keys).toContain(key);
}

test("a decision frame carries the action chosen, the line on top it gives and what the words are about", () => {
  expect(keysOf(decision)).toEqual(sorted(DECISION_KEYS));
  expect(typeof decision.action).toBe("string");
  expect(LINES).toContain(decision.line as GoingLine);
  expect(CONTEXTS).toContain(decision.context as GoingDecidedContext);
});

test("a block frame of each kind carries its title, the spec its kind takes and rows of the fields it draws", () => {
  for (const [kind, block] of Object.entries(BLOCKS) as [
    GoingBlockKind,
    Record<string, unknown>,
  ][]) {
    expect(keysOf(block)).toEqual(sorted(BLOCK_KEYS));
    expect(block.kind).toBe(kind);
    const spec = block.spec as Readonly<Record<string, unknown>>;
    expectKeys(spec, SPEC_KEYS[kind], kind === "metric" ? METRIC_OPTIONAL : []);
    if (kind === "chart") expect(MARKS).toContain(spec.mark as GoingChartSpec["mark"]);
    const rows = block.rows as readonly Readonly<Record<string, unknown>>[];
    expect(rows.length).toBeGreaterThan(0);
    const fixed = FIXED_ROW_KEYS[kind];
    if (fixed !== undefined) for (const row of rows) expect(keysOf(row)).toEqual(sorted(fixed));
  }
});

test("a roster frame says how old the roster was and where it came from", () => {
  expect(keysOf(roster)).toEqual(sorted(ROSTER_KEYS));
  expect(typeof roster.age_ms).toBe("number");
  expect(ROSTER_SOURCES).toContain(roster.source as Roster["source"]);
});
