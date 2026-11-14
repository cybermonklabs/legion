import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useMemo } from "react";
import {
  advancesIssuesQuery,
  issueQuery,
  projectDoneIssuesQuery,
  projectIssuesQuery,
} from "../../api/queries";
import type { IssueSummary, LegionBoardResponse } from "../../api/types";
import { claimIsTakeable } from "../issue/ClaimChip";
import { useAgentText } from "../refs/AgentName";
import type { AgentRegistry } from "../refs/agent-registry";
import { openIssueStatuses } from "./board-model";
import { buildItems, type EpicAncestor, isEpic } from "./issues-table-model";

/** How many parent reads the ancestry walk keeps in flight. A project that has been running a
 *  while has many closed parents, and one table load must not be a burst of detail reads. */
const PARENT_READS_IN_FLIGHT = 8;

const NO_ISSUES: readonly IssueSummary[] = [];

/**
 * What a table lists. A project's table lists that project's issues; a scoped table lists the
 * issues that advance one priority, where `advances` is the priority node's id and the server
 * narrows the listing by it (`GET /api/v1/issues?advances=<id>`). A priority belongs to no
 * project, so a scoped table lists the work of every project and stands on none of them: it
 * can make no write that names a project, and it offers none (`issues-table-actions.ts`).
 */
export type IssueListing = { readonly project: string } | AdvancesListing;

/**
 * The issues of every project the server narrows by what they advance: one priority's id, or
 * `none` for the work that advances no live priority and `any` for the work that advances one.
 * `statuses`, where it is set, is the whole of what the listing holds - the issues standing in
 * one of those statuses, which is how the priorities page asks for each of its two faults (see
 * `answeredWhole` for when its open read is every row of it). `moved` asks for the time each row
 * standing in backlog or icebox was moved there and who moved it.
 */
export type AdvancesListing = {
  readonly advances: string;
  readonly statuses?: readonly string[];
  readonly moved?: boolean;
};

/**
 * Whether a listing's one open read (`open: true`) answers every row its table can show, so the
 * table never reads the closed issues beside it: a listing that names its statuses, every one of
 * them open. A listing whose statuses include `done` holds closed issues the open read leaves
 * out, and a project's listing, or a listing that names no status, holds every closed issue it
 * has: each of those reads its closed issues once the reader lets them through.
 */
export function answeredWhole(listing: IssueListing): boolean {
  return (
    "advances" in listing &&
    listing.statuses !== undefined &&
    listing.statuses.every((status) => (openIssueStatuses as readonly string[]).includes(status))
  );
}

/** The name what a table keeps of its listing is held under: the ancestry it walked and the
 *  action a reload interrupted. Two listings of different rows never share one. */
export const listingName = (listing: IssueListing): string =>
  "project" in listing
    ? listing.project
    : [`advances:${listing.advances}`, ...(listing.statuses ?? [])].join(":");

/** Which done issues a view reads: those closed in a window, every done issue (`{}`), or none
 *  (`undefined`), for a view that lists no closed issue. */
export type DoneRead =
  | { readonly closed_since?: string; readonly closed_before?: string }
  | undefined;

/**
 * One listing's issues as the table shows them: the open listing, the done issues a view that
 * lists closed issues reads (only those closed in its window where it has one), and the parents
 * neither read carries, so every row knows which epic it belongs to. No view of a project reads
 * the project's full listing. A scoped listing (`advances`) is narrowed by the server already,
 * so its done issues come from the listing read whole, and a listing its open read answers
 * whole (`answeredWhole`) reads no done issue at all.
 *
 * A done read whose rows carry no `closed_at` (an older server during a rolling deploy, which
 * ignores `closed_since`) lists no row for a window, rather than every done issue.
 *
 * The ancestry walk has a key of its own, outside the listing's, and reads each parent with no
 * staleness of its own. Nothing observes `["issue", key]`, so an event on a parent only marks it
 * stale: this query runs again when the set of missing parents changes, and on the whole-cache
 * refresh after a reconnect.
 */
