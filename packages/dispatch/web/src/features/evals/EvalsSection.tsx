import { type ReactNode, useId, useRef } from "react";

import type { EvalKind } from "../../api/queries";
import { QueryError } from "../../components/QueryError";
import { textMutedOnCanvas, textPrimaryOnCanvas } from "../../theme/classes";
import { WINDOW_ONLY } from "./narrowing";
import { SectionCharts, useSectionDraw } from "./sectionDraw";
import { UnservedViews } from "./unserved";
import { useDrawnMark } from "./useDrawnMark";
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
 * so on a line of its own; a refused view shows its error with Retry. While the server does not
 * serve the tab's views (`unserved`, `useUnservedViews`), a section that has not painted says so
 * in place of both. What the section draws of its views is `children`'s (the target cards draw
 * their cards, each as its own view lands). A section of evals no facet narrows (`windowOnly`)
 * says "window and repository only" under its heading while a facet or the search is set (plan
 * D3). A section shown only once it holds an eval (`untilHeld`) draws nothing before, while its
 * views stay open, unless the server does not serve them or refuses them.
 *
 * Once every view has answered or been refused (or none is served), and nothing in it holds its
 * box back (a CI job list still out, `useHoldBox`), the section shows its header and its box:
 * everything it draws, each chart's frame at its drawn size and none of the charts' marks
 * (`ChartsDrawn`), marked `dispatch:evals-section-box:<id>` on the page's performance timeline.
 * It draws its charts when the tab's plan says (`useSectionDraw`: at once where it lies in the
 * viewport, else in the page's idle time or as it is scrolled into view, INFRA-979's ruling (A),
 * dispatch://INFRA-2918/comment/75af800e-9db9-47d9-9c4d-427b0cff36c0), so nothing in its box moves
 * when it does; `data-drawn` says whether it has. The commit in which it first draws every view
 * painted with its charts is marked `dispatch:evals-section:<id>` (`useDrawnMark`).
 */
export function EvalsSection({
  children,
  id,
  kinds,
  scope,
  select,
  title,
  unserved = false,
  untilHeld = false,
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
  /** Whether the server refused one of the tab's views as a frame it cannot read. */
  unserved?: boolean;
  /** Whether the section draws nothing until one of its views holds an eval: the Logged section
   *  of a project with every source missing (spec R19). */
  untilHeld?: boolean;
  /** Whether no eval the section draws is narrowed by the facets. */
  windowOnly?: boolean;
}): ReactNode {
  const headingId = useId();
  const element = useRef<HTMLElement>(null);
  const views = useEvalsViews(scope, kinds, select);
  const state = sectionState(views.map((view) => view.status));
  const notServed = unserved && state !== "painted";
  const hidden =
    untilHeld && !notServed && state !== "failed" && views.every((view) => view.items.length === 0);
  // Its size is its last once every view has answered or been refused, or none is served.
  const settled = state !== "subscribed" || notServed;
  const draw = useSectionDraw(element, {
    box: settled && !hidden,
    boxMark: `dispatch:evals-section-box:${id}`,
    settled,
  });
  useDrawnMark(`dispatch:evals-section:${id}`, state === "painted" && !hidden && draw.drawn);
  if (hidden) return null;
  return (
    <section
      aria-labelledby={headingId}
      className="flex flex-col gap-2"
      data-drawn={draw.drawn}
      data-section={id}
      data-state={state}
      ref={element}
    >
      <h2 className={`text-[15px] font-semibold ${textPrimaryOnCanvas}`} id={headingId}>
        {title}
      </h2>
      {windowOnly && views.some((view) => view.facets?.set === true) ? (
        <p className={`text-xs ${textMutedOnCanvas}`} data-narrowing="window-only">
          {WINDOW_ONLY}
        </p>
      ) : null}
      {notServed ? <UnservedViews what={title} /> : null}
      {state === "failed" && !notServed ? (
        <QueryError
          message={`${title}: could not load.`}
          onRetry={() => {
            for (const view of views) if (view.status === "failed") view.retry();
          }}
        />
      ) : null}
      {state === "subscribed" && !notServed ? (
        <p className={`text-sm ${textMutedOnCanvas}`} role="status">
          {title}: loading…
        </p>
      ) : null}
      <SectionCharts draw={draw}>{children(views)}</SectionCharts>
    </section>
  );
}
