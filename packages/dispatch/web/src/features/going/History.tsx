import { useQuery } from "@tanstack/react-query";
import { type KeyboardEvent, type ReactNode, useEffect, useId, useRef, useState } from "react";

import { goingConversationsQuery } from "../../api/queries";
import type { GoingConversation } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { SearchBox } from "../../components/SearchBox";
import { inputMethodKey } from "../../lib/input-method-key";
import {
  surfaceMutedBg,
  surfaceMutedHoverBg,
  textMutedOnSurface,
  textMutedOnSurfaceMuted,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { Timestamp } from "../refs/Timestamp";
import {
  openConversation,
  runWithdrawal,
  shownState,
  titleWords,
  useGoingValue,
} from "./going-store";
import { RunUndo } from "./RunUndo";
import { stateOfRecord } from "./run-frames";
import {
  providerRefused,
  providerRefusedWords,
  type RunState,
  runEnded,
  WITHDRAWN_WORDS,
  type Withdrawal,
  withdrawalOf,
} from "./run-states";

/** How a conversation stands, as History names it by its newest run (design v3, state 11). */
const STATE_WORDS: Readonly<Record<RunState, string>> = {
  done: "Done",
  failed: "Failed",
  idle: "",
  stopped_bound: "Stopped",
  stopped_person: "Stopped",
  waiting: "Needs you",
  working: "Working",
};

/** History's groups, in the order it lists them: by the day of a conversation's newest turn. */
const GROUPS = [
  { group: "today", words: "Today" },
  { group: "yesterday", words: "Yesterday" },
  { group: "week", words: "This week" },
] as const;

type HistoryGroup = (typeof GROUPS)[number]["group"];

/** The group of a conversation whose newest turn came at `at`, on the reader's own calendar as
 *  it stands at `now`: today, yesterday, or earlier in the 7 days History keeps. */
function groupOf(at: number, now: Date): HistoryGroup {
  const day = (back: number) =>
    new Date(now.getFullYear(), now.getMonth(), now.getDate() - back).getTime();
  if (at >= day(0)) return "today";
  return at >= day(1) ? "yesterday" : "week";
}

/** One conversation as History lists it: its title (its first question), how its newest run
 *  stands, what the model's provider refused that run to read where it did (undefined where it
 *  did not), how that run is withdrawn where it is, its group, and that run where it ended holding
 *  a write Undo can take back, or where this page undid it, whose Undo the row holds. */
interface Listed {
  readonly conversation: GoingConversation;
  readonly title: string;
  readonly state: RunState;
  readonly refused: string | undefined;
  readonly withdrawal: Withdrawal | undefined;
  readonly group: HistoryGroup;
  readonly undo: string | undefined;
}

/**
 * History (R11; design v3, state 11), in the bar or the panel in the conversation's place: the
 * person's conversations of the last 7 days (`GET /api/v1/me/going-runs`), grouped Today,
 * Yesterday and This week, the newest first. Each row is the conversation's title, how it stands
 * - Working, Needs you, Done, Failed or Stopped, as this page follows its newest run, else as
 * Dispatch keeps it - and the time of its newest turn; a press opens it in the bar, where a
 * follow-up sees the turns before it. A row whose newest run the model's provider stopped,
 * refusing to read part of what it found, says so under it, by its reason's code; the row offers
 * no Continue, as no row does, and the conversation it opens draws Continue where the run's end
 * offers it. A row whose newest run ended holding a write Undo can still take back (`undoable`),
 * as a filing from the line on top does (R9), holds that run's Undo (R6). A row whose newest run
 * is withdrawn - it read a comment or an issue message deleted since - says so under it in one
 * line, which says whether the run's own delete withdrew it.
 *
 * The box at its top takes the focus as it opens, and typing filters the rows by the words of
 * every question of a conversation. While the focus is in it, the first row is drawn selected,
 * as Enter in it opens that row; Down and Up move the focus through the rows, Up from the first
 * going back to the box, Enter opens the row the focus is on, and typing on a row goes on in the
 * box. The Enter that confirms an input method's conversion in the box opens nothing. The surface
 * draws no box of its own beside this one while History shows.
 */
export function History({ onOpen }: { readonly onOpen: () => void }): ReactNode {
  const conversations = useQuery(goingConversationsQuery());
  const runs = useGoingValue((of) => of.runs);
  const undone = useGoingValue((of) => of.undone);
  const [filter, setFilter] = useState("");
  // Whether the focus is in the box, where the first row is the one Enter opens.
  const [inBox, setInBox] = useState(true);
  const box = useRef<HTMLInputElement>(null);
  const list = useRef<HTMLDivElement>(null);
  const ids = useId();

  useEffect(() => {
    box.current?.focus();
  }, []);

  const open = (conversation: GoingConversation) => {
    openConversation(conversation);
    onOpen();
  };
  const now = new Date();
  const needle = filter.trim().toLowerCase();
  const listed = (conversations.data?.rows ?? []).flatMap((conversation): Listed[] => {
    // Dispatch lists a conversation's runs the newest first.
    const held = conversation.runs ?? [];
    const newest = held[0];
    const asked = held.map((run) => run.words.toLowerCase());
    if (newest === undefined || (needle !== "" && !asked.some((words) => words.includes(needle)))) {
      return [];
    }
    const followed = runs.get(newest.id);
    const state =
      followed === undefined
        ? stateOfRecord(newest.state, newest.stop_reason)
        : shownState(followed);
    const reason = followed?.frames.end?.reason ?? newest.stop_reason ?? "";
    const withdrawal =
      followed === undefined ? withdrawalOf(newest.stop_reason ?? "") : runWithdrawal(followed);
    return [
      {
        conversation,
        group: groupOf(Date.parse(conversation.newest_at), now),
        refused:
          (state === "stopped_bound" || state === "stopped_person") && providerRefused(reason)
            ? providerRefusedWords(reason)
            : undefined,
        state,
        withdrawal,
        title: titleWords(held.at(-1)?.words ?? ""),
        // Undo is offered only while the run holds a write Undo can still take back, as
        // Dispatch keeps it: not once it was undone, on this page or another, and not for a
        // run whose writes were all messages, which are sent, not undone. An Undo pressed on
        // this page keeps its row, which says what it did.
        undo: runEnded(state) && (newest.undoable || undone.has(newest.id)) ? newest.id : undefined,
      },
    ];
  });
  const groups = GROUPS.map(({ group, words }) => ({
    group,
    rows: listed.filter((row) => row.group === group),
    words,
  })).filter(({ rows }) => rows.length > 0);
  // The row Enter in the box opens, drawn selected while the focus is there.
  const firstRow = groups[0]?.rows[0]?.conversation.id;

  const walk = (event: KeyboardEvent<HTMLButtonElement>) => {
    if (event.key === "ArrowDown" || event.key === "ArrowUp") {
      event.preventDefault();
      const rows = [...(list.current?.querySelectorAll<HTMLElement>("[data-history-row]") ?? [])];
      const to = rows.indexOf(event.currentTarget) + (event.key === "ArrowDown" ? 1 : -1);
      (to < 0 ? box.current : (rows[to] ?? event.currentTarget))?.focus();
    } else if (
      // A character typed on a row, with no key held that makes it a command, goes on in the
      // box; Space presses the row, as it presses any button.
      event.key.length === 1 &&
      event.key !== " " &&
      !event.ctrlKey &&
      !event.metaKey &&
      !event.altKey
    ) {
      event.preventDefault();
      setFilter((words) => words + event.key);
      box.current?.focus();
    }
  };

  let shown: ReactNode;
  if (conversations.isPending) {
    shown = (
      <p className={`py-1 text-sm ${textMutedOnSurface}`} role="status">
        Reading History…
      </p>
    );
  } else if (conversations.isError) {
    shown = (
      <QueryError
        message="History could not be read."
        onRetry={() => void conversations.refetch()}
        retrying={conversations.isFetching}
      />
    );
  } else if (groups.length === 0) {
    shown = (
      <p className={`py-1 text-sm ${textMutedOnSurface}`}>
        {needle === ""
          ? "No conversation in the last 7 days."
          : `No conversation matches “${filter.trim()}”.`}
      </p>
    );
  } else {
    shown = (
      <div ref={list}>
        {groups.map(({ group, rows, words }) => (
          <section
            aria-labelledby={`${ids}-${group}`}
            data-group={group}
            data-testid="going-history-group"
            key={group}
          >
            <h3
              className={`pt-2 pb-1 text-xs font-semibold ${textMutedOnSurface}`}
              id={`${ids}-${group}`}
            >
              {words}
            </h3>
            <ul>
              {rows.map((row) => (
                <li
                  data-conversation={row.conversation.id}
                  data-state={row.state}
                  data-testid="going-history-conversation"
                  key={row.conversation.id}
                >
                  <button
                    className={`flex w-full min-w-0 items-baseline gap-2 rounded px-2 py-1.5 text-left text-sm ${surfaceMutedHoverBg} ${inBox && row.conversation.id === firstRow ? surfaceMutedBg : ""}`}
                    data-selected={inBox && row.conversation.id === firstRow ? "true" : undefined}
                    data-history-row=""
                    data-testid="going-history-open"
                    onClick={() => open(row.conversation)}
                    onKeyDown={walk}
                    type="button"
                  >
                    <span
                      className={`min-w-0 flex-1 truncate ${textPrimaryOnSurface}`}
                      data-testid="going-history-title"
                    >
                      {row.title}
                    </span>
                    <span
                      className={`shrink-0 text-xs ${textSecondaryOnSurface}`}
                      data-testid="going-history-state"
                    >
                      {STATE_WORDS[row.state]}
                    </span>
                    {/* The row's time sits on the card at rest and on the muted fill while the
                        row is the one Enter opens, so it takes the muted shade that reads on both. */}
                    <span className={`shrink-0 text-xs ${textMutedOnSurfaceMuted}`}>
                      <Timestamp at={row.conversation.newest_at} />
                    </span>
                  </button>
                  {row.refused === undefined ? null : (
                    <p
                      className={`px-2 pb-1 text-xs ${textSecondaryOnSurface}`}
                      data-testid="going-history-refused"
                    >
                      {row.refused}
                    </p>
                  )}
                  {row.withdrawal === undefined ? null : (
                    <p
                      className={`px-2 pb-1 text-xs ${textSecondaryOnSurface}`}
                      data-testid="going-history-withdrawn"
                    >
                      {WITHDRAWN_WORDS[row.withdrawal]}
                    </p>
                  )}
                  {row.undo === undefined ? null : (
                    <div className="px-2 pb-1">
                      {/* One run's Undo: the row names its conversation's newest run, and a newer
                          one takes the place of the Undo, never its press or its refusal. */}
                      <RunUndo key={row.undo} run={row.undo} />
                    </div>
                  )}
                </li>
              ))}
            </ul>
          </section>
        ))}
      </div>
    );
  }

  return (
    // The section notes where the focus went, so the first row is marked as Enter's only while
    // the focus is in the filter box.
    <section
      aria-label="History"
      className="max-h-[50vh] min-h-0 overflow-y-auto px-3 py-2"
      data-testid="going-history"
      onFocus={(event) => setInBox(event.target === box.current)}
    >
      <SearchBox
        className="w-full"
        data-testid="going-history-filter"
        label="Filter history"
        onBlur={() => setInBox(false)}
        onChange={setFilter}
        onKeyDown={(event) => {
          // A key of an input method's composition is the input method's (`inputMethodKey`): the
          // Enter that confirms a conversion opens no conversation.
          if (inputMethodKey(event.nativeEvent)) return;
          if (event.key === "ArrowDown") {
            event.preventDefault();
            list.current?.querySelector<HTMLElement>("[data-history-row]")?.focus();
          } else if (event.key === "Enter") {
            event.preventDefault();
            const first = groups[0]?.rows[0];
            if (first !== undefined) open(first.conversation);
          }
        }}
        placeholder="Filter history…"
        ref={box}
        value={filter}
      />
      {shown}
    </section>
  );
}
