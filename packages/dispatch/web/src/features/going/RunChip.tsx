import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useEffect, useRef } from "react";

import { goingPlanQuery } from "../../api/queries";
import {
  badgeHigh,
  borderDefault,
  calloutWarningBorder,
  card,
  textPrimaryOnSurface,
  unreadDotBg,
} from "../../theme/classes";
import {
  type GoingState,
  goingState,
  openRun,
  type RunEntry,
  shownState,
  titleWords,
  useGoing,
} from "./going-store";
import { showNotice } from "./Notice";
import { planTitle } from "./plan-rows";
import { type RunState, runEnded } from "./run-states";

/** How long the notice of a run that ended while the bar was closed shows (design v3, state 11). */
const RUN_NOTICE_MS = 6_000;

/** The word the chip names each state by, and its rank: the chip names the most urgent first. */
const CHIP_STATES: Readonly<
  Record<RunState, { readonly word: string; readonly rank: number } | undefined>
> = {
  done: { rank: 3, word: "Done" },
  failed: { rank: 1, word: "Failed" },
  idle: undefined,
  stopped_bound: { rank: 4, word: "Stopped" },
  stopped_person: { rank: 4, word: "Stopped" },
  waiting: { rank: 0, word: "Needs you" },
  working: { rank: 2, word: "Working" },
};

/** One conversation the chip can name: its newest run, how that run stands and the rank of that
 *  state, and its title. */
interface GoingOn {
  readonly run: RunEntry;
  readonly state: RunState;
  readonly rank: number;
  readonly title: string;
}

/** When Dispatch started `run`, on its own clock (`created_at`). Every run the page holds has one,
 *  so runs started here and runs read from History order alike; `since` would not: it is this
 *  page's clock for a run started here and Dispatch's for any other. */
function startedAt(run: RunEntry): number {
  return Date.parse(run.record.created_at);
}

/** The title of `run`'s conversation: its first question, the words of the first of its runs
 *  the page holds, as a title (`titleWords`). */
function titleOf(of: GoingState, run: RunEntry): string {
  let first = run;
  for (const other of of.runs.values()) {
    if (
      other.record.conversation === run.record.conversation &&
      startedAt(other) < startedAt(first)
    ) {
      first = other;
    }
  }
  return titleWords(first.record.words);
}

/**
 * The conversations of this page that go on while none is on the screen (R11): each whose newest
 * run was started on this page and works, waits for the person, or ended and was not seen since,
 * the most urgent first - Needs you, then Failed, then Working, then Done, then Stopped - and the
 * newest first among equals.
 */
function goingOn(of: GoingState): readonly GoingOn[] {
  const newest = new Map<string, RunEntry>();
  for (const run of of.runs.values()) {
    const held = newest.get(run.record.conversation);
    if (held === undefined || startedAt(run) >= startedAt(held)) {
      newest.set(run.record.conversation, run);
    }
  }
  const shown: GoingOn[] = [];
  for (const run of newest.values()) {
    const state = shownState(run);
    const named = CHIP_STATES[state];
    const goes = state === "working" || state === "waiting" || (runEnded(state) && !run.seen);
    if (run.startedHere && goes && named !== undefined) {
      shown.push({ rank: named.rank, run, state, title: titleOf(of, run) });
    }
  }
  return shown.sort((a, b) => a.rank - b.rank || startedAt(b.run) - startedAt(a.run));
}

/**
 * Shows the notice at the bottom right of the page (design v3, state 11) for 6 s when a run of
 * this page that the page saw work ends, or comes to wait on its plan, while the bar is closed:
 * "Done: ‹title› · Open", "Needs you: Change DEMO-10 · Review", with the plan's own title, or
 * "Failed: ‹title› · Open". Each action opens the run's conversation in the bar, on its plan's
 * card for Review. None shows while the conversation is on the screen, over the page or at the
 * side.
 */
