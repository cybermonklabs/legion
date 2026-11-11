import { rankItem, rankings } from "@tanstack/match-sorter-utils";
import type { ColumnFiltersState, GroupingState, SortingState } from "@tanstack/react-table";
import type {
  Actor,
  IssueSummary,
  LegionBoardCard,
  LegionBoardResponse,
  UpdateIssueInput,
} from "../../api/types";
import { capSearchQuery } from "../../lib/query-param";
import { compareTimestamps } from "../../lib/timestamps";
import { DAY_MS } from "../delivery/lib/window";
import { type IssueStatus, issueStatuses, openIssueStatuses, statusText } from "./board-model";
import { LIFECYCLE_ORDER, stageOf } from "./issues-table-lifecycle";
import { expandView, viewOf } from "./issues-table-view-spec";
import { PHASE_ORDER } from "./PhaseHoursBar";

export { LIFECYCLE_ORDER, stageOf, statusStage } from "./issues-table-lifecycle";

export type LegionIssueFacts = NonNullable<LegionBoardResponse["issues"]>[string];

export interface Item {
  readonly issue: IssueSummary;
  /** The issue's place on the Legion board, for a project a daemon serves: the card, its
   *  column's id and label, and the column's place on the board. Absent for an issue the board
   *  does not hold, and for every issue of a project with no daemon. */
  readonly legion?: {
    readonly card: LegionBoardCard;
    readonly id: string;
    readonly label: string;
    readonly order: number;
  };
  /** The issue's facts from the board's one per-issue computation. */
  readonly facts?: LegionIssueFacts;
  /** The closed timestamp the list response supplies, null while the issue remains open. */
  readonly closedAt: string | null;
  /** Whether the signed-in reader created this issue. */
  readonly mine: boolean;
  readonly queue?: number;
  readonly epic: string;
  readonly epicKey?: string;
  readonly author: string;
  readonly claimedBy: string;
  /** Whether an agent could pick this issue up now: nobody holds it, or the server would hand
   *  its claim to the next agent that asks. The Claimed by filter's "Nobody or lapsed". */
  readonly claimFree: boolean;
  readonly search: string;
}
export const AGES = ["Today", "This week", "This month", "Older"] as const;
export const ALL_ROWS = Number.MAX_SAFE_INTEGER;
/** The page sizes the pager offers. 10 is here for a grid that is one section of a page, where
 *  a page of rows stands in the page's own flow rather than in a box of its own. */
export const PAGE_SIZES = [10, 25, 50, 100, ALL_ROWS];
/** The statuses an issue is being worked in, as the status bar counts them. */
export const runningIssueStatuses: readonly IssueStatus[] = [
  "in_progress",
  "testing",
  "needs_review",
  "retro",
];
/** The Claimed by value that picks the issues nobody is working: no claim at all, or a claim
 *  the server would hand over (`claimIsTakeable`). The `?unclaimed=1` link sets it. */
export const UNCLAIMED_FACET = "Nobody or lapsed";

/** How many of one action's writes are in flight at once, whether it is sending them for the
 *  first time or Finish is retrying what a refusal left behind. A selection is as large as the
 *  project, and "Select all N matching" is two clicks away, so the table sends a bounded
 *  stream rather than every request at once; each write is still independent in the store. */
export const WRITES_IN_FLIGHT = 8;

/** How a column filters, and everything a filter of its kind needs: a column that filters by
 *  picking values cannot exist without the values a row files under, nor one that filters by
 *  typed text without the text it reads. Matching, the URL parameter and the options a filter
 *  always offers live here too, so a column is one entry rather than a branch in three files. */
export type ColumnFilterSpec =
  | {
      readonly kind: "picks";
      /** Every value this row files under here: its filter options and its facet counts. */
      readonly values: (item: Item) => string[];
      /** The heading this row groups under; absent when the column cannot group. */
      readonly group?: (item: Item) => string;
      /** The order the filter lists its values in; by count when absent. */
      readonly order?: readonly string[];
      /** A value as the reader sees it, in an option and in a group's heading. */
      readonly show?: (value: string) => string;
      /** A row must carry every picked value, not just one of them, compared case-insensitively. */
      readonly match?: "all";
      /** The URL parameter the picks travel in; `f.<id>` when absent. */
      readonly param?: string;
      /** Values the filter offers whatever the loaded rows hold. */
      readonly always?: readonly string[];
      /** A value out of the URL as this column keeps it. */
      readonly normalize?: (value: string) => string;
    }
  | { readonly kind: "text"; readonly value: (item: Item) => string }
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
  | "phase"
  | "slot"
  | "roles"
  | "ci"
  | "phase_hours"
  | "closed"
  | "work_hours"
  | "review_rounds"
  | "slot_use"
  | "waited"
  | "waiting_reason"
  | "time_in_phase"
  | "legion"
  | "stage"
  | "epic"
  | "assignee"
  | "author"
  | "labels"
  | "claim"
  | "asks"
  | "created"
  | "updated";

/** One column's identity: everything about it that is not how its cell renders. The column
 *  definitions, `columnIds`, `groupable`, `facets`, the default widths and the minimums are all
 *  derived from this list, so a new column is one entry here plus its cell. */
