// The Legion parts of the Issues tab that hold no React and no query cache: the board's phase
// order, the phases of a board's own columns (`legionPhasesOf`), the time formats a cell and a
// card write, where each listed row takes its Legion facts from (the board's `issues` map, grown
// to the page's clock, else the items route of `legion-work-time`), the windows and pages that
// items read spans, and the rule that keeps an older server's Done answer off the page. The page
// reads what these name (`api/queries.ts`); the tests read the rules here rather than through the
// markup.
import type {
  EvalItemRow,
  EvalItemsResponse,
  IssueSummary,
  LegionBoardColumn,
  LegionBoardFacts,
  LegionBoardResponse,
} from "../../api/types";
import { HOUR_MS } from "../../lib/closed-window";
import { DAY_MS, MAX_WINDOW_DAYS } from "../delivery/lib/window";

/** The moment an RFC 3339 time names, in milliseconds since the epoch, or undefined for no time
 *  or one that does not parse: where a card's time runs from, or when a row closed. */
export function sinceMs(since: string | null | undefined): number | undefined {
  if (since === null || since === undefined) return undefined;
  const at = Date.parse(since);
  return Number.isNaN(at) ? undefined : at;
}

/**
 * The Legion phases of today's board route, in its order (`legionBoardColumnOrder` in
 * `packages/envoy/internal/dispatch/api/legion_board.go`): every column of the route, each by its
 * id and its title, with Retro between Accepting and Merging. An issue is in a phase only while
 * the daemon holds a card for it, and its Dispatch status is a column of its own. Once the board
 * answers, the page takes the phases from the route's own columns (`legionPhasesOf`), so a column
 * a newer server adds is a phase too; until then, and in a project with no daemon, it draws these.
 * Time per phase draws the ones from Design to Held (`WORK_PHASES`). Each id is one of the
 * generated column ids (`LegionBoardColumn["id"]`), and `issues-legion.test.ts` holds this list to
 * that whole set: a column that the route adds and this list lacks fails that test.
 */
export const LEGION_PHASES = [
  { id: "queue", title: "Queue" },
  { id: "design", title: "Design" },
  { id: "planning", title: "Planning" },
  { id: "implementing", title: "Implementing" },
  { id: "testing", title: "Testing" },
  { id: "reviewing", title: "Reviewing" },
  { id: "accepting", title: "Accepting" },
  { id: "retro", title: "Retro" },
  { id: "merging", title: "Merging" },
  { id: "awaiting_children", title: "Waiting on children" },
  { id: "awaiting_person", title: "Waiting on a person" },
  { id: "held", title: "Held" },
] as const satisfies readonly { readonly id: LegionBoardColumn["id"]; readonly title: string }[];

/** A board column's id: the address value of a Legion phase (`f.legion_phase=implementing`). */
export type LegionPhase = (typeof LEGION_PHASES)[number]["id"];

/** One Legion phase as the page draws it: its board column's id, which the address keeps, and
 *  the title it reads by. */
export interface LegionPhaseColumn {
  readonly id: string;
  readonly title: string;
}

/** The Legion phases of `board`: its columns, in its order and under its labels, a column a newer
 *  server adds among them. Until the board answers, and in a project with no daemon, which reads
 *  none, they are the phases of today's route (`LEGION_PHASES`). A board's columns map to a new
 *  list at each call: the view keeps the list it drew while the columns are the same
 *  (`useLegionPhases` in `IssuesTable.tsx`). */
export function legionPhasesOf(
  board: Pick<LegionBoardResponse, "columns"> | undefined
): readonly LegionPhaseColumn[] {
  if (board === undefined) return LEGION_PHASES;
  return (board.columns ?? []).map(({ id, label }) => ({ id, title: label }));
}

const phaseTitles: ReadonlyMap<string, string> = new Map(
  LEGION_PHASES.map((phase) => [phase.id, phase.title])
);

/** A phase's title in today's route, or the id as it is for a phase that route does not have. */
function legionPhaseTitle(phase: string): string {
  return phaseTitles.get(phase) ?? phase;
}

/** The part of `hours_by_phase` that no phase took (`breakdownOther` on #3578). */
export const OTHER_PHASE = "other";

/** The Legion phases Time per phase has no fixed colour for: Queue, which never has hours, and
 *  Waiting on children, the route's twelfth column, which the spec's Time per phase does not
 *  list. Their hours count as Other's. */
