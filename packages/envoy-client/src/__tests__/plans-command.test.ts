import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import {
  type FinishedIssue,
  LEGION_PLAN_RATING_LEAST,
  LEGION_PLAN_RATING_MOST,
  type PlanRating,
  type PlanRecord,
  PROJECT_KEY_RULE,
  type ProjectPlans,
} from "@legion/contracts";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import {
  type FakeDispatchServer,
  fakeDispatchFetch,
  startFakeDispatch,
} from "../../test/fake-dispatch";
import { bearerCredential } from "../bearer";
import { runDispatchCli } from "../dispatch-cli";
import { executeDispatchTool } from "../dispatch-execute";

// `dispatch plans --project <KEY>`: each finished issue of a project, as Dispatch's
// `GET /api/v1/projects/{key}/plans` answers them, one header line and one line per issue: its
// plan, each rating its retros gave the plan with the reason, its time and its count of failed
// steps, and, where the records of the issue's newest run name an experiment,
// `variant <experiment>/<variant>` at its end. The Legion whole-daemon driver parses the line, so
// each case asserts it whole. The format cases drive `executeDispatchTool` over the shared fake
// Dispatch (`test/fake-dispatch.ts`) as a fetch, so each reads the text and the details a host is
// handed; the command-line cases drive the real CLI against that fake on a loopback port, and read
// its exit code and output.

/** The Dispatch bearer each command-line case is handed. */
const TOKEN = fixtureSecret("dsp");

/** The plan node `id` Legion writes for `issue` when it runs the issue on its default steps; each
 *  issue's node is its own, under an id of its own. */
function defaultPlan(issue: string, id: string): PlanRecord {
  return {
    id,
    issue,
    generation: 1,
    source: "default",
    artifact_id: null,
    version: null,
    tasks: [
      {
        id: "default",
        steps: ["planner", "implementer", "tester", "reviewer", "acceptance:owner"],
      },
    ],
  };
}

/** A finished issue Legion ran on its default plan, rated once: an hour and a half, one failed
 *  review. */
const ratedOnce: FinishedIssue = {
  issue: "INFRA-12",
  title: "Retry failed webhook deliveries",
  closed_at: "2026-11-16T12:00:00Z",
  plan: defaultPlan("INFRA-12", "6b1f0c3e-2d4a-5e8b-9c7d-1a2b3c4d5e6f"),
  ratings: [
    {
      rating: 4,
      reason: "The plan fit the work; the review sent it back once.",
      step_id: "8d2e4f6a-1b3c-5d7e-9f0a-2b4c6d8e0f1a",
      ended_at: "2026-11-16T11:50:00Z",
    },
  ],
  started_at: "2026-11-16T10:30:00Z",
  ended_at: "2026-11-16T12:00:00Z",
  duration_seconds: 5400,
  failed_steps: 1,
  experiment: null,
  variant: null,
};

/** INFRA-11's plan: its owner's, in three tasks. The first task is neither the first nor the last
 *  by id, and each task has its own steps, so the line names the first task of the plan and no
 *  other. */
const ownerPlan: PlanRecord = {
  id: "3c5e7a9b-4d6f-5a8c-8e0b-2d4f6a8c0e1b",
  issue: "INFRA-11",
  generation: 2,
  source: "owner",
  artifact_id: "9a8b7c6d-5e4f-4a3b-8c2d-1e0f9a8b7c6d",
  version: 3,
  tasks: [
    { id: "server", steps: ["implementer", "reviewer"] },
    { id: "api", steps: ["implementer:api"] },
    { id: "web", steps: ["implementer:web", "tester"] },
  ],
};

/** A finished issue its owner planned in three tasks, which no retro rated. */
const unrated: FinishedIssue = {
  issue: "INFRA-11",
  title: "Document the retry",
  closed_at: "2026-11-15T08:00:00Z",
  plan: ownerPlan,
  ratings: [],
  started_at: "2026-11-15T06:55:00Z",
  ended_at: "2026-11-15T07:57:05Z",
  duration_seconds: 3725,
  failed_steps: 0,
  experiment: null,
  variant: null,
};