export interface ColumnSpec {
  readonly id: ColumnId;
  /** Its width before the reader drags it. */
  readonly size: number;
  /** How narrow a drag may take it: label, sort mark, a 44 px filter target and padding. */
  readonly min: number;
  /** How narrow it may go on a phone, where the key and the title are the row. */
  readonly compactMin?: number;
  /** Stays in view while the table scrolls sideways. */
  readonly pinned?: boolean;
  /** Headers sort unless this says otherwise. */
  readonly sortable?: boolean;
  readonly filter?: ColumnFilterSpec;
}

const one = (
  value: (item: Item) => string,
  rest: { order?: readonly string[]; show?: (value: string) => string; groups?: boolean } = {}
): ColumnFilterSpec => ({
  kind: "picks",
  values: (item) => [value(item)],
  ...(rest.groups === false ? {} : { group: value }),
  ...(rest.order === undefined ? {} : { order: rest.order }),
  ...(rest.show === undefined ? {} : { show: rest.show }),
});

export const priorityName = (value: string) => (value === "" ? "No priority" : `P${value}`);
export function ageOf(iso: string): string {
  const days = (Date.now() - new Date(iso).getTime()) / 86_400_000;
  return days < 1 ? "Today" : days < 7 ? "This week" : days < 30 ? "This month" : "Older";
}

/** The value a row with no Legion phase files under, in the Phase, Slot, Roles and CI filters. */
export const NO_PHASE = "No Legion phase";
/** Every state an issue of a Legion project can be in, in the order the work runs: the
 *  Dispatch statuses an issue passes before Legion takes it, the daemon's phases while Legion
 *  works it, and the statuses that close it. One list, used by the bar above the table, the
 *  Phase column's filter and its group headings, so the three never disagree.
 *
 *  `Testing` is one segment for both: the Legion phase and the Dispatch status of the same
 *  name are the same moment of the work. `In progress` and `Needs review` hold no Legion
 *  issue — one that Legion works stands in a phase, and a Legion issue with no card of its own
 *  counts under the phase its status stands for.
 *
 *  `Held`, `Waiting on children` and any other phase a later daemon adds stand beside
 *  `Waiting on a person`: each is an issue that waits rather than one being worked. */
export const LEGION_PHASE_LABELS: Readonly<Record<string, string>> = {
  queue: "Queue",
  design: "Design",
  planning: "Planning",
  implementing: "Implementing",
  testing: "Testing",
  reviewing: "Reviewing",
  accepting: "Accepting",
  merging: "Merging",
  awaiting_person: "Waiting on a person",
  held: "Held",
  waiting_on_children: "Waiting on children",
  other: "Other",
};
/** The Phase column's own order: the phases of the lifecycle, then the rows with no phase. */
export const LEGION_PHASE_ORDER: readonly string[] = [
  ...LIFECYCLE_ORDER.flatMap((entry) =>
    entry.startsWith("phase:") ? [entry.slice("phase:".length)] : []
  ),
  NO_PHASE,
];
export const legionPhaseText = (value: string) => LEGION_PHASE_LABELS[value] ?? value;

/** How long a card has stood in its phase, in words; an unknown entry time is "—", never a time
 *  counted from when Dispatch first heard of the card. */
export function formatPhaseTime(since: string | null, nowMs: number): string {
  if (since === null) return "—";
  const sinceMs = Date.parse(since);
  if (Number.isNaN(sinceMs)) return "—";
  const minutes = Math.floor(Math.max(0, nowMs - sinceMs) / 60_000);
  if (minutes < 60) return `${minutes} m`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours} h ${minutes % 60} m`;
  return `${Math.floor(hours / 24)} d ${hours % 24} h`;
}
/** The Slot column's value: the slot number the issue works in, its tree root's for a child,
 *  as text. A row the daemon's waiting line names with no slot reads "Waiting for a slot"; any
 *  other row with no slot, and every row of a project with no daemon, reads "No slot". */
export function legionSlotValue(item: Item): string {
  const card = item.legion?.card;
  const slot = card?.slot ?? card?.tree_slot;
  if (slot !== undefined && slot !== null) return String(slot);
  return card?.waiting_place != null ? "Waiting for a slot" : "No slot";
}
/** A Legion phase's label as the views write it: the board column's label, except that an issue
 *  in retro reads "Retro". The board shows retro in its Merging column, but Evals and the status
 *  bar call it Retro, so every view uses one word for it. */
export function legionPhaseLabel(label: string, phase: string): string {
  return phase === "retro" ? "Retro" : label;
}
/** A card's slot in words: its own slot, its tree root's for a child (a slot belongs to the
 *  tree's root), its place in the waiting line, or none. */
export function legionSlotText(card: LegionBoardCard): string {
  if (card.slot !== null) return `slot ${card.slot}`;
  if (card.tree_slot !== null) return `slot ${card.tree_slot} · via ${card.tree}`;
  if (card.waiting_place !== null) return `waiting for a slot, #${card.waiting_place} in line`;
  return "no slot";
}
/** A card's roles in a turn in words; none working is "between turns". */
export function legionRolesText(card: LegionBoardCard): string {
  return card.roles.length === 0 ? "between turns" : `${card.roles.join(", ")} in a turn`;
}
/** Each board card's Legion phase as the table draws it. One reading of the board for the
 *  table, the project board's cards and Evals. */
