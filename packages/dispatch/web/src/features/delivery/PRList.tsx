import type {
  ColumnDef,
  ColumnFiltersState,
  OnChangeFn,
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
} from "./lib/prList";

/** The facets that are columns of this table, so each is filtered on its own header rather than
 *  from the page's panel (INFRA-3135, R2). The page maps them to and from the column filters and
 *  keeps writing its own parameters: the table filters nothing itself, because what it draws is
 *  the answer Dispatch already filtered (`serverFiltered` below). */
export const COLUMN_FACETS = [
  "issue",
  "parentAgent",
  "author",
  "rework",
  "deployed",
] as const satisfies readonly FacetKey[];

/** The attribute each drawn row carries, which the keys, the anchor and the box's height read,
 *  and the one inside it that opening the row follows. */
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

/** Every column, in the order the table opens with, at the width its longest usual content
 *  takes at `text-xs`: "deployed Sep 26, 10:48 PM", "ACME-1028 · No priority", a parent agent's
 *  title, and a sortable header's label with its arrow and its filter. Title takes the rest. */
const COLUMNS: { id: ColumnId; label: string; size: number; min: number }[] = [
  { id: "color", label: "", size: 24, min: 24 },
  { id: "merged", label: "Merged", size: 112, min: 96 },
  { id: "pr", label: "PR", size: 96, min: 80 },
  { id: "title", label: "Title", size: 320, min: 128 },
  { id: "issue", label: "Issue", size: 176, min: 120 },
  { id: "parentAgent", label: "Parent agent", size: 144, min: 112 },
  { id: "author", label: "Author", size: 120, min: 96 },
  { id: "size", label: "Size", size: 88, min: 72 },
  { id: "deployed", label: "Deployed", size: 176, min: 128 },
  { id: "leadTime", label: "Lead time", size: 104, min: 84 },
  { id: "rework", label: "Rework", size: 96, min: 80 },
];

/** What the layout is read and stored for: every column the table has, the attribute its rows
 *  carry, and how wide and how narrow each column is drawn. */
const PR_LAYOUT_COLUMNS: LayoutColumns = {
  ids: COLUMNS.map((column) => column.id),
  minimums: Object.fromEntries(COLUMNS.map((column) => [column.id, column.min])),
  rowAttribute: PR_ROW_ATTRIBUTE,
  sizes: Object.fromEntries(COLUMNS.map((column) => [column.id, column.size])),
};

/** This list's own words, which the table core reads and never invents. The keys are the ones
 *  the list has always answered: arrow up and down walk the rows, and Enter opens the focused
 *  one. They are registered in this list's own `delivery` scope, which the list pushes while it
 *  is drawn - the List view alone - so no key of this table is answered on a page that does not
 *  draw it, and Enter only while a row itself has focus. Home and End are the caller's, below:
 *  the core's keymap names a next, a previous, an open and a select, and has no slot for them. */
const PR_TABLE_NAMES: DataTableNames = {
  filterCountNoun: { one: "PR", many: "PRs" },
  groupAttribute: "data-pr-group",
  keymap: {
    next: { id: "delivery-next", keys: "ArrowDown", label: "Next pull request" },
    open: { id: "delivery-open", keys: "o", label: "Open the pull request's details" },
    openOnEnter: {
      id: "delivery-open-enter",
      keys: "Enter",
      label: "Open the focused pull request",
    },
    previous: { id: "delivery-previous", keys: "ArrowUp", label: "Previous pull request" },
    scope: "delivery",
    select: { id: "delivery-select", keys: "x", label: "Select or deselect the focused row" },
  },
  linkAttribute: PR_OPEN_ATTRIBUTE,
  rowAttribute: PR_ROW_ATTRIBUTE,
};

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

/** How each column renders, sorts and - for the five that are facets - filters. Rebuilt only
 *  when what a value reads as changes, since a new definition is a new component to React. */
