import type { IssueSteps, StepRecord } from "../../api/types";

/** Where a run stands: running until the daemon writes its end, then the result it ended with.
 *  `other` is a result this build has no word for, which only a newer Dispatch can send; the row
 *  keeps it in `result`, in the server's own word. */
export type StepState = "running" | "passed" | "failed" | "stopped" | "other";

/** One run of one step, as the Steps tab draws it. `label` names the step: its place in the task
 *  for a step of the plan, and `Retro` or `Merge` for the two runs that no plan names (step 0).
 *  An empty `agent`, `model` or `type` is a field the record does not hold, as is a null `started`
 *  or `ended`; `attempt` is the server's count of this step's runs in its task. */
export interface StepRow {
  id: string;
  task: string;
  step: number;
  label: string;
  role: string;
  type: string;
  agent: string;
  model: string;
  attempt: number;
  started: string | null;
  ended: string | null;
  state: StepState;
  result: string;
  /** The step a failed or stopped run sent its task back to, when it sent it back. */
  returnedTo: number | null;
}

/** One task's runs, in the order the server read them. */
export interface StepTaskRows {
  task: string;
  rows: StepRow[];
}

const KIND_LABELS: Readonly<Record<string, string>> = { merge: "Merge", retro: "Retro" };

function stepState(result: string): StepState {
  switch (result) {
    case "":
      return "running";
    case "passed":
    case "failed":
    case "stopped":
      return result;
    default:
      return "other";
  }
}

function stepRow(record: StepRecord): StepRow {
  return {
    agent: record.agent,
    attempt: record.attempt,
    ended: record.ended_at,
    id: record.id,
    // A kind this build does not know keeps its own word rather than a step number it lacks.
    label: record.kind === "step" ? String(record.step) : (KIND_LABELS[record.kind] ?? record.kind),
    model: record.model,
    result: record.result,
    returnedTo: record.returned_to,
    role: record.role,
    started: record.started_at,
    state: stepState(record.result),
    step: record.step,
    task: record.task,
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
