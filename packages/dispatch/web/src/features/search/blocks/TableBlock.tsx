import type { ReactNode } from "react";

import { borderDefault, textSecondaryOnSurface } from "../../../theme/classes";
import { type BlockOf, cellText } from "./block-cells";

/** The cell classes every tabular block shares. */
export const blockCellClassName = `border-b px-2 py-1 text-left align-top ${borderDefault}`;
export const blockHeadClassName = `${blockCellClassName} font-medium ${textSecondaryOnSurface}`;

/** A table: one column for each field the spec names, in its order, and one row for each row the
 *  read returned. */
export function TableBlock({ block }: { readonly block: BlockOf<"table"> }): ReactNode {
  const columns = block.spec.columns;
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-xs" data-testid="going-block-table">
        <thead>
          <tr>
            {columns.map((column) => (
              <th className={blockHeadClassName} key={column} scope="col">
                {column}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {block.rows.map((row, index) => (
            <tr
              // The rows are the read's, in its order, and carry no key of their own.
              // biome-ignore lint/suspicious/noArrayIndexKey: a block's rows never reorder.
              key={index}
            >
              {columns.map((column) => (
                <td className={blockCellClassName} key={column}>
                  {cellText(row[column])}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
