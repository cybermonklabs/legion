import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useState } from "react";
import { priorityHistoryQuery } from "../../api/queries";
import type { PriorityAct, PriorityRow } from "../../api/types";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import {
  borderDefault,
  dangerText,
  surfaceBg,
  textMutedOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { dateKey } from "../activity/activity-model";
import { AgentName } from "../refs/AgentName";
import { Timestamp } from "../refs/Timestamp";
import {
  MARGIN_STATES,
  type MarginState,
  readMarginState,
  writeMarginState,
} from "./priority-margin-state";

/** Why each state but Everything is empty on a priority. A priority is a node of the graph and
 *  not a Dispatch document: nothing hangs off it that a person can write on, ask about or pin,
 *  so these are facts about what a priority is, not a list that failed to load. */
const NOTHING_OF_THAT_KIND: Record<Exclude<MarginState, "everything">, string> = {
  words: "A priority carries no comments yet, so nobody has written anything here.",
  questions: "A priority carries no asks yet, so no question has been asked about this one.",
  pinned: "A priority carries nothing a reader can pin yet.",
};

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
    case "visibility":
      return "wrote who can see this priority";
    default:
      return `wrote the ${act.field}`;
  }
}

/** The day heading an entry falls under, newest first. */
function dayLabel(at: string, today: string): string {
  const day = dateKey(at);
  if (day === today) return "Today";
  const yesterday = new Date();
  yesterday.setDate(yesterday.getDate() - 1);
  if (day === dateKey(yesterday.toISOString())) return "Yesterday";
  return new Date(at).toLocaleDateString(undefined, {
    day: "numeric",
    month: "short",
    year: "numeric",
  });
}

/** Everything that happened to the priority, newest first, as the server answers it
 *  (`GET /api/v1/priorities/{id}/history`): each write of its title, description, owner and
 *  order, and each `advances` relation an issue asserted or retracted on it, each naming who
 *  stated it and when, under the day it happened on. It is mounted only while Everything is the
 *  chosen state, so no other state reads the history. */
function PriorityHistory({ id }: { id: string }): ReactNode {
  const history = useQuery(priorityHistoryQuery(id));
  if (history.isPending) return <LoadingSkeleton label="Loading what happened" />;
  if (history.isError)
    return (
      <p className={`text-sm ${dangerText}`} role="alert">
        Could not load what happened to this priority.
      </p>
    );
  const today = dateKey(new Date().toISOString());
  let day = "";
  return (
    <ol className="space-y-3">
      {history.data.map((act) => {
        const heading = dayLabel(act.at, today);
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
 * The margin of a priority: one list of what happened to it, beside the page, newest first,
 * under the one labelled control that holds its four states. The page carries no tab strip and
 * this carries no tabs of its own, because a tab and a state are the same object.
 *
 * Everything is the priority's own history, every act of it the server holds. The other three
 * each say in one line why nothing of that kind is on a priority, which is what a priority is
 * and not a list that failed to load.
 */
export function PriorityMargin({ priority }: { priority: PriorityRow }): ReactNode {
  const [state, setState] = useState<MarginState>(readMarginState);
  return (
    <aside
      aria-label="Margin"
      className={`w-full shrink-0 space-y-3 border-t pt-4 lg:w-80 lg:border-t-0 lg:border-l lg:pt-0 lg:pl-4 ${borderDefault}`}
      data-testid="priority-margin"
    >
      <label className={`flex items-center gap-2 text-sm ${textSecondaryOnCanvas}`}>
        Margin
        <select
          className={`rounded-lg border px-2 py-1 ${borderDefault} ${surfaceBg}`}
          onChange={(event) => {
            const next = event.target.value as MarginState;
            setState(next);
            writeMarginState(next);
          }}
          value={state}
        >
          {MARGIN_STATES.map((option) => (
            <option key={option.id} value={option.id}>
              {option.label}
            </option>
          ))}
        </select>
      </label>
      {state === "everything" ? (
        <PriorityHistory id={priority.id} />
      ) : (
        <p className={`text-sm ${textMutedOnCanvas}`}>{NOTHING_OF_THAT_KIND[state]}</p>
      )}
    </aside>
  );
}
