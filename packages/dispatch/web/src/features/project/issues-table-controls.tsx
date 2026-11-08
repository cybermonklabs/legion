import { SEARCH_QUERY_MAX } from "@legion/contracts/dispatch-tools";
import type { Column, Row, Table } from "@tanstack/react-table";
import {
  type ReactNode,
  type RefObject,
  useCallback,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
} from "react";
import { createPortal } from "react-dom";
import { Link, useSearchParams } from "react-router-dom";
import { AGENT_LIST_REFILLING } from "../../api/client";
import type { IssueSummary } from "../../api/types";
import { Chip } from "../../components/Chip";
import { MultiSelect } from "../../components/MultiSelect";
import {
  borderDefault,
  card,
  checkboxAccent,
  issueStatusScale,
  linkHoverText,
  linkText,
  surfaceBg,
  surfaceMutedBg,
  surfaceMutedHoverBg,
  tableFilterActive,
  textMutedOnCanvas,
  textMutedOnSurfaceMuted,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { buildIssuePath } from "../refs/routes";
import { type IssueStatus, issueStatuses, statusLabel } from "./board-model";
import { useIssueFilters } from "./issue-filters";
import { ALL_ROWS, type Item, priorityName, readUrl, writeUrl } from "./issues-table-model";

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
  const name = String(column.columnDef.header);
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
  const options = [
    ...new Set([...counts.keys(), ...picks, ...(column.id === "status" ? issueStatuses : [])]),
  ].sort((a, b) =>
    order === undefined
      ? (counts.get(b) ?? 0) - (counts.get(a) ?? 0)
      : order.indexOf(a) - order.indexOf(b)
  );
  const show = meta?.show ?? ((value: string) => value);
  return (
    <MultiSelect
      emptyMessage="No matching value."
      label={name}
      onChange={(next) => column.setFilterValue(next.length === 0 ? undefined : next)}
      onOpenChange={onOpenChange}
      open={open}
      optionDetail={(value) =>
        column.id === "status" && value === "done" && !doneLoaded
          ? "load"
          : String(counts.get(value) ?? 0)
      }
      optionDetailLabel={(value) =>
        column.id === "status" && value === "done" && !doneLoaded
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

/** A group's header row: its value, how many issues, its issues by status, and for an epic its
 *  actions. */
export function GroupRow({
  row,
  grouping,
  columns,
  epics,
  onRename,
  onMerge,
  onStartAll,
  continued,
  index,
  measure,
  viewportWidth,
}: {
  /** A repeat of the header of a group the page opens in the middle of. */
  continued?: boolean;
  index: number;
  measure: (element: Element | null) => void;
  row: Row<Item>;
  grouping: string;
  columns: number;
  viewportWidth: number;
  epics: readonly IssueSummary[];
  onRename: (key: string) => void;
  onMerge: (from: string, into: string) => void;
  onStartAll: (epic: string) => void;
}): ReactNode {
  const leaves = row.getLeafRows().filter((leaf) => !leaf.getIsGrouped());
  const value = String(row.groupingValue ?? "");
  const shown =
    grouping === "status"
      ? statusLabel(value as IssueStatus)
      : grouping === "priority"
        ? priorityName(value)
        : value;
  const epicKey = grouping === "epic" ? leaves[0]?.original.epicKey : undefined;
  const byStatus = issueStatuses
    .map(
      (status) =>
        [status, leaves.filter((leaf) => leaf.original.issue.status === status).length] as const
    )
    .filter(([, count]) => count > 0);
  const next = Math.min(...leaves.map((leaf) => leaf.original.queue ?? Number.POSITIVE_INFINITY));
  return (
    <tr className={surfaceMutedBg} data-index={index} ref={measure}>
      <td className="px-2 py-2" colSpan={columns}>
        <div
          className="sticky left-0 flex flex-wrap items-center gap-x-4 gap-y-1"
          style={{ width: Math.max(180, viewportWidth - 20) }}
        >
          <button
            aria-expanded={row.getIsExpanded()}
            aria-label={`${row.getIsExpanded() ? "Collapse" : "Expand"} ${shown}, ${leaves.length} ${leaves.length === 1 ? "issue" : "issues"}${continued ? ", continued" : ""}`}
            className={`flex min-w-0 items-center gap-2 text-left font-semibold ${textPrimaryOnCanvas}`}
            onClick={row.getToggleExpandedHandler()}
            type="button"
          >
            <span className={`w-4 ${textMutedOnSurfaceMuted}`}>
              {row.getIsExpanded() ? "▾" : "▸"}
            </span>
            {epicKey === undefined ? shown : null}
            {continued ? (
              <span className={`font-normal ${textMutedOnSurfaceMuted}`}>(continued)</span>
            ) : null}
            <span className={`font-normal ${textMutedOnSurfaceMuted}`}>
              {leaves.length} {leaves.length === 1 ? "issue" : "issues"}
              {Number.isFinite(next) ? ` · next in queue #${next}` : ""}
            </span>
          </button>
          {epicKey === undefined ? null : (
            <Link
              className={`font-semibold ${linkText} ${linkHoverText}`}
              to={buildIssuePath({ key: epicKey, kind: "issue" })}
            >
              {shown}
            </Link>
          )}
          <span className="flex items-center gap-2">
            <span
              aria-hidden
              className={`flex h-2 w-32 overflow-hidden rounded-full ${surfaceMutedBg}`}
            >
              {byStatus.map(([status, count]) => (
                <span className={issueStatusScale[status]} key={status} style={{ flex: count }} />
              ))}
            </span>
            <span className={`text-xs ${textMutedOnSurfaceMuted}`}>
              {byStatus
                .map(([status, count]) => `${count} ${statusLabel(status).toLowerCase()}`)
                .join(" · ")}
            </span>
          </span>
          {epicKey === undefined ? null : (
            <EpicActions
              epicKey={epicKey}
              epics={epics}
              onMerge={(into) => onMerge(epicKey, into)}
              onRename={() => onRename(epicKey)}
              onStartAll={() => onStartAll(epicKey)}
            />
          )}
        </div>
      </td>
    </tr>
  );
}

function Popover({
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
  const panel = useRef<HTMLDivElement>(null);
  const [position, setPosition] = useState({ top: 0, left: 0 });
  useLayoutEffect(() => {
    if (!open) return;
    const place = () => {
      const box = trigger.current?.getBoundingClientRect();
      if (!box) return;
      const width = Math.min(256, window.innerWidth - 16);
      const left = Math.max(
        8,
        Math.min(align === "right" ? box.right - width : box.left, window.innerWidth - width - 8)
      );
      const top = Math.max(
        8,
        Math.min(box.bottom + 4, window.innerHeight - (panel.current?.offsetHeight ?? 200) - 8)
      );
      setPosition((current) =>
        current.left === left && current.top === top ? current : { left, top }
      );
    };
    place();
    window.addEventListener("resize", place);
    window.addEventListener("scroll", place, true);
    return () => {
      window.removeEventListener("resize", place);
      window.removeEventListener("scroll", place, true);
    };
  }, [open, align]);
  useEffect(() => {
    if (!open) return;
    panel.current?.querySelector<HTMLElement>("input, button, select, [tabindex]")?.focus();
    const outside = (event: MouseEvent) => {
      if (
        !panel.current?.contains(event.target as Node) &&
        !trigger.current?.contains(event.target as Node)
      )
        setOpen(false);
    };
    const keyboard = (event: KeyboardEvent) => {
      if (event.key !== "Escape") return;
      event.preventDefault();
      event.stopPropagation();
      setOpen(false);
      trigger.current?.focus();
    };
    const focus = () =>
      queueMicrotask(() => {
        if (!panel.current?.contains(document.activeElement)) setOpen(false);
      });
    document.addEventListener("mousedown", outside);
    document.addEventListener("keydown", keyboard, true);
    document.addEventListener("focusin", focus);
    return () => {
      document.removeEventListener("mousedown", outside);
      document.removeEventListener("keydown", keyboard, true);
      document.removeEventListener("focusin", focus);
    };
  }, [open, setOpen]);
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
      {open
        ? createPortal(
            <div
              aria-label={title ?? (typeof label === "string" ? label : "Table actions")}
              className={`fixed z-50 flex max-h-[min(20rem,calc(100dvh-16px))] w-64 max-w-[calc(100vw-16px)] flex-col overflow-y-auto rounded-lg border p-1 text-sm font-normal shadow-lg ${card}`}
              ref={panel}
              role="dialog"
              style={position}
            >
              {children(close)}
            </div>,
            document.body
          )
        : null}
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
              {String(column.columnDef.header)}
            </label>
          ))
      }
    </Popover>
  );
}

