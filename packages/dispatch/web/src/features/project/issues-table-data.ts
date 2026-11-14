import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useMemo } from "react";
import { issueQuery, projectDoneIssuesQuery, projectIssuesQuery } from "../../api/queries";
import type { IssueSummary, LegionBoardResponse } from "../../api/types";
import { claimIsTakeable } from "../issue/ClaimChip";
import { useAgentText } from "../refs/AgentName";
import type { AgentRegistry } from "../refs/agent-registry";
import { buildItems, type EpicAncestor, isEpic } from "./issues-table-model";

/** How many parent reads the ancestry walk keeps in flight. A project that has been running a
 *  while has many closed parents, and one table load must not be a burst of detail reads. */
const PARENT_READS_IN_FLIGHT = 8;

const NO_ISSUES: readonly IssueSummary[] = [];

/** Which done issues a view reads: those closed in a window, every done issue (`{}`), or none
 *  (`undefined`), for a view that lists no closed issue. */
export type DoneRead =
  | { readonly closed_since?: string; readonly closed_before?: string }
  | undefined;

/**
 * One project's issues as the views show them: the open listing, the done issues a view that
 * lists closed issues reads (only those closed in its window where it has one), and the parents
 * neither read carries, so every row knows which epic it belongs to. No view reads the full
 * listing.
 *
 * A done read whose rows carry no `closed_at` (an older server during a rolling deploy, which
 * ignores `closed_since`) lists no row for a window, rather than every done issue.
 *
 * The ancestry walk has a key of its own, outside the project listing's, and reads each parent
 * with no staleness of its own. Nothing observes `["issue", key]`, so an event on a parent only
 * marks it stale: this query runs again when the set of missing parents changes, and on the
 * whole-cache refresh after a reconnect.
 */
export function useProjectIssueItems(
  project: string,
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
  const issues = useQuery(projectIssuesQuery(project, [], true));
  const doneIssues = useQuery({
    ...projectDoneIssuesQuery(project, done ?? {}),
    enabled: done !== undefined,
  });
  const windowed = done !== undefined && done.closed_since !== undefined;
  const doneRows = done === undefined ? NO_ISSUES : (doneIssues.data ?? NO_ISSUES);
  const doneLoaded = done !== undefined && doneIssues.data !== undefined;
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
    queryKey: ["issue-ancestors", project, ...missingParents],
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
