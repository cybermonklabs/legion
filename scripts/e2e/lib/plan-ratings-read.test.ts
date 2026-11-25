import { beforeAll, describe, expect, test } from "bun:test";
import { join } from "node:path";
import type { FinishedIssue, PlanRecord, ProjectPlans, SetModelSteps } from "@legion/contracts";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { bearerCredential } from "../../../packages/envoy-client/src/bearer";
import { executeDispatchTool } from "../../../packages/envoy-client/src/dispatch-execute";
import { fakeDispatchFetch } from "../../../packages/envoy-client/test/fake-dispatch";
import { changed, fakeDispatchRead } from "./fake-dispatch-command";

// plan-ratings-read.sh (the read of a deployed issue's plan and ratings after the merge) against
// the fake `dispatch` of lib/fake-dispatch-command.ts, first on PATH. The whole answer below is
// what `dispatch plans` (packages/envoy-client/src/dispatch-commands/plans.ts) prints of typed
// finished issues, rendered by that command itself, so a line the command prints and the script
// cannot read fails here: a header, then one finished issue per line, newest close first, the line
// of an issue in an experiment's group ending with ` variant <experiment>/<variant>`. Bun
// drops the answer's types when it runs this file, so the typecheck of scripts/e2e/lib, whose
// tsconfig.json includes it, holds the answer to the contract's types.
const script = join(import.meta.dir, "..", "plan-ratings-read.sh");
const dir = temporaryDirectory("plan-ratings-read-test");

const project = "INFRA";
const key = "INFRA-42";
const defaultSteps = ["planner", "implementer", "tester", "reviewer", "acceptance:owner"];
const plan = `plan default (1 tasks: ${defaultSteps.join(" ")})`;
const owner = "plan owner (2 tasks: implementer reviewer)";

/**
 * The id of a thing of issue in the form Dispatch takes for a plan, a plan's document and a step
 * record, a uuid (here of version 5): the issue's number and which thing it is (1 the plan, 2 its
 * document, 10 and up its retros) in the last group, so no two things of the answer share one.
 */
function uuidOf(issue: string, which: number): string {
  const number = Number(issue.split("-")[1]);
  return `00000000-0000-5000-8000-${String(number * 100 + which).padStart(12, "0")}`;
}

/** The plan of issue's first run: its owner's, of these tasks' steps, or the default one. */
function planOf(issue: string, tasks?: string[][]): PlanRecord {
  const id = uuidOf(issue, 1);
  return tasks === undefined
    ? {
        id,
        issue,
        generation: 1,
        source: "default",
        artifact_id: null,
        version: null,
        tasks: [{ id: "default", steps: defaultSteps }],
      }
    : {
        id,
        issue,
        generation: 1,
        source: "owner",
        artifact_id: uuidOf(issue, 2),
        version: 2,
        tasks: tasks.map((steps, place) => ({ id: `task-${place + 1}`, steps })),
      };
}

/**
 * A finished issue as Dispatch lists it: what the script reads of it, and the rest an answer
 * Dispatch can give. Its records end a minute before its close and start seconds before that
 * (duration_seconds is the whole seconds from started_at to ended_at, as Dispatch reckons it), and
 * each retro ends inside that span, a minute after the one before. An issue of unknown time
 * (seconds null) has no start and no end. counts are its records counted against their set model.
 */
function finished(
  issue: string,
  closed: string,
  plan: PlanRecord | null,
  ratings: [number, string][],
  seconds: number | null,
  failed: number,
  counts: SetModelSteps
): FinishedIssue {
  const end = Date.parse(closed) - 60_000;
  const start = seconds === null ? null : end - seconds * 1000;
  const at = (moment: number) => new Date(moment).toISOString();
  return {
    issue,
    title: `The work of ${issue}`,
    closed_at: closed,
    plan,
    ratings: ratings.map(([rating, reason], place) => ({
      rating,
      reason,
      step_id: uuidOf(issue, 10 + place),
      ended_at: start === null ? null : at(start + (place + 1) * 60_000),
    })),
    started_at: start === null ? null : at(start),
    ended_at: start === null ? null : at(end),
    duration_seconds: seconds,
    failed_steps: failed,
    set_model_steps: counts,
    experiment: null,
    variant: null,
  };
}

