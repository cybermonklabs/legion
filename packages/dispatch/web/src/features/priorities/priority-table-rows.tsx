import type { Row } from "@tanstack/react-table";
import type { ReactNode } from "react";
import { DataTableCells, type LineProps } from "../../components/DataTable";
import { focusVisibleRing, selectedCardBg } from "../../theme/classes";

/**
 * One row of the priorities table: the shared cells, and, while the table's order is the
 * reader's to drag, the drop that lands another row here. The row carries `data-priority-row`
 * and `data-priority-group` (`PRIORITY_ROW_ATTRIBUTE` and `PRIORITY_GROUP_ATTRIBUTE`), which
 * the table's own keys walk it by and its box measures its height from. The row being dragged
 * is painted to its left edge, its pinned cells with it (`DataTableCells`' `highlight`).
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
      data-priority-group={row.getParentRow()?.id ?? "ungrouped"}
      data-priority-row={row.id}
      onDragOver={(event) => {
        if (dragging !== undefined) event.preventDefault();
      }}
      onDrop={() => {
        if (dragging !== undefined && dragging !== row.id) onDrop(row.id);
      }}
      ref={measure}
      tabIndex={-1}
    >
      <DataTableCells highlight={dragged ? selectedCardBg : undefined} row={row} />
    </tr>
  );
}
