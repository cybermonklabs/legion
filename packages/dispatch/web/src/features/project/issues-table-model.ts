import { rankItem, rankings } from "@tanstack/match-sorter-utils";
import type { ColumnFiltersState } from "@tanstack/react-table";
import type {
  Actor,
  IssueSummary,
  LegionBoardCard,
  LegionBoardColumn,
  LegionBoardResponse,
  UpdateIssueInput,
} from "../../api/types";
import { fold } from "../../components/MultiSelect";
import type { LayoutColumns } from "../../components/table-layout";
import { HOUR_MS, inWindow, readWindow } from "../../lib/closed-window";
import { copyAddress, type TableAddressView } from "../../lib/page-address";
import { compareTimestamps } from "../../lib/timestamps";
import { type IssueStatus, issueStatuses, openIssueStatuses, statusText } from "./board-model";
import {
  ASKS_PICKS,
  activityPick,
  asksPick,
  createdByReader,
  DURATION_PICKS,
  type DurationPick,
  durationPick,
  LEGION_TREE_PICKS,
  legionPhasePicks,
  legionPick,
  ME,
  NONE_PICK,
  phaseColumnPick,
  phaseHoursPicks,
  QUEUE_PICKS,
  REVIEW_ROUND_PICKS,
  ROLE_PICKS,
  readAsksPick,
  reviewRoundsPick,
  sameSet,
  sinceDurationPick,
  WAITING_REASON_PICKS,
  WORKER_STATE_PICKS,
  waitingReasonPick,
  workerPicks,
} from "./issues-filter-picks";
import {
  LEGION_PHASES,
  type LegionFacts,
  type LegionPhaseColumn,
  legionFactsOf,
  WORK_PHASES,
  workPhaseTitle,
} from "./issues-legion";

/** An issue's card on the Legion board: the card, and the board column it is in, by its id and
 *  the board's own label for it. That column is the issue's Legion phase. */
export interface BoardCardRow {
  readonly card: LegionBoardCard;
  readonly column: LegionBoardColumn["id"];
  readonly label: string;
}

/** A read's status as TanStack Query names it (`query.status`): no answer yet, a failed read,
 *  or an answer. A caller hands a query's `status` as it is. */
export type ReadStatus = "pending" | "error" | "success";

/** What a project with a Legion daemon reads beside the listing, and how far each read is: the
 *  board route (`legionBoardQuery`), and the facts of the closed rows its `issues` map does not
 *  hold (`legionItemsQuery`). A read the page does not make, such as the items of a view that
 *  lists no closed row, is `success` with no answer. */
export interface LegionReads {
  readonly board: LegionBoardResponse | undefined;
  readonly boardStatus: ReadStatus;
  readonly items: Readonly<Record<string, LegionFacts>> | undefined;
  readonly itemsStatus: ReadStatus;
}

/** What the rows read beyond the listing: the Legion reads of a project with a Legion daemon
 *  (`undefined` for a project with none, for one whose daemon setting has not answered, and for
 *  a listing of no project), the signed-in login, which `f.author=@me` names, and the clock the
 *  rows are built at (`Item.at`), in milliseconds since the epoch. The clock is now where the
 *  caller gives none. */
export interface ItemContext {
  readonly legion?: LegionReads;
  readonly nowMs?: number;
  readonly viewer?: string;
}

export interface Item {
  readonly issue: IssueSummary;
  /** Its place in Start next's queue: the Todo issues labelled `legion`, in rank order. Start
   *  next keeps this rule of its own, apart from the Queue column's number (`place`). */
  readonly queue?: number;
  readonly author: string;
  readonly claimedBy: string;
  /** Whether an agent could pick this issue up now: nobody holds it, or the server would hand
   *  its claim to the next agent that asks. The Claimed by filter's "Nobody or lapsed". */
  readonly claimFree: boolean;
  readonly search: string;
  /** Its card on the board, where the daemon gives it one. A done issue has none. */
  readonly card?: BoardCardRow;
  /** Its Legion facts (Work time, Time per phase, Review rounds, Legion tree), where the read
   *  that holds them answered and it has Legion work. */
  readonly facts?: LegionFacts;
  /** How far the read of its facts is. Only `success` tells that an issue with no `facts` has no
   *  Legion work: a pending or failed read is not missing facts. */
  readonly factsRead: ReadStatus;
  /** The clock its row was built at, in milliseconds since the epoch. Facts from a board entry
   *  are closed at this clock (`boardFactsAt`). Its Time waiting and Time in Legion phase count
   *  to it, in a cell, on a card and in a filter. One build gives every row one clock, so all
   *  the times of a row are of one moment. */
  readonly at: number;
  /** The Queue column's number: the card's `waiting_place` in a project with a Legion daemon,
   *  else Start next's number (`queue`). */
  readonly place?: number;
  /** In the Legion queue: it has a place, or its card is in the board's Queue column. */
  readonly queued: boolean;
  /** Whether the reader created it, or an agent session the reader owns did (`f.author=@me`). */
  readonly mine: boolean;
}

/** The attributes the List's own rows carry, which the table core is told to read: its keys
 *  walk `data-issue-row`, its anchor holds a row's place by it and its groups by
 *  `data-issue-group`, and opening a row follows the link marked `data-issue-link`. */
export const ISSUE_ROW_ATTRIBUTE = "data-issue-row";
export const ISSUE_GROUP_ATTRIBUTE = "data-issue-group";
export const ISSUE_LINK_ATTRIBUTE = "data-issue-link";

/** The one key the List keeps a project's table under in the browser: the reader's layout, and
 *  the record of an action a reload interrupted. */
export const tableStorageKey = (project: string) => `dispatch.project-table.${project}`;

export const AGES = ["Today", "This week", "This month", "Older"] as const;
export const ALL_ROWS = Number.MAX_SAFE_INTEGER;
export const PAGE_SIZES = [25, 50, 100, 250, ALL_ROWS];
/** The statuses an issue is being worked in, which the priority pages count and list together.
 *  No status bar counts them as one: Open's `IssueStatusBar` on the Issues tab and the priority
 *  pages' `IssueTableStatus` count each status on its own. */
export const runningIssueStatuses: readonly IssueStatus[] = [
  "in_progress",
  "testing",
  "needs_review",
  "retro",
];
/** The Claimed by value that picks the issues nobody is working: no claim at all, or a claim
 *  the server would hand over (`claimIsTakeable`). The `?unclaimed=1` link sets it. */
export const UNCLAIMED_FACET = "Nobody or lapsed";
/** The Claimed by value of an issue no one has claimed, and the heading its group reads under.
 *  Which issues carry it is read off the issues alone: the agent registry never changes it. */
export const NOBODY_FACET = "Nobody";

/** How many of Finish's retries are in flight at once: the refused writes of an interrupted
 *  action, sent again (`useTableRecovery`'s `settle`). A selection is as large as the project, and
 *  "Select all N matching" is two clicks away, so Finish sends a bounded stream rather than every
 *  retry at once. Each write is still independent in the store. Start next itself sends one
 *  write at a time. */
export const WRITES_IN_FLIGHT = 8;

/** How a column filters, and everything a filter of its kind needs: a column that filters by
 *  picking values cannot exist without the values a row files under, nor one that filters by
 *  typed text without the text it reads. Matching, the URL parameter and the options a filter
 *  always offers live here too, so a column is one entry rather than a branch in three files. */
