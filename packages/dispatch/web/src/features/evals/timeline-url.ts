// The merge timeline's facets, search and view, read from and written to the URL under the keys
// the Delivery page gave them, so an old `/delivery` link keeps them through the redirect
// (dispatch://INFRA-2918/artifact/plan-md, D8; decision 2): each facet's repeatable key
// (`FACET_KEYS`), the search `q`, `colorBy`, `lanes=1`, `mode=list` and each `agents` value. The
// Repository facet's key, `repo`, is the header's repository filter too (`evals-url.ts`), so a
// pick of it sets the repository every number reads (decision 3).
import { useCallback, useMemo } from "react";
import { useSearchParams } from "react-router-dom";

import { useSettled } from "../../hooks/useSettled";
import { useRepeatableSearchParams, useSearchParamsUpdate } from "../../lib/url-array-params";
import { COLOR_FACET_OPTIONS, type ColorFacet } from "../delivery/lib/colorScale";
import {
  type DeployedFacet,
  FACET_KEYS,
  FACET_PARAMS,
  type Filters,
  type ReworkFacet,
} from "../delivery/lib/facets";

const COLOR_FACETS = COLOR_FACET_OPTIONS.map((option) => option.value);

/** The colour-by facet the timeline uses when its URL names none, as the prototype does. */
export const DEFAULT_COLOR_BY: ColorFacet = "repo";

/** The repeatable URL parameter that names the Parent agent values given their own colour and
 *  lane, kept apart from the facet filters, which narrow the rows. */
const AGENTS_PARAM = ["agents"] as const;

/** How the timeline shows its rows: the chart (`timeline`, with its graph and its table) or the
 *  merged pull request list. */
export type TimelineMode = "timeline" | "list";

/** The facet the merges are coloured by, whether the chart splits them into one column per value,
 *  the chart or the list, and, coloured by Parent agent, the values chosen to have their own
 *  colour and lane. */
export interface TimelineView {
  readonly colorBy: ColorFacet;
  readonly lanes: boolean;
  readonly mode: TimelineMode;
  readonly agents: readonly string[];
}

export interface TimelineUrlState {
  readonly filters: Filters;
  readonly view: TimelineView;
}

/** The query `GET /api/v1/projects/{key}/timeline` takes for `filters` and `search`, without its
 *  window and its limit: the search as `q`, then each facet value under its own key. */
export function timelineQuery(filters: Filters, search: string): string {
  const params = new URLSearchParams();
  if (search !== "") params.set("q", search);
  for (const key of FACET_KEYS) {
    for (const value of filters[key]) params.append(FACET_PARAMS[key], value);
  }
  return params.toString();
}

/** The query the evals views take for `filters` and `search` (dispatch://INFRA-2918/artifact/
 *  plan-md, D7a): `timelineQuery`'s without the Repository facet, which the header's repository
 *  filter names every view by already. Empty with no other facet and no search. */
export function narrowingQuery(filters: Filters, search: string): string {
  return timelineQuery({ ...filters, repo: [] }, search);
}

/** How long the search waits after the last keystroke before a view is subscribed again. */
const SEARCH_DEBOUNCE_MS = 250;

/** The search `filters` holds, trimmed, once it has held still for SEARCH_DEBOUNCE_MS, which the
 *  views the search narrows are read by, so a view is not subscribed again at each keystroke. A
 *  search the URL holds when the page opens is read at once. */
export function useSettledSearch(filters: Filters): string {
  return useSettled(filters.search.trim(), SEARCH_DEBOUNCE_MS);
}

/** The timeline's facets, search and view, and their one writer, which replaces the URL's entry
 *  rather than pushing one. Each part is memoized on the URL's own identity, so a render with an
 *  unchanged URL hands the view the same query. */
export function useTimelineUrlState(): [TimelineUrlState, (next: TimelineUrlState) => void] {
  const [searchParams] = useSearchParams();
  const update = useSearchParamsUpdate();
  const search = searchParams.get("q") ?? "";
  const colorByParam = searchParams.get("colorBy");
  const colorBy = COLOR_FACETS.find((facet) => facet === colorByParam) ?? DEFAULT_COLOR_BY;
  const lanes = searchParams.get("lanes") === "1";
  const mode: TimelineMode = searchParams.get("mode") === "list" ? "list" : "timeline";
  const { agents: agentParams } = useRepeatableSearchParams(AGENTS_PARAM);
  const agents = useMemo(() => [...new Set(agentParams)], [agentParams]);
  const view = useMemo(() => ({ agents, colorBy, lanes, mode }), [agents, colorBy, lanes, mode]);
  const arrayParams = useRepeatableSearchParams(FACET_KEYS);
  const filters: Filters = useMemo(
    () => ({
      ...arrayParams,
      deployed: arrayParams.deployed as DeployedFacet[],
      rework: arrayParams.rework as ReworkFacet[],
      search,
    }),
    [arrayParams, search]
  );
  const state = useMemo(() => ({ filters, view }), [filters, view]);

  const setState = useCallback(
    (next: TimelineUrlState) => {
      update((params) => {
        for (const key of FACET_KEYS) {
          params.delete(key);
          for (const value of next.filters[key]) params.append(key, value);
        }
        if (next.filters.search.trim() === "") params.delete("q");
        else params.set("q", next.filters.search);
        if (next.view.colorBy === DEFAULT_COLOR_BY) params.delete("colorBy");
        else params.set("colorBy", next.view.colorBy);
        if (next.view.lanes) params.set("lanes", "1");
        else params.delete("lanes");
        if (next.view.mode === "list") params.set("mode", "list");
        else params.delete("mode");
        params.delete("agents");
        for (const agent of next.view.agents) params.append("agents", agent);
      });
    },
    [update]
  );
  return [state, setState];
}
