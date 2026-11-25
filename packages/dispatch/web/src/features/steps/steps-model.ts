import { answeredText, setModelText } from "@legion/contracts";

import type { IssueSteps, StepRecord } from "../../api/types";

/** Where a run stands, with only what that can hold: running until the daemon writes its end,
 *  then the result it ended with. Only a failed or a stopped run sent its task back, so only it
 *  carries `returnedTo` (Dispatch takes `returned_to` only from the write that sets a failed or
 *  stopped result, so a record never pairs one end's result with another end's destination):
 *  where it sent it, as its Result cell names it - `step <n>`, or `the retro` for place 0
 *  (`returnPlace`) - or null when it sent it nowhere. `other` is a result this build has no word
 *  for, which only a newer Dispatch can send, kept in `result` in the server's own word. */
export type StepEnd =
  | { state: "running" | "passed" }
  | { state: "failed" | "stopped"; returnedTo: string | null }
  | { state: "other"; result: string };

/** One run of one step, as the Steps tab draws it: the fields of its row, and nothing the row
 *  does not show (the task is the table's, `StepTaskRows`). `label` names the step: its place in
 *  the task for a step of the plan, and `Retro` or `Merge` for the two runs that no plan names
 *  (step 0). `model` is the model that answered last, and `setModel` the model the run's role was
 *  set to. An empty `agent`, `model`, `setModel` or `type` is a field the record does not hold, as
 *  is a null `started` or `ended`; `attempt` is the server's count of this step's runs in its
 *  task. `answered` is the models that answered the run's agent, as `answeredText` writes them. */
export type StepRow = StepEnd & {
  id: string;
  label: string;
  role: string;
  type: string;
  agent: string;
  model: string;
  setModel: string;
  answered: string;
  attempt: number;
  started: string | null;
  ended: string | null;
};

/** One task's runs, in the order the server read them. */
export interface StepTaskRows {
  task: string;
  rows: StepRow[];
}

/** The label of each kind no plan names, keyed by the contract's kinds, so a kind the contract
 *  adds fails to compile here until it has one. */
const KIND_LABELS: Readonly<Record<Exclude<StepRecord["kind"], "step">, string>> = {
  merge: "Merge",
  retro: "Retro",
};

/** The place a move back sent the task to, in words. Place 0 is the retro's: of the two runs no
 *  plan names, a move back can reach only the retro, since the merge is the task's last run
 *  (Dispatch reads `returned_to` 0 as a return to the retro). It is no step, so it never reads
 *  `step 0`. */
function returnPlace(place: number | null): string | null {
  if (place === null) return null;
  return place === 0 ? "the retro" : `step ${place}`;
}

function stepEnd(record: StepRecord): StepEnd {
  switch (record.result) {
    case "":
      return { state: "running" };
    case "passed":
      return { state: "passed" };
    case "failed":
    case "stopped":
      return { returnedTo: returnPlace(record.returned_to), state: record.result };
    default:
      // A result only a newer Dispatch sends, which this build's contract does not name, keeps its
      // own word; a result the contract names fails to compile here until it has a case.
      return { result: record.result satisfies never, state: "other" };
  }
}

function stepRow(record: StepRecord): StepRow {
  return {
    ...stepEnd(record),
    agent: record.agent,
    answered: answeredText(record),
    attempt: record.attempt,
    ended: record.ended_at,
    id: record.id,
    // A kind only a newer Dispatch sends, which this build's contract does not name, keeps its
    // own word rather than a step number it lacks.
    label: record.kind === "step" ? String(record.step) : (KIND_LABELS[record.kind] ?? record.kind),
    model: record.model,
    role: record.role,
    setModel: setModelText(record) ?? "",
    started: record.started_at,
    type: record.type,
  };
}

/** The issue's tasks in the server's order (the order of each task's first step), each with its
 *  runs in the order of the read: by generation, then start, a run with no start first. The
 *  generated shape lets either list be null, which reads as empty. */
export function stepTasks(steps: IssueSteps): StepTaskRows[] {
  return (steps.tasks ?? []).map((task) => ({
    rows: (task.steps ?? []).map(stepRow),
    task: task.task,
  }));
}

/** The experiment a draw put the issue in and the variant it drew there, as the records of its
 *  newest run, its highest generation, name them: `Experiment <experiment>: variant <variant>`,
 *  or null where none of those records names one. A Dispatch older than the two fields answers a
 *  record without them, which names none either. */
export function stepVariant(steps: IssueSteps): string | null {
  const records = (steps.tasks ?? []).flatMap((task) => task.steps ?? []);
  const newest = Math.max(...records.map((record) => record.generation));
  const named = records.find(
    (record) => record.generation === newest && record.experiment != null && record.variant != null
  );
  return named === undefined ? null : `Experiment ${named.experiment}: variant ${named.variant}`;
}
