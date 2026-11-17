import { afterAll, beforeEach, describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import type { IssueSteps, StepRecord } from "@legion/contracts";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { bearerCredential } from "../bearer";
import { runDispatchCli } from "../dispatch-cli";
import { executeDispatchTool } from "../dispatch-execute";
import { forgetKeyedLayerMemory } from "../dispatch-http";

// `dispatch steps --issue <KEY>`: the steps that ran on an issue, as Dispatch's
// `GET /api/v1/issues/{key}/steps` answers them, one header line and one line per run. The
// format cases drive `executeDispatchTool` over a recording fetch, so each reads the text and the
// details a host is handed; the last case drives the real CLI against a fake Dispatch server.

/** The implementer's run of LEG-1's step 2: ended, with its model and its report. */
const implementer: StepRecord = {
  id: "3f0c8a52-6d1e-5b7a-9c2d-4e8f1a6b7c90",
  issue: "LEG-1",
  task: "default",
  step: 2,
  kind: "step",
  role: "implementer",
  type: "implementation",
  agent: "legion-leg-leg-1-implementer",
  session: "ses-implementer",
  model: "anthropic/claude-opus-5",
  generation: 1,
  attempt: 1,
  started_at: "2026-11-16T09:00:00Z",
  ended_at: "2026-11-16T09:40:00Z",
  result: "passed",
  returned_to: null,
  report: "9b2e4d6f-8a1c-4e3b-a5d7-c9f1e3b5d7a9",
};

/** The tester's run of step 3, still running: no end, no result, and no model answered yet. */
const tester: StepRecord = {
  id: "5a1d9c3e-7b2f-5d8a-8e4c-6f0a2b4c6d81",
  issue: "LEG-1",
  task: "default",
  step: 3,
  kind: "step",
  role: "tester",
  type: "testing",
  agent: "legion-leg-leg-1-tester",
  session: "ses-tester",
  model: "",
  generation: 1,
  attempt: 1,
  started_at: "2026-11-16T10:00:00Z",
  ended_at: null,
  result: "",
  returned_to: null,
  report: null,
};

/** A merge GitHub already made: no merger started, so no agent, no model and no start. */
const mergedByGitHub: StepRecord = {
  id: "7c3e1a5b-9d4f-5a2c-b6e8-0a2c4e6f8a03",
  issue: "LEG-1",
  task: "default",
  step: 0,
  kind: "merge",
  role: "merger",
  type: "",
  agent: "",
  session: "",
  model: "",
  generation: 1,
  attempt: 1,
  started_at: null,
  ended_at: "2026-11-16T12:00:00Z",
  result: "passed",
  returned_to: null,
  report: null,
};

/** A second run of a review of another task, sent back to step 1 of that task. */
const docsReview: StepRecord = {
  id: "1e5a3c7d-2b6f-5e9a-8c1d-3f5a7b9c1d25",
  issue: "LEG-1",
  task: "docs",
  step: 2,
  kind: "step",
  role: "reviewer",
  type: "review",
  agent: "legion-leg-leg-1-reviewer",
  session: "ses-reviewer",
  model: "openai/gpt-6.1-sol",
  generation: 1,
  attempt: 2,
  started_at: "2026-11-16T11:00:00Z",
  ended_at: "2026-11-16T11:20:00Z",
  result: "failed",
  returned_to: 1,
  report: null,
};

const config = {
  enabled: true,
  url: "http://dispatch.test",
  credential: bearerCredential("secret"),
  error: null,
};

/** A fetch that records each request and answers the steps read with `steps`. */
function answering(requests: string[], steps: unknown): typeof fetch {
  return (async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const method = init?.method ?? "GET";
    const { pathname } = new URL(String(url));
    requests.push(`${method} ${pathname}`);
    return Response.json(steps);
  }) as typeof fetch;
}

function readSteps(issue: string, fetchImpl: typeof fetch) {
  return executeDispatchTool({
    tool: "dispatch_steps",
    args: { issue },
    cwd: "/workspace",
    host: "omp",
    sessionId: "session-1",
    config,
    env: {},
    exec: async (file, args) => {
      throw new Error(`unexpected command: ${file} ${args.join(" ")}`);
    },
    fetchImpl,
  });
}