export type ColumnFilterSpec =
  | {
      readonly kind: "picks";
      /** Every value this row files under here: its filter options and its facet counts. A
       *  length of time is counted to the row's clock (`Item.at`). */
      readonly values: (item: Item) => readonly string[];
      /** The heading this row groups under; absent when the column cannot group. */
      readonly group?: (item: Item) => string;
      /** The order the filter lists its values in; by count when absent. */
      readonly order?: readonly string[];
      /** A value as the reader sees it, in an option and in a group's heading. */
      readonly show?: (value: string) => string;
      /** A row must carry every picked value, not just one of them. Every pick matches without
       *  regard to case (`matchesFilters`). */
      readonly match?: "all";
      /** The URL parameter the picks travel in; `f.<id>` when absent. */
      readonly param?: string;
      /** Values the filter offers whatever the loaded rows hold. */
      readonly always?: readonly string[];
      /** A value out of the URL as this column keeps it. */
      readonly normalize?: (value: string) => string;
    }
  | { readonly kind: "text"; readonly value: (item: Item) => string }
  /** A window of time, `<N>d`, `<from>..<to>` or `all` (`readWindow` in `lib/closed-window.ts`),
   *  over the moment `value` reads, which is null where the row has none. */
  | { readonly kind: "range"; readonly value: (item: Item) => string | null };

/** Every column the table has. Narrow enough that a rendering, a width or a minimum keyed by
 *  an id no column carries fails to compile. */
export type ColumnId =
  | "select"
  | "priority"
  | "queue"
  | "key"
  | "title"
  | "status"
  | "legion_phase"
  | "assignee"
  | "author"
  | "labels"
  | "claim"
  | "asks"
  | "created"
  | "updated"
  | "closed"
  | "roles"
  | "activity"
  | "waited"
  | "waiting_reason"
  | "time_in_phase"
  | "work_hours"
  | "review_rounds"
  | "legion"
  | "phase_hours";

/** One column's identity: everything about it that is not how its cell renders. The column
 *  definitions, `columnIds`, `groupable`, `facets`, the default widths and the minimums are all
 *  derived from this list, so a new column is one entry here plus its cell. These are the
 *  Issues tab's widths; a table another page draws keeps today's (`EMBEDDED_WIDTHS`). */
export interface ColumnSpec {
  readonly id: ColumnId;
  /** Its width before the reader drags it. */
  readonly size: number;
  /** How narrow a drag or a narrow window may take it below 1280 px, where every button is a
   *  44 px touch target: the whole header label, its sort mark and its 44 px filter button with
   *  the cell's padding, the mark left out where `markOutsideMin` says so. From 1280 px the
   *  filter button is 32 px, and a filtered column's minimum is 12 px less (`columnMinimums`),
   *  unless its cell sets it (`cellMin`). */
  readonly min: number;
  /** Its minimum holds its header and filter button without the sort mark, so the column is
   *  drawn wider by the mark (`SORT_MARK_WIDTH`) only while the table is sorted by it. */
  readonly markOutsideMin?: true;
  /** Its minimum is what its cell holds, not its header, so it is the same at any width: a key
   *  with its unread dot, two lines of a title, a status chip or a claim chip whole. */
  readonly cellMin?: true;
  /** How narrow it may go on a phone, where the key and the title are the row. */
  readonly compactMin?: number;
  /** Stays in view while the table scrolls sideways. */
  readonly pinned?: boolean;
  /** Its cells read the row's Legion facts (`LegionFacts`): a filter, a sort or a grouping of it
   *  waits for the read of those facts (`FACTS_COLUMNS`). */
  readonly facts?: true;
  /** Its source is the board route (the Design's columns table, "No daemon": hidden), so it shows
   *  in no project with no Legion daemon (`isBoardFedColumn`). */
  readonly boardFed?: true;
  /** Headers sort unless this says otherwise. */
  readonly sortable?: boolean;
  readonly filter?: ColumnFilterSpec;
}

const one = (
  value: (item: Item) => string,
  rest: {
    order?: readonly string[];
    show?: (value: string) => string;
    groups?: boolean;
    always?: readonly string[];
  } = {}
): ColumnFilterSpec => ({
  kind: "picks",
  values: (item) => [value(item)],
  ...(rest.groups === false ? {} : { group: value }),
  ...(rest.order === undefined ? {} : { order: rest.order }),
  ...(rest.show === undefined ? {} : { show: rest.show }),
  ...(rest.always === undefined ? {} : { always: rest.always }),
});

export const priorityName = (value: string) => (value === "" ? "No priority" : `P${value}`);
export function ageOf(iso: string): string {
  const days = (Date.now() - new Date(iso).getTime()) / 86_400_000;
  return days < 1 ? "Today" : days < 7 ? "This week" : days < 30 ? "This month" : "Older";
}

/**
 * How each of the five measures is taken, word for word as the Design's method table gives it:
 * the sentences its header's tooltip, a Board card's fact and the Columns menu's muted line say.
 * Each says what the server computes (`legion/metrics` and `api/legion_board.go` on #3578), and no
 * sentence has more than 20 words (R9).
 */
export const MEASURE_METHODS: Readonly<Partial<Record<ColumnId, string>>> = {
  work_hours:
    "Dispatch counts the hours from the first Legion worker event until the issue closes, or until Dispatch last read it. It does not count Backlog or Icebox hours, or hours when Legion has admitted the issue but not started it.",
  phase_hours:
    "Each hour goes to Waiting on a person while the issue's ask or root waits, else to that hour's phase. Other holds the rest, and a root's hours waiting on its children do not count.",
  review_rounds:
    "Dispatch counts each start of the Reviewing phase. For an issue with no recorded Legion phases, it counts each move to Needs review.",
  waited:
    "Dispatch shows the time since Legion asked a person, when the issue waits on a person. If it does not wait on a person, it shows the time since its Legion phase started.",
  time_in_phase:
    "Dispatch shows the time since Legion first recorded the issue in its current Legion phase.",
};

/** A value's words from `words`, or the value as it is where `words` has none: a value this
 *  build does not know keeps its own text, a name of an `Object` member (`constructor`) too. */
const wordsOf =
  (words: Readonly<Record<string, string>>) =>
  (value: string): string =>
    Object.hasOwn(words, value) ? words[value] : value;

const NONE_WORD = "None";

const durationWords = wordsOf({
  "under-1h": "Under 1 hour",
  "1h-1d": "1 hour to 1 day",
  "1d-7d": "1 to 7 days",
  "over-7d": "Over 7 days",
  [NONE_PICK]: NONE_WORD,
} satisfies Record<DurationPick, string>);

/** Worker state's words for each `slot_use`. */
export const workerStateWords = wordsOf({
  working: "Working",
  between_turns: "Between turns",
  held_children: "Waiting on child issues",
  held_person: "Waiting on a person",
  held_idle: "Idle",
  [NONE_PICK]: NONE_WORD,
});

/** Why it waits' words for each `waiting.reason`. */
export const waitingReasonWords = wordsOf({
  ready: "Ready to start",
  slot: "Capacity to go on",
  blocked: "A blocker",
  person: "A person",
  [NONE_PICK]: NONE_WORD,
});

/** A Legion role as the Workers column names it: `implementer` is Implementer. */
export const workerName = (role: string): string =>
  role === NONE_PICK ? NONE_WORD : `${role.slice(0, 1).toUpperCase()}${role.slice(1)}`;

/**
 * The Legion phase filter over `phases`, the board's columns: the board column a row's card is
 * in, None with no card, every phase offered in the board's order under its title whatever the
 * rows hold, then None. The registry's is over today's phases (`LEGION_PHASES`), which the page
 * draws until the board answers; the Issues tab draws the board's own (`IssueTableMode`).
 * `picks` are those phases' picks (`legionPhasePicks`), which a caller that has built them hands
 * in.
 */
