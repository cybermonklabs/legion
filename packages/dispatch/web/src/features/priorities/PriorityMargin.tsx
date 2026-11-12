import { type ReactNode, useState } from "react";
import type { PriorityRow } from "../../api/types";
import {
  borderDefault,
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

/** What the margin can say about the history Dispatch does not answer. The page holds the
 *  making of the priority and nothing else: no route reads a priority's own tuple log, so the
 *  writes since it was made are not listed, and the margin says so rather than drawing a list
 *  that looks loaded. */
const HISTORY_UNREAD =
  "Dispatch answers no reading of a priority's own history yet, so the writes since — the title, the description, the owner, the order and each advances — are not listed here.";

/** One entry of the list beside the page: when it happened and who did it. */
interface MarginEntry {
  readonly id: string;
  readonly at: string;
  readonly actor: PriorityRow["made_by"];
  readonly text: string;
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

/**
 * The margin of a priority: one list of what happened to it, beside the page, newest first,
 * under the one labelled control that holds its four states. The page carries no tab strip and
 * this carries no tabs of its own, because a tab and a state are the same object.
 *
 * Everything lists what the page holds of the priority's history. Today that is its making;
 * the rest of the history has no route to read it, which the list says under itself in plain
 * words rather than drawing rows that stand for nothing.
 */
export function PriorityMargin({ priority }: { priority: PriorityRow }): ReactNode {
  const [state, setState] = useState<MarginState>(readMarginState);
  const entries: MarginEntry[] =
    state === "everything"
      ? [
          {
            id: "made",
            at: priority.made_at,
            actor: priority.made_by,
            text: "wrote this priority",
          },
        ]
      : [];
  const today = dateKey(new Date().toISOString());
  let day = "";
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
        <>
          <ol className="space-y-3">
            {entries.map((entry) => {
              const heading = dayLabel(entry.at, today);
              const first = heading !== day;
              day = heading;
              return (
                <li className="space-y-1 text-sm" key={entry.id}>
                  {first ? (
                    <p className={`text-xs font-semibold tracking-wide uppercase ${textMutedOnCanvas}`}>
                      {heading}
                    </p>
                  ) : null}
                  <p className={textSecondaryOnCanvas}>
                    <AgentName actor={entry.actor} inline /> ·{" "}
                    <Timestamp at={entry.at} className={textMutedOnCanvas} />
                  </p>
                  <p>{entry.text}</p>
                </li>
              );
            })}
          </ol>
          <p className={`text-sm ${textMutedOnCanvas}`}>{HISTORY_UNREAD}</p>
        </>
      ) : (
        <p className={`text-sm ${textMutedOnCanvas}`}>{NOTHING_OF_THAT_KIND[state]}</p>
      )}
    </aside>
  );
}