export function legionPhases(
  board: LegionBoardResponse | null | undefined
): Map<string, NonNullable<Item["legion"]>> {
  const legion = new Map<string, NonNullable<Item["legion"]>>();
  board?.columns.forEach((column, order) => {
    for (const card of column.cards) {
      legion.set(card.key, { card, id: column.id, label: column.label, order });
    }
  });
  return legion;
}

export function slotUseText(value: string): string {
  switch (value) {
    case "working":
      return "Working";
    case "between_turns":
      return "Between turns";
    case "held_children":
      return "Held: children";
    case "held_person":
      return "Held: person";
    case "held_idle":
      return "Held: idle";
    default:
      return value;
  }
}

/** The one CI vocabulary the Issues table, Board and Evals use. */
export function ciText(value: string | null | undefined): string {
  switch (value) {
    case "passing":
    case "passed":
      return "Passing";
    case "failing":
    case "failed":
      return "Failing";
    case "running":
      return "Running";
    default:
      return "—";
  }
}

export function waitingReasonText(value: string): string {
  switch (value) {
    case "ready":
      return "Ready";
    case "slot":
      return "For a slot";
    case "blocked":
      return "Blocked";
    case "person":
      return "On a person";
    default:
      return value;
  }
}

export function stageText(value: string): string {
  const [kind, id] = value.split(":");
  return kind === "status" ? statusText(id ?? value) : legionPhaseText(id ?? value);
}

const SLOT_USE_ORDER = [
  "working",
  "between_turns",
  "held_children",
  "held_person",
  "held_idle",
] as const;
const WAITING_REASON_ORDER = ["ready", "slot", "blocked", "person"] as const;

export const COLUMNS: readonly ColumnSpec[] = [
  { id: "select", size: 44, min: 44, pinned: true, sortable: false },
  {
    id: "priority",
    size: 100,
    min: 100,
    pinned: true,
    filter: one((item) => String(item.issue.priority ?? ""), {
      order: ["0", "1", "2", "3", ""],
      show: priorityName,
    }),
  },
  {
    id: "queue",
    size: 80,
    min: 80,
    filter: one((item) => (item.queue === undefined ? "Not queued" : "Queued"), {
      order: ["Queued", "Not queued"],
      groups: false,
    }),
  },
  {
    id: "key",
    size: 104,
    min: 108,
    compactMin: 88,
    pinned: true,
    filter: { kind: "text", value: (item) => item.issue.key },
  },
  {
    id: "title",
    size: 280,
    min: 116,
    compactMin: 120,
    pinned: true,
    filter: { kind: "text", value: (item) => item.issue.title },
  },
  {
    id: "status",
    size: 112,
    min: 112,
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
    id: "phase",
    size: 160,
    min: 132,
    filter: {
      kind: "picks",
      values: (item) => [item.legion?.id ?? NO_PHASE],
      group: (item) => item.legion?.id ?? NO_PHASE,
      order: LEGION_PHASE_ORDER,
      show: legionPhaseText,
      param: "phase",
    },
  },
  {
    id: "slot",
    size: 80,
    min: 80,
    filter: one((item) => legionSlotValue(item), { groups: false }),
  },
  {
    id: "roles",
    size: 200,
    min: 196,
    filter: {
      kind: "picks",
      values: (item) =>
        item.legion?.card.roles.length
          ? [...item.legion.card.roles]
          : [item.legion ? "between turns" : NO_PHASE],
    },
  },
  {
    id: "ci",
    size: 100,
    min: 100,
    filter: one((item) => ciText(item.legion?.card.checks_verdict), { groups: false }),
  },
  {
    id: "phase_hours",
    size: 260,
    min: 160,
    filter: {
      kind: "picks",
      values: (item) =>
        Object.entries(item.facts?.phase_hours ?? {})
          .filter(([, hours]) => hours > 0)
          .map(([phase]) => phase),
      order: PHASE_ORDER.map((phase) => phase.id),
    },
  },
  {
    id: "closed",
    size: 110,
    min: 110,
    filter: { kind: "range", value: (item) => item.closedAt },
  },
  { id: "work_hours", size: 100, min: 100 },
  { id: "review_rounds", size: 80, min: 80 },
  {
    id: "slot_use",
    size: 140,
    min: 140,
    filter: {
      kind: "picks",
      values: (item) => (item.legion?.card.slot_use ? [item.legion.card.slot_use] : []),
      order: SLOT_USE_ORDER,
      show: slotUseText,
    },
  },
  { id: "waited", size: 100, min: 100 },
  {
    id: "waiting_reason",
    size: 140,
    min: 140,
    filter: {
      kind: "picks",
      values: (item) => (item.legion?.card.waiting ? [item.legion.card.waiting.reason] : []),
      order: WAITING_REASON_ORDER,
      show: waitingReasonText,
    },
  },
  { id: "time_in_phase", size: 112, min: 112 },
  {
    id: "legion",
    size: 140,
    min: 140,
    filter: {
      kind: "picks",
      values: (item) =>
        item.facts ? ["worked", item.facts.root ? "roots" : "children"] : ["none"],
      order: ["worked", "roots", "children", "none"],
    },
  },
  {
    id: "stage",
    size: 160,
    min: 160,
    filter: one(stageOf, {
      order: LIFECYCLE_ORDER,
      show: stageText,
    }),
  },
  { id: "epic", size: 220, min: 116, filter: one((item) => item.epic) },
  {
    id: "assignee",
    size: 140,
    min: 140,
    filter: one((item) => item.issue.assignee ?? "Unassigned"),
  },
  {
    id: "author",
    size: 150,
    min: 150,
    filter: {
      kind: "picks",
      values: (item) => (item.mine ? [item.author, "@me"] : [item.author]),
      group: (item) => item.author,
    },
  },
  {
    id: "labels",
    size: 200,
    min: 132,
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
    id: "claim",
    size: 220,
    min: 164,
    filter: {
      kind: "picks",
      values: (item) => (item.claimFree ? [item.claimedBy, UNCLAIMED_FACET] : [item.claimedBy]),
      group: (item) => item.claimedBy,
    },
  },
  {
    id: "asks",
    size: 96,
    min: 148,
    filter: one((item) => String(item.issue.open_asks), { groups: false }),
  },
  {
    id: "created",
    size: 104,
    min: 104,
    filter: one((item) => ageOf(item.issue.created_at), { order: AGES }),
  },
  {
    id: "updated",
    size: 120,
    min: 148,
    filter: one((item) => ageOf(item.issue.updated_at), { order: AGES }),
  },
];
const columnSpec: ReadonlyMap<string, ColumnSpec> = new Map(
  COLUMNS.map((column) => [column.id, column])
);
export const columnFilterSpec = (id: string): ColumnFilterSpec | undefined =>
  columnSpec.get(id)?.filter;
