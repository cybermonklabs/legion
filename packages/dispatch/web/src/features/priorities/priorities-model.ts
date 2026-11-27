import type { PriorityRow } from "../../api/types";
import type { IssueStatus } from "../project/board-model";
import { ageOf, AGES as ISSUE_AGES, runningIssueStatuses } from "../project/issues-table-model";
import { columnsById, phoneHidden, type TableColumn, tableLayout } from "./priority-table-model";

/** The statuses each count column adds up. In flight is the statuses an issue is being worked
 *  in, which the issues table's own status bar counts (`runningIssueStatuses`); waiting is work
 *  admitted and not started; backlog is work that stands in the backlog or the icebox. The three
 *  together are every open status, so no open issue under a priority is uncounted. */
const WAITING_STATUSES: readonly IssueStatus[] = ["todo", "triage"];
/** The one list of the statuses backlog and icebox: the count here and the priorities page's
 *  fault list of the priority work in either (`PrioritiesPage.tsx`) both read it. */
export const BACKLOG_STATUSES: readonly IssueStatus[] = ["backlog", "icebox"];

/** What the newest-event column files a row under when nothing under the priority has moved. */
export const NEVER_MOVED = "Never";

/** One row of the priorities table: the priority as the server answers it, its place in the one
 *  order, and the four counts the row shows, added up once rather than per cell. */
export interface PriorityItem {
  readonly priority: PriorityRow;
  /** The priority's place in the order, from 1: what `#7` reads from. */
  readonly place: number;
  readonly order: string;
  readonly inFlight: number;
  readonly waiting: number;
  readonly backlog: number;
  readonly closed: number;
  readonly newest: string | null;
}

/** What a priority's place in the one order is called, from the index the read answers it at:
 *  the row's `#7`, and the same name the issue header's Advances line writes
 *  (`IssuePriorityLine`), so one rule says what `#7` means. It is not `P7`: P0 to P3 are an
 *  issue's own priority, a different thing. */
export const orderLabel = (index: number): string => `#${index + 1}`;

/** What a priority is named by beside its title: its place in the one order, from 1, then its
 *  title (`#3 Ship it`), as its chip's accessible name and the picker's option read. A priority
 *  the priorities read does not hold, one deleted since, is named by its id. */
export function priorityName(
  id: string,
  place: number | undefined,
  title: string | undefined
): string {
  const label = place === undefined ? id : orderLabel(place - 1);
  return title === undefined ? label : `${label} ${title}`;
}

const countOf = (by: PriorityRow["work"]["by_status"], statuses: readonly string[]): number =>
  statuses.reduce((total, status) => total + (by?.[status] ?? 0), 0);

/** The rows in the order the server answers them, which is rank order: the place a row shows
 *  is its place in that answer, so the order a reader drags and the order they read are one. */
export function buildPriorityItems(rows: readonly PriorityRow[]): PriorityItem[] {
  return rows.map((priority, index) => ({
    priority,
    place: index + 1,
    order: orderLabel(index),
    inFlight: countOf(priority.work.by_status, runningIssueStatuses),
    waiting: countOf(priority.work.by_status, WAITING_STATUSES),
    backlog: countOf(priority.work.by_status, BACKLOG_STATUSES),
    closed: priority.work.closed_last_7_days,
    newest: priority.work.newest_event_at,
  }));
}

/** What a drop writes: the priority the dragged one lands above (`before`), or the one it lands
 *  below (`after`), which is the shape `PATCH /api/v1/priorities/{id}` takes. A drop on the
 *  dragged row itself, or on a row that is not in the order, moves nothing. */
export function rankMove(
  order: readonly string[],
  moved: string,
  onto: string
): { before?: string; after?: string } | undefined {
  const from = order.indexOf(moved);
  const to = order.indexOf(onto);
  if (from === -1 || to === -1 || from === to) return undefined;
  return from > to ? { before: onto } : { after: onto };
}

/** The age bucket the newest event under a priority files under, which its filter picks by. */
const newestBucket = (at: string | null): string => (at === null ? NEVER_MOVED : ageOf(at));

/** The ages the issues table files a time under, with the words for a priority nothing has
 *  happened under at all. */
const AGES = [...ISSUE_AGES, NEVER_MOVED];