describe("dispatch steps", () => {
  test("prints a header, then one line per run, a running step running", async () => {
    const requests: string[] = [];
    const answer: IssueSteps = {
      issue: "LEG-1",
      tasks: [{ task: "default", steps: [implementer, tester] }],
    };

    const result = await readSteps("LEG-1", answering(requests, answer));

    expect(requests).toEqual(["GET /api/v1/issues/LEG-1/steps"]);
    expect(result.text).toBe(
      [
        "LEG-1: 2 steps in 1 tasks",
        "default 2 step implementer implementation attempt 1 legion-leg-leg-1-implementer anthropic/claude-opus-5 2026-11-16T09:00:00Z 2026-11-16T09:40:00Z passed",
        "default 3 step tester testing attempt 1 legion-leg-leg-1-tester - 2026-11-16T10:00:00Z running running",
      ].join("\n")
    );
    expect(result.details).toEqual({ issue: "LEG-1", tasks: answer.tasks });
  });

  test("a field with no value prints -, the start of a run with none among them", async () => {
    const answer: IssueSteps = {
      issue: "LEG-1",
      tasks: [{ task: "default", steps: [implementer, mergedByGitHub] }],
    };

    const result = await readSteps("LEG-1", answering([], answer));

    expect(result.text.split("\n")[2]).toBe(
      "default 0 merge merger - attempt 1 - - - 2026-11-16T12:00:00Z passed"
    );
  });

  test("each task's runs print together, in the order the read lists the tasks", async () => {
    const answer: IssueSteps = {
      issue: "LEG-1",
      tasks: [
        { task: "default", steps: [implementer, tester] },
        { task: "docs", steps: [docsReview] },
      ],
    };

    const result = await readSteps("LEG-1", answering([], answer));

    expect(result.text).toBe(
      [
        "LEG-1: 3 steps in 2 tasks",
        "default 2 step implementer implementation attempt 1 legion-leg-leg-1-implementer anthropic/claude-opus-5 2026-11-16T09:00:00Z 2026-11-16T09:40:00Z passed",
        "default 3 step tester testing attempt 1 legion-leg-leg-1-tester - 2026-11-16T10:00:00Z running running",
        "docs 2 step reviewer review attempt 2 legion-leg-leg-1-reviewer openai/gpt-6.1-sol 2026-11-16T11:00:00Z 2026-11-16T11:20:00Z failed",
      ].join("\n")
    );
  });

  // Dispatch answers an issue it does not hold, and one no step ran on, with no task; the
  // generated type also lets the list be null, which reads as the empty list.
  test("an issue with no step prints 0 steps in 0 tasks", async () => {
    for (const answer of [
      { issue: "LEG-1", tasks: [] },
      { issue: "LEG-1", tasks: null },
    ]) {
      const result = await readSteps("LEG-1", answering([], answer));

      expect(result.text, JSON.stringify(answer)).toBe("LEG-1: 0 steps in 0 tasks");
      expect(result.details, JSON.stringify(answer)).toEqual({ issue: "LEG-1", tasks: [] });
    }
  });
});

describe("dispatch steps on the command line", () => {
  const requests: string[] = [];
  const makeDirectory = temporaryDirectories("dispatch-steps");
  const server = Bun.serve({
    port: 0,
    fetch(request) {
      const { pathname } = new URL(request.url);
      requests.push(`${request.method} ${pathname}`);
      if (pathname === "/api/v1/issues/LEG-1/steps" && request.method === "GET") {
        return Response.json({
          issue: "LEG-1",
          tasks: [{ task: "default", steps: [implementer, tester] }],
        });
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

  test("--issue and a bare key read the same steps and print them, exit 0", async () => {
    for (const argv of [
      ["steps", "--issue", "LEG-1"],
      ["steps", "LEG-1"],
    ]) {
      requests.length = 0;

      const { code, stdout } = await run(argv);

      expect(code, argv.join(" ")).toBe(0);
      expect(requests).toEqual(["GET /api/v1/issues/LEG-1/steps"]);
      expect(stdout).toContain(
        [
          "LEG-1: 2 steps in 1 tasks",
          "default 2 step implementer implementation attempt 1 legion-leg-leg-1-implementer anthropic/claude-opus-5 2026-11-16T09:00:00Z 2026-11-16T09:40:00Z passed",
          "default 3 step tester testing attempt 1 legion-leg-leg-1-tester - 2026-11-16T10:00:00Z running running",
        ].join("\n")
      );
    }
  });
});
