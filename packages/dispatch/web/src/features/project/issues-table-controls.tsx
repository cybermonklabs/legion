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
import { useWidth } from "./useWidth";

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

export function ColumnFilter({
  column,
  counts,
  doneLoaded,
  open,
  onOpenChange,
}: {
  column: Column<Item, unknown>;
  counts: ReadonlyMap<string, number>;
  doneLoaded: boolean;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}): ReactNode {
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

export function ColumnsMenu({ columns }: { columns: Column<Item, unknown>[] }): ReactNode {
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

/** The width a segment keeps on a phone, where a share of the bar would be too narrow to
 *  press: the bar wraps to a second row rather than cut a segment off. */
const SEGMENT_MIN_PX = 88;

/** The width one segment needs to hold a piece of text, in pixels: the glyphs at the bar's
 *  12px type, plus the segment's own padding. Measuring each of fifteen segments on every
 *  render would cost more than it tells; a segment that this call gets wrong by a character
 *  drops to its count, which always fits. */
function textWidth(text: string): number {
  return text.length * 6.6 + 14;
}

/**
 * Each segment's width as a percentage of the bar, from the counts: a share of the issues,
 * floored so that a count of one is still wide enough to press, and then scaled back so the
 * segments together are the full bar and none of it is cut off.
 */
export function segmentWidths(counts: readonly number[], floorPercent = 3): number[] {
  const total = counts.reduce((sum, count) => sum + count, 0);
  if (total <= 0) return counts.map(() => 100 / Math.max(counts.length, 1));
  const floored = counts.map((count) => Math.max(floorPercent, (100 * count) / total));
  const sum = floored.reduce((running, width) => running + width, 0);
  return floored.map((width) => (100 * width) / sum);
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
  const stateCounts = countIssueStates(items);
  const [barRef, barWidth] = useWidth();
  const widths = segmentWidths(stateCounts.map(({ count }) => count));
  // A phone holds each segment to a pressable width and wraps the bar to a second row; a wide
  // page lays every segment on one row, each as wide as its share.
  const compact = barWidth < 640;
  return (
    <div className={`rounded-xl border p-3 ${borderDefault}`}>
      <div
        className={`mb-2 flex flex-wrap items-baseline gap-x-5 gap-y-1 text-sm ${textSecondaryOnCanvas}`}
      >
        {hasLegion && board ? (
          <>
            <span data-legion-slots>
              <b className={`text-lg tabular-nums ${textPrimaryOnCanvas}`}>
                {board.header.slots_in_use}/{board.header.slot_cap ?? "?"}
              </b>{" "}
              slots
            </span>
            {board.header.read_ok && board.header.read_at ? (
              <span data-legion-read-at>
                <InfoHint label="About these counts">
                  Slots counts the workers at work. The second number is the most workers the daemon
                  runs at once. The bar below counts every issue on this page. Each segment is one
                  phase, and its width is its share of the issues. Press a segment to show only that
                  phase. Dispatch read the daemon at{" "}
                  <time dateTime={board.header.read_at}>
                    {new Date(board.header.read_at).toLocaleString()}
                  </time>
                  .
                </InfoHint>
              </span>
            ) : (
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
            )}
          </>
        ) : null}
      </div>
      <fieldset
        aria-label={hasLegion ? "Issues by phase or status" : "Issues by status"}
        className={`flex min-h-11 min-w-0 flex-wrap rounded-md border-0 p-0 md:min-h-7 md:flex-nowrap ${surfaceMutedBg}`}
        ref={barRef}
        role={hasLegion ? "group" : undefined}
      >
        {stateCounts.map(({ id, label, column, count, picks }, index) => {
          const current = (table.getColumn(column)?.getFilterValue() as string[] | undefined) ?? [];
          const on = current.length === 0 || current.includes(id);
          const wanted = hasLegion ? picks : { status: picks.status ?? [id] };
          // Pressed again, the segment lets every state through rather than narrowing further.
          const showsThisAlone =
            current.length === 1 && current[0] === id && (column === "phase") === hasLegion;
          const scale: Readonly<Record<string, string>> =
            column === "phase" ? legionPhaseScale : issueStatusScale;
          const width = widths[index] ?? 0;
          // What the segment is drawn at: its share of the bar, or the pressable floor a phone
          // holds it to. The label is written inside it only where the words fit.
          const drawn = Math.max((barWidth * width) / 100, compact ? SEGMENT_MIN_PX : 0);
          const shown =
            drawn >= textWidth(`${label} ${count}`)
              ? `${label} ${count}`
              : drawn >= textWidth(`${count}`)
                ? `${count}`
                : "";
          return (
            <button
              aria-label={`${label} ${count}`}
              className={`min-h-11 overflow-hidden px-1 py-2 text-left text-xs font-medium whitespace-nowrap md:min-h-7 md:py-1 ${scale[id] ?? ""} ${on ? "" : "opacity-30"}`}
              key={`${column}:${id}`}
              onClick={() =>
                table.setColumnFilters((filters) => {
                  const next = filters.filter((filter) => !["phase", "status"].includes(filter.id));
                  if (showsThisAlone) return next;
                  return [
                    ...next,
                    ...(wanted.phase === undefined
                      ? []
                      : [{ id: "phase", value: [...wanted.phase] }]),
                    ...(wanted.status === undefined
                      ? []
                      : [{ id: "status", value: [...wanted.status] }]),
                  ];
                })
              }
              style={{ width: `${width}%`, minWidth: compact ? SEGMENT_MIN_PX : 0 }}
              title={`${label}: ${count}`}
              type="button"
            >
              {shown}
            </button>
          );
        })}
      </fieldset>
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
        <ColumnsMenu columns={table.getAllLeafColumns()} />
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

export function IssueTablePager({
  table,
  items,
  pagerRef,
}: {
  table: Table<Item>;
  items: readonly Item[];
  pagerRef: RefObject<HTMLDivElement | null>;
}): ReactNode {
  const { pageIndex, pageSize } = table.getState().pagination;
  const { grouping } = table.getState();
  const rowsOnPages = table.getPrePaginationRowModel().rows.length;
  const leafCount = table.getFilteredRowModel().rows.length;
  const groupCount = table.getGroupedRowModel().rows.length;
  return (
    <div
      ref={pagerRef}
      data-testid="issues-table-pager"
      className={`flex shrink-0 flex-wrap items-center gap-3 text-sm ${textSecondaryOnCanvas}`}
    >
      <span>
        {leafCount} of {items.length} {items.length === 1 ? "issue" : "issues"}
        {grouping.length > 0 ? ` in ${groupCount} ${groupCount === 1 ? "group" : "groups"}` : ""}
      </span>
      <span className="ml-auto">
        {pageSize >= ALL_ROWS
          ? `All ${rowsOnPages} ${rowsOnPages === 1 ? "row" : "rows"}`
          : `Rows ${rowsOnPages === 0 ? 0 : pageIndex * pageSize + 1}–${Math.min((pageIndex + 1) * pageSize, rowsOnPages)} of ${rowsOnPages}`}
      </span>
      <Action disabled={!table.getCanPreviousPage()} onClick={() => table.previousPage()}>
        ‹ Previous
      </Action>
      <span>
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