export function legionPhaseFilter(
  phases: readonly LegionPhaseColumn[],
  picks: readonly string[] = legionPhasePicks(phases)
): ColumnFilterSpec {
  const titles = new Map(phases.map(({ id, title }) => [id, title]));
  return one((item) => phaseColumnPick(item.card?.column), {
    order: picks,
    show: (value) => (value === NONE_PICK ? NONE_WORD : (titles.get(value) ?? value)),
    always: picks,
  });
}

/** The sum of a row's hours by phase, 0 where its facts hold none: Time per phase's total, which
 *  its sort reads. */
export function phaseHoursTotal(facts: Pick<LegionFacts, "hours_by_phase"> | undefined): number {
  let total = 0;
  for (const hours of Object.values(facts?.hours_by_phase ?? {})) if (hours > 0) total += hours;
  return total;
}

/** The issue's work time in hours, or undefined where none was measured: an issue with no
 *  Legion work, one whose facts read has not answered, or one whose facts hold no measured work
 *  (`work_first_at` null, which the server answers with `work_hours` 0). A missing work time
 *  draws an empty cell, never 0. */
export function workHoursOf(item: Pick<Item, "facts">): number | undefined {
  const facts = item.facts;
  if (facts === undefined) return undefined;
  return facts.work_first_at === null && !(facts.work_hours > 0) ? undefined : facts.work_hours;
}

/** The filter of a column that holds a length of time: every duration pick, in its order. */
const duration = (pick: (item: Item) => readonly string[]): ColumnFilterSpec => ({
  kind: "picks",
  values: pick,
  order: DURATION_PICKS,
  show: durationWords,
  always: DURATION_PICKS,
});

/** The picks of a facts column: `picks` where the row's facts read has answered, else none,
 *  since a pending or failed read is not an issue with no Legion work. */
const factsPicks =
  (picks: (item: Item) => readonly string[]) =>
  (item: Item): readonly string[] =>
    item.factsRead === "success" ? picks(item) : [];

/** `created_at` and `created_by` as this build's pages read them while a release rolls out: a
 *  Dispatch process of the release before the one that added the two members answers with
 *  neither, and the table still has to draw the row ("Two builds at once",
 *  `packages/envoy/cmd/dispatch/README.md`). A parameter of this type takes an `IssueSummary`
 *  as it is, so no reader of the two members needs a cast. */
export interface CreatedMembers {
  readonly created_at?: string;
  readonly created_by?: Actor | null;
}
/** When the issue was created, or undefined from a server of the release before the one that
 *  added `created_at`. Every caller branches on it: `compareTimestamps` and `<Timestamp>` both
 *  need a time that parses. Without that branch, `IssuesTable.test.tsx`'s "a row a server sends
 *  without created_by or created_at renders, with no author and no time" fails. */
export const createdAt = (issue: CreatedMembers): string | undefined => issue.created_at;
/** Who created the issue, or null: both a server that says nobody did and one of the release
 *  before the one that added `created_by`, which says nothing at all. Either reads as Unknown,
 *  and without this, `IssuesTable.test.tsx`'s "a row a server sends without created_by or
 *  created_at renders, with no author and no time" fails in `AgentName`. */
export const createdBy = (issue: CreatedMembers): Actor | null => issue.created_by ?? null;
/** Orders two issues by when they were created, keeping one a server of the release before
 *  `created_at` sent without a time after every issue that carries one, rather than handing an
 *  absent value to `compareTimestamps`, which throws on anything it cannot parse. Without it,
 *  `IssuesTable.test.tsx`'s "the Legion queue puts an issue with no created_at after one of the
 *  same rank that has one" fails. */
export function compareCreated(left: CreatedMembers, right: CreatedMembers): number {
  const a = createdAt(left);
  const b = createdAt(right);
  if (a === undefined || b === undefined) {
    return a === b ? 0 : a === undefined ? 1 : -1;
  }
  return compareTimestamps(a, b);
}

