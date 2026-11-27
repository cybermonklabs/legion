import { type ReactNode, useState } from "react";

import {
  TableBody,
  TableCell,
  TableHead,
  TableHeadCell,
  TableRow,
  TableShell,
} from "../../../components/TableShell";
import { linkHoverText, linkText } from "../../../theme/classes";
import { PriorityValue } from "../../going/PriorityValue";
import { issuePriority } from "../../issue/issue-priority";
import { ItemChip } from "../../refs/ItemChip";
import { buildReferencePath } from "../../refs/routes";
import { type BlockOf, columnText, columnWords } from "./block-cells";

/** How many rows a table shows in an answer before "Show all" (design v2, state 5). */
export const TABLE_ROWS = 8;

/** An issue key, as a read names an issue: a row whose first column holds one is that issue,
 *  and that cell draws the issue as an item chip. */
const ISSUE_KEY = /^[A-Z][A-Z0-9]{1,9}-[1-9][0-9]*$/u;

/**
 * A table (design v2, state 5): the shared table (`TableShell`) in its bordered box, as wide as
 * the answer, one column for each field the spec names, in its order, headed in the issue page's
 * words, and one row for each row the read returned, each value in Dispatch's words
 * (`columnText`). A row whose first column is an issue's key draws that cell as the issue's chip,
 * a link to it, and the column is headed "Issue", which is what it holds: the server's steps put
 * each row there from the run's own reads, never from the model's words, so the key is of an issue
 * the run read, as an answer's item frame is. The header stays on top while the rows scroll under
 * it. With more rows than `TABLE_ROWS` and no `whole`, the table shows the first of them and
 * "Show all 42 ›", which calls `onShowAll` - the conversation opens the table whole over the bar
 * or the panel - or, with none given, shows every row in place.
 */
export function TableBlock({
  block,
  onShowAll,
  whole = false,
}: {
  readonly block: BlockOf<"table">;
  readonly onShowAll?: () => void;
  readonly whole?: boolean;
}): ReactNode {
  const [open, setOpen] = useState(false);
  const columns = block.spec.columns;
  const all = whole || open || block.rows.length <= TABLE_ROWS;
  const rows = all ? block.rows : block.rows.slice(0, TABLE_ROWS);
  const first = columns[0];
  const itemColumn =
    first !== undefined &&
    block.rows.length > 0 &&
    block.rows.every((row) => typeof row[first] === "string" && ISSUE_KEY.test(row[first]));
  return (
    <div className="space-y-1">
      <div data-testid="going-block-table">
        <TableShell
          className={whole ? undefined : "max-h-80"}
          label={block.title === "" ? "Table" : block.title}
          size="tight"
          stickyHead
        >
          <TableHead>
            {columns.map((column, at) => (
              <TableHeadCell key={column}>
                {at === 0 && itemColumn ? "Issue" : columnWords(column)}
              </TableHeadCell>
            ))}
          </TableHead>
          <TableBody>
            {rows.map((row, index) => (
              <TableRow
                // The rows are the read's, in its order, and carry no key of their own.
                // biome-ignore lint/suspicious/noArrayIndexKey: a block's rows never reorder.
                key={index}
              >
                {columns.map((column, at) => {
                  const value = row[column];
                  const issue =
                    at === 0 && typeof value === "string" && ISSUE_KEY.test(value)
                      ? value
                      : undefined;
                  // A priority set reads as the issue page's badge, as every priority the bar
                  // shows does; none reads as the page's words (`columnText`).
                  const priority =
                    column === "priority" && typeof value === "number"
                      ? issuePriority(value)
                      : null;
                  let cell: ReactNode = columnText(column, value);
                  if (issue !== undefined) {
                    cell = (
                      <a
                        className="inline-flex max-w-full"
                        data-testid="going-block-item"
                        href={buildReferencePath({ key: issue, kind: "issue" })}
                      >
                        <ItemChip issue={issue} />
                      </a>
                    );
                  } else if (priority !== null) {
                    cell = <PriorityValue priority={priority} />;
                  }
                  return <TableCell key={column}>{cell}</TableCell>;
                })}
              </TableRow>
            ))}
          </TableBody>
        </TableShell>
      </div>
      {all ? null : (
        <button
          className={`text-xs ${linkText} ${linkHoverText}`}
          data-testid="going-block-show-all"
          onClick={() => (onShowAll === undefined ? setOpen(true) : onShowAll())}
          type="button"
        >
          Show all {block.rows.length} ›
        </button>
      )}
    </div>
  );
}
