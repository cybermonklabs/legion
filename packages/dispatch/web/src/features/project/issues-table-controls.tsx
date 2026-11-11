import { SEARCH_QUERY_MAX } from "@legion/contracts/dispatch-tools";
import type { Column, Table } from "@tanstack/react-table";
import { type ReactNode, type RefObject, useCallback, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";
import type { IssueSummary, LegionBoardResponse } from "../../api/types";
import { AnchoredPanel } from "../../components/AnchoredPanel";
import { InfoHint } from "../../components/InfoHint";
import { MultiSelect } from "../../components/MultiSelect";
import {
  borderDefault,
  card,
  checkboxAccent,
  dangerText,
  issueStatusScale,
  legionPhaseScale,
  pickedSegmentRing,
  surfaceBg,
  surfaceMutedBg,
  surfaceMutedHoverBg,
  tableFilterActive,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { IssueFlagChips, UnclaimedUnchecked } from "./IssueFlags";
import { useIssueFilters } from "./issue-filters";
import { columnLabel } from "./issues-table-columns";
import {
  ALL_ROWS,
  countIssueStates,
  defaultView,
  type Item,
  PAGE_SIZES,
  readUrl,
  writeUrl,
} from "./issues-table-model";

function FunnelIcon(): ReactNode {
  return (
    <svg aria-hidden="true" className="size-3.5" fill="currentColor" viewBox="0 0 16 16">
      <title>Filter</title>
      <path d="M1.5 2h13l-5 6.2V13l-3 1.5V8.2z" />
    </svg>
  );
}

const filterTrigger = (active: boolean) =>
  `min-h-11 min-w-11 rounded p-1 md:min-h-8 md:min-w-8 ${active ? tableFilterActive : `${textMutedOnCanvas} ${surfaceMutedHoverBg}`}`;

export function ColumnFilter<Row>({
  column,
  counts,
  doneLoaded: doneLoadedOverride,
  open,
  onOpenChange,
}: {
  column: Column<Row, unknown>;
  counts: ReadonlyMap<string, number>;
  doneLoaded?: boolean;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}): ReactNode {
  const doneLoaded = doneLoadedOverride ?? column.columnDef.meta?.doneLoaded ?? true;
  const meta = column.columnDef.meta;
  const name = columnLabel(column);
  if (meta?.text) {
    const value = (column.getFilterValue() as string | undefined) ?? "";
    return (
      <Popover
        align="right"
        label={<FunnelIcon />}
        labelClassName={filterTrigger(value !== "")}
        open={open}
        onOpenChange={onOpenChange}
        title={`Filter ${name}`}
      >
        {() => (
          <input
            aria-label={`Filter ${name}`}
            className={`m-1 rounded-md border px-2 py-1 text-sm ${borderDefault} ${surfaceBg}`}
            // biome-ignore lint/a11y/noAutofocus: the popover opens to type a filter
            autoFocus
            onChange={(event) => column.setFilterValue(event.target.value || undefined)}
            placeholder={`${name} contains…`}
            value={value}
          />
        )}
      </Popover>
    );
  }
  const picks = (column.getFilterValue() as string[] | undefined) ?? [];
  const order = meta?.order;
  const options = [...new Set([...counts.keys(), ...picks, ...(meta?.always ?? [])])].sort(
    (a, b) =>
      order === undefined
        ? (counts.get(b) ?? 0) - (counts.get(a) ?? 0)
        : order.indexOf(a) - order.indexOf(b)
  );
  const show = meta?.show ?? ((value: string) => value);
  // The one option whose detail is not a count: Done is offered before the closed issues it
  // would count have been read, and says so rather than claiming zero.
  const unloadedDone = (value: string) => column.id === "status" && value === "done" && !doneLoaded;
  return (
    <MultiSelect
      emptyMessage="No matching value."
      label={name}
      onChange={(next) => column.setFilterValue(next.length === 0 ? undefined : next)}
      onOpenChange={onOpenChange}
      open={open}
      optionDetail={(value) => (unloadedDone(value) ? "load" : String(counts.get(value) ?? 0))}
      optionDetailLabel={(value) =>
        unloadedDone(value)
          ? "not loaded"
          : `${counts.get(value) ?? 0} ${(counts.get(value) ?? 0) === 1 ? "issue" : "issues"}`
      }
      optionLabel={show}
      options={options}
      searchLabel={`Search ${name}`}
      selected={picks}
      triggerAriaLabel={
        picks.length === 0 ? `Filter ${name}` : `Filter ${name}: ${picks.length} picked`
      }
      triggerClassName={filterTrigger(picks.length > 0)}
    >
      <FunnelIcon />
    </MultiSelect>
  );
}

/** A button and the panel it opens: the table's own popovers, on the shared `AnchoredPanel`. */
export function Popover({
  label,
  labelClassName,
  title,
  children,
  align = "left",
  open: controlledOpen,
  onOpenChange,
}: {
  label: ReactNode;
  labelClassName?: string;
  title?: string;
  children: (close: () => void) => ReactNode;
  align?: "left" | "right";
  open?: boolean;
  onOpenChange?: (open: boolean) => void;
}): ReactNode {
  const [localOpen, setLocalOpen] = useState(false);
  const open = controlledOpen ?? localOpen;
  const setOpen = useCallback(
    (value: boolean) => {
      setLocalOpen(value);
      onOpenChange?.(value);
    },
    [onOpenChange]
  );
  const trigger = useRef<HTMLButtonElement>(null);

  const close = () => {
    setOpen(false);
    trigger.current?.focus();
  };
  return (
    <>
      <button
        aria-expanded={open}
        aria-haspopup="dialog"
        aria-label={title}
        className={
          labelClassName ??
          `rounded-md border px-2 py-1 text-xs font-normal whitespace-nowrap ${borderDefault} ${surfaceBg} ${textSecondaryOnCanvas}`
        }
        onClick={() => setOpen(!open)}
        ref={trigger}
        title={title}
        type="button"
      >
        {label}
      </button>
      {open ? (
        <AnchoredPanel
          align={align}
          anchorRef={trigger}
          className={`z-50 flex max-h-[min(20rem,calc(100dvh-16px))] w-64 max-w-[calc(100vw-16px)] flex-col overflow-y-auto rounded-lg border p-1 text-sm font-normal shadow-lg ${card}`}
          label={title ?? (typeof label === "string" ? label : "Table actions")}
          onDismiss={() => setOpen(false)}
          role="dialog"
        >
          {children(close)}
        </AnchoredPanel>
      ) : null}
    </>
  );
}

export function ColumnsMenu<Row>({ columns }: { columns: Column<Row, unknown>[] }): ReactNode {
  return (
    <Popover align="right" label="Columns">
      {() =>
        columns
          .filter((column) => column.getCanHide())
          .map((column) => (
            <label
              className={`flex items-center gap-2 rounded px-2 py-1 ${surfaceMutedHoverBg}`}
              key={column.id}
            >
              <input
                checked={column.getIsVisible()}
                className={checkboxAccent}
                onChange={column.getToggleVisibilityHandler()}
                type="checkbox"
              />
              {columnLabel(column)}
            </label>
          ))
      }
    </Popover>
  );
}

export function Action({
  children,
  disabled,
  onClick,
}: {
  children: ReactNode;
  disabled?: boolean;
  onClick: () => void;
}): ReactNode {
  return (
    <button
      className={`rounded-lg border px-3 py-1 text-sm disabled:opacity-40 ${borderDefault} ${surfaceMutedHoverBg}`}
      disabled={disabled}
      onClick={onClick}
      type="button"
    >
      {children}
    </button>
  );
}

/** A state's colour: a Legion phase runs on the phase scale, a Dispatch status on its own. */
const scaleOf = (column: string): Readonly<Record<string, string>> =>
  column === "phase" ? legionPhaseScale : issueStatusScale;

/**
 * Each segment's width as a percentage of the bar, from the counts: its share of the issues,
 * never under `floorPercent` of the bar, so a count of one is still wide enough to press and
 * to read. The segments together are the whole bar, so none of it is cut off: what the floors
 * take, the segments above the floor give, which is why the share each of those keeps is
 * settled by raising one segment to the floor at a time.
 */
export function segmentWidths(counts: readonly number[], floorPercent = 3): number[] {
  const total = counts.reduce((sum, count) => sum + count, 0);
  if (total <= 0) return counts.map(() => 100 / Math.max(counts.length, 1));
  const widths = counts.map((count) => (100 * count) / total);
  const atFloor = new Set<number>();
  for (;;) {
    const room = 100 - atFloor.size * floorPercent;
    const rest = widths.reduce((sum, width, index) => (atFloor.has(index) ? sum : sum + width), 0);
    const under = widths.findIndex(
      (width, index) => !atFloor.has(index) && (rest <= 0 || (room * width) / rest < floorPercent)
    );
    if (under === -1) {
      return widths.map((width, index) =>
        atFloor.has(index) ? floorPercent : rest <= 0 ? room / widths.length : (room * width) / rest
      );
    }
    atFloor.add(under);
  }
}

export function IssueTableStatus({
  table,
  items,
  hasLegion = false,
  board,
}: {
  table: Table<Item>;
  items: readonly Item[];
  hasLegion?: boolean;
  board?: LegionBoardResponse;
}): ReactNode {
  // Only the states with issues behind them: an empty phase is neither a segment nor a legend
  // item. The order is the lifecycle's, from `countIssueStates`.
  const shown = countIssueStates(items, hasLegion).filter(({ count }) => count > 0);
  /** What the bar counts, which is what its title says: every issue in one of its segments. */
  const counted = shown.reduce((sum, { count }) => sum + count, 0);
  const widths = segmentWidths(shown.map(({ count }) => count));
  const filterOf = (column: string) =>
    (table.getColumn(column)?.getFilterValue() as string[] | undefined) ?? [];
  /** This state is among those the table lists: every one, while nothing is picked. */
  const isLit = (column: string, id: string) => {
    const current = filterOf(column);
    return current.length === 0 || current.includes(id);
  };
  /** The table shows this state alone, which is what a second press undoes. */
  const isPicked = (column: string, id: string) => {
    const current = filterOf(column);
    return current.length === 1 && current[0] === id && (column === "phase") === hasLegion;
  };
  /** Show this state alone, or every state again where it is the one already shown. The
   *  segment and its legend item do the same thing. */
  const pick = (column: string, id: string) => {
    const entry = shown.find((state) => state.column === column && state.id === id);
    if (entry === undefined) return;
    const wanted = hasLegion ? entry.picks : { status: entry.picks.status ?? [id] };
    const undo = isPicked(column, id);
    table.setColumnFilters((filters) => {
      const next = filters.filter((filter) => !["phase", "status"].includes(filter.id));
      if (undo) return next;
      return [
        ...next,
        ...(wanted.phase === undefined ? [] : [{ id: "phase", value: [...wanted.phase] }]),
        ...(wanted.status === undefined ? [] : [{ id: "status", value: [...wanted.status] }]),
      ];
    });
  };
  return (
    <div className={`rounded-xl border p-3 ${borderDefault}`}>
      <div
        className={`mb-2 flex flex-wrap items-baseline gap-x-5 gap-y-1 text-sm ${textSecondaryOnCanvas}`}
      >
        {/* What the bar counts, not what the daemon runs: the Legion tab says how many slots
            are in use, and a count of workers beside a bar of issues read as the bar's own. */}
        <span className="flex items-baseline gap-1" data-issues-bar-title>
          <span data-issues-counted>
            <b className={`text-lg tabular-nums ${textPrimaryOnCanvas}`}>
              {counted.toLocaleString("en-US")}
            </b>{" "}
            {counted === 1 ? "issue" : "issues"}
          </span>
          <InfoHint label="About these counts">
            The bar counts every issue on this page once. Each colour is one phase or status, and
            its width is its share. The list under the bar names each colour. Press a colour to show
            only those issues.
            {hasLegion && board?.header.read_ok && board.header.read_at ? (
              <>
                {" "}
                Dispatch read the daemon at{" "}
                <time dateTime={board.header.read_at}>
                  {new Date(board.header.read_at).toLocaleString()}
                </time>
                .
              </>
            ) : null}
          </InfoHint>
        </span>
        {hasLegion && board && !(board.header.read_ok && board.header.read_at) ? (
          <span role="status" className={`text-xs ${dangerText}`} data-legion-read-error>
            {board.header.read_at ? (
              <>
                Stale since{" "}
                <time dateTime={board.header.read_at}>
                  {new Date(board.header.read_at).toLocaleString()}
                </time>
              </>
            ) : (
              "Stale: the daemon has never answered"
            )}
            {board.header.read_error ? (
              <InfoHint label="About the stale data">{board.header.read_error}</InfoHint>
            ) : null}
          </span>
        ) : null}
      </div>
      {/* The bar is colour alone: at fifteen phases a segment is too narrow for a word, and a
          bare count names nothing. The legend under it carries the names, and is the touch
          target for each phase (the page's 44px control minimum is off on the segments). */}
      <fieldset
        aria-label={hasLegion ? "Issues by phase or status" : "Issues by status"}
        className={`flex h-6 min-w-0 overflow-hidden rounded-md border-0 p-0 md:h-5 ${surfaceMutedBg}`}
        role={hasLegion ? "group" : undefined}
      >
        {shown.map(({ id, label, column, count }, index) => (
          <button
            aria-label={`${label} ${count}`}
            className={`h-full p-0 ${scaleOf(column)[id] ?? ""} ${
              isPicked(column, id) ? pickedSegmentRing : isLit(column, id) ? "" : "opacity-30"
            }`}
            key={`${column}:${id}`}
            onClick={() => pick(column, id)}
            style={{ width: `${widths[index] ?? 0}%`, minWidth: 0, minHeight: 0 }}
            title={`${label}: ${count}`}
            type="button"
          />
        ))}
      </fieldset>
      <ul
        aria-label={hasLegion ? "Phases and statuses" : "Statuses"}
        className="mt-1 flex flex-wrap gap-x-1"
        data-issues-legend
      >
        {shown.map(({ id, label, column, count }) => (
          <li key={`${column}:${id}`}>
            <button
              aria-label={`${label}: ${count}`}
              aria-pressed={isPicked(column, id)}
              className={`inline-flex min-h-11 items-center gap-1.5 rounded px-2 text-xs whitespace-nowrap md:min-h-7 ${surfaceMutedHoverBg} ${
                isPicked(column, id)
                  ? `font-semibold ${textPrimaryOnCanvas}`
                  : textSecondaryOnCanvas
              } ${isLit(column, id) ? "" : "opacity-50"}`}
              onClick={() => pick(column, id)}
              type="button"
            >
              <span
                aria-hidden="true"
                className={`size-3 shrink-0 rounded-sm ${scaleOf(column)[id] ?? ""}`}
              />
              {label} <span className="tabular-nums">{count}</span>
            </button>
          </li>
        ))}
      </ul>
    </div>
  );
}

export function IssueTableToolbar({
  table,
  unclaimedUnchecked,
}: {
  table: Table<Item>;
  /** Why the Claimed by filter cannot tell which claims have lapsed, while it is asked to. */
  unclaimedUnchecked: "refilling" | "unreachable" | undefined;
}): ReactNode {
  const flags = useIssueFilters();
  const [, setParams] = useSearchParams();
  const { globalFilter: search, columnFilters: filters, grouping } = table.getState();
  const setSearch = table.setGlobalFilter;
  const setGrouping = table.setGrouping;
  const setExpanded = table.setExpanded;
  return (
    <>
      <div className="flex flex-wrap items-center gap-2">
        <input
          aria-label="Search issues"
          className={`w-full min-w-0 flex-none rounded-lg border px-3 py-1.5 text-sm sm:w-auto sm:min-w-64 sm:flex-1 ${borderDefault} ${surfaceBg} ${textPrimaryOnCanvas}`}
          onChange={(event) => {
            setSearch(event.target.value);
          }}
          placeholder="Search issues"
          type="search"
          value={search}
          maxLength={SEARCH_QUERY_MAX}
        />
        <InfoHint label="About the search">
          The search reads the key, the title, the epic, the assignee, the author and the labels. A
          row must match every word.
        </InfoHint>
        <label className={`flex items-center gap-2 text-sm ${textSecondaryOnCanvas}`}>
          Group by
          <select
            className={`rounded-lg border px-2 py-1.5 ${borderDefault} ${surfaceBg}`}
            onChange={(event) => {
              setGrouping(event.target.value ? [event.target.value] : []);
              setExpanded(true);
            }}
            value={grouping[0] ?? ""}
          >
            <option value="">Nothing</option>
            {table
              .getAllLeafColumns()
              .filter((column) => column.getCanGroup())
              .map((column) => (
                <option key={column.id} value={column.id}>
                  {columnLabel(column)}
                </option>
              ))}
          </select>
        </label>
        <IssueFlagChips />
        {filters.length > 0 || search !== "" || flags.needsYou || flags.unread ? (
          <button
            className={`rounded-lg border px-2 py-1.5 text-sm ${borderDefault} ${surfaceMutedHoverBg}`}
            onClick={() =>
              setParams(
                (current) => {
                  const next = writeUrl(current, {
                    ...readUrl(current),
                    // Back to the view the table opens with, not to no filter at all: an empty
                    // status pick asks the server for every closed issue in the project.
                    filters: defaultView.filters,
                    search: "",
                    page: 0,
                  });
                  for (const flag of ["needs-you", "unread"]) next.delete(flag);
                  return next;
                },
                { replace: true }
              )
            }
            type="button"
          >
            Clear filters
          </button>
        ) : null}
      </div>
      <UnclaimedUnchecked reason={unclaimedUnchecked} />
    </>
  );
}

export function IssueTablePager<Row>({
  table,
  items,
  pagerRef,
}: {
  table: Table<Row>;
  items: readonly Row[];
  pagerRef: RefObject<HTMLDivElement | null>;
}): ReactNode {
  const { pageIndex, pageSize } = table.getState().pagination;
  const { grouping } = table.getState();
  const rowsOnPages = table.getPrePaginationRowModel().rows.length;
  const leafCount = table.getFilteredRowModel().rows.length;
  const groupCount = table.getGroupedRowModel().rows.length;
  // The row wraps inside the table's own box, and nothing in it holds a width the box cannot
  // give: `min-w-0` on the row and on each piece of text, so a phone reads the count, the rows,
  // the pages and the page size on as many lines as they need rather than pushing the page
  // wider than the window.
  return (
    <div
      ref={pagerRef}
      data-testid="issues-table-pager"
      className={`flex min-w-0 grow flex-wrap items-center gap-x-3 gap-y-2 text-sm ${textSecondaryOnCanvas}`}
    >
      <span className="min-w-0">
        {leafCount} of {items.length} {items.length === 1 ? "issue" : "issues"}
        {grouping.length > 0 ? ` in ${groupCount} ${groupCount === 1 ? "group" : "groups"}` : ""}
      </span>
      <span className="ml-auto min-w-0">
        {pageSize >= ALL_ROWS
          ? `All ${rowsOnPages} ${rowsOnPages === 1 ? "row" : "rows"}`
          : `Rows ${rowsOnPages === 0 ? 0 : pageIndex * pageSize + 1}–${Math.min((pageIndex + 1) * pageSize, rowsOnPages)} of ${rowsOnPages}`}
      </span>
      <Action disabled={!table.getCanPreviousPage()} onClick={() => table.previousPage()}>
        ‹ Previous
      </Action>
      <span className="min-w-0">
        Page {pageIndex + 1} of {Math.max(table.getPageCount(), 1)}
      </span>
      <Action disabled={!table.getCanNextPage()} onClick={() => table.nextPage()}>
        Next ›
      </Action>
      <select
        aria-label="Rows per page"
        className={`rounded-lg border px-2 py-1 ${borderDefault} ${surfaceBg}`}
        onChange={(event) => table.setPageSize(Number(event.target.value))}
        value={pageSize}
      >
        {PAGE_SIZES.map((size) => (
          <option key={size} value={size}>
            {size >= ALL_ROWS ? "All rows" : `${size} per page`}
          </option>
        ))}
      </select>
    </div>
  );
}

export function IssueTableSelection({
  table,
  epics,
  busy,
  unansweredEpic,
  startNext,
  moveTo,
  newEpic,
}: {
  table: Table<Item>;
  epics: readonly IssueSummary[];
  busy: boolean;
  unansweredEpic: string | undefined;
  startNext: (keys: readonly string[], confirmFirst?: boolean) => void;
  moveTo: (keys: readonly string[], epic: string | null) => void;
  newEpic: (keys: readonly string[]) => void;
}): ReactNode {
  const selectedKeys = table.getSelectedRowModel().flatRows.map((row) => row.original.issue.key);
  const leafCount = table.getFilteredRowModel().rows.length;
  const setSelection = table.setRowSelection;
  /** The epic the reader has picked to move the selection into; no write has been sent yet. */
  const [epic, setEpic] = useState("");
  return (
    <>
      {/* Over the page, not in it: selecting a row moves nothing else. */}
      {selectedKeys.length === 0 ? null : (
        <div
          className={`fixed inset-x-2 top-14 z-30 mx-auto flex w-fit max-w-[calc(100vw-1rem)] flex-nowrap items-center gap-2 overflow-x-auto whitespace-nowrap rounded-2xl border px-4 py-2 text-sm shadow-xl sm:top-auto sm:bottom-4 [&>*]:shrink-0 ${card} ${textPrimaryOnCanvas}`}
          role="toolbar"
        >
          <span className="mr-1 font-semibold">{selectedKeys.length} selected</span>
          {selectedKeys.length < leafCount ? (
            <Action disabled={busy} onClick={() => table.toggleAllRowsSelected(true)}>
              Select all {leafCount} matching
            </Action>
          ) : null}
          {/* A selection the reader cannot see all of — "Select all N matching" — is asked
              about first: Start next writes once per issue. */}
          <Action
            disabled={busy}
            onClick={() =>
              startNext(selectedKeys, selectedKeys.length > table.getRowModel().rows.length)
            }
          >
            Start next
          </Action>
          {/* Two steps: the select stages an epic and Move commits it. A closed `<select>`
              commits on an arrow key in Chrome on Linux and Windows, and this one reparents
              every selected issue. */}
          <select
            aria-label="Move to epic"
            className={`rounded-lg border px-2 py-1 ${borderDefault} ${surfaceBg}`}
            disabled={busy}
            onChange={(event) => setEpic(event.target.value)}
            value={epic}
          >
            <option value="">Move to epic…</option>
            {epics.map((option) => (
              <option key={option.key} value={option.key}>
                {option.title}
              </option>
            ))}
          </select>
          <Action
            disabled={busy || epic === ""}
            onClick={() => {
              const into = epic;
              setEpic("");
              void moveTo(selectedKeys, into);
            }}
          >
            Move
          </Action>
          <Action disabled={busy} onClick={() => void moveTo(selectedKeys, null)}>
            Take out of epic
          </Action>
          <Action
            disabled={busy || unansweredEpic !== undefined}
            onClick={() => void newEpic(selectedKeys)}
          >
            New epic from these
          </Action>
          <Action disabled={busy} onClick={() => setSelection({})}>
            Clear selection
          </Action>
        </div>
      )}
    </>
  );
}
