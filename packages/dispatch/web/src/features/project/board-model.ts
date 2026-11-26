import { ISSUE_STATUSES, type IssueStatus } from "@legion/contracts/dispatch-tools";
import type { IssuePriority, UpdateIssueInput } from "../../api/types";
import { issuePriority } from "../issue/issue-priority";
import type { LegionPhaseColumn } from "./issues-legion";
import type { Item } from "./issues-table-model";

/** The Go server's `model.IssueStatuses`, in lifecycle order: the board's columns. */
export const issueStatuses = ISSUE_STATUSES;
export type { IssueStatus };
export const openIssueStatuses = issueStatuses.filter((status) => status !== "done");

const statusLabels: Record<IssueStatus, string> = {
  triage: "Triage",
  icebox: "Icebox",
  backlog: "Backlog",
  todo: "Todo",
  in_progress: "In progress",
  testing: "Testing",
  needs_review: "Needs review",
  retro: "Retro",
  done: "Done",
};

export function isIssueStatus(value: string): value is IssueStatus {
  return (issueStatuses as readonly string[]).includes(value);
}

export function statusLabel(status: IssueStatus): string {
  return statusLabels[status];
}

/** The lifecycle label for a known status; any other string (a filter value the server no
 *  longer knows, a free-text status) is shown as it is. */
export function statusText(status: string): string {
  return isIssueStatus(status) ? statusLabel(status) : status;
}

/** The statuses Dispatch's rule (R24) counts as in work: while a child is in work its parent is
 *  in work. Typed by the status names, so a misspelt one fails the type check. */
export const IN_WORK_STATUSES: ReadonlySet<string> = new Set<IssueStatus>([
  "todo",
  "in_progress",
  "testing",
  "needs_review",
  "retro",
]);

/** A swimlane: one of the four priorities, or `null` for an issue with none set. */
export type PriorityLane = IssuePriority | null;

/** Lane order: P0 through P3, then issues with no priority. */
export const priorityLanes: readonly PriorityLane[] = [0, 1, 2, 3, null];

export function laneLabel(lane: PriorityLane): string {
  return lane === null ? "No priority" : `P${lane}`;
}

/** A lane's value as a DOM attribute and droppable-id fragment: `"0"`-`"3"` or `"none"`. */
export function laneKey(lane: PriorityLane): string {
  return lane === null ? "none" : String(lane);
}

/** What the Board's columns are: the issues' statuses, or their Legion phases (Group by). */
export type BoardBy = "status" | "legion_phase";

/** The rail of the rows the daemon holds no card for, on a Board by Legion phase: the id the
 *  Legion phase filter gives its None pick (`NONE_PICK` in `issues-filter-picks.ts`). */
const NONE_COLUMN = "none";

/** The one column of a Board by Legion phase whose cards a reader may put in order. */
const QUEUE_COLUMN = "queue";

/** What a refused move by Legion phase says: the daemon puts each card in its phase. */
export const DAEMON_SETS_PHASE = "The Legion daemon sets the Legion phase.";

/** One column the Board draws, in its order, under its title. A rail holds its cards out of
 *  sight: Icebox until Show Icebox is on, Done while the view lists no done issue, and None. */
export interface BoardColumn {
  readonly id: string;
  readonly rail: boolean;
  readonly title: string;
}

/**
 * The Board's columns, in its order.
 *
 * By Status they follow the status order: each status the view's Status filter picks, an empty
 * one too, since it takes a drop, or, where the view picks no status, each status a row is in.
 * Icebox is a rail in its place, after Triage, until Show Icebox is on. Done is always last: the
 * column of the view's done cards where the view lists done issues, and a rail otherwise, which
 * still takes a drop that closes an issue.
 *
 * By Legion phase there is one column for each of `phases`, the board's columns, in its order and
 * under its labels, empty ones included, so a column a newer server adds holds its cards too; then
 * the None rail of the rows that have no card. A Legion phase filter keeps only its picks, and the
 * None rail only where it picks None.
 */
