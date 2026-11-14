import type { ColumnDef, FilterFn, Row, RowData } from "@tanstack/react-table";
import { type ReactNode, useMemo } from "react";
import { Link } from "react-router-dom";
import type { UserState } from "../../api/types";
import { AttentionBadge } from "../../components/Badge";
import { LabelPill, StatusPill } from "../../components/Pill";
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
import { AgentName } from "../refs/AgentName";
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
import {
  COLUMN_MIN,
  COLUMNS,
  COMPACT_COLUMN_MIN,
  type ColumnId,
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

/** The List's columns. `onMark` is told the row a row's own checkbox marks; it is a dependency
 *  of the columns, so a caller hands a stable one. */
export function useIssueTableColumns(
  userState: UserState | undefined,
  compact: boolean,
  onMark: (key: string) => void
): ColumnDef<Item>[] {
  return useMemo<ColumnDef<Item>[]>(() => {
    const rendering = renderings(userState, onMark);
    return COLUMNS.map((column) => {
      const picks = column.filter?.kind === "picks" ? column.filter : undefined;
      return {
        ...rendering[column.id],
        id: column.id,
        size: column.size,
        minSize: (compact ? COMPACT_COLUMN_MIN[column.id] : undefined) ?? COLUMN_MIN[column.id],
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
          ...(picks?.show === undefined ? {} : { show: picks.show }),
          ...(picks?.always === undefined ? {} : { always: picks.always }),
        },
      };
    });
  }, [userState, compact, onMark]);
}
