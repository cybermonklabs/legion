import { expect, test } from "bun:test";

import type { StepRecord } from "../../api/types";
import { defaultRuns, stepRecord as record } from "./step-record-fixture";
import { stepTasks, stepVariant } from "./steps-model";

// What the tab draws, without a DOM: the server's tasks in the server's order, each task's runs
// in the order the read listed them, a retro and a merge named by their kind, and a run that has
// not ended read as running.
test("stepTasks keeps the server's order and names each run's step, state and times", () => {
  const tasks = stepTasks({
    issue: "CORE-1",
    tasks: [
      {
        needs: null,
        steps: [
          defaultRuns.implement1,
          defaultRuns.review1,
          defaultRuns.implement2,
          record({ id: "retro-1", kind: "retro", step: 0, type: "" }),
          defaultRuns.merge1,
        ],
        task: "default",
      },
      { needs: null, steps: [record({ id: "other-task-1", task: "docs" })], task: "docs" },
    ],
  });

  expect(tasks.map((task) => [task.task, task.rows.map((row) => row.id)])).toEqual([
    ["default", ["implement-1", "review-1", "implement-2", "retro-1", "merge-1"]],
    ["docs", ["other-task-1"]],
  ]);
  const [first] = tasks;
  expect(first?.rows.map((row) => row.label)).toEqual(["2", "4", "2", "Retro", "Merge"]);
  expect(first?.rows.map((row) => row.state)).toEqual([
    "passed",
    "failed",
    "running",
    "passed",
    "passed",
  ]);
  expect(first?.rows[1]).toEqual({
    agent: "legion-core-core-1-reviewer",
    attempt: 1,
    ended: "2026-11-16T10:30:00Z",
    id: "review-1",
    label: "4",
    model: "openai/gpt-6.1-sol",
    returnedTo: "step 2",
    role: "reviewer",
    started: "2026-11-16T10:00:00Z",
    state: "failed",
    type: "review",
  });
  expect(first?.rows[2]).toMatchObject({ attempt: 2, ended: null, state: "running" });
  expect(first?.rows[4]).toMatchObject({ agent: "", model: "", started: null, type: "" });
});

// The server orders tasks by each task's first step, which is no order of their names: three
// tasks whose names sort neither up nor down keep the server's order, each with its own runs.
test("stepTasks keeps the server's order of tasks, not the order of their names", () => {
  const tasks = stepTasks({
    issue: "CORE-1",
    tasks: [
      { needs: null, steps: [record({ id: "review-1", task: "review" })], task: "review" },
      {
        needs: null,
        steps: [record({ id: "default-1" }), record({ attempt: 2, id: "default-2" })],
        task: "default",
      },
      { needs: null, steps: [record({ id: "docs-1", task: "docs" })], task: "docs" },
    ],
  });

  expect(tasks.map((task) => [task.task, task.rows.map((row) => row.id)])).toEqual([
    ["review", ["review-1"]],
    ["default", ["default-1", "default-2"]],
    ["docs", ["docs-1"]],
  ]);
});

// The generated shape lets both lists be null, and an issue with no plan and no run has no task at
// all. A task whose needs are null - its plan node recorded none, or the plan does not hold it -
// keeps them null, which is not [], a task that needs none.
test("stepTasks reads a null list as empty, and null needs as not recorded", () => {
  expect(stepTasks({ issue: "CORE-1", tasks: null })).toEqual([]);
  expect(stepTasks({ issue: "CORE-1", tasks: [] })).toEqual([]);
  expect(
    stepTasks({ issue: "CORE-1", tasks: [{ needs: null, steps: null, task: "default" }] })
  ).toEqual([{ needs: null, planned: [], rows: [], task: "default" }]);
  expect(
    stepTasks({ issue: "CORE-1", tasks: [{ needs: [], steps: null, task: "default" }] })[0]?.needs
  ).toEqual([]);
});

// The tasks of the plan come with what the plan says of them: the tasks each needs, and each step
// the plan gives it, in its place, at the state the server answers for it. The fault is a planned
// step that a person cannot see; the control is the model that keeps the runs alone, under which b
// has no step and a no second one.
test("stepTasks gives each task of the plan its needs and its planned steps", () => {
  const tasks = stepTasks({
    issue: "CORE-1",
    tasks: [
      {
        needs: [],
        planned: [
          { name: "implementer", state: "running", step: 1 },
          { name: "reviewer", state: "not started", step: 2 },
        ],
        steps: [record({ ended_at: null, id: "a-1", result: "", step: 1, task: "a" })],
        task: "a",
      },
      {
        needs: ["a"],
        planned: [{ name: "tester", state: "not started", step: 1 }],
        steps: [],
        task: "b",
      },
    ],
  });
  expect(
    tasks.map((task) => ({
      needs: task.needs,
      planned: task.planned.map((step) => [step.step, step.name, step.state]),
      task: task.task,
    }))
  ).toEqual([
    {
      needs: [],
      planned: [
        [1, "implementer", "running"],
        [2, "reviewer", "not started"],
      ],
      task: "a",
    },
    { needs: ["a"], planned: [[1, "tester", "not started"]], task: "b" },
  ]);
});

