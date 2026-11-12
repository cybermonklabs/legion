import type { PriorityIssue } from "../../api/types";
import { issueStatuses, statusText } from "../project/board-model";
import { ageOf } from "../project/issues-table-model";
import { columnsById, type TableColumn, tableLayout } from "./priority-table-model";

/** The ages a fault row files under, oldest last, with the words for a time the row has none:
 *  a parked issue whose move predates this change, or whose events no longer reach back to it,
 *  is still parked, so it is listed with that cell empty rather than left out. */
export const AGES = ["Today", "This week", "This month", "Older"] as const;
export const NOT_RECORDED = "Not recorded";

/** Who holds the issue now: whoever claimed it, else whoever it is assigned to, else nobody. */
export const holderOf = (issue: PriorityIssue): string =>
  issue.claim?.actor.id ?? issue.assignee ?? "Nobody";

/** What a search over a fault list matches: the key, the title and the project. */
export function matchesFaultSearch(issue: PriorityIssue, search: string): boolean {
  const words = search.trim().toLowerCase();
  if (words === "") return true;
  return `${issue.key} ${issue.title} ${issue.project}`.toLowerCase().includes(words);
}

const bucket = (at: string | null): string => (at === null ? NOT_RECORDED : ageOf(at));

/** The columns both fault lists share: what R6 says each row names — the issue, its project,
 *  its status, who holds it, and how long it has stood as it is. */
const SHARED: readonly TableColumn<PriorityIssue>[] = [
  { id: "key", header: "Key", size: 120, min: 110, pinned: true, text: (issue) => issue.key },
  {
    id: "title",
    header: "Title",
    size: 380,
    min: 180,
    pinned: true,
    text: (issue) => issue.title,
  },
  {
    id: "project",
    header: "Project",
    size: 120,
    min: 120,
    values: (issue) => [issue.project],
    group: (issue) => issue.project,
  },
  {
    id: "status",
    header: "Status",
    size: 140,
    min: 140,
    values: (issue) => [issue.status],
    group: (issue) => issue.status,
    order: issueStatuses,
    show: statusText,
  },
  {
    id: "holder",
    header: "Claimed by",
    size: 200,
    min: 160,
    values: (issue) => [holderOf(issue)],
    group: holderOf,
  },
  {
    id: "changed",
    header: "Last changed",
    size: 150,
    min: 150,
    values: (issue) => [bucket(issue.changed_at)],
    group: (issue) => bucket(issue.changed_at),
    order: AGES,
  },
];

/** The parked list says which priorities the work advances and when it was moved out of the
 *  way; the in-flight list says neither, because its rows advance none and nothing moved them. */
const PARKED: readonly TableColumn<PriorityIssue>[] = [
  {
    id: "priorities",
    header: "Advances",
    size: 200,
    min: 160,
    values: (issue) => issue.priorities ?? [],
  },
  {
    id: "moved",
    header: "Moved there",
    size: 200,
    min: 160,
    values: (issue) => [bucket(issue.moved_at)],
    group: (issue) => bucket(issue.moved_at),
    order: [...AGES, NOT_RECORDED],
  },
];

export const NO_PRIORITY_COLUMNS = SHARED;
export const PARKED_COLUMNS: readonly TableColumn<PriorityIssue>[] = [...SHARED, ...PARKED];
export const NO_PRIORITY_COLUMNS_BY_ID = columnsById(NO_PRIORITY_COLUMNS);
export const PARKED_COLUMNS_BY_ID = columnsById(PARKED_COLUMNS);
export const NO_PRIORITY_LAYOUT = tableLayout(NO_PRIORITY_COLUMNS, "title");
export const PARKED_LAYOUT = tableLayout(PARKED_COLUMNS, "title");
/** Both lists open on the key, which is how a reader reads a list of issues. */
export const FAULT_SORTING = [{ id: "key", desc: false }];

/** The per-project or per-priority counts under a fault's rule, in plain words: `CORE 12 ·
 *  OPS 7`, greatest first, which is what says where the fault is. */
export function countsLine(by: Readonly<Record<string, number>> | null): string {
  return Object.entries(by ?? {})
    .sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]))
    .map(([name, count]) => `${name} ${count}`)
    .join(" · ");
}
