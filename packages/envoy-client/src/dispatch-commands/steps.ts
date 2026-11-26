import type { StepRecord } from "@legion/contracts";
import { answeredText, setModelText } from "@legion/contracts";
import type { CommandContext, DispatchToolResult } from "./context";

/** A field the record holds empty or null prints `-`, so every line keeps its columns. */
function orDash(value: string | null): string {
  return value === null || value === "" ? "-" : value;
}

/** One run of a step: where it stands in its task, who ran it on which model, and how it ended.
 *  A run that has not ended holds no end and no result, and prints `running` for both. The model
 *  is the one that answered last. Then `set` names the model the run's role was set to, and
 *  `answered` the models that answered its agent (`setModelText`, `answeredText`). */
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
    "set",
    orDash(setModelText(record)),
    "answered",
    answeredText(record),
  ].join(" ");
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

/** `dispatch steps`: each run of each step of an issue, task by task, as Dispatch lists them, with
 *  the variant of the issue above them where its records name one. */
export async function stepsCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { client, issue } = context;
  const read = await client.listSteps(issue());
  // The generated shape lets either list be null; Dispatch writes `[]`, and null reads the same.
  const tasks = (read.tasks ?? []).map((task) => ({ task: task.task, steps: task.steps ?? [] }));
  const records = tasks.flatMap((task) => task.steps);
  return {
    text: [
      `${read.issue}: ${records.length} steps in ${tasks.length} tasks`,
      ...variantLines(records),
      ...records.map(stepLine),
    ].join("\n"),
    details: { issue: read.issue, tasks },
  };
}