/** Three retros, so three ratings, in the order the retros ran: neither by score (4, 2, 5) nor by
 *  step id (9e…, 1a…, 5c…), either way up, and the two sorts disagree, so a list sorted on either
 *  reads differently. One reason holds a quotation mark and a line break. The issue's time runs
 *  from its first start to the end of its last retro: five hours and eight minutes. */
const ratedThrice: FinishedIssue = {
  ...ratedOnce,
  issue: "INFRA-13",
  plan: defaultPlan("INFRA-13", "7c2a1d4f-3e5b-5f9c-8d1e-2b3c4d5e6f70"),
  closed_at: "2026-11-17T09:00:00Z",
  ratings: [
    {
      rating: 4,
      reason: "The plan fit the first merge.",
      step_id: "9e0f1a2b-3c4d-5e6f-8a7b-9c0d1e2f3a4b",
      ended_at: "2026-11-17T05:10:00Z",
    },
    {
      rating: 2,
      reason: "The plan had no test step.",
      step_id: "1a2b3c4d-5e6f-5a7b-8c9d-0e1f2a3b4c5d",
      ended_at: "2026-11-17T07:00:00Z",
    },
    {
      rating: 5,
      reason: 'It said "ship it"\nand it shipped.',
      step_id: "5c6d7e8f-9a0b-5c1d-8e2f-3a4b5c6d7e8f",
      ended_at: "2026-11-17T08:55:00Z",
    },
  ],
  started_at: "2026-11-17T03:47:00Z",
  ended_at: "2026-11-17T08:55:00Z",
  duration_seconds: 18480,
  failed_steps: 0,
};

/** A finished issue Legion never ran: no plan, no rating, no time. */
const neverRan: FinishedIssue = {
  issue: "INFRA-9",
  title: "Closed by hand",
  closed_at: "2026-11-14T08:00:00Z",
  plan: null,
  ratings: [],
  started_at: null,
  ended_at: null,
  duration_seconds: null,
  failed_steps: 0,
  experiment: null,
  variant: null,
};

/** A finished issue whose time Dispatch cannot count: a plan and a rating, a start and an end,
 *  and no time, since its latest end is before its earliest start. An older run, whose starts
 *  Dispatch never got, ended and was rated; the newer run, whose plan the line names, started
 *  after it and never ended. A new run is a new generation, so that plan is generation 2's. */
const uncountedTime: FinishedIssue = {
  ...ratedOnce,
  issue: "INFRA-10",
  plan: { ...defaultPlan("INFRA-10", "8d3b2e5a-4f6c-5a0d-9e2f-3c4d5e6f7a81"), generation: 2 },
  closed_at: "2026-11-14T09:00:00Z",
  ratings: [
    {
      rating: 3,
      reason: "Enough.",
      step_id: "4d5e6f7a-8b9c-5d0e-8f1a-3b4c5d6e7f80",
      ended_at: "2026-11-14T08:50:00Z",
    },
  ],
  started_at: "2026-11-14T08:58:00Z",
  ended_at: "2026-11-14T08:55:00Z",
  duration_seconds: null,
  failed_steps: 2,
};

/** A finished issue of a second project, ACME: its own key, plan, rating, time and count, so a
 *  read of the wrong project prints none of it. */
const otherProject: FinishedIssue = {
  issue: "ACME-3",
  title: "Rotate the signing key",
  closed_at: "2026-11-13T17:00:00Z",
  plan: {
    id: "2b4d6f8a-0c1e-5a3b-9d5f-7a9b1c3d5e7f",
    issue: "ACME-3",
    generation: 1,
    source: "owner",
    artifact_id: "5e6f7a8b-9c0d-4e1f-8a2b-3c4d5e6f7a8b",
    version: 1,
    tasks: [{ id: "keys", steps: ["implementer", "tester"] }],
  },
  ratings: [
    {
      rating: 2,
      reason: "The plan left out the rollback.",
      step_id: "6c7d8e9f-0a1b-5c2d-8e3f-4a5b6c7d8e9f",
      ended_at: "2026-11-13T16:40:00Z",
    },
  ],
  started_at: "2026-11-13T14:05:00Z",
  ended_at: "2026-11-13T16:40:00Z",
  duration_seconds: 9300,
  failed_steps: 3,
  experiment: null,
  variant: null,
};

const config = {
  enabled: true,
  url: "http://dispatch.test",
  credential: bearerCredential("secret"),
  error: null,
};

