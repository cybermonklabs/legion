import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useEffect, useState } from "react";

import { api, apiErrorCodeIn, apiErrorMessage, goingPlanApproveRequest } from "../../api/client";
import { goingConversationsQuery, goingPlanQuery, goingWaitingPlansQuery } from "../../api/queries";
import type { GoingConversation, GoingPlanAnswer, GoingWaitingPlan } from "../../api/types";
import { QueryError } from "../../components/QueryError";
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
import { TimedWords } from "../refs/dispatch-time";
import { Timestamp } from "../refs/Timestamp";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import { openConversation } from "./going-store";
import { planTitle } from "./plan-rows";
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

/** The separator between the parts of a row's line, which a screen reader does not read out. */
const dot = <span aria-hidden="true"> · </span>;

/**
 * One plan that waits, as the Inbox lists it (R11; design v3, state 11): "Plan · Change DEMO-10
 * · from ‹title› · 2 h ago" - the plan named as its card names it, once its rows are read (its
 * summary until then), and the conversation it came from by that conversation's title - with
 * Review, which opens the bar on that conversation and its plan's card, and Approve, which runs
 * every change that waits as the plan shows it and says how many, as the card's Approve does.
 */
function WaitingPlanRow({
  plan,
  title,
}: {
  readonly plan: GoingWaitingPlan;
  /** The title of the plan's conversation: its first question. */
  readonly title: string;
}): ReactNode {
  const queryClient = useQueryClient();
  const read = useQuery(goingPlanQuery(plan.run));
  // The row names the plan the list names: where the run proposed another plan since the plan
  // was last read, it is read again.
  const shown = read.data?.plan === plan.plan ? read.data : undefined;
  const behind = read.data !== undefined && read.data.plan < plan.plan;
  useEffect(() => {
    if (behind) {
      void queryClient.invalidateQueries({
        exact: true,
        queryKey: goingPlanQuery(plan.run).queryKey,
      });
    }
  }, [behind, plan.run, queryClient]);
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
          ? "This plan changed since the list read it. Review it to read what it does now before you approve it."
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
      className={`space-y-2 border-t py-2 first:border-t-0 ${borderDefault}`}
      data-run={plan.run}
      data-testid="inbox-going-plan"
    >
      <p className={`text-sm ${textSecondaryOnSurface}`}>
        <span className={textMutedOnSurface}>Plan</span>
        {dot}
        <span
          className={`font-medium ${textPrimaryOnSurface}`}
          data-testid="inbox-going-plan-title"
        >
          {shown === undefined ? <TimedWords text={plan.summary} /> : planTitle(shown.rows ?? [])}
        </span>
        {dot}
        <span data-testid="inbox-going-plan-from">from {title}</span>
        {dot}
        <span className={textMutedOnSurface}>
          <Timestamp at={plan.created_at} />
        </span>
      </p>
      <div className="flex flex-wrap items-center gap-2">
        <button
          className={primaryButton}
          data-testid="inbox-going-plan-review"
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
          Review
        </button>
        <button
          className={secondaryButtonCompact}
          data-testid="inbox-going-plan-approve"
          disabled={approve.isPending}
          onClick={() => {
            setRefused(undefined);
            approve.mutate(plan);
          }}
          type="button"
        >
          {plan.waiting === 1 ? "Approve 1 change" : `Approve ${plan.waiting} changes`}
        </button>
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
 * person (`event-refresh.ts`), so a plan answered in another tab leaves it. Each row names its
 * conversation by its title, its first question, as History lists it, else by the words of the
 * plan's own run. Nothing shows while no plan waits; a read of the list that failed says so, with
 * its Retry, above any plans an earlier read listed.
 */
export function WaitingPlans(): ReactNode {
  const plans = useQuery(goingWaitingPlansQuery());
  const rows = plans.data?.rows ?? [];
  const conversations = useQuery({ ...goingConversationsQuery(), enabled: rows.length > 0 });
  if (rows.length === 0 && !plans.isError) {
    return null;
  }
  // History lists a conversation's runs the newest first: its first question is its last run's.
  const titles = new Map(
    (conversations.data?.rows ?? []).map((listed) => [listed.id, listed.runs?.at(-1)?.words])
  );
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
      {plans.isError ? (
        <QueryError
          message={apiErrorMessage(plans.error, "The plans that wait for you could not be read.")}
          onRetry={() => void plans.refetch()}
          retrying={plans.isFetching}
        />
      ) : null}
      {rows.length === 0 ? null : (
        <ul>
          {rows.map((plan) => (
            <WaitingPlanRow
              key={plan.run}
              plan={plan}
              title={titles.get(plan.conversation) ?? plan.words}
            />
          ))}
        </ul>
      )}
    </section>
  );
}