const UNCOLOURED_PHASES: readonly string[] = ["queue", "awaiting_children"];

/** Time per phase's phases, in the board's order and Other last: the Legion phases from Design to
 *  Held that have a fixed colour (`legionPhaseFill`). A key of `hours_by_phase` that is none of
 *  these, such as a phase a newer server adds or Waiting on children, has no colour of its own:
 *  its hours count as Other's. */
export const WORK_PHASES: readonly string[] = [
  ...LEGION_PHASES.flatMap((phase) => (UNCOLOURED_PHASES.includes(phase.id) ? [] : [phase.id])),
  OTHER_PHASE,
];

/** The phase of Time per phase that a key of `hours_by_phase` counts under: the key where it is
 *  one of `WORK_PHASES`, else Other. */
export const workPhaseOf = (key: string): string => (WORK_PHASES.includes(key) ? key : OTHER_PHASE);

/** A phase of `hours_by_phase` that Time per phase draws (`WORK_PHASES`) by its name: a Legion
 *  phase's title, or Other. */
export const workPhaseTitle = (phase: string): string =>
  phase === OTHER_PHASE ? "Other" : legionPhaseTitle(phase);

/** How long the issue has been in its phase, from the moment the daemon's state first showed it
 *  there: minutes under an hour, hours and minutes under a day, then days and hours. An issue
 *  whose phase entry no read has recorded shows an em dash rather than a time nobody measured. */
export function formatPhaseTime(since: string | null, nowMs: number): string {
  const at = sinceMs(since);
  return at === undefined ? "—" : formatSpan(Math.floor(Math.max(0, nowMs - at) / 60_000));
}

/** The work time of the issue's facts, in the same words as the time in its phase. The hours are
 *  the `work_hours` of the stored row. A board entry that grows adds its growth from `as_of` to
 *  the page's clock (`boardFactsAt`). */
export function formatWorkTime(hours: number): string {
  return formatSpan(Math.floor(Math.max(0, hours) * 60));
}

/** A span of whole minutes in the page's words: minutes under an hour, hours and minutes under a
 *  day, then days and hours. */
