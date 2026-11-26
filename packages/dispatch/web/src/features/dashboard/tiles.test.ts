import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";

import type {
  CheckBlock,
  CheckResult,
  IssueAskRow,
  IssueCheckResults,
  IssueCoverage,
  IssueDetails,
} from "../../api/types";
import { pageSegmentBg, pageSegmentDoneBg, pageSegmentProgressBg } from "../../theme/classes";
import { checkStanding } from "../issue-page/statement-rules";
import { tileViews } from "./ProgressTiles";
import {
  childrenTile,
  metTile,
  passedTile,
  questionsTile,
  TILE_LIST_STEP,
  type TileIssue,
  tileList,
} from "./tiles";

// The reads of one Dashboard seed (`e2e/dashboard-seed.ts`'s `seedDashboard`), as the server at
// this branch's head answered them to the reader (`fixtures/README.md`): four statements, r1 and
// r4 met; five checks, c1, c3 and c5 passed; seven children, three done; six open asks, two of
// them not the reader's turn.
const READER = "alice";
const FIXTURES = join(import.meta.dir, "fixtures");

/** A captured body, read whole as the server wrote it. */
function captured<Body>(name: string): Body {
  return JSON.parse(readFileSync(join(FIXTURES, name), "utf8"));
}

const ISSUE: IssueDetails = captured("issue.json");
const COVERAGE: IssueCoverage = captured("coverage.json");
const RESULTS: IssueCheckResults = captured("results.json");
const ASKS: readonly IssueAskRow[] = captured("asks-open.json");

function checkOf(id: string): CheckBlock {
  const found = (RESULTS.checks ?? []).find((check) => check.block_id === id);
  if (found === undefined) throw new Error(`the captured results hold no check ${id}`);
  return found;
}

function resultOf(check: CheckBlock): CheckResult {
  const [result] = check.results ?? [];
  if (result === undefined) throw new Error(`the captured check ${check.block_id} has no result`);
  return result;
}

describe("the four tiles over the captured reads", () => {
  test("Statements met counts each statement every check of whose serves reads Passed, of all statements", () => {
    const tile = metTile(ISSUE, COVERAGE, RESULTS);
    expect(tile.counted.map((statement) => statement.block_id)).toEqual(["r1", "r4"]);
    expect(tile.whole).toBe(4);
  });

  test("Checks passed counts each check checkStanding reads Passed, of all checks", () => {
    const tile = passedTile(RESULTS);
    expect(tile.counted.map((check) => check.block_id)).toEqual(["c1", "c3", "c5"]);
    expect(tile.whole).toBe(5);
  });

  test("Children done counts the done children of all, with the count of each status for the bar", () => {
    const tile = childrenTile(ISSUE);
    expect(tile.counted.map((child) => child.status)).toEqual(["done", "done", "done"]);
    expect(tile.whole).toBe(7);
    // The board's order of the statuses, each with its children.
    expect(tile.statuses).toEqual([
      { count: 1, status: "backlog" },
      { count: 1, status: "todo" },
      { count: 1, status: "in_progress" },
      { count: 1, status: "needs_review" },
      { count: 3, status: "done" },
    ]);
  });

  test("Questions waiting on others counts the open questions whose turn is not the reader's, of all open questions", () => {
    const tile = questionsTile(ASKS, READER);
    expect(tile.counted.map((ask) => ask.question)).toEqual([
      "Which reads does the first load send?",
      "Who reviews the tile words?",
    ]);
    expect(tile.whole).toBe(6);
    // While the reader's login has not resolved, no question is the reader's.
    expect(questionsTile(ASKS, undefined).counted).toHaveLength(6);
  });
});

// The children's bar draws a child whose work moves - started, and not done - in the progress
// colour, by the statuses the page reads as moving (`issue-page/issue-status.ts`): one that waits
// for review among them.
test("the children's bar draws a child in review in the progress colour", () => {
  const children = tileViews({
    asks: ASKS,
    coverage: COVERAGE,
    issue: ISSUE,
    results: RESULTS,
    viewer: READER,
  }).find((tile) => tile.key === "children");
  // backlog, todo, in_progress, needs_review, done: the order of `childrenTile`'s statuses.
  expect(children?.bar).toEqual([
    { className: pageSegmentBg, count: 1 },
    { className: pageSegmentBg, count: 1 },
    { className: pageSegmentProgressBg, count: 1 },
    { className: pageSegmentProgressBg, count: 1 },
    { className: pageSegmentDoneBg, count: 3 },
  ]);
});

