import type { PlannedStep, StepRecord } from "@legion/contracts";
import type { CommandContext, DispatchToolResult } from "./context";

/** A field the record does not hold prints `-`, so every line keeps its columns. */
function orDash(value: string | null): string {
  return value === null || value === "" ? "-" : value;
}

/** One run of a step: where it stands in its task, who ran it on which model, and how it ended.
 *  A run that has not ended holds no end and no result, and prints `running` for both. */
function stepLine(record: StepRecord): string {
  return [
    record.task,
    record.step,
    record.kind,
    record.role,
    orDash(record.type),
    "attempt",
    record.attempt,
    orDash(record.agent),
    orDash(record.model),
    orDash(record.started_at),
    ...(record.ended_at === null
      ? ["running", "running"]
      : [record.ended_at, orDash(record.result)]),
  ].join(" ");
}

/** A task of the plan: the tasks it needs, as a list in brackets joined by commas, `[]` for none,
 *  and `-` where its plan node recorded none, as `orDash` prints a field a record does not hold;
 *  then each step the plan gives it, at the state Dispatch answers for it from the runs of the
 *  plan's own run of the issue. A task id is lowercase letters, digits and hyphens, so neither `[]`
 *  nor `-` is one, and the list holds no space, so it stays one field of the line. */
function taskLine(
  task: string,
  needs: readonly string[] | null,
  planned: readonly PlannedStep[]
): string {
  const states = planned.map((step) => `${step.step} ${step.name} ${step.state}`);
  const needed = needs === null ? "-" : `[${needs.join(",")}]`;
  return `task ${task} needs ${needed}: ${states.join(", ")}`;
}

/** `dispatch steps`: each task of an issue as Dispatch lists them, the tasks of its plan first,
 *  and each run of each step, task by task. A task of the plan prints the line of its needs and
 *  its planned steps (taskLine) before its runs; any other task prints its runs alone. */
export async function stepsCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { client, issue } = context;
  const read = await client.listSteps(issue());
  // The generated shape lets either list be null; Dispatch writes `[]`, and null reads the same.
  // A Dispatch older than the plan's tasks sends no `needs` or `planned`: no task line prints.
  const tasks = (read.tasks ?? []).map((task) => ({ ...task, steps: task.steps ?? [] }));
  const count = tasks.reduce((count, task) => count + task.steps.length, 0);
  const lines = tasks.flatMap((task) => {
    const runs = task.steps.map(stepLine);
    const planned = task.planned ?? [];
    if (planned.length === 0) return runs;
    return [taskLine(task.task, task.needs ?? null, planned), ...runs];
  });
  return {
    text: [`${read.issue}: ${count} steps in ${tasks.length} tasks`, ...lines].join("\n"),
    details: { issue: read.issue, tasks },
  };
}
