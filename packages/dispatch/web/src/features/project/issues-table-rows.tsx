import type { Row } from "@tanstack/react-table";
import { type ReactNode, useState } from "react";
import { Link } from "react-router-dom";
import type { IssueSummary } from "../../api/types";
import { flexRender } from "../../components/DataTable";
import { PanelTrigger } from "../../components/PanelTrigger";
import {
  borderDefault,
  focusVisibleRing,
  issueStatusScale,
  linkHoverText,
  linkText,
  selectedCardBg,
  surfaceBg,
  surfaceMutedBg,
  surfaceMutedHoverBg,
  textMutedOnCanvas,
  textMutedOnSurfaceMuted,
  textPrimaryOnCanvas,
} from "../../theme/classes";
import { buildIssuePath } from "../refs/routes";
import { issueStatuses, statusLabel } from "./board-model";
import { columnFilterSpec, type Item } from "./issues-table-model";

/** One issue's row: its cells, and the Start next the reader reaches by hovering it. */
export function IssueRow({
  busy,
  index,
  measure,
  moved,
  onStartNext,
  row,
}: {
  busy: boolean;
  index: number;
  measure: (element: Element | null) => void;
  /** The issues this action has just changed, which the row shows until the next one. */
  moved: ReadonlySet<string>;
  onStartNext: (key: string) => void;
  row: Row<Item>;
}): ReactNode {
  const { issue, queue } = row.original;
  const changed = moved.has(issue.key);
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
            className={`overflow-hidden border-b px-2 py-2 ${borderDefault} ${pinned ? `sticky z-10 ${changed ? selectedCardBg : surfaceBg}` : ""} ${pinned && cell.column.getIsLastColumn("left") ? "border-r" : ""} ${row.depth > 0 && cell.column.id === "select" ? "pl-6" : ""}`}
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
  const [open, setOpen] = useState(false);
  return (
    <span className="ml-auto">
      <PanelTrigger
        align="right"
        label="Epic actions ▾"
        onOpenChange={setOpen}
        open={open}
        size="compact"
      >
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
      </PanelTrigger>
    </span>
  );
}
