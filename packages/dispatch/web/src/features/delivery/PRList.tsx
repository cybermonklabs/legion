import type {
  ColumnDef,
  ColumnFiltersState,
  GroupingState,
  OnChangeFn,
  PaginationState,
  Row,
  SortingFn,
  SortingState,
} from "@tanstack/react-table";
import { type ReactNode, useLayoutEffect, useMemo, useRef, useState } from "react";

import type { DeliveryPR, DeliverySessionGroup } from "../../api/types";
import {
  DataTable,
  type DataTableNames,
  flexRender,
  type RowNoun,
  useDataTable,
} from "../../components/DataTable";
import { type LayoutColumns, useTableLayout } from "../../components/table-layout";
import {
  borderDefault,
  dangerText,
  inlineWarningText,
  linkText,
  successText,
  surfaceBg,
  surfaceMutedBg,
  surfaceMutedHoverBg,
  textMutedOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { useKeymapScope } from "../shell/keymap";
import { type ColorFacet, colorKeyFor, PLACEHOLDER_COLOR } from "./lib/colorScale";
import {
  DEPLOYED_LABELS,
  type DeployedFacet,
  FACET_PARAMS,
  type FacetKey,
  ownEntry,
  PLACEHOLDER_LABELS,
  REWORK_LABELS,
  type ReworkFacet,
} from "./lib/facets";
import { parentAgentLabel } from "./lib/parentAgent";
import {
  buildPRListRows,
  compareIssueSort,
  formatMinutes,
  type IssueSortKey,
  type PRListRow,
  ROW_HEIGHT_PX,
} from "./lib/prList";

/** The facets that are columns of this table, so in the List view each is filtered on its own
 *  header rather than from the page's panel. The page maps them to and from the
 *  column filters and keeps writing its own parameters: the table filters nothing itself,
 *  because what it draws is the answer Dispatch already filtered (`serverFiltered` below). */
export const COLUMN_FACETS = [
  "issue",
  "parentAgent",
  "author",
  "rework",
  "deployed",
] as const satisfies readonly FacetKey[];

export type ColumnFacet = (typeof COLUMN_FACETS)[number];

/** Whether a facet, a column or an open filter is one of the column facets. */
export function isColumnFacet(key: string | undefined): key is ColumnFacet {
  return COLUMN_FACETS.some((one) => one === key);
}

/** The values a column facet offers whatever the window holds, in its own order: every verdict
 *  a pull request can have. The other three list what the window counts, busiest first. */
export const FIXED_FACET_VALUES: Partial<Record<ColumnFacet, readonly string[]>> = {
  deployed: ["deployed", "waiting", "not_tracked"] satisfies DeployedFacet[],
  rework: ["value", "rework"] satisfies ReworkFacet[],
};

/** What a column facet's value reads as - the issue's title, a session group's name, a
 *  placeholder in words - on its header's filter and wherever else the page names it. Every
 *  table is read by its own keys alone (`ownEntry`), since the value can be any word the address
 *  names. */
export function columnFacetValue(
  key: ColumnFacet,
  value: string,
  {
    issueTitles,
    sessionGroups,
  }: {
    readonly issueTitles: Readonly<Record<string, string>>;
    readonly sessionGroups: readonly DeliverySessionGroup[];
  }
): string {
  switch (key) {
    case "issue":
      return (
        ownEntry(PLACEHOLDER_LABELS, value) ??
        `${value} \u2014 ${ownEntry(issueTitles, value) ?? ""}`
      );
    case "parentAgent":
      return parentAgentLabel(value, sessionGroups);
    case "author":
      return ownEntry(PLACEHOLDER_LABELS, value) ?? value;
    case "rework":
      return ownEntry(REWORK_LABELS, value) ?? value;
    case "deployed":
      return ownEntry(DEPLOYED_LABELS, value) ?? value;
  }
}

/** The attribute each drawn row carries, which the keys, the anchor and the box's height read,
 *  and the one on its plain title that `o` and Enter press to open the row. */
const PR_ROW_ATTRIBUTE = "data-pr-row";
const PR_OPEN_ATTRIBUTE = "data-pr-open";

/** The reader's own widths, order and visibility for this table, kept in their browser. */
const PR_LAYOUT_KEY = "dispatch.delivery-prs";

type ColumnId =
  | "color"
  | "merged"
  | "pr"
  | "title"
  | "issue"
  | "parentAgent"
  | "author"
  | "size"
  | "deployed"
  | "leadTime"
  | "rework";

/** Every column, in the order the table opens with, at the width the list drew it before the
 *  shared table: each fits its longest usual content at `text-xs`, "deployed
 *  Sep 26, 10:48 PM", "ACME-1028 · No priority", a parent agent's title, and a sortable header's
 *  label with its arrow. Title is the one the table fills its box with (`fillColumn`): never
 *  narrower than its 128 px, it takes whatever the box has over the others, so the columns come
 *  to 1152 px in a narrower box and to the box's width in a wider one. Every column but the
 *  colour dot sorts. */
const COLUMNS: { id: ColumnId; label: string; size: number; min: number; sortable?: false }[] = [
  { id: "color", label: "", size: 20, min: 20, sortable: false },
  { id: "merged", label: "Merged", size: 112, min: 96 },
  { id: "pr", label: "PR", size: 96, min: 80 },
  { id: "title", label: "Title", size: 128, min: 128 },
  { id: "issue", label: "Issue", size: 160, min: 120 },
  { id: "parentAgent", label: "Parent agent", size: 112, min: 112 },
  { id: "author", label: "Author", size: 116, min: 96 },
  { id: "size", label: "Size", size: 88, min: 72 },
  { id: "deployed", label: "Deployed", size: 164, min: 128 },
  { id: "leadTime", label: "Lead time", size: 84, min: 84 },
  { id: "rework", label: "Rework", size: 72, min: 72 },
];

/** The columns the list can be sorted by, which `?sort=` may name: an id that is none of them
 *  is no sort the table can draw. */
export const SORTABLE_COLUMNS: readonly string[] = COLUMNS.filter(
  (column) => column.sortable !== false
).map((column) => column.id);

/** A column facet's header, which names the facet wherever else the page draws it. */
export function columnHeader(key: ColumnFacet): string {
  return COLUMNS.find((column) => column.id === key)?.label ?? key;
}

/** What the layout is read and stored for: every column the table has, the attribute its rows
 *  carry, and how wide and how narrow each column is drawn. */
const PR_LAYOUT_COLUMNS: LayoutColumns = {
  ids: COLUMNS.map((column) => column.id),
  minimums: Object.fromEntries(COLUMNS.map((column) => [column.id, column.min])),
  rowAttribute: PR_ROW_ATTRIBUTE,
  sizes: Object.fromEntries(COLUMNS.map((column) => [column.id, column.size])),
};

/** This list's own words, which the table core reads and never invents. The keys are the ones
 *  the list has always answered, and only while a row holds the focus (`moveFrom: "row"`):
 *  arrow up and down walk the rows, and Enter opens the focused one. Anywhere else on the page
 *  the arrows are the page's, which scrolls. They are registered in this list's own `delivery`
 *  scope, which the list pushes while it is drawn - the List view alone - so no key of this
 *  table is answered on a page that does not draw it. The list selects nothing, so it names no
 *  select key. Home and End are the row's own, below: the core's keymap names a next, a
 *  previous and an open, and has no slot for them. */
const PR_TABLE_NAMES: DataTableNames = {
  groupAttribute: "data-pr-group",
  keymap: {
    moveFrom: "row",
    next: { id: "delivery-next", keys: "ArrowDown", label: "Next pull request" },
    open: { id: "delivery-open", keys: "o", label: "Open the pull request's details" },
    openOnEnter: {
      id: "delivery-open-enter",
      keys: "Enter",
      label: "Open the focused pull request",
    },
    previous: { id: "delivery-previous", keys: "ArrowUp", label: "Previous pull request" },
    scope: "delivery",
  },
  linkAttribute: PR_OPEN_ATTRIBUTE,
  rowAttribute: PR_ROW_ATTRIBUTE,
};

/** What this list's rows are, which a header filter counts in. The list groups nothing and
 *  pages nothing, so its group and line words are never drawn; they are given because every
 *  table names all of its own words. */
const PR_NOUN: RowNoun = {
  group: { one: "group", many: "groups" },
  line: { one: "row", many: "rows" },
  many: "PRs",
  one: "PR",
};

/** This table groups nothing and pages nothing: every row of the window is on one page, and the
 *  list scrolls. Both are module constants rather than literals in the state below, so the
 *  state object the virtualizer's every scroll notification re-renders carries the same two
 *  values it did before - a fresh array there re-runs a row model over every row. */
const NO_GROUPING: GroupingState = [];
const ONE_PAGE: PaginationState = { pageIndex: 0, pageSize: Number.MAX_SAFE_INTEGER };

const DEPLOYED_TONE: Record<DeliveryPR["deployed_status"], string> = {
  deployed: successText,
  waiting: inlineWarningText,
  not_tracked: textMutedOnSurface,
};

const dateTime = new Intl.DateTimeFormat(undefined, {
  month: "short",
  day: "numeric",
  hour: "numeric",
  minute: "2-digit",
});

/** Dispatch applies every facet and the search, so the rows handed here are already the ones
 *  the reader asked for: a column filter here says what is picked and writes the page's own
 *  parameter, and keeps every row the answer holds. */
const serverFiltered = () => true;

/** Text with numeric runs compared as numbers, so "widgets#9" sorts before "widgets#10". */
const byText: SortingFn<PRListRow> = (a, b, id) =>
  String(a.getValue(id)).localeCompare(String(b.getValue(id)), undefined, { numeric: true });

/** A number, which is every other sortable value here: a time, a size, a lead time, a rework
 *  flag. A row with no value for the column sorts last in either direction (`sortUndefined`). */
const byNumber: SortingFn<PRListRow> = (a, b, id) =>
  Number(a.getValue(id)) - Number(b.getValue(id));

/** By the issue's priority, then its Dispatch key (`compareIssueSort`). */
const byIssue: SortingFn<PRListRow> = (a, b, id) =>
  compareIssueSort(a.getValue<IssueSortKey>(id), b.getValue<IssueSortKey>(id));

function Deployed({
  onSelectRun,
  row,
}: {
  onSelectRun: (runId: number) => void;
  row: PRListRow;
}): ReactNode {
  const runId = row.pr.deploy_run;
  const label = <span className={DEPLOYED_TONE[row.pr.deployed_status]}>{row.deployedLabel}</span>;
  return (
    <span className="truncate">
      {runId === null ? (
        label
      ) : (
        // The list's way to a deploy's drill-down, which the chart offers only to a pointer.
        <button
          aria-label={`Open deploy run #${runId}`}
          className="underline"
          onClick={(event) => {
            event.stopPropagation();
            onSelectRun(runId);
          }}
          type="button"
        >
          {label}
        </button>
      )}
      {row.deployedMs === undefined ? null : (
        <span className={textSecondaryOnSurface}> {dateTime.format(row.deployedMs)}</span>
      )}
    </span>
  );
}

/** What a cell reads that changes with every live answer: the colours of the window, the issue
 *  titles, the session groups and the page's handler for a deploy run. */
interface PRCellInputs {
  readonly colorBy: ColorFacet;
  readonly colorScale: Map<string, string>;
  readonly issueTitles: Readonly<Record<string, string>>;
  readonly onSelectRun: (runId: number) => void;
  readonly sessionGroups: readonly DeliverySessionGroup[];
}

/**
 * How each column renders, sorts and - for the five that are facets - filters.
 *
 * Built once and never rebuilt. A column definition's `cell` is a component type to React
 * (`flexRender` calls `createElement` with it), so a new definition unmounts and remounts
 * everything inside every cell. This page takes a live answer every second or two, each with a
 * new colour scale and new issue titles, and a remount in the middle of one takes the focused
 * control out of the document: the drill-down dialog then has nothing to hand focus back to
 * and drops the reader on the body. So a definition closes over a ref rather than over the
 * values, and each cell reads what the page holds now at the moment it draws.
 */
function usePRColumns(inputs: PRCellInputs): ColumnDef<PRListRow>[] {
  const live = useRef(inputs);
  live.current = inputs;
  return useMemo(() => {
    const renderings: Record<ColumnId, Partial<ColumnDef<PRListRow>>> = {
      color: {
        cell: ({ row }) => (
          <span
            aria-hidden="true"
            className="block size-2 rounded-full"
            style={{
              backgroundColor:
                live.current.colorScale.get(colorKeyFor(row.original.pr, live.current.colorBy)) ??
                PLACEHOLDER_COLOR,
            }}
          />
        ),
      },
      merged: {
        accessorFn: (row) => row.mergedMs,
        sortingFn: byNumber,
        sortUndefined: "last",
        cell: ({ row }) =>
          row.original.mergedMs === undefined ? null : (
            <span title={new Date(row.original.mergedMs).toLocaleString()}>
              {dateTime.format(row.original.mergedMs)}
            </span>
          ),
      },
      pr: {
        accessorFn: (row) => row.prLabel,
        sortingFn: byText,
        cell: ({ row }) => (
          <a
            className={`truncate hover:underline ${linkText}`}
            href={row.original.pr.url}
            onClick={(event) => event.stopPropagation()}
            rel="noreferrer"
            target="_blank"
            title={row.original.pr.id}
          >
            {row.original.prLabel}
          </a>
        ),
      },
      title: {
        accessorFn: (row) => row.pr.title,
        sortingFn: byText,
        // Plain text, as the list drew it before the shared table, and what `o`
        // and Enter press for the row in hand: the core's keymap clicks `PR_OPEN_ATTRIBUTE`. It
        // is no control and no stop for the focus; the press goes up to the row's own, which
        // opens the details as a pointer press anywhere on the row does.
        cell: ({ row }) => (
          <span className="truncate" data-pr-open="" title={row.original.pr.title}>
            {row.original.pr.title}
          </span>
        ),
      },
      issue: {
        accessorFn: (row) => row.issueSort,
        sortingFn: byIssue,
        sortUndefined: "last",
        cell: ({ row }) => (
          <span
            className={`truncate ${row.original.issueSort === undefined ? textMutedOnSurface : ""}`}
          >
            {row.original.issueLabel}
          </span>
        ),
      },
      parentAgent: {
        accessorFn: (row) => row.parentAgentSort,
        sortingFn: byText,
        sortUndefined: "last",
        cell: ({ row }) => (
          <span
            className={`truncate ${row.original.parentAgentSort === undefined ? textMutedOnSurface : ""}`}
            title={row.original.parentAgentLabel}
          >
            {row.original.parentAgentLabel}
          </span>
        ),
      },
      author: {
        accessorFn: (row) => row.pr.author,
        sortingFn: byText,
        cell: ({ row }) => <span className="truncate">{row.original.pr.author}</span>,
      },
      size: {
        accessorFn: (row) => row.size,
        sortingFn: byNumber,
        sortUndefined: "last",
        cell: ({ row }) =>
          row.original.pr.additions === null || row.original.pr.deletions === null ? null : (
            <span className="tabular-nums">
              <span className={successText}>+{row.original.pr.additions}</span>{" "}
              <span className={dangerText}>-{row.original.pr.deletions}</span>
            </span>
          ),
      },
      deployed: {
        accessorFn: (row) => row.deployedMs,
        sortingFn: byNumber,
        sortUndefined: "last",
        cell: ({ row }) => <Deployed onSelectRun={live.current.onSelectRun} row={row.original} />,
      },
      leadTime: {
        accessorFn: (row) => row.leadTimeMinutes,
        sortingFn: byNumber,
        sortUndefined: "last",
        cell: ({ row }) =>
          row.original.leadTimeMinutes === undefined ? null : (
            <span className="tabular-nums" title="Merge to production">
              {formatMinutes(row.original.leadTimeMinutes)}
            </span>
          ),
      },
      rework: {
        accessorFn: (row) => Number(row.pr.rework),
        sortingFn: byNumber,
        cell: ({ row }) =>
          row.original.pr.rework ? <span className={inlineWarningText}>rework</span> : null,
      },
    };
    return COLUMNS.map(({ id, label, min, size, sortable }) => {
      const facet = isColumnFacet(id) ? id : undefined;
      const fixed = facet === undefined ? undefined : FIXED_FACET_VALUES[facet];
      return {
        ...renderings[id],
        header: label,
        id,
        minSize: min,
        size,
        enableSorting: sortable !== false,
        // The list has always offered both directions of a column from its first press, and
        // never no sort at all: the page puts a removed sort back (`DeliveryPage`).
        sortDescFirst: false,
        enableColumnFilter: facet !== undefined,
        enableGrouping: false,
        ...(facet === undefined
          ? {}
          : {
              filterFn: serverFiltered,
              meta: {
                show: (value: string) => columnFacetValue(facet, value, live.current),
                ...(fixed === undefined ? {} : { always: fixed, order: fixed }),
              },
            }),
      };
    });
    // No dependency, deliberately: every value a cell reads comes from `live` at draw time, and
    // a new definition would remount every cell (see this hook's own comment).
  }, []);
}

/**
 * The filtered pull requests as the application's one table (`components/DataTable.tsx`),
 * coloured by the same facet as the timeline; only the rows the box holds are drawn. A row
 * opens the DrillDown, and a deployed row's "deployed" opens its deploy run's. No row is a Tab
 * stop (each is `tabIndex={-1}`): a row takes the focus from a press on it, and Tab reaches the
 * links inside a row. While a row holds the focus, arrow up and down move between the rows,
 * Home and End jump to the ends, and Enter on the row opens it; with the focus anywhere else
 * those keys are the page's own, and the arrows scroll it.
 *
 * Nothing here narrows or counts anything itself: Dispatch answers with the filtered rows and
 * with every facet's counts, and the page hands both in, so a header's filter writes the page's
 * own parameter and reads the server's count (R2 and decision 7).
 */
export function PRList({
  allRepos,
  colorBy,
  colorScale,
  columnFilters,
  counted,
  facetCounts,
  issueTitles,
  onColumnFiltersChange,
  onSelect,
  onSelectRun,
  onSortingChange,
  prs,
  selectedId,
  sessionGroups,
  sorting,
}: {
  prs: readonly DeliveryPR[];
  allRepos: readonly string[];
  colorBy: ColorFacet;
  colorScale: Map<string, string>;
  /** The picked values of the five facets that are columns, as TanStack holds them. */
  columnFilters: ColumnFiltersState;
  /** Whether `facetCounts` is the answer for the window, filters and search the address names
   *  (`DeliveryPage`'s `counted`): while it is not, a header's filter draws no count beside a
   *  value, since the counts on screen are the previous filter's. */
  counted: boolean;
  /** Every facet's counts as Dispatch answered them, by the server's own facet name. */
  facetCounts: Readonly<Record<string, Readonly<Record<string, number>>>>;
  issueTitles: Readonly<Record<string, string>>;
  onColumnFiltersChange: OnChangeFn<ColumnFiltersState>;
  onSelect: (id: string) => void;
  onSelectRun: (runId: number) => void;
  onSortingChange: OnChangeFn<SortingState>;
  selectedId: string | undefined;
  sessionGroups: readonly DeliverySessionGroup[];
  sorting: SortingState;
}): ReactNode {
  // The list's own keys are answered while the list is drawn and nowhere else, so the List
  // view's arrows do not reach a reader on the Timeline.
  useKeymapScope("delivery");
  const layout = useTableLayout(PR_LAYOUT_KEY, PR_LAYOUT_COLUMNS);
  const [openFilter, setOpenFilter] = useState<string>();
  const rows = useMemo(
    () => buildPRListRows(prs, allRepos, sessionGroups),
    [prs, allRepos, sessionGroups]
  );
  const columns = usePRColumns({
    colorBy,
    colorScale,
    issueTitles,
    onSelectRun,
    sessionGroups,
  });
  // The view the table renders and the key that names it, built together and only when the sort
  // or a filter moves: the virtualizer re-renders this host on every scroll notification, and a
  // state object or a key rebuilt there would re-run a row model over every row per frame.
  const { state, viewKey } = useMemo(
    () => ({
      state: {
        columnFilters,
        globalFilter: "",
        grouping: NO_GROUPING,
        pagination: ONE_PAGE,
        sorting,
      },
      viewKey: `${JSON.stringify(sorting)}|${JSON.stringify(columnFilters)}`,
    }),
    [columnFilters, sorting]
  );
  const model = useDataTable<PRListRow>({
    columns,
    getRowId: (row) => row.pr.id,
    layout,
    names: PR_TABLE_NAMES,
    onColumnFiltersChange,
    // The page's search box is Dispatch's own `q`, applied where the rows are read, so the
    // table has no search of its own to answer.
    onGlobalFilterChange: () => {},
    onGroupingChange: () => {},
    onPaginationChange: () => {},
    onSortingChange,
    // Every row is drawn at the one height (`PRRow`), so the box scrolls the list's whole
    // height before a row is measured, as it did before the shared table.
    rowHeight: ROW_HEIGHT_PX,
    rows,
    state,
    viewKey,
  });
  /** A row Home or End asked for, until the render that draws it puts focus there: the row may
   *  be outside the drawn window, and the virtualizer scrolls to it first. */
  const jumpTo = useRef<string | null>(null);
  const rowElement = (id: string) =>
    layout.scrollRef.current?.querySelector<HTMLElement>(
      `[${PR_ROW_ATTRIBUTE}="${CSS.escape(id)}"]`
    ) ?? null;
  useLayoutEffect(() => {
    const id = jumpTo.current;
    if (id === null) return;
    const element = rowElement(id);
    if (element === null) return;
    jumpTo.current = null;
    element.focus({ preventScroll: true });
  });
  // Home and End, the two keys the core's keymap has no slot for, answered by the row the
  // caller draws, as this list has always answered them.
  const jump = (end: "first" | "last") => {
    const lines = model.displayRows;
    const line = end === "first" ? lines[0] : lines.at(-1);
    if (line === undefined) return;
    jumpTo.current = line.row.id;
    model.virtualizer.scrollToIndex(end === "first" ? 0 : lines.length - 1, { align: "auto" });
    const element = rowElement(line.row.id);
    if (element !== null) {
      jumpTo.current = null;
      element.focus();
    }
  };

  const openFacet = isColumnFacet(openFilter) ? openFilter : undefined;
  const counts = useMemo(() => {
    if (openFacet === undefined) return new Map<string, number>();
    const facet = FACET_PARAMS[openFacet];
    return new Map(Object.entries(facetCounts[facet] ?? {}));
  }, [facetCounts, openFacet]);

  return (
    <DataTable
      empty="No PRs in the current filter/window."
      fillColumn="title"
      filterCounted={counted}
      filterCounts={counts}
      label="Merged pull requests"
      model={model}
      noun={PR_NOUN}
      onOpenFilterChange={setOpenFilter}
      openFilter={openFilter}
      renderGroup={() => null}
      renderRow={({ index, measure, row }) => (
        <PRRow
          index={index}
          measure={measure}
          onJump={jump}
          onSelect={onSelect}
          row={row}
          selected={row.original.pr.id === selectedId}
        />
      )}
      scrollTestId="delivery-pr-list"
      size="compact"
      stackColumn="title"
    />
  );
}

/** Each cell's box: the 36 px row the list has always drawn (`ROW_HEIGHT_PX`), less the 1 px
 *  rule under it, at every width. A row is as tall as its tallest cell, and under 1280 px a link
 *  or a button is 44 px tall (`styles.css`): in a box of the row's own height it overflows the
 *  cell, as it overflowed the row before the shared table, rather than make the row taller. */
const CELL_BOX = { height: ROW_HEIGHT_PX - 1 } as const;

/** One pull request's row: its cells, the press that opens its details, and the two keys the
 *  list answers itself. */
function PRRow({
  index,
  measure,
  onJump,
  onSelect,
  row,
  selected,
}: {
  index: number;
  measure: (element: Element | null) => void;
  onJump: (end: "first" | "last") => void;
  onSelect: (id: string) => void;
  row: Row<PRListRow>;
  selected: boolean;
}): ReactNode {
  return (
    <tr
      aria-selected={selected}
      className={`cursor-pointer outline-none ${selected ? surfaceMutedBg : surfaceMutedHoverBg}`}
      data-index={index}
      data-pr-row={row.original.pr.id}
      onClick={() => onSelect(row.original.pr.id)}
      onKeyDown={(event) => {
        if (event.key !== "Home" && event.key !== "End") return;
        event.preventDefault();
        onJump(event.key === "Home" ? "first" : "last");
      }}
      ref={measure}
      tabIndex={-1}
    >
      {row.getVisibleCells().map((cell) => (
        <td
          className={`overflow-hidden border-b px-1.5 py-0 text-xs whitespace-nowrap ${borderDefault} ${selected ? surfaceMutedBg : surfaceBg}`}
          key={cell.id}
          style={{ width: cell.column.getSize() }}
        >
          <span className="flex min-w-0 items-center" style={CELL_BOX}>
            {flexRender(cell.column.columnDef.cell, cell.getContext())}
          </span>
        </td>
      ))}
    </tr>
  );
}
