import { afterAll, beforeEach, describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { type FinishedIssue, PROJECT_KEY_RULE, type ProjectPlans } from "@legion/contracts";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { bearerCredential } from "../bearer";
import { runDispatchCli } from "../dispatch-cli";
import { executeDispatchTool } from "../dispatch-execute";
import { forgetKeyedLayerMemory } from "../dispatch-http";

// `dispatch plans --project <KEY>`: each finished issue of a project, as Dispatch's
// `GET /api/v1/projects/{key}/plans` answers them, one header line and one line per issue: its
// plan, each rating its retros gave the plan with the reason, its time and its count of failed
// steps. The Legion whole-daemon driver parses the line, so each case asserts it whole. The format
// cases drive `executeDispatchTool` over a recording fetch, so each reads the text and the details
// a host is handed; the last case drives the real CLI against a fake Dispatch server.

/** A finished issue Legion ran on its default plan, rated once: an hour and a half, one failed
 *  review. */
const ratedOnce: FinishedIssue = {
  issue: "INFRA-12",
  title: "Retry failed webhook deliveries",
  closed_at: "2026-11-16T12:00:00Z",
  plan: {
    id: "6b1f0c3e-2d4a-5e8b-9c7d-1a2b3c4d5e6f",
    issue: "INFRA-12",
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
  },
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
};

/** A finished issue its owner planned in two tasks, which no retro rated. */
const unrated: FinishedIssue = {
  issue: "INFRA-11",
  title: "Document the retry",
  closed_at: "2026-11-15T08:00:00Z",
  plan: {
    id: "3c5e7a9b-4d6f-5a8c-8e0b-2d4f6a8c0e1b",
    issue: "INFRA-11",
    generation: 2,
    source: "owner",
    artifact_id: "9a8b7c6d-5e4f-4a3b-8c2d-1e0f9a8b7c6d",
    version: 3,
    tasks: [
      { id: "api", steps: ["implementer", "reviewer"] },
      { id: "docs", steps: ["implementer:docs"] },
    ],
  },
  ratings: [],
  started_at: "2026-11-15T06:55:00Z",
  ended_at: "2026-11-15T07:57:05Z",
  duration_seconds: 3725,
  failed_steps: 0,
};

/** Two retros, so two ratings, one reason holding a quotation mark and a line break. */
const ratedTwice: FinishedIssue = {
  ...ratedOnce,
  issue: "INFRA-13",
  closed_at: "2026-11-17T09:00:00Z",
  ratings: [
    {
      rating: 2,
      reason: "The plan had no test step.",
      step_id: "1a2b3c4d-5e6f-5a7b-8c9d-0e1f2a3b4c5d",
      ended_at: "2026-11-17T07:00:00Z",
    },
    {
      rating: 5,
      reason: 'It said "ship it"\nand it shipped.',
      step_id: "2b3c4d5e-6f7a-5b8c-9d0e-1f2a3b4c5d6e",
      ended_at: "2026-11-17T08:55:00Z",
    },
  ],
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
};

/** A finished issue whose first step started before Dispatch recorded starts: a plan and a
 *  rating, and no time. */
const unknownStart: FinishedIssue = {
  ...ratedOnce,
  issue: "INFRA-10",
  closed_at: "2026-11-14T09:00:00Z",
  ratings: [
    {
      rating: 3,
      reason: "Enough.",
      step_id: "4d5e6f7a-8b9c-5d0e-8f1a-3b4c5d6e7f80",
      ended_at: "2026-11-14T08:50:00Z",
    },
  ],
  started_at: null,
  duration_seconds: null,
  failed_steps: 2,
};

const config = {
  enabled: true,
  url: "http://dispatch.test",
  credential: bearerCredential("secret"),
  error: null,
};

/** A fetch that records each request, its query included, and answers the plans read with
 *  `plans`. */
function answering(requests: string[], plans: unknown): typeof fetch {
  return (async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const method = init?.method ?? "GET";
    const { pathname, search } = new URL(String(url));
    requests.push(`${method} ${pathname}${search}`);
    return Response.json(plans);
  }) as typeof fetch;
}