function EpicActions({
  epicKey,
  epics,
  onRename,
  onMerge,
  onStartAll,
}: {
  epicKey: string;
  epics: readonly IssueSummary[];
  onRename: () => void;
  onMerge: (into: string) => void;
  onStartAll: () => void;
}): ReactNode {
  return (
    <span className="ml-auto">
      <Popover align="right" label="Epic actions ▾">
        {(close) => (
          <>
            <button
              className={`rounded px-2 py-1 text-left ${surfaceMutedHoverBg}`}
              onClick={() => {
                close();
                onRename();
              }}
              type="button"
            >
              Rename
            </button>
            <button
              className={`rounded px-2 py-1 text-left ${surfaceMutedHoverBg}`}
              onClick={() => {
                close();
                onStartAll();
              }}
              type="button"
            >
              Start all its queued issues next
            </button>
            <label className={`px-2 py-1 text-xs ${textMutedOnCanvas}`}>
              Merge into another epic
              <select
                className={`mt-1 block w-full rounded-md border px-1 py-1 text-sm ${borderDefault} ${surfaceBg} ${textPrimaryOnCanvas}`}
                onChange={(event) => {
                  if (event.target.value) {
                    close();
                    onMerge(event.target.value);
                  }
                }}
                value=""
              >
                <option value="">Choose an epic…</option>
                {epics
                  .filter((epic) => epic.key !== epicKey)
                  .map((epic) => (
                    <option key={epic.key} value={epic.key}>
                      {epic.title}
                    </option>
                  ))}
              </select>
            </label>
            <span className={`px-2 py-1 text-xs ${textMutedOnCanvas}`}>
              To split it, tick some of its issues and choose “New epic from these”.
            </span>
          </>
        )}
      </Popover>
    </span>
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

export function IssueTableStatus({
  table,
  items,
  epics,
  doneLoaded,
}: {
  table: Table<Item>;
  items: readonly Item[];
  epics: readonly IssueSummary[];
  doneLoaded: boolean;
}): ReactNode {
  const statusColumn = table.getColumn("status");
  const statusPicks = (statusColumn?.getFilterValue() as string[] | undefined) ?? [];
  const statusCounts = new Map<string, number>();
  for (const item of items)
    statusCounts.set(item.issue.status, (statusCounts.get(item.issue.status) ?? 0) + 1);
  const queuedCount = items.filter((item) => item.queue !== undefined).length;
  const runningCount = items.filter((item) =>
    ["in_progress", "testing", "needs_review", "retro"].includes(item.issue.status)
  ).length;
  return (
    <div className={`rounded-xl border p-3 ${borderDefault}`}>
      <div
        className={`mb-2 flex flex-wrap items-baseline gap-x-5 gap-y-1 text-sm ${textSecondaryOnCanvas}`}
      >
        <span>
          <b className={`text-lg tabular-nums ${textPrimaryOnCanvas}`}>{queuedCount}</b> in Legion's
          queue
        </span>
        <span>
          <b className={`text-lg tabular-nums ${textPrimaryOnCanvas}`}>{runningCount}</b> running
        </span>
        <span>
          <b className={`text-lg tabular-nums ${textPrimaryOnCanvas}`}>{epics.length}</b>{" "}
          {epics.length === 1 ? "epic" : "epics"}
        </span>
      </div>
      <fieldset
        aria-label="Issues by status"
        className={`flex min-h-11 min-w-0 overflow-x-auto rounded-md border-0 p-0 ${surfaceMutedBg}`}
      >
        {issueStatuses.map((status) => {
          const count = statusCounts.get(status) ?? 0;
          if (count === 0) return null;
          const on = statusPicks.length === 0 || statusPicks.includes(status);
          return (
            <button
              className={`min-h-11 shrink-0 px-3 py-2 text-left text-xs font-medium whitespace-nowrap ${issueStatusScale[status]} ${on ? "" : "opacity-30"}`}
              key={status}
              onClick={() =>
                statusColumn?.setFilterValue(
                  statusPicks.length === 1 && statusPicks[0] === status ? undefined : [status]
                )
              }
              title={`${statusLabel(status)}: ${count}`}
              type="button"
            >
              {statusLabel(status)} {count}
            </button>
          );
        })}
        {doneLoaded ? null : (
          <button
            className={`min-h-11 shrink-0 px-3 py-2 text-xs font-medium whitespace-nowrap ${issueStatusScale.done}`}
            onClick={() => statusColumn?.setFilterValue(["done"])}
            title="Closed issues load when you show them"
            type="button"
          >
            Done: load
          </button>
        )}
      </fieldset>
    </div>
  );
}

export function IssueTableToolbar({ table }: { table: Table<Item> }): ReactNode {
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
          placeholder="Search issues: key, title, epic, assignee, author, labels"
          type="search"
          value={search}
          maxLength={SEARCH_QUERY_MAX}
        />
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
                  {String(column.columnDef.header)}
                </option>
              ))}
          </select>
        </label>
        <ColumnsMenu columns={table.getAllLeafColumns()} />
        <Chip selected={flags.needsYou} onClick={() => flags.setNeedsYou(!flags.needsYou)}>
          Needs you
        </Chip>
        <Chip selected={flags.unread} onClick={() => flags.setUnread(!flags.unread)}>
          Unread
        </Chip>
        <Chip selected={flags.unclaimed} onClick={() => flags.setUnclaimed(!flags.unclaimed)}>
          Unclaimed
        </Chip>
        {filters.length > 0 ||
        search !== "" ||
        flags.needsYou ||
        flags.unread ||
        flags.unclaimed ? (
          <button
            className={`rounded-lg border px-2 py-1.5 text-sm ${borderDefault} ${surfaceMutedHoverBg}`}
            onClick={() =>
              setParams(
                (current) => {
                  const next = writeUrl(current, {
                    ...readUrl(current),
                    filters: [],
                    search: "",
                    page: 0,
                  });
                  for (const flag of ["needs-you", "unread", "unclaimed"]) next.delete(flag);
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
      {flags.unclaimedUnchecked ? (
        <p
          role="status"
          data-testid="unclaimed-unchecked"
          className={`text-sm ${textSecondaryOnCanvas}`}
        >
          Couldn't check which claims have lapsed, so every claim counts as held:{" "}
          {flags.unclaimedUnchecked === "refilling"
            ? AGENT_LIST_REFILLING
            : "Envoy listener unreachable."}
        </p>
      ) : null}
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
        {[25, 50, 100, 250, ALL_ROWS].map((size) => (
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
  startNext: (keys: readonly string[]) => void;
  moveTo: (keys: readonly string[], epic: string | null) => void;
  newEpic: (keys: readonly string[]) => void;
}): ReactNode {
  const selectedKeys = table.getSelectedRowModel().flatRows.map((row) => row.original.issue.key);
  const leafCount = table.getFilteredRowModel().rows.length;
  const setSelection = table.setRowSelection;
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
          <Action disabled={busy} onClick={() => startNext(selectedKeys)}>
            Start next
          </Action>
          <select
            aria-label="Move to epic"
            className={`rounded-lg border px-2 py-1 ${borderDefault} ${surfaceBg}`}
            disabled={busy}
            onChange={(event) => {
              if (event.target.value) void moveTo(selectedKeys, event.target.value);
            }}
            value=""
          >
            <option value="">Move to epic…</option>
            {epics.map((epic) => (
              <option key={epic.key} value={epic.key}>
                {epic.title}
              </option>
            ))}
          </select>
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