/** The columns a URL may name in `sort=` or a filter parameter: every one a reader controls. */
export const columnIds: readonly string[] = COLUMNS.filter(
  (column) => column.sortable !== false || column.filter !== undefined
).map((column) => column.id);
const groupable: readonly string[] = COLUMNS.filter(
  (column) => column.filter?.kind === "picks" && column.filter.group !== undefined
).map((column) => column.id);
export const canGroupBy = (id: string) => groupable.includes(id);
export const COLUMN_SIZES: Readonly<Record<string, number>> = Object.fromEntries(
  COLUMNS.map((column) => [column.id, column.size])
);
export const COLUMN_MIN: Readonly<Record<string, number>> = Object.fromEntries(
  COLUMNS.map((column) => [column.id, column.min])
);
export const COMPACT_COLUMN_MIN: Readonly<Record<string, number>> = Object.fromEntries(
  COLUMNS.flatMap((column) =>
    column.compactMin === undefined ? [] : [[column.id, column.compactMin]]
  )
);
/**
 * The width the Key column needs to write a key of `chars` characters in full, at any box
 * width: the glyphs at the cell's 12px monospace type, the cell's padding, and the room the
 * unread dot takes beside them. A key cut to "INFRA-255…" names no issue, so this is a floor
 * the phone layout keeps as the wide one does.
 */
export function keyColumnWidth(chars: number): number {
  return Math.min(176, Math.ceil(chars * 7.4) + 34);
}

export const PINNED: readonly string[] = COLUMNS.filter((column) => column.pinned).map(
  (column) => column.id
);
/** How much of the table's box the pinned columns may cover: on a phone (below 640 px) the key
 *  and title are what identify a row, so they take most of it. */
export const PINNED_SHARE = 0.6;
export const PHONE_PINNED_SHARE = 0.85;
/** What a table's box takes for its own border, left and right: the width its columns may use
 *  is what `getBoundingClientRect` measures less this. */
export const BOX_BORDERS = 2;
/** Below 640 px, in a table that names its primary columns: how much of the box the Key column
 *  takes, with the title drawn under the key inside it. A little over half, so a title reads in
 *  two lines while the columns beside it stay wide enough to say something. */
export const PHONE_KEY_SHARE = 0.55;
/** The narrowest a primary column is drawn there. Below this a phase name, a wait or a bar says
 *  nothing, so the Key column gives way instead. */
export const PHONE_PRIMARY_MIN = 96;

export interface View {
  sorting: SortingState;
  grouping: GroupingState;
  filters: ColumnFiltersState;
  search: string;
  page: number;
  pageSize: number;
}
export const defaultView: View = {
  sorting: [{ id: "queue", desc: false }],
  grouping: [],
  filters: [{ id: "status", value: [...openIssueStatuses] }],
  search: "",
  page: 0,
  pageSize: 50,
};
const hasLabel = (issue: Pick<IssueSummary, "labels">, label: string) =>
  (issue.labels ?? []).some((value) => value.toLowerCase() === label);
/** The server matches a label on `lower(label)`, so the table does too: an issue labelled
 *  `Legion` is in Legion's queue, and one labelled `Epic` is an epic. */
