import type { ColumnDef, FilterFn, RowData } from "@tanstack/react-table";
import { useMemo } from "react";
import { Link } from "react-router-dom";
import type { UserState } from "../../api/types";
import { AttentionBadge } from "../../components/Badge";
import { LabelPill, StatusPill } from "../../components/Pill";
import { TruncatedText } from "../../components/TruncatedText";
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
import { PriorityChip } from "../priorities/PriorityChip";
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
  createdAt,
  createdBy,
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
    /** A column of counts: its header's words stand at its right edge, over its numbers, and
     *  wrap rather than be cut (`DataTable`'s header). */
    numeric?: boolean;
    /** A header whose words wrap onto a second line rather than be cut, in a column narrower
     *  than its words on one line. */
    wrap?: boolean;
    /** The types for the declaration's own parameters, which it does not otherwise use. */
    value?: [TData, TValue];
  }
}
const inPicks: FilterFn<Item> = (row, id, value: string[]) =>
  matchesFilters(row.original, [{ id, value }]);

/** How each column renders and sorts, for the reader `userState` describes, with each title
 *  in eight lines at most where the table wraps its titles (`wrapTitles`), ending in '…' with the
 *  whole of it in its tooltip where it runs longer, and in two lines that end in '…' otherwise,
 *  and each claim whole there too, its holder's name wrapping rather than cut (`ClaimChip`'s
 *  `wraps`). Keyed by the registry's own ids, so a typo or a column with no rendering fails to
 *  compile. Everything else about a column — whether it filters, what a row files under, what
 *  it groups by, how wide it starts and how narrow it may go — comes from `COLUMNS` in the
 *  model, so the two never disagree. */
