import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import type { IssueSteps, StepRecord } from "@legion/contracts";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import {
  type FakeDispatchFetch,
  type FakeDispatchOptions,
  type FakeDispatchServer,
  fakeDispatchFetch,
  startFakeDispatch,
} from "../../test/fake-dispatch";
import { bearerCredential } from "../bearer";
import { runDispatchCli } from "../dispatch-cli";
import { executeDispatchTool } from "../dispatch-execute";
import { DispatchNoRouteError, forgetKeyedLayerMemory } from "../dispatch-http";
import { asksWhoami, WHOAMI_NO_CUT } from "./whoami-stand-in";

// `dispatch steps --issue <KEY>`: the steps that ran on an issue, as Dispatch's
// `GET /api/v1/issues/{key}/steps` answers them, one header line and one line per run. The
// format cases drive `executeDispatchTool` over the shared fake Dispatch (`test/fake-dispatch.ts`)
// as a fetch, so each reads the text and the details a host is handed; the command-line cases
// drive the real CLI against that fake on a loopback port, and read its exit code and output.

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
  rating: null,
  rating_reason: null,
};

/** The tester's run of step 3, still running: no end, no result, and no session or model, which
 *  only the end writes, as one read of its agent. */
const tester: StepRecord = {
  id: "5a1d9c3e-7b2f-5d8a-8e4c-6f0a2b4c6d81",
  issue: "LEG-1",
  task: "default",
  step: 3,
  kind: "step",
  role: "tester",
  type: "testing",
  agent: "legion-leg-leg-1-tester",
  session: "",
  model: "",
  generation: 1,
  attempt: 1,
  started_at: "2026-11-16T10:00:00Z",
  ended_at: null,
  result: "",
  returned_to: null,
  report: null,
  rating: null,
  rating_reason: null,
};

/**
 * A merge GitHub had already made, which started under the release before this one: that release
 * counted no start, so the end names no agent, no model and no start.
 */
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
  rating: null,
  rating_reason: null,
};

/** The docs task's first run, which started before any run of task default. */
const docsDraft: StepRecord = {
  id: "4b6d8f0a-1c3e-5a7b-9d2f-6e8a0c2e4a63",
  issue: "LEG-1",
  task: "docs",
  step: 1,
  kind: "step",
  role: "implementer",
  type: "implementation",
  agent: "legion-leg-leg-1-implementer",
  session: "ses-docs-implementer",
  model: "anthropic/claude-opus-5",
  generation: 1,
  attempt: 1,
  started_at: "2026-11-16T08:00:00Z",
  ended_at: "2026-11-16T08:30:00Z",
  result: "passed",
  returned_to: null,
  report: null,
  rating: null,
  rating_reason: null,
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
  rating: null,
  rating_reason: null,
};

/** The implementer's retro of LEG-1, which rated the plan: a retro is the one kind of run that
 *  holds a rating and its reason. */
const ratedRetro: StepRecord = {
  id: "2f6b4d8a-3c7e-5f1b-9d2a-4e6f8a0c2e47",
  issue: "LEG-1",
  task: "default",
  step: 0,
  kind: "retro",
  role: "implementer",
  type: "",
  agent: "legion-leg-leg-1-implementer",
  session: "ses-implementer",
  model: "anthropic/claude-opus-5",
  generation: 1,
  attempt: 1,
  started_at: "2026-11-16T11:30:00Z",
  ended_at: "2026-11-16T11:45:00Z",
  result: "passed",
  returned_to: null,
  report: null,
  rating: 4,
  rating_reason: "The plan fit the work; the review sent it back once.",
};

/** A merger's run that failed and sent its task back to place 0, the retro: a destination of 0
 *  is a place, not the absence of one. Merging ends with no handoff record, so it names no report. */
const mergeSentBack: StepRecord = {
  id: "9d4b2f6a-3c8e-5f1b-a7d9-2e4c6a8b0d47",
  issue: "LEG-1",
  task: "default",
  step: 0,
  kind: "merge",
  role: "merger",
  type: "",
  agent: "legion-leg-leg-1-merger",
  session: "ses-merger",
  model: "anthropic/claude-opus-5",
  generation: 1,
  attempt: 1,
  started_at: "2026-11-16T12:00:00Z",
  ended_at: "2026-11-16T12:10:00Z",
  result: "failed",
  returned_to: 0,
  report: null,
  rating: null,
  rating_reason: null,
};

