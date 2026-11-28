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
        steps: [
          defaultRuns.implement1,
          defaultRuns.review1,
          defaultRuns.implement2,
          record({ id: "retro-1", kind: "retro", step: 0, type: "" }),
          defaultRuns.merge1,
        ],
        task: "default",
      },
      { steps: [record({ id: "other-task-1", task: "docs" })], task: "docs" },
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
    commits: null,
    ended: "2026-11-16T10:30:00Z",
    fixOf: null,
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
      { steps: [record({ id: "review-1", task: "review" })], task: "review" },
      {
        steps: [record({ id: "default-1" }), record({ attempt: 2, id: "default-2" })],
        task: "default",
      },
      { steps: [record({ id: "docs-1", task: "docs" })], task: "docs" },
    ],
  });

  expect(tasks.map((task) => [task.task, task.rows.map((row) => row.id)])).toEqual([
    ["review", ["review-1"]],
    ["default", ["default-1", "default-2"]],
    ["docs", ["docs-1"]],
  ]);
});

// The generated shape lets both lists be null, and an issue Legion never ran has no task at all.
test("stepTasks reads a null list as empty", () => {
  expect(stepTasks({ issue: "CORE-1", tasks: null })).toEqual([]);
  expect(stepTasks({ issue: "CORE-1", tasks: [] })).toEqual([]);
  expect(stepTasks({ issue: "CORE-1", tasks: [{ steps: null, task: "default" }] })).toEqual([
    { rows: [], task: "default" },
  ]);
});

// Two commits of the head of CORE-1's task, in the order it moved.
const HEAD_X = "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678";
const HEAD_Y = "b2c3d4e5f60718293a4b5c6d7e8f9012345678a1";

// A run carries the commits its task's head was at when it started and when it ended, which the
// column Commits draws, and a fix carries the CI check it fixes, which the mark "Fix of <check>"
// names. A run still running holds its start commit alone.
test("stepTasks gives each run its commits and a fix the check it fixes", () => {
  const [task] = stepTasks({
    issue: "CORE-1",
    tasks: [
      {
        steps: [
          record({
            attempt: 2,
            end_commit: HEAD_Y,
            fix_check: "k-ci",
            id: "fix-1",
            start_commit: HEAD_X,
          }),
          record({
            attempt: 3,
            ended_at: null,
            id: "implement-3",
            result: "",
            start_commit: HEAD_Y,
          }),
        ],
        task: "default",
      },
    ],
  });

  expect(task?.rows.map((row) => [row.id, row.commits, row.fixOf])).toEqual([
    ["fix-1", { end: HEAD_Y, start: HEAD_X }, "k-ci"],
    ["implement-3", { end: null, start: HEAD_Y }, null],
  ]);
});

// The control of the row above: a run whose record names no check carries no mark, and one whose
// record holds no commit carries no commits, so the cell draws none.
test("stepTasks gives a run with no check no mark, and a run with no commit no commits", () => {
  const [task] = stepTasks({
    issue: "CORE-1",
    tasks: [
      {
        steps: [
          record({ end_commit: HEAD_Y, id: "unchecked", start_commit: HEAD_X }),
          record({ id: "uncommitted" }),
        ],
        task: "default",
      },
    ],
  });

  expect(task?.rows.map((row) => [row.id, row.commits, row.fixOf])).toEqual([
    ["unchecked", { end: HEAD_Y, start: HEAD_X }, null],
    ["uncommitted", null, null],
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
