import { useQueryClient } from "@tanstack/react-query";
import { useMemo } from "react";
import { issueQuery, projectIssuesQuery } from "../../api/queries";
import { claimIsTakeable } from "../issue/ClaimChip";
import { useAgentText } from "../refs/AgentName";
import type { AgentRegistry } from "../refs/agent-registry";
import { useShownQuery } from "../writes/shown-query";
import { writesOf } from "../writes/write-store";
import { buildItems, type EpicAncestor, isEpic } from "./issues-table-model";

/** How many parent reads the ancestry walk keeps in flight. A project that has been running a
 *  while has many closed parents, and one table load must not be a burst of detail reads. */
const PARENT_READS_IN_FLIGHT = 8;

/**
 * One project's issues as the table shows them: the open listing, the full one once the reader
 * lets closed issues through, and the parents neither listing carries, so every row knows which
 * epic it belongs to.
 *
 * Two reads, never three. The open-only listing stops once the full one has answered, so a live
 * event refetches one listing. The ancestry walk has a key of its own, outside the project
 * listing's, and reads each parent with no staleness of its own. Nothing observes
 * `["issue", key]`, so an event on a parent only marks it stale: this query runs again when the
 * set of missing parents changes — a parent that joins the listing, a row whose parent changes —
 * and on the whole-cache refresh after a reconnect. Until one of those happens, or the page is
 * reloaded, a closed parent renamed somewhere else keeps the title it was read with.
 */
export function useProjectIssueItems(
  project: string,
  { wantsDone, registry }: { wantsDone: boolean; registry: AgentRegistry }
) {
  const queryClient = useQueryClient();
  const text = useAgentText();
  const everyIssue = useShownQuery({ ...projectIssuesQuery(project, []), enabled: wantsDone });
  const doneLoaded = wantsDone && everyIssue.data !== undefined;
  const openIssues = useShownQuery({
    ...projectIssuesQuery(project, [], true),
    enabled: !doneLoaded,
  });
  const issues = doneLoaded ? everyIssue : openIssues;
  const listingError =
    wantsDone && everyIssue.isError ? everyIssue : issues.isError ? issues : undefined;
  const all = useMemo(() => issues.data ?? [], [issues.data]);
  const missingParents = useMemo(() => {
    const keys = new Set(all.map((issue) => issue.key));
    return [
      ...new Set(
        all.flatMap((issue) => (issue.parent && !keys.has(issue.parent) ? [issue.parent] : []))
      ),
    ];
  }, [all]);
  const ancestry = useShownQuery({
    queryKey: ["issue-ancestors", project, ...missingParents],
    enabled: missingParents.length > 0,
    retry: false,
    queryFn: async () => {
      const known = new Map<string, EpicAncestor>(all.map((issue) => [issue.key, issue]));
      const ancestors: EpicAncestor[] = [];
      // A parent this reader cannot read — deleted, or in a project they are not in — costs
      // its own rows their epic and nothing else: the walk keeps every ancestor it did read,
      // and stops asking for the one that failed. Each parent is Dispatch's answer
      // (`fetchDispatched`), since the walk's own answer is a cache's data, which holds no
      // live change.
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
            slice.map((key) =>
              writesOf(queryClient).fetchDispatched({ ...issueQuery(key), staleTime: Infinity })
            )
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
        (issue) => issue.claim === null || claimIsTakeable(issue.claim, registry)
      ),
    [all, text, ancestry.data, registry]
  );
  return {
    all,
    allItems,
    ancestry,
    doneLoaded,
    epics,
    everyIssue,
    issues,
    listingError,
    missingParents,
  };
}
