import { type ReactNode, useEffect, useId } from "react";

import {
  selectedCardBorder,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { TimedWords } from "../refs/dispatch-time";
import { drawOnPage } from "./going-store";
import { PlanControls, PlanRowView, usePlanDraft } from "./PlanRows";
import { type PlanSurface, rowsOnSurface } from "./plan-surfaces";
import { useOpenPlan } from "./usePlan";

/**
 * The rows of the open conversation's plan that this page shows the items of (`plan-surfaces.ts`),
 * drawn on the page itself in a "Planned" outline, each with the look it has in the plan card:
 * each new issue, move and new value with its tick and its edit, and the plan's Approve and
 * Discard. The bar's plan card leaves these rows out while the page draws them, and its Approve
 * approves them too. Nothing is drawn, and nothing is read, while no conversation is on the
 * screen.
 */
export function PlannedChanges({ surface }: { readonly surface: PlanSurface }): ReactNode {
  const id = useId();
  const answering = useOpenPlan();
  const plan = answering.plan;
  const draft = usePlanDraft(plan);
  const rows =
    plan === undefined || answering.run === undefined || plan.run !== answering.run
      ? []
      : rowsOnSurface(surface, plan.rows ?? []);
  const shown = rows.join(",");
  // biome-ignore lint/correctness/useExhaustiveDependencies: `shown` is the rows, by value
  useEffect(() => {
    drawOnPage(id, rows);
  }, [id, shown]);
  useEffect(() => () => drawOnPage(id, []), [id]);
  if (plan === undefined || draft === undefined || rows.length === 0) {
    return null;
  }
  return (
    <section
      aria-label="Planned changes"
      className={`space-y-2 rounded-lg border-2 border-dashed p-3 ${selectedCardBorder}`}
      data-plan={plan.plan}
      data-surface={surface.kind}
      data-testid="going-plan-on-page"
    >
      <h2 className={`text-sm font-semibold ${textPrimaryOnSurface}`}>Planned changes</h2>
      {plan.summary === "" ? null : (
        <p className={`text-sm ${textSecondaryOnSurface}`}>
          <TimedWords text={plan.summary} />
        </p>
      )}
      <ul>
        {(plan.rows ?? [])
          .filter((row) => rows.includes(row.row))
          .map((row) => (
            <PlanRowView draft={draft} key={row.row} plan={plan} row={row} />
          ))}
      </ul>
      <PlanControls answering={answering} draft={draft} enterApproves={false} />
    </section>
  );
}