const config = {
  enabled: true,
  url: "http://dispatch.test",
  credential: bearerCredential("secret"),
  error: null,
};

/** The one route the command reads. */
const STEPS_PATH = "/api/v1/issues/LEG-1/steps";

/** `dispatch steps --issue LEG-1` over `dispatch`, a fake Dispatch as a fetch. The client waits
 *  no time before it asks again a request Dispatch answered with no route. */
function runSteps(dispatch: FakeDispatchFetch) {
  return executeDispatchTool({
    tool: "dispatch_steps",
    args: { issue: "LEG-1" },
    cwd: "/workspace",
    host: "omp",
    sessionId: "session-1",
    config,
    env: {},
    exec: async (file, args) => {
      throw new Error(`unexpected command: ${file} ${args.join(" ")}`);
    },
    fetchImpl: dispatch.fetchImpl,
    sleep: async () => {},
  });
}

/** `dispatch steps --issue LEG-1` over a fake Dispatch whose steps route answers `answer`: the
 *  result, and each request the fake saw as `<method> <pathname>`. */
async function readSteps(answer: IssueSteps) {
  const dispatch = fakeDispatchFetch({ routes: { [STEPS_PATH]: answer } });
  const result = await runSteps(dispatch);
  return {
    result,
    requests: dispatch.calls.map((call) => `${call.method} ${call.url.pathname}`),
  };
}

