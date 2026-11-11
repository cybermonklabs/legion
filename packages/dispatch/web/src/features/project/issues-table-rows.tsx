import { flexRender, type Header, type Row } from "@tanstack/react-table";
import { type ReactNode, useState } from "react";
import { Link } from "react-router-dom";
import type { IssueSummary } from "../../api/types";
import {
  borderDefault,
  focusVisibleRing,
  linkHoverText,
  linkText,
  selectedCardBg,
  stageClass,
  surfaceBg,
  surfaceMutedBg,
  surfaceMutedHoverBg,
  tableResizeActive,
  tableResizeHover,
  textMutedOnCanvas,
  textMutedOnSurfaceMuted,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { buildIssuePath } from "../refs/routes";
import { issueStatuses, statusLabel } from "./board-model";
import { ColumnFilter, Popover } from "./issues-table-controls";
import { columnFilterSpec, type Item, reorder, statusStage } from "./issues-table-model";

/** One header cell: its sort control, which doubles as the handle that drags the column
 *  elsewhere, its filter, and the edge that resizes it. */
export function TableHeaderCell<Row>({
  compact,
  counts,
  doneLoaded,
  dragColumn,
  header,
  openFilter,
  order,
  setDragColumn,
  setOpenFilter,
  setOrder,
}: {
  /** A phone, where the title's header stacks its controls. */
  compact: boolean;
  counts: ReadonlyMap<string, number>;
  doneLoaded?: boolean;
  dragColumn: string | undefined;
  header: Header<Row, unknown>;
  openFilter: string | undefined;
  /** The columns in the order they are shown, which a drop rewrites. */
  order: readonly string[];
  setDragColumn: (id: string | undefined) => void;
  setOpenFilter: (id: string | undefined) => void;
  setOrder: (next: string[]) => void;
}): ReactNode {
  const column = header.column;
  const pinned = column.getIsPinned() === "left";
  return (
    <th
      aria-sort={
        column.getIsSorted() === "asc"
          ? "ascending"
          : column.getIsSorted() === "desc"
            ? "descending"
            : undefined
      }
      className={`sticky top-0 border-b px-1 ${compact ? "py-0 [&_button]:min-h-6 [&_button]:min-w-6 [&_button[aria-label^='About']]:hidden" : "py-2"} pr-2 text-left align-middle text-xs font-semibold md:px-2 ${borderDefault} ${surfaceBg} ${textSecondaryOnCanvas} ${pinned ? "z-30" : "z-20"} ${pinned && column.getIsLastColumn("left") ? "border-r" : ""}`}
      onDragOver={(event) => {
        if (dragColumn !== undefined && !pinned) event.preventDefault();
      }}
      onDrop={() => {
        if (dragColumn === undefined || pinned || dragColumn === column.id) return;
        setOrder(reorder(order, dragColumn, column.id));
        setDragColumn(undefined);
      }}
      style={{
        width: header.getSize(),
        left: pinned ? column.getStart("left") : undefined,
      }}
    >
      <span
        className={`flex w-full min-w-0 justify-between ${compact && column.id === "title" ? "flex-col items-center" : "items-center gap-0 md:gap-1"}`}
      >
        {column.columnDef.meta?.control ? (
          // The header draws its own controls, so the sort is a mark beside them: a press on a
          // hint or a switch inside the header sorts nothing, and no button nests in another.
          // A header with a switch of its own is wider than a phone's column, so there its
          // parts wrap — the name on one line, the switch under it — rather than standing side
          // by side and reaching past the table's box. The header a column supplies is laid out
          // by its own row of parts, so the wrap reaches into it as well as this row.
          <span
            data-header-label
            className={`flex min-w-0 items-center gap-1 ${compact ? "[&>span]:min-w-0" : ""}`}
          >
            {flexRender(column.columnDef.header, header.getContext())}
            {column.getCanSort() ? (
              <button
                aria-label={`Sort by ${column.id}`}
                className={compact ? "hidden" : textMutedOnCanvas}
                draggable={!pinned}
                onClick={column.getToggleSortingHandler()}
                onDragEnd={() => setDragColumn(undefined)}
                onDragStart={() => setDragColumn(column.id)}
                title="Sort (shift-click to add a second sort)"
                type="button"
              >
                {{ asc: "▲", desc: "▼" }[column.getIsSorted() as string] ?? "↕"}
              </button>
            ) : null}
          </span>
        ) : column.getCanSort() ? (
          <button
            data-header-label
            draggable={!pinned}
            onClick={column.getToggleSortingHandler()}
            onDragEnd={() => setDragColumn(undefined)}
            onDragStart={() => setDragColumn(column.id)}
            title={
              pinned
                ? "Sort (shift-click to add a second sort)"
                : "Sort (shift-click to add a second sort); drag to move the column"
            }
            type="button"
          >
            {flexRender(column.columnDef.header, header.getContext())}
            <span className={compact ? "hidden" : textMutedOnCanvas}>
              {{ asc: "▲", desc: "▼" }[column.getIsSorted() as string] ?? ""}
            </span>
          </button>
        ) : (
          <span data-header-label>{flexRender(column.columnDef.header, header.getContext())}</span>
        )}
        {column.getCanFilter() ? (
          <ColumnFilter
            column={column}
            counts={counts}
            doneLoaded={doneLoaded}
            onOpenChange={(open) => setOpenFilter(open ? column.id : undefined)}
            open={openFilter === column.id}
          />
        ) : null}
      </span>
      {column.getCanResize() ? (
        <span
          aria-hidden
          className={`absolute top-0 right-0 h-full w-1.5 cursor-col-resize touch-none select-none ${column.getIsResizing() ? tableResizeActive : tableResizeHover}`}
          onDoubleClick={() => column.resetSize()}
          onMouseDown={header.getResizeHandler()}
          onTouchStart={header.getResizeHandler()}
        />
      ) : null}
    </th>
  );
}

/** One issue's row: its cells, and the Start next the reader reaches by hovering it. */
export function IssueRow({
  busy = false,
  continues = false,
  index,
  measure,
  moved,
  onStartNext,
  row,
}: {
  busy?: boolean;
  /** Whether a row of this issue's own follows — its title on a line of its own, on a phone —
   *  so this one draws no rule under it and the two read as one row. */
  continues?: boolean;
  index: number;
  measure: (element: Element | null) => void;
  /** The issues this action has just changed, which the row shows until the next one. */
  moved?: ReadonlySet<string>;
  /** Moves the issue to the head of Legion's queue. A grid acts on nothing and gives none; its
   *  rows then end at their last column, rather than carrying an empty cell with no header. */
  onStartNext?: (key: string) => void;
  row: Row<Item>;
}): ReactNode {
  const { issue, queue } = row.original;
  const changed = moved?.has(issue.key) ?? false;
  return (
    <tr
      aria-label={`${issue.key} ${issue.title}`}
      className={`group align-top outline-none focus-visible:ring-2 ${focusVisibleRing} ${changed ? selectedCardBg : ""}`}
      data-index={index}
      data-issue-group={row.getParentRow()?.id ?? "ungrouped"}
      data-issue-row={issue.key}
      data-moved={changed || undefined}
      ref={measure}
      tabIndex={-1}
    >
      {row.getVisibleCells().map((cell) => {
        const pinned = cell.column.getIsPinned() === "left";
        return (
          <td
            className={`overflow-hidden px-2 py-2 ${continues ? "" : "border-b"} ${borderDefault} ${changed ? selectedCardBg : surfaceBg} ${pinned ? `sticky z-10 ${changed ? selectedCardBg : surfaceBg}` : ""} ${pinned && cell.column.getIsLastColumn("left") ? "border-r" : ""} ${row.depth > 0 && cell.column.id === "select" ? "pl-6" : ""}`}
            key={cell.id}
            style={{
              width: cell.column.getSize(),
              left: pinned ? cell.column.getStart("left") : undefined,
            }}
          >
            {flexRender(cell.column.columnDef.cell, cell.getContext())}
          </td>
        );
      })}
      {onStartNext === undefined ? null : (
        <td className={`border-b px-2 py-2 text-right ${borderDefault}`}>
          {queue !== undefined && queue > 1 ? (
            <button
              className={`rounded-md border px-2 py-0.5 text-xs whitespace-nowrap opacity-0 group-hover:opacity-100 focus:opacity-100 ${borderDefault} ${surfaceBg}`}
              disabled={busy}
              onClick={() => onStartNext(issue.key)}
              type="button"
            >
              Start next
            </button>
          ) : null}
        </td>
      )}
    </tr>
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
  shown,
  viewportWidth,
}: {
  /** A repeat of the header of a group the page opens in the middle of. */
  continued?: boolean;
  /** How many of the group's rows this page holds, when that is fewer than the group has. */
  shown?: number;
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
  // A heading reads as the column's filter does: the registry holds that text once.
  const groupedBy = columnFilterSpec(grouping);
  const heading = groupedBy?.kind === "picks" && groupedBy.show ? groupedBy.show(value) : value;
  // What the reader can see under this heading, which a page boundary can make fewer than the
  // group holds: "3 of 5 issues" rather than a count of rows that are not on this page.
  const here = shown ?? leaves.length;
  const count =
    here === leaves.length
      ? `${leaves.length} ${leaves.length === 1 ? "issue" : "issues"}`
      : `${here} of ${leaves.length} issues`;
  const epicKey = grouping === "epic" ? leaves[0]?.original.epicKey : undefined;
  const leafCounts = new Map<string, number>();
  let next = Number.POSITIVE_INFINITY;
  for (const leaf of leaves) {
    const { issue, queue } = leaf.original;
    leafCounts.set(issue.status, (leafCounts.get(issue.status) ?? 0) + 1);
    if (queue !== undefined && queue < next) next = queue;
  }
  const byStatus = issueStatuses
    .map((status) => [status, leafCounts.get(status) ?? 0] as const)
    .filter(([, count]) => count > 0);
  return (
    <tr className={surfaceMutedBg} data-index={index} ref={measure}>
      <td className="px-2 py-2" colSpan={columns}>
        <div
          className="sticky left-0 flex flex-wrap items-center gap-x-4 gap-y-1"
          style={{ width: Math.max(180, viewportWidth - 20) }}
        >
          <button
            aria-expanded={row.getIsExpanded()}
            aria-label={`${row.getIsExpanded() ? "Collapse" : "Expand"} ${heading}, ${count}${continued ? ", continued" : ""}`}
            className={`flex min-w-0 items-center gap-2 text-left font-semibold ${textPrimaryOnCanvas}`}
            onClick={row.getToggleExpandedHandler()}
            type="button"
          >
            <span className={`w-4 ${textMutedOnSurfaceMuted}`}>
              {row.getIsExpanded() ? "▾" : "▸"}
            </span>
            {epicKey === undefined ? heading : null}
            {continued ? (
              <span className={`font-normal ${textMutedOnSurfaceMuted}`}>(continued)</span>
            ) : null}
            <span className={`font-normal ${textMutedOnSurfaceMuted}`}>
              {count}
              {Number.isFinite(next) ? ` · next in queue #${next}` : ""}
            </span>
          </button>
          {epicKey === undefined ? null : (
            <Link
              className={`font-semibold ${linkText} ${linkHoverText}`}
              to={buildIssuePath({ key: epicKey, kind: "issue" })}
            >
              {heading}
            </Link>
          )}
          <span className="flex items-center gap-2">
            <span
              aria-hidden
              className={`flex h-2 w-32 overflow-hidden rounded-full ${surfaceMutedBg}`}
            >
              {byStatus.map(([status, count]) => (
                <span
                  className={stageClass(statusStage(status))}
                  key={status}
                  style={{ flex: count }}
                />
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
  /** The epic the reader has picked to merge into, which no write has been sent for yet. */
  const [target, setTarget] = useState("");
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
            {/* Two steps, because a merge closes an issue: the select stages a target, and
                Merge… asks before anything is written. A closed `<select>` commits on an arrow
                key in Chrome on Linux and Windows, which is one keystroke from a closed epic. */}
            <label className={`px-2 py-1 text-xs ${textMutedOnCanvas}`}>
              Merge into another epic
              <select
                className={`mt-1 block w-full rounded-md border px-1 py-1 text-sm ${borderDefault} ${surfaceBg} ${textPrimaryOnCanvas}`}
                onChange={(event) => setTarget(event.target.value)}
                value={target}
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
            <button
              className={`mx-2 my-1 rounded border px-2 py-1 text-sm disabled:opacity-40 ${borderDefault} ${surfaceMutedHoverBg}`}
              disabled={target === ""}
              onClick={() => {
                close();
                onMerge(target);
              }}
              type="button"
            >
              Merge…
            </button>
            <span className={`px-2 py-1 text-xs ${textMutedOnCanvas}`}>
              To split it, tick some of its issues and choose “New epic from these”.
            </span>
          </>
        )}
      </Popover>
    </span>
  );
}