function readPlans(args: Record<string, unknown>, fetchImpl: typeof fetch) {
  return executeDispatchTool({
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
    fetchImpl,
  });
}

describe("dispatch plans", () => {
  test("prints a header, then one line per finished issue, a rated one and an unrated one", async () => {
    const requests: string[] = [];
    const answer: ProjectPlans = { project: "INFRA", issues: [ratedOnce, unrated] };

    const result = await readPlans({ project: "INFRA" }, answering(requests, answer));

    expect(requests).toEqual(["GET /api/v1/projects/INFRA/plans"]);
    expect(result.text).toBe(
      [
        "INFRA: 2 finished issues",
        'INFRA-12 closed 2026-11-16T12:00:00Z plan default (1 tasks: planner implementer tester reviewer acceptance:owner) ratings 4/5 "The plan fit the work; the review sent it back once." time 1h30m failed steps 1',
        "INFRA-11 closed 2026-11-15T08:00:00Z plan owner (2 tasks: implementer reviewer) ratings none time 1h2m failed steps 0",
      ].join("\n")
    );
    expect(result.details).toEqual({ project: "INFRA", issues: [ratedOnce, unrated] });
  });

  test("an issue with two ratings lists both in order, each reason one JSON string", async () => {
    const answer: ProjectPlans = { project: "INFRA", issues: [ratedTwice] };

    const result = await readPlans({ project: "INFRA" }, answering([], answer));

    expect(result.text.split("\n")).toEqual([
      "INFRA: 1 finished issues",
      'INFRA-13 closed 2026-11-17T09:00:00Z plan default (1 tasks: planner implementer tester reviewer acceptance:owner) ratings 2/5 "The plan had no test step.", 5/5 "It said \\"ship it\\"\\nand it shipped." time 1h30m failed steps 0',
    ]);
  });

  test("an issue with no time prints time -, and one Legion never ran prints plan none too", async () => {
    const answer: ProjectPlans = { project: "INFRA", issues: [unknownStart, neverRan] };

    const result = await readPlans({ project: "INFRA" }, answering([], answer));

    expect(result.text.split("\n")).toEqual([
      "INFRA: 2 finished issues",
      'INFRA-10 closed 2026-11-14T09:00:00Z plan default (1 tasks: planner implementer tester reviewer acceptance:owner) ratings 3/5 "Enough." time - failed steps 2',
      "INFRA-9 closed 2026-11-14T08:00:00Z plan none ratings none time - failed steps 0",
    ]);
  });

  // The generated shape lets each list be null; Dispatch writes `[]`, and null reads the same.
  test("a null list reads as an empty one: no issue, no rating, no task", async () => {
    for (const answer of [
      { project: "INFRA", issues: [] },
      { project: "INFRA", issues: null },
    ]) {
      const result = await readPlans({ project: "INFRA" }, answering([], answer));

      expect(result.text, JSON.stringify(answer)).toBe("INFRA: 0 finished issues");
      expect(result.details, JSON.stringify(answer)).toEqual({ project: "INFRA", issues: [] });
    }
    const nulls = {
      project: "INFRA",
      issues: [{ ...neverRan, ratings: null, plan: { ...unrated.plan, tasks: null } }],
    };

    const result = await readPlans({ project: "INFRA" }, answering([], nulls));

    expect(result.text.split("\n")[1]).toBe(
      "INFRA-9 closed 2026-11-14T08:00:00Z plan owner (0 tasks: -) ratings none time - failed steps 0"
    );
  });

  test("since and until ride as the read's query, and are left out when not given", async () => {
    const requests: string[] = [];
    const answer: ProjectPlans = { project: "INFRA", issues: [] };

    await readPlans(
      { project: "INFRA", since: "2026-11-01T00:00:00Z", until: "2026-11-17T00:00:00Z" },
      answering(requests, answer)
    );
    await readPlans(
      { project: "INFRA", until: "2026-11-17T00:00:00Z" },
      answering(requests, answer)
    );

    expect(requests).toEqual([
      "GET /api/v1/projects/INFRA/plans?since=2026-11-01T00%3A00%3A00Z&until=2026-11-17T00%3A00%3A00Z",
      "GET /api/v1/projects/INFRA/plans?until=2026-11-17T00%3A00%3A00Z",
    ]);
  });

  test("time counts whole hours and the minutes left, rounded down", async () => {
    const lines: string[] = [];
    for (const seconds of [0, 59, 3599, 3600, 90061]) {
      const answer = {
        project: "INFRA",
        issues: [{ ...neverRan, duration_seconds: seconds }],
      };
      const result = await readPlans({ project: "INFRA" }, answering([], answer));
      lines.push((result.text.split("\n")[1] ?? "").replace(/.* time (\S+) .*/, "$1"));
    }

    expect(lines).toEqual(["0h0m", "0h0m", "0h59m", "1h0m", "25h1m"]);
  });
});

