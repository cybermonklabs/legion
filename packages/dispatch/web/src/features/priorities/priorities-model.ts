import type { PriorityRow } from "../../api/types";
import { ageOf } from "../project/issues-table-model";
import { columnsById, type TableColumn, tableLayout } from "./priority-table-model";

/** The statuses each count column adds up. In flight is work somebody is doing now; waiting is
 *  work admitted and not started; parked is work that stands in the backlog or the icebox. The
 *  three together are every open status, so no open issue under a priority is uncounted. */
export const IN_FLIGHT_STATUSES = ["in_progress", "testing", "needs_review", "retro"] as const;
export const WAITING_STATUSES = ["todo", "triage"] as const;
export const PARKED_STATUSES = ["backlog", "icebox"] as const;

/** What the newest-event column files a row under when nothing under the priority has moved. */
export const NEVER_MOVED = "Never";

/** One row of the priorities table: the priority as the server answers it, its place in the one
 *  order, and the four counts the row shows, added up once rather than per cell. */
export interface PriorityItem {
  readonly priority: PriorityRow;
  /** The priority's place in the order, from 1: what `P7` reads from. */
  readonly place: number;
  readonly order: string;
  /** The first line of the description, which the row shows while it is closed. */
  readonly summary: string;
  readonly inFlight: number;
  readonly waiting: number;
  readonly parked: number;
  readonly closed: number;
  readonly newest: string | null;
}

/** The first line a reader sees of a description of any length; the empty text for one that is
 *  blank or holds only blank lines. */
export function firstLine(description: string): string {
  return (
    description
      .split("\n")
      .find((line) => line.trim() !== "")
      ?.trim() ?? ""
  );
}

const countOf = (by: PriorityRow["work"]["by_status"], statuses: readonly string[]): number =>
  statuses.reduce((total, status) => total + (by?.[status] ?? 0), 0);

/** The rows in the order the server answers them, which is rank order: the place a row shows
 *  is its place in that answer, so the order a reader drags and the order they read are one. */
export function buildPriorityItems(rows: readonly PriorityRow[]): PriorityItem[] {
  return rows.map((priority, index) => ({
    priority,
    place: index + 1,
    order: `P${index + 1}`,
    summary: firstLine(priority.description),
    inFlight: countOf(priority.work.by_status, IN_FLIGHT_STATUSES),
    waiting: countOf(priority.work.by_status, WAITING_STATUSES),
    parked: countOf(priority.work.by_status, PARKED_STATUSES),
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
export const newestBucket = (at: string | null): string => (at === null ? NEVER_MOVED : ageOf(at));

const AGES = ["Today", "This week", "This month", "Older", NEVER_MOVED];

/** What a search over the priorities table matches: the title and the owner, as the box says. */
export function matchesPrioritySearch(item: PriorityItem, search: string): boolean {
  const words = search.trim().toLowerCase();
  if (words === "") return true;
  return `${item.priority.title} ${item.priority.owner}`.toLowerCase().includes(words);
}

/** The owner a row files under, which is the owner's own text or the words for none. */
export const ownerOf = (item: PriorityItem): string =>
  item.priority.owner.trim() === "" ? "No owner" : item.priority.owner.trim();

/** The four count columns are alike: a number, filtered by picking the numbers the rows hold,
 *  which is how a reader asks for the priorities with nothing in flight. */
const COUNT_WIDTH = { size: 132, min: 132 };

/** Every column of the priorities table, left to right: the order and the title stay pinned at
 *  the left edge while the counts scroll. */
export const PRIORITY_COLUMNS: readonly TableColumn<PriorityItem>[] = [
  { id: "order", header: "Order", size: 88, min: 88, pinned: true, text: (item) => item.order },
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
    size: 200,
    min: 160,
    values: (item) => [ownerOf(item)],
    group: ownerOf,
  },
  { id: "in-flight", header: "In flight", ...COUNT_WIDTH, values: (i) => [String(i.inFlight)] },
  { id: "waiting", header: "Waiting", ...COUNT_WIDTH, values: (i) => [String(i.waiting)] },
  {
    id: "parked",
    header: "In backlog or icebox",
    size: 180,
    min: 180,
    values: (item) => [String(item.parked)],
  },
  { id: "closed", header: "Closed, 7 days", ...COUNT_WIDTH, values: (i) => [String(i.closed)] },
  {
    id: "newest",
    header: "Newest event",
    size: 160,
    min: 160,
    values: (item) => [newestBucket(item.newest)],
    group: (item) => newestBucket(item.newest),
    order: AGES,
    always: AGES,
  },
];

export const PRIORITY_COLUMNS_BY_ID = columnsById(PRIORITY_COLUMNS);
export const PRIORITY_LAYOUT = tableLayout(PRIORITY_COLUMNS, "title");
/** The table opens in the one order, which is what the page is about. */
export const PRIORITY_SORTING = [{ id: "order", desc: false }];
