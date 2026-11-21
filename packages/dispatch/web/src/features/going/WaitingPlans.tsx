import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useState } from "react";

import { api, apiErrorCodeIn, apiErrorMessage, goingPlanApproveRequest } from "../../api/client";
import { goingConversationsQuery, goingPlanQuery, goingWaitingPlansQuery } from "../../api/queries";
import type { GoingConversation, GoingPlanAnswer, GoingWaitingPlan } from "../../api/types";
import {
  borderDefault,
  card,
  dangerText,
  primaryButton,
  secondaryButtonCompact,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { Timestamp } from "../refs/Timestamp";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import { openConversation } from "./going-store";
import { PLAN_CHANGED, planTarget } from "./usePlan";

/** Opens the conversation of a waiting plan in the bar: the conversation as History lists it
 *  (`listed`), or, where History does not list it yet, its run alone. */
async function openPlanConversation(
  plan: GoingWaitingPlan,
  listed: readonly GoingConversation[]
): Promise<void> {
  const conversation = listed.find((row) => row.id === plan.conversation);
  if (conversation !== undefined) {
    openConversation(conversation);
    return;
  }
  const run = await api.goingRun(plan.run);
  openConversation({ id: run.conversation, newest_at: run.created_at, runs: [run] });
}

/** One plan that waits: the words its run was asked, what the plan does, how many of its changes
 *  wait, Approve, which runs every change as the plan shows it, and Open, which shows its
 *  conversation in the bar to tick, edit or revise it there. */
function WaitingPlanRow({ plan }: { readonly plan: GoingWaitingPlan }): ReactNode {
  const queryClient = useQueryClient();
  // The row stays mounted as its run proposes another plan, so a refusal names the plan it was
  // about and shows only while the row shows that plan.
  const [refused, setRefused] = useState<{
    readonly message: string;
    readonly plan: number;
    readonly run: string;
  }>();
  const refusal =
    refused !== undefined && refused.run === plan.run && refused.plan === plan.plan
      ? refused.message
      : undefined;
  const approve = useKeyedWrite<GoingPlanAnswer, GoingWaitingPlan>({
    inline: true,
    label: () => "Approve the command bar's plan",
    onError: (error, waiting) => {
      const changed = apiErrorCodeIn(error, PLAN_CHANGED);
      setRefused({
        message: changed
          ? "This plan changed since the list read it. Open it to read what it does now before you approve it."
          : apiErrorMessage(error, "The approval did not reach Dispatch."),
        plan: waiting.plan,
        run: waiting.run,
      });
      // The list is read again, so the row holds the plan's version now and its next Approve
      // names that version rather than meeting the same refusal.
      if (changed) {
        void queryClient.invalidateQueries({ queryKey: goingWaitingPlansQuery().queryKey });
      }
    },
    onSuccess: (answered) => {
      if (answered !== null) {
        queryClient.setQueryData(goingPlanQuery(plan.run).queryKey, answered);
      }
      void queryClient.invalidateQueries({ queryKey: goingWaitingPlansQuery().queryKey });
    },
    // The approval names the version of the plan the list read, and Dispatch refuses it once the
    // plan has changed since (409 GOING_PLAN_CHANGED), which the row says under that plan.
    request: (waiting) =>
      goingPlanApproveRequest(waiting.run, { plan: waiting.plan, version: waiting.version }),
    target: (waiting) => planTarget(waiting.run),
  });
  return (
    <li
      className={`space-y-1 border-t py-2 first:border-t-0 ${borderDefault}`}
      data-run={plan.run}
      data-testid="inbox-going-plan"
    >
      <p className={`text-sm font-medium ${textPrimaryOnSurface}`}>{plan.words}</p>
      {plan.summary === "" ? null : (
        <p className={`text-sm ${textSecondaryOnSurface}`}>{plan.summary}</p>
      )}
      <div className="flex flex-wrap items-center gap-2">
        <button
          className={primaryButton}
          data-testid="inbox-going-plan-approve"
          disabled={approve.isPending}
          onClick={() => {
            setRefused(undefined);
            approve.mutate(plan);
          }}
          type="button"
        >
          Approve
        </button>
        <button
          className={secondaryButtonCompact}
          data-testid="inbox-going-plan-open"
          onClick={() => {
            setRefused(undefined);
            queryClient
              .fetchQuery(goingConversationsQuery())
              .then((page) => openPlanConversation(plan, page.rows ?? []))
              .catch((error: unknown) =>
                setRefused({
                  message: apiErrorMessage(error, "The conversation could not be read."),
                  plan: plan.plan,
                  run: plan.run,
                })
              );
          }}
          type="button"
        >
          Open
        </button>
        <span className={`text-xs ${textMutedOnSurface}`}>
          {plan.waiting === 1 ? "1 change waits" : `${plan.waiting} changes wait`} ·{" "}
          <Timestamp at={plan.created_at} />
        </span>
      </div>
      {refusal === undefined ? null : (
        <p className={`text-xs ${dangerText}`} role="alert">
          {refusal}
        </p>
      )}
    </li>
  );
}

/**
 * The plans of the person's runs that wait for them (R11), at the head of the Inbox
 * (`GET /api/v1/me/going-plans`): a run whose bar was closed made its plan, and the person can
 * approve it here hours later. The list is read again on each `going_plan.updated` that names the
 * person (`event-refresh.ts`), so a plan answered in another tab leaves it. Nothing shows while no
 * plan waits.
 */
export function WaitingPlans(): ReactNode {
  const plans = useQuery(goingWaitingPlansQuery());
  const rows = plans.data?.rows ?? [];
  if (rows.length === 0) {
    return null;
  }
  return (
    <section
      aria-labelledby="inbox-going-plans-heading"
      className={`rounded-lg border p-3 ${card}`}
      data-testid="inbox-going-plans"
    >
      <h2
        className={`text-sm font-semibold ${textPrimaryOnSurface}`}
        id="inbox-going-plans-heading"
      >
        Plans that wait for you
      </h2>
      <ul>
        {rows.map((plan) => (
          <WaitingPlanRow key={plan.run} plan={plan} />
        ))}
      </ul>
    </section>
  );
}
