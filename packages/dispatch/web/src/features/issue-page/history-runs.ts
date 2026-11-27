import type { DispatchEvent, HandoffSummary } from "../../api/types";
import { compareTimestamps } from "../../lib/timestamps";
import { activityDescription } from "../activity/activity-model";
import type { SentencePart } from "../activity/SentenceParts";
import { type StepRow, stepTasks } from "../steps/steps-model";
import type { Dated, HistoryInput } from "./history-model";
import { comparePlaces, type HistoryPlace } from "./history-place";

/**
 * The runs and the handoffs of the timeline (the plan's D9): each run of `input.runs` one entry,
 * whose place is its time with sequence 0, so runs at one instant stand in the order of their ids
 * (`comparePlaces`); and each handoff of `input.handoffs` that no run holds one entry at its own
 * time. A run is its record with the row the steps model reads from it (`stepTasks`), found by the
 * record's id. A run stands at its start; where its record holds none, at its end; then at the
 * time of the handoff it reports, where the list holds it; then at the place of the next run of
 * its task in the server's order; then at the issue's own start. It holds the handoff its
 * `report` names where the list holds it, and where two runs report one, the newer holds it: the
 * one that stands higher. The handoff's own event (`recorded`) gives the entry that holds the
 * handoff its Pin, and an entry of the handoff alone its place in the log. A run or a handoff
 * older than what the log has read waits for its page, as a thread does (`threadsOf`), all but
 * the entry an address names (`HistoryInput.addressed`).
 */
export function runsOf(input: HistoryInput, recorded: ReadonlyMap<string, DispatchEvent>): Dated[] {
  const handoffs = new Map((input.handoffs ?? []).map((handoff) => [handoff.id, handoff]));
  const runs: { at: string; id: string; report: string | null; row: StepRow; task: string }[] = [];
  if (input.runs !== undefined) {
    // The row and the task of each run as the steps model reads them, by the id of its record.
    const rowsById = new Map(
      stepTasks(input.runs).flatMap(({ rows, task }) =>
        rows.map((row) => [row.id, { row, task }] as const)
      )
    );
    for (const { steps } of input.runs.tasks ?? []) {
      // Each run's place, from the task's last run back, so that a run with no time of its own
      // finds the place of the run after it.
      let next: string | undefined;
      for (const record of [...(steps ?? [])].reverse()) {
        const reported =
          record.report === null ? undefined : handoffs.get(record.report)?.created_at;
        next = record.started_at ?? record.ended_at ?? reported ?? next ?? input.issueCreatedAt;
        const run = rowsById.get(record.id);
        if (run === undefined) continue;
        runs.push({ at: next, id: `run:${record.id}`, report: record.report, ...run });
      }
    }
  }
  // The run that holds each handoff the list holds and a run reports: the one that stands higher.
  const placeOfRun = (run: (typeof runs)[number]): HistoryPlace => ({
    at: run.at,
    id: run.id,
    seq: 0,
  });
  const holders = new Map<string, (typeof runs)[number]>();
  for (const run of runs) {
    if (run.report === null || !handoffs.has(run.report)) continue;
    const holder = holders.get(run.report);
    if (holder === undefined || comparePlaces(placeOfRun(run), placeOfRun(holder)) > 0) {
      holders.set(run.report, run);
    }
  }
  const entries: Dated[] = runs.map((run) => {
    const handoff =
      run.report !== null && holders.get(run.report) === run ? handoffs.get(run.report) : undefined;
    return {
      at: run.at,
      handoff,
      id: run.id,
      kind: "run",
      recorded: handoff === undefined ? undefined : recorded.get(handoff.id)?.id,
      row: run.row,
      seq: 0,
      task: run.task,
    };
  });
  for (const handoff of handoffs.values()) {
    if (holders.has(handoff.id)) continue;
    const event = recorded.get(handoff.id);
    entries.push({
      at: handoff.created_at,
      handoff,
      id: `handoff:${handoff.id}`,
      kind: "handoff",
      recorded: event?.id,
      seq: event?.seq ?? 0,
    });
  }
  const from = input.loadedFrom;
  if (from === undefined) return entries;
  const named = addressedEntry(entries, input.addressed);
  return entries.filter((entry) => entry.id === named || compareTimestamps(entry.at, from) >= 0);
}

/** The id of the entry of `entries` that `addressed` names: the one that holds the handoff, or the
 *  newest run, the first of the runs that stand highest. */
export function addressedEntry(
  entries: readonly Dated[],
  addressed: HistoryInput["addressed"]
): string | undefined {
  if (addressed === undefined) return undefined;
  let named: Dated | undefined;
  for (const entry of entries) {
    if (addressed.kind === "handoff") {
      if (
        (entry.kind === "run" || entry.kind === "handoff") &&
        entry.handoff?.id === addressed.id
      ) {
        return entry.id;
      }
    } else if (entry.kind === "run" && (named === undefined || comparePlaces(entry, named) > 0)) {
      named = entry;
    }
  }
  return named?.id;
}

/** The line of a handoff of the list, which names its phase and its version, in the words of
 *  `main`'s line of its event (`activityDescription`), which carries the same three members: the
 *  handoff's entry is drawn from the list, never from that event. */
export function handoffLine(handoff: HandoffSummary): SentencePart[] {
  return activityDescription({
    actor: handoff.author,
    created_at: handoff.created_at,
    id: 0,
    issue_key: handoff.issue_key,
    notify: false,
    payload: { handoff_id: handoff.id, phase: handoff.phase, version: handoff.version },
    seq: 0,
    type: "handoff.recorded",
  });
}
