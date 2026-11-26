import { flexRender, type Row } from "@tanstack/react-table";
import type { ReactNode } from "react";
import { drawsPinnedDivider, type LineProps } from "../../components/DataTable";
import { selectedCardBg } from "../../theme/classes";
import { borderDefault, focusVisibleRing, surfaceBg } from "../../theme/shell-classes";

/**
 * One row of the priorities table: its cells, and, while the table's order is the reader's to
 * drag, the drop that lands another row here. The row carries `data-issue-row`, which the
 * table's own keys walk it by and its box measures its height from.
 */
export function TableBodyRow<Row_>({
  dragging,
  index,
  label,
  measure,
  onDrop,
  row,
}: LineProps & {
  /** The row the reader is dragging now, if any: only then is this row a drop target. */
  dragging: string | undefined;
  /** What a reader hears the row called. */
  label: string;
  /** Lands the dragged row at this one's place. */
  onDrop: (onto: string) => void;
  row: Row<Row_>;
}): ReactNode {
  const dragged = dragging === row.id;
  return (
    <tr
      aria-label={label}
      className={`group align-top outline-none focus-visible:ring-2 ${focusVisibleRing} ${dragged ? selectedCardBg : ""}`}
      data-index={index}
      data-issue-group={row.getParentRow()?.id ?? "ungrouped"}
      data-issue-row={row.id}
      onDragOver={(event) => {
        if (dragging === undefined) return;
        event.preventDefault();
        event.dataTransfer.dropEffect = "move";
      }}
      onDrop={(event) => {
        if (dragging === undefined) return;
        // The drag carries the id as text, which a browser would otherwise open as a search.
        event.preventDefault();
        if (dragging !== row.id) onDrop(row.id);
      }}
      ref={measure}
      tabIndex={-1}
    >
      {row.getVisibleCells().map((cell) => {
        const pinned = cell.column.getIsPinned() === "left";
        return (
          <td
            className={`overflow-hidden border-b px-2 py-2 ${borderDefault} ${pinned ? `sticky z-10 ${surfaceBg}` : ""} ${drawsPinnedDivider(cell.column, false) ? "border-r" : ""}`}
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
    </tr>
  );
}
