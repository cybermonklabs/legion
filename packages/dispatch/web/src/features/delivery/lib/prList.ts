// Row model for the PR list view: display labels and sort keys per PR, computed once per
// population so the table's cells and sort functions read plain fields. Pure: no React.
import type { DeliveryPR, DeliverySessionGroup } from "../../../api/types";
import {
  DEPLOYED_LABELS,
  NO_ISSUE,
  NO_PRIORITY,
  NO_SESSION,
  PLACEHOLDER_LABELS,
  shortRepoLabel,
} from "./facets";
import { parentAgentLabel } from "./parentAgent";

const PRIORITY_RANK: Record<NonNullable<DeliveryPR["priority"]>, number> = {
  P0: 0,
  P1: 1,
  P2: 2,
  P3: 3,
};
const NO_PRIORITY_RANK = 4;

/** Issue sort order: by priority (P0 first, "No priority" after P3), then by Dispatch key. */
export interface IssueSortKey {
  priorityRank: number;
  key: string;
}

export interface PRListRow {
  pr: DeliveryPR;
  mergedMs: number | undefined;
  /** Short repo label plus number, e.g. "widgets#123" (see shortRepoLabel). */
  prLabel: string;
  /** "ACME-100 · P1", "ACME-100 · P0 · from ACME-1", "ACME-100 · No priority", or "No issue"
   *  (`issuePriorityLabel`). */
  issueLabel: string;
  /** undefined for a PR with no issue, so it sorts last in either direction. */
  issueSort: IssueSortKey | undefined;
  /** The Parent agent as the page shows it: a group by its name, a session by its title, or "No
   *  session". */
  parentAgentLabel: string;
  /** undefined when parentAgentLabel is the placeholder, so it sorts last. */
  parentAgentSort: string | undefined;
  /** additions + deletions; undefined while the row is partial. */
  size: number | undefined;
  deployedLabel: string;
  deployedMs: number | undefined;
  /** Merge-to-production lead time in minutes; undefined while not deployed. */
  leadTimeMinutes: number | undefined;
}

/** A PR's lead time: merge to production, in minutes; undefined while it isn't deployed. */
export function leadTimeMinutes(pr: DeliveryPR): number | undefined {
  if (pr.merged_at === null || pr.deployed_at === null) return undefined;
  return (Date.parse(pr.deployed_at) - Date.parse(pr.merged_at)) / 60_000;
}

/** Minutes as the page shows durations: "45m", "3.5h", "2.1d". */
export function formatMinutes(minutes: number): string {
  if (minutes < 60) return `${Math.round(minutes)}m`;
  if (minutes < 1440) return `${(minutes / 60).toFixed(1)}h`;
  return `${(minutes / 1440).toFixed(1)}d`;
}

/** A PR's issue with the priority it works at, as the list's Issue cell and the drill-down's
 *  Priority row say it: "ACME-100 · P1", "ACME-100 · P0 · from ACME-1" where the issue inherits
 *  it (`priority_from`), "ACME-100 · No priority", or "No issue". `priority` is already the
 *  priority the issue resolves to, as Dispatch labels it; a Dispatch of the release before
 *  `priority_from` sends none, and its row names no source. The source is text, not a link, so
 *  a row and the drill-down hold one link to the issue each. */
export function issuePriorityLabel(pr: DeliveryPR): string {
  if (pr.issue === null) return PLACEHOLDER_LABELS[NO_ISSUE];
  const from = pr.priority === null ? null : (pr.priority_from ?? null);
  const source = from === null ? "" : ` \u00b7 from ${from}`;
  return `${pr.issue} \u00b7 ${pr.priority ?? PLACEHOLDER_LABELS[NO_PRIORITY]}${source}`;
}

export function buildPRListRows(
  prs: readonly DeliveryPR[],
  allRepos: readonly string[],
  sessionGroups: readonly DeliverySessionGroup[]
): PRListRow[] {
  return prs.map((pr) => {
    const parentAgent =
      pr.parent_agent === null ? undefined : parentAgentLabel(pr.parent_agent, sessionGroups);
    return {
      pr,
      mergedMs: pr.merged_at === null ? undefined : Date.parse(pr.merged_at),
      prLabel: `${shortRepoLabel(allRepos, pr.repo)}#${pr.number}`,
      issueLabel: issuePriorityLabel(pr),
      issueSort:
        pr.issue === null
          ? undefined
          : {
              priorityRank: pr.priority === null ? NO_PRIORITY_RANK : PRIORITY_RANK[pr.priority],
              key: pr.issue,
            },
      parentAgentLabel: parentAgent ?? PLACEHOLDER_LABELS[NO_SESSION],
      parentAgentSort: parentAgent,
      size:
        pr.additions === null || pr.deletions === null ? undefined : pr.additions + pr.deletions,
      deployedLabel: DEPLOYED_LABELS[pr.deployed_status],
      deployedMs: pr.deployed_at === null ? undefined : Date.parse(pr.deployed_at),
      leadTimeMinutes: leadTimeMinutes(pr),
    };
  });
}

/** Ascending comparator for IssueSortKey: priority rank, then Dispatch key with numeric runs
 *  compared as numbers ("ACME-99" before "ACME-100"). */
export function compareIssueSort(a: IssueSortKey, b: IssueSortKey): number {
  return (
    a.priorityRank - b.priorityRank || a.key.localeCompare(b.key, undefined, { numeric: true })
  );
}

/** How tall each body row of the list is drawn, which the shared table's virtualizer counts a
 *  row as until it has measured it. The sticky header row is the same height. */
export const ROW_HEIGHT_PX = 36;
