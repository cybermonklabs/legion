import { beforeAll, describe, expect, test } from "bun:test";
import { join } from "node:path";
import type { FinishedIssue, PlanRecord, ProjectPlans } from "@legion/contracts";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { bearerCredential } from "../../../packages/envoy-client/src/bearer";
import { executeDispatchTool } from "../../../packages/envoy-client/src/dispatch-execute";
import { fakeDispatchFetch } from "../../../packages/envoy-client/test/fake-dispatch";
import { changed, fakeDispatchRead } from "./fake-dispatch-command";

// step-records-read.sh (the read of a deployed issue's step records after the merge, judged by
// its plan) against the fake `dispatch` of lib/fake-dispatch-command.ts, first on PATH, which
// prints a warning on stderr where a case gives one, as the real command prints its
// `dispatch: <warning>` lines before its answer. The step answers below are the lines `dispatch
// steps` prints (packages/envoy-client/src/dispatch-commands/steps.ts): a header; where the
// records of the issue's newest run name an experiment, the line
// `Experiment <experiment>: variant <variant>.`, and no such line where they name none; then one
// run per line. The plan answers are what `dispatch plans` (dispatch-commands/plans.ts) prints of
// typed finished issues, rendered by that command itself, so a plan line the command prints and
// the script cannot read fails here.
const script = join(import.meta.dir, "..", "step-records-read.sh");
const dir = temporaryDirectory("step-records-read-test");

const project = "INFRA";
const key = "INFRA-42";
const defaultSteps = ["planner", "implementer", "tester", "reviewer", "acceptance:owner"];
const run = (step: string) =>
  `default ${step} legion-infra-infra-42-${step.split(" ")[2]} anthropic/claude-opus-5 2026-11-20T09:00:00Z 2026-11-20T09:40:00Z passed`;
// A whole answer: steps 1 to 5, a review that failed and the runs it sent back, the retro and the
// merge, each ended, each with an agent, each step with a model.
const whole = [
  `${key}: 10 steps in 1 tasks`,
  run("1 step planner planning attempt 1"),
  run("2 step implementer implementation attempt 1"),
  run("3 step tester testing attempt 1"),
  "default 4 step reviewer review attempt 1 legion-infra-infra-42-reviewer openai/gpt-6.1-sol 2026-11-20T10:00:00Z 2026-11-20T10:30:00Z failed",
  run("2 step implementer implementation attempt 2"),
  run("3 step tester testing attempt 2"),
  run("4 step reviewer review attempt 2"),
  run("5 step acceptance review attempt 1"),
  "default 0 retro implementer - attempt 1 legion-infra-infra-42-implementer anthropic/claude-opus-5 2026-11-20T11:00:00Z 2026-11-20T11:10:00Z passed",
  "default 0 merge merger - attempt 1 legion-infra-infra-42-merger - 2026-11-20T11:11:00Z 2026-11-20T11:20:00Z passed",
];
// records is the answer of these runs alone, the header's count following.
function records(lines: string[]): string {
  return `${[`${key}: ${lines.length} steps in 1 tasks`, ...lines].join("\n")}\n`;
}
// without is the whole answer with the lines that match drop taken out.
function without(drop: RegExp): string {
  return records(whole.slice(1).filter((line) => !drop.test(line)));
}

/** A finished issue of the project, closed at noon, whose newest run ran from a plan of these
 *  tasks' steps (the default task of the default steps when they are those), or from none. */
function finished(issue: string, tasks: string[][] | null): FinishedIssue {
  const source = tasks?.length === 1 && tasks[0] === defaultSteps ? "default" : "owner";
  const plan: PlanRecord | null =
    tasks === null
      ? null
      : {
          id: `00000000-0000-5000-8000-${issue.split("-")[1].padStart(12, "0")}`,
          issue,
          generation: 1,
          source,
          artifact_id: null,
          version: null,
          tasks: tasks.map((steps, place) => ({
            id: source === "default" ? "default" : `task-${place + 1}`,
            steps,
          })),
        };
  return {
    issue,
    title: `The work of ${issue}`,
    closed_at: "2026-11-20T12:00:00Z",
    plan,
    ratings: [],
    started_at: null,
    ended_at: null,
    duration_seconds: null,
    failed_steps: 1,
    fixes: 0,
    experiment: null,
    variant: null,
  };
}

/** What `dispatch plans --project INFRA` prints of these finished issues: by default INFRA-42 on
 *  the default steps and INFRA-41, which the read passes over, on another plan. */