export function boardColumns({
  by,
  listsDone,
  phaseFilterPicks,
  phases,
  rows,
  showIcebox,
  statusPicks,
}: {
  by: BoardBy;
  listsDone: boolean;
  phaseFilterPicks: readonly string[];
  phases: readonly LegionPhaseColumn[];
  rows: readonly Item[];
  showIcebox: boolean;
  statusPicks: readonly string[];
}): BoardColumn[] {
  if (by === "legion_phase") {
    const picked = (id: string) => phaseFilterPicks.length === 0 || phaseFilterPicks.includes(id);
    return [
      ...phases
        .filter((phase) => picked(phase.id))
        .map((phase) => ({ id: phase.id, rail: false, title: phase.title })),
      ...(picked(NONE_COLUMN) ? [{ id: NONE_COLUMN, rail: true, title: "None" }] : []),
    ];
  }
  const named = new Set<string>(
    statusPicks.length > 0 ? statusPicks : rows.map((item) => item.issue.status)
  );
  return [
    ...issueStatuses
      .filter((status) => status !== "done" && named.has(status))
      .map((status) => ({
        id: status,
        rail: status === "icebox" && !showIcebox,
        title: statusLabel(status),
      })),
    { id: "done", rail: !listsDone, title: statusLabel("done") },
  ];
}

/** The column a row stands in on a Board of `by`: its status, or the board column its card is
 *  in, None with no card. */
function columnOf(item: Item, by: BoardBy): string {
  return by === "status" ? item.issue.status : (item.card?.column ?? NONE_COLUMN);
}

/** The title of the column `id` among `columns`, or the id where the Board draws no such column. */
export function columnTitle(columns: readonly BoardColumn[], id: string): string {
  return columns.find((column) => column.id === id)?.title ?? id;
}

/** One cell of the Board: a column, inside a lane where the Board has lanes, with its rows in
 *  the view's sort. */
export interface BoardCell {
  readonly column: BoardColumn;
  /** The cell's lane, or undefined on a Board with no lanes. */
  readonly lane: PriorityLane | undefined;
  readonly items: readonly Item[];
}

/** One line of the Board: every column once, in one lane, or the whole Board with no lanes. */
export interface BoardLine {
  readonly lane: PriorityLane | undefined;
  readonly cells: readonly BoardCell[];
}

/** A cell's drop id, and how the page names it: `column:<id>`, or `lane:<l>|column:<id>`. */
export function cellId(column: string, lane: PriorityLane | undefined): string {
  return lane === undefined ? `column:${column}` : `lane:${laneKey(lane)}|column:${column}`;
}

/** The prefix of a card's drop id. */
const CARD_DROP_PREFIX = "card:";

/** A card's drop id. Its drag id is its key, as dnd-kit keeps drags and drops apart. */
export function cardDropId(key: string): string {
  return `${CARD_DROP_PREFIX}${key}`;
}

/** The cell of `cells` that holds the card of `key`, and the card's place in it, or undefined
 *  where no cell holds it. */
export function cardIn(
  cells: readonly BoardCell[],
  key: string
): { readonly cell: BoardCell; readonly index: number } | undefined {
  for (const cell of cells) {
    const index = cell.items.findIndex((item) => item.issue.key === key);
    if (index !== -1) return { cell, index };
  }
  return undefined;
}

/**
 * The Board's lines: one with every column, or one for each priority lane (P0 to P3, then no
 * priority). Each cell keeps the rows in the view's order, which is the view's sort. A row whose
 * column the Board does not draw is in no cell.
 */
export function boardLines(
  rows: readonly Item[],
  columns: readonly BoardColumn[],
  by: BoardBy,
  lanes: boolean
): BoardLine[] {
  const placeOf = (column: string, lane: PriorityLane | undefined) =>
    lane === undefined ? column : `${laneKey(lane)}|${column}`;
  const buckets = new Map<string, Item[]>();
  for (const item of rows) {
    const key = placeOf(columnOf(item, by), lanes ? issuePriority(item.issue.priority) : undefined);
    const bucket = buckets.get(key);
    if (bucket === undefined) buckets.set(key, [item]);
    else bucket.push(item);
  }
  const laneList: readonly (PriorityLane | undefined)[] = lanes ? priorityLanes : [undefined];
  return laneList.map((lane) => ({
    lane,
    cells: columns.map((column) => ({
      column,
      lane,
      items: buckets.get(placeOf(column.id, lane)) ?? [],
    })),
  }));
}