export const COLUMNS: readonly ColumnSpec[] = [
  { id: "select", size: 44, min: 44, pinned: true, sortable: false },
  {
    id: "priority",
    size: 132,
    min: 132,
    pinned: true,
    filter: one((item) => String(item.issue.priority ?? ""), {
      order: ["0", "1", "2", "3", ""],
      show: priorityName,
    }),
  },
  {
    id: "queue",
    size: 124,
    min: 124,
    // The address keeps the values `Queued` and `Not queued` (`f.queue=Queued`), so a link of
    // today keeps working; the reader sees the set by its name, the Legion queue.
    filter: one((item) => (item.queued ? "Queued" : "Not queued"), {
      order: QUEUE_PICKS,
      show: wordsOf({ Queued: "In Legion queue", "Not queued": "Not in Legion queue" }),
      groups: false,
    }),
  },
  {
    // A ten-character key and its unread dot, whole.
    id: "key",
    size: 124,
    min: 124,
    compactMin: 124,
    cellMin: true,
    pinned: true,
    filter: { kind: "text", value: (item) => item.issue.key },
  },
  {
    // Two lines of the title at 240 px hold about 60 characters, half of a median INFRA title,
    // so its subject reads. Title gives way first, down to this.
    id: "title",
    size: 380,
    min: 240,
    compactMin: 120,
    cellMin: true,
    pinned: true,
    filter: { kind: "text", value: (item) => item.issue.title },
  },
  {
    // The widest status chip whole ("NEEDS REVIEW", 134 px) with the cell's padding.
    id: "status",
    size: 150,
    min: 150,
    cellMin: true,
    filter: {
      kind: "picks",
      values: (item) => [item.issue.status],
      group: (item) => item.issue.status,
      order: issueStatuses,
      // A `?status=` this bundle does not know stays under its own text: the filter applies
      // and matches nothing, which is what the Board has always done (`statusText`).
      show: statusText,
      // Shared with the Board, and every status is offered even before its rows are loaded.
      param: "status",
      always: issueStatuses,
    },
  },
  {
    // The board column the issue's card is in, beside its status and never merged with it. An
    // issue the daemon holds no card for has none. The address keeps the board's column ids
    // (`f.legion_phase=implementing`).
    id: "legion_phase",
    size: 168,
    min: 168,
    boardFed: true,
    // Every Legion phase is offered in the board's order, then None, whatever the rows hold.
    filter: legionPhaseFilter(LEGION_PHASES),
  },
  {
    id: "assignee",
    size: 150,
    min: 140,
    filter: one((item) => item.issue.assignee ?? "Unassigned"),
  },
  {
    // Me (`f.author=@me`) is the reader who opens the page, so a saved link means its reader.
    id: "author",
    size: 180,
    min: 156,
    filter: {
      kind: "picks",
      values: (item) => (item.mine ? [item.author, ME] : [item.author]),
      group: (item) => item.author,
      show: (value) => (value === ME ? "Me" : value),
    },
  },
  {
    id: "labels",
    size: 180,
    min: 124,
    filter: {
      kind: "picks",
      // The project's own spelling of each label, which is the Board's too: one `?label=` is
      // one cache key and one ticked option in both views. Matching is case-insensitive, so a
      // link in another casing still picks the same rows.
      values: (item) => item.issue.labels ?? [],
      match: "all",
      param: "label",
    },
  },
  {
    // The claim chip whole: "Claimed by", the holder's name, which truncates down to 48 px, the
    // time and "not running", with the cell's padding.
    id: "claim",
    size: 324,
    min: 324,
    cellMin: true,
    filter: {
      kind: "picks",
      values: (item) => (item.claimFree ? [item.claimedBy, UNCLAIMED_FACET] : [item.claimedBy]),
      group: (item) => item.claimedBy,
    },
  },
  {
    // Whether the issue has an open ask, not how many: the address keeps `any` and `none`, and a
    // link of the count the filter picked before keeps its rows (`readAsksPick`). Its minimum
    // leaves the sort mark out, so Open's first screen at 1440 px holds it and Created by whole in
    // the view's own sort; sorted by it, it is 150 px, and 138 px from 1280 px.
    id: "asks",
    size: 152,
    min: 140,
    markOutsideMin: true,
    filter: {
      kind: "picks",
      values: (item) => [asksPick(item.issue.open_asks)],
      order: ASKS_PICKS,
      show: wordsOf({ any: "With open asks", [NONE_PICK]: "No open asks" }),
      always: ASKS_PICKS,
      normalize: readAsksPick,
    },
  },
  {
    id: "created",
    size: 132,
    min: 132,
    // A server of the release before the one that added `created_at` sends none, so the filter
    // asks `createdAt` and files a row without a time under Older, the bucket an issue old
    // enough to come from that release would reach anyway. `AGES` keeps its four values. No row
    // of the suite fails without this one, since `ageOf` buckets an unreadable time as Older
    // through `NaN`; it says the case rather than resting on that.
    filter: one(
      (item) => {
        const at = createdAt(item.issue);
        return at === undefined ? "Older" : ageOf(at);
      },
      { order: AGES }
    ),
  },
  {
    id: "updated",
    size: 136,
    min: 136,
    filter: one((item) => ageOf(item.issue.updated_at), { order: AGES }),
  },
  {
    id: "closed",
    size: 124,
    min: 124,
    filter: { kind: "range", value: (item) => item.issue.closed_at },
  },
  {
    // The roles of the workers the daemon runs on the issue, or None.
    id: "roles",
    size: 136,
    min: 136,
    boardFed: true,
    filter: {
      kind: "picks",
      values: (item) => workerPicks(item.card?.card.roles),
      order: ROLE_PICKS,
      show: workerName,
    },
  },
  {
    // What the worker on an issue the daemon holds capacity for does now (`slot_use`).
    id: "activity",
    size: 168,
    min: 168,
    boardFed: true,
    filter: one((item) => activityPick(item.card?.card.slot_use), {
      order: WORKER_STATE_PICKS,
      show: workerStateWords,
      groups: false,
    }),
  },
  {
    id: "waited",
    size: 176,
    min: 176,
    boardFed: true,
    filter: duration((item) => [sinceDurationPick(item.card?.card.waiting?.since, item.at)]),
  },
  {
    // What holds the card up (`waiting.reason`): set for a card in the queue, and for a root
    // that waits on a person outside it.
    id: "waiting_reason",
    size: 164,
    min: 164,
    boardFed: true,
    filter: one((item) => waitingReasonPick(item.card?.card.waiting), {
      order: WAITING_REASON_PICKS,
      show: waitingReasonWords,
      groups: false,
    }),
  },
  {
    id: "time_in_phase",
    size: 224,
    min: 224,
    boardFed: true,
    filter: duration((item) => [sinceDurationPick(item.card?.card.phase_since, item.at)]),
  },
  {
    id: "work_hours",
    size: 148,
    min: 148,
    boardFed: true,
    facts: true,
    filter: duration(
      factsPicks((item) => {
        const hours = workHoursOf(item);
        return [durationPick(hours === undefined ? undefined : hours * HOUR_MS)];
      })
    ),
  },
  {
    id: "review_rounds",
    size: 180,
    min: 180,
    boardFed: true,
    facts: true,
    filter: {
      kind: "picks",
      values: factsPicks((item) => [reviewRoundsPick(item.facts?.review_rounds)]),
      order: REVIEW_ROUND_PICKS,
      show: wordsOf({ "3-or-more": "3 or more", [NONE_PICK]: NONE_WORD }),
      always: REVIEW_ROUND_PICKS,
    },
  },
  {
    // Where the issue stands in its Legion tree: the root, a child under it, or None for an
    // issue with no Legion work. Two picks select their union.
    id: "legion",
    size: 156,
    min: 156,
    boardFed: true,
    facts: true,
    filter: {
      kind: "picks",
      values: factsPicks((item) => [legionPick(item.facts)]),
      order: LEGION_TREE_PICKS,
      show: wordsOf({ root: "Root", child: "Child", [NONE_PICK]: NONE_WORD }),
      always: LEGION_TREE_PICKS,
    },
  },
  {
    // Its filter picks the phases with hours and has no None: a row with no hours is in no pick.
    id: "phase_hours",
    size: 284,
    min: 284,
    // On a phone its header stacks its name over its Share and Hours control and its filter
    // (`STACKED_HEADERS` in `issues-table-frame.tsx`), so it holds the wider of those two lines:
    // not the label with its sort mark (118 px) but the control (94 px with 44 px buttons), a 4
    // px gap and the 44 px filter button, with the cell's 16 px of padding from `md` (12 px below
    // it).
    compactMin: 158,
    boardFed: true,
    facts: true,
    // Its minimum holds, on one line, its label with its sort mark (118 px drawn), the Share and
    // Hours control and the filter button, with 44 px buttons and the cell's padding; from 1280
    // px, with 32 px filter buttons, 12 px less.
    filter: {
      kind: "picks",
      values: factsPicks((item) => phaseHoursPicks(item.facts?.hours_by_phase)),
      order: WORK_PHASES,
      show: workPhaseTitle,
    },
  },
];
export const columnSpec: ReadonlyMap<string, ColumnSpec> = new Map(
  COLUMNS.map((column) => [column.id, column])
);
export const columnFilterSpec = (id: string): ColumnFilterSpec | undefined =>
  columnSpec.get(id)?.filter;
/** The columns a URL may name in `sort=` or a filter parameter: every one a reader controls. */
export const columnIds: readonly string[] = COLUMNS.filter(
  (column) => column.sortable !== false || column.filter !== undefined
).map((column) => column.id);
/** The columns whose cells read the Legion facts (`ColumnSpec`'s `facts`). */
export const FACTS_COLUMNS: readonly string[] = COLUMNS.filter((column) => column.facts).map(
  (column) => column.id
);
/** Whether a column's source is the board route (`ColumnSpec`'s `boardFed`). */
export const isBoardFedColumn = (id: string): boolean => columnSpec.get(id)?.boardFed === true;
const groupable: readonly string[] = COLUMNS.filter(
  (column) => column.filter?.kind === "picks" && column.filter.group !== undefined
).map((column) => column.id);
export const canGroupBy = (id: string) => groupable.includes(id);
/** How much wider a button is below 1280 px, where every button is a 44 px touch target
 *  (`styles.css`), than a filter button's 32 px from 1280 px. */
const TOUCH_TARGET_GAIN = 44 - 32;
const minimumsFor = (touch: boolean): Readonly<Partial<Record<string, number>>> =>
  Object.fromEntries(
    COLUMNS.map((column) => [
      column.id,
      touch || column.filter === undefined || column.cellMin
        ? column.min
        : column.min - TOUCH_TARGET_GAIN,
    ])
  );
const TOUCH_MINIMUMS = minimumsFor(true);
const WIDE_MINIMUMS = minimumsFor(false);
/** Each registry column's minimum by id, with the buttons in use: 44 px touch targets below
 *  1280 px (`touch`), 32 px filter buttons from 1280 px. Looked up by a column id, so a lookup
 *  can miss: a column the registry gives no such width. */
export const columnMinimums = (touch: boolean) => (touch ? TOUCH_MINIMUMS : WIDE_MINIMUMS);
/** The registry's sorted minimums with the buttons `minimums` has: each column's own minimum,
 *  which holds its header with the sort mark, and none for a column whose minimum leaves the
 *  mark out (`markOutsideMin`), which the layout widens by the mark while it is sorted. */
