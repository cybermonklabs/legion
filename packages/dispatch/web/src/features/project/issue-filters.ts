import { useCallback } from "react";

import { flagParam, repeatedParam, useAddressFields } from "../../lib/page-address";
import { useAgentRegistry } from "../refs/agent-registry";

export interface IssueFiltersState {
  /** The project's `?label=` values (every one required, case-insensitively). */
  readonly labels: readonly string[];
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
   *  control row says so. `undefined` while the filter is off or the list is current or loading. */
  readonly unclaimedUnchecked: "refilling" | "unreachable" | undefined;
  readonly setLabels: (next: string[]) => void;
  readonly setNeedsYou: (next: boolean) => void;
  readonly setUnclaimed: (next: boolean) => void;
  readonly setUnread: (next: boolean) => void;
}

/** The one query key prefix of a project's issue listings. `api/queries.ts` builds every listing
 *  of the project under it: the plain list under the prefix alone, and the open listing the
 *  Issues tab reads (`open`), its done reads and Open's count each under a suffix of its own. A
 *  Board move cancels the array listings under it that hold the moved card, and reads every
 *  listing under it again (`board-moves.ts`). */
export function projectIssuesQueryKey(project: string): readonly unknown[] {
  return ["issues", "project", project];
}

/** The project page's own parameters, under the names they have always had. A module-level
 *  constant: `useAddressFields` memoizes what it reads on this object's identity, so a spec
 *  built in a render would hand every render new arrays. */
const ISSUE_FILTER_PARAMS = {
  label: repeatedParam(),
  "needs-you": flagParam(),
  unclaimed: flagParam(),
  unread: flagParam(),
};

/** The Needs you and Unread flags `params` sets, read as the page reads its own (`?needs-you=1`,
 *  `?unread=1`): for an address that is not the page's, as a view's own address. */
export function readIssueFlags(
  params: URLSearchParams
): Pick<IssueFiltersState, "needsYou" | "unread"> {
  return {
    needsYou: ISSUE_FILTER_PARAMS["needs-you"].read(params, "needs-you"),
    unread: ISSUE_FILTER_PARAMS.unread.read(params, "unread"),
  };
}

/**
 * The project page's issue filters, read from and written to the URL - `?label=` (repeatable),
 * `?needs-you=1`, `?unread=1`, `?unclaimed=1` - so the control row and the issues table share one
 * filter state and a filtered view survives a reload. Every write replaces the history entry, as
 * the label filter always has. The table's own `?q=` search and `?status=` column filter are its
 * business, not this hook's.
 */
export function useIssueFilters(): IssueFiltersState {
  const [params, setParams] = useAddressFields(ISSUE_FILTER_PARAMS);
  const labels = params.label;
  const needsYou = params["needs-you"];
  const unread = params.unread;
  const unclaimed = params.unclaimed;
  // The one ["agents"] registry read every claim chip on the page already shares, fetched only
  // while the filter that needs it is on.
  const registry = useAgentRegistry(unclaimed);
  const setLabels = useCallback((next: string[]) => setParams({ label: next }), [setParams]);
  const setNeedsYou = useCallback((next: boolean) => setParams({ "needs-you": next }), [setParams]);
  const setUnread = useCallback((next: boolean) => setParams({ unread: next }), [setParams]);
  const setUnclaimed = useCallback((next: boolean) => setParams({ unclaimed: next }), [setParams]);
  return {
    labels,
    needsYou,
    setLabels,
    setNeedsYou,
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
