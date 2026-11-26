import { useQueries } from "@tanstack/react-query";

import {
  EVALS_VIEW_LIMIT,
  type EvalKind,
  type EvalsViewParams,
  evalsViewQuery,
} from "../../api/queries";
import type { EvalListItem, EvalListResponse } from "../../api/types";

/** What every view of the tab shares: everything of a view but its kind and its page. */
export interface EvalsScope extends Omit<EvalsViewParams, "kind" | "offset"> {
  /** The narrowing of the delivery view the tab drew last (dispatch://INFRA-2918/artifact/
   *  plan-md, D3): while it is not `narrowing`, that view is read beside the delivery view of
   *  `narrowing` and drawn in its place until that one answers. Absent: the same as `narrowing`. */
  readonly held?: string;
}

/** Where one kind's views stand: `subscribed` until its first page lands, `painted` once it has,
 *  `failed` while any of its pages is refused. */
export type EvalsViewStatus = "subscribed" | "painted" | "failed";

/** How the timeline's facets and search stand for a view (plan D3, D7a): `set`, whether a facet
 *  but the repository, or the search, is set, which narrows the delivery evals counting pull
 *  requests and nothing else; `loading`, for the delivery view, whether the values drawn are the
 *  held view's (`EvalsScope.held`), while the view of the facets set has not answered. */
export interface EvalsViewFacets {
  readonly set: boolean;
  readonly loading: boolean;
}

export interface EvalsViewState {
  readonly kind: EvalKind;
  readonly status: EvalsViewStatus;
  /** The first page's answer: its window, compare range, total and freshness. */
  readonly answer: EvalListResponse | undefined;
  /** Every eval of the pages landed, in their order. */
  readonly items: readonly EvalListItem[];
  /** Subscribes again each page that was refused. */
  readonly retry: () => void;
  /** How the facets stand; absent, none is set. */
  readonly facets?: EvalsViewFacets;
}

/** The offsets past the first page that a first answer of `total` evals says a read needs: one
 *  page more for each EVALS_VIEW_LIMIT evals past the first page's, up to its total. */
export function laterOffsets(total: number): number[] {
  const offsets: number[] = [];
  for (let offset = EVALS_VIEW_LIMIT; offset < total; offset += EVALS_VIEW_LIMIT) {
    offsets.push(offset);
  }
  return offsets;
}

/** What a reader keeps of each page of a view: the page as answered, or the part of it the reader
 *  draws. One function for the reader's life (a module's, or memoized), since each new function
 *  reads every page again. */
export type EvalsViewSelect = (answer: EvalListResponse) => EvalListResponse;

/**
 * The evals views of `kinds` over `scope`, all subscribed at once (dispatch://INFRA-2918/
 * artifact/plan-md, D3): one view per kind, and, for a kind whose first answer counts more than a
 * page holds, one view more per page at the next offset. Every component that asks for the same
 * view shares its one subscription, through its query key, so two sections that read one kind
 * cost one view; and only the components that read a view render again when its delta lands.
 * `select` keeps the part of each page a reader draws (the Logged section's logged evals, the
 * panel's one eval), so a delta that moves nothing of that part renders that reader not at all;
 * a page's `total` stays the kind's.
 *
 * While the scope's held narrowing is not its own, the delivery view of the held one is read too,
 * and its pages are drawn until the delivery view of `scope.narrowing` answers (plan D3): so the
 * first paint of a tab opened with a facet set draws the unnarrowed view, and a facet change
 * blanks no card. Both first pages are entries of one `useQueries`, so a view that moves from one
 * role to the other keeps its observer, and its subscription, across the move.
 */
export function useEvalsViews(
  scope: EvalsScope,
  kinds: readonly EvalKind[],
  select?: EvalsViewSelect
): EvalsViewState[] {
  const narrowing = scope.narrowing ?? "";
  const held = scope.held ?? narrowing;
  const holding = held !== narrowing && kinds.includes("delivery");
  const heldScope: EvalsScope = { ...scope, narrowing: held };
  const firsts = useQueries({
    queries: [
      ...kinds.map((kind) => ({ ...evalsViewQuery({ ...scope, kind, offset: 0 }), select })),
      ...(holding
        ? [{ ...evalsViewQuery({ ...heldScope, kind: "delivery", offset: 0 }), select }]
        : []),
    ],
  });
  // Each kind's drawn view: its own, or, for the delivery kind while its own has not answered and
  // is not refused, the held one.
  const drawn = kinds.map((kind, index) => {
    const own = firsts[index];
    const fallback =
      holding && kind === "delivery" && own?.data === undefined && own?.isError !== true;
    return fallback
      ? { first: firsts[kinds.length], loading: true, scope: heldScope }
      : { first: own, loading: false, scope };
  });
  const later = kinds.flatMap((kind, index) => {
    const view = drawn[index];
    return laterOffsets(view?.first?.data?.total ?? 0).map((offset) => ({
      kind,
      offset,
      scope: view?.scope ?? scope,
    }));
  });
  const rest = useQueries({
    queries: later.map(({ kind, offset, scope: pageScope }) => ({
      ...evalsViewQuery({ ...pageScope, kind, offset }),
      select,
    })),
  });
  const set = narrowing !== "";
  return kinds.map((kind, index) => {
    const view = drawn[index];
    const first = view?.first;
    const pages = [first, ...rest.filter((_, at) => later[at]?.kind === kind)].flatMap((page) =>
      page === undefined ? [] : [page]
    );
    const refused = pages.filter((page) => page.isError);
    return {
      kind,
      status: refused.length > 0 ? "failed" : first?.data === undefined ? "subscribed" : "painted",
      answer: first?.data,
      items: pages.flatMap((page) => page.data?.items ?? []),
      retry: () => {
        for (const page of refused) void page.refetch();
      },
      facets: { loading: view?.loading ?? false, set },
    };
  });
}
