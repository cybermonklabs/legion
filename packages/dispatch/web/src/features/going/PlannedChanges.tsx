import { type ReactNode, useEffect, useId } from "react";

import { borderDefault, textPrimaryOnSurface, textSecondaryOnSurface } from "../../theme/classes";
import { draftOf, drawOnPage, useGoingValue } from "./going-store";
import { PlanControls, PlanRowView } from "./PlanRows";
import { type PlanSurface, rowsOnSurface } from "./plan-surfaces";
import { useOpenPlan } from "./usePlan";

/**
 * The rows of the open conversation's plan that this page shows the items of (`plan-surfaces.ts`),
 * drawn on the page itself: each new issue, move and new value with its tick and its edit, and the
 * plan's Approve and Reject. The bar's plan card leaves these rows out while the page draws them.
 * Nothing is drawn, and nothing is read, while no conversation is on the screen.
 */
export function PlannedChanges({ surface }: { readonly surface: PlanSurface }): ReactNode {
  const id = useId();
  const held = useGoingValue((of) => of.draft);
  const answering = useOpenPlan();
  const plan = answering.plan;
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
  if (plan === undefined || rows.length === 0) {
    return null;
  }
  const draft = draftOf(plan.run, plan.plan, held);
  return (
    <section
      aria-label="Planned changes"
      className={`space-y-2 rounded-lg border p-3 ${borderDefault}`}
      data-plan={plan.plan}
      data-surface={surface.kind}
      data-testid="going-plan-on-page"
    >
      <h2 className={`text-sm font-semibold ${textPrimaryOnSurface}`}>Planned changes</h2>
      {plan.summary === "" ? null : (
        <p className={`text-sm ${textSecondaryOnSurface}`}>{plan.summary}</p>
      )}
      <ul>
        {(plan.rows ?? [])
          .filter((row) => rows.includes(row.row))
          .map((row) => (
            <PlanRowView draft={draft} key={row.row} plan={plan} row={row} />
          ))}
      </ul>
      <PlanControls answering={answering} />
    </section>
  );
}
