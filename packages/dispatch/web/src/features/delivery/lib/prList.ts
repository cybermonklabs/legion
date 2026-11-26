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
  /** "ACME-100 · P1", "ACME-100 · No priority", or "No issue". */
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

/** Minutes written whole, rounded ("45"), or to `extra` decimals ("44.6"). */
export function wholeMinutes(minutes: number, extra = 0): string {
  return extra === 0 ? `${Math.round(minutes)}` : minutes.toFixed(extra);
}

/** Minutes as the page shows durations: "45m", "3.5h", "2.1d"; with `extra` decimals past those
 *  ("44.6m"), as a toned figure takes them where its usual ones would print it on the other side
 *  of its target (`lib/target.ts`'s `judgeFigure`). */
export function formatMinutes(minutes: number, extra = 0): string {
  if (minutes < 60) return `${wholeMinutes(minutes, extra)}m`;
  if (minutes < 1440) return `${(minutes / 60).toFixed(1 + extra)}h`;
  return `${(minutes / 1440).toFixed(1 + extra)}d`;
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
      issueLabel:
        pr.issue === null
          ? PLACEHOLDER_LABELS[NO_ISSUE]
          : `${pr.issue} \u00b7 ${pr.priority ?? PLACEHOLDER_LABELS[NO_PRIORITY]}`,
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