function useRunEndNotices(going: GoingState): void {
  const queryClient = useQueryClient();
  // How each run stood when the store was last drawn, so a notice comes once, on the change.
  const stood = useRef(new Map<string, RunState>());
  useEffect(() => {
    const before = stood.current;
    stood.current = new Map([...going.runs.values()].map((run) => [run.id, shownState(run)]));
    if (going.place !== "closed") return;
    for (const run of going.runs.values()) {
      const state = shownState(run);
      if (!run.startedHere || before.get(run.id) !== "working" || state === "working") continue;
      const title = titleOf(going, run);
      if (state === "done" || state === "failed") {
        showNotice({
          actions: [{ label: "Open", press: () => openRun(run.id) }],
          ms: RUN_NOTICE_MS,
          text: `${state === "done" ? "Done" : "Failed"}: ${title}`,
        });
      } else if (state === "waiting") {
        // The notice names the plan by its card's title, read now: the plan is new.
        void queryClient
          .fetchQuery({ ...goingPlanQuery(run.id), staleTime: 0 })
          .then(
            (plan) => (plan.state === "waiting" ? planTitle(plan.rows ?? []) : title),
            () => title
          )
          .then((named) => {
            const now = goingState();
            const still = now.runs.get(run.id);
            if (now.place !== "closed" || still === undefined || shownState(still) !== "waiting") {
              return;
            }
            showNotice({
              actions: [{ label: "Review", press: () => openRun(run.id) }],
              ms: RUN_NOTICE_MS,
              text: `Needs you: ${named}`,
            });
          });
      }
    }
  }, [going, queryClient]);
}

/** The chip's mark before its words: a spinner while the run works, a blue dot on an answer the
 *  person has not seen. */
function ChipMark({ state }: { readonly state: RunState }): ReactNode {
  if (state === "working") {
    return (
      <span
        aria-hidden="true"
        className="inline-block size-3 shrink-0 rounded-full border-2 border-current border-t-transparent motion-safe:animate-spin"
      />
    );
  }
  return state === "done" ? (
    <span
      aria-hidden="true"
      className={`inline-block size-2 shrink-0 rounded-full ${unreadDotBg}`}
      data-testid="going-chip-unread"
    />
  ) : null;
}

/** The chip on the screen: the conversation it names, and how many more go on. A plan that waits
 *  is named by its card's title once read. It is a pill of its own - a background, a border and a
 *  small shadow - 24 px tall, a target a pointer and a finger can press (review R4; WCAG 2.2's
 *  2.5.8), with room above it rather than against the screen's edge (review n6): 8 px from the
 *  `md` width up, and 4 px below it, where a page's title spans the row under it. The shell
 *  floats it over the top of the page (`app.tsx`), from the 24 px band above the page's first
 *  row, so it moves nothing; its foot reaches a few pixels over that row's own leading, clear of
 *  the title's letters. The button is the whole pill, its rule included (`-m-px`), and takes no
 *  44 px floor (`data-run-chip`, styles.css); it keeps that height rather than stretching. */
function RunPill({ going, more }: { readonly going: GoingOn; readonly more: number }): ReactNode {
  const waiting = going.state === "waiting";
  const plan = useQuery({ ...goingPlanQuery(going.run.id), enabled: waiting });
  const named =
    waiting && plan.data?.state === "waiting" ? planTitle(plan.data.rows ?? []) : going.title;
  const look = waiting
    ? `${badgeHigh.bg} ${badgeHigh.text} ${calloutWarningBorder}`
    : `${card} ${borderDefault} ${textPrimaryOnSurface}`;
  return (
    <p
      className={`mt-1 flex max-w-[min(32rem,calc(100vw-2rem))] items-center self-start rounded-full border text-xs leading-4 font-medium shadow-md md:mt-2 ${look}`}
      data-run-chip=""
      data-state={going.state}
      data-testid="going-chip"
      role="status"
    >
      <button
        className="-m-px flex min-h-6 min-w-0 items-center gap-2 rounded-full px-3"
        data-testid="going-chip-open"
        onClick={() => openRun(going.run.id)}
        title="Open this conversation in the bar"
        type="button"
      >
        <ChipMark state={going.state} />
        <span className="min-w-0 truncate" data-testid="going-chip-text">
          {CHIP_STATES[going.state]?.word} · {named}
        </span>
        {more === 0 ? null : (
          <>
            {" "}
            <span className="shrink-0" data-testid="going-chip-more">
              +{more}
            </span>
          </>
        )}
      </button>
    </p>
  );
}

/**
 * The chip at the top of the page (R11; design v3, state 11) while no conversation is on the
 * screen: it names one conversation of this page and how it stands, in words - "Working ·
 * ‹title›" with a spinner, amber "Needs you · Change DEMO-10", "Done · ‹title›" with a blue dot
 * until it is seen, "Failed · ‹title›", "Stopped · ‹title›" - the most urgent where more than one
 * goes on, with how many more ("+1"). A click opens that conversation in the bar. It hides when
 * nothing goes on. It also shows the notice of a run that ends while the bar is closed.
 */
export function RunChip(): ReactNode {
  const going = useGoing();
  useRunEndNotices(going);
  if (going.place !== "closed") {
    return null;
  }
  const [first, ...others] = goingOn(going);
  return first === undefined ? null : <RunPill going={first} more={others.length} />;
}