/** Where a move puts a card: a cell by its column and lane, and the place in it, counted among
 *  the cell's cards without the moving one. */
export interface BoardPlace {
  readonly column: string;
  readonly lane: PriorityLane | undefined;
  readonly index: number;
}

/**
 * Where a drop lands. Over a card, that card's place in its cell: in its own cell a card one step
 * down lands below the card it passed and one step up above it, as dnd-kit's `arrayMove` moves
 * it, and in another cell it lands above the card. Over a column or a rail, the cell's end.
 * Undefined for a drop onto the card itself, or onto nothing the Board drew.
 */
export function dropPlace(
  lines: readonly BoardLine[],
  activeKey: string,
  overId: string
): BoardPlace | undefined {
  const cells = lines.flatMap((line) => line.cells);
  if (overId === cardDropId(activeKey)) return undefined;
  if (overId.startsWith(CARD_DROP_PREFIX)) {
    const found = cardIn(cells, overId.slice(CARD_DROP_PREFIX.length));
    return found === undefined
      ? undefined
      : { column: found.cell.column.id, lane: found.cell.lane, index: found.index };
  }
  const cell = cells.find((candidate) => cellId(candidate.column.id, candidate.lane) === overId);
  return cell === undefined
    ? undefined
    : {
        column: cell.column.id,
        lane: cell.lane,
        index: cell.items.filter((item) => item.issue.key !== activeKey).length,
      };
}

/** What a move writes, and the rank the card takes on the page until the server answers with
 *  the one it gave, where the move writes a rank (`rankAfter`). */
export interface BoardChange {
  readonly input: UpdateIssueInput;
  readonly rank?: string;
}

/** The rules a move on the Board follows: its grouping, its lanes, and why the view's sort lets
 *  no rank decide where a card lands, if it does not (`rankRefusal`). */
export interface MoveRules {
  readonly by: BoardBy;
  readonly lanes: boolean;
  readonly rankRefusal: string | undefined;
}

/** A move the Board takes, or one it refuses with the text that says why. `position` is where
 *  the card lands in its cell, counted from 1, where the rank decides it; elsewhere the view's
 *  sort puts the card, which only the drawn Board tells. */
export type MovePlan =
  | { readonly kind: "refused"; readonly text: string }
  | {
      readonly kind: "move";
      readonly key: string;
      readonly change: BoardChange;
      readonly position?: { readonly at: number; readonly of: number };
    };

/**
 * Why the view's sort lets no rank decide where a card lands within a column, or undefined while
 * it sorts by Place, first to last, which on Open is Place and then rank. `sort` is the view's
 * first sort and `header` the name of the column it sorts by. A sorted Board takes no move within
 * a column, since the sort would put the card back (GitHub Projects' rule).
 */
export function rankRefusal(
  sort: { readonly id: string; readonly desc: boolean } | undefined,
  header: string
): string | undefined {
  if (sort === undefined) return "The view has no sort. Sort by Queue to move cards in a column.";
  if (sort.id !== "queue")
    return `The view sorts by ${header}. Sort by Queue to move cards in a column.`;
  return sort.desc
    ? "The view sorts by Queue from last to first. Sort from first to last to move cards in a column."
    : undefined;
}

/** Whether the rank decides where `item` lands between `above` and `below`: all of them have a
 *  place in the Legion queue, or none has, since the Place sort puts every card with one first. */
function sameStanding(item: Item, above: Item | undefined, below: Item | undefined): boolean {
  const placed = item.place !== undefined;
  return [above, below].every(
    (neighbour) => neighbour === undefined || (neighbour.place !== undefined) === placed
  );
}

/**
 * A rank that sorts just after `after`, or before every other where there is none, which the
 * moved card shows until the server answers with the rank it wrote: ranks compare as strings,
 * code unit by code unit, as the Queue column's sort reads them, so one more character after
 * `after` sorts after it and before every greater rank, and the empty rank before every other.
 */
export function rankAfter(after: string | undefined): string {
  return after === undefined ? "" : `${after}\u0000`;
}