const sortedMinimumsOf = (
  minimums: Readonly<Partial<Record<string, number>>>
): Readonly<Partial<Record<string, number>>> =>
  Object.fromEntries(
    COLUMNS.flatMap((column) => (column.markOutsideMin ? [] : [[column.id, minimums[column.id]]]))
  );
const TOUCH_SORTED_MINIMUMS = sortedMinimumsOf(TOUCH_MINIMUMS);
const WIDE_SORTED_MINIMUMS = sortedMinimumsOf(WIDE_MINIMUMS);
const COMPACT_COLUMN_MIN: Readonly<Partial<Record<string, number>>> = Object.fromEntries(
  COLUMNS.flatMap((column) =>
    column.compactMin === undefined ? [] : [[column.id, column.compactMin]]
  )
);
/** A column's narrowest on a phone, which has touch targets: its phone minimum where the
 *  registry gives one. */
export const phoneMin = (id: string): number => COMPACT_COLUMN_MIN[id] ?? TOUCH_MINIMUMS[id] ?? 0;

/** One width rule of a table another page draws: its starting width, its minimum, and its
 *  narrowest on a phone where that differs. */
interface EmbeddedWidth {
  readonly size: number;
  readonly min: number;
  readonly phone?: number;
}

/**
 * The registry columns an issue table another page draws has (the priority pages'), at today's
 * widths: those tables keep today's columns and look, but for a width that cut its header, which
 * widens to hold it (R13): Key's on a phone. They also offer Closed, with its date range filter,
 * hidden until the reader shows it. The Legion columns read a project's daemon board, so they are
 * the Issues tab's alone.
 */
const EMBEDDED_WIDTHS: Readonly<Partial<Record<ColumnId, EmbeddedWidth>>> = {
  select: { size: 44, min: 44 },
  priority: { size: 140, min: 140 },
  queue: { size: 96, min: 124 },
  // On a phone its header whole, sorted by it or not: its sort button, a 44 px touch target
  // that holds Key and the mark (38 px), its 44 px filter button and the cell's 12 px padding
  // (measured: at 88 px the filter spilled 12 px past the cell). A phone-wide box in a window
  // 1280 px wide or wider, with 32 px filter buttons, needs 90 px sorted.
  key: { size: 104, min: 108, phone: 100 },
  title: { size: 380, min: 116, phone: 120 },
  status: { size: 128, min: 132 },
  assignee: { size: 180, min: 148 },
  author: { size: 200, min: 164 },
  labels: { size: 200, min: 132 },
  claim: { size: 220, min: 164 },
  asks: { size: 96, min: 148 },
  created: { size: 120, min: 148 },
  updated: { size: 120, min: 148 },
  closed: { size: 120, min: 148 },
};
/** Those columns, in the registry's order. */
const EMBEDDED_IDS: readonly ColumnId[] = COLUMNS.flatMap(({ id }) =>
  EMBEDDED_WIDTHS[id] === undefined ? [] : [id]
);
/** A column's narrowest on a phone in a table another page draws: the width the registry names
 *  for a phone, else `named`, the minimum the table's layout names (`widths`), else today's. */
export const embeddedPhoneMin = (id: string, named?: number): number => {
  const width = EMBEDDED_WIDTHS[id as ColumnId];
  return width?.phone ?? named ?? width?.min ?? 0;
};
/** Today's minimums of a table another page draws. Each holds its header with the sort mark as
 *  well (measured: Open asks' 148 px holds its 83 px label and mark), so each is its column's
 *  sorted minimum too, and no column there grows while the table is sorted by it. */
const EMBEDDED_MINIMUMS: Readonly<Partial<Record<string, number>>> = Object.fromEntries(
  EMBEDDED_IDS.map((id) => [id, EMBEDDED_WIDTHS[id]?.min])
);

/**
 * The layout of an issue table another page draws, for `useTableLayout`: today's columns at
 * today's widths and never below today's minimums, with Updated and Closed hidden until a reader
 * shows them. Select, Priority, Key and Title stay pinned within 60% of a box 640 px wide or
 * wider, Title narrowing first, from a width the reader dragged too (today's rule, R13); on a
 * phone, where the key and the title are what identify a row, Select, Key and Title stay pinned
 * within 85% of the box, Key at its phone width and Title taking what is left down to its phone
 * minimum, so an edge of the next column shows the table scrolls sideways. Every saved
 * `dispatch.project-table.<key>` record was written for these columns, whether its key is a
 * project or the one key every table scoped to a priority shares (`SCOPED_LAYOUT_KEY` in
 * `ListingTable.tsx`).
 */
export const ISSUE_TABLE_COLUMNS: LayoutColumns = {
  ids: EMBEDDED_IDS,
  rowAttribute: ISSUE_ROW_ATTRIBUTE,
  hiddenByDefault: { updated: false, closed: false } satisfies Partial<Record<ColumnId, boolean>>,
  sizes: Object.fromEntries(EMBEDDED_IDS.map((id) => [id, EMBEDDED_WIDTHS[id]?.size])),
  minimums: EMBEDDED_MINIMUMS,
  sortedMinimums: EMBEDDED_MINIMUMS,
  pins: {
    wide: { columns: COLUMNS.filter((column) => column.pinned).map(({ id }) => id), share: 0.6 },
    phone: {
      columns: ["select", "key", "title"] satisfies ColumnId[],
      share: 0.85,
      widths: { key: embeddedPhoneMin("key") },
      flexibleMin: embeddedPhoneMin("title"),
    },
    flexible: "title" satisfies ColumnId,
  },
};

const viewLayouts = new Map<string, LayoutColumns>();

/**
 * One view's layout on the Issues tab, for `useTableLayout`: every registry column, Select and
 * `shown` (`shownColumns` in `issues-views.ts`) first in that order, which is the order a view
 * draws until the reader drags a column, and every other column hidden until the reader shows
 * it. Each column starts at the registry's width and never goes below its minimum with the
 * buttons in use (`touch`, `columnMinimums`), nor, while the view is sorted by it, below the one
 * that holds its sort mark too: the same minimum for every column but Open asks, which takes
 * the mark on top (`markOutsideMin`). The pinned columns the view shows (Select, Priority, Key
 * and Title) stay within 60% of a box 640 px wide or wider, Title narrowing first until the
 * reader drags its width (R10): a Title the reader dragged is drawn at that width, and scrolls
 * with the table where it would take the pins past their share. On a phone Select and Key stay
 * pinned, and Title, which the Issues tab draws right after them (`phoneOrder` in
 * `issues-tab-layout.ts`), fills the rest of the box down to its phone minimum. Held per set of
 * columns and button size, since the stored layout is read again whenever the rules' identity
 * changes.
 */
export function viewLayoutColumns(shown: readonly string[], touch: boolean): LayoutColumns {
  const name = `${shown.join(",")}|${touch}`;
  const known = viewLayouts.get(name);
  if (known !== undefined) return known;
  const ids = [
    ...new Set<string>([
      "select",
      ...shown.filter((id) => columnSpec.has(id)),
      ...COLUMNS.map(({ id }) => id),
    ]),
  ];
  const visible = new Set<string>(["select", ...shown]);
  const made: LayoutColumns = {
    ids,
    rowAttribute: ISSUE_ROW_ATTRIBUTE,
    hiddenByDefault: Object.fromEntries(
      ids.flatMap((id) => (visible.has(id) ? [] : [[id, false]]))
    ),
    sizes: Object.fromEntries(COLUMNS.map((column) => [column.id, column.size])),
    minimums: columnMinimums(touch),
    sortedMinimums: touch ? TOUCH_SORTED_MINIMUMS : WIDE_SORTED_MINIMUMS,
    pins: {
      wide: {
        columns: COLUMNS.filter((column) => column.pinned && visible.has(column.id)).map(
          ({ id }) => id
        ),
        share: 0.6,
        keepDragged: true,
      },
      phone: {
        columns: (["select", "key"] satisfies ColumnId[]).filter((id) => visible.has(id)),
        // Just under the whole box: its two border pixels are not the rows'.
        share: 0.99,
        widths: { key: phoneMin("key"), phase_hours: phoneMin("phase_hours") },
        flexibleMin: phoneMin("title"),
      },
      flexible: "title" satisfies ColumnId,
    },
  };
  viewLayouts.set(name, made);
  return made;
}