async function plans(
  issues: FinishedIssue[] = [finished(key, [defaultSteps]), finished("INFRA-41", [["implementer"]])]
): Promise<string> {
  const answer: ProjectPlans = { project, issues };
  const rendered = await executeDispatchTool({
    tool: "dispatch_plans",
    args: { project },
    cwd: dir,
    host: "omp",
    sessionId: "session-1",
    config: { url: "http://dispatch.test", credential: bearerCredential("secret") },
    env: {},
    exec: async (file, argv) => {
      throw new Error(`unexpected command: ${file} ${argv.join(" ")}`);
    },
    fetchImpl: fakeDispatchFetch({ routes: { [`/api/v1/projects/${project}/plans`]: answer } })
      .fetchImpl,
  });
  return `${rendered.text}\n`;
}

// The plans answer of INFRA-42 on the default steps, rendered once.
let planned = "";
const readOf = fakeDispatchRead(script, dir, [key]);
beforeAll(async () => {
  planned = await plans();
});
// read runs the script with args against the fake dispatch, which answers `dispatch steps` with
// steps and `dispatch plans` with plan (by default INFRA-42 on the default steps), prints err on
// stderr and exits exit, on path, which by default finds the fake first.
const read = (
  steps: string,
  args?: string[],
  exit?: number,
  path?: string,
  err?: string,
  plan = planned
) => readOf({ steps, plans: plan }, args, exit, path, err);