/**
 * The move a drop or a key asks for, by the Board's rules (the spec's Design, "The Board").
 *
 * By Status, a move within a cell writes the rank, and only where the rank decides where the card
 * lands: while the view sorts by Place, first to last, and the card and the cards around its new
 * place all have a place in the Legion queue or all have none. The rank names one neighbour: the
 * card the move lands above, or the card above it where it lands last in the cell. Any other move
 * within a cell is refused with a text that says why. A move into another column sets that
 * status, whatever the card's Legion phase, and into another lane that lane's priority, under any
 * sort; each writes the rank only where the same rule lets it, and the card otherwise lands where
 * the sort puts it.
 *
 * By Legion phase the daemon puts each card in its phase, so the one move taken is one within
 * Queue, under the same rank rule; every other move is refused with `DAEMON_SETS_PHASE`.
 *
 * Undefined where the move changes nothing: the card stays where it is.
 */
export function planMove(
  lines: readonly BoardLine[],
  rules: MoveRules,
  key: string,
  to: BoardPlace
): MovePlan | undefined {
  const cells = lines.flatMap((line) => line.cells);
  const found = cardIn(cells, key);
  const target = cells.find((cell) => cell.column.id === to.column && cell.lane === to.lane);
  if (found === undefined || target === undefined) return undefined;
  const from = found.cell;
  const item = from.items[found.index];
  if (item === undefined) return undefined;
  const within = from === target;
  const others = target.items.filter((candidate) => candidate.issue.key !== key);
  const index = Math.min(Math.max(to.index, 0), others.length);
  if (within && index === found.index) return undefined;
  if (rules.by === "legion_phase" && !(within && to.column === QUEUE_COLUMN))
    return { kind: "refused", text: DAEMON_SETS_PHASE };
  const above = others[index - 1];
  const below = others[index];
  const ranked = rules.rankRefusal === undefined && sameStanding(item, above, below);
  // The Place sort puts the cards with a place in the Legion queue first.
  if (within && !ranked)
    return {
      kind: "refused",
      text:
        rules.rankRefusal ??
        (item.place === undefined
          ? "A card with no place in the Legion queue stays after the cards with a place."
          : "A card with a place in the Legion queue stays before the cards with no place."),
    };
  const status = rules.by === "status" && to.column !== item.issue.status ? to.column : undefined;
  const lane = rules.lanes ? to.lane : undefined;
  const priority =
    lane === undefined || lane === issuePriority(item.issue.priority) ? undefined : { lane };
  // One neighbour, so the server takes the rank whatever order the neighbours' own ranks run in:
  // the daemon gives Places in an order of its own, so two cards next to each other by Place need
  // not be in rank order, and the server refuses a rank between two that are not.
  let rank: UpdateIssueInput["rank"] | undefined;
  if (ranked && below !== undefined) rank = { before: below.issue.key };
  else if (ranked && above !== undefined) rank = { after: above.issue.key };
  const input: UpdateIssueInput = {
    ...(status === undefined ? {} : { status }),
    ...(priority === undefined ? {} : { priority: priority.lane }),
    ...(rank === undefined ? {} : { rank }),
  };
  if (Object.keys(input).length === 0) return undefined;
  return {
    kind: "move",
    key,
    change: {
      input,
      ...(rank === undefined ? {} : { rank: rankAfter(above?.issue.rank) }),
    },
    ...(ranked ? { position: { at: index + 1, of: others.length + 1 } } : {}),
  };
}

/**
 * Why a key move of a card in `column` is refused before the Board looks where it would land, so
 * a refusal is read out even at the column's end: `within` is Shift+J and Shift+K, `across`
 * Shift+H and Shift+L. By Legion phase every key move is refused but one within Queue, which
 * follows the rank rule, as a move within a column does by Status.
 */
export function keyRefusal(
  rules: MoveRules,
  column: string,
  direction: "within" | "across"
): string | undefined {
  if (rules.by === "legion_phase" && (direction === "across" || column !== QUEUE_COLUMN))
    return DAEMON_SETS_PHASE;
  return direction === "within" ? rules.rankRefusal : undefined;
}
