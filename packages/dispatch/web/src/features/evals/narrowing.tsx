// What the Evals tab's charts say of the merge timeline's facets and search
// (dispatch://INFRA-2918/artifact/plan-md, D3, D7a): Dispatch narrows the five evals that count
// pull requests by them, and no other eval, which keeps its stored answer of the window and the
// repositories alone.
import type { ReactNode } from "react";

import { textMutedOnSurface } from "../../theme/classes";
import type { EvalSeriesValues } from "./chart-model";
import type { EvalsViewFacets } from "./useEvalsViews";

/** The evals a facet but the repository, or the search, narrows: those that count pull requests,
 *  which the base's measures narrowed (D7a). A chart of one of them shows the loading mark while
 *  the narrowed view has not answered, which no other chart does. */
export const NARROWED_EVALS: Readonly<Record<string, true>> = {
  "first-commit-to-production": true,
  "merge-to-production": true,
  "open-to-production": true,
  "pr-open-to-merge": true,
  "rework-share": true,
};

/** What a chart of an eval no facet narrows says while one is set; a section of such evals alone
 *  says it once, under its heading. */
export const WINDOW_ONLY = "window and repository only";

/** The name of each key a narrowed series names (`narrowed`): its facet's on the merge timeline
 *  (`FacetPanel`), and the search. */
const APPLIED_NAMES: Readonly<Record<string, string>> = {
  author: "Author",
  component: "Component",
  deployed: "Deployed",
  issue: "Dispatch issue",
  parent_agent: "Parent agent",
  priority: "Priority",
  q: "Search",
  rework: "Rework",
  session: "Session",
};

/** What a chart of one eval says of the facets: `loading` over the held view's values while the
 *  view of the facets set has not answered; `narrowed` by the facets and search its series names;
 *  `window-only` for an eval no facet narrows, while one is set. */
export type Narrowing =
  | { readonly kind: "loading" }
  | { readonly kind: "narrowed"; readonly applied: readonly string[] }
  | { readonly kind: "window-only" };

/** What the chart of eval `slug`, drawing `series` of a view whose facets stand as `facets`, says
 *  of them; null for nothing, as with no facet set. */
export function narrowingOf(
  facets: EvalsViewFacets | undefined,
  slug: string,
  series: EvalSeriesValues | undefined
): Narrowing | null {
  if (facets === undefined) return null;
  if (NARROWED_EVALS[slug] !== true) return facets.set ? { kind: "window-only" } : null;
  if (facets.loading) return { kind: "loading" };
  const applied = series?.narrowed ?? [];
  return applied.length === 0 ? null : { applied, kind: "narrowed" };
}

function narrowingWords(narrowing: Narrowing): string {
  switch (narrowing.kind) {
    case "loading":
      return "narrowing to the facets set\u2026";
    case "narrowed":
      return `narrowed by ${narrowing.applied.map((key) => APPLIED_NAMES[key] ?? key).join(", ")}`;
    case "window-only":
      return WINDOW_ONLY;
  }
}

/** A chart of one eval (`children`, its chart or the line standing for it) with what it says of
 *  the facets (`narrowingOf`) under it: dimmed and marked busy while its values are the held
 *  view's, the names of the facets its values are narrowed by, or "window and repository only".
 *  The chart keeps its place in the tree whatever the mark, so a mark that changes keeps the
 *  reader's Graph or Table choice. */
export function NarrowedChart({
  children,
  narrowing,
}: {
  children: ReactNode;
  narrowing: Narrowing | null;
}): ReactNode {
  const loading = narrowing?.kind === "loading";
  return (
    <>
      <div
        aria-busy={loading ? true : undefined}
        className={`flex min-w-0 flex-col gap-1 ${loading ? "opacity-50" : ""}`}
      >
        {children}
      </div>
      {narrowing === null ? null : (
        <p
          className={`text-[11px] ${textMutedOnSurface}`}
          data-narrowing={narrowing.kind}
          role={loading ? "status" : undefined}
        >
          {narrowingWords(narrowing)}
        </p>
      )}
    </>
  );
}