// A whole answer: an issue its owner planned in two tasks, rated once; an issue rated twice whose
// second reason holds a quotation mark and a line break; the issue rated once; an issue its owner
// planned in one task, unrated; and an issue Legion never ran, or ran before the release that
// records each step, which lists no plan, no rating and no time.
const answer: ProjectPlans = {
  project,
  issues: [
    finished(
      "INFRA-44",
      "2026-11-20T13:00:00Z",
      planOf("INFRA-44", [["implementer", "reviewer"], ["implementer:docs"]]),
      [[3, "Two tasks were one too many."]],
      3720,
      0,
      { on: 5, off: 0, unknown: 1 }
    ),
    finished(
      "INFRA-43",
      "2026-11-20T12:00:00Z",
      planOf("INFRA-43"),
      [
        [2, "The plan had no test step."],
        [5, 'It said "ship it"\nand it shipped.'],
      ],
      3300,
      0,
      { on: 6, off: 1, unknown: 0 }
    ),
    finished(
      key,
      "2026-11-20T11:20:00.123456Z",
      planOf(key),
      [[4, "The plan fit the work; the review sent it back once."]],
      8400,
      1,
      { on: 7, off: 1, unknown: 2 }
    ),
    finished(
      "INFRA-30",
      "2026-11-19T10:00:00Z",
      planOf("INFRA-30", [["implementer"]]),
      [],
      300,
      0,
      { on: 3, off: 0, unknown: 0 }
    ),
    finished("INFRA-7", "2026-11-01T08:00:00Z", null, [], null, 0, { on: 0, off: 0, unknown: 0 }),
  ],
};

// The same answer with the issue judged and INFRA-44 drawn into the experiment planning's group,
// at its variants b and a, so `dispatch plans` ends the line of each with
// ` variant planning/<variant>`; the other three are in no experiment's group.
const drawn = new Map([
  [key, "b"],
  ["INFRA-44", "a"],
]);
const groupedAnswer: ProjectPlans = {
  project,
  issues: (answer.issues ?? []).map((issue) => {
    const variant = drawn.get(issue.issue);
    return variant === undefined ? issue : { ...issue, experiment: "planning", variant };
  }),
};

/** The whole answer's lines as `dispatch plans` printed them, the line of the issue judged, and the
 *  grouped answer's lines. */
let whole: string[] = [];
let line42 = "";
let grouped: string[] = [];
// read runs the script with args against the fake dispatch, which prints err on stderr and out on
// stdout and exits exit, on path, which by default finds the fake first.
const read = fakeDispatchRead(script, dir, [project, key]);
/** plans as `dispatch plans` prints them: the command itself renders them, one line each. */
async function render(plans: ProjectPlans): Promise<string[]> {
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
    fetchImpl: fakeDispatchFetch({ routes: { [`/api/v1/projects/${project}/plans`]: plans } })
      .fetchImpl,
  });
  return rendered.text.split("\n");
}
beforeAll(async () => {
  [whole, grouped] = await Promise.all([render(answer), render(groupedAnswer)]);
  line42 = whole.find((line) => line.startsWith(`${key} `)) ?? "";
});

/**
 * lines as the fake `dispatch` prints them, one per line and newline-ended: the whole answer by
 * default.
 */
function printed(lines: string[] = whole): string {
  return `${lines.join("\n")}\n`;
}