export const isQueued = (issue: Pick<IssueSummary, "status" | "labels">) =>
  issue.status === "todo" && hasLabel(issue, "legion");
export const isEpic = (issue: Pick<IssueSummary, "parent" | "labels" | "status">) =>
  issue.parent === null && hasLabel(issue, "epic") && issue.status !== "done";
export type EpicAncestor = Pick<IssueSummary, "key" | "parent" | "status" | "labels" | "title">;
export function buildItems(
  issues: readonly IssueSummary[],
  label: (actor: Actor) => string,
  ancestors: readonly EpicAncestor[] = [],
  isFree: (issue: IssueSummary) => boolean = (issue) => issue.claim === null,
  // undefined: no Legion setting; null: configured, and its board has not answered yet.
  board?: LegionBoardResponse | null,
  viewer?: string
): Item[] {
  const byKey = new Map<string, EpicAncestor>(
    [...ancestors, ...issues].map((issue) => [issue.key, issue])
  );
  const legion = legionPhases(board);
  const positions = new Map<string, number>();
  if (board === undefined) {
    // Rank's alphabet is byte-ordered, not locale-ordered.
    issues
      .filter((issue) => !isEpic(issue) && isQueued(issue))
      .sort((a, b) =>
        a.rank < b.rank
          ? -1
          : a.rank > b.rank
            ? 1
            : compareTimestamps(a.created_at, b.created_at) || a.key.localeCompare(b.key)
      )
      .forEach((issue, index) => {
        positions.set(issue.key, index + 1);
      });
  } else {
    // The daemon's waiting places are the only queue order for a configured project; a card
    // may retain its phase while it waits, so read every column rather than Queue alone.
    board?.columns.forEach((column) => {
      column.cards.forEach((card) => {
        if (card.waiting_place !== null) positions.set(card.key, card.waiting_place);
      });
    });
  }
  return issues.map((issue) => {
    let root: EpicAncestor | undefined = issue;
    const seen = new Set<string>();
    while (root?.parent && !seen.has(root.key)) {
      seen.add(root.key);
      root = byKey.get(root.parent);
    }
    const epic = root && isEpic(root) ? root : undefined;
    const author = issue.created_by ? label(issue.created_by) : "Unknown";
    const claimedBy = issue.claim ? label(issue.claim.actor) : "Nobody";
    const facts = board?.issues?.[issue.key];
    const epicName = epic
      ? `${epic.key} · ${epic.title}`
      : root === undefined || root.parent !== null
        ? "Unknown epic"
        : "No epic";
    return {
      issue,
      legion: legion.get(issue.key),
      facts,
      closedAt: issue.closed_at,
      mine: viewer !== undefined && issue.created_by?.id === viewer,
      queue: positions.get(issue.key),
      epic: epicName,
      epicKey: epic?.key,
      author,
      claimedBy,
      claimFree: isFree(issue),
      search: [
        issue.key,
        issue.title,
        epicName,
        issue.assignee ?? "",
        author,
        claimedBy,
        ...(issue.labels ?? []),
      ].join(" "),
    };
  });
}

/** Move the first open issue on each path out of the source. Its open or closed descendants
 *  travel with it; only a path made entirely of closed intermediates needs another parent write. */
