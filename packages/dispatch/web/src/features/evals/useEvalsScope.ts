import { type UseQueryResult, useQueries } from "@tanstack/react-query";
import { useEffect, useMemo, useState } from "react";

import { evalsViewQuery } from "../../api/queries";
import {
  compareRange,
  DAY_MS,
  liveWindow,
  MAX_WINDOW_DAYS,
  type WindowChoice,
  windowDays,
} from "../delivery/lib/window";
import type { EvalsUrlState } from "./evals-url";
import type { EvalsScope } from "./useEvalsViews";

/**
 * The present a page reads its default compare range at: the moment it mounted, and again just
 * after each UTC midnight it stays open past. A preset window is subscribed by its days, so the
 * server moves it to the next day at midnight; this moves the range the tab compares it with to
 * the days before it on the same tick. The timer goes when the page does.
 */
export function useUtcDayClock(): number {
  const [now, setNow] = useState(Date.now);
  useEffect(() => {
    // A millisecond past the midnight that ends the day `windowDays` reads `now` in: at midnight
    // itself a preset's days still end the day before. A timer that fires early still reads the
    // new day.
    const next = (Math.floor((now - 1) / DAY_MS) + 1) * DAY_MS + 1;
    const timer = setTimeout(
      () => setNow(Math.max(Date.now(), next)),
      Math.max(next - Date.now(), 0)
    );
    return () => clearTimeout(timer);
  }, [now]);
  return now;
}

/** What every evals view of the tab shares (plan D2, D3, D7a): the project, the header's window as
 *  the live socket names it, its compare range, read at the present `useUtcDayClock` keeps, the
 *  repositories, the merge timeline's facets but the repository and its search (`narrowing`,
 *  `narrowingQuery`), and the narrowing of the delivery view drawn while that of `narrowing` has not
 *  answered (`held`, `useTabScopes`). It is the same object while none of those changes, so a
 *  render subscribes nothing again. */
export function useEvalsScope(
  project: string,
  state: EvalsUrlState,
  narrowing = "",
  held = narrowing
): EvalsScope {
  const now = useUtcDayClock();
  // The brush is the target cards' alone (`useCardsScope`), so it moves no other view's scope.
  const { compare, repos, window: choice } = state;
  return useMemo(
    () => ({
      project,
      window: liveWindow(choice),
      compare: compareRange(compare, choice, now),
      repos,
      narrowing,
      held,
    }),
    [project, choice, compare, repos, now, narrowing, held]
  );
}

/** The days the target cards read while the merge timeline's brush is set (decision 3): the whole
 *  UTC days the brushed span touches, at most the MAX_WINDOW_DAYS that end with its last, as a
 *  custom window; null with no brush. */
export function brushDays(brush: EvalsUrlState["brush"]): { from: string; to: string } | null {
  if (brush === null) return null;
  const days = windowDays({ kind: "custom", from: brush.start, to: brush.end }, Date.now());
  const to = Date.parse(days.to);
  const from = Math.max(Date.parse(days.from), to - MAX_WINDOW_DAYS * DAY_MS);
  return { from: new Date(from).toISOString(), to: days.to };
}

/** What the target cards' views share (decision 3): the tab's scope, or, while the merge
 *  timeline's brush is set, the same over the brushed days (`brushDays`), compared as the header
 *  compares a window of those days. The cards alone follow the brush; every other section keeps
 *  the header's window. */
export function useCardsScope(scope: EvalsScope, state: EvalsUrlState): EvalsScope {
  const { brush, compare } = state;
  return useMemo(() => {
    const days = brushDays(brush);
    if (days === null) return scope;
    const choice: WindowChoice = { kind: "custom", ...days };
    return {
      ...scope,
      compare: compareRange(compare, choice, Date.now()),
      window: liveWindow(choice),
    };
  }, [scope, brush, compare]);
}

/** A view's select that keeps only that it has answered, so its reader renders again only when
 *  that changes. */
const answered = (): true => true;

/** Whether every view handed has answered. */
const allAnswered = (views: readonly UseQueryResult<true>[]): boolean =>
  views.every((view) => view.data === true);

/**
 * The scopes of the tab's views (plan D3, D7a; Sami's option B, "The tab's first paint is not
 * narrowed"): the header's (`useEvalsScope`) and the target cards' (`useCardsScope`), both read
 * with `narrowing`, and both holding the narrowing of the delivery views the tab drew last until
 * every delivery view of `narrowing` has answered. So a tab opened from an address that sets a
 * facet but the repository, or the search, paints first from the unnarrowed delivery views, which
 * each section reads beside the narrowed ones (`useEvalsViews`), and a facet changed on an open tab
 * leaves the views it drew on screen until those of the facets set land. Then the tab holds
 * `narrowing`, and the views it held, which no section reads any more, are unsubscribed.
 */
export function useTabScopes(
  project: string,
  state: EvalsUrlState,
  narrowing: string
): { scope: EvalsScope; cards: EvalsScope } {
  const [held, setHeld] = useState("");
  const scope = useEvalsScope(project, state, narrowing, held);
  const cards = useCardsScope(scope, state);
  const landed = useQueries({
    queries:
      held === narrowing
        ? []
        : [...new Set([scope, cards])].map((each) => ({
            ...evalsViewQuery({ ...each, kind: "delivery", offset: 0 }),
            select: answered,
          })),
    combine: allAnswered,
  });
  useEffect(() => {
    if (landed) setHeld(narrowing);
  }, [landed, narrowing]);
  return { cards, scope };
}
