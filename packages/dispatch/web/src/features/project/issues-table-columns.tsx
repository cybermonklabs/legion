import type { ColumnDef, FilterFn, Row, RowData } from "@tanstack/react-table";
import { type ReactNode, useMemo } from "react";
import { Link } from "react-router-dom";
import type { UserState } from "../../api/types";
import { AttentionBadge } from "../../components/Badge";
import { LabelPill, StatusPill } from "../../components/Pill";
import type { LayoutColumns } from "../../components/table-layout";
import { issueProject } from "../../lib/issue-key";
import { compareTimestamps } from "../../lib/timestamps";
import {
  checkboxAccent,
  linkHoverText,
  linkText,
  stageFill,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { ClaimChip } from "../issue/ClaimChip";
import { issuePriority } from "../issue/issue-priority";
import { PriorityControl } from "../issue/PriorityControl";
import { ProgressChips } from "../issue/ProgressChips";
import { UnreachableRouteMarker } from "../issue/RouteReach";
import { formatPhaseTime, formatWorkTime } from "../legion/lib/format";
import { priorityPath } from "../priorities/priority-routes";
import { AgentName } from "../refs/AgentName";
import { ReferenceLink } from "../refs/ReferenceLink";
import { referenceTriggerProps } from "../refs/RefPreview";
import { buildIssuePath } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { type IssueStatus, issueStatuses, statusLabel } from "./board-model";
import { PhaseHoursCell, phaseHours } from "./issues-phase-hours";
import {
  SLOT_USES,
  slotUseLabel,
  stageIds,
  stageLabel,
  WAITING_REASONS,
  waitingLabel,
} from "./issues-stages";
import type { IssueListing } from "./issues-table-data";
import {
  COLUMN_MIN,
  COLUMNS,
  COMPACT_COLUMN_MIN,
  type ColumnId,
  ISSUE_ROW_ATTRIBUTE,
  type Item,
  matchesFilters,
} from "./issues-table-model";
import { issueIsUnread, UnreadDot } from "./UnreadDot";

/** Rows with no value of their own sort after every row with one, whichever way the column is
 *  sorted: an empty Place or Slot is not "first". TanStack reverses a sorting function for a
 *  descending sort, so the empty side is turned with it. */
function emptyLast<T>(
  value: (item: Item) => T | undefined,
  compare: (a: T, b: T) => number
): (a: Row<Item>, b: Row<Item>, id: string) => number {
  return (a, b) => {
    const left = value(a.original);
    const right = value(b.original);
    if (left === undefined && right === undefined) return 0;
    if (left === undefined) return 1;
    if (right === undefined) return -1;
    return compare(left, right);
  };
}

const byNumber = (a: number, b: number) => a - b;

/** How long ago `since` was, in milliseconds; undefined with no time. */
function ageMs(since: string | null | undefined): number | undefined {
  if (since === null || since === undefined) return undefined;
  const at = Date.parse(since);
  return Number.isNaN(at) ? undefined : Date.now() - at;
}

/** Every stage's fill, read by a stage id the caller holds as a string. */
const STAGE_FILLS: Readonly<Record<string, string>> = stageFill;

/** A stage's swatch and its label: the mark the Stage cell, a card and a column head draw. */
export function StageMark({ stage }: { stage: string }): ReactNode {
  return (
    <span className="inline-flex items-center gap-1.5 whitespace-nowrap">
      <span
        aria-hidden
        className={`size-2.5 shrink-0 rounded-sm ${STAGE_FILLS[stage] ?? stageFill.held}`}
      />
      {stageLabel(stage)}
    </span>
  );
}

declare module "@tanstack/react-table" {
  interface ColumnMeta<TData extends RowData, TValue> {
    /** A facet value as the reader sees it. */
    show?: (value: string) => string;
    /** The facet values in the order the filter lists them; by count when absent. */
    order?: readonly string[];
    /** The values the filter offers whatever the loaded rows hold. */
    always?: readonly string[];
    /** Filter by typed text instead of picking values: for a column of free text. */
    text?: boolean;
    /** Filter by a window of time (`RangeFilter`): for a column of moments. */
    range?: boolean;
    /** The types for the declaration's own parameters, which it does not otherwise use. */
    value?: [TData, TValue];
  }
}
const inPicks: FilterFn<Item> = (row, id, value: string[] | string) =>
  matchesFilters(row.original, [{ id, value }]);

/** How each column renders and sorts, for the reader `userState` describes. Keyed by the
 *  registry's own ids, so a typo or a column with no rendering fails to compile. Everything
 *  else about a column — whether it filters, what a row files under, what it groups by, how
 *  wide it starts and how narrow it may go — comes from `COLUMNS` in the model, so the two
 *  never disagree. A row's own checkbox tells `onMark` the row it marks. */
const renderings = (
  userState: UserState | undefined,
  onMark: (key: string) => void
): Record<ColumnId, Partial<ColumnDef<Item>>> => ({
  select: {
    header: ({ table }) => (
      <input
        aria-label="Select every row on this page"
        checked={table.getIsAllPageRowsSelected()}
        className={checkboxAccent}
        onChange={table.getToggleAllPageRowsSelectedHandler()}
        type="checkbox"
      />
    ),
    cell: ({ row }) => (
      <input
        aria-label={`Select ${row.original.issue.key}`}
        checked={row.getIsSelected()}
        className={checkboxAccent}
        onChange={(event) => {
          if (event.target.checked) onMark(row.id);
          row.toggleSelected(event.target.checked);
        }}
        type="checkbox"
      />
    ),
    enableHiding: false,
  },
  priority: {
    header: "Priority",
    accessorFn: (item) => item.issue.priority ?? undefined,
    sortUndefined: "last",
    sortDescFirst: false,
    cell: ({ row }) => (
      <PriorityControl
        disabled={row.original.issue.status === "done"}
        issueKey={row.original.issue.key}
        priority={issuePriority(row.original.issue.priority)}
      />
    ),
  },
  queue: {
    header: "Place",
    accessorFn: (item) => item.place,
    sortDescFirst: false,
    // By Place, then the rows with no Place by rank, which is the order Legion starts them in.
    sortingFn: (a, b) => {
      const left = a.original.place;
      const right = b.original.place;
      if (left !== undefined && right !== undefined) return left - right;
      if (left !== undefined) return -1;
      if (right !== undefined) return 1;
      const rankA = a.original.issue.rank;
      const rankB = b.original.issue.rank;
      return rankA < rankB ? -1 : rankA > rankB ? 1 : 0;
    },
    cell: ({ row }) => {
      const { place, queued } = row.original;
      if (place !== undefined)
        return (
          <span className="font-semibold tabular-nums" title="Legion starts issues in this order">
            #{place}
          </span>
        );
      // In the waiting line with no place of its own: a Queue card the line does not name.
      return queued ? (
        <span className={`text-xs ${textMutedOnCanvas}`}>In waiting line</span>
      ) : null;
    },
  },
  key: {
    header: "Key",
    accessorFn: (item) => item.issue.key,
    sortingFn: (a, b) =>
      Number(a.original.issue.key.split("-")[1]) - Number(b.original.issue.key.split("-")[1]),
    cell: ({ row }) => (
      <span className="flex items-center gap-1 font-semibold whitespace-nowrap">
        <Link
          className={`${linkText} ${linkHoverText}`}
          to={buildIssuePath({ key: row.original.issue.key, kind: "issue" })}
          {...referenceTriggerProps({ key: row.original.issue.key, kind: "issue" })}
        >
          {row.original.issue.key}
        </Link>
        {issueIsUnread(
          row.original.issue,
          userState?.[row.original.issue.key]?.last_read_seq ?? 0
        ) ? (
          <UnreadDot />
        ) : null}
      </span>
    ),
  },
  title: {
    header: "Title",
    accessorFn: (item) => item.issue.title,
    cell: ({ row }) => (
      <div className="min-w-0 space-y-1">
        <Link
          aria-label={`${row.original.issue.key} ${row.original.issue.title}`}
          data-issue-link
          className={`flex w-full min-w-0 ${linkText} ${linkHoverText}`}
          to={buildIssuePath({ key: row.original.issue.key, kind: "issue" })}
          {...referenceTriggerProps({ key: row.original.issue.key, kind: "issue" })}
        >
          <span className={`line-clamp-2 min-w-0 flex-1 break-words ${textPrimaryOnCanvas}`}>
            {row.original.issue.title}
          </span>
        </Link>
        <span className="flex flex-wrap items-center gap-1">
          {row.original.issue.open_asks > 0 ? (
            <AttentionBadge count={row.original.issue.open_asks} />
          ) : null}
          <ProgressChips progress={row.original.issue.progress} />
          <UnreachableRouteMarker issue={row.original.issue} />
          {row.original.issue.parent ? (
            <Link
              className={`text-xs ${linkText} ${linkHoverText}`}
              to={buildIssuePath({ key: row.original.issue.parent, kind: "issue" })}
              {...referenceTriggerProps({ key: row.original.issue.parent, kind: "issue" })}
            >
              {row.original.issue.parent}
            </Link>
          ) : null}
        </span>
      </div>
    ),
  },
  stage: {
    header: "Stage",
    accessorFn: (item) => item.stage,
    sortingFn: (a, b) => stageIds.indexOf(a.original.stage) - stageIds.indexOf(b.original.stage),
    cell: ({ row }) => (
      <span className={`text-xs ${textSecondaryOnCanvas}`} data-stage={row.original.stage}>
        <StageMark stage={row.original.stage} />
      </span>
    ),
  },
  status: {
    header: "Status",
    accessorFn: (item) => item.issue.status,
    sortingFn: (a, b) =>
      issueStatuses.indexOf(a.original.issue.status as IssueStatus) -
      issueStatuses.indexOf(b.original.issue.status as IssueStatus),
    cell: ({ getValue }) => <StatusPill>{statusLabel(getValue<IssueStatus>())}</StatusPill>,
  },
  epic: {
    header: "Epic",
    accessorFn: (item) => item.epic,
    cell: ({ getValue }) => (
      <span className={`line-clamp-2 ${textSecondaryOnCanvas}`}>{getValue<string>()}</span>
    ),
  },
  assignee: {
    header: "Assignee",
    accessorFn: (item) => item.issue.assignee ?? "Unassigned",
    // A narrow column ends a long address with an ellipsis, the whole of it in the title.
    cell: ({ getValue }) => (
      <span className={`block truncate ${textSecondaryOnCanvas}`} title={getValue<string>()}>
        {getValue<string>()}
      </span>
    ),
  },
  author: {
    header: "Created by",
    accessorFn: (item) => item.author,
    cell: ({ row }) =>
      row.original.issue.created_by === null ? (
        <span className={textSecondaryOnCanvas}>Unknown</span>
      ) : (
        <AgentName actor={row.original.issue.created_by} />
      ),
  },
  labels: {
    header: "Labels",
    accessorFn: (item) => (item.issue.labels ?? []).join(", "),
    cell: ({ row }) => (
      <span className="flex flex-wrap gap-1">
        {(row.original.issue.labels ?? []).map((label) => (
          <LabelPill key={label}>{label}</LabelPill>
        ))}
      </span>
    ),
  },
  claim: {
    header: "Claimed by",
    accessorFn: (item) => item.claimedBy,
    cell: ({ row }) => <ClaimChip claim={row.original.issue.claim} />,
  },
  asks: {
    header: "Open asks",
    accessorFn: (item) => item.issue.open_asks,
    sortDescFirst: true,
    cell: ({ getValue }) =>
      getValue<number>() === 0 ? null : <AttentionBadge count={getValue<number>()} />,
  },
  created: {
    header: "Created",
    accessorFn: (item) => item.issue.created_at,
    sortingFn: (a, b) =>
      compareTimestamps(a.original.issue.created_at, b.original.issue.created_at),
    cell: ({ getValue }) => (
      <Timestamp
        at={getValue<string>()}
        className={`whitespace-nowrap text-xs ${textMutedOnCanvas}`}
      />
    ),
  },
  updated: {
    header: "Updated",
    accessorFn: (item) => item.issue.updated_at,
    sortingFn: (a, b) =>
      compareTimestamps(a.original.issue.updated_at, b.original.issue.updated_at),
    cell: ({ getValue }) => (
      <Timestamp
        at={getValue<string>()}
        className={`whitespace-nowrap text-xs ${textMutedOnCanvas}`}
      />
    ),
  },
  closed: {
    header: "Closed",
    accessorFn: (item) => item.issue.closed_at ?? undefined,
    sortingFn: emptyLast((item) => item.issue.closed_at ?? undefined, compareTimestamps),
    cell: ({ row }) =>
      row.original.issue.closed_at === null ? null : (
        <Timestamp
          at={row.original.issue.closed_at}
          className={`whitespace-nowrap text-xs ${textMutedOnCanvas}`}
        />
      ),
  },
  slot: {
    header: "Slot",
    accessorFn: (item) => item.card?.card.slot ?? undefined,
    sortingFn: emptyLast((item) => item.card?.card.slot ?? undefined, byNumber),
    cell: ({ row }) =>
      row.original.card?.card.slot == null ? null : (
        <span className="tabular-nums">{row.original.card.card.slot}</span>
      ),
  },
  roles: {
    header: "Roles",
    accessorFn: (item) => (item.card?.card.roles ?? []).join(", "),
    cell: ({ getValue }) => (
      <span className={`text-xs ${textSecondaryOnCanvas}`}>{getValue<string>()}</span>
    ),
  },
  slot_use: {
    header: "Slot use",
    accessorFn: (item) => item.card?.card.slot_use ?? "",
    sortingFn: emptyLast((item) => {
      const use = item.card?.card.slot_use ?? "";
      const index = (SLOT_USES as readonly string[]).indexOf(use);
      return index === -1 ? undefined : index;
    }, byNumber),
    cell: ({ getValue }) =>
      getValue<string>() === "" ? null : (
        <span className={`text-xs ${textSecondaryOnCanvas}`}>
          {slotUseLabel(getValue<string>())}
        </span>
      ),
  },
  waited: {
    header: "Waited",
    accessorFn: (item) => ageMs(item.card?.card.waiting?.since),
    sortingFn: emptyLast((item) => ageMs(item.card?.card.waiting?.since), byNumber),
    cell: ({ row }) => {
      const since = row.original.card?.card.waiting?.since ?? null;
      return since === null ? null : (
        <span className={`text-xs tabular-nums ${textSecondaryOnCanvas}`}>
          {formatPhaseTime(since, Date.now())}
        </span>
      );
    },
  },
  waiting_reason: {
    header: "Waiting",
    accessorFn: (item) => item.card?.card.waiting?.reason ?? "",
    sortingFn: emptyLast((item) => {
      const index = (WAITING_REASONS as readonly string[]).indexOf(
        item.card?.card.waiting?.reason ?? ""
      );
      return index === -1 ? undefined : index;
    }, byNumber),
    cell: ({ row }) => {
      const waiting = row.original.card?.card.waiting;
      if (waiting === null || waiting === undefined) return null;
      const blockers = waiting.blocked_by ?? [];
      return (
        <span className={`text-xs ${textSecondaryOnCanvas}`}>
          {waitingLabel(waiting.reason)}
          {blockers.length === 0 ? "" : ` by ${blockers.join(", ")}`}
        </span>
      );
    },
  },
  time_in_phase: {
    header: "In phase",
    accessorFn: (item) => ageMs(item.card?.card.phase_since),
    sortingFn: emptyLast((item) => ageMs(item.card?.card.phase_since), byNumber),
    cell: ({ row }) => {
      const since = row.original.card?.card.phase_since ?? null;
      return since === null ? null : (
        <span className={`text-xs tabular-nums ${textSecondaryOnCanvas}`}>
          {formatPhaseTime(since, Date.now())}
        </span>
      );
    },
  },
  work_hours: {
    header: "Work time",
    accessorFn: (item) => item.facts?.work_hours,
    sortingFn: emptyLast((item) => item.facts?.work_hours, byNumber),
    cell: ({ row }) =>
      row.original.facts === undefined ? null : (
        <span className={`text-xs tabular-nums ${textSecondaryOnCanvas}`}>
          {formatWorkTime(row.original.facts.work_hours)}
        </span>
      ),
  },
  review_rounds: {
    header: "Rounds",
    accessorFn: (item) => item.facts?.review_rounds,
    sortingFn: emptyLast((item) => item.facts?.review_rounds, byNumber),
    cell: ({ row }) =>
      row.original.facts === undefined ? null : (
        <span className="tabular-nums">{row.original.facts.review_rounds}</span>
      ),
  },
  legion: {
    header: "Tree",
    accessorFn: (item) =>
      item.facts === undefined ? "" : item.facts.tree_depth === 0 ? "Root" : item.facts.root_key,
    cell: ({ row }) => {
      const facts = row.original.facts;
      if (facts === undefined) return null;
      if (facts.tree_depth === 0) return <span className="text-xs">Root</span>;
      return (
        <Link
          className={`text-xs ${linkText} ${linkHoverText}`}
          to={buildIssuePath({ key: facts.root_key, kind: "issue" })}
        >
          {facts.root_key}
        </Link>
      );
    },
  },
  phase_hours: {
    header: "Time per phase",
    accessorFn: (item) => phaseHours(item.facts).total,
    sortDescFirst: true,
    cell: ({ row }) => <PhaseHoursCell facts={row.original.facts} />,
  },
});

/** The columns only some listings have a use for: which project a row is in, which priorities
 *  it advances, when it was moved into backlog or icebox and by whom, and why. A table has each
 *  only where its listing gives the cell something true to say (`listingColumns`). */
export type ListingColumnId = "project" | "advances" | "moved" | "reason";

/**
 * The listing columns a table of `listing` has, `titled` when its caller gives each priority's
 * title:
 *
 * - Project, on a listing of no project (an `advances` listing, the work of every project); a
 *   project's own table would name that one project on every row.
 * - Advances, where the titles are given: a title is the only name a reader knows a priority by,
 *   and without them the column could name each priority only by its id.
 * - Moved there, on a listing that asks for the move (`moved`): no other listing is answered with
 *   one, so its every cell would be empty, whatever the record holds.
 * - Reason, on that same listing and no other: why the work was moved there. Nothing records a
 *   reason yet, and every cell says so, naming the work that records one.
 */
export function listingColumns(listing: IssueListing, titled: boolean): ListingColumnId[] {
  const scoped = "advances" in listing;
  return [
    ...(scoped ? (["project"] as const) : []),
    ...(titled ? (["advances"] as const) : []),
    ...(scoped && listing.moved === true ? (["moved", "reason"] as const) : []),
  ];
}

/** The work that records why an issue was moved into backlog or icebox. Until it lands no move
 *  carries a reason, and the Reason cell names it. */
const MOVE_REASON_WORK = "dispatch://INFRA-2934";

/**
 * The four listing columns. None of them sorts, filters or groups: the table's view in the
 * address reads only the columns `issues-table-model.ts` registers, and these are the cells'
 * own, kept out of that registry. A table has only the ones its listing gives a use
 * (`listingColumns`).
 */
const LISTING_COLUMNS: readonly {
  readonly id: ListingColumnId;
  readonly size: number;
  readonly min: number;
  readonly rendering: (titleOf: (id: string) => string) => Partial<ColumnDef<Item>>;
}[] = [
  {
    id: "project",
    size: 120,
    min: 96,
    rendering: () => ({
      header: "Project",
      accessorFn: (item) => issueProject(item.issue.key),
      cell: ({ getValue }) => (
        <span className={`whitespace-nowrap ${textSecondaryOnCanvas}`}>{getValue<string>()}</span>
      ),
    }),
  },
  {
    id: "advances",
    size: 220,
    min: 140,
    rendering: (titleOf) => ({
      header: "Advances",
      accessorFn: (item) => (item.issue.advances ?? []).map(({ id }) => titleOf(id)).join(", "),
      cell: ({ row }) => (
        <span className="flex flex-wrap gap-x-2">
          {(row.original.issue.advances ?? []).map(({ id }) => (
            <Link className={`${linkText} ${linkHoverText}`} key={id} to={priorityPath(id)}>
              {titleOf(id)}
            </Link>
          ))}
        </span>
      ),
    }),
  },
  {
    id: "moved",
    size: 220,
    min: 160,
    rendering: () => ({
      header: "Moved there",
      accessorFn: (item) => item.issue.moved_at ?? "",
      // A row the listing answered with no move draws an empty cell: absent is absent, and no
      // cell claims a record is missing.
      cell: ({ row }) => {
        const { moved_at: at, moved_by: by } = row.original.issue;
        return at === undefined ? null : (
          <span className="flex flex-wrap items-center gap-1">
            <Timestamp at={at} className={`whitespace-nowrap text-xs ${textMutedOnCanvas}`} />
            {by === undefined ? null : <AgentName actor={by} />}
          </span>
        );
      },
    }),
  },
  {
    id: "reason",
    size: 240,
    min: 140,
    rendering: () => ({
      header: "Reason",
      accessorFn: () => "",
      // The same words on every row, since no move carries a reason yet: the cell says the
      // record is missing rather than drawing an empty cell a reader could take for no reason.
      cell: () => (
        <span className={textSecondaryOnCanvas}>
          Not recorded until <ReferenceLink reference={MOVE_REASON_WORK} />
        </span>
      ),
    }),
  },
];

/** The columns a caller may name among those its table opens with shown: the listing columns,
 *  and Updated, which a fault list of the priorities page opens with shown. */
export type ShownColumnId = ListingColumnId | "updated";

/**
 * The registry columns an issue table another page draws has (the priority pages'), at the
 * widths main's registry gives them: those tables keep main's columns and look, which the pages'
 * approved pictures show. They also offer Closed, with its date-range filter, hidden until the
 * reader shows it, since the reader keeps every column the project's table has that needs no
 * project (INFRA-2947, R14); Stage, Place and the Legion columns read a project's daemon board,
 * so they are the Issues tab's alone. `phone` is a column's narrowest on a phone where it
 * differs.
 */
const EMBEDDED_WIDTHS: Readonly<
  Partial<
    Record<ColumnId, { readonly size: number; readonly min: number; readonly phone?: number }>
  >
> = {
  select: { size: 44, min: 44 },
  priority: { size: 140, min: 140 },
  queue: { size: 96, min: 124 },
  key: { size: 104, min: 108, phone: 88 },
  title: { size: 380, min: 116, phone: 120 },
  status: { size: 128, min: 132 },
  epic: { size: 220, min: 116 },
  assignee: { size: 180, min: 148 },
  author: { size: 200, min: 164 },
  labels: { size: 200, min: 132 },
  claim: { size: 220, min: 164 },
  asks: { size: 96, min: 148 },
  created: { size: 120, min: 148 },
  updated: { size: 120, min: 148 },
  closed: { size: 120, min: 148 },
};
/** Those columns, in the registry's order, which is main's. */
const EMBEDDED_IDS: readonly ColumnId[] = COLUMNS.map(({ id }) => id).filter(
  (id) => EMBEDDED_WIDTHS[id] !== undefined
);
const embeddedMin = (id: ColumnId): number => EMBEDDED_WIDTHS[id]?.min ?? 0;
const embeddedPhoneMin = (id: ColumnId): number =>
  EMBEDDED_WIDTHS[id]?.phone ?? EMBEDDED_WIDTHS[id]?.min ?? 0;

/**
 * The cells such a table draws as main draws them, where the Issues tab's differ: Queue, the
 * number Start next reads, with the rows that have none after the rest in the listing's order
 * (where the Issues tab's Place is the daemon's waiting line), and Assignee, whole on one line.
 */
const EMBEDDED_RENDERINGS: Readonly<Partial<Record<ColumnId, Partial<ColumnDef<Item>>>>> = {
  queue: {
    header: "Queue",
    accessorFn: (item) => item.queue,
    sortUndefined: "last",
    sortDescFirst: false,
    cell: ({ getValue }) => {
      const place = getValue<number | undefined>();
      return place === undefined ? null : (
        <span className="font-semibold tabular-nums" title="Legion starts issues in this order">
          #{place}
        </span>
      );
    },
  },
  assignee: {
    header: "Assignee",
    accessorFn: (item) => item.issue.assignee ?? "Unassigned",
    cell: ({ getValue }) => (
      <span className={`whitespace-nowrap ${textSecondaryOnCanvas}`}>{getValue<string>()}</span>
    ),
  },
};

/** Each layout `issueTableLayout` built, by its columns and the ones it opens with shown. */
const layouts = new Map<string, LayoutColumns>();

/**
 * The layout of an issue table another page draws, as main lays it out: main's registry columns
 * at main's widths with Updated hidden, Closed offered and hidden, the listing columns `columns`
 * (`listingColumns`) at their widths, each hidden unless `shown` names it, and main's pins -
 * Select, Priority, Key and Title within 60% of a wide box, Select, Key and Title within 85% of
 * a phone's. Built once for each pair and kept, since the stored layout is read again whenever
 * this object changes (`useTableLayout`).
 */
export function issueTableLayout(
  columns: readonly ListingColumnId[],
  shown: readonly ShownColumnId[] = []
): LayoutColumns {
  const name = `${columns.join(" ")}|${shown.join(" ")}`;
  const built = layouts.get(name);
  if (built !== undefined) return built;
  const listing = LISTING_COLUMNS.filter(({ id }) => columns.includes(id));
  const hidden: [string, boolean][] = [
    ["updated", false],
    ["closed", false],
    ...listing.map(({ id }): [string, boolean] => [id, false]),
  ];
  const layout: LayoutColumns = {
    ids: [...EMBEDDED_IDS, ...listing.map(({ id }) => id)],
    rowAttribute: ISSUE_ROW_ATTRIBUTE,
    hiddenByDefault: Object.fromEntries(
      hidden.filter(([id]) => !shown.includes(id as ShownColumnId))
    ),
    sizes: {
      ...Object.fromEntries(EMBEDDED_IDS.map((id) => [id, EMBEDDED_WIDTHS[id]?.size])),
      ...Object.fromEntries(listing.map(({ id, size }) => [id, size])),
    },
    minimums: {
      ...Object.fromEntries(EMBEDDED_IDS.map((id) => [id, embeddedMin(id)])),
      ...Object.fromEntries(listing.map(({ id, min }) => [id, min])),
    },
    pins: {
      wide: {
        columns: COLUMNS.filter((column) => column.pinned).map(({ id }) => id),
        share: 0.6,
      },
      phone: {
        columns: ["select", "key", "title"] satisfies ColumnId[],
        share: 0.85,
        widths: { key: embeddedPhoneMin("key") },
        flexibleMin: embeddedPhoneMin("title"),
      },
      flexible: "title" satisfies ColumnId,
    },
  };
  layouts.set(name, layout);
  return layout;
}

const NO_LISTING_COLUMNS: readonly ListingColumnId[] = [];

/** The List's columns, and the listing columns `listing` names after Title. `onMark` is told
 *  the row a row's own checkbox marks; it is a dependency of the columns, so a caller hands a
 *  stable one, as it does `listing` and `priorityTitles`. A table another page draws
 *  (`embedded`) has main's registry columns at main's widths, drawn as main draws them. */
export function useIssueTableColumns(
  userState: UserState | undefined,
  compact: boolean,
  onMark: (key: string) => void,
  listing: readonly ListingColumnId[] = NO_LISTING_COLUMNS,
  /** Each priority's title by its id, which the Advances column names a priority by; one this
   *  map does not hold, a priority deleted since it was read, is named by its id. */
  priorityTitles?: ReadonlyMap<string, string>,
  embedded = false
): ColumnDef<Item>[] {
  return useMemo<ColumnDef<Item>[]>(() => {
    const rendering = renderings(userState, onMark);
    const titleOf = (id: string) => priorityTitles?.get(id) ?? id;
    const registered = COLUMNS.filter(
      (column) => !embedded || EMBEDDED_WIDTHS[column.id] !== undefined
    ).map((column): ColumnDef<Item> => {
      const picks = column.filter?.kind === "picks" ? column.filter : undefined;
      const minSize = embedded
        ? compact
          ? embeddedPhoneMin(column.id)
          : embeddedMin(column.id)
        : ((compact ? COMPACT_COLUMN_MIN[column.id] : undefined) ?? COLUMN_MIN[column.id]);
      // Main's Queue filter names its values as they are, Queued and Not queued.
      const show = embedded && column.id === "queue" ? undefined : picks?.show;
      return {
        ...((embedded ? EMBEDDED_RENDERINGS[column.id] : undefined) ?? rendering[column.id]),
        id: column.id,
        size: embedded ? (EMBEDDED_WIDTHS[column.id]?.size ?? column.size) : column.size,
        minSize,
        enableResizing: column.id !== "select",
        enableSorting: column.sortable !== false,
        enableColumnFilter: column.filter !== undefined,
        enableGrouping: picks?.group !== undefined,
        ...(picks?.group === undefined ? {} : { getGroupingValue: picks.group }),
        ...(column.filter === undefined
          ? {}
          : { filterFn: column.filter.kind === "text" ? "includesString" : inPicks }),
        meta: {
          ...(column.filter?.kind === "text" ? { text: true } : {}),
          ...(column.filter?.kind === "range" ? { range: true } : {}),
          ...(picks?.order === undefined ? {} : { order: picks.order }),
          ...(show === undefined ? {} : { show }),
          ...(picks?.always === undefined ? {} : { always: picks.always }),
        },
      };
    });
    if (listing.length === 0) return registered;
    const extra = LISTING_COLUMNS.filter(({ id }) => listing.includes(id)).map(
      (column): ColumnDef<Item> => ({
        ...column.rendering(titleOf),
        id: column.id,
        size: column.size,
        minSize: column.min,
        enableResizing: true,
        enableSorting: false,
        enableColumnFilter: false,
        enableGrouping: false,
      })
    );
    // Beside the title, which is what a reader reads a row of every project's issues by.
    const after = registered.findIndex(({ id }) => id === "title") + 1;
    return [...registered.slice(0, after), ...extra, ...registered.slice(after)];
  }, [userState, compact, onMark, listing, priorityTitles, embedded]);
}
