import type { Column } from "@tanstack/react-table";
import { type ReactNode, useState } from "react";
import { checkboxAccent, surfaceMutedHoverBg } from "../theme/classes";
import { CONTROL, type ControlSize } from "./control-names";
import { PanelTrigger } from "./PanelTrigger";

export interface ColumnsMenuProps<TData> {
  /** The table's leaf columns, in the order the menu lists them; the ones the reader may not
   *  hide are left out. */
  readonly columns: readonly Column<TData, unknown>[];
  readonly size?: ControlSize;
}

/**
 * The one columns menu: a trigger and the panel of tick boxes that says which of a table's
 * columns are shown. It keeps its own open state, because a table's header has no other panel
 * for it to be open beside.
 */
export function ColumnsMenu<TData>({ columns, size }: ColumnsMenuProps<TData>): ReactNode {
  const [open, setOpen] = useState(false);
  return (
    <PanelTrigger
      align="right"
      control={CONTROL.columnsMenu}
      label="Columns"
      onOpenChange={setOpen}
      open={open}
      size={size}
    >
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
    </PanelTrigger>
  );
}