/** The repository's root, which the Dispatch docs pages sit under. */
const REPO_ROOT = join(import.meta.dir, "..", "..", "..", "..");

/** The route of the plans read of `project`. */
function plansPath(project: string): string {
  return `/api/v1/projects/${project}/plans`;
}

/** `dispatch_plans` with `args` over a fake Dispatch whose plans route of INFRA answers `answer`:
 *  the result, and each request the fake saw as `<method> <pathname><search>`. */
async function readPlans(args: Record<string, unknown>, answer: ProjectPlans) {
  const dispatch = fakeDispatchFetch({ routes: { [plansPath("INFRA")]: answer } });
  const result = await executeDispatchTool({
    tool: "dispatch_plans",
    args,
    cwd: "/workspace",
    host: "omp",
    sessionId: "session-1",
    config,
    env: {},
    exec: async (file, argv) => {
      throw new Error(`unexpected command: ${file} ${argv.join(" ")}`);
    },
    fetchImpl: dispatch.fetchImpl,
  });
  return {
    result,
    requests: dispatch.calls.map((call) => `${call.method} ${call.url.pathname}${call.url.search}`),
  };
}

/** The header `dispatch plans` prints with no `since`: how many issues it lists, of the finished
 *  issues that have a plan or a rating, and how many finished in all. */
function defaultHeader(project: string, listed: number, planned: number, finished: number): string {
  return `${project}: ${listed} of ${planned} finished issues with a plan or a rating; ${finished} finished in all; --since <RFC 3339 time> prints every finished issue closed since then`;
}

/** The query `dispatch plans` sends with no `since`: the issues with a plan or a rating, at most
 *  the 20 it prints. */
const DEFAULT_QUERY = "planned_or_rated=true&limit=20";

/** `count` finished issues, newest close first, each a copy of `issue` under a key of its own, the
 *  first `INFRA-<first>` and each next one a key lower and an hour older. */
function finishedRun(issue: FinishedIssue, first: number, count: number): FinishedIssue[] {
  return Array.from({ length: count }, (_, place) => ({
    ...issue,
    issue: `INFRA-${first - place}`,
    closed_at: new Date(Date.parse("2026-11-20T12:00:00Z") - place * 3_600_000)
      .toISOString()
      .replace(".000Z", "Z"),
  }));
}