function formatSpan(minutes: number): string {
  if (minutes < 60) return `${minutes} m`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours} h ${minutes % 60} m`;
  return `${Math.floor(hours / 24)} d ${hours % 24} h`;
}

/** The facts a row's Legion cells draw: Work time, Time per phase (`hours_by_phase`), Review
 *  rounds and Legion tree (`root_key`, `tree_depth`), with the close and the first worker event
 *  they are counted between. The board's `issues` map holds them whole for the issues it carries,
 *  and an items row of `legion-work-time` carries the same facts as columns (`itemFacts`). */
export type LegionFacts = Pick<
  LegionBoardFacts,
  | "closed_at"
  | "hours_by_phase"
  | "review_rounds"
  | "root_key"
  | "tree_depth"
  | "work_first_at"
  | "work_hours"
>;

/**
 * A board entry's facts closed at `nowMs`, as the board route closes a stored row at its read
 * (`legionfacts.Row.At`). The live view sends an entry again only when its stored row moves, so
 * the page grows the values itself. `work_hours` and each phase of `hours_by_phase` gain their
 * rate (`growth`) times the hours from `as_of` to `nowMs`. A phase that only the growth names
 * starts at 0. A clock before `as_of` adds nothing. An entry none of whose values grows carries
 * null for both `as_of` and `growth`, and it, an entry whose every rate is 0, and one whose
 * `as_of` this cannot read keep the values they were sent with, in the same object.
 */
export function boardFactsAt(entry: LegionBoardFacts, nowMs: number): LegionFacts {
  const { as_of: asOf, growth } = entry;
  if (asOf == null || growth == null) return entry;
  const hours = Math.max(0, nowMs - Date.parse(asOf)) / HOUR_MS;
  const workRate = growth.work_hours;
  const phaseRates = Object.entries(growth.hours_by_phase ?? {}).filter(([, rate]) => rate !== 0);
  if (Number.isNaN(hours) || hours === 0 || (workRate === 0 && phaseRates.length === 0))
    return entry;
  const hoursByPhase: Record<string, number> = { ...entry.hours_by_phase };
  for (const [phase, rate] of phaseRates) {
    const sent = Object.hasOwn(hoursByPhase, phase) ? hoursByPhase[phase] : 0;
    hoursByPhase[phase] = sent + rate * hours;
  }
  return {
    closed_at: entry.closed_at,
    hours_by_phase: hoursByPhase,
    review_rounds: entry.review_rounds,
    root_key: entry.root_key,
    tree_depth: entry.tree_depth,
    work_first_at: entry.work_first_at,
    work_hours: entry.work_hours + workRate * hours,
  };
}

/** A listed row's facts: the board's `issues[key]` where the map holds the key, closed at
 *  `nowMs` (`boardFactsAt`), else its items row (`items`, by `row_key`), else none, the facts of
 *  an issue with no Legion work. An items row is a closed issue's, and does not grow. A row both
 *  hold keeps the map's facts. Whether the items read is still out, or failed, is the caller's to
 *  tell apart from no Legion work: this reads only what has answered. */
export function legionFactsOf(
  key: string,
  board: LegionBoardResponse | undefined,
  items: Readonly<Record<string, LegionFacts>> | undefined,
  nowMs: number
): LegionFacts | undefined {
  const held = board?.issues;
  if (held && Object.hasOwn(held, key)) return boardFactsAt(held[key], nowMs);
  return items && Object.hasOwn(items, key) ? items[key] : undefined;
}

/** The prefix of an items row's column of one phase's hours (`hours_implementing`): the route
 *  writes each key of the stored `hours_by_phase` as a column of its own
 *  (`ColumnHoursPrefix` in `packages/envoy/internal/dispatch/legionfacts/items.go`). */
const HOURS_COLUMN_PREFIX = "hours_";

const numberIn = (cols: Readonly<Record<string, unknown>>, name: string): number | undefined => {
  const value = cols[name];
  return typeof value === "number" && Number.isFinite(value) ? value : undefined;
};

/** The facts an items row of `legion-work-time` carries, read from its columns as the route
 *  serves them (`itemColumns` in `legionfacts/items.go`): `work_hours`, `review_rounds`,
 *  `root_key`, `tree_depth`, `closed_at` and `work_first_at` under their own names, and the hours
 *  of each phase under `hours_<phase>`. A row without the four facts every issue row carries is
 *  no facts row, and reads as none. */
export function itemFacts(row: EvalItemRow): LegionFacts | undefined {
  const cols: Readonly<Record<string, unknown>> = row.cols ?? {};
  const workHours = numberIn(cols, "work_hours");
  const reviewRounds = numberIn(cols, "review_rounds");
  const treeDepth = numberIn(cols, "tree_depth");
  const { root_key: rootKey, closed_at: closedAt, work_first_at: workFirstAt } = cols;
  if (
    workHours === undefined ||
    reviewRounds === undefined ||
    treeDepth === undefined ||
    typeof rootKey !== "string"
  ) {
    return undefined;
  }
  const hoursByPhase: Record<string, number> = {};
  for (const name of Object.keys(cols)) {
    if (!name.startsWith(HOURS_COLUMN_PREFIX)) continue;
    const hours = numberIn(cols, name);
    if (hours !== undefined) hoursByPhase[name.slice(HOURS_COLUMN_PREFIX.length)] = hours;
  }
  return {
    closed_at: typeof closedAt === "string" ? closedAt : null,
    hours_by_phase: hoursByPhase,
    review_rounds: reviewRounds,
    root_key: rootKey,
    tree_depth: treeDepth,
    work_first_at: typeof workFirstAt === "string" ? workFirstAt : null,
    work_hours: workHours,
  };
}

/** The eval whose items carry a closed issue's Legion facts, one `issue` row per issue. */
export const LEGION_WORK_TIME = "legion-work-time";

/** The row kind of an issue's items row (`legionfacts.ItemRowKind`). */
const ISSUE_ROW_KIND = "issue";

/** The most rows one items read answers (`evalPageMax` in `api/evals.go`). */
export const ITEMS_PAGE_SIZE = 200;

/** One items read's window, as the route takes it: `from` included and `to` left out, each a
 *  UTC midnight. */
export interface ItemsWindow {
  readonly from: string;
  readonly to: string;
}

const midnight = (ms: number): string => `${new Date(ms).toISOString().slice(0, 10)}T00:00:00Z`;

/**
 * The windows that cover the UTC days from the day of `firstMs` to the day of `lastMs`, both
 * included, oldest first. Each starts and ends at a UTC midnight and spans at most 84 days
 * (`MAX_WINDOW_DAYS`, `maxDeliveryWindow` in `api/delivery.go`). The route refuses a raw
 * `from`..`to` over 84 days before it widens the window to whole days, so a window that started
 * off midnight would cover an 85th day, or be refused. `to` is left out, as the route leaves it
 * out (`parseDeliveryWindow`, then `evalDaysOf` in `api/evals.go`), so each window ends at the
 * midnight after its last day.
 */
export function closedDaysWindows(firstMs: number, lastMs: number): readonly ItemsWindow[] {
  const start = Math.floor(firstMs / DAY_MS) * DAY_MS;
  const end = (Math.floor(lastMs / DAY_MS) + 1) * DAY_MS;
  const windows: ItemsWindow[] = [];
  for (let from = start; from < end; from += MAX_WINDOW_DAYS * DAY_MS) {
    windows.push({
      from: midnight(from),
      to: midnight(Math.min(from + MAX_WINDOW_DAYS * DAY_MS, end)),
    });
  }
  return windows;
}

/**
 * The windows the items read of `rows` spans: the UTC days from the oldest close of a row that
 * the board's map does not hold to the newest, or to `nowMs` for a read of every done row. None
 * where every closed row is the map's, or no row has closed.
 */
export function itemsWindows(
  rows: readonly Pick<IssueSummary, "key" | "closed_at">[],
  board: LegionBoardResponse | undefined,
  nowMs?: number
): readonly ItemsWindow[] {
  const held = board?.issues;
  let first = Number.POSITIVE_INFINITY;
  let last = Number.NEGATIVE_INFINITY;
  for (const row of rows) {
    if (held && Object.hasOwn(held, row.key)) continue;
    const closed = sinceMs(row.closed_at);
    if (closed === undefined) continue;
    first = Math.min(first, closed);
    last = Math.max(last, closed);
  }
  if (first === Number.POSITIVE_INFINITY) return [];
  return closedDaysWindows(first, Math.max(last, nowMs ?? last));
}

/**
 * Every issue row of `legion-work-time` closed in `windows`, as facts by issue key. The windows
 * are read at once, each page by page, `row_kind=issue` with `limit` 200 and `offset`, until
 * `offset` and the page's rows reach the answer's `total`. A key two windows hold takes the later
 * window's facts. `list` reads one page (`api.listEvalItems` for the project). A refusal it
 * throws fails the whole read, `404 EVAL_NOT_FOUND` among them: a failed read is never the empty
 * facts of an issue with no Legion work.
 */
export async function readLegionItems(
  windows: readonly ItemsWindow[],
  list: (
    query: ItemsWindow & {
      readonly row_kind: string;
      readonly limit: number;
      readonly offset: number;
    }
  ) => Promise<Pick<EvalItemsResponse, "items" | "total">>
): Promise<Readonly<Record<string, LegionFacts>>> {
  const readWindow = async (window: ItemsWindow): Promise<Record<string, LegionFacts>> => {
    const facts: Record<string, LegionFacts> = {};
    let offset = 0;
    let total = 0;
    do {
      const page = await list({
        ...window,
        row_kind: ISSUE_ROW_KIND,
        limit: ITEMS_PAGE_SIZE,
        offset,
      });
      const rows = page.items ?? [];
      for (const row of rows) {
        const read = itemFacts(row);
        if (read !== undefined) facts[row.row_key] = read;
      }
      // A page with no rows ends the window, whatever `total` says: rows that left the window
      // between two pages would otherwise be asked for again and again.
      if (rows.length === 0) break;
      offset += rows.length;
      total = page.total;
    } while (offset < total);
    return facts;
  };
  return Object.assign({}, ...(await Promise.all(windows.map(readWindow))));
}

const NO_ROWS: readonly never[] = [];

/**
 * The rows a Done read lists. A server older than `closed_at`, during a rolling deploy, writes
 * no `closed_at` and ignores `closed_since` and `closed_before`. So a read with a window none of
 * whose rows carries `closed_at` lists no row, rather than every done issue. The view lists rows
 * again once a server that carries `closed_at` answers. Whether a page read's rows are in the
 * sort it sent is for its answer to say (`sort`), not its rows: a server that ignores `sort` may
 * write `closed_at` too, and the page read fails on such an answer (`UnsortedPageError`).
 */
export function listedDoneRows<Row extends { readonly closed_at?: string | null }>(
  rows: readonly Row[],
  windowed: boolean
): readonly Row[] {
  if (!windowed) return rows;
  return rows.some((row) => typeof row.closed_at === "string") ? rows : NO_ROWS;
}
