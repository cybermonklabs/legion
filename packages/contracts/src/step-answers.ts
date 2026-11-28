import type { StepRecord } from "./dispatch-api";

/**
 * The model a step's role was set to and the models that answered its agent, as `dispatch steps`
 * prints them and the issue's Steps tab draws them, so both surfaces read a record alike.
 */

/** What the two texts read of a step record. A Dispatch older than the set model and the answers
 *  sends neither key, so each may be absent. */
export type StepRecordModels = Pick<StepRecord, "ended_at"> &
  Partial<Pick<StepRecord, "answers" | "set_model">>;

/** The model the step's role was set to, or null where the record names none. */
export function setModelText(record: StepRecordModels): string | null {
  const setModel = record.set_model ?? "";
  return setModel === "" ? null : setModel;
}

/** The models that answered the step's agent: `<model>=<calls>(<share>%)` for each, in the
 *  record's order, joined by `,`. The share is of all the step's calls, in whole percent. It is
 *  `running` for a run that has not ended, `unknown` where the record does not know its counts,
 *  and `none` for a step with no call. */
export function answeredText(record: StepRecordModels): string {
  if (record.ended_at === null) return "running";
  const answers = record.answers ?? null;
  if (answers === null) return "unknown";
  if (answers.length === 0) return "none";
  const calls = answers.reduce((sum, answer) => sum + answer.calls, 0);
  return answers
    .map(
      (answer) => `${answer.model}=${answer.calls}(${Math.round((100 * answer.calls) / calls)}%)`
    )
    .join(",");
}
