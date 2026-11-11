import { type QueryKey, useQuery, useQueryClient } from "@tanstack/react-query";
import type { ColumnFiltersState } from "@tanstack/react-table";
import { useCallback, useMemo } from "react";
import {
  issueQuery,
  legionBoardQuery,
  legionSettingsQuery,
  projectDoneIssuesQuery,
  projectIssuesQuery,
} from "../../api/queries";
import type { IssueSummary } from "../../api/types";
import { claimIsTakeable } from "../issue/ClaimChip";
import { useAgentText } from "../refs/AgentName";
import type { AgentRegistry } from "../refs/agent-registry";
import { projectIssuesQueryKey } from "./issue-filters";
import { buildItems, type EpicAncestor, isEpic } from "./issues-table-model";

/** How many parent reads the ancestry walk keeps in flight. A project that has been running a
 *  while has many closed parents, and one table load must not be a burst of detail reads. */
const PARENT_READS_IN_FLIGHT = 8;

/** The project's Legion capability, resolved before a view expands its Legion-only parameters. */
export function useHasLegion(project: string) {
  const settings = useQuery(legionSettingsQuery());
  return {
    hasLegion: (settings.data ?? []).some((setting) => setting.project === project),
    pending: settings.isPending,
  };
}

/** The closed window a table view must read, or null when its Status filter excludes done. */
export function doneWindowOf(filters: ColumnFiltersState): string | null {
  const statuses = filters.find((filter) => filter.id === "status")?.value;
  if (Array.isArray(statuses) && statuses.length > 0 && !statuses.includes("done")) return null;
  const closed = filters.find((filter) => filter.id === "closed")?.value;
  return typeof closed === "string" && closed !== "" ? closed : "28d";
}

function joinedIssues(
  open: readonly IssueSummary[],
  done: readonly IssueSummary[]
): readonly IssueSummary[] {
  if (done.length === 0) return open;
  const keys = new Set(open.map((issue) => issue.key));
  return [...open, ...done.filter((issue) => !keys.has(issue.key))];
}

/**
 * One project's issue rows: its open listing and, only where the view can list one, its windowed
 * done listing. The ancestry walk has a key outside both listings and reads closed parents on
 * demand, so a parent still supplies every row's epic after a narrow read.
 */
export function useProjectIssueItems(
  project: string,
  {
    done,
    doneDefaulted = false,
    doneOnly = false,
    registry,
    viewer,
  }: {
    done: string | null;
    doneDefaulted?: boolean;
    doneOnly?: boolean;
    registry: AgentRegistry;
    viewer: string | undefined;
  }
) {
  const queryClient = useQueryClient();
  const text = useAgentText();
  const openQuery = projectIssuesQuery(project, [], true);
  const issues = useQuery(openQuery);
  const doneQueryOptions = projectDoneIssuesQuery(project, done ?? "28d");
  const doneQuery = useQuery({
    ...doneQueryOptions,
    enabled: done !== null,
  });
  const doneRead = done === null ? undefined : doneQuery;
  const doneLoaded = doneRead?.data !== undefined;
  const listingKeys = useMemo<readonly QueryKey[]>(
    () => [
      [...projectIssuesQueryKey(project, []), "open"],
      ...(done === null ? [] : [[...projectIssuesQueryKey(project, []), "done", done]]),
    ],
    [done, project]
  );
  const open = issues.data ?? [];
  const closed = doneRead?.data ?? [];
  const all = useMemo(
    () => (done === null ? open : doneOnly ? closed : joinedIssues(open, closed)),
    [closed, done, doneOnly, open]
  );
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
  const { hasLegion } = useHasLegion(project);
  const legion = useQuery({ ...legionBoardQuery({ kind: "legion", project }), enabled: hasLegion });
  const board = hasLegion ? (legion.data ?? null) : undefined;
  const isFree = useCallback(
    (issue: IssueSummary) => issue.claim === null || claimIsTakeable(issue.claim, registry),
    [registry]
  );
  const allItems = useMemo(
    () => buildItems(all, text.label, ancestry.data?.ancestors, isFree, board, viewer),
    [all, ancestry.data?.ancestors, board, isFree, text.label, viewer]
  );
  const openItems = useMemo(
    () => buildItems(open, text.label, ancestry.data?.ancestors, isFree, board, viewer),
    [ancestry.data?.ancestors, board, isFree, open, text.label, viewer]
  );
  const listingError = doneRead?.isError ? doneRead : issues.isError ? issues : undefined;
  return {
    all,
    allItems,
    ancestry,
    doneDefaulted,
    doneLoaded,
    doneRead,
    epics,
    hasLegion,
    issues,
    legion,
    listingError,
    listingKeys,
    missingParents,
    openItems,
  };
}