describe("step-records-read.sh", () => {
  test("a whole answer passes, exit 0, read with dispatch steps --issue, then the plan with dispatch plans --project", () => {
    const { code, stdout, calls } = read(`${whole.join("\n")}\n`);
    expect(calls).toBe(`steps --issue ${key}\nplans --project ${project}\n`);
    expect(stdout).toContain(whole[0]);
    // The plan read prints the issue's own line alone, not the other finished issues.
    expect(stdout).toContain(
      `${key} closed 2026-11-20T12:00:00Z plan default (1 tasks: ${defaultSteps.join(" ")})`
    );
    expect(stdout).not.toContain("INFRA-41");
    expect(stdout).toContain(
      `PASS: ${key}: 10 records; steps 1 to 5 of its plan (${defaultSteps.join(" ")}), the retro and the merge each have one, each record ended, each step and retro with an agent, each step with a model`
    );
    expect(code).toBe(0);
  });

  test("a warning the command prints on stderr is no part of its answer: the read passes", () => {
    const warning = "dispatch: the bearer token expires in 2 days\n";
    const { code, stdout, stderr } = read(`${whole.join("\n")}\n`, [key], 0, undefined, warning);
    expect(stdout).toContain(`PASS: ${key}: 10 records`);
    expect(stderr).toContain(warning.trim());
    expect(code).toBe(0);
  });

  // steps.ts ends the line of a run whose record holds a commit with ` at <start>..<end>`: the
  // commits its task's head was at when the run started and when it ended, each cut to ten
  // characters, or `-` for one the record does not hold yet. A fix's line then ends with ` fix of
  // <check>`, the CI check it fixes. Both follow the result and move no column the read judges, so
  // the whole answer with them passes. The control is a line with another word after its result,
  // which is no run.
  test("runs that print their commits, and a fix that names its check, pass, exit 0", () => {
    const committed = changed(whole, /^default /, (line) => {
      if (/ 2 step implementer implementation attempt 2 /.test(line)) {
        return `${line} at a1b2c3d4e5..b2c3d4e5f6 fix of k-ci`;
      }
      if (/ merge merger /.test(line)) return `${line} at b2c3d4e5f6..-`;
      return `${line} at a1b2c3d4e5..a1b2c3d4e5`;
    });
    const passed = read(committed);
    expect(passed.stdout).toContain(`PASS: ${key}: 10 records`);
    expect(passed.stdout).not.toContain("FAIL");
    expect(passed.code).toBe(0);
    const stray = read(
      changed(whole, / 3 step tester testing attempt 2 /, (line) => `${line} again`)
    );
    expect(stray.stdout).toContain(
      "UNKNOWN: the line is no run: default 3 step tester testing attempt 2"
    );
    expect(stray.stdout).not.toContain("PASS");
    expect(stray.code).toBe(2);
  });

  test("the line of the issue's experiment right after the header is no fault: the read passes", () => {
    const variant = "Experiment planning: variant b.";
    const { code, stdout } = read(`${[whole[0], variant, ...whole.slice(1)].join("\n")}\n`);
    expect(stdout).toContain(`${whole[0]}\n${variant}\n`);
    expect(stdout).toContain(`PASS: ${key}: 10 records`);
    expect(stdout).not.toContain("UNKNOWN");
    expect(code).toBe(0);
  });

  test("the same line anywhere but right after the header is no run: UNKNOWN, exit 2", () => {
    const variant = "Experiment planning: variant b.";
    for (const lines of [
      [...whole, variant],
      [whole[0], whole[1], variant, ...whole.slice(2)],
      [whole[0], variant, variant, ...whole.slice(1)],
    ]) {
      const { code, stdout } = read(`${lines.join("\n")}\n`);
      expect(stdout).toContain(`UNKNOWN: the line is no run: ${variant}`);
      expect(stdout).not.toContain("PASS");
      expect(code).toBe(2);
    }
  });

  test("an answer with the merge record missing fails, exit 1, naming it", () => {
    const { code, stdout } = read(without(/ merge merger /));
    expect(stdout).toContain(`FAIL: ${key}: no record of the merge`);
    expect(stdout).not.toContain("PASS");
    expect(code).toBe(1);
  });

  test("an answer with no record at all is UNKNOWN, exit 2, never a pass", () => {
    const { code, stdout } = read(`${key}: 0 steps in 0 tasks\n`);
    expect(stdout).toContain(`UNKNOWN: dispatch steps lists no record of ${key}`);
    expect(stdout).not.toContain("PASS");
    expect(code).toBe(2);
  });

  test("each step of kind step must be there: a missing step 3 fails, exit 1", () => {
    const { code, stdout } = read(without(/ 3 step tester /));
    expect(stdout).toContain(`FAIL: ${key}: no record of step 3 (tester)`);
    expect(code).toBe(1);
  });

  // The steps come from the plan, not from the records: a record lost for the last step leaves
  // no higher step to show the gap, and the read still fails, naming it.
  test("an answer with the record of the plan's last step lost fails, exit 1, naming it", () => {
    const { code, stdout } = read(without(/ 5 step acceptance /));
    expect(stdout).toContain(`FAIL: ${key}: no record of step 5 (acceptance:owner)`);
    expect(stdout).not.toContain("PASS");
    expect(code).toBe(1);
  });

  // A plan in another order than the default steps runs only its own steps: `implementer reviewer`
  // is two steps, and its read passes on steps 1 to 2; without step 1, or without step 2, its
  // last, it fails, naming it.
  test("a plan of two steps passes on its steps 1 to 2, and fails without step 1 or its last", async () => {
    const plan = await plans([finished(key, [["implementer", "reviewer"]])]);
    const two = [
      run("1 step implementer implementation attempt 1"),
      run("2 step reviewer review attempt 1"),
      whole[whole.length - 2],
      whole[whole.length - 1],
    ];
    const passed = read(records(two), [key], 0, undefined, "", plan);
    expect(passed.stdout).toContain(
      `PASS: ${key}: 4 records; steps 1 to 2 of its plan (implementer reviewer), the retro and the merge each have one`
    );
    expect(passed.code).toBe(0);
    for (const [lost, named] of [
      [0, "step 1 (implementer)"],
      [1, "step 2 (reviewer)"],
    ] as const) {
      const kept = two.filter((_, at) => at !== lost);
      const failed = read(records(kept), [key], 0, undefined, "", plan);
      expect(failed.stdout).toContain(`FAIL: ${key}: no record of ${named}`);
      expect(failed.stdout).not.toContain("PASS");
      expect(failed.code).toBe(1);
    }
  });

  test("a step whose records name another role than its plan's step fails, exit 1, naming both", async () => {
    const plan = await plans([finished(key, [["implementer", "reviewer", "tester"]])]);
    const swapped = [
      run("1 step implementer implementation attempt 1"),
      run("2 step tester testing attempt 1"),
      run("3 step reviewer review attempt 1"),
      whole[whole.length - 2],
      whole[whole.length - 1],
    ];
    const { code, stdout } = read(records(swapped), [key], 0, undefined, "", plan);
    expect(stdout).toContain(
      `FAIL: ${key}: no record of step 2 (reviewer): its records name tester`
    );
    expect(stdout).toContain(
      `FAIL: ${key}: no record of step 3 (tester): its records name reviewer`
    );
    expect(stdout).not.toContain("PASS");
    expect(code).toBe(1);
  });

  test("an issue whose plan the read cannot learn is UNKNOWN, exit 2, never a pass", async () => {
    const cases: [FinishedIssue[], string][] = [
      [
        [finished(key, null)],
        `UNKNOWN: Dispatch holds no plan of ${key}, so the steps it ran are unknown`,
      ],
      [
        [finished("INFRA-41", [["implementer"]])],
        `UNKNOWN: dispatch plans lists 1 finished issues of ${project} and no line of ${key}, so it is no issue that is done`,
      ],
      [
        [finished(key, [["implementer"], ["reviewer"]])],
        `UNKNOWN: the plan of ${key} has 2 tasks, and dispatch plans names the steps of the first alone`,
      ],
      [[], `UNKNOWN: dispatch plans lists no finished issue of ${project}, so ${key} is not done`],
    ];
    for (const [issues, said] of cases) {
      const { code, stdout } = read(
        `${whole.join("\n")}\n`,
        [key],
        0,
        undefined,
        "",
        await plans(issues)
      );
      expect(stdout).toContain(said);
      expect(stdout).not.toContain("PASS");
      expect(code).toBe(2);
    }
  });

  test("a plans answer that is no read of the project is UNKNOWN, exit 2", () => {
    const other = read(
      `${whole.join("\n")}\n`,
      [key],
      0,
      undefined,
      "",
      "OTHER: 1 finished issues\n"
    );
    expect(other.stdout).toContain(
      `UNKNOWN: the first line of the plans answer is no header of ${project}: OTHER: 1 finished issues`
    );
    expect(other.code).toBe(2);
    const short = read(
      `${whole.join("\n")}\n`,
      [key],
      0,
      undefined,
      "",
      planned.replace(": 2 finished", ": 3 finished")
    );
    expect(short.stdout).toContain(
      "UNKNOWN: the plans header says 3 finished issues and 2 lines of issues follow it"
    );
    expect(short.code).toBe(2);
  });

  test("a record still running fails, exit 1, naming its line", () => {
    const { code, stdout } = read(
      changed(whole, / 5 step acceptance /, (line) =>
        line.replace(/ \S+ passed$/, " running running")
      )
    );
    expect(stdout).toContain(
      "FAIL: the record never ended: default 5 step acceptance review attempt 1"
    );
    expect(code).toBe(1);
  });

  test("an end that is no moment, `-` or any word, fails, exit 1: only a moment ends a record", () => {
    // steps.ts prints `running` there for a run that has not ended; a printer that wrote the end
    // through orDash would print `-`, and another a word of its own: none of them is an end.
    for (const end of ["-", "unknown"]) {
      const { code, stdout } = read(
        changed(whole, / 2 step implementer implementation attempt 2 /, (line) =>
          line.replace(/ \S+ passed$/, ` ${end} passed`)
        )
      );
      expect(stdout).toContain(
        `FAIL: the record never ended: default 2 step implementer implementation attempt 2 legion-infra-infra-42-implementer anthropic/claude-opus-5 2026-11-20T09:00:00Z ${end} passed`
      );
      expect(stdout).not.toContain("PASS");
      expect(code).toBe(1);
    }
  });

  test("a retro with no agent fails, exit 1, naming it", () => {
    const { code, stdout } = read(
      changed(whole, / retro implementer /, (line) =>
        line.replace("legion-infra-infra-42-implementer", "-")
      )
    );
    expect(stdout).toContain("FAIL: the record names no agent: default 0 retro implementer");
    expect(code).toBe(1);
  });

  test("a step with no agent fails, exit 1, naming it", () => {
    const { code, stdout } = read(
      changed(whole, / 3 step tester testing attempt 2 /, (line) =>
        line.replace("legion-infra-infra-42-tester", "-")
      )
    );
    expect(stdout).toContain(
      "FAIL: the record names no agent: default 3 step tester testing attempt 2 - anthropic/claude-opus-5"
    );
    expect(code).toBe(1);
  });

  test("a merge with no agent passes, with a line that says no merger ran it", () => {
    const { code, stdout } = read(
      changed(whole, / merge merger /, (line) => line.replace("legion-infra-infra-42-merger", "-"))
    );
    expect(stdout).toContain(
      "the merge has no agent: no merger ran it: default 0 merge merger - attempt 1 - -"
    );
    expect(stdout).not.toContain("FAIL");
    expect(stdout).toContain(`PASS: ${key}: 10 records`);
    expect(code).toBe(0);
  });

  test("a merge with an agent is checked as every record is: one never ended fails, exit 1", () => {
    const { code, stdout } = read(
      changed(whole, / merge merger /, (line) => line.replace(/ \S+ passed$/, " running running"))
    );
    expect(stdout).toContain("FAIL: the record never ended: default 0 merge merger");
    expect(stdout).not.toContain("the merge has no agent");
    expect(code).toBe(1);
  });

  test("a merge with no agent that never ended fails, exit 1: no agent waives no end", () => {
    const { code, stdout } = read(
      changed(whole, / merge merger /, (line) =>
        line
          .replace("legion-infra-infra-42-merger", "-")
          .replace(/ \S+ passed$/, " running running")
      )
    );
    expect(stdout).toContain(
      "FAIL: the record never ended: default 0 merge merger - attempt 1 - - 2026-11-20T11:11:00Z running running"
    );
    expect(stdout).not.toContain("PASS");
    expect(code).toBe(1);
  });

  test("a step of kind step with no model fails, exit 1, naming it", () => {
    const { code, stdout } = read(
      changed(whole, / 1 step planner /, (line) => line.replace("anthropic/claude-opus-5", "-"))
    );
    expect(stdout).toContain("FAIL: the step names no model: default 1 step planner planning");
    expect(code).toBe(1);
  });

  test("a retro with no model passes, exit 0: only a record of kind step must name one", () => {
    const { code, stdout } = read(
      changed(whole, / retro implementer /, (line) => line.replace("anthropic/claude-opus-5", "-"))
    );
    expect(stdout).toContain(
      "default 0 retro implementer - attempt 1 legion-infra-infra-42-implementer - "
    );
    expect(stdout).not.toContain("FAIL");
    expect(stdout).toContain(`PASS: ${key}: 10 records`);
    expect(code).toBe(0);
  });

  test("a read that fails is UNKNOWN, exit 2, with what the command said", () => {
    const { code, stdout } = read(
      "Dispatch at https://dispatch.example is unreachable: ECONNREFUSED\n",
      [key],
      1
    );
    expect(stdout).toContain(
      `UNKNOWN: dispatch steps --issue ${key} exited 1, so nothing was read`
    );
    expect(stdout).toContain("is unreachable: ECONNREFUSED");
    expect(code).toBe(2);
  });

  test("a failed read's message keeps what it said on stderr and on stdout on lines of their own", () => {
    const warning = "dispatch: the bearer token expires in 2 days\n";
    const { code, stdout } = read(`${key}: 7 steps in 1 tasks\n`, [key], 1, undefined, warning);
    expect(stdout).toContain(`exited 1, so nothing was read: ${warning}${key}: 7 steps in 1 tasks`);
    expect(stdout).not.toContain(`days${key}`);
    expect(code).toBe(2);
  });

  test("an answer that is no read of the issue is UNKNOWN, exit 2", () => {
    const header = read("OTHER-1: 10 steps in 1 tasks\n");
    expect(header.stdout).toContain(
      "UNKNOWN: the first line of the answer is no header of INFRA-42"
    );
    expect(header.code).toBe(2);
    const short = read(`${key}: 11 steps in 1 tasks\n${whole.slice(1).join("\n")}\n`);
    expect(short.stdout).toContain(
      "UNKNOWN: the header says 11 records and 10 lines of runs follow it"
    );
    expect(short.code).toBe(2);
  });

  test("an answer with a line that is no run is UNKNOWN, exit 2, even when the count holds", () => {
    const { code, stdout } = read(
      `${key}: 11 steps in 1 tasks\n${whole.slice(1).join("\n")}\nwarning: the server is slow\n`
    );
    expect(stdout).toContain("UNKNOWN: the line is no run: warning: the server is slow");
    expect(stdout).not.toContain("PASS");
    expect(code).toBe(2);
  });

  test("no dispatch command on PATH is UNKNOWN, exit 2, naming what is missing", () => {
    // The PATH the script gets holds no directory: bash is named by the path read found it at.
    const { code, stdout } = read("", [key], 0, join(dir, "no-such-directory"));
    expect(stdout).toContain("UNKNOWN: no dispatch command is on PATH, so nothing was read");
    expect(code).toBe(2);
  });

  test("no issue, two issues or a malformed key is refused, exit 2, reading nothing", () => {
    for (const args of [[], [key, "INFRA-43"], ["infra-42"]]) {
      const { code, stderr, calls } = read(`${whole.join("\n")}\n`, args);
      expect(stderr).toContain("usage: scripts/e2e/step-records-read.sh <ISSUE>");
      expect(calls).toBe("");
      expect(code).toBe(2);
    }
  });
});
