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
 *  (step 0). An empty `agent`, `model` or `type` is a field the record does not hold, as is a
 *  null `started` or `ended`; `attempt` is the server's count of this step's runs in its task. */
export type StepRow = StepEnd & {
  id: string;
  label: string;
  role: string;
  type: string;
  agent: string;
  model: string;
  attempt: number;
  started: string | null;
  ended: string | null;
};

/** Where a step of the plan stands, as the server answers it from the runs of the plan's own run of
 *  the issue: `not started`, `running`, or the result its newest run ended with. `other` is a word
 *  this build does not know, which only a newer Dispatch can send. */
export type PlannedStepState =
  | "not started"
  | "running"
  | "passed"
  | "failed"
  | "stopped"
  | "other";

/** One step the plan gives a task: `step` is its place in the task, from 1, and `name` the plan's
 *  word for it, a role or `role:key`. `state` is where the server says it stands, and `result` the
 *  server's own word for it, which the tab shows for a state of `other`. */
export interface PlannedStep {
  step: number;
  name: string;
  state: PlannedStepState;
  result: string;
}

/** One planned step as the server answers it. */
type AnsweredStep = NonNullable<NonNullable<IssueSteps["tasks"]>[number]["planned"]>[number];

/** One task: the tasks of the plan it needs, null where its plan node recorded none or the plan
 *  does not hold it, and [] where it needs none; the steps the plan gives it, empty for a task the
 *  plan does not hold; and its runs in the order the server read them. */
export interface StepTaskRows {
  task: string;
  needs: string[] | null;
  planned: PlannedStep[];
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
    attempt: record.attempt,
    ended: record.ended_at,
    id: record.id,
    // A kind only a newer Dispatch sends, which this build's contract does not name, keeps its
    // own word rather than a step number it lacks.
    label: record.kind === "step" ? String(record.step) : (KIND_LABELS[record.kind] ?? record.kind),
    model: record.model,
    role: record.role,
    started: record.started_at,
    type: record.type,
  };
}

function plannedState(state: string): PlannedStepState {
  switch (state) {
    case "not started":
    case "running":
    case "passed":
    case "failed":
    case "stopped":
      return state;
    default:
      return "other";
  }
}

/** A planned step as the server answers it, its state kept as answered: the client works out no
 *  state from the runs it lists, which hold every run of the issue. */
function plannedStep(answered: AnsweredStep): PlannedStep {
  return {
    name: answered.name,
    result: answered.state,
    state: plannedState(answered.state),
    step: answered.step,
  };
}

/** The issue's tasks in the server's order - the tasks of its newest plan first, in the plan's
 *  order, then each other task with a run, by its first run - each with its needs, its planned
 *  steps and its runs in the order of the read: by generation, then start, a run with no start
 *  first. The generated shape lets either list be null, which reads as empty; a server older than
 *  the plan's tasks sends no `needs` or `planned`, which read as needs not recorded and no step. */
export function stepTasks(steps: IssueSteps): StepTaskRows[] {
  return (steps.tasks ?? []).map((task) => ({
    needs: task.needs ?? null,
    planned: (task.planned ?? []).map(plannedStep),
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
