import type { StepRecord } from "../../api/types";

/** One step record as the server reads it back, which the Steps tab's tests build on: a passed
 *  implementation run of step 2 of the task `default` of CORE-1, with every field a fixture does
 *  not set itself. */
export function stepRecord(fields: Partial<StepRecord> & Pick<StepRecord, "id">): StepRecord {
  return {
    agent: "legion-core-core-1-implementer",
    attempt: 1,
    ended_at: "2026-11-16T09:40:00Z",
    generation: 1,
    issue: "CORE-1",
    kind: "step",
    model: "anthropic/claude-opus-5",
    report: null,
    result: "passed",
    returned_to: null,
    role: "implementer",
    session: "",
    started_at: "2026-11-16T09:00:00Z",
    step: 2,
    task: "default",
    type: "implementation",
    ...fields,
  };
}

/** The runs of the task `default` that the tab and its model both draw, in the server's order: the
 *  implementer's passed run, a review that failed and sent the task back to step 2, the
 *  implementer's second run, still running, and a merge. */
export const defaultRuns = {
  implement1: stepRecord({ id: "implement-1" }),
  review1: stepRecord({
    agent: "legion-core-core-1-reviewer",
    ended_at: "2026-11-16T10:30:00Z",
    id: "review-1",
    model: "openai/gpt-6.1-sol",
    result: "failed",
    returned_to: 2,
    role: "reviewer",
    started_at: "2026-11-16T10:00:00Z",
    step: 4,
    type: "review",
  }),
  implement2: stepRecord({
    attempt: 2,
    ended_at: null,
    id: "implement-2",
    model: "",
    result: "",
    started_at: "2026-11-16T10:31:00Z",
  }),
  // A merge GitHub had already made, which started under the release before this one: that
  // release counted no start, so the end names no agent, no model and no start.
  merge1: stepRecord({
    agent: "",
    id: "merge-1",
    kind: "merge",
    model: "",
    role: "merger",
    started_at: null,
    step: 0,
    type: "",
  }),
} as const;
