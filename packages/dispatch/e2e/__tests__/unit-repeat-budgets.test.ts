/** The budgets of .github/workflows/dispatch-unit-repeat.yaml against the measurements beside
 *  them. Each lane's matrix row writes its slowest test step and its slowest other steps, each
 *  read from CI's own runs, and three budgets set from them by one rule, which
 *  .github/ci-notes/dispatch-unit-repeat.md ("The budgets measured from CI's own runs") states:
 *
 *  - `deadline_seconds`: twice `slowest_seconds`, up to the next whole minute;
 *  - `step_minutes`: the deadline, the 60 s of `timeout --kill-after` and 60 s of slack, in whole
 *    minutes, up;
 *  - `job_minutes`: `step_minutes`, and twice `other_steps_seconds`, in whole minutes, up.
 *
 *  This file reads no Bun run: it parses the workflow and holds each budget to exactly its rule,
 *  so a budget raised or cut with no new measurement fails, and so does a measurement written
 *  with no budget set again from it. The row says which lie of a weekly result it catches.
 */

import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

import { rowLimit } from "./bounded-subprocess";

/** What the budget check reads of one leg of the matrix. */
interface MeasuredLeg {
  readonly lane: string;
  readonly slowest: number;
  readonly otherSteps: number;
  readonly deadline: number;
  readonly stepMinutes: number;
  readonly jobMinutes: number;
}

/** The margin of every budget over what it was measured at: twice the slowest run. */
const MARGIN = 2;
/** The seconds the step's `timeout --kill-after` gives the tool to report after the deadline. */
const KILL_AFTER = 60;
/** The seconds the step's budget keeps past the kill-after, for the step's own start. It sits
 *  behind the deadline and the kill-after, so it decides no verdict. */
const STEP_SLACK = 60;

/** The rule a budget problem breaks: the workflow's shape, a lane's matrix row that does not
 *  read as numbers, or one of the three budget rules of a lane. */
type BudgetRule =
  | "no-repeat-job"
  | "job-timeout"
  | "step-timeout"
  | "step-run"
  | "unread"
  | "deadline"
  | "step"
  | "job";

/** One way the budgets miss their rule: the lane whose row it is in, or null for the job's and
 *  the step's own shape, the rule it breaks, and the words a reader gets. */
interface BudgetProblem {
  readonly lane: string | null;
  readonly rule: BudgetRule;
  readonly text: string;
}

/** One matrix row or one step of the workflow, as YAML parses it. */
type WorkflowEntry = Record<string, unknown>;

/** The parts of dispatch-unit-repeat.yaml the budget check reads, as YAML parses them. A part the
 *  file lacks reads as undefined, and the check names it as a problem. The objects are the
 *  parsed ones, not copies, so a control's change to one reaches the workflow it checks. */
interface RepeatWorkflow {
  jobs?: {
    repeat?: {
      "timeout-minutes"?: unknown;
      strategy?: { matrix?: { include?: unknown } };
      steps?: unknown;
    };
  };
}

/** The repeat job's matrix rows. */
function matrixRows(workflow: RepeatWorkflow | null): WorkflowEntry[] {
  const include = workflow?.jobs?.repeat?.strategy?.matrix?.include;
  return Array.isArray(include) ? include : [];
}

/** The repeat job's test step. */
function testStep(workflow: RepeatWorkflow | null): WorkflowEntry | undefined {
  const steps = workflow?.jobs?.repeat?.steps;
  return Array.isArray(steps)
    ? steps.find(
        (each: WorkflowEntry) =>
          typeof each?.name === "string" && each.name.startsWith("Run the lane")
      )
    : undefined;
}

/** What the test step's `run:` holds: test:repeat under `timeout` at the matrix's deadline. */
const DEADLINE_RUN = `timeout --foreground --signal=TERM --kill-after=${KILL_AFTER} \${{ matrix.deadline_seconds }} bun run test:repeat `;

/** Each budget of `leg` by its rule, from the value the rule takes as it is written: the deadline
 *  from the slowest leg, the step from the deadline, the job from the step and the other steps. */
function byRule(leg: MeasuredLeg): { deadline: number; stepMinutes: number; jobMinutes: number } {
  return {
    deadline: Math.ceil((MARGIN * leg.slowest) / 60) * 60,
    stepMinutes: Math.ceil((leg.deadline + KILL_AFTER + STEP_SLACK) / 60),
    jobMinutes: Math.ceil((leg.stepMinutes * 60 + MARGIN * leg.otherSteps) / 60),
  };
}