describe("dispatch plans", () => {
  // An owner reads the earlier plans to learn from plans, and a busy project closes hundreds of
  // issues a day that Legion never ran, each `plan none ratings none`. So with no `since` the
  // command asks Dispatch for only the newest PLANS_DEFAULT_COUNT finished issues that have a plan
  // or a rating, and Dispatch counts the rest: the header's two counts are Dispatch's, which here
  // differ from every count of the issues answered.
  test("with no since, it asks for the 20 newest finished issues with a plan or a rating, and prints Dispatch's counts", async () => {
    const planned = finishedRun(ratedOnce, 125, 20);
    const answer: ProjectPlans = {
      project: "INFRA",
      issues: planned,
      total: 25,
      finished_total: 3567,
    };

    const { result, requests } = await readPlans({ project: "INFRA" }, answer);

    expect(requests).toEqual([`GET /api/v1/projects/INFRA/plans?${DEFAULT_QUERY}`]);
    const lines = result.text.split("\n");
    expect(lines[0]).toBe(defaultHeader("INFRA", 20, 25, 3567));
    expect(lines.slice(1).map((line) => line.split(" ")[0])).toEqual(
      planned.map((issue) => issue.issue)
    );
    expect(result.details).toEqual({ project: "INFRA", issues: planned });
  });

  // A Dispatch built before planned_or_rated and limit leaves both out and answers every finished
  // issue with no count, so the command keeps and counts the issues with a plan or a rating itself
  // and prints what it printed before Dispatch took the two.
  test("a Dispatch that answers every finished issue and no count still prints the 20 newest with a plan or a rating, and counts them", async () => {
    const planned = finishedRun(ratedOnce, 125, 25);
    const unplanned = finishedRun(neverRan, 99, 10);
    // Newest close first, as Dispatch answers: the issues Legion never ran stand among the others.
    const issues = [
      planned[0],
      ...unplanned.slice(0, 5),
      ...planned.slice(1),
      ...unplanned.slice(5),
    ];
    const answer: ProjectPlans = { project: "INFRA", issues };

    const { result, requests } = await readPlans({ project: "INFRA" }, answer);

    expect(requests).toEqual([`GET /api/v1/projects/INFRA/plans?${DEFAULT_QUERY}`]);
    const lines = result.text.split("\n");
    expect(lines[0]).toBe(defaultHeader("INFRA", 20, 25, 35));
    expect(lines.slice(1).map((line) => line.split(" ")[0])).toEqual(
      planned.slice(0, 20).map((issue) => issue.issue)
    );
    expect(result.details).toEqual({ project: "INFRA", issues: planned.slice(0, 20) });
  });

  test("an explicit since prints every finished issue of its window, those with no plan too, under the plain header", async () => {
    const issues = [...finishedRun(ratedOnce, 125, 25), neverRan];
    const answer: ProjectPlans = { project: "INFRA", issues };

    const { result, requests } = await readPlans(
      { project: "INFRA", since: "2026-11-01T00:00:00Z" },
      answer
    );

    expect(requests).toEqual(["GET /api/v1/projects/INFRA/plans?since=2026-11-01T00%3A00%3A00Z"]);
    const lines = result.text.split("\n");
    expect(lines[0]).toBe("INFRA: 26 finished issues");
    expect(lines).toHaveLength(27);
    expect(lines[26]).toBe(
      "INFRA-9 closed 2026-11-14T08:00:00Z plan none ratings none time - failed steps 0"
    );
    expect(result.details).toEqual({ project: "INFRA", issues });
  });

  test("prints a header, then one line per finished issue, a rated one and an unrated one", async () => {
    const answer: ProjectPlans = { project: "INFRA", issues: [ratedOnce, unrated] };

    const { result, requests } = await readPlans({ project: "INFRA" }, answer);

    expect(requests).toEqual([`GET /api/v1/projects/INFRA/plans?${DEFAULT_QUERY}`]);
    expect(result.text).toBe(
      [
        defaultHeader("INFRA", 2, 2, 2),
        'INFRA-12 closed 2026-11-16T12:00:00Z plan default (1 tasks: planner implementer tester reviewer acceptance:owner) ratings 4/5 "The plan fit the work; the review sent it back once." time 1h30m failed steps 1',
        "INFRA-11 closed 2026-11-15T08:00:00Z plan owner (3 tasks: implementer reviewer) ratings none time 1h2m failed steps 0",
      ].join("\n")
    );
    expect(result.details).toEqual({ project: "INFRA", issues: [ratedOnce, unrated] });
  });

  // An issue a draw put in an experiment names the experiment and the variant its newest run's
  // records name, at the end of its line; an issue that names none prints nothing more.
  test("an issue in an experiment prints its variant on its line, and another prints none", async () => {
    const drawn: FinishedIssue = { ...ratedOnce, experiment: "planning", variant: "b" };
    const answer: ProjectPlans = { project: "INFRA", issues: [drawn, unrated] };

    const { result } = await readPlans({ project: "INFRA" }, answer);

    expect(result.text.split("\n")).toEqual([
      defaultHeader("INFRA", 2, 2, 2),
      'INFRA-12 closed 2026-11-16T12:00:00Z plan default (1 tasks: planner implementer tester reviewer acceptance:owner) ratings 4/5 "The plan fit the work; the review sent it back once." time 1h30m failed steps 1 variant planning/b',
      "INFRA-11 closed 2026-11-15T08:00:00Z plan owner (3 tasks: implementer reviewer) ratings none time 1h2m failed steps 0",
    ]);
  });

  test("an issue with three ratings lists each in the order the retros ran, each reason one JSON string", async () => {
    const answer: ProjectPlans = { project: "INFRA", issues: [ratedThrice] };

    const { result } = await readPlans({ project: "INFRA" }, answer);

    expect(result.text.split("\n")).toEqual([
      defaultHeader("INFRA", 1, 1, 1),
      'INFRA-13 closed 2026-11-17T09:00:00Z plan default (1 tasks: planner implementer tester reviewer acceptance:owner) ratings 4/5 "The plan fit the first merge.", 2/5 "The plan had no test step.", 5/5 "It said \\"ship it\\"\\nand it shipped." time 5h8m failed steps 0',
    ]);
  });

  // Each rating prints out of the most of the contract's scale. The ratings here are the scale's
  // two ends and the one above its least, in an order no sort of their scores or step ids gives, so
  // a line that printed a rating out of another number, or out of itself, reads differently.
  test("each rating prints out of the most of the contract's scale", async () => {
    const [least, most] = [LEGION_PLAN_RATING_LEAST, LEGION_PLAN_RATING_MOST];
    const answer: ProjectPlans = {
      project: "INFRA",
      issues: [
        {
          ...ratedThrice,
          ratings: [
            {
              rating: most,
              reason: "The plan fit the first merge.",
              step_id: "5a6b7c8d-9e0f-5a1b-8c2d-3e4f5a6b7c8d",
              ended_at: "2026-11-17T05:10:00Z",
            },
            {
              rating: least,
              reason: "The plan sent the second merge back.",
              step_id: "0b1c2d3e-4f5a-5b6c-9d7e-8f9a0b1c2d3e",
              ended_at: "2026-11-17T07:00:00Z",
            },
            {
              rating: least + 1,
              reason: "The plan had no rollback step.",
              step_id: "9f0a1b2c-3d4e-5f6a-8b7c-9d0e1f2a3b4c",
              ended_at: "2026-11-17T08:55:00Z",
            },
          ],
        },
      ],
    };

    const { result } = await readPlans({ project: "INFRA" }, answer);

    expect(result.text.split("\n")).toEqual([
      defaultHeader("INFRA", 1, 1, 1),
      `INFRA-13 closed 2026-11-17T09:00:00Z plan default (1 tasks: planner implementer tester reviewer acceptance:owner) ratings ${most}/${most} "The plan fit the first merge.", ${least}/${most} "The plan sent the second merge back.", ${least + 1}/${most} "The plan had no rollback step." time 5h8m failed steps 0`,
    ]);
  });

  // The guide for agents spells out the line, and so states the scale twice: each rating out of
  // the most, and the scale's two ends. Each statement is read from the page's Plans row and held
  // to the contract's scale, so a new scale fails here until the page states it.
  test("the guide for agents' Plans row states the contract's scale", () => {
    const page = readFileSync(
      join(REPO_ROOT, "docs", "site", "src", "content", "docs", "dispatch", "for-agents.md"),
      "utf8"
    );
    const rows = page.split("\n").filter((line) => line.startsWith("| Plans |"));
    expect(rows).toHaveLength(1);
    const row = rows[0] ?? "";
    const outOf = [...row.matchAll(/<rating>\/(\d+)/g)].map((match) => Number(match[1]));

    expect(outOf).toEqual([LEGION_PLAN_RATING_MOST]);
    expect(row).toContain(
      `each rating is the one a retro gave the plan, ${LEGION_PLAN_RATING_LEAST} to ${LEGION_PLAN_RATING_MOST}, with its reason`
    );
  });

  // JSON escapes each character below U+0020, so a line feed in a reason stays on the line. It
  // leaves U+0085, U+2028 and U+2029 as they are, and a reader that splits lines the Unicode way
  // (Python's `str.splitlines`, for one) breaks the issue's line at each, so each prints escaped,
  // every time it occurs: each of the first three reasons holds one of them twice, and the last
  // holds each of the three twice, mixed, the second time all three side by side.
  test("a reason holding U+0085, U+2028 or U+2029 prints each one escaped, the same JSON string", async () => {
    for (const [reason, printed] of [
      [
        "The plan fit.\u0085The review did not.\u0085The merge did.",
        '"The plan fit.\\u0085The review did not.\\u0085The merge did."',
      ],
      [
        "The plan fit.\u2028The review did not.\u2028The merge did.",
        '"The plan fit.\\u2028The review did not.\\u2028The merge did."',
      ],
      [
        "The plan fit.\u2029The review did not.\u2029The merge did.",
        '"The plan fit.\\u2029The review did not.\\u2029The merge did."',
      ],
      [
        "Tasks.\u2028Steps.\u0085Roles.\u2029Agents.\u2028\u0085\u2029Done.",
        '"Tasks.\\u2028Steps.\\u0085Roles.\\u2029Agents.\\u2028\\u0085\\u2029Done."',
      ],
    ] as const) {
      const rating: PlanRating = {
        rating: 3,
        reason,
        step_id: "7e8f9a0b-1c2d-5e3f-8a4b-5c6d7e8f9a0b",
        ended_at: "2026-11-14T07:50:00Z",
      };
      // A run that began before Legion wrote plans and before Dispatch recorded starts: no plan,
      // no start, and its last record, the retro, ends the issue.
      const answer: ProjectPlans = {
        project: "INFRA",
        issues: [{ ...neverRan, ended_at: "2026-11-14T07:50:00Z", ratings: [rating] }],
      };

      const { result } = await readPlans({ project: "INFRA" }, answer);

      const lines = result.text.split("\n");
      expect(lines, printed).toEqual([
        defaultHeader("INFRA", 1, 1, 1),
        `INFRA-9 closed 2026-11-14T08:00:00Z plan none ratings 3/5 ${printed} time - failed steps 0`,
      ]);
      // The reason as the line prints it reads back as the reason Dispatch answered.
      const shown = /^.* ratings 3\/5 (".*") time - failed steps 0$/.exec(lines[1] ?? "")?.[1];
      expect(JSON.parse(shown ?? "null"), printed).toBe(reason);
    }
  });

  // Dispatch answers newest closed first. Here that order is no order of the keys, by text or by
  // number, either way up, so a command that sorted the issues by key would print another order.
  test("the issues print in the order Dispatch answers them, newest closed first, not by key", async () => {
    const answer: ProjectPlans = {
      project: "INFRA",
      issues: [
        { ...neverRan, issue: "INFRA-12", closed_at: "2026-11-17T10:00:00Z" },
        { ...neverRan, issue: "INFRA-9", closed_at: "2026-11-16T10:00:00Z" },
        { ...neverRan, issue: "INFRA-11", closed_at: "2026-11-15T10:00:00Z" },
      ],
    };

    const { result } = await readPlans({ project: "INFRA", since: "2026-11-01T00:00:00Z" }, answer);

    expect(result.text.split("\n")).toEqual([
      "INFRA: 3 finished issues",
      "INFRA-12 closed 2026-11-17T10:00:00Z plan none ratings none time - failed steps 0",
      "INFRA-9 closed 2026-11-16T10:00:00Z plan none ratings none time - failed steps 0",
      "INFRA-11 closed 2026-11-15T10:00:00Z plan none ratings none time - failed steps 0",
    ]);
  });

  test("an issue with no time prints time -, and one Legion never ran prints plan none too", async () => {
    const answer: ProjectPlans = { project: "INFRA", issues: [uncountedTime, neverRan] };

    const { result } = await readPlans({ project: "INFRA", since: "2026-11-01T00:00:00Z" }, answer);

    expect(result.text.split("\n")).toEqual([
      "INFRA: 2 finished issues",
      'INFRA-10 closed 2026-11-14T09:00:00Z plan default (1 tasks: planner implementer tester reviewer acceptance:owner) ratings 3/5 "Enough." time - failed steps 2',
      "INFRA-9 closed 2026-11-14T08:00:00Z plan none ratings none time - failed steps 0",
    ]);
  });

  // Each generated list is nullable, including a plan's tasks; null reads as an empty list.
  test("a null list reads as an empty one: no issue, no rating, no task", async () => {
    const empty: readonly ProjectPlans[] = [
      { project: "INFRA", issues: [] },
      { project: "INFRA", issues: null },
    ];
    for (const answer of empty) {
      const { result } = await readPlans({ project: "INFRA" }, answer);

      expect(result.text, JSON.stringify(answer)).toBe(defaultHeader("INFRA", 0, 0, 0));
      expect(result.details, JSON.stringify(answer)).toEqual({ project: "INFRA", issues: [] });
    }
    const nulls: ProjectPlans = {
      project: "INFRA",
      issues: [{ ...unrated, ratings: null, plan: { ...ownerPlan, tasks: null } }],
    };

    const { result } = await readPlans({ project: "INFRA" }, nulls);

    expect(result.text.split("\n")[1]).toBe(
      "INFRA-11 closed 2026-11-15T08:00:00Z plan owner (0 tasks: -) ratings none time 1h2m failed steps 0"
    );
  });

  // With no since the read is the default list, so an until alone rides beside planned_or_rated
  // and the limit; a since asks for every finished issue of its window, so it sends neither.
  test("since and until ride as the read's query, and the default list's filter and limit ride without since", async () => {
    const answer: ProjectPlans = { project: "INFRA", issues: [] };

    const both = await readPlans(
      { project: "INFRA", since: "2026-11-01T00:00:00Z", until: "2026-11-17T00:00:00Z" },
      answer
    );
    const sinceAlone = await readPlans({ project: "INFRA", since: "2026-11-01T00:00:00Z" }, answer);
    const untilAlone = await readPlans({ project: "INFRA", until: "2026-11-17T00:00:00Z" }, answer);
    const neither = await readPlans({ project: "INFRA" }, answer);

    expect(
      [both.requests, sinceAlone.requests, untilAlone.requests, neither.requests].flat()
    ).toEqual([
      "GET /api/v1/projects/INFRA/plans?since=2026-11-01T00%3A00%3A00Z&until=2026-11-17T00%3A00%3A00Z",
      "GET /api/v1/projects/INFRA/plans?since=2026-11-01T00%3A00%3A00Z",
      `GET /api/v1/projects/INFRA/plans?until=2026-11-17T00%3A00%3A00Z&${DEFAULT_QUERY}`,
      `GET /api/v1/projects/INFRA/plans?${DEFAULT_QUERY}`,
    ]);
  });

  // Each time is the span of its own start and end, as Dispatch counts it.
  test("time counts whole hours and the minutes left, rounded down", async () => {
    const lines: string[] = [];
    for (const [start, end, seconds] of [
      ["2026-11-14T06:00:00Z", "2026-11-14T06:00:00Z", 0],
      ["2026-11-14T06:00:00Z", "2026-11-14T06:00:59Z", 59],
      ["2026-11-14T06:00:00Z", "2026-11-14T06:59:59Z", 3599],
      ["2026-11-14T06:00:00Z", "2026-11-14T07:00:00Z", 3600],
      ["2026-11-14T06:00:00Z", "2026-11-15T07:01:01Z", 90061],
    ] as const) {
      const answer: ProjectPlans = {
        project: "INFRA",
        issues: [{ ...unrated, started_at: start, ended_at: end, duration_seconds: seconds }],
      };
      const { result } = await readPlans({ project: "INFRA" }, answer);
      lines.push((result.text.split("\n")[1] ?? "").replace(/.* time (\S+) .*/, "$1"));
    }

    expect(lines).toEqual(["0h0m", "0h0m", "0h59m", "1h0m", "25h1m"]);
  });
});