test("the passed tile counts what checkStanding reads Passed", () => {
  const c1 = checkOf("c1");
  const pass = resultOf(c1);
  // A pass at the check's basis, and a newer fail at another commit: the fail says nothing
  // about the basis, so the check reads Passed, and the tile counts it.
  const failElsewhere: CheckResult = {
    ...pass,
    commit_sha: "e".repeat(40),
    commit_stands: { answer: "other_commit" },
    created_at: "2026-11-25T22:30:00.000001Z",
    id: "6f7e8d9c-0b1a-4c2d-9e8f-7a6b5c4d3e2f",
    verdict: "fail",
  };
  const passedHere: CheckBlock = { ...c1, results: [failElsewhere, pass] };
  expect(checkStanding(passedHere).state).toBe("passed");
  expect(passedTile({ ...RESULTS, checks: [passedHere] }).counted).toEqual([passedHere]);
  // A newer fail at the basis is the check's state, whatever pass the server still counts.
  const failHere: CheckBlock = {
    ...c1,
    results: [{ ...failElsewhere, commit_stands: pass.commit_stands }, pass],
  };
  expect(checkStanding(failHere).state).toBe("failed");
  expect(passedTile({ ...RESULTS, checks: [failHere] }).counted).toEqual([]);
});

test(`a tile's list draws ${TILE_LIST_STEP} entries at a time and says how many are left`, () => {
  const entries = Array.from({ length: 45 }, (_, index) => index);
  expect(tileList(entries, 1)).toEqual({ left: 25, shown: entries.slice(0, 20) });
  expect(tileList(entries, 2)).toEqual({ left: 5, shown: entries.slice(0, 40) });
  expect(tileList(entries, 3)).toEqual({ left: 0, shown: entries });
  expect(tileList(entries.slice(0, 7), 1)).toEqual({ left: 0, shown: entries.slice(0, 7) });
});

/** Each issue of `fixtures/serves-forms.txt` with the references its Verification document's
 *  `serves` attributes write, in the order written. */
function servesForms(): ReadonlyMap<string, readonly string[]> {
  const forms = new Map<string, string[]>();
  let references: string[] = [];
  for (const line of readFileSync(join(FIXTURES, "serves-forms.txt"), "utf8").split("\n")) {
    const issue = /^# (\S+)$/.exec(line)?.[1];
    if (issue !== undefined) {
      references = [];
      forms.set(issue, references);
      continue;
    }
    const written = /^serves="([^"]*)"$/.exec(line)?.[1];
    if (written !== undefined) references.push(...written.split(/\s+/).filter(Boolean));
  }
  return forms;
}

test("a reference to an ancestor's statement, in the forms real Verification documents write, serves no statement of the issue", () => {
  const forms = servesForms();
  expect([...forms.keys()]).toEqual(["INFRA-5702", "INFRA-1196", "INFRA-4097", "INFRA-1150"]);
  const pass = resultOf(checkOf("c1"));
  for (const [key, references] of forms) {
    const own = references.filter((reference) => reference.startsWith(`dispatch://${key}/spec#b-`));
    const others = references.filter((reference) => !own.includes(reference));
    // The issue whose spec the references name, under the captured issue's spec.
    const issue: TileIssue = { ...ISSUE, key };
    const statements = [...new Set(own.map((reference) => reference.split("#b-")[1] ?? ""))];
    const coverage: IssueCoverage = {
      ...COVERAGE,
      issue: key,
      requirements: statements.map((block_id) => ({
        block_id,
        checks: 1,
        gaps: [],
        plan_tasks: 0,
        ref: `dispatch://${key}/spec#b-${block_id}`,
        text: block_id,
      })),
    };
    /** One passed check for each reference of `serving`, serving it alone. */
    const checksServing = (serving: readonly string[]): IssueCheckResults => ({
      ...RESULTS,
      checks: serving.map((reference, index) => ({
        ...checkOf("c1"),
        block_id: `c${index}`,
        results: [pass],
        serves: [reference],
      })),
    });
    // Every reference of these documents is a full address of a spec's statement.
    expect(
      references.every((reference) => /^dispatch:\/\/[A-Z]+-\d+\/spec#b-/.test(reference))
    ).toBe(true);
    expect(own.length).toBeGreaterThan(0);
    // The ancestor's references, each passed, meet none of the issue's statements.
    expect(metTile(issue, coverage, checksServing(others)).counted).toEqual([]);
    // The issue's own, each passed, meet every one.
    expect(
      metTile(issue, coverage, checksServing(own)).counted.map((statement) => statement.block_id)
    ).toEqual(statements);
  }
  // The documents serve an ancestor's statements beside their own, so the case reads both forms.
  expect(
    [...forms].filter(([key, references]) =>
      references.some((reference) => !reference.startsWith(`dispatch://${key}/`))
    ).length
  ).toBeGreaterThan(0);
});