/** Every way the budgets of a parsed workflow miss their rule over the measurements beside them,
 *  and the legs it read. A list of no problem over no leg is a check that read nothing, which the
 *  caller refuses. */
function budgetProblems(workflow: RepeatWorkflow | null): {
  problems: BudgetProblem[];
  legs: MeasuredLeg[];
} {
  const problems: BudgetProblem[] = [];
  const legs: MeasuredLeg[] = [];
  const job = workflow?.jobs?.repeat;
  if (job === undefined) {
    problems.push({
      lane: null,
      rule: "no-repeat-job",
      text: "the workflow has no job named repeat",
    });
    return { problems, legs };
  }
  if (job["timeout-minutes"] !== `\${{ matrix.job_minutes }}`) {
    problems.push({
      lane: null,
      rule: "job-timeout",
      text: "the job's timeout-minutes is not the matrix's job_minutes",
    });
  }
  const step = testStep(workflow);
  if (step?.["timeout-minutes"] !== `\${{ matrix.step_minutes }}`) {
    problems.push({
      lane: null,
      rule: "step-timeout",
      text: "the test step's timeout-minutes is not the matrix's step_minutes",
    });
  }
  if (typeof step?.run !== "string" || !step.run.includes(DEADLINE_RUN)) {
    problems.push({
      lane: null,
      rule: "step-run",
      text: `the test step does not run test:repeat under timeout at the matrix's deadline_seconds with --kill-after=${KILL_AFTER}`,
    });
  }
  for (const row of matrixRows(workflow)) {
    const number = (key: string) => {
      const value = row[key];
      return typeof value === "number" ? value : Number.NaN;
    };
    const leg = {
      lane: String(row.lane),
      slowest: number("slowest_seconds"),
      otherSteps: number("other_steps_seconds"),
      deadline: number("deadline_seconds"),
      stepMinutes: number("step_minutes"),
      jobMinutes: number("job_minutes"),
    };
    legs.push(leg);
    const { lane } = leg;
    if (Object.values(leg).some((value) => Number.isNaN(value))) {
      problems.push({
        lane,
        rule: "unread",
        text: `${lane}: a measurement or a budget is missing or is not a number`,
      });
      continue;
    }
    const rule = byRule(leg);
    if (leg.deadline !== rule.deadline) {
      problems.push({
        lane,
        rule: "deadline",
        text: `${lane}: deadline_seconds ${leg.deadline} is not ${MARGIN} x the slowest ${leg.slowest} s up to the next whole minute, ${rule.deadline}`,
      });
    }
    if (leg.stepMinutes !== rule.stepMinutes) {
      problems.push({
        lane,
        rule: "step",
        text: `${lane}: step_minutes ${leg.stepMinutes} is not the deadline, the ${KILL_AFTER} s kill-after and ${STEP_SLACK} s up to the next whole minute, ${rule.stepMinutes}`,
      });
    }
    if (leg.jobMinutes !== rule.jobMinutes) {
      problems.push({
        lane,
        rule: "job",
        text: `${lane}: job_minutes ${leg.jobMinutes} is not step_minutes and ${MARGIN} x the other steps' ${leg.otherSteps} s up to the next whole minute, ${rule.jobMinutes}`,
      });
    }
  }
  return { problems, legs };
}