const renderings = (
  userState: UserState | undefined,
  wrapTitles: boolean
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
          <span
            className={`${wrapTitles ? "line-clamp-8" : "line-clamp-2"} min-w-0 flex-1 break-words ${textPrimaryOnCanvas}`}
            title={wrapTitles ? row.original.issue.title : undefined}
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
  status: {
    header: "Status",
    accessorFn: (item) => item.issue.status,
    sortingFn: (a, b) =>
      issueStatuses.indexOf(a.original.issue.status as IssueStatus) -
      issueStatuses.indexOf(b.original.issue.status as IssueStatus),
    cell: ({ getValue }) => {
      const label = statusLabel(getValue<IssueStatus>());
      return (
        <span className="flex min-w-0 [&>span]:min-w-0 [&>span]:max-w-full">
          <StatusPill>
            <TruncatedText className="min-w-0" title={label}>
              {label}
            </TruncatedText>
          </StatusPill>
        </span>
      );
    },
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
    cell: ({ row }) => {
      // A server of the release before the one that added `created_by` sends none, so the cell
      // asks `createdBy`: an absent creator reads as the unknown one rather than reaching
      // `AgentName` as undefined, which would throw and take the whole table with it. Without
      // it, `IssuesTable.test.tsx`'s "a row a server sends without created_by or created_at
      // renders, with no author and no time" fails in `AgentName`.
      const actor = createdBy(row.original.issue);
      return actor === null ? (
        <span className={textSecondaryOnCanvas}>Unknown</span>
      ) : (
        <AgentName actor={actor} />
      );
    },
  },
  labels: {
    header: "Labels",
    accessorFn: (item) => (item.issue.labels ?? []).join(", "),
    cell: ({ row }) => (
      <span className="flex min-w-0 flex-wrap gap-1 [&>span]:min-w-0 [&>span]:max-w-full">
        {(row.original.issue.labels ?? []).map((label) => (
          <LabelPill key={label}>
            <TruncatedText className="min-w-0" title={label}>
              {label}
            </TruncatedText>
          </LabelPill>
        ))}
      </span>
    ),
  },
  claim: {
    header: "Claimed by",
    accessorFn: (item) => item.claimedBy,
    cell: ({ row }) => <ClaimChip claim={row.original.issue.claim} wraps={wrapTitles} />,
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
    accessorFn: (item) => createdAt(item.issue),
    // A server of the release before the one that added `created_at` sends none, and the table
    // sorts such a row last in either direction: TanStack orders an undefined accessor value
    // before it calls `sortingFn` and then reverses that for a descending sort, so a sorting
    // function alone would put those rows first under `?sort=-created`. `priority` and `queue`
    // say the same thing the same way. What `compareTimestamps` then compares is always a time
    // it can parse. `IssuesTable.test.tsx` sorts a row without a time in both directions.
    sortUndefined: "last",
    sortingFn: (a, b) =>
      compareTimestamps(a.original.issue.created_at, b.original.issue.created_at),
    cell: ({ row }) => {
      // The cell is empty for a row that server sent, which carries no time to show, which the
      // same row of `IssuesTable.test.tsx` reads.
      const at = createdAt(row.original.issue);
      return at === undefined ? null : (
        <Timestamp at={at} className={`whitespace-nowrap text-xs ${textMutedOnCanvas}`} />
      );
    },
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

/** What the Advances column knows of one priority by its id: its place in the one order and its
 *  title, each absent for a priority the priorities read does not hold (`PriorityChip`). */
type PriorityOf = (id: string) => { place: number | undefined; title: string | undefined };

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
  readonly rendering: (priorityOf: PriorityOf) => Partial<ColumnDef<Item>>;
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
    // Each priority is its chip and its title after it, wrapped whole: the cell is where a fault
    // row says which priority it stands under, and a phone has no tooltip to read a title in. A
    // layout names its own width where its other columns leave this one more or less room.
    size: 200,
    min: 160,
    rendering: (priorityOf) => ({
      header: "Advances",
      accessorFn: (item) =>
        (item.issue.advances ?? []).map(({ id }) => priorityOf(id).title ?? id).join(", "),
      cell: ({ row }) => (
        <span className="flex flex-col gap-1">
          {(row.original.issue.advances ?? []).map(({ id, inherited }) => (
            <PriorityChip id={id} inherited={inherited} key={id} titled {...priorityOf(id)} />
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

/** A column an issues table can open with shown, or give a width of its own: one of the
 *  registry's (`ColumnId`) or a listing column (`ListingColumnId`). */
export type IssueTableColumnId = ColumnId | ListingColumnId;

/** A column's width where a table's layout names one: where it starts, and the narrowest the
 *  reader may drag it (`size`, `min`); or what it holds (`fit`): as wide as its header or its
 *  widest cell on the page needs on one line, measured as the table draws them, and where `most`
 *  is given no wider than that, past which its cells wrap (`LayoutColumns.fits`) - until it is
 *  measured it stands at `most`, else its own width. Either, given `leaves`, gives Title, the
 *  column that fills the box, room in a desktop's box: where the others would leave Title less
 *  than that, it narrows - toward `min`, or toward the narrowest what it holds needs - to leave
 *  it that much (`LayoutColumns.yields`), each such column in turn in the order the layout names
 *  them. A column of set width that does so draws its header's words wrapped beside its filter
 *  once it is narrower than they are on one line (`DataTable`'s `wrap`), and is never narrower
 *  than its header needs as drawn, sorted or not (`LayoutColumns.wrappedHeaders`). */
export type IssueTableColumnWidth =
  | { readonly size: number; readonly min: number; readonly leaves?: number }
  | { readonly fit: true; readonly most?: number; readonly leaves?: number };

/** The widths a table's layout names, by column: each over the registry's or the listing
 *  column's own. */
export type IssueTableWidths = Readonly<Partial<Record<IssueTableColumnId, IssueTableColumnWidth>>>;

/** The width an issues table draws after its columns, for a row's actions, where its listing can
 *  hold a row they act on (`listsQueued`) and its layout does not leave them out (`rowActions`,
 *  `IssuesTable`). */
export const ROW_ACTIONS_WIDTH = 104;

/** The one column no reader can hide or show (`enableHiding: false`): the row's checkbox, which
 *  `x` selects with. A layout that names what it opens with keeps it whatever it names. */
const ALWAYS_SHOWN: ColumnId = "select";

/** Each layout `issueTableLayout` built, by its listing columns, the columns it opens with (or
 *  none, for a project's opening set), the widths it names, the row's actions' width and the
 *  columns it pins. */
const layouts = new Map<string, LayoutColumns>();

/**
 * The layout of an issues table with the listing columns `columns` (`listingColumns`): the
 * registry's own (`ISSUE_TABLE_COLUMNS`), with those listing columns at their widths. With no
 * `opens` it opens as a project's table always has: the registry's opening set, Updated hidden,
 * and every listing column hidden until the reader shows it. With `opens`, it opens with exactly
 * those columns shown, registry and listing columns alike, and every other one hidden until the
 * reader shows it from the column control; the row's checkbox stays (`ALWAYS_SHOWN`), and Title
 * fills a desktop box (`fills`, `useTableLayout`): it takes the width the other shown columns
 * and the row's actions (`actions`, none where the table draws none) leave.
 * `widths` puts a column's starting width and minimum in place of the registry's or the listing
 * column's, with the room it leaves Title where it names one (`yields`), or makes the column as
 * wide as what it holds (`fits`); and `pins` the columns pinned at the left of a desktop's box in
 * place of the registry's (`LayoutPins.wide`), where the registry's would count a column the
 * table opens hidden. A project's table names none of them; each fault list of the priorities
 * page and a priority's work table name their own. Built once for each set and kept, since the
 * stored layout is read again whenever this object changes (`useTableLayout`).
 */
export function issueTableLayout(
  columns: readonly ListingColumnId[],
  opens?: readonly IssueTableColumnId[],
  widths: IssueTableWidths = {},
  actions = ROW_ACTIONS_WIDTH,
  pins?: readonly IssueTableColumnId[]
): LayoutColumns {
  const name = `${columns.join(" ")}|${opens === undefined ? "*" : opens.join(" ")}|${JSON.stringify(widths)}|${actions}|${pins === undefined ? "*" : pins.join(" ")}`;
  const built = layouts.get(name);
  if (built !== undefined) return built;
  const listing = LISTING_COLUMNS.filter(({ id }) => columns.includes(id));
  const ids = [...ISSUE_TABLE_COLUMNS.ids, ...listing.map(({ id }) => id)];
  const shown = new Set<string>(opens);
  const named = Object.entries(widths).flatMap(([id, width]) =>
    width === undefined ? [] : [[id, width] as const]
  );
  const set = named.flatMap(([id, width]) => ("fit" in width ? [] : [[id, width] as const]));
  const fitted = named.flatMap(([id, width]) => ("fit" in width ? [[id, width] as const] : []));
  const yielding = named.flatMap(([id, { leaves }]) =>
    leaves === undefined ? [] : [[id, leaves] as const]
  );
  // A column of set width that gives way can narrow below its header's words on one line, so its
  // header's words wrap there, and the column is never narrower than its header needs as drawn.
  const wrapped = set.flatMap(([id, { leaves }]) => (leaves === undefined ? [] : [id]));
  const registryPins = ISSUE_TABLE_COLUMNS.pins;
  const layout: LayoutColumns = {
    ...ISSUE_TABLE_COLUMNS,
    ids,
    hiddenByDefault:
      opens === undefined
        ? {
            ...ISSUE_TABLE_COLUMNS.hiddenByDefault,
            ...Object.fromEntries(listing.map(({ id }) => [id, false])),
          }
        : Object.fromEntries(
            ids.filter((id) => id !== ALWAYS_SHOWN && !shown.has(id)).map((id) => [id, false])
          ),
    sizes: {
      ...ISSUE_TABLE_COLUMNS.sizes,
      ...Object.fromEntries(listing.map(({ id, size }) => [id, size])),
      ...Object.fromEntries(set.map(([id, { size }]) => [id, size])),
      ...Object.fromEntries(
        fitted.flatMap(([id, { most }]) => (most === undefined ? [] : [[id, most]]))
      ),
    },
    // A column as wide as what it holds is never narrower than that (`useTableLayout`), and no
    // minimum of its own holds it wider.
    minimums: {
      ...ISSUE_TABLE_COLUMNS.minimums,
      ...Object.fromEntries(listing.map(({ id, min }) => [id, min])),
      ...Object.fromEntries(set.map(([id, { min }]) => [id, min])),
      ...Object.fromEntries(fitted.map(([id]) => [id, 0])),
    },
    ...(fitted.length === 0 ? {} : { fits: Object.fromEntries(fitted) }),
    ...(yielding.length === 0 ? {} : { yields: Object.fromEntries(yielding) }),
    ...(wrapped.length === 0 ? {} : { wrappedHeaders: wrapped }),
    ...(pins === undefined || registryPins === undefined
      ? {}
      : { pins: { ...registryPins, wide: { ...registryPins.wide, columns: pins } } }),
    ...(opens === undefined ? {} : { fills: "title", trailing: actions }),
  };
  layouts.set(name, layout);
  return layout;
}

export function useIssueTableColumns(
  userState: UserState | undefined,
  compact: boolean,
  /** The table's layout (`issueTableLayout`): which listing columns it has, and the narrowest
   *  each column may be drawn at, its `widths` included. */
  layout: LayoutColumns,
  {
    priorityTitles,
    wrapTitles = false,
  }: {
    /** Each priority's title by its id, in the order the priorities read answers them, which is
     *  the one order: the Advances column names each priority by its place in this map (`#3`)
     *  with its title. One this map does not hold, a priority deleted since it was read, is
     *  named by its id. */
    readonly priorityTitles?: ReadonlyMap<string, string>;
    /** Whether each title wraps onto up to eight lines, with the whole of it in its tooltip,
     *  rather than two (`IssuesTable`'s `wrapTitles`). */
    readonly wrapTitles?: boolean;
  }
): ColumnDef<Item>[] {
  return useMemo<ColumnDef<Item>[]>(() => {
    const rendering = renderings(userState, wrapTitles);
    const places = new Map([...(priorityTitles?.keys() ?? [])].map((id, at) => [id, at + 1]));
    const priorityOf: PriorityOf = (id) => ({
      place: places.get(id),
      title: priorityTitles?.get(id),
    });
    const registered = COLUMNS.map((column): ColumnDef<Item> => {
      const picks = column.filter?.kind === "picks" ? column.filter : undefined;
      return {
        ...rendering[column.id],
        id: column.id,
        size: column.size,
        minSize:
          (compact ? COMPACT_COLUMN_MIN[column.id] : undefined) ??
          layout.minimums?.[column.id] ??
          COLUMN_MIN[column.id],
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
          // A column of set width that gives Title its room may narrow below its header's words
          // on one line, so they wrap there beside the filter rather than be cut, the column no
          // narrower than they need (`wrappedHeaders`); where the column is as wide as one line,
          // they stand on one.
          ...(layout.wrappedHeaders?.includes(column.id) === true ? { wrap: true } : {}),
        },
      };
    });
    const listing = LISTING_COLUMNS.filter(({ id }) => layout.ids.includes(id)).map(
      (column): ColumnDef<Item> => ({
        ...column.rendering(priorityOf),
        id: column.id,
        size: column.size,
        minSize: layout.minimums?.[column.id] ?? column.min,
        enableResizing: true,
        enableSorting: false,
        enableColumnFilter: false,
        enableGrouping: false,
      })
    );
    // Beside the title, which is what a reader reads a row of every project's issues by.
    const after = registered.findIndex(({ id }) => id === "title") + 1;
    return [...registered.slice(0, after), ...listing, ...registered.slice(after)];
  }, [userState, compact, layout, priorityTitles, wrapTitles]);
}
