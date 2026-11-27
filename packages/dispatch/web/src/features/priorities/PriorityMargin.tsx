import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { priorityHistoryQuery } from "../../api/queries";
import type { PriorityAct } from "../../api/types";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import {
  borderDefault,
  dangerText,
  textMutedOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { dateKey, dayLabel } from "../activity/activity-model";
import { AgentName } from "../refs/AgentName";
import { Timestamp } from "../refs/Timestamp";

/** One act of the history in the words a person reads, with no field name and no value a
 *  reader cannot use: a write names what it wrote, a description names itself rather than
 *  quoting a markdown body, and an `advances` act names the issue at its other end. An act of
 *  a field written after this was (`prioritystore`'s attributes) still reads as a write of it. */
function actWords(act: PriorityAct): string {
  switch (act.field) {
    case "advances":
      return act.asserted
        ? `related the issue ${act.issue}`
        : `retracted the relation to the issue ${act.issue}`;
    case "title":
      return act.value === "" ? "wrote the title" : `wrote the title “${act.value}”`;
    case "description":
      return "wrote the description";
    case "owner":
      return act.value === "" ? "wrote the owner" : `wrote the owner “${act.value}”`;
    case "rank":
      return "moved the order";
    case "state":
      return act.value === "gone" ? "ended this priority" : "wrote this priority";
    default:
      return `wrote the ${act.field}`;
  }
}

/** The field of the act that writes who may see a priority. The server writes it with the
 *  priority, and no control of these pages writes it, so it is a part of "wrote this priority"
 *  and the margin draws no entry of its own for it; the history keeps it. */
const VISIBILITY = "visibility";

/** The history as the margin lists it, newest first: a run of consecutive writes of one field by
 *  one actor - every save of a field while it is being written, half a second apart - is one
 *  entry, the newest of the run, at its time. A write of another field, or by another actor,
 *  between two writes breaks the run, and an `advances` act names an issue of its own, so it
 *  folds into nothing. The history itself keeps every act; only this list folds them. */
function foldRuns(acts: readonly PriorityAct[]): PriorityAct[] {
  const folded: PriorityAct[] = [];
  for (const act of acts) {
    const newer = folded.at(-1);
    const sameRun =
      newer !== undefined &&
      act.field !== "advances" &&
      newer.field === act.field &&
      newer.actor.kind === act.actor.kind &&
      newer.actor.id === act.actor.id;
    if (!sameRun) folded.push(act);
  }
  return folded;
}

/** Everything that happened to the priority, newest first, as the server answers it
 *  (`GET /api/v1/priorities/{id}/history`): each write of its title, description, owner and
 *  order, and each `advances` relation an issue asserted or retracted on it, each naming who
 *  stated it and when, under the day it happened on, consecutive writes of one field by one
 *  actor read as one (`foldRuns`). The visibility the priority was written with is no entry
 *  (`VISIBILITY`). */
function PriorityHistory({ id }: { id: string }): ReactNode {
  const history = useQuery(priorityHistoryQuery(id));
  if (history.isPending) return <LoadingSkeleton label="Loading what happened" />;
  if (history.isError)
    return (
      <p className={`text-sm ${dangerText}`} role="alert">
        Could not load what happened to this priority.
      </p>
    );
  // The day a reading stands on, read once for the whole list: a reading that crossed midnight
  // part way down it would head two entries of the same day differently.
  const today = dateKey(new Date().toISOString());
  let day = "";
  return (
    <ol className="space-y-3">
      {foldRuns(history.data.filter((act) => act.field !== VISIBILITY)).map((act) => {
        const heading = dayLabel(dateKey(act.at), today);
        const first = heading !== day;
        day = heading;
        return (
          <li
            className="space-y-1 text-sm"
            key={`${act.at}|${act.field}|${act.issue}|${act.value}`}
          >
            {first ? (
              <p className={`text-xs font-semibold tracking-wide uppercase ${textMutedOnCanvas}`}>
                {heading}
              </p>
            ) : null}
            <p className={textSecondaryOnCanvas}>
              <AgentName actor={act.actor} inline /> ·{" "}
              <Timestamp at={act.at} className={textMutedOnCanvas} />
            </p>
            <p>{actWords(act)}</p>
          </li>
        );
      })}
    </ol>
  );
}

/**
 * The margin of a priority: the list of what happened to it, beside the page, newest first,
 * each act naming who made it and when. That history is the whole margin, with no control and
 * no view of its own: a priority carries no comments, questions or pins until
 * dispatch://INFRA-1098 makes its description a document, so a choice of what the margin
 * shows would only offer views that are always empty.
 *
 * Its acts are short lines, so it is 280 px wide beside the page, which leaves the work under
 * the priority a 768 px box in a 1440 px window.
 */
export function PriorityMargin({ id }: { id: string }): ReactNode {
  return (
    <aside
      aria-label="Margin"
      className={`w-full shrink-0 space-y-3 border-t pt-4 lg:w-70 lg:border-t-0 lg:border-l lg:pt-0 lg:pl-4 ${borderDefault}`}
      data-testid="priority-margin"
    >
      <PriorityHistory id={id} />
    </aside>
  );
}