// The server reads where a planned step stands from the runs of the plan's own run of the issue;
// the model takes that state as answered and works out none from the runs it lists, which hold the
// runs of every run of the issue. A state this build has no word for keeps the server's word. The
// fault is a step of a new run that shows the result of an older one; the control is the model that
// takes the newest run at the step's place, under which the reviewer reads passed.
test("stepTasks draws each planned step at the state the server answers", () => {
  const [task] = stepTasks({
    issue: "CORE-1",
    tasks: [
      {
        needs: [],
        planned: [
          { name: "implementer", state: "running", step: 1 },
          { name: "reviewer", state: "not started", step: 2 },
          { name: "acceptance:owner", state: "skipped", step: 3 },
        ],
        steps: [
          record({
            agent: "legion-core-core-1-reviewer",
            id: "review-1",
            role: "reviewer",
            step: 2,
            type: "review",
          }),
          record({ ended_at: null, generation: 2, id: "implement-2", result: "", step: 1 }),
        ],
        task: "default",
      },
    ],
  });
  expect(task?.planned).toEqual([
    { name: "implementer", result: "running", state: "running", step: 1 },
    { name: "reviewer", result: "not started", state: "not started", step: 2 },
    { name: "acceptance:owner", result: "skipped", state: "other", step: 3 },
  ]);
});

// A move back names where it sent the task. Place 0 is the retro's, which a merger's move back
// reaches and no plan names, so it reads as the retro and never as `step 0`; and a return to place
// 0 is a return, not the nothing a failed run that returned nowhere names.
test("stepTasks names where a run returned its task, the retro for place 0", () => {
  const [task] = stepTasks({
    issue: "CORE-1",
    tasks: [
      {
        needs: null,
        steps: [
          record({
            id: "review-1",
            result: "stopped",
            returned_to: 2,
            role: "reviewer",
            step: 4,
            type: "review",
          }),
          record({
            id: "merge-1",
            kind: "merge",
            result: "failed",
            returned_to: 0,
            role: "merger",
            step: 0,
            type: "",
          }),
          record({ id: "implement-1", result: "failed" }),
        ],
        task: "default",
      },
    ],
  });

  expect(task?.rows).toMatchObject([
    { id: "review-1", returnedTo: "step 2", state: "stopped" },
    { id: "merge-1", returnedTo: "the retro", state: "failed" },
    { id: "implement-1", returnedTo: null, state: "failed" },
  ]);
});

// A result or a kind this build has no word for comes from a newer Dispatch: the run is kept, in
// its place, with the server's own word, rather than dropped or read as one of the known states.
test("stepTasks keeps a run whose result or kind it does not know, in the server's words", () => {
  const [task] = stepTasks({
    issue: "CORE-1",
    tasks: [
      {
        needs: null,
        steps: [
          record({ id: "skipped", result: "skipped" as StepRecord["result"] }),
          record({ id: "pause", kind: "pause" as StepRecord["kind"], step: 0 }),
        ],
        task: "default",
      },
    ],
  });

  expect(
    task?.rows.map((row) => [row.id, row.label, row.state, row.state === "other" && row.result])
  ).toEqual([
    ["skipped", "2", "other", "skipped"],
    ["pause", "pause", "passed", false],
  ]);
});

// The variant of the issue is the one the records of its newest run, its highest generation,
// name: an older run that names another and a newest record that names none yet change nothing,
// and records that name none, null or left out as a Dispatch older than the two fields answers
// them, name none.
test("stepVariant names the experiment and the variant the newest run's records name", () => {
  const named = { experiment: "planning", variant: "b" };
  expect(
    stepVariant({
      issue: "CORE-1",
      tasks: [
        {
          steps: [
            record({ experiment: "planning", id: "older", variant: "a" }),
            record({ generation: 2, id: "newest-unnamed" }),
            record({ generation: 2, id: "newest", ...named }),
          ],
          task: "default",
        },
      ],
    })
  ).toBe("Experiment planning: variant b");
  const older = record({ id: "no-fields" }) as Partial<StepRecord>;
  delete older.experiment;
  delete older.variant;
  for (const steps of [[record({ id: "none" })], [older as StepRecord]]) {
    expect(stepVariant({ issue: "CORE-1", tasks: [{ steps, task: "default" }] })).toBeNull();
  }
  expect(stepVariant({ issue: "CORE-1", tasks: [] })).toBeNull();
});