/**
 * The widths a view draws its columns at in a box `boxWidth` wide until the reader drags one:
 * where `ids`, the columns shown in their order, and the `trailing` width after them do not fit,
 * Title gives way first, down to its minimum, then every other column but Select toward its
 * minimum, each by its share of the room they have. What still does not fit scrolls sideways
 * under the pinned columns. `widths` are the ones the layout draws (`useTableLayout`'s
 * `responsive.sizing`).
 */
export function fitViewWidths(
  ids: readonly string[],
  widths: Readonly<Record<string, number>>,
  minimums: Readonly<Partial<Record<string, number>>>,
  boxWidth: number,
  trailing: number
): Record<string, number> {
  const fitted = { ...widths };
  // The box's two border pixels are not the columns'.
  let over = ids.reduce((sum, id) => sum + (fitted[id] ?? 0), 0) + trailing - boxWidth + 2;
  const title = fitted.title;
  if (over > 0 && title !== undefined && ids.includes("title")) {
    const cut = Math.min(over, Math.max(0, title - (minimums.title ?? 0)));
    fitted.title = title - cut;
    over -= cut;
  }
  if (over <= 0) return fitted;
  const room = ids
    .filter((id) => id !== "select" && id !== "title")
    .map((id) => [id, Math.max(0, (fitted[id] ?? 0) - (minimums[id] ?? 0))] as const);
  const total = room.reduce((sum, [, slack]) => sum + slack, 0);
  if (total === 0) return fitted;
  const given = Math.min(over, total);
  for (const [id, slack] of room) fitted[id] = (fitted[id] ?? 0) - (given * slack) / total;
  return fitted;
}

/** What this table holds in the address, in the shape every table holds it
 *  (`lib/page-address.ts`), and the narrowing a link names beside it; `readUrl` and `writeUrl`
 *  below are this table's own spelling of it. */
export interface View extends TableAddressView {
  /** The issues the reader picked elsewhere, which the table shows alone; `null` is every row.
   *  It is the address's own narrowing (`readSelection`), not one of the table's controls, so
   *  it takes no `stateKey` prefix: one page shows one selection however many tables it draws. */
  selection: IssueSelection | null;
}
export const defaultView: View = {
  sorting: [{ id: "queue", desc: false }],
  grouping: [],
  filters: [{ id: "status", value: [...openIssueStatuses] }],
  search: "",
  page: 0,
  pageSize: 50,
  selection: null,
};
const hasLabel = (issue: Pick<IssueSummary, "labels">, label: string) =>
  (issue.labels ?? []).some((value) => value.toLowerCase() === label);
/** A Todo issue labelled `legion`: the set Start next moves, and the one the Queue column numbers
 *  in a project with no Legion daemon. The server matches a label on `lower(label)`, so the
 *  table does too: an issue labelled `Legion` is one. */
export const isQueued = (issue: Pick<IssueSummary, "status" | "labels">) =>
  issue.status === "todo" && hasLabel(issue, "legion");

/** Every card of the board by its issue, with the column it is in. A card whose issue is not in
 *  the listing names no row. */
function cardsByKey(board: LegionBoardResponse | undefined): Map<string, BoardCardRow> {
  const cards = new Map<string, BoardCardRow>();
  for (const column of board?.columns ?? [])
    for (const card of column.cards ?? [])
      cards.set(card.key, { card, column: column.id, label: column.label });
  return cards;
}

const NO_CARDS: ReadonlyMap<string, BoardCardRow> = new Map();

/**
 * A row's facts at `nowMs` and how far their read is. The board's `issues` map holds the facts of
 * every open issue with Legion phase work and of every issue the board shows, so once the board
 * has answered an open row it does not hold has no Legion work. A closed row it does not hold
 * takes its facts from the items read (`legionFactsOf`), and has none once that read answered
 * without it.
 */
function factsOf(
  issue: Pick<IssueSummary, "key" | "status">,
  legion: LegionReads | undefined,
  nowMs: number
): Pick<Item, "facts" | "factsRead"> {
  if (legion === undefined) return { factsRead: "success" };
  const { board, boardStatus, items, itemsStatus } = legion;
  if (board === undefined) return { factsRead: boardStatus === "error" ? "error" : "pending" };
  const facts = legionFactsOf(issue.key, board, items, nowMs);
  if (facts !== undefined) return { facts, factsRead: "success" };
  return { factsRead: issue.status === "done" ? itemsStatus : "success" };
}

/**
 * The table's rows: each issue with the names the table reads it by and, in a project with a
 * Legion daemon (`context.legion`), its card, its place in the Legion queue and its facts. Every
 * row is of one clock (`Item.at`): `context.nowMs`, else the moment of the build. The rows keep
 * the order of `issues`, which is the listing's own (status, rank, created, key), and every sort
 * ends with it, since TanStack's last tie-break is a row's index; a caller that joins the open
 * listing and the done one lists the open rows first, as the listing's order does.
 */
export function buildItems(
  issues: readonly IssueSummary[],
  label: (actor: Actor) => string,
  isFree: (issue: IssueSummary) => boolean = (issue) => issue.claim === null,
  { legion, nowMs = Date.now(), viewer }: ItemContext = {}
): Item[] {
  // Rank's alphabet is byte-ordered, not locale-ordered. Two issues of one rank break the tie
  // by `compareCreated`, not `compareTimestamps`: a server of the release before the one that
  // added `created_at` sends none, and that absent time would throw there, which is what
  // `IssuesTable.test.tsx`'s "the Legion queue puts an issue with no created_at after one of
  // the same rank that has one" shows.
  const queue = issues
    .filter((issue) => isQueued(issue))
    .sort((a, b) =>
      a.rank < b.rank
        ? -1
        : a.rank > b.rank
          ? 1
          : compareCreated(a, b) || a.key.localeCompare(b.key)
    );
  const positions = new Map(queue.map((issue, index) => [issue.key, index + 1]));
  const cards = legion === undefined ? NO_CARDS : cardsByKey(legion.board);
  return issues.map((issue) => {
    const author = issue.created_by ? label(issue.created_by) : "Unknown";
    const claimedBy = issue.claim ? label(issue.claim.actor) : NOBODY_FACET;
    // The daemon gives cards to open issues, so a done issue has none.
    const card = issue.status === "done" ? undefined : cards.get(issue.key);
    const place =
      legion === undefined ? positions.get(issue.key) : (card?.card.waiting_place ?? undefined);
    return {
      issue,
      queue: positions.get(issue.key),
      author,
      claimedBy,
      claimFree: isFree(issue),
      search: [
        issue.key,
        issue.title,
        issue.assignee ?? "",
        author,
        claimedBy,
        ...(issue.labels ?? []),
      ].join(" "),
      ...(card === undefined ? {} : { card }),
      ...factsOf(issue, legion, nowMs),
      at: nowMs,
      ...(place === undefined ? {} : { place }),
      queued: place !== undefined || card?.column === "queue",
      mine: createdByReader(createdBy(issue), viewer),
    };
  });
}