describe("plan-ratings-read.sh", () => {
  test("a whole answer passes, exit 0, read once with dispatch plans --project, naming the rating and its reason", () => {
    const { code, stdout, calls } = read(printed());
    expect(calls).toBe(`plans --project ${project}\n`);
    expect(stdout).toContain(whole[0]);
    // The printed line ends in the issue's step records counted against their set model.
    expect(line42).toEndWith(" time 2h20m failed steps 1 on set model 7 off 1 unknown 2");
    expect(stdout).toContain(
      `PASS: ${key}: ${plan}, ratings 4/5 "The plan fit the work; the review sent it back once.", time 2h20m, failed steps 1`
    );
    expect(code).toBe(0);
  });

  test("a warning the command prints on stderr is no part of its answer: the read passes", () => {
    const warning = "dispatch: the bearer token expires in 2 days\n";
    const { code, stdout, stderr } = read(printed(), [project, key], 0, undefined, warning);
    expect(stdout).toContain(`PASS: ${key}: ${plan}, ratings 4/5`);
    expect(stderr).toContain(warning.trim());
    expect(code).toBe(0);
  });

  // A Dispatch before the three counts sends none, and the command prints `-` for each.
  test("a line with no count from a Dispatch before the counts passes, exit 0", () => {
    const { code, stdout } = read(
      changed(whole, /^INFRA-42 /, (line) =>
        line.replace(/ on set model 7 off 1 unknown 2$/, " on set model - off - unknown -")
      )
    );
    expect(stdout).toContain(" failed steps 1 on set model - off - unknown -\n");
    expect(stdout).toContain(`PASS: ${key}: ${plan}, ratings 4/5`);
    expect(code).toBe(0);
  });

  // The command before the counts ended each line at its failed steps. A read of such a line
  // would read no count, so it is no finished issue.
  test("a line of the command before the counts, ending at its failed steps, is UNKNOWN, exit 2, naming it", () => {
    const old = line42.replace(/ on set model \S+ off \S+ unknown \S+$/, "");
    const { code, stdout } = read(changed(whole, /^INFRA-42 /, () => old));
    expect(old).toEndWith(" failed steps 1");
    expect(stdout).toContain(`UNKNOWN: the line is no finished issue: ${old}\n`);
    expect(stdout).not.toContain("PASS");
    expect(code).toBe(2);
  });

  // Each count is a whole number, or `-` from a Dispatch before the counts, after its own words.
  test("counts not as the command prints them are no finished issue: UNKNOWN, exit 2, naming the line", () => {
    for (const [why, counts] of [
      ["no third count", "on set model 7 off 1"],
      ["a count in words", "on set model seven off 1 unknown 2"],
      ["a word after the counts", "on set model 7 off 1 unknown 2 more"],
      ["the words of a count misspelt", "on model 7 off 1 unknown 2"],
    ]) {
      const line = line42.replace(/on set model 7 off 1 unknown 2$/, counts);
      const { code, stdout } = read(changed(whole, /^INFRA-42 /, () => line));
      expect(stdout, why).toContain(`UNKNOWN: the line is no finished issue: ${line}\n`);
      expect(stdout, why).not.toContain("PASS");
      expect(code, why).toBe(2);
    }
  });

  test("an issue rated twice, a reason with a quotation mark and a line break among them, passes", () => {
    const { code, stdout } = read(printed(), [project, "INFRA-43"]);
    expect(stdout).toContain(
      `PASS: INFRA-43: ${plan}, ratings 2/5 "The plan had no test step.", 5/5 "It said \\"ship it\\"\\nand it shipped.", time 0h55m, failed steps 0`
    );
    expect(code).toBe(0);
  });

  test("an issue its owner planned in two tasks passes, exit 0, naming the owner's plan", () => {
    const { code, stdout } = read(printed(), [project, "INFRA-44"]);
    expect(stdout).toContain(
      `PASS: INFRA-44: ${owner}, ratings 3/5 "Two tasks were one too many.", time 1h2m, failed steps 0`
    );
    expect(code).toBe(0);
  });

  test("an issue with no plan and no rating fails, exit 1, one line per fault, and its unknown time is none of them", () => {
    const { code, stdout } = read(printed(), [project, "INFRA-7"]);
    expect(stdout).toContain("FAIL: INFRA-7 lists no plan: INFRA-7 closed");
    expect(stdout).toContain("FAIL: INFRA-7 lists no rating: INFRA-7 closed");
    expect(stdout.split("\n").filter((line) => line.startsWith("FAIL: "))).toHaveLength(2);
    expect(stdout).not.toContain("PASS");
    expect(code).toBe(1);
  });

  test("an issue drawn into an experiment's group passes, exit 0, naming its variant, and the issues of no group still read", () => {
    expect(grouped.find((line) => line.startsWith(`${key} `))).toEndWith(
      " failed steps 1 variant planning/b"
    );
    const judged = read(printed(grouped));
    expect(judged.stdout).toContain(
      `PASS: ${key}: ${plan}, ratings 4/5 "The plan fit the work; the review sent it back once.", time 2h20m, failed steps 1, variant planning/b`
    );
    expect(judged.code).toBe(0);
    const other = read(printed(grouped), [project, "INFRA-43"]);
    expect(other.stdout).toContain(
      `PASS: INFRA-43: ${plan}, ratings 2/5 "The plan had no test step.", 5/5 "It said \\"ship it\\"\\nand it shipped.", time 0h55m, failed steps 0\n`
    );
    expect(other.code).toBe(0);
  });

  test("a line whose end after its failed steps is no variant of an experiment is no finished issue: UNKNOWN, exit 2", () => {
    for (const end of [
      " variant planning",
      " variant planning/b now",
      " variant Planning/b",
      " planning/b",
    ]) {
      const { code, stdout } = read(
        changed(grouped, /^INFRA-42 /, (line) => line.replace(/ variant planning\/b$/, end))
      );
      expect(stdout).toContain(`UNKNOWN: the line is no finished issue: ${key} closed`);
      expect(stdout).not.toContain("PASS");
      expect(code).toBe(2);
    }
  });

  test("a plan with no rating fails, exit 1, naming it", () => {
    const { code, stdout } = read(
      changed(whole, /^INFRA-42 /, (line) => line.replace(/ratings .* time/, "ratings none time"))
    );
    expect(stdout).toContain(`FAIL: ${key} lists no rating: ${key} closed`);
    expect(stdout).not.toContain("lists no plan");
    expect(stdout).not.toContain("PASS");
    expect(code).toBe(1);
  });

  test("a rating with an empty reason fails, exit 1, naming it", () => {
    const { code, stdout } = read(
      changed(whole, /^INFRA-42 /, (line) => line.replace(/4\/5 "[^"]*"/, '4/5 ""'))
    );
    expect(stdout).toContain(`FAIL: ${key} lists a rating with no reason: ${key} closed`);
    expect(code).toBe(1);
  });

  // `dispatch plans` prints `time -` for an issue whose time Dispatch does not know: no record has
  // a start, none has an end, or the latest end is before the earliest start (a run whose start
  // never reached Dispatch, and a later one whose end has not yet). That says nothing of the plan
  // or its ratings, so the issue is judged by those, and the verdict says its time is unknown.
  test("a plan and a rating whose time Dispatch does not know pass, exit 0, saying the time is unknown", () => {
    const { code, stdout } = read(
      changed(whole, /^INFRA-42 /, (line) => line.replace("time 2h20m", "time -"))
    );
    expect(stdout).toContain(
      `PASS: ${key}: ${plan}, ratings 4/5 "The plan fit the work; the review sent it back once.", time unknown, failed steps 1`
    );
    expect(stdout).not.toContain("FAIL");
    expect(code).toBe(0);
  });

  test("a read that lists finished issues and not the issue fails, exit 1, naming it", () => {
    const lines = whole.slice(1).filter((line) => !line.startsWith(`${key} `));
    const { code, stdout } = read(
      printed([`${project}: ${lines.length} finished issues`, ...lines])
    );
    expect(stdout).toContain(
      `FAIL: the read lists 4 finished issues of ${project} and no line of ${key}`
    );
    expect(stdout).not.toContain("PASS");
    expect(code).toBe(1);
  });

  test("a read that lists the issue twice, its header counting both, fails, exit 1, naming it", () => {
    const lines = [...whole.slice(1), line42];
    const { code, stdout } = read(
      printed([`${project}: ${lines.length} finished issues`, ...lines])
    );
    expect(stdout).not.toContain("UNKNOWN");
    expect(stdout).toContain(`FAIL: the read lists ${key} 2 times`);
    expect(stdout).not.toContain("PASS");
    expect(code).toBe(1);
  });

  test("a read that lists no finished issue at all is UNKNOWN, exit 2, never a pass", () => {
    const { code, stdout } = read(`${project}: 0 finished issues\n`);
    expect(stdout).toContain(`UNKNOWN: dispatch plans lists no finished issue of ${project}`);
    expect(stdout).not.toContain("PASS");
    expect(code).toBe(2);
  });

  test("a read that fails is UNKNOWN, exit 2, with what the command said", () => {
    const { code, stdout } = read(
      "Dispatch at https://dispatch.example is unreachable: ECONNREFUSED\n",
      [project, key],
      1
    );
    expect(stdout).toContain(
      `UNKNOWN: dispatch plans --project ${project} exited 1, so nothing was read`
    );
    expect(stdout).toContain("is unreachable: ECONNREFUSED");
    expect(code).toBe(2);
  });

  test("a failed read's message keeps what it said on stderr and on stdout on lines of their own", () => {
    const warning = "dispatch: the bearer token expires in 2 days\n";
    const { code, stdout } = read(printed([whole[0]]), [project, key], 1, undefined, warning);
    expect(stdout).toContain(`exited 1, so nothing was read: ${warning}${whole[0]}`);
    expect(stdout).not.toContain(`days${whole[0]}`);
    expect(code).toBe(2);
  });

  test("an answer that is no read of the project is UNKNOWN, exit 2", () => {
    const header = read(printed(["OTHER: 3 finished issues", ...whole.slice(1)]));
    expect(header.stdout).toContain(
      `UNKNOWN: the first line of the answer is no header of ${project}`
    );
    expect(header.code).toBe(2);
    const short = read(printed([`${project}: 6 finished issues`, ...whole.slice(1)]));
    expect(short.stdout).toContain(
      "UNKNOWN: the header says 6 finished issues and 5 lines of issues follow it"
    );
    expect(short.code).toBe(2);
    const torn = read(
      changed(whole, /^INFRA-43 /, (line) => line.replace(/, 5\/5 .*" time/, ', 5/5 "torn time'))
    );
    expect(torn.stdout).toContain("UNKNOWN: the line is no finished issue: INFRA-43 closed");
    expect(torn.stdout).not.toContain("PASS");
    expect(torn.code).toBe(2);
  });

  test("no dispatch command on PATH is UNKNOWN, exit 2, naming what is missing", () => {
    // The PATH the script gets holds no directory: bash is named by the path read found it at.
    const { code, stdout } = read("", [project, key], 0, join(dir, "no-such-directory"));
    expect(stdout).toContain("UNKNOWN: no dispatch command is on PATH, so nothing was read");
    expect(code).toBe(2);
  });

  test("a wrong count of arguments, a malformed key or an issue of another project is refused, exit 2, reading nothing", () => {
    for (const args of [
      [],
      [project],
      [project, key, "INFRA-43"],
      ["infra", key],
      [project, "infra-42"],
      [project, "OTHER-42"],
    ]) {
      const { code, stderr, calls } = read(printed(), args);
      expect(stderr).toContain("usage: scripts/e2e/plan-ratings-read.sh <PROJECT> <ISSUE>");
      expect(calls).toBe("");
      expect(code).toBe(2);
    }
  });
});