describe("dispatch steps", () => {
  test("prints a header, then one line per run, a running step running", async () => {
    const answer: IssueSteps = {
      issue: "LEG-1",
      tasks: [{ task: "default", steps: [implementer, tester] }],
    };

    const { result, requests } = await readSteps(answer);

    expect(requests).toEqual([`GET ${STEPS_PATH}`]);
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

    const { result } = await readSteps(answer);

    expect(result.text.split("\n")[2]).toBe(
      "default 0 merge merger - attempt 1 - - - 2026-11-16T12:00:00Z passed"
    );
  });

  // The line keeps its columns and prints no rating; the details a host is handed are the
  // records as Dispatch answered them, so a retro's rating and its reason reach it.
  test("a retro's rating and its reason reach the details, and its line keeps the columns", async () => {
    const answer: IssueSteps = {
      issue: "LEG-1",
      tasks: [{ task: "default", steps: [implementer, ratedRetro] }],
    };

    const { result } = await readSteps(answer);

    expect(result.text.split("\n")[2]).toBe(
      "default 0 retro implementer - attempt 1 legion-leg-leg-1-implementer anthropic/claude-opus-5 2026-11-16T11:30:00Z 2026-11-16T11:45:00Z passed"
    );
    expect(result.details).toEqual({
      issue: "LEG-1",
      tasks: [{ task: "default", steps: [implementer, ratedRetro] }],
    });
  });

  // Dispatch stores passed, failed or stopped as the result of a run that ended, and "" as that of
  // one still running (workflowstore's stepResults). The read's generated type admits those words
  // and no other: each run below is a StepRecord, so `bun run typecheck` holds its result to them,
  // and it fails at the directive below once model.StepRecord.Result loses its enum and the
  // contract is generated again.
  test("an ended run prints the result Dispatch stored, a word the read's type holds", async () => {
    // @ts-expect-error - a word Dispatch never stores is no result.
    const notStored: StepRecord["result"] = "skipped";
    void notStored;

    // The implementer's run as each end leaves it: passed, naming its report; failed, sent back to
    // step 1 by the architect's backward move, which names no report of the implementer's; and
    // stopped, ended under it, which returns nowhere and names none.
    const ended: readonly StepRecord[] = [
      implementer,
      { ...implementer, result: "failed", returned_to: 1, report: null },
      { ...implementer, result: "stopped", report: null },
    ];
    for (const run of ended) {
      const { result } = await readSteps({
        issue: "LEG-1",
        tasks: [{ task: "default", steps: [run] }],
      });

      expect(result.text.split("\n")[1], run.result).toEndWith(
        ` 2026-11-16T09:40:00Z ${run.result}`
      );
    }
  });

  // A run is of a step, a retro or a merge (model.StepRecord.Kind). The read's generated type
  // admits those kinds and no other, so `bun run typecheck` fails at the directive below once the
  // field loses its enum and the contract is generated again.
  test("a retro's run prints its kind at place 0, a kind the read's type holds", async () => {
    // @ts-expect-error - a kind Dispatch never stores is no kind.
    const notStored: StepRecord["kind"] = "pause";
    void notStored;
    // The implementer runs the retro, at place 0 and of no type, and names no report.
    const retro: StepRecord = {
      ...implementer,
      id: "6e8a0c2e-4b6d-5f8a-9c1e-3a5c7e9b1d47",
      step: 0,
      kind: "retro",
      type: "",
      started_at: "2026-11-16T11:30:00Z",
      ended_at: "2026-11-16T11:45:00Z",
      report: null,
    };

    const { result } = await readSteps({
      issue: "LEG-1",
      tasks: [{ task: "default", steps: [implementer, retro] }],
    });

    expect(result.text.split("\n")[2]).toBe(
      "default 0 retro implementer - attempt 1 legion-leg-leg-1-implementer anthropic/claude-opus-5 2026-11-16T11:30:00Z 2026-11-16T11:45:00Z passed"
    );
  });

  // Dispatch lists the tasks in the order each first ran, which is not the order of all runs'
  // starts: docs first ran before default, and ran again after.
  test("each task's runs print together, in the order the read lists the tasks", async () => {
    const answer: IssueSteps = {
      issue: "LEG-1",
      tasks: [
        { task: "docs", steps: [docsDraft, docsReview] },
        { task: "default", steps: [implementer] },
      ],
    };

    const { result } = await readSteps(answer);

    expect(result.text).toBe(
      [
        "LEG-1: 3 steps in 2 tasks",
        "docs 1 step implementer implementation attempt 1 legion-leg-leg-1-implementer anthropic/claude-opus-5 2026-11-16T08:00:00Z 2026-11-16T08:30:00Z passed",
        "docs 2 step reviewer review attempt 2 legion-leg-leg-1-reviewer openai/gpt-6.1-sol 2026-11-16T11:00:00Z 2026-11-16T11:20:00Z failed",
        "default 2 step implementer implementation attempt 1 legion-leg-leg-1-implementer anthropic/claude-opus-5 2026-11-16T09:00:00Z 2026-11-16T09:40:00Z passed",
      ].join("\n")
    );
    expect(result.details).toEqual({ issue: "LEG-1", tasks: answer.tasks });
  });

  // The text leaves out where a run sent its task back and the report it named; the details hold
  // every field of every record as Dispatch answered it.
  test("the details hold each record whole, a destination of 0 and a report, and neither", async () => {
    const answer: IssueSteps = {
      issue: "LEG-1",
      tasks: [
        { task: "default", steps: [implementer, tester, mergeSentBack] },
        { task: "docs", steps: [docsReview] },
      ],
    };

    const { result } = await readSteps(answer);

    expect(result.details).toEqual({ issue: "LEG-1", tasks: answer.tasks });
  });

  // Dispatch answers an issue no step ran on with no task (a key no issue carries is its 404
  // NOT_FOUND, as the issue read answers it); the generated type also lets the list be null, which
  // reads as the empty list.
  test("an issue with no step prints 0 steps in 0 tasks", async () => {
    for (const answer of [
      { issue: "LEG-1", tasks: [] },
      { issue: "LEG-1", tasks: null },
    ]) {
      const { result } = await readSteps(answer);

      expect(result.text, JSON.stringify(answer)).toBe("LEG-1: 0 steps in 0 tasks");
      expect(result.details, JSON.stringify(answer)).toEqual({ issue: "LEG-1", tasks: [] });
    }
  });

  // A Dispatch older than the steps route answers it with its router's no-route 404. The client
  // asks again after each wait and then throws, so the read fails: it never reads as an issue
  // with no step, which only that route's 200 says.
  test("a Dispatch with no steps route fails the read, and answers no steps", async () => {
    const dispatch = fakeDispatchFetch({
      handler: (call) =>
        Response.json(
          {
            code: "NOT_FOUND",
            error: `no route for ${call.method} ${call.url.pathname}`,
            hint: "GET /api/v1 lists every route",
          },
          { status: 404 }
        ),
    });

    await expect(runSteps(dispatch)).rejects.toBeInstanceOf(DispatchNoRouteError);
    expect(new Set(dispatch.calls.map((call) => `${call.method} ${call.url.pathname}`))).toEqual(
      new Set([`GET ${STEPS_PATH}`])
    );
  });

  // The generated type lets one task's own list be null too, which reads as a task with no run.
  test("a task whose list of runs is null holds no run, beside a task that holds one", async () => {
    const answer: IssueSteps = {
      issue: "LEG-1",
      tasks: [
        { task: "docs", steps: null },
        { task: "default", steps: [implementer] },
      ],
    };

    const { result } = await readSteps(answer);

    expect(result.text).toBe(
      [
        "LEG-1: 1 steps in 2 tasks",
        "default 2 step implementer implementation attempt 1 legion-leg-leg-1-implementer anthropic/claude-opus-5 2026-11-16T09:00:00Z 2026-11-16T09:40:00Z passed",
      ].join("\n")
    );
    expect(result.details).toEqual({
      issue: "LEG-1",
      tasks: [
        { task: "docs", steps: [] },
        { task: "default", steps: [implementer] },
      ],
    });
  });
});