function withinOneEdit(a: string, b: string): boolean {
  if (Math.abs(a.length - b.length) > 1) return false;
  let i = 0,
    j = 0,
    edits = 0;
  while (i < a.length && j < b.length) {
    if (a[i] === b[j]) {
      i++;
      j++;
      continue;
    }
    if (++edits > 1) return false;
    if (a.length > b.length) i++;
    else if (b.length > a.length) j++;
    else {
      i++;
      j++;
    }
  }
  return edits + a.length - i + b.length - j <= 1;
}
export function matchesSearch(item: Item, query: string): boolean {
  // Nothing typed matches everything, and the facet counts ask on every render, so this
  // answers before any work is done.
  if (query.trim() === "") return true;
  let words: string[] | undefined;
  const searchWords = () => {
    if (words === undefined) words = item.search.toLowerCase().split(/[^\p{L}\p{N}]+/u);
    return words;
  };
  return query
    .trim()
    .toLowerCase()
    .split(/\s+/)
    .every(
      (word) =>
        rankItem(item.search, word, { threshold: rankings.CONTAINS }).passed ||
        (word.length >= 5 && searchWords().some((candidate) => withinOneEdit(candidate, word)))
    );
}
/** The values `item` files under in column `id`'s filter, a length of time counted to the row's
 *  clock (`Item.at`). */
export function facets(item: Item, id: string): readonly string[] {
  const filter = columnFilterSpec(id);
  return filter?.kind === "picks" ? filter.values(item) : [];
}
/** Whether `item` passes every filter but `except`'s, a window counted to `nowMs` and a length
 *  of time to the row's clock (`Item.at`). */
export function matchesFilters(
  item: Item,
  filters: ColumnFiltersState,
  except?: string,
  nowMs = Date.now()
): boolean {
  return filters.every(({ id, value }) => {
    if (id === except) return true;
    const filter = columnFilterSpec(id);
    if (filter?.kind === "text")
      return filter.value(item).toLowerCase().includes(String(value).toLowerCase());
    if (filter?.kind === "range")
      return inWindow(filter.value(item), readWindow(String(value ?? ""), nowMs));
    if (!Array.isArray(value) || value.length === 0) return true;
    // A pick matches a value by its `fold`, as the filter ticks it (`MultiSelect`): a link that
    // spells a label or a name in another case picks the same rows.
    const folded = facets(item, id).map(fold);
    const carries = (picked: string) => folded.includes(fold(String(picked)));
    return filter?.kind === "picks" && filter.match === "all"
      ? value.every(carries)
      : value.some(carries);
  });
}
/** The headings that stand for "none of them", wherever a column has one: they read last, after
 *  every named group. */
const EMPTY_GROUPS: readonly string[] = [NOBODY_FACET, "Unassigned"];
/** Which of two of column `id`'s groups comes first: the order its filter lists its values in,
 *  the drawn column's (`listed`, the board's phases for Legion phase) where it is given, else the
 *  registry's, and by name where there is none — numbers inside a name counted as numbers, so
 *  `CORE-2` reads before `CORE-10`, and the empty group last. A group's place is its column's,
 *  not the row sort's, so the headings read P0, P1, P2, P3 however the rows are sorted. */
export function compareGroups(
  id: string,
  a: string,
  b: string,
  listed?: readonly string[]
): number {
  const filter = columnFilterSpec(id);
  if (filter?.kind !== "picks" || filter.group === undefined) return 0;
  const order = listed ?? filter.order;
  if (order === undefined) {
    const empty = Number(EMPTY_GROUPS.includes(a)) - Number(EMPTY_GROUPS.includes(b));
    return empty !== 0 ? empty : a.localeCompare(b, undefined, { numeric: true });
  }
  const place = (value: string) => {
    const index = order.indexOf(value);
    return index === -1 ? order.length : index;
  };
  return place(a) - place(b);
}
/** The heading `item` groups under in column `id`, which is what `compareGroups` orders. */
export function groupValue(id: string, item: Item): string {
  const filter = columnFilterSpec(id);
  return filter?.kind === "picks" && filter.group !== undefined ? filter.group(item) : "";
}
/** How many of `items` each value of column `id` would list, under the search and every other
 *  filter, a window counted to `nowMs` and a length of time to each row's clock (`Item.at`). */
export function facetCounts(
  items: readonly Item[],
  id: string,
  filters: ColumnFiltersState,
  search: string,
  nowMs = Date.now()
): Map<string, number> {
  const counts = new Map<string, number>();
  for (const item of items)
    if (matchesFilters(item, filters, id, nowMs) && matchesSearch(item, search)) {
      for (const value of new Set(facets(item, id)))
        counts.set(value, (counts.get(value) ?? 0) + 1);
    }
  return counts;
}
const paramFor = (id: string) => {
  const filter = columnFilterSpec(id);
  return filter?.kind === "picks" && filter.param !== undefined ? filter.param : `f.${id}`;
};
/** The column a filter parameter in a URL belongs to. */
const columnForParam: ReadonlyMap<string, string> = new Map(
  COLUMNS.map((column) => [paramFor(column.id), column.id])
);
const defaultSort = defaultView.sorting.map(({ id, desc }) => `${desc ? "-" : ""}${id}`).join(",");
/** What a narrowing that names itself in no other way is called. */
const DEFAULT_SELECTION_LABEL = "Selected issues";

/** The issues behind a number the reader clicked elsewhere, as the URL holds them: the keys,
 *  and the words naming them. */
export interface IssueSelection {
  readonly keys: ReadonlySet<string>;
  readonly label: string;
}
export function readSelection(params: URLSearchParams): IssueSelection | null {
  const only = params.get("only");
  if (only === null) return null;
  return {
    keys: new Set(only.split(",").filter(Boolean)),
    label: params.get("onlyLabel") ?? DEFAULT_SELECTION_LABEL,
  };
}
/** Narrows the table to these issues. They are mostly finished ones, so the Status filter is
 *  lifted (closed issues load) and the page goes back to the first; other filters stay. */
export function selectIssues(
  params: URLSearchParams,
  keys: readonly string[],
  label: string
): URLSearchParams {
  const next = copyAddress(params);
  next.set("only", keys.join(","));
  next.set("onlyLabel", label);
  next.delete("status");
  next.set("nofilter", "1");
  next.delete("page");
  return next;
}
/** Drops that narrowing, leaving every other control as it is. */
export function clearPicked(params: URLSearchParams): URLSearchParams {
  const next = copyAddress(params);
  next.delete("only");
  next.delete("onlyLabel");
  next.delete("page");
  return next;
}

/** The address key one of this table's controls is written under: the bare name, or that name
 *  under the prefix the table's `stateKey` gives it, so two tables on one page keep a sort, a
 *  search, a filter set and a page each. */
const viewKey = (name: string, stateKey: string | undefined) =>
  stateKey === undefined ? name : `${stateKey}.${name}`;
/** The control `key` names for this table, or `undefined` where the key is another table's: a
 *  prefixed table reads only its own prefix, and a bare one never matches a prefixed key. */
const viewName = (key: string, stateKey: string | undefined): string | undefined =>
  stateKey === undefined
    ? key
    : key.startsWith(`${stateKey}.`)
      ? key.slice(stateKey.length + 1)
      : undefined;

/** A picked value out of the URL as column `filter` keeps it: through its `normalize`, then in
 *  the spelling of the column's own value of that name, matched without regard to case, so
 *  `?status=Done` picks `done` and its filter draws one ticked option. A value the column does
 *  not know, or one its rows name (a label), stays as the URL spells it. */
function readPick(filter: ColumnFilterSpec | undefined, value: string): string {
  if (filter?.kind !== "picks") return value;
  const read = filter.normalize ? filter.normalize(value) : value;
  const folded = fold(read);
  return (filter.always ?? filter.order)?.find((own) => fold(own) === folded) ?? read;
}

