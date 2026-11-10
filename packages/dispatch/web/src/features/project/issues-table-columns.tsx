import type { ColumnDef, FilterFn, RowData } from "@tanstack/react-table";
import { useMemo } from "react";
import { Link } from "react-router-dom";
import type { UserState } from "../../api/types";
import { AttentionBadge } from "../../components/Badge";
import { LabelPill, StatusPill } from "../../components/Pill";
import { compareTimestamps } from "../../lib/timestamps";
import {
  checkboxAccent,
  linkHoverText,
  linkText,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { ClaimChip } from "../issue/ClaimChip";
import { issuePriority } from "../issue/issue-priority";
import { PriorityControl } from "../issue/PriorityControl";
import { ProgressChips } from "../issue/ProgressChips";
import { UnreachableRouteMarker } from "../issue/RouteReach";
import { AgentName } from "../refs/AgentName";
import { referenceTriggerProps } from "../refs/RefPreview";
import { buildIssuePath } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { type IssueStatus, issueStatuses, statusLabel } from "./board-model";
import {
  COLUMN_MIN,
  COLUMNS,
  COMPACT_COLUMN_MIN,
  type Item,
  matchesFilters,
} from "./issues-table-model";
import { issueIsUnread, UnreadDot } from "./UnreadDot";

declare module "@tanstack/react-table" {
  interface ColumnMeta<TData extends RowData, TValue> {
    /** A facet value as the reader sees it. */
    show?: (value: string) => string;
    /** The facet values in the order the filter lists them; by count when absent. */
    order?: readonly string[];
    /** Filter by typed text instead of picking values: for a column of free text. */
    text?: boolean;
    /** The types for the declaration's own parameters, which it does not otherwise use. */
    value?: [TData, TValue];
  }
}
const inPicks: FilterFn<Item> = (row, id, value: string[]) =>
  matchesFilters(row.original, [{ id, value }]);

/** How each column renders and sorts, for the reader `userState` describes. Everything else
 *  about a column — whether it filters, what a row files under, what it groups by, how wide it
 *  starts and how narrow it may go — comes from `COLUMNS` in the model, so the two never
 *  disagree. */
const renderings = (
  userState: UserState | undefined
): Record<string, Partial<ColumnDef<Item>>> => ({
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
        onChange={row.getToggleSelectedHandler()}
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
    cell: ({ getValue }) => (
      <span className={`whitespace-nowrap ${textSecondaryOnCanvas}`}>{getValue<string>()}</span>
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
      <span className={`whitespace-nowrap text-xs ${textMutedOnCanvas}`}>
        <Timestamp at={getValue<string>()} />
      </span>
    ),
  },
  updated: {
    header: "Updated",
    accessorFn: (item) => item.issue.updated_at,
    sortingFn: (a, b) =>
      compareTimestamps(a.original.issue.updated_at, b.original.issue.updated_at),
    cell: ({ getValue }) => (
      <span className={`whitespace-nowrap text-xs ${textMutedOnCanvas}`}>
        <Timestamp at={getValue<string>()} />
      </span>
    ),
  },
});

export function useIssueTableColumns(
  userState: UserState | undefined,
  compact: boolean
): ColumnDef<Item>[] {
  return useMemo<ColumnDef<Item>[]>(() => {
    const rendering = renderings(userState);
    return COLUMNS.map((column) => {
      const picks = column.filter?.kind === "picks" ? column.filter : undefined;
      return {
        ...(rendering[column.id] ?? {}),
        id: column.id,
        size: column.size,
        minSize: (compact ? COMPACT_COLUMN_MIN[column.id] : undefined) ?? COLUMN_MIN[column.id],
        enableResizing: column.id !== "select",
        enableSorting: column.sortable !== false,
        enableColumnFilter: column.filter !== undefined,
        enableGrouping: picks?.group !== undefined,
        ...(picks?.group === undefined ? {} : { getGroupingValue: picks.group }),
        ...(column.filter === undefined ? {} : { filterFn: picks ? inPicks : "includesString" }),
        meta: {
          ...(column.filter?.kind === "text" ? { text: true } : {}),
          ...(picks?.order === undefined ? {} : { order: picks.order }),
          ...(picks?.show === undefined ? {} : { show: picks.show }),
        },
      };
    });
  }, [userState, compact]);
}
