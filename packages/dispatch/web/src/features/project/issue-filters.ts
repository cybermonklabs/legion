import { useCallback } from "react";

import type { IssueSummary } from "../../api/types";
import {
  cappedTextParam,
  flagParam,
  repeatedParam,
  useAddressFields,
} from "../../lib/page-address";
import { claimIsTakeable } from "../issue/ClaimChip";
import { useAgentRegistry } from "../refs/agent-registry";
import { issueIsUnread } from "./UnreadDot";

/** One removable chip in the filter strip. */
export interface ActiveFilter {
  readonly label: string;
  readonly remove: () => void;
}

export interface IssueFiltersState {
  /** The project's `?label=` values (every one required, case-insensitively). */
  readonly labels: readonly string[];
  readonly search: string;
  readonly needsYou: boolean;
  readonly unread: boolean;
  /** Only issues an agent could pick up: nobody has claimed them, or the server would hand the
   *  claim to the next agent that asks (`claimIsTakeable`) - its holder has gone, or has missed
   *  its heartbeats for longer than the claim's grace. A holder that has missed its heartbeats
   *  inside that grace keeps the issue, and the server refuses the take, so it stays out of
   *  this list (dispatch://INFRA-392). */
  readonly unclaimed: boolean;
  /** Why the Unclaimed filter cannot tell which claims are takeable while it is on: the agent
   *  list is `refilling`, or its read failed (`unreachable`). Until it can, every session's
   *  claim counts as held - no claim is called free from a list that did not answer - and the
   *  strip says so. `undefined` while the filter is off or the list is current or loading. */
  readonly unclaimedUnchecked: "refilling" | "unreachable" | undefined;
  readonly activeFilterCount: number;
  readonly activeFilters: ActiveFilter[];
  readonly setLabels: (next: string[]) => void;
  readonly setSearch: (next: string) => void;
  readonly setNeedsYou: (next: boolean) => void;
  readonly setUnclaimed: (next: boolean) => void;
  readonly setUnread: (next: boolean) => void;
  /** Whether `issue` passes every active filter. */
  readonly matches: (issue: IssueSummary, lastReadSequence: number) => boolean;
}

/** The one query key for a project's issue list: the plain list, or the server-filtered list
 *  when labels are active. List and Board build their keys here, so one set of labels is one
 *  key in both; `api/queries.ts` then forks on `open`, so the open-only read of a view that
 *  hides closed issues is a key of its own under the same prefix. */
export function projectIssuesQueryKey(
  project: string,
  labels: readonly string[]
): readonly unknown[] {
  return labels.length === 0
    ? ["issues", "project", project]
    : ["issues", "project", project, "labels", labels];
}

/** The project page's own parameters, under the names they have always had. A module-level
 *  constant: `useAddressFields` memoizes what it reads on this object's identity, so a spec
 *  built in a render would hand every render new arrays. */
const ISSUE_FILTER_PARAMS = {
  label: repeatedParam(),
  "needs-you": flagParam(),
  q: cappedTextParam(),
  unclaimed: flagParam(),
  unread: flagParam(),
};

/**
 * The project page's issue filters, read from and written to the URL - `?label=` (repeatable),
 * `?q=`, `?needs-you=1`, `?unread=1`, `?unclaimed=1` - so the strip and the issues table share
 * one filter state and a filtered view survives a reload. Every write replaces the history
 * entry, as the label filter always has. The table's own `?status=` column filter is its
 * business, not this hook's.
 */
export function useIssueFilters(): IssueFiltersState {
  const [params, setParams] = useAddressFields(ISSUE_FILTER_PARAMS);
  const labels = params.label;
  const search = params.q;
  const needsYou = params["needs-you"];
  const unread = params.unread;
  const unclaimed = params.unclaimed;
  // The one ["agents"] registry read every claim chip on the page already shares, fetched only
  // while the filter that needs it is on.
  const registry = useAgentRegistry(unclaimed);
  const setSearch = useCallback((next: string) => setParams({ q: next }), [setParams]);
  const setLabels = useCallback((next: string[]) => setParams({ label: next }), [setParams]);
  const setNeedsYou = useCallback((next: boolean) => setParams({ "needs-you": next }), [setParams]);
  const setUnread = useCallback((next: boolean) => setParams({ unread: next }), [setParams]);
  const setUnclaimed = useCallback((next: boolean) => setParams({ unclaimed: next }), [setParams]);
  const matches = useCallback(
    (issue: IssueSummary, lastReadSequence: number): boolean => {
      const query = search.trim().toLocaleLowerCase();
      return (
        (!needsYou || issue.open_asks > 0) &&
        (!unread || issueIsUnread(issue, lastReadSequence)) &&
        (!unclaimed || issue.claim === null || claimIsTakeable(issue.claim, registry)) &&
        (query === "" ||
          issue.key.toLocaleLowerCase().includes(query) ||
          issue.title.toLocaleLowerCase().includes(query))
      );
    },
    [needsYou, registry, search, unclaimed, unread]
  );
  const activeFilterCount =
    labels.length +
    Number(search.trim() !== "") +
    Number(needsYou) +
    Number(unread) +
    Number(unclaimed);
  const activeFilters: ActiveFilter[] = [
    ...labels.map((label) => ({
      label: `Label: ${label}`,
      remove: () => setLabels(labels.filter((current) => current !== label)),
    })),
    ...(search.trim() === ""
      ? []
      : [{ label: `Search: ${search.trim()}`, remove: () => setSearch("") }]),
    ...(needsYou ? [{ label: "Needs you", remove: () => setNeedsYou(false) }] : []),
    ...(unread ? [{ label: "Unread", remove: () => setUnread(false) }] : []),
    ...(unclaimed ? [{ label: "Unclaimed", remove: () => setUnclaimed(false) }] : []),
  ];
  return {
    activeFilterCount,
    activeFilters,
    labels,
    matches,
    needsYou,
    search,
    setLabels,
    setNeedsYou,
    setSearch,
    setUnclaimed,
    setUnread,
    unclaimed,
    unclaimedUnchecked:
      unclaimed && registry.isError
        ? registry.refilling
          ? "refilling"
          : "unreachable"
        : undefined,
    unread,
  };
}
