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
import { PriorityControl } from "../issue/PriorityControl";
import { ProgressChips } from "../issue/ProgressChips";
import { UnreachableRouteMarker } from "../issue/RouteReach";
import { AgentName } from "../refs/AgentName";
import { referenceTriggerProps } from "../refs/RefPreview";
import { buildIssuePath } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { type IssueStatus, issueStatuses, statusLabel } from "./board-model";
import { AGES, ageOf, type Item, matchesFilters, priorityName } from "./issues-table-model";
import { issueIsUnread, UnreadDot } from "./UnreadDot";

declare module "@tanstack/react-table" {
  interface ColumnMeta<TData extends RowData, TValue> {
    /** The value or values a row files under in the column's filter (and its group). */
    facet?: (item: TData) => string | string[];
    /** A facet value as the reader sees it. */
    show?: (value: string) => string;
    /** The facet values in the order the filter lists them; by count when absent. */
    order?: readonly string[];
    /** Filter by typed text instead of picking values: for a column of free text. */
    text?: boolean;
    /** The type for `TValue`, so the declaration uses both parameters. */
    value?: TValue;
  }
}
const inPicks: FilterFn<Item> = (row, id, value: string[]) =>
  matchesFilters(row.original, [{ id, value }]);
export const PINNED = ["select", "priority", "key", "title"];
export const COLUMN_SIZES: Record<string, number> = {
  select: 44,
  priority: 140,
  queue: 96,
  key: 104,
  title: 380,
  status: 128,
  epic: 220,
  assignee: 180,
  author: 200,
  labels: 200,
  claim: 220,
  asks: 96,
  created: 120,
  updated: 120,
};
// Label, sort indicator, 44px filter target, padding and a separate resize edge.
export const COLUMN_MIN: Record<string, number> = {
  select: 44,
  priority: 140,
  key: 108,
  title: 116,
  queue: 124,
  status: 132,
  epic: 116,
  assignee: 148,
  author: 164,
  labels: 132,
  claim: 164,
  asks: 148,
  created: 148,
  updated: 148,
};
export const COMPACT_COLUMN_MIN = { key: 88, title: 60 };