test(
  "each budget of the repeat workflow is exactly its rule over the measurement beside it",
  () => {
    // The lie: a budget set below what the leg takes on main's suite ends a leg that was passing,
    // as the 16-minute web and the two-minute web-cold budgets did; or a budget raised with no
    // new measurement hides a leg that has grown. The workflow writes each lane's measurement
    // beside its budgets, and .github/ci-notes/dispatch-unit-repeat.md says where each was read.
    // The repository's own file, read as the parts the check reads: a part it lacks reads as
    // undefined, which the check names.
    const workflow = Bun.YAML.parse(
      readFileSync(
        fileURLToPath(
          new URL("../../../../.github/workflows/dispatch-unit-repeat.yaml", import.meta.url)
        ),
        "utf8"
      )
    ) as RepeatWorkflow | null;
    const { problems, legs } = budgetProblems(workflow);
    expect(legs.map((leg) => leg.lane)).toEqual(["web", "web-cold", "harness"]);
    expect(problems).toEqual([]);

    // The controls. Each plants one fault in a parsed copy of the workflow, and reads its lane and
    // rules by name. A budget one step above and one below its rule, worked out from that lane's
    // own numbers, fails its rule, and the rule of the budget set from it. A measurement that a
    // new reading moves past what its budgets were set from fails the budget set from it. So this
    // row repeats no measured number, and a new measurement of the workflow changes no line of it.
    const broken = (change: (copy: RepeatWorkflow | null) => void) => {
      const copy = structuredClone(workflow);
      change(copy);
      return budgetProblems(copy).problems.map(({ lane, rule }) => ({ lane, rule }));
    };
    const inRow = (lane: string, change: (row: WorkflowEntry, leg: MeasuredLeg) => void) =>
      broken((copy) => {
        const row = matrixRows(copy).find((each) => each.lane === lane);
        const leg = legs.find((each) => each.lane === lane);
        if (row === undefined || leg === undefined) throw new Error(`no matrix row for ${lane}`);
        change(row, leg);
      });
    for (const { lane } of legs) {
      const at = (key: string, value: (leg: MeasuredLeg) => number) =>
        inRow(lane, (row, leg) => {
          row[key] = value(leg);
        });
      const fails = (...rules: BudgetRule[]) => rules.map((rule) => ({ lane, rule }));
      // The deadline a minute under or over twice the slowest: the deadline rule, and the step
      // set from that deadline.
      expect(at("deadline_seconds", (leg) => leg.deadline - 60)).toEqual(fails("deadline", "step"));
      expect(at("deadline_seconds", (leg) => leg.deadline + 60)).toEqual(fails("deadline", "step"));
      // A slowest leg a new reading puts past half the deadline, or below what the deadline was
      // set from: the deadline rule alone. The bounds of the slowest the deadline holds pass.
      const slowestCeiling = (leg: MeasuredLeg) => Math.floor(leg.deadline / MARGIN);
      expect(at("slowest_seconds", (leg) => slowestCeiling(leg) + 1)).toEqual(fails("deadline"));
      expect(at("slowest_seconds", slowestCeiling)).toEqual([]);
      const slowestFloor = (leg: MeasuredLeg) => Math.floor((leg.deadline - 60) / MARGIN) + 1;
      expect(at("slowest_seconds", (leg) => slowestFloor(leg) - 1)).toEqual(fails("deadline"));
      expect(at("slowest_seconds", slowestFloor)).toEqual([]);
      // The step a minute under or over the deadline, the kill-after and the slack: the step
      // rule, and the job set from that step.
      expect(at("step_minutes", (leg) => leg.stepMinutes - 1)).toEqual(fails("step", "job"));
      expect(at("step_minutes", (leg) => leg.stepMinutes + 1)).toEqual(fails("step", "job"));
      // The job a minute under or over the step and twice the other steps: the job rule.
      expect(at("job_minutes", (leg) => leg.jobMinutes - 1)).toEqual(fails("job"));
      expect(at("job_minutes", (leg) => leg.jobMinutes + 1)).toEqual(fails("job"));
      // Other steps a new reading makes too long for the job, or so short that the job was set
      // from more: the job rule. The bounds of the other steps the job holds pass.
      const otherCeiling = (leg: MeasuredLeg) =>
        Math.floor((leg.jobMinutes * 60 - leg.stepMinutes * 60) / MARGIN);
      expect(at("other_steps_seconds", (leg) => otherCeiling(leg) + 1)).toEqual(fails("job"));
      expect(at("other_steps_seconds", otherCeiling)).toEqual([]);
      const otherFloor = (leg: MeasuredLeg) =>
        Math.floor(((leg.jobMinutes - 1) * 60 - leg.stepMinutes * 60) / MARGIN) + 1;
      expect(at("other_steps_seconds", (leg) => otherFloor(leg) - 1)).toEqual(fails("job"));
      expect(at("other_steps_seconds", otherFloor)).toEqual([]);
      // A row whose measurement is gone reads as no number, not as a pass.
      expect(
        inRow(lane, (row) => {
          delete row.slowest_seconds;
        })
      ).toEqual(fails("unread"));
    }

    // The job's and the step's own shape: each takes its limit from the matrix, and the step
    // runs the tool under the deadline.
    expect(
      broken((copy) => {
        const job = copy?.jobs?.repeat;
        if (job !== undefined) job["timeout-minutes"] = 60;
      })
    ).toEqual([{ lane: null, rule: "job-timeout" }]);
    expect(
      broken((copy) => {
        const step = testStep(copy);
        if (step !== undefined) step["timeout-minutes"] = 60;
      })
    ).toEqual([{ lane: null, rule: "step-timeout" }]);
    expect(
      broken((copy) => {
        const step = testStep(copy);
        if (step !== undefined) {
          step.run = String(step.run).replace(`\${{ matrix.deadline_seconds }}`, "600");
        }
      })
    ).toEqual([{ lane: null, rule: "step-run" }]);
  },
  rowLimit()
);
