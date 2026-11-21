import { useQuery, useQueryClient } from "@tanstack/react-query";

import { goingConversationsQuery, whoAmIQuery } from "../../api/queries";
import type { GoingContextInput, GoingRunInput } from "../../api/types";
import {
  continueRun,
  conversationWorking,
  goingState,
  movePanel,
  newestRun,
  startRun,
  stopRun,
} from "./going-store";
import { type PlanAnswering, useOpenPlan } from "./usePlan";

/** What the bar's box and the panel's box do with the person's words and keys. */
export interface Asking {
  /** The open plan, as this surface answers it. */
  readonly answering: PlanAnswering;
  /** Whether the open conversation's newest run waits on its plan: Enter then answers it. */
  readonly planWaits: boolean;
  /** Enter with `words` in the box: approves the waiting plan from an empty box, revises it in
   *  words, or starts a run of the words, of `kind` where one is given (`new_issue` for the line
   *  File as a new issue); answers whether it did any. */
  readonly enter: (words: string, kind?: GoingRunInput["kind"]) => boolean;
  /** Esc in the box: stops the run that works where the box is empty, else moves the
   *  conversation as Esc does (`panelNext`). */
  readonly escape: (words: string) => void;
  readonly stop: () => void;
  readonly onContinue: (run: string) => void;
}

/**
 * The one behaviour of the conversation's two boxes, the bar over the page and the panel at the
 * side, with the page's `context`: Enter approves a plan that waits (R4: Enter approves) or, with
 * words, says what to change of it; with no plan waiting, it starts a run of the words, a
 * follow-up of the conversation where its newest run has ended. Esc in the empty box while the run
 * works stops the run (R1, R10). Each run started here is a run History lists, so History is read
 * again once it starts.
 */
export function useAsk(context: GoingContextInput): Asking {
  const queryClient = useQueryClient();
  const login = useQuery(whoAmIQuery()).data?.login;
  const answering = useOpenPlan();
  const planWaits = answering.plan?.state === "waiting";
  const listed = (start: Promise<boolean>) =>
    void start.then((started) => {
      if (started) {
        void queryClient.invalidateQueries({ queryKey: goingConversationsQuery().queryKey });
      }
    });
  return {
    answering,
    enter: (words, kind) => {
      const said = words.trim();
      if (planWaits) {
        if (said === "") answering.approve();
        else answering.revise(said);
        return true;
      }
      if (said === "" || goingState().starting) {
        return false;
      }
      listed(startRun(said, context, login, undefined, kind));
      return true;
    },
    escape: (words) => {
      movePanel(
        words === "" ? { kind: "esc_empty", working: conversationWorking() } : { kind: "esc_full" },
        login
      );
    },
    onContinue: (run) => listed(continueRun(run, context, login)),
    planWaits,
    stop: () => {
      const run = newestRun();
      if (run !== undefined) stopRun(run.id, login);
    },
  };
}