describe("dispatch plans on the command line", () => {
  const makeDirectory = temporaryDirectories("dispatch-plans");
  // Each row starts the fake Dispatch it reads, and this hook stops it. A server started in this
  // body would start under a filter that selects no row here, and no hook would stop it.
  const servers: FakeDispatchServer<unknown>[] = [];
  afterEach(() => {
    for (const server of servers.splice(0)) server.stop();
  });

  let stateDir = "";
  beforeEach(() => {
    stateDir = makeDirectory();
  });

  /** A fake Dispatch on a loopback port of its own whose plans reads of INFRA and of ACME each
   *  answer that project's own issues, which the hook above stops when the row ends. */
  function dispatchServing(): FakeDispatchServer<unknown> {
    const infra: ProjectPlans = { project: "INFRA", issues: [ratedOnce, unrated] };
    const acme: ProjectPlans = { project: "ACME", issues: [otherProject] };
    const server = startFakeDispatch({
      routes: { [plansPath("INFRA")]: infra, [plansPath("ACME")]: acme },
    });
    servers.push(server);
    return server;
  }

  /** Each request `server` got from its `before`th on, as `<pathname><search>`. */
  function readsSince(server: FakeDispatchServer<unknown>, before: number): string[] {
    return server.reads.slice(before).map((url) => `${url.pathname}${url.search}`);
  }

  async function run(
    server: FakeDispatchServer<unknown>,
    argv: readonly string[],
    session: Readonly<Record<string, string>> = {}
  ): Promise<{ code: number; stdout: string }> {
    let stdout = "";
    const code = await runDispatchCli(
      argv,
      {
        HOME: stateDir,
        DISPATCH_HOST: "omp",
        DISPATCH_SESSION_ID: "ses-1",
        DISPATCH_URL: server.url,
        DISPATCH_TOKEN: TOKEN,
        DISPATCH_STATE_DIR: stateDir,
        ...session,
      },
      {
        stdout: (text) => {
          stdout += text;
        },
        stderr: () => {},
        readText: (path) => readFileSync(path, "utf-8"),
        cwd: stateDir,
      }
    );
    return { code, stdout };
  }

  // The command names a project and never an issue, so a session whose LEGION_ISSUE names an issue
  // of a third project reads the project the call names, and reads no issue: the fake answers each
  // of two projects with issues of its own, and records every request it gets.
  const sessions: readonly Readonly<Record<string, string>>[] = [{}, { LEGION_ISSUE: "CORE-8" }];

  test("--project reads that project's finished issues and prints them, exit 0, whatever LEGION_ISSUE names", async () => {
    const server = dispatchServing();
    for (const [project, printed] of [
      [
        "INFRA",
        [
          defaultHeader("INFRA", 2, 2, 2),
          'INFRA-12 closed 2026-11-16T12:00:00Z plan default (1 tasks: planner implementer tester reviewer acceptance:owner) ratings 4/5 "The plan fit the work; the review sent it back once." time 1h30m failed steps 1',
          "INFRA-11 closed 2026-11-15T08:00:00Z plan owner (3 tasks: implementer reviewer) ratings none time 1h2m failed steps 0",
        ],
      ],
      [
        "ACME",
        [
          defaultHeader("ACME", 1, 1, 1),
          'ACME-3 closed 2026-11-13T17:00:00Z plan owner (1 tasks: implementer tester) ratings 2/5 "The plan left out the rollback." time 2h35m failed steps 3',
        ],
      ],
    ] as const) {
      for (const session of sessions) {
        const label = `${project} ${JSON.stringify(session)}`;
        const before = server.reads.length;

        const { code, stdout } = await run(server, ["plans", "--project", project], session);

        expect(readsSince(server, before), label).toEqual([
          `/api/v1/projects/${project}/plans?${DEFAULT_QUERY}`,
        ]);
        expect(code, label).toBe(0);
        expect(stdout, label).toBe(`${printed.join("\n")}\n`);
      }
    }
  });

  // The bounds go through the parser `dispatch plans` runs, each to its own member of the query,
  // and a bound left out is not sent; with no --since the default list's filter and limit ride
  // too. The two bounds differ, and neither is a whole minute.
  test("--since and --until ride as the read's query, each alone or both, and neither when not given", async () => {
    const server = dispatchServing();
    const since = "2026-11-01T08:15:30Z";
    const until = "2026-11-17T06:30:45Z";
    const cases: readonly [readonly string[], string][] = [
      [[], `?${DEFAULT_QUERY}`],
      [["--since", since], "?since=2026-11-01T08%3A15%3A30Z"],
      [["--until", until], `?until=2026-11-17T06%3A30%3A45Z&${DEFAULT_QUERY}`],
      [
        ["--since", since, "--until", until],
        "?since=2026-11-01T08%3A15%3A30Z&until=2026-11-17T06%3A30%3A45Z",
      ],
    ];
    for (const [bounds, query] of cases) {
      const before = server.reads.length;

      const { code } = await run(server, ["plans", "--project", "INFRA", ...bounds]);

      expect(readsSince(server, before), bounds.join(" ")).toEqual([
        `/api/v1/projects/INFRA/plans${query}`,
      ]);
      expect(code, bounds.join(" ")).toBe(0);
    }
  });

  // A key that is not one is refused once, naming its one problem, with nothing sent, whatever
  // issue LEGION_ISSUE names.
  test("a project that is not a key is refused once, sending nothing, exit 2", async () => {
    const server = dispatchServing();
    for (const session of sessions) {
      const before = server.reads.length;

      const { code, stdout } = await run(server, ["plans", "--project", "infra"], session);

      expect(readsSince(server, before), JSON.stringify(session)).toEqual([]);
      expect(code, JSON.stringify(session)).toBe(2);
      expect(stdout, JSON.stringify(session)).toContain(
        "dispatch plans was not called: 1 problem\n"
      );
      expect(
        stdout.split(`project is not a project key: ${PROJECT_KEY_RULE}`),
        JSON.stringify(session)
      ).toHaveLength(2);
    }
  });
});