describe("dispatch plans on the command line", () => {
  const requests: string[] = [];
  const makeDirectory = temporaryDirectories("dispatch-plans");
  const server = Bun.serve({
    port: 0,
    fetch(request) {
      const { pathname } = new URL(request.url);
      requests.push(`${request.method} ${pathname}`);
      if (pathname === "/api/v1/projects/INFRA/plans" && request.method === "GET") {
        return Response.json({ project: "INFRA", issues: [ratedOnce, unrated] });
      }
      return Response.json({ error: `unexpected ${request.method} ${pathname}` }, { status: 500 });
    },
  });
  afterAll(() => server.stop(true));

  let stateDir = "";
  beforeEach(() => {
    requests.length = 0;
    stateDir = makeDirectory();
    forgetKeyedLayerMemory();
  });

  async function run(argv: readonly string[]): Promise<{ code: number; stdout: string }> {
    let stdout = "";
    const code = await runDispatchCli(
      argv,
      {
        HOME: stateDir,
        DISPATCH_HOST: "omp",
        DISPATCH_SESSION_ID: "ses-1",
        DISPATCH_URL: `http://127.0.0.1:${server.port}`,
        DISPATCH_TOKEN: "test-token",
        DISPATCH_STATE_DIR: stateDir,
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

  test("--project reads the project's finished issues and prints them, exit 0", async () => {
    const { code, stdout } = await run(["plans", "--project", "INFRA"]);

    expect(code).toBe(0);
    expect(requests).toEqual(["GET /api/v1/projects/INFRA/plans"]);
    expect(stdout).toContain(
      [
        "INFRA: 2 finished issues",
        'INFRA-12 closed 2026-11-16T12:00:00Z plan default (1 tasks: planner implementer tester reviewer acceptance:owner) ratings 4/5 "The plan fit the work; the review sent it back once." time 1h30m failed steps 1',
        "INFRA-11 closed 2026-11-15T08:00:00Z plan owner (2 tasks: implementer reviewer) ratings none time 1h2m failed steps 0",
      ].join("\n")
    );
  });

  // The command names a project and never an issue, so no owner is read from the checkout, and a
  // key that is not one is refused once, naming its one problem, with nothing sent.
  test("a project that is not a key is refused once, sending nothing, exit 2", async () => {
    const { code, stdout } = await run(["plans", "--project", "infra"]);

    expect(code).toBe(2);
    expect(requests).toEqual([]);
    expect(stdout).toContain("dispatch plans was not called: 1 problem\n");
    expect(stdout.split(`project is not a project key: ${PROJECT_KEY_RULE}`)).toHaveLength(2);
  });
});
