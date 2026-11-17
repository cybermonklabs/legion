import type { StepRecord } from "@legion/contracts";
import type { CommandContext, DispatchToolResult } from "./context";

/** A field the record does not hold prints `-`, so every line keeps its columns. */
function orDash(value: string | null): string {
  return value === null || value === "" ? "-" : value;
}

/** One run of a step: where it stands in its task, who ran it on which model, and how it ended.
 *  A run that has not ended holds no end and no result, and prints `running` for both. */
function stepLine(record: StepRecord): string {
  const ended = record.ended_at !== null;
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
    ended ? record.ended_at : "running",
    ended ? orDash(record.result) : "running",
  ].join(" ");
}

/** `dispatch steps`: each run of each step of an issue, task by task, as Dispatch lists them. */
export async function stepsCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { client, issue } = context;
  const read = await client.listSteps(issue());
  // The generated shape lets either list be null; Dispatch writes `[]`, and null reads the same.
  const tasks = (read.tasks ?? []).map((task) => ({ task: task.task, steps: task.steps ?? [] }));
  const records = tasks.flatMap((task) => task.steps);
  return {
    text: [
      `${read.issue}: ${records.length} steps in ${tasks.length} tasks`,
      ...records.map(stepLine),
    ].join("\n"),
    details: { issue: read.issue, tasks },
  };
}
