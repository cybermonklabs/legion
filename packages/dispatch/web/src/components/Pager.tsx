import type { Table } from "@tanstack/react-table";
import type { ReactNode, RefObject } from "react";
import {
  borderDefault,
  surfaceBg,
  surfaceMutedHoverBg,
  textSecondaryOnCanvas,
} from "../theme/classes";
import { CONTROL } from "./control-names";

/** Singular and plural of one of the things a pager counts. */
export interface PagerNoun {
  readonly one: string;
  readonly many: string;
}

/** What the pager calls what it counts, in the caller's own words: the project List lists
 *  issues, in groups, over rows. */
export interface PagerNouns {
  /** What the table's rows are: `3 of 40 issues`. */
  readonly item: PagerNoun;
  /** What a page holds: `Rows 1–25 of 40`, `All 40 rows`, `Rows per page`. */
  readonly row: PagerNoun;
  /** What a grouping makes: `in 4 groups`. */
  readonly group: PagerNoun;
}

/** Everything a pager takes from the page it belongs to, which a table hands on unread. */
export interface PagerOptions {
  readonly nouns: PagerNouns;
  /** The page sizes the reader may choose, in the order they are offered. */
  readonly pageSizes: readonly number[];
  /** The page size that means every row, which reads as `All rows` rather than a count per
   *  page. Any size at least this big is that choice. */
  readonly allRows?: number;
  readonly testId: string;
}

export interface PagerProps<TData> extends PagerOptions {
  readonly table: Table<TData>;
  /** Measured by `useTableLayout`, which gives the box the room left under this line. */
  readonly pagerRef?: RefObject<HTMLDivElement | null>;
}

/** A caller's noun where a line starts with it: `rows` gives `Rows 1–25 of 40` and `Rows per
 *  page`, so the caller names its rows once rather than once per capitalisation. */
const sentence = (word: string) => `${word.charAt(0).toLocaleUpperCase()}${word.slice(1)}`;

/** One step between pages. A step there is no page for is `aria-disabled` rather than
 *  `disabled`, and its press does nothing: the press that reaches the last page, or the first,
 *  would otherwise disable the button under the reader, and the browser takes the focus off a
 *  control that disables itself, dropping a keyboard reader onto the page. */
function PagerButton({
  ariaDisabled,
  children,
  onClick,
}: {
  ariaDisabled: boolean;
  children: ReactNode;
  onClick: () => void;
}): ReactNode {
  return (
    <button
      aria-disabled={ariaDisabled || undefined}
      className={`rounded-lg border px-3 py-1 text-sm aria-disabled:opacity-40 ${borderDefault} ${surfaceMutedHoverBg}`}
      onClick={() => {
        if (!ariaDisabled) onClick();
      }}
      type="button"
    >
      {children}
    </button>
  );
}

/**
 * The one line under a table: how much of the listing it is showing, which page of it, the
 * steps between pages and how many rows a page holds. Every word it counts in is its caller's,
 * so the same pager serves a list of issues and a list of anything else.
 */
export function Pager<TData>({
  allRows = Number.MAX_SAFE_INTEGER,
  nouns,
  pageSizes,
  pagerRef,
  table,
  testId,
}: PagerProps<TData>): ReactNode {
  const { pageIndex, pageSize } = table.getState().pagination;
  const { grouping } = table.getState();
  const rowsOnPages = table.getPrePaginationRowModel().rows.length;
  const leafCount = table.getFilteredRowModel().rows.length;
  const groupCount = table.getGroupedRowModel().rows.length;
  const total = table.options.data.length;
  const { group, item, row } = nouns;
  return (
    <div
      ref={pagerRef}
      data-control={CONTROL.pager}
      data-testid={testId}
      className={`flex shrink-0 flex-wrap items-center gap-3 text-sm ${textSecondaryOnCanvas}`}
    >
      <span>
        {leafCount} of {total} {total === 1 ? item.one : item.many}
        {grouping.length > 0
          ? ` in ${groupCount} ${groupCount === 1 ? group.one : group.many}`
          : ""}
      </span>
      <span className="ml-auto">
        {pageSize >= allRows
          ? `All ${rowsOnPages} ${rowsOnPages === 1 ? row.one : row.many}`
          : `${sentence(row.many)} ${rowsOnPages === 0 ? 0 : pageIndex * pageSize + 1}–${Math.min((pageIndex + 1) * pageSize, rowsOnPages)} of ${rowsOnPages}`}
      </span>
      <PagerButton ariaDisabled={!table.getCanPreviousPage()} onClick={() => table.previousPage()}>
        ‹ Previous
      </PagerButton>
      <span>
        Page {pageIndex + 1} of {Math.max(table.getPageCount(), 1)}
      </span>
      <PagerButton ariaDisabled={!table.getCanNextPage()} onClick={() => table.nextPage()}>
        Next ›
      </PagerButton>
      <select
        aria-label={`${sentence(row.many)} per page`}
        className={`rounded-lg border px-2 py-1 ${borderDefault} ${surfaceBg}`}
        onChange={(event) => table.setPageSize(Number(event.target.value))}
        value={pageSize}
      >
        {pageSizes.map((size) => (
          <option key={size} value={size}>
            {size >= allRows ? `All ${row.many}` : `${size} per page`}
          </option>
        ))}
      </select>
    </div>
  );
}
