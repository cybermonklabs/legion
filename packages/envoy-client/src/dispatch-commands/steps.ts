import type { StepRecord } from "@legion/contracts";
import type { CommandContext, DispatchToolResult } from "./context";
import { oneLineJson } from "./one-line-json";

/** A field the record holds empty or null prints `-`, so every line keeps its columns. */
function orDash(value: string | null): string {
  return value === null || value === "" ? "-" : value;
}

/** A check id the line prints as it stands: a plain word, holding no space of any kind, no
 *  quotation mark, no backslash, no control character, no format character, no combining mark, no
 *  lone surrogate and no `##[`. A check id is a Dispatch block id, which holds whatever the writer
 *  of the check wrote, so anything else prints as a JSON string (`oneLineJson`), which escapes
 *  each such character and the `[` of a `##[`. A line break in an id would otherwise print a line
 *  of a run nobody ran, a space would hide where the id begins, a format character such as a bidi
 *  override would show the line in another order, an invisible mark (`\p{Mn}`, a variation
 *  selector U+FE0F or U+034F) would make an id read as another, and a `##[` would start a
 *  workflow command in a runner's log. */
const plainWord = /^(?!.*##\[)[^\s"\\\p{Cc}\p{Cf}\p{Cs}\p{Mn}]+$/u;

/** What a run's line ends with after its result: ` at <start>..<end>`, the commits its task's head
 *  was at when the run started and when it ended, where the record holds either, each cut to its
 *  first ten characters, or `-` for one the record does not hold yet; then, for a fix, ` fix of
 *  <check>`, the CI check it fixes, as it stands or quoted (`plainWord`), which is the rest of
 *  the line. A record that holds neither field, as an older Dispatch answers one, ends at its
 *  result. */
function commitsAndFix(record: StepRecord): string[] {
  const words: string[] = [];
  if (record.start_commit || record.end_commit) {
    const start = record.start_commit?.slice(0, 10) || "-";
    words.push("at", `${start}..${record.end_commit?.slice(0, 10) || "-"}`);
  }
  const check = record.fix_check;
  if (check) words.push("fix of", plainWord.test(check) ? check : oneLineJson(check));
  return words;
}

/** One run of a step: where it stands in its task, who ran it on which model, how it ended, and
 *  then its commits and the check a fix fixes. A run that has not ended holds no end and no
 *  result, and prints `running` for both. */
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
    ...commitsAndFix(record),
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