function usePRColumns({
  colorBy,
  colorScale,
  issueTitles,
  onSelect,
  onSelectRun,
  sessionGroups,
}: {
  colorBy: ColorFacet;
  colorScale: Map<string, string>;
  issueTitles: Readonly<Record<string, string>>;
  onSelect: (id: string) => void;
  onSelectRun: (runId: number) => void;
  sessionGroups: readonly DeliverySessionGroup[];
}): ColumnDef<PRListRow>[] {
  return useMemo(() => {
    const show = (value: string, label: string) => PLACEHOLDER_LABELS[value] ?? label;
    const renderings: Record<ColumnId, Partial<ColumnDef<PRListRow>>> = {
      color: {
        enableSorting: false,
        cell: ({ row }) => (
          <span
            aria-hidden="true"
            className="block size-2 rounded-full"
            style={{
              backgroundColor:
                colorScale.get(colorKeyFor(row.original.pr, colorBy)) ?? PLACEHOLDER_COLOR,
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
        // The row's own way in, and what the keys open: `PR_OPEN_ATTRIBUTE` is what the core's
        // keymap clicks for the row in hand, and a pointer press anywhere on the row does the
        // same, so the title is never the only way to the details.
        cell: ({ row }) => (
          <button
            className="w-full truncate text-left"
            data-pr-open=""
            onClick={(event) => {
              event.stopPropagation();
              onSelect(row.original.pr.id);
            }}
            tabIndex={-1}
            title={row.original.pr.title}
            type="button"
          >
            {row.original.pr.title}
          </button>
        ),
      },
      issue: {
        accessorFn: (row) => row.issueSort,
        sortingFn: byIssue,
        sortUndefined: "last",
        meta: { show: (value) => show(value, `${value} \u2014 ${issueTitles[value] ?? ""}`) },
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
        meta: { show: (value) => show(value, parentAgentLabel(value, sessionGroups)) },
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
        meta: { show: (value) => show(value, value) },
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
        meta: {
          always: ["deployed", "waiting", "not_tracked"] satisfies DeployedFacet[],
          order: ["deployed", "waiting", "not_tracked"],
          show: (value) => DEPLOYED_LABELS[value as DeployedFacet] ?? value,
        },
        cell: ({ row }) => <Deployed onSelectRun={onSelectRun} row={row.original} />,
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
        meta: {
          always: ["value", "rework"] satisfies ReworkFacet[],
          order: ["value", "rework"],
          show: (value) => REWORK_LABELS[value as ReworkFacet] ?? value,
        },
        cell: ({ row }) =>
          row.original.pr.rework ? <span className={inlineWarningText}>rework</span> : null,
      },
    };
    return COLUMNS.map(({ id, label, min, size }) => {
      const facet = (COLUMN_FACETS as readonly string[]).includes(id);
      return {
        ...renderings[id],
        header: label,
        id,
        minSize: min,
        size,
        // The list has always offered both directions of a column from its first press, and
        // never no sort at all: the page puts a removed sort back (`DeliveryPage`).
        sortDescFirst: false,
        enableColumnFilter: facet,
        enableGrouping: false,
        ...(facet ? { filterFn: serverFiltered } : {}),
      };
    });
  }, [colorBy, colorScale, issueTitles, onSelect, onSelectRun, sessionGroups]);
}

/**
 * The filtered pull requests as the application's one table (`components/DataTable.tsx`),
 * coloured by the same facet as the timeline; only the rows the box holds are drawn. A row
 * opens the DrillDown, and a deployed row's "deployed" opens its deploy run's. The rows are one
 * keyboard stop: arrow up and down move between them, Home and End jump to the ends, and Enter
 * opens the focused row.
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
    onSelect,
    onSelectRun,
    sessionGroups,
  });
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
    rows,
    state: {
      columnFilters,
      globalFilter: "",
      grouping: [],
      // Every row of the window on one page: the list scrolls, and has never paged.
      pagination: { pageIndex: 0, pageSize: Number.MAX_SAFE_INTEGER },
      sorting,
    },
    viewKey: `${JSON.stringify(sorting)}|${JSON.stringify(columnFilters)}`,
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

  const openFacet = COLUMN_FACETS.find((key) => key === openFilter);
  const counts = useMemo(() => {
    if (openFacet === undefined) return new Map<string, number>();
    const facet = FACET_PARAMS[openFacet];
    return new Map(Object.entries(facetCounts[facet] ?? {}));
  }, [facetCounts, openFacet]);

  return (
    <DataTable
      empty="No PRs in the current filter/window."
      filterCounts={counts}
      label="Merged pull requests"
      model={model}
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
    />
  );
}

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
          className={`overflow-hidden border-b px-1.5 py-2 text-xs whitespace-nowrap ${borderDefault} ${selected ? surfaceMutedBg : surfaceBg}`}
          key={cell.id}
          style={{ width: cell.column.getSize() }}
        >
          <span className="flex min-w-0 items-center">
            {flexRender(cell.column.columnDef.cell, cell.getContext())}
          </span>
        </td>
      ))}
    </tr>
  );
}