export function mergeMembers(issues: readonly IssueSummary[], source: string): string[] {
  const byKey = new Map(issues.map((issue) => [issue.key, issue]));
  return issues
    .filter((issue) => {
      if (issue.status === "done" || issue.key === source) return false;
      let parent = issue.parent;
      const seen = new Set<string>();
      while (parent !== null && parent !== source && !seen.has(parent)) {
        seen.add(parent);
        const intermediate = byKey.get(parent);
        if (!intermediate || intermediate.status !== "done") return false;
        parent = intermediate.parent;
      }
      return parent === source;
    })
    .map((issue) => issue.key);
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
  // Nothing typed matches everything, and the two hot callers — the epic fold-in and the
  // facet counts — ask on every render, so this answers before any work is done.
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
export function facets(item: Item, id: string): string[] {
  const filter = columnFilterSpec(id);
  return filter?.kind === "picks" ? filter.values(item) : [];
}

export const LEGION_FILTER_IDS: readonly string[] = [
  "legion",
  "slot_use",
  "waiting_reason",
  "phase_hours",
  "phase",
  "slot",
  "roles",
  "ci",
];

export function isLegionFilter(filter: ColumnFiltersState[number]): boolean {
  return (
    LEGION_FILTER_IDS.includes(filter.id) ||
    (filter.id === "stage" &&
      Array.isArray(filter.value) &&
      filter.value.some((value) => String(value).startsWith("phase:")))
  );
}

/** Legion-filtered views wait for their board snapshot instead of guessing from listing rows. */
export function effectiveFilters(
  filters: ColumnFiltersState,
  _boardReady: boolean
): ColumnFiltersState {
  return filters;
}

/** Resolves the closed-issue window a URL filter names, or rejects malformed input. */
export function closedWindow(
  value: string,
  nowMs: number
): { since?: string; before?: string } | null {
  if (value === "all") return {};
  const range = /^(\d{4}-\d{2}-\d{2})\.\.(\d{4}-\d{2}-\d{2})$/.exec(value);
  if (range) {
    const [, from, to] = range;
    const since = `${from}T00:00:00Z`;
    const before = `${to}T00:00:00Z`;
    if (
      new Date(since).toISOString().slice(0, 10) !== from ||
      new Date(before).toISOString().slice(0, 10) !== to ||
      before <= since
    )
      return null;
    return { since, before };
  }
  const preset = /^([1-9]\d*)d$/.exec(value);
  if (!preset) return null;
  return {
    since: new Date(Math.floor(nowMs / 60_000) * 60_000 - Number(preset[1]) * DAY_MS).toISOString(),
  };
}

export function matchesFilters(
  item: Item,
  filters: ColumnFiltersState,
  except?: string,
  boardReady = true,
  nowMs = Date.now()
): boolean {
  return filters.every((entry) => {
    if (entry.id === except || (!boardReady && isLegionFilter(entry))) return true;
    const { id, value } = entry;
    const filter = columnFilterSpec(id);
    if (filter?.kind === "text")
      return filter.value(item).toLowerCase().includes(String(value).toLowerCase());
    if (filter?.kind === "range") {
      const window = closedWindow(String(value), nowMs);
      const at = filter.value(item);
      return (
        window !== null &&
        at !== null &&
        !Number.isNaN(Date.parse(at)) &&
        (window.since === undefined || at >= window.since) &&
        (window.before === undefined || at < window.before)
      );
    }
    if (!Array.isArray(value) || value.length === 0) return true;
    return filter?.kind === "picks" && filter.match === "all"
      ? value.every((picked: string) =>
          facets(item, id).some((entry) => entry.toLowerCase() === picked.toLowerCase())
        )
      : facets(item, id).some((entry) => value.includes(entry));
  });
}
/** The headings that stand for "none of them", wherever a column has one: they read last, after
 *  every named group. */
const EMPTY_GROUPS: readonly string[] = ["No epic", "Unknown epic", "Nobody", "Unassigned"];
/** Which of two of column `id`'s groups comes first: the order its filter lists its values in,
 *  and by name where the registry gives none — numbers inside a name counted as numbers, so
 *  `CORE-2` reads before `CORE-10`, and the empty group last. A group's place is its column's,
 *  not the row sort's, so the headings read P0, P1, P2, P3 however the rows are sorted. */
export function compareGroups(id: string, a: string, b: string): number {
  const filter = columnFilterSpec(id);
  if (filter?.kind !== "picks" || filter.group === undefined) return 0;
  const order = filter.order;
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
export function facetCounts(
  items: readonly Item[],
  id: string,
  filters: ColumnFiltersState,
  search: string,
  boardReady = true
): Map<string, number> {
  const counts = new Map<string, number>();
  for (const item of items)
    if (matchesFilters(item, filters, id, boardReady) && matchesSearch(item, search)) {
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

function readExpandedUrl(params: URLSearchParams): View {
  const sorting = (params.get("sort") ?? "")
    .split(",")
    .map((part) => ({ id: part.replace(/^-/, ""), desc: part.startsWith("-") }))
    .filter(({ id }) => columnIds.includes(id));
  const grouping = params.get("group") ?? "";
  const filters: ColumnFiltersState = [...new Set(params.keys())].flatMap<
    ColumnFiltersState[number]
  >((key) => {
    const id = columnForParam.get(key) ?? "";
    if (!columnIds.includes(id)) return [];
    const filter = columnFilterSpec(id);
    if (filter?.kind === "text" || filter?.kind === "range")
      return [{ id, value: params.get(key) ?? "" }];
    return [
      {
        id,
        value: params
          .getAll(key)
          .map((value) =>
            filter?.kind === "picks" && filter.normalize ? filter.normalize(value) : value
          ),
      },
    ];
  });
  // `?unclaimed=1` is the Board strip's own parameter and what shared List links carry: it picks
  // the Claimed by filter's "Nobody or lapsed", so both views read one URL.
  if (params.get("unclaimed") === "1") {
    const claim = filters.find((filter) => filter.id === "claim");
    if (claim === undefined) filters.push({ id: "claim", value: [UNCLAIMED_FACET] });
    else
      claim.value = [
        ...new Set([...(Array.isArray(claim.value) ? claim.value : []), UNCLAIMED_FACET]),
      ];
  }
  if (!params.has("status") && !params.has("nofilter")) filters.unshift(...defaultView.filters);
  const page = Number(params.get("page"));
  const size = Number(params.get("size"));
  return {
    sorting:
      params.has("sort") && params.get("sort") === ""
        ? []
        : sorting.length
          ? sorting
          : defaultView.sorting,
    grouping: canGroupBy(grouping) ? [grouping] : [],
    filters,
    search: capSearchQuery(params.get("q") ?? ""),
    page: Number.isSafeInteger(page) && page > 0 ? page - 1 : 0,
    pageSize: PAGE_SIZES.includes(size) ? size : defaultView.pageSize,
  };
}

/** Reads URL controls after a selected built-in view supplies its defaults. */
export function readUrl(params: URLSearchParams, hasLegion = false): View {
  return readExpandedUrl(expandView(params, hasLegion));
}

/** The Legion trees a shared URL removes from its listing. */
export function readExcludedTrees(params: URLSearchParams): ReadonlySet<string> {
  return new Set(params.getAll("exclude_tree").filter((key) => key !== ""));
}

function serializeViewState(state: View): URLSearchParams {
  const serialized = new URLSearchParams();
  serialized.set("sort", state.sorting.map(({ id, desc }) => `${desc ? "-" : ""}${id}`).join(","));
  if (state.grouping[0]) serialized.set("group", state.grouping[0]);
  const hasStatus = state.filters.some(
    (filter) => filter.id === "status" && Array.isArray(filter.value) && filter.value.length > 0
  );
  if (!hasStatus) serialized.set("nofilter", "1");
  for (const filter of state.filters) {
    const values = Array.isArray(filter.value) ? filter.value : [filter.value];
    for (const value of values) {
      if (filter.id === "claim" && value === UNCLAIMED_FACET) serialized.set("unclaimed", "1");
      else serialized.append(paramFor(filter.id), String(value));
    }
  }
  if (state.search) serialized.set("q", state.search);
  if (state.page) serialized.set("page", String(state.page + 1));
  if (state.pageSize !== defaultView.pageSize) serialized.set("size", String(state.pageSize));
  return serialized;
}

function sameValues(left: readonly string[], right: readonly string[]): boolean {
  return left.length === right.length && left.every((value, index) => value === right[index]);
}

/** Writes only the table controls that differ from the selected built-in view. */
export function writeUrl(params: URLSearchParams, state: View, hasLegion = false): URLSearchParams {
  const selected = viewOf(params);
  const view = selected.legionOnly && !hasLegion ? viewOf(new URLSearchParams()) : selected;
  const base = serializeViewState(readExpandedUrl(view.params(hasLegion)));
  const nextState = serializeViewState(state);
  const next = new URLSearchParams(params);
  for (const key of [...next.keys()])
    if (
      key.startsWith("f.") ||
      columnForParam.has(key) ||
      ["sort", "group", "q", "page", "size", "nofilter", "unclaimed"].includes(key)
    )
      next.delete(key);
  if (view.id === "open") next.delete("view");
  else next.set("view", view.id);

  const keys = new Set([...base.keys(), ...nextState.keys()]);
  for (const key of keys) {
    const original = base.getAll(key);
    const changed = nextState.getAll(key);
    if (sameValues(original, changed)) continue;
    if (key === "nofilter") {
      const value = changed[0];
      if (value !== undefined) next.set(key, value);
      continue;
    }
    if (changed.length) {
      for (const value of changed) next.append(key, value);
    } else if (key !== "status" && original.length) {
      next.append(key, "");
    }
  }
  return next;
}

/** Whether serializing this view leaves a control that differs from the selected view. */
export function viewIsChanged(params: URLSearchParams, state: View, hasLegion = false): boolean {
  return [...new Set(writeUrl(params, state, hasLegion).keys())].some(
    (key) => !["view", "layout", "cols", "page", "size"].includes(key)
  );
}

/** The column order a drag leaves behind: `dragged` taken out of `current` and put back
 *  immediately before `before`. A drop on itself, or on an id the order does not hold, leaves
 *  the order as it was. */
export function reorder(current: readonly string[], dragged: string, before: string): string[] {
  if (dragged === before || !current.includes(dragged) || !current.includes(before))
    return [...current];
  const next = current.filter((id) => id !== dragged);
  next.splice(next.indexOf(before), 0, dragged);
  return next;
}

/** One segment of a view's bar. `value` is the filter value and `stage` selects its colour. */
export interface BarCount {
  readonly value: string;
  readonly label: string;
  readonly detail?: string;
  readonly count: number;
  readonly stage: string;
}

const WAITING_REASON_STAGE: Readonly<Record<string, string>> = {
  ready: "phase:design",
  slot: "phase:queue",
  blocked: "phase:held",
  person: "phase:awaiting_person",
};
const SLOT_USE_STAGE: Readonly<Record<string, string>> = {
  working: "phase:implementing",
  between_turns: "phase:queue",
  held_children: "phase:waiting_on_children",
  held_person: "phase:awaiting_person",
  held_idle: "phase:held",
};

/** The Dispatch statuses that read as a Legion phase as well: a segment under one of these
 *  names holds only the issues Legion does not run, and says so where a daemon serves the
 *  project. Every other status — Triage, Todo, Blocked, Done — names no phase, so its segment
 *  needs no telling apart. */
const STATUSES_NAMED_LIKE_A_PHASE: readonly string[] = ["in_progress", "needs_review", "retro"];
/** What a segment of those statuses is called, where Legion serves the project. */
export const NOT_IN_LEGION = "not in Legion";

export const NOT_STARTED_STAGES = [
  "status:triage",
  "status:icebox",
  "status:backlog",
  "status:todo",
  "phase:queue",
] as const;

function stageLabel(stage: string, hasLegion: boolean): string {
  const [kind, id = stage] = stage.split(":");
  if (kind === "status")
    return hasLegion && STATUSES_NAMED_LIKE_A_PHASE.includes(id)
      ? `${statusText(id)} · ${NOT_IN_LEGION}`
      : statusText(id);
  return legionPhaseText(id);
}

/**
 * Counts a view's listed rows in the order and the terms its bar uses. A row with no value for
 * Waiting or Slot use contributes no segment, so those bars leave their empty categories out.
 */
export function countBy(
  rows: readonly Item[],
  column: "stage" | "waiting_reason" | "slot_use",
  hasLegion: boolean,
  options: { readonly collapseNotStarted?: boolean } = {}
): BarCount[] {
  const values =
    column === "stage"
      ? LIFECYCLE_ORDER
      : column === "waiting_reason"
        ? WAITING_REASON_ORDER
        : SLOT_USE_ORDER;
  const counts = new Map<string, number>();
  for (const item of rows) {
    const value =
      column === "stage"
        ? stageOf(item)
        : column === "waiting_reason"
          ? item.legion?.card.waiting?.reason
          : item.legion?.card.slot_use;
    if (value !== undefined && value !== null) counts.set(value, (counts.get(value) ?? 0) + 1);
  }
  const segments = values.flatMap((value) => {
    const count = counts.get(value);
    if (count === undefined) return [];
    const stage =
      column === "stage"
        ? value
        : column === "waiting_reason"
          ? WAITING_REASON_STAGE[value]
          : SLOT_USE_STAGE[value];
    const label =
      column === "stage"
        ? stageLabel(value, hasLegion)
        : column === "waiting_reason"
          ? waitingReasonText(value)
          : slotUseText(value);
    return [{ value, label, count, stage }];
  });
  if (!options.collapseNotStarted || column !== "stage") return segments;

  const count = NOT_STARTED_STAGES.reduce((total, stage) => total + (counts.get(stage) ?? 0), 0);
  if (count === 0) return segments;
  const detail = NOT_STARTED_STAGES.flatMap((stage) => {
    const stageCount = counts.get(stage);
    return stageCount === undefined ? [] : [`${stageLabel(stage, hasLegion)} ${stageCount}`];
  }).join(" · ");
  return [
    ...segments.filter((segment) => !NOT_STARTED_STAGES.includes(segment.value as never)),
    { value: "not_started", label: "Not started", detail, count, stage: "status:triage" },
  ];
}

/** The issues behind a number the reader clicked on the Evals tab, as the URL holds them:
 *  the keys, and the words naming them. */
export interface IssueSelection {
  readonly keys: ReadonlySet<string>;
  readonly label: string;
}
export function readSelection(params: URLSearchParams): IssueSelection | null {
  const only = params.get("only");
  if (only === null) return null;
  return {
    keys: new Set(only.split(",").filter(Boolean)),
    label: params.get("onlyLabel") ?? "Selected issues",
  };
}
/** Narrows the table to these issues. They are mostly finished ones, so the Status filter is
 *  lifted (closed issues load) and the page goes back to the first; other filters stay. */
export function selectIssues(
  params: URLSearchParams,
  keys: readonly string[],
  label: string
): URLSearchParams {
  const next = new URLSearchParams(params);
  next.set("only", keys.join(","));
  next.set("onlyLabel", label);
  next.delete("status");
  next.set("nofilter", "1");
  next.delete("page");
  return next;
}
/** Drops that narrowing, leaving every other control as it is. */
export function clearPicked(params: URLSearchParams): URLSearchParams {
  const next = new URLSearchParams(params);
  next.delete("only");
  next.delete("onlyLabel");
  next.delete("page");
  return next;
}

/** The selected issues that are in Legion's queue, in the order the queue holds them. */
export function queuedInOrder(items: readonly Item[], keys: readonly string[]): string[] {
  return items
    .filter((item): item is Item & { queue: number } => item.queue !== undefined)
    .sort((a, b) => a.queue - b.queue)
    .filter((item) => keys.includes(item.issue.key))
    .map((item) => item.issue.key);
}

/**
 * The rank writes that move `keys` to the head of Legion's queue, each one placing its issue
 * before the one the previous write placed. Rows already ahead of everything unselected are
 * left alone, so a finished action writes nothing, and a resumed one writes only what is left:
 * the same function serves the first attempt and the one that picks it up again.
 */
export function startNextWrites(
  items: readonly Item[],
  keys: readonly string[]
): { key: string; input: UpdateIssueInput }[] {
  const queue = items
    .filter((item) => item.queue !== undefined)
    .sort((a, b) => (a.queue ?? 0) - (b.queue ?? 0))
    .map((item) => item.issue.key);
  const queued = new Set(queue);
  const selected = keys.filter((key) => queued.has(key));
  const selectedKeys = new Set(selected);
  let before = queue.find((key) => !selectedKeys.has(key));
  const operations: { key: string; input: UpdateIssueInput }[] = [];
  for (const key of selected.reverse()) {
    if (before && queue.indexOf(key) > queue.indexOf(before)) {
      operations.push({ key, input: { rank: { before } } });
      queue.splice(queue.indexOf(key), 1);
      queue.splice(queue.indexOf(before), 0, key);
    }
    before = key;
  }
  return operations;
}
