import type { Row } from "@tanstack/react-table";
import type { ReactNode } from "react";
import { DataTableCells, DataTableGroupRow } from "../../components/DataTable";
import { heldPress } from "../../components/held-press";
import {
  borderDefault,
  focusVisibleRing,
  issueStatusScale,
  selectedCardBg,
  surfaceBg,
  surfaceMutedBg,
  textMutedOnSurfaceMuted,
} from "../../theme/classes";
import { statusLabel } from "./board-model";
import { countStatuses, statusSegments } from "./issues-table-bar";
import type { Item } from "./issues-table-model";

/** One issue's row: its cells, and the Start next the reader reaches by hovering it, which is
 *  held while a table action is out (`busy`, `heldPress`) as a `StepButton` is, and for the same
 *  reason: from its own press until the list read again moves its issue up the queue and takes
 *  it away, so it keeps the focus while its write is out and its going is the press's doing. */
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
  /** Starts the row's issue next: absent on a table that draws no row's actions - its listing
   *  holds no issue in Legion's queue (`listsQueued`), or its layout leaves them out
   *  (`rowActions`, `ListingTable`) - which draws no cell for it. */
  onStartNext: ((key: string) => void) | undefined;
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
      <DataTableCells
        cellClass={(cell) => (row.depth > 0 && cell.column.id === "select" ? "pl-6" : "")}
        highlight={changed ? selectedCardBg : undefined}
        row={row}
        // A table that offers no Start next (`onStartNext`) draws no cell after its columns.
        trailing={
          onStartNext === undefined ? undefined : queue !== undefined && queue > 1 ? (
            <button
              className={`rounded-md border px-2 py-0.5 text-xs whitespace-nowrap opacity-0 group-hover:opacity-100 focus:opacity-100 ${borderDefault} ${surfaceBg}`}
              {...heldPress(busy, () => onStartNext(issue.key))}
              type="button"
            >
              Start next
            </button>
          ) : null
        }
      />
    </tr>
  );
}

/** A group's header row: its value, how many issues, and its issues by status. */
export function GroupRow({
  row,
  columns,
  continued,
  index,
  legionQueue,
  measure,
  show,
  shown,
  viewportWidth,
}: {
  /** A repeat of the header of a group the page opens in the middle of. */
  continued?: boolean;
  /** How many of the group's rows this page holds, when that is fewer than the group has. */
  shown?: number;
  index: number;
  /** Whether the heading names the first Place in the Legion queue, the Queue column's number,
   *  as the Issues tab does. A table another page draws keeps `next in queue` and Start next's
   *  number (`queue`). */
  legionQueue: boolean;
  measure: (element: Element | null) => void;
  row: Row<Item>;
  columns: number;
  /** A value as the grouped column's filter shows it (its `meta.show`): Legion phase's, the
   *  board's own label. A column with none shows the value as it is. */
  show: ((value: string) => string) | undefined;
  viewportWidth: number;
}): ReactNode {
  const leaves = row.getLeafRows().filter((leaf) => !leaf.getIsGrouped());
  const value = String(row.groupingValue ?? "");
  // A heading reads as the column's filter does.
  const heading = show === undefined ? value : show(value);
  // What the reader can see under this heading, which a page boundary can make fewer than the
  // group holds: "3 of 5 issues" rather than a count of rows that are not on this page.
  const here = shown ?? leaves.length;
  const count =
    here === leaves.length
      ? `${leaves.length} ${leaves.length === 1 ? "issue" : "issues"}`
      : `${here} of ${leaves.length} issues`;
  // The first number among the group's rows. With `legionQueue` it is the Queue column's
  // (`place`): the board's Place, which an issue has whether or not it is labelled legion, or
  // Start next's number in a project with no daemon. Without it, it is Start next's number
  // (`queue`), which only a Todo issue labelled legion has.
  let next = Number.POSITIVE_INFINITY;
  for (const { original } of leaves) {
    const number = legionQueue ? original.place : original.queue;
    if (number !== undefined && number < next) next = number;
  }
  const byStatus = statusSegments(countStatuses(leaves.map((leaf) => leaf.original.issue.status)));
  return (
    <DataTableGroupRow
      columns={columns}
      expanded={row.getIsExpanded()}
      heading={
        <>
          {heading}
          {continued ? (
            <span className={`font-normal ${textMutedOnSurfaceMuted}`}>(continued)</span>
          ) : null}
          <span className={`font-normal ${textMutedOnSurfaceMuted}`}>
            {count}
            {Number.isFinite(next)
              ? ` · next in ${legionQueue ? "Legion queue" : "queue"} #${next}`
              : ""}
          </span>
        </>
      }
      index={index}
      label={`${row.getIsExpanded() ? "Collapse" : "Expand"} ${heading}, ${count}${continued ? ", continued" : ""}`}
      measure={measure}
      onToggle={row.getToggleExpandedHandler()}
      slot={
        <span className="flex items-center gap-2">
          <span
            aria-hidden
            className={`flex h-2 w-32 overflow-hidden rounded-full ${surfaceMutedBg}`}
          >
            {byStatus.map(({ count, status }) => (
              <span className={issueStatusScale[status]} key={status} style={{ flex: count }} />
            ))}
          </span>
          <span className={`text-xs ${textMutedOnSurfaceMuted}`}>
            {byStatus
              .map(({ count, status }) => `${count} ${statusLabel(status).toLowerCase()}`)
              .join(" · ")}
          </span>
        </span>
      }
      width={Math.max(180, viewportWidth - 20)}
    />
  );
}