export function useIssueTableColumns(
  userState: UserState | undefined,
  compact: boolean
): ColumnDef<Item>[] {
  const columns = useMemo<ColumnDef<Item>[]>(
    () =>
      (
        [
          {
            id: "select",
            enableSorting: false,
            enableHiding: false,
            enableColumnFilter: false,
            enableGrouping: false,
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
          },
          {
            id: "priority",
            header: "Priority",
            accessorFn: (item) => item.issue.priority ?? undefined,
            sortUndefined: "last",
            sortDescFirst: false,
            filterFn: inPicks,
            getGroupingValue: (item) => String(item.issue.priority ?? ""),
            meta: {
              show: priorityName,
              order: ["0", "1", "2", "3", ""],
            },
            cell: ({ row }) => (
              <PriorityControl
                disabled={row.original.issue.status === "done"}
                issueKey={row.original.issue.key}
                priority={row.original.issue.priority}
              />
            ),
          },
          {
            id: "queue",
            header: "Queue",
            accessorFn: (item) => item.queue,
            sortUndefined: "last",
            sortDescFirst: false,
            filterFn: inPicks,
            enableGrouping: false,
            meta: {
              order: ["Queued", "Not queued"],
            },
            cell: ({ getValue }) => {
              const place = getValue<number | undefined>();
              return place === undefined ? null : (
                <span
                  className="font-semibold tabular-nums"
                  title="Legion starts issues in this order"
                >
                  #{place}
                </span>
              );
            },
          },
          {
            id: "key",
            header: "Key",
            accessorFn: (item) => item.issue.key,
            sortingFn: (a, b) =>
              Number(a.original.issue.key.split("-")[1]) -
              Number(b.original.issue.key.split("-")[1]),
            filterFn: inPicks,
            enableGrouping: false,
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
          {
            id: "title",
            header: "Title",
            accessorFn: (item) => item.issue.title,
            filterFn: "includesString",
            enableGrouping: false,
            meta: { text: true },
            cell: ({ row }) => (
              <div className="min-w-0 space-y-1">
                <Link
                  aria-label={`${row.original.issue.key} ${row.original.issue.title}`}
                  data-issue-link
                  className={`flex w-full min-w-0 ${linkText} ${linkHoverText}`}
                  to={buildIssuePath({ key: row.original.issue.key, kind: "issue" })}
                  {...referenceTriggerProps({ key: row.original.issue.key, kind: "issue" })}
                >
                  <span
                    className={`line-clamp-2 min-w-0 flex-1 break-words ${textPrimaryOnCanvas}`}
                  >
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
          {
            id: "status",
            header: "Status",
            accessorFn: (item) => item.issue.status,
            sortingFn: (a, b) =>
              issueStatuses.indexOf(a.original.issue.status as IssueStatus) -
              issueStatuses.indexOf(b.original.issue.status as IssueStatus),
            filterFn: inPicks,
            meta: {
              show: (value) => statusLabel(value as IssueStatus),
              order: issueStatuses,
            },
            cell: ({ getValue }) => <StatusPill>{statusLabel(getValue<IssueStatus>())}</StatusPill>,
          },
          {
            id: "epic",
            header: "Epic",
            accessorFn: (item) => item.epic,
            filterFn: inPicks,
            cell: ({ getValue }) => (
              <span className={`line-clamp-2 ${textSecondaryOnCanvas}`}>{getValue<string>()}</span>
            ),
          },
          {
            id: "assignee",
            header: "Assignee",
            accessorFn: (item) => item.issue.assignee ?? "Unassigned",
            filterFn: inPicks,
            cell: ({ getValue }) => (
              <span className={`whitespace-nowrap ${textSecondaryOnCanvas}`}>
                {getValue<string>()}
              </span>
            ),
          },
          {
            id: "author",
            header: "Created by",
            accessorFn: (item) => item.author,
            filterFn: inPicks,
            cell: ({ row }) =>
              row.original.issue.created_by === null ? (
                <span className={textSecondaryOnCanvas}>Unknown</span>
              ) : (
                <AgentName actor={row.original.issue.created_by} />
              ),
          },
          {
            id: "labels",
            header: "Labels",
            accessorFn: (item) => (item.issue.labels ?? []).join(", "),
            filterFn: inPicks,
            enableGrouping: false,
            cell: ({ row }) => (
              <span className="flex flex-wrap gap-1">
                {(row.original.issue.labels ?? []).map((label) => (
                  <LabelPill key={label}>{label}</LabelPill>
                ))}
              </span>
            ),
          },
          {
            id: "claim",
            header: "Claimed by",
            accessorFn: (item) => item.claimedBy,
            filterFn: inPicks,
            cell: ({ row }) => <ClaimChip claim={row.original.issue.claim} />,
          },
          {
            id: "asks",
            header: "Open asks",
            accessorFn: (item) => item.issue.open_asks,
            sortDescFirst: true,
            filterFn: inPicks,
            enableGrouping: false,
            cell: ({ getValue }) =>
              getValue<number>() === 0 ? null : <AttentionBadge count={getValue<number>()} />,
          },
          {
            id: "created",
            header: "Created",
            accessorFn: (item) => item.issue.created_at,
            filterFn: inPicks,
            sortingFn: (a, b) =>
              compareTimestamps(a.original.issue.created_at, b.original.issue.created_at),
            getGroupingValue: (item) => ageOf(item.issue.created_at),
            meta: {
              order: AGES,
            },
            cell: ({ getValue }) => (
              <span className={`whitespace-nowrap text-xs ${textMutedOnCanvas}`}>
                <Timestamp at={getValue<string>()} />
              </span>
            ),
          },
          {
            id: "updated",
            header: "Updated",
            accessorFn: (item) => item.issue.updated_at,
            filterFn: inPicks,
            sortingFn: (a, b) =>
              compareTimestamps(a.original.issue.updated_at, b.original.issue.updated_at),
            getGroupingValue: (item) => ageOf(item.issue.updated_at),
            meta: { order: AGES },
            cell: ({ getValue }) => (
              <span className={`whitespace-nowrap text-xs ${textMutedOnCanvas}`}>
                <Timestamp at={getValue<string>()} />
              </span>
            ),
          },
        ] as ColumnDef<Item>[]
      ).map((column) => ({
        ...column,
        size: COLUMN_SIZES[column.id ?? ""] ?? 140,
        minSize:
          compact && column.id === "key"
            ? COMPACT_COLUMN_MIN.key
            : compact && column.id === "title"
              ? COMPACT_COLUMN_MIN.title
              : (COLUMN_MIN[column.id ?? ""] ?? 116),
        enableResizing: column.id !== "select",
      })),
    [userState, compact]
  );
  return columns;
}