/** What a search over the priorities table matches: the title and the owner, as the box says. */
export function matchesPrioritySearch(item: PriorityItem, search: string): boolean {
  const words = search.trim().toLowerCase();
  if (words === "") return true;
  return `${item.priority.title} ${item.priority.owner}`.toLowerCase().includes(words);
}

/** The owner a row files under, which is the owner's own text or the words for none. */
export const ownerOf = (item: PriorityItem): string =>
  item.priority.owner.trim() === "" ? "No owner" : item.priority.owner.trim();

/** The width and the shape of each of the four count columns (`numeric`). */
const COUNT = { size: 76, min: 76, numeric: true } as const;

/** Every column of the priorities table, left to right: the order and the title stay pinned at
 *  the left edge. Each header reads in 16 characters or fewer, in the words a status is named by
 *  (INFRA-3221, R13). What closed in the last 7 days opens hidden, one tick away under Columns,
 *  so the table fits a 1280 px window beside the sidebar with the work as it stands now - who
 *  owns each priority, what is active, in triage or todo, and in backlog or icebox - and when
 *  anything under it last moved.
 *
 *  Every column but the title is as wide as its header's words, its sort arrow and its filter
 *  control need, so the title takes the rest of a desktop box (`PRIORITY_LAYOUT`): at 1280 px a
 *  title of 90 characters reads in two lines. A count is a number no reader picks rows by, so
 *  the four counts have no filter, and their headers no filter control; a count's column is as
 *  narrow as its numbers, which stand at its right edge under its header's words, two lines of
 *  them where they need it (`numeric`). The newest event's words take two lines as well, beside
 *  its filter, over a time that takes one (`wrap`), in 104 px, and while the table is sorted by
 *  it the column widens to what the header needs with its sort arrow beside the words, 116 px
 *  at 1280 (`LayoutColumns.wrappedHeaders`), so the arrow stays clear of the filter. A phone
 *  draws the place and the title alone, which take its box (`PRIORITY_PHONE_HIDDEN`), and the
 *  title's cell names the owner, the three counts under their headers' words and the newest
 *  event, so nothing scrolls sideways. */
export const PRIORITY_COLUMNS: readonly TableColumn<PriorityItem>[] = [
  { id: "order", header: "#", size: 76, min: 76, pinned: true, text: (item) => item.order },
  {
    id: "title",
    header: "Priority",
    size: 420,
    min: 180,
    pinned: true,
    text: (item) => `${item.priority.title} ${item.priority.description}`,
  },
  {
    id: "owner",
    header: "Owner",
    size: 112,
    min: 108,
    values: (item) => [ownerOf(item)],
    group: ownerOf,
  },
  { id: "in-flight", header: "Active", ...COUNT },
  { id: "waiting", header: "Triage, Todo", ...COUNT },
  { id: "backlog", header: "Backlog, icebox", ...COUNT },
  { id: "closed", header: "Closed, 7 days", ...COUNT, hidden: true },
  {
    id: "newest",
    header: "Newest event",
    size: 104,
    min: 104,
    wrap: true,
    values: (item) => [newestBucket(item.newest)],
    group: (item) => newestBucket(item.newest),
    order: AGES,
    always: AGES,
  },
];

export const PRIORITY_COLUMNS_BY_ID = columnsById(PRIORITY_COLUMNS);

/** The priorities table's own words for its rows, which the table core reads and never
 *  invents: the attribute a row carries holding its priority's id, the one naming the group it
 *  sits in, and the one on the link opening the row follows, its order's link to the priority's
 *  page. A priority is no issue, so none of them is the issue table's. */
export const PRIORITY_ROW_ATTRIBUTE = "data-priority-row";
export const PRIORITY_GROUP_ATTRIBUTE = "data-priority-group";
export const PRIORITY_LINK_ATTRIBUTE = "data-priority-link";

/** The priorities table's layout: the order and the title pinned, and the title filling a
 *  desktop box with what the other shown columns leave (`fills`), so nothing scrolls sideways. */
export const PRIORITY_LAYOUT = {
  ...tableLayout(PRIORITY_COLUMNS, "title", PRIORITY_ROW_ATTRIBUTE),
  fills: "title",
};
/** What a phone's box hides, whatever the reader shows: every column but the place and the
 *  title, whose cell names what the others hold there. */
export const PRIORITY_PHONE_HIDDEN = phoneHidden(PRIORITY_COLUMNS);
/** The table opens in the one order, which is what the page is about. */
export const PRIORITY_SORTING = [{ id: "order", desc: false }];