describe("dispatch steps on the command line", () => {
  const makeDirectory = temporaryDirectories("dispatch-steps");
  // Each row starts the fake Dispatch it reads, and this hook stops it. A server started in this
  // body would start under a filter that selects no row here, and no hook would stop it.
  const servers: FakeDispatchServer<unknown>[] = [];
  afterEach(() => {
    for (const server of servers.splice(0)) server.stop();
  });

  let stateDir = "";
  beforeEach(() => {
    stateDir = makeDirectory();
    forgetKeyedLayerMemory();
  });

  /** A fake Dispatch on a loopback port of its own, answering as `options` say, which the hook
   *  above stops when the row ends. */
  function dispatchServing(options: FakeDispatchOptions): FakeDispatchServer<unknown> {
    const server = startFakeDispatch(options);
    servers.push(server);
    return server;
  }

  async function run(
    server: FakeDispatchServer<unknown>,
    argv: readonly string[]
  ): Promise<{ code: number; stdout: string }> {
    let stdout = "";
    const code = await runDispatchCli(
      argv,
      {
        HOME: stateDir,
        DISPATCH_HOST: "omp",
        DISPATCH_SESSION_ID: "ses-1",
        DISPATCH_URL: server.url,
        DISPATCH_TOKEN: fixtureSecret("dsp"),
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
    const answer: IssueSteps = {
      issue: "LEG-1",
      tasks: [{ task: "default", steps: [implementer, tester] }],
    };
    for (const argv of [
      ["steps", "--issue", "LEG-1"],
      ["steps", "LEG-1"],
    ]) {
      const label = argv.join(" ");
      const server = dispatchServing({ routes: { [STEPS_PATH]: answer } });

      const { code, stdout } = await run(server, argv);

      expect(code, label).toBe(0);
      // The run read the steps route once and asked Dispatch nothing else.
      expect(
        server.reads.map((url) => url.pathname),
        label
      ).toEqual([STEPS_PATH]);
      expect(stdout, label).toContain(
        [
          "LEG-1: 2 steps in 1 tasks",
          "default 2 step implementer implementation attempt 1 legion-leg-leg-1-implementer anthropic/claude-opus-5 2026-11-16T09:00:00Z 2026-11-16T09:40:00Z passed",
          "default 3 step tester testing attempt 1 legion-leg-leg-1-tester - 2026-11-16T10:00:00Z running running",
        ].join("\n")
      );
    }
  });

  // Only Dispatch's 200 reads as an issue with no step.
  test("an issue with no step exits 0 and prints 0 steps in 0 tasks", async () => {
    const server = dispatchServing({ routes: { [STEPS_PATH]: { issue: "LEG-1", tasks: [] } } });

    const { code, stdout } = await run(server, ["steps", "--issue", "LEG-1"]);

    expect(code).toBe(0);
    expect(stdout).toContain("LEG-1: 0 steps in 0 tasks");
  });

  // Dispatch's own refusal of the read, and a page something in its place answered, are the read's
  // failure: each exits 1 with the refusal. They are a credential Dispatch does not take (401), a
  // fault of its own (500), and a page something in its place answered (502). No GET is
  // project-scoped, so it answers no project's 403; a key Dispatch does not hold is the issue
  // read's 404, the next row.
  test("a refusal of the read exits 1 with the refusal, and prints no steps", async () => {
    /** The run against `server` exits 1, reads the steps route once, and prints `refusal` and no
     *  steps; `label` names the case in each assertion's message. */
    async function refused(server: FakeDispatchServer<unknown>, label: string, refusal: string) {
      const { code, stdout } = await run(server, ["steps", "--issue", "LEG-1"]);

      expect(code, label).toBe(1);
      // The one request was the steps read, and the refusal is its answer.
      expect(
        server.reads.map((url) => url.pathname),
        label
      ).toEqual([STEPS_PATH]);
      expect(stdout, label).toContain(refusal);
      expect(stdout, label).not.toContain("steps in");
    }

    // Dispatch's own refusals print as `messageFor` writes them: `<code>: <error>`.
    for (const [status, code, error] of [
      [401, "UNAUTHORIZED", "invalid bearer token"],
      [500, "INTERNAL", "internal server error"],
    ] as const) {
      const server = dispatchServing({ handler: () => Response.json({ code, error }, { status }) });
      await refused(server, String(status), `${code}: ${error}`);
    }

    // A page something in Dispatch's place answered, named as such with the URL it came from.
    const page = dispatchServing({
      handler: () =>
        new Response("<html><body><h1>502 Bad Gateway</h1></body></html>", {
          status: 502,
          statusText: "Bad Gateway",
          headers: { "Content-Type": "text/html" },
        }),
    });
    await refused(
      page,
      "502 page",
      `HTTP_502: GET ${page.url}${STEPS_PATH} answered 502 Bad Gateway with a body that is not ` +
        `Dispatch's error JSON ("502 Bad Gateway"), which looks like a proxy or gateway page ` +
        "rather than Dispatch's own answer, so a retry may succeed."
    );
  });

  // A key Dispatch does not hold is the issue read's 404 NOT_FOUND on the steps route as on the
  // issue's own, so `dispatch steps` fails for it as `dispatch read` does, and a mistyped key
  // never reads as an issue Legion never ran.
  test("a key Dispatch does not hold fails as dispatch read fails for it, and prints no steps", async () => {
    /** What each command prints and exits with against a Dispatch answering every request as it
     *  answers a read of an issue it does not hold, and the paths the command read. Whoami, which
     *  every read command reads first, answers as a token with no cut (`whoami-stand-in.ts`). */
    async function runUnknown(argv: readonly string[]) {
      const server = dispatchServing({
        handler: (call) =>
          asksWhoami(call.url.pathname)
            ? Response.json(WHOAMI_NO_CUT)
            : Response.json({ code: "NOT_FOUND", error: "not found" }, { status: 404 }),
      });
      const { code, stdout } = await run(server, argv);
      return { code, stdout, reads: server.reads.map((url) => url.pathname) };
    }

    const steps = await runUnknown(["steps", "--issue", "LEG-999"]);
    const read = await runUnknown(["read", "--issue", "LEG-999"]);

    expect(steps.reads).toEqual(["/api/v1/issues/LEG-999/steps"]);
    expect(read.reads).toContain("/api/v1/issues/LEG-999");
    expect(read.code).toBe(1);
    expect(read.stdout).toContain("NOT_FOUND: not found");
    expect(steps.code).toBe(1);
    expect(steps.stdout).toBe(read.stdout);
    expect(steps.stdout).not.toContain("steps in");
  });
});