export function readUrl(params: URLSearchParams, stateKey?: string): View {
  const key = (name: string) => viewKey(name, stateKey);
  const sorting = (params.get(key("sort")) ?? "")
    .split(",")
    .map((part) => ({ id: part.replace(/^-/, ""), desc: part.startsWith("-") }))
    .filter(({ id }) => columnIds.includes(id));
  const grouping = params.get(key("group")) ?? "";
  const filters: ColumnFiltersState = [...new Set(params.keys())].flatMap((param) => {
    const id = columnForParam.get(viewName(param, stateKey) ?? "") ?? "";
    const filter = columnFilterSpec(id);
    return columnIds.includes(id)
      ? [
          {
            id,
            value:
              filter?.kind === "text" || filter?.kind === "range"
                ? params.get(param)
                : [...new Set(params.getAll(param).map((value) => readPick(filter, value)))],
          },
        ]
      : [];
  });
  // `?unclaimed=1` is the parameter the Board's Unclaimed flag writes and what shared table links
  // carry: it picks the Claimed by filter's "Nobody or lapsed", so both layouts read one URL.
  if (params.get(key("unclaimed")) === "1") {
    const claim = filters.find((filter) => filter.id === "claim");
    if (claim === undefined) filters.push({ id: "claim", value: [UNCLAIMED_FACET] });
    else
      claim.value = [
        ...new Set([...(Array.isArray(claim.value) ? claim.value : []), UNCLAIMED_FACET]),
      ];
  }
  if (!params.has(key("status")) && !params.has(key("nofilter")))
    filters.unshift(...defaultView.filters);
  const page = Number(params.get(key("page")));
  const size = Number(params.get(key("size")));
  return {
    sorting:
      params.has(key("sort")) && params.get(key("sort")) === ""
        ? []
        : sorting.length
          ? sorting
          : defaultView.sorting,
    grouping: canGroupBy(grouping) ? [grouping] : [],
    filters,
    // Read whole: the search box cuts what the reader types (`useTableAddress`'s `setSearch`),
    // and every write below rewrites `q` from this, so a cut here would shorten a longer `?q=`
    // a link carried as soon as the reader sorted or paged.
    search: params.get(key("q")) ?? "",
    page: Number.isSafeInteger(page) && page > 0 ? page - 1 : 0,
    pageSize: PAGE_SIZES.includes(size) ? size : defaultView.pageSize,
    selection: readSelection(params),
  };
}
/** Whether a status pick is exactly the set the table opens with: no narrowing of the reader's,
 *  which a link leaves out and **Clear filters** goes back to. */
export function isDefaultStatus(value: unknown): boolean {
  const picked = Array.isArray(value) ? value.map(String) : [];
  const fallback = (defaultView.filters[0]?.value ?? []) as string[];
  return sameSet(fallback, picked);
}
/** Whether `view` narrows the rows by nothing the reader set: no filter but the statuses the
 *  table opens with (or none, `nofilter=1`), no search and no issues picked (`only=`). */
export function narrowsNothing(view: View): boolean {
  return (
    view.search.trim() === "" &&
    view.selection === null &&
    view.filters.every(({ id, value }) => id === "status" && isDefaultStatus(value))
  );
}
export function writeUrl(params: URLSearchParams, state: View, stateKey?: string): URLSearchParams {
  const key = (name: string) => viewKey(name, stateKey);
  const next = copyAddress(params);
  for (const param of [...next.keys()]) {
    const name = viewName(param, stateKey);
    if (
      name !== undefined &&
      (name.startsWith("f.") ||
        columnForParam.has(name) ||
        ["sort", "group", "q", "page", "size", "nofilter", "unclaimed"].includes(name))
    )
      next.delete(param);
  }
  // A control left at its default says nothing, so a shared link carries only what was changed.
  const sort = state.sorting.map(({ id, desc }) => `${desc ? "-" : ""}${id}`).join(",");
  if (sort !== defaultSort) next.set(key("sort"), sort);
  if (state.grouping[0]) next.set(key("group"), state.grouping[0]);
  if (!state.filters.some(({ id }) => id === "status")) next.set(key("nofilter"), "1");
  for (const filter of state.filters) {
    // The open statuses are what the table shows when nothing says otherwise, so a link that
    // carries them says nothing; `readUrl` puts them back, and `nofilter=1` is how an empty
    // selection stays expressible.
    if (filter.id === "status" && isDefaultStatus(filter.value)) continue;
    const values = Array.isArray(filter.value) ? filter.value : [filter.value];
    for (const value of values) {
      if (filter.id === "claim" && value === UNCLAIMED_FACET) next.set(key("unclaimed"), "1");
      else next.append(key(paramFor(filter.id)), String(value));
    }
  }
  if (state.search) next.set(key("q"), state.search);
  if (state.page) next.set(key("page"), String(state.page + 1));
  if (state.pageSize !== defaultView.pageSize) next.set(key("size"), String(state.pageSize));
  // The narrowing belongs to the address rather than to one table's view, so it is written
  // bare under every `stateKey`, and `clearPicked` is what takes it off.
  next.delete("only");
  next.delete("onlyLabel");
  if (state.selection !== null) {
    next.set("only", [...state.selection.keys].join(","));
    if (state.selection.label !== DEFAULT_SELECTION_LABEL)
      next.set("onlyLabel", state.selection.label);
  }
  return next;
}

/** The selected issues in Start next's set, the Todo issues labelled `legion`, in the order the
 *  set holds them. The selection is a `Set`, so one pass over the rows reads it: `Select all N
 *  matching` hands this every issue of the project. */
export function queuedInOrder(items: readonly Item[], keys: readonly string[]): string[] {
  const selected = new Set(keys);
  return items
    .filter(
      (item): item is Item & { queue: number } =>
        item.queue !== undefined && selected.has(item.issue.key)
    )
    .sort((a, b) => a.queue - b.queue)
    .map((item) => item.issue.key);
}

/**
 * The rank writes that move `keys` ahead of the other issues of Start next's set, each one
 * placing its issue before the one the previous write placed. Rows already ahead of everything
 * unselected are left alone, so a finished action writes nothing, and a resumed one writes only
 * what is left: the same function serves the first attempt and the one that picks it up again.
 *
 * One pass, with no search of the queue per key. A write puts its issue straight before the one
 * the previous step placed, so every issue a write moved stands just ahead of the last issue
 * left in its place. A key behind that issue in the queue is behind them all and moves; a key
 * ahead of it stays, and it is then the issue the next key is compared against.
 */
export function startNextWrites(
  items: readonly Item[],
  keys: readonly string[]
): { key: string; input: UpdateIssueInput }[] {
  const queue = items
    .filter((item): item is Item & { queue: number } => item.queue !== undefined)
    .sort((a, b) => a.queue - b.queue)
    .map((item) => item.issue.key);
  const place = new Map(queue.map((key, index) => [key, index]));
  const selected = [...new Set(keys)].flatMap((key) => {
    const at = place.get(key);
    return at === undefined ? [] : [{ key, at }];
  });
  const chosen = new Set(selected.map(({ key }) => key));
  let before = queue.find((key) => !chosen.has(key));
  // The place of the last issue left where it stands, which `before` is or stands just ahead of.
  let still = before === undefined ? undefined : place.get(before);
  const operations: { key: string; input: UpdateIssueInput }[] = [];
  for (const { key, at } of selected.reverse()) {
    if (before !== undefined && still !== undefined && at > still)
      operations.push({ key, input: { rank: { before } } });
    else still = at;
    before = key;
  }
  return operations;
}
