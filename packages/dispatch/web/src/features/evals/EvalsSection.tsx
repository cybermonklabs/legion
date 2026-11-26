import { type ReactNode, useId } from "react";

import type { EvalKind } from "../../api/queries";
import { QueryError } from "../../components/QueryError";
import { textMutedOnCanvas, textPrimaryOnCanvas } from "../../theme/classes";
import { WINDOW_ONLY } from "./narrowing";
import {
  type EvalsScope,
  type EvalsViewSelect,
  type EvalsViewState,
  type EvalsViewStatus,
  useEvalsViews,
} from "./useEvalsViews";

/** A section's own state: `failed` while any of its views is refused, else `subscribed` while any
 *  has not answered, else `painted`. A section with no view (the merge timeline, closed) is
 *  `closed`. */
type SectionState = EvalsViewStatus | "closed";

function sectionState(statuses: readonly EvalsViewStatus[]): SectionState {
  if (statuses.length === 0) return "closed";
  if (statuses.includes("failed")) return "failed";
  return statuses.includes("subscribed") ? "subscribed" : "painted";
}

/**
 * One section of the Evals tab (dispatch://INFRA-2918/spec R2, R3, R5): its heading, and what its
 * own views answer, painted as each lands, whatever another section's views do. Each section reads
 * its views itself, keeping of each page what it draws (`select`), so a delta of one view renders
 * only the sections that draw something it moved. While a view has not answered the section says
 * so on a line of its own; a refused view shows its error with Retry. What the section draws of
 * its views is `children`'s (the target cards draw their cards, each as its own view lands). A
 * section of evals no facet narrows (`windowOnly`) says "window and repository only" under its
 * heading while a facet or the search is set (plan D3).
 */
export function EvalsSection({
  children,
  id,
  kinds,
  scope,
  select,
  title,
  windowOnly = false,
}: {
  /** Draws the section's views, each as it stands. */
  children: (views: readonly EvalsViewState[]) => ReactNode;
  id: string;
  /** The kinds of eval the section reads, each its own view. */
  kinds: readonly EvalKind[];
  scope: EvalsScope;
  /** What the section keeps of each page; all of it where absent. */
  select?: EvalsViewSelect;
  title: string;
  /** Whether no eval the section draws is narrowed by the facets. */
  windowOnly?: boolean;
}): ReactNode {
  const headingId = useId();
  const views = useEvalsViews(scope, kinds, select);
  const state = sectionState(views.map((view) => view.status));
  return (
    <section
      aria-labelledby={headingId}
      className="flex flex-col gap-2"
      data-section={id}
      data-state={state}
    >
      <h2 className={`text-[15px] font-semibold ${textPrimaryOnCanvas}`} id={headingId}>
        {title}
      </h2>
      {windowOnly && views.some((view) => view.facets?.set === true) ? (
        <p className={`text-xs ${textMutedOnCanvas}`} data-narrowing="window-only">
          {WINDOW_ONLY}
        </p>
      ) : null}
      {state === "failed" ? (
        <QueryError
          message={`${title}: could not load.`}
          onRetry={() => {
            for (const view of views) if (view.status === "failed") view.retry();
          }}
        />
      ) : null}
      {state === "subscribed" ? (
        <p className={`text-sm ${textMutedOnCanvas}`} role="status">
          {title}: loading…
        </p>
      ) : null}
      {children(views)}
    </section>
  );
}
