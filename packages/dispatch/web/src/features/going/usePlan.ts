import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";

import {
  apiErrorCodeIn,
  apiErrorMessage,
  goingPlanApproveRequest,
  goingPlanRejectRequest,
  goingPlanReviseRequest,
} from "../../api/client";
import { goingConversationsQuery, goingPlanQuery, goingWaitingPlansQuery } from "../../api/queries";
import type { GoingPlanAnswer } from "../../api/types";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import { claimPlanRead, draftOf, newestRun, useGoingValue } from "./going-store";
import { approvalOf } from "./plan-rows";

/** The target every write about the plan of `run` goes under - an answer to it, and its run's
 *  Undo - so two surfaces that write about one plan go one at a time. */
export function planTarget(run: string): string {
  return `going-plan:${run}`;
}

/** Dispatch's refusal of an approval naming a version of the plan older than the plan now, which
 *  every surface that approves a plan meets by reading the plan again. */
export const PLAN_CHANGED: ReadonlySet<string> = new Set(["GOING_PLAN_CHANGED"]);

/** What a surface answers a plan with, and how that stands. */
export interface PlanAnswering {
  /** The plan, once read; undefined while it is read, or for a run that proposed none. */
  readonly plan: GoingPlanAnswer | undefined;
  /** Why the plan could not be read, with its Retry; undefined unless the read failed. */
  readonly unreadable:
    | { readonly message: string; readonly retry: () => void; readonly retrying: boolean }
    | undefined;
  /** Approves the waiting plan with the person's ticks and edits (Enter, or Approve), naming the
   *  version of the plan drawn, which Dispatch refuses once the plan has changed since. */
  readonly approve: () => void;
  /** Rejects the waiting plan. */
  readonly reject: () => void;
  /** Says in words what to change; the run proposes another plan in its place. */
  readonly revise: (words: string) => void;
  readonly pending: boolean;
  /** Why Dispatch refused the last answer to the plan drawn now, in its words; undefined under
   *  any other plan, of this run or of another. */
  readonly refusal: string | undefined;
}

/**
 * The latest plan of run `run` (`GET /api/v1/going/runs/{id}/plan`), read once the run's stream
 * has carried a plan frame and again at each frame after that tells it to (`planReads`: each plan
 * frame, and the run's end), and the person's three answers to it: approve, with the ticks and
 * edits the store holds for it, reject, and revise in words. Each answer is the person's write,
 * under the plan's own target, so two surfaces that answer one plan go one at a time. A surface
 * that mounts reads the plan again; each new reading frame reads it again once, however many
 * surfaces draw it (`claimPlanRead`).
 */
export function usePlan(run: string | undefined, planReads: number): PlanAnswering {
  const queryClient = useQueryClient();
  const query = goingPlanQuery(run ?? "");
  const plan = useQuery({
    ...query,
    enabled: run !== undefined && planReads > 0,
    retry: false,
  });
  const mounted = useRef(false);
  // biome-ignore lint/correctness/useExhaustiveDependencies: each new reading frame is the reason to read the plan again
  useEffect(() => {
    const mounting = !mounted.current;
    mounted.current = true;
    if (run !== undefined && planReads > 1 && (claimPlanRead(run, planReads) || mounting)) {
      void queryClient.invalidateQueries({ exact: true, queryKey: query.queryKey });
    }
  }, [planReads]);
  // The refusal names the plan it answered, so the hook, which stays mounted as the open run
  // changes, shows it only under that plan.
  const [refused, setRefused] = useState<{
    readonly message: string;
    readonly plan: number;
    readonly run: string;
  }>();
  const refuse = (error: unknown, answered: GoingPlanAnswer) =>
    setRefused({
      message: apiErrorMessage(error, "The answer to the plan did not reach Dispatch."),
      plan: answered.plan,
      run: answered.run,
    });
  const settle = (answered: GoingPlanAnswer | null) => {
    if (answered !== null) {
      queryClient.setQueryData(query.queryKey, answered);
    } else {
      void queryClient.invalidateQueries({ exact: true, queryKey: query.queryKey });
    }
    void queryClient.invalidateQueries({ queryKey: goingWaitingPlansQuery().queryKey });
    void queryClient.invalidateQueries({ queryKey: goingConversationsQuery().queryKey });
  };
  const common = {
    inline: true,
    onSuccess: settle,
    target: () => planTarget(run ?? ""),
  };
  const approve = useKeyedWrite<GoingPlanAnswer, GoingPlanAnswer>({
    ...common,
    label: () => "Approve the command bar's plan",
    // An approval names the version of the plan the page shows; one refused because the plan
    // changed since reads the plan again, so the person sees what they would approve under the
    // refusal, and their next approval names that version.
    onError: (error, answered) => {
      refuse(error, answered);
      if (apiErrorCodeIn(error, PLAN_CHANGED)) {
        void queryClient.invalidateQueries({
          exact: true,
          queryKey: goingPlanQuery(answered.run).queryKey,
        });
      }
    },
    request: (answered) =>
      goingPlanApproveRequest(
        answered.run,
        approvalOf(answered, draftOf(answered.run, answered.plan))
      ),
  });
  const reject = useKeyedWrite<GoingPlanAnswer, GoingPlanAnswer>({
    ...common,
    label: () => "Reject the command bar's plan",
    onError: refuse,
    request: (answered) => goingPlanRejectRequest(answered.run, { plan: answered.plan }),
  });
  const revise = useKeyedWrite<GoingPlanAnswer, { plan: GoingPlanAnswer; words: string }>({
    ...common,
    label: () => "Revise the command bar's plan",
    onError: (error, { plan: answered }) => refuse(error, answered),
    request: ({ plan: answered, words }) =>
      goingPlanReviseRequest(answered.run, { plan: answered.plan, words }),
  });
  const shown = plan.data;
  const waiting = shown?.state === "waiting" ? shown : undefined;
  const refusal =
    refused !== undefined &&
    shown !== undefined &&
    refused.run === shown.run &&
    refused.plan === shown.plan
      ? refused.message
      : undefined;
  return {
    approve: () => {
      if (waiting === undefined) return;
      setRefused(undefined);
      approve.mutate(waiting);
    },
    pending: approve.isPending || reject.isPending || revise.isPending,
    plan: shown,
    refusal,
    reject: () => {
      if (waiting === undefined) return;
      setRefused(undefined);
      reject.mutate(waiting);
    },
    revise: (words) => {
      if (waiting === undefined || words.trim() === "") return;
      setRefused(undefined);
      revise.mutate({ plan: waiting, words: words.trim() });
    },
    unreadable: plan.isError
      ? {
          message: apiErrorMessage(plan.error, "The run's plan could not be read."),
          retry: () => void plan.refetch(),
          retrying: plan.isFetching,
        }
      : undefined,
  };
}

/** The plan of the conversation open in the bar or in the side panel: its newest run's, while the
 *  conversation is on the screen. A plan that waits in the Inbox draws on no page until its
 *  conversation is opened. A surface that draws it is drawn again only when the newest run or
 *  its reading frames change, not on each frame of the run. */
export function useOpenPlan(): PlanAnswering & { readonly run: string | undefined } {
  const run = useGoingValue((of) => (of.place === "closed" ? undefined : newestRun(of)?.id));
  const planReads = useGoingValue((of) =>
    of.place === "closed" ? 0 : (newestRun(of)?.frames.planReads ?? 0)
  );
  const answering = usePlan(run, planReads);
  return { ...answering, run };
}
