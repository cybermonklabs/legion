import type { ColumnDef, FilterFn, RowData } from "@tanstack/react-table";
import { useMemo } from "react";
import { Link } from "react-router-dom";
import type { UserState } from "../../api/types";
import { AttentionBadge } from "../../components/Badge";
import { LabelPill, StatusPill } from "../../components/Pill";
import { issueProject } from "../../lib/issue-key";
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
import { priorityPath } from "../priorities/priority-routes";
import { AgentName } from "../refs/AgentName";
import { referenceTriggerProps } from "../refs/RefPreview";
import { buildIssuePath } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { type IssueStatus, issueStatuses, statusLabel } from "./board-model";
import type { IssueListing } from "./issues-table-data";
import type { LayoutColumns } from "./issues-table-layout";
import {
  COLUMN_MIN,
  COLUMNS,
  COMPACT_COLUMN_MIN,
  type ColumnId,
  ISSUE_TABLE_COLUMNS,
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
    /** The values the filter offers whatever the loaded rows hold. */
    always?: readonly string[];
    /** Filter by typed text instead of picking values: for a column of free text. */
    text?: boolean;
    /** The types for the declaration's own parameters, which it does not otherwise use. */
    value?: [TData, TValue];
  }
}
const inPicks: FilterFn<Item> = (row, id, value: string[]) =>
  matchesFilters(row.original, [{ id, value }]);

/** How each column renders and sorts, for the reader `userState` describes. Keyed by the
 *  registry's own ids, so a typo or a column with no rendering fails to compile. Everything
 *  else about a column — whether it filters, what a row files under, what it groups by, how
 *  wide it starts and how narrow it may go — comes from `COLUMNS` in the model, so the two
 *  never disagree. */
const renderings = (
  userState: UserState | undefined
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
});

/** The columns only some listings have a use for: which project a row is in, which priorities
 *  it advances, and when it was moved into backlog or icebox and by whom. A table has each only
 *  where its listing gives the cell something true to say (`listingColumns`). */
export type ListingColumnId = "project" | "advances" | "moved";

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
 */
export function listingColumns(listing: IssueListing, titled: boolean): ListingColumnId[] {
  const scoped = "advances" in listing;
  return [
    ...(scoped ? (["project"] as const) : []),
    ...(titled ? (["advances"] as const) : []),
    ...(scoped && listing.moved === true ? (["moved"] as const) : []),
  ];
}

/**
 * The three listing columns. They name what R6 asks each fault row to name, and none of them
 * sorts, filters or groups - deliberately: the table's view in the address reads only the
 * columns `features/project/issues-table-model.ts` registers (`readUrl` drops any other id), and
 * these are the cells' own, kept out of that registry. Every registered column still sorts,
 * filters and groups on every table that shows these beside it. A table has only the ones its
 * listing gives a use (`listingColumns`).
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
      // A row the listing answered with no move - it stands outside backlog and icebox, or the
      // issue's events do not reach back to the move - draws an empty cell: absent is absent,
      // and no cell claims a record is missing.
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
];

/** The columns a caller may name among those its table opens with shown: the listing columns,
 *  and Updated, which a project's table opens with hidden and a fault list opens with shown,
 *  since R6 asks every fault row to name how long it has stood. */
export type ShownColumnId = ListingColumnId | "updated";

/** Each layout `issueTableLayout` built, by its columns and the ones it opens with shown. */
const layouts = new Map<string, LayoutColumns>();

/**
 * The layout of an issues table with the listing columns `columns` (`listingColumns`) that opens
 * with `shown` shown: the registry's own (`ISSUE_TABLE_COLUMNS`), with those listing columns at
 * their widths, and every column the registry opens hidden or a listing column not in `shown`
 * hidden until the reader shows it. A project's table has none of them and names none; each
 * fault list of the priorities page names its own. Built once for each pair and kept, since the
 * stored layout is read again whenever this object changes (`useTableLayout`).
 */
export function issueTableLayout(
  columns: readonly ListingColumnId[],
  shown: readonly ShownColumnId[] = []
): LayoutColumns {
  const name = `${columns.join(" ")}|${shown.join(" ")}`;
  const built = layouts.get(name);
  if (built !== undefined) return built;
  const listing = LISTING_COLUMNS.filter(({ id }) => columns.includes(id));
  const hidden = {
    ...ISSUE_TABLE_COLUMNS.hiddenByDefault,
    ...Object.fromEntries(listing.map(({ id }) => [id, false])),
  };
  const layout: LayoutColumns = {
    ...ISSUE_TABLE_COLUMNS,
    ids: [...ISSUE_TABLE_COLUMNS.ids, ...listing.map(({ id }) => id)],
    hiddenByDefault: Object.fromEntries(
      Object.entries(hidden).filter(([id]) => !shown.includes(id as ShownColumnId))
    ),
    sizes: {
      ...ISSUE_TABLE_COLUMNS.sizes,
      ...Object.fromEntries(listing.map(({ id, size }) => [id, size])),
    },
    minimums: {
      ...ISSUE_TABLE_COLUMNS.minimums,
      ...Object.fromEntries(listing.map(({ id, min }) => [id, min])),
    },
  };
  layouts.set(name, layout);
  return layout;
}

export function useIssueTableColumns(
  userState: UserState | undefined,
  compact: boolean,
  /** The table's layout (`issueTableLayout`): which listing columns it has. */
  layout: LayoutColumns,
  /** Each priority's title by its id, which the Advances column names a priority by; one this
   *  map does not hold, a priority deleted since it was read, is named by its id. */
  priorityTitles?: ReadonlyMap<string, string>
): ColumnDef<Item>[] {
  return useMemo<ColumnDef<Item>[]>(() => {
    const rendering = renderings(userState);
    const titleOf = (id: string) => priorityTitles?.get(id) ?? id;
    const registered = COLUMNS.map((column): ColumnDef<Item> => {
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
        ...(column.filter === undefined ? {} : { filterFn: picks ? inPicks : "includesString" }),
        meta: {
          ...(column.filter?.kind === "text" ? { text: true } : {}),
          ...(picks?.order === undefined ? {} : { order: picks.order }),
          ...(picks?.show === undefined ? {} : { show: picks.show }),
          ...(picks?.always === undefined ? {} : { always: picks.always }),
        },
      };
    });
    const listing = LISTING_COLUMNS.filter(({ id }) => layout.ids.includes(id)).map(
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
    return [...registered.slice(0, after), ...listing, ...registered.slice(after)];
  }, [userState, compact, layout, priorityTitles]);
}