export function useIssueTableItems(
  listing: IssueListing,
  {
    done,
    registry,
    board,
    hasLegion,
    viewer,
  }: {
    done: DoneRead;
    registry: AgentRegistry;
    board: LegionBoardResponse | undefined;
    hasLegion: boolean;
    viewer: string | undefined;
  }
) {
  const queryClient = useQueryClient();
  const text = useAgentText();
  const issues = useQuery(
    "project" in listing
      ? projectIssuesQuery(listing.project, [], true)
      : advancesIssuesQuery(listing, true)
  );
  const reads = done !== undefined && !answeredWhole(listing);
  const doneIssues = useQuery({
    ...("project" in listing
      ? projectDoneIssuesQuery(listing.project, done ?? {})
      : advancesIssuesQuery(listing)),
    enabled: reads,
  });
  const windowed = reads && done?.closed_since !== undefined;
  const doneRows = reads ? (doneIssues.data ?? NO_ISSUES) : NO_ISSUES;
  const doneLoaded = reads && doneIssues.data !== undefined;
  const listingError = issues.isError ? issues : doneIssues.isError ? doneIssues : undefined;
  const all = useMemo(() => {
    const open = issues.data ?? NO_ISSUES;
    if (doneRows.length === 0) return open;
    const keys = new Set(open.map((issue) => issue.key));
    const closed = doneRows.filter(
      (issue) => !keys.has(issue.key) && (!windowed || issue.closed_at !== null)
    );
    return [...open, ...closed];
  }, [issues.data, doneRows, windowed]);
  const missingParents = useMemo(() => {
    const keys = new Set(all.map((issue) => issue.key));
    return [
      ...new Set(
        all.flatMap((issue) => (issue.parent && !keys.has(issue.parent) ? [issue.parent] : []))
      ),
    ];
  }, [all]);
  const ancestry = useQuery({
    queryKey: ["issue-ancestors", listingName(listing), ...missingParents],
    enabled: missingParents.length > 0,
    retry: false,
    queryFn: async () => {
      const known = new Map<string, EpicAncestor>(all.map((issue) => [issue.key, issue]));
      const ancestors: EpicAncestor[] = [];
      // A parent this reader cannot read — deleted, or in a project they are not in — costs
      // its own rows their epic and nothing else: the walk keeps every ancestor it did read,
      // and stops asking for the one that failed.
      const unreadable = new Set<string>();
      for (;;) {
        const missing = [
          ...new Set(
            [...known.values()].flatMap((issue) =>
              issue.parent && !known.has(issue.parent) && !unreadable.has(issue.parent)
                ? [issue.parent]
                : []
            )
          ),
        ];
        if (missing.length === 0) return { ancestors, unreadable: [...unreadable] };
        for (let from = 0; from < missing.length; from += PARENT_READS_IN_FLIGHT) {
          const slice = missing.slice(from, from + PARENT_READS_IN_FLIGHT);
          const answers = await Promise.allSettled(
            slice.map((key) => queryClient.fetchQuery({ ...issueQuery(key), staleTime: Infinity }))
          );
          answers.forEach((answer, index) => {
            const key = slice[index];
            if (answer.status === "fulfilled") {
              known.set(answer.value.key, answer.value);
              ancestors.push(answer.value);
            } else if (key !== undefined) unreadable.add(key);
          });
        }
      }
    },
  });
  const epics = useMemo(() => all.filter(isEpic), [all]);
  const allItems = useMemo(
    () =>
      buildItems(
        all,
        text.label,
        ancestry.data?.ancestors,
        (issue) => issue.claim === null || claimIsTakeable(issue.claim, registry),
        { board, hasLegion, viewer }
      ),
    [all, text, ancestry.data, registry, board, hasLegion, viewer]
  );
  return {
    all,
    allItems,
    ancestry,
    doneIssues,
    doneLoaded,
    epics,
    issues,
    listingError,
    missingParents,
  };
}
