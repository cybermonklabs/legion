import { useQuery, useQueryClient } from "@tanstack/react-query";

import { goingConversationsQuery, whoAmIQuery } from "../../api/queries";
import type { GoingContextInput, GoingLine, GoingRun } from "../../api/types";
import {
  continueRun,
  conversationWorking,
  goingState,
  handOffToPanel,
  movePanel,
  newestRun,
  offersContinue,
  offersRetry,
  shownState,
  startRun,
  stopRun,
  useGoingValue,
} from "./going-store";
import { planWaits } from "./plan-rows";
import { type PlanAnswering, useOpenPlan } from "./usePlan";

/** What the bar's box and the panel's box do with the person's words and keys. */
export interface Asking {
  /** The open plan, as this surface answers it. */
  readonly answering: PlanAnswering;
  /** Whether the open conversation's newest run waits on its plan: Enter then answers it. A plan
   *  of a withdrawn run waits for nobody (`planWaits`), so Enter neither approves it nor sends
   *  the words in the box to it, where they would be lost. */
  readonly planWaits: boolean;
  /** Whether the open conversation's newest run waits on its plan as its frames show it, which
   *  can come before the plan is read: until then Enter answers nothing and starts no run. */
  readonly runWaits: boolean;
  /** Enter with `words` in the box: approves the waiting plan from an empty box, revises it in
   *  words, continues from an empty box a run whose end offers Continue - a bound of its level, or
   *  the person's own Stop (design v3, state 9) - tries again, from an empty box, a run that
   *  failed and offers Try again (review a9), or starts a run of the words, which records
   *  `line`, the line on top Enter was pressed on, where the words were sent from it; answers
   *  whether it did any. */
  readonly enter: (words: string, line?: GoingLine) => boolean;
  /** Open in panel on the bar's card and Enter on its line Ask, one action: submits `words` once
   *  as a run of the page's `context` at the level the bar is set to, with `line` as the line it
   *  was pressed on, and hands the conversation to the panel at the side. It answers whether it
   *  did, and does nothing - starts no run and leaves the bar where it is - for empty words, a
   *  plan that waits, a run that works or a start that is out, so a press made twice, or on a
   *  card of words that were typed past, cannot show a second conversation. */
  readonly openInPanel: (words: string, line: GoingLine) => boolean;
  /** Esc in the box: stops the run that works where the box is empty, else moves the
   *  conversation as Esc does (`panelNext`). */
  readonly escape: (words: string) => void;
  readonly stop: () => void;
  /** Continue on a run a bound or its person stopped: a follow-up of `level`, else of the run's
   *  own level. */
  readonly onContinue: (run: string, level?: GoingRun["level"]) => void;
  /** Retry under an answer: the same `words` as Enter in the box takes them, so it never starts a
   *  conversation beside the open one's waiting plan: a revision of the plan that waits, nothing
   *  while the newest run's plan is still being read or a start is out, else a new run, a
   *  follow-up of the conversation where its newest run has ended. */
  readonly retry: (words: string) => void;
}

/**
 * The one behaviour of the conversation's two boxes, the bar over the page and the panel at the
 * side, with the page's `context`: Enter approves a plan that waits (R4: Enter approves) or, with
 * words, says what to change of it; with no plan waiting, it starts a run of the words, a
 * follow-up of the conversation where its newest run has ended, and in the empty box it continues
 * a run whose end offers Continue (R10, Continue ↵). Esc in the empty box while the run works
 * stops the run (R1, R10). Each run started here is a run History lists, so History is read
 * again once it starts.
 */
export function useAsk(context: GoingContextInput): Asking {
  const queryClient = useQueryClient();
  const login = useQuery(whoAmIQuery()).data?.login;
  const answering = useOpenPlan();
  const waits = planWaits(answering.plan);
  const runWaits = useGoingValue((of) => {
    const run = of.place === "closed" ? undefined : newestRun(of);
    return run !== undefined && shownState(run) === "waiting";
  });
  const listed = (start: Promise<boolean>) =>
    void start.then((started) => {
      if (started) {
        void queryClient.invalidateQueries({ queryKey: goingConversationsQuery().queryKey });
      }
    });
  const enter = (words: string, line?: GoingLine): boolean => {
    const said = words.trim();
    if (waits) {
      // Enter stands for the card's Approve and keeps its guard (`approvable`): it approves
      // nothing with no row ticked, nor again while the first approval is out.
      if (said === "") return answering.approve();
      answering.revise(said);
      return true;
    }
    if (runWaits || goingState().starting) {
      return false;
    }
    const newest = newestRun();
    if (said === "") {
      if (newest === undefined) return false;
      if (offersContinue(newest)) {
        listed(continueRun(newest.id, context, login));
        return true;
      }
      if (!offersRetry(newest)) return false;
      // Try again runs the failed run's own words again, as its button does.
      listed(startRun(newest.record.words, context, login, {}));
      return true;
    }
    listed(startRun(said, context, login, { line }));
    return true;
  };
  const openInPanel = (words: string, line: GoingLine): boolean => {
    const said = words.trim();
    const newest = newestRun();
    if (
      said === "" ||
      waits ||
      runWaits ||
      goingState().starting !== undefined ||
      (newest !== undefined && shownState(newest) === "working")
    ) {
      return false;
    }
    if (!enter(said, line)) {
      return false;
    }
    handOffToPanel();
    return true;
  };
  return {
    answering,
    enter,
    escape: (words) => {
      movePanel(
        words === "" ? { kind: "esc_empty", working: conversationWorking() } : { kind: "esc_full" },
        login
      );
    },
    onContinue: (run, level) => listed(continueRun(run, context, login, level)),
    openInPanel,
    planWaits: waits,
    retry: (words) => {
      enter(words);
    },
    runWaits,
    stop: () => {
      const run = newestRun();
      if (run !== undefined) stopRun(run.id, login);
    },
  };
}
