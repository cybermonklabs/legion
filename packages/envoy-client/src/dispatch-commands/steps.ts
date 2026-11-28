import type { PlannedStep, StepRecord } from "@legion/contracts";
import type { CommandContext, DispatchToolResult } from "./context";

/** A field the record holds empty or null prints `-`, so every line keeps its columns. */
function orDash(value: string | null): string {
  return value === null || value === "" ? "-" : value;
}

/** One run of a step: where it stands in its task, who ran it on which model, and how it ended.
 *  A run that has not ended holds no end and no result, and prints `running` for both. */
function stepLine(record: StepRecord): string {
  return [
    orDash(record.task),
    record.step,
    record.kind,
    orDash(record.role),
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

/** The experiment a draw put the issue in and the variant it drew there, as the records of its
 *  newest run, its highest generation, name them: `Experiment <experiment>: variant <variant>.`,
 *  or nothing where none of those records names one. A Dispatch older than the two fields answers
 *  a record without them, which names none either. */
function variantLines(records: readonly StepRecord[]): string[] {
  const newest = Math.max(...records.map((record) => record.generation));
  const named = records.find(
    (record) => record.generation === newest && record.experiment != null && record.variant != null
  );
  return named === undefined ? [] : [`Experiment ${named.experiment}: variant ${named.variant}.`];
}

/** `dispatch steps`: each task of an issue as Dispatch lists them, the tasks of its plan first,
 *  and each run of each step, task by task, with the variant of the issue above them where its
 *  records name one. A task of the plan prints the line of its needs and its planned steps
 *  (taskLine) before its runs; any other task prints its runs alone. */
export async function stepsCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { client, issue } = context;
  const read = await client.listSteps(issue());
  // The generated shape lets either list be null; Dispatch writes `[]`, and null reads the same.
  // A Dispatch older than the plan's tasks sends no `needs` or `planned`: no task line prints.
  const tasks = (read.tasks ?? []).map((task) => ({ ...task, steps: task.steps ?? [] }));
  const records = tasks.flatMap((task) => task.steps);
  const lines = tasks.flatMap((task) => {
    const runs = task.steps.map(stepLine);
    const planned = task.planned ?? [];
    if (planned.length === 0) return runs;
    return [taskLine(task.task, task.needs ?? null, planned), ...runs];
  });
  return {
    text: [
      `${read.issue}: ${records.length} steps in ${tasks.length} tasks`,
      ...variantLines(records),
      ...lines,
    ].join("\n"),
    details: { issue: read.issue, tasks },
  };
}
