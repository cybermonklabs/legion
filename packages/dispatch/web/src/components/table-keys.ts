import type { Row } from "@tanstack/react-table";
import { type RefObject, useLayoutEffect, useRef } from "react";
import { type KeymapScope, useKeymap } from "../features/shell/keymap";
import { closestMatching } from "../features/shell/roving";
import { CONTROL } from "./control-names";

/** The attribute a page puts on the element that holds one table's control row, its box and its
 *  pager (the issue table's section), so another table's page-wide keys read a focus anywhere in
 *  it as in that table, not only in its box (`useRowKeymap`'s `moveFrom`). */
export const TABLE_FRAME_ATTRIBUTE = "data-table-frame";

/** The frame's mark, spread on the element that holds the table's control row, box and pager
 *  (`TABLE_FRAME_ATTRIBUTE`). */
export const TABLE_FRAME_MARK = { [TABLE_FRAME_ATTRIBUTE]: "" };

/** One key a reader walks the table with, as the page that draws the table names it: the
 *  palette and the shortcut help read these words, so they are the caller's and not the
 *  table's. */
export interface RowKeyBinding {
  /** Unique within the scope: the project List's next row is `list-next`. */
  readonly id: string;
  /** In tinykeys syntax, e.g. `j`. */
  readonly keys: string;
  readonly label: string;
}

/** What the table's keys are called and where they apply. */
export interface RowKeymap {
  /** The scope the bindings are registered in, which the page pushes while it is open. */
  readonly scope: KeymapScope;
  readonly next: RowKeyBinding;
  readonly previous: RowKeyBinding;
  /** Where next and previous are answered: anywhere on the page while the table holds a row and
   *  the focus is in no other table (`page`, the default), for keys nothing else on the page
   *  answers, such as `j` and `k`; or only while the focus is in one of this table's own rows
   *  (`row`), for keys the page answers itself everywhere else, such as the arrows, which scroll
   *  it, and for every table a page draws beside its main one over the same keys - the
   *  priorities page's issue tables beside its priorities - so each table answers from its own
   *  rows and the main one from everywhere else. */
  readonly moveFrom?: "page" | "row";
  /** Opens the row in hand; the one binding the palette lists. */
  readonly open: RowKeyBinding;
  /** Opens the row in hand from Enter, which only a focused row answers. */
  readonly openOnEnter: RowKeyBinding;
  /** Marks the row in hand. A table that selects nothing names none, and no key is registered
   *  for it, so the shortcut help lists no key that does nothing. */
  readonly select?: RowKeyBinding;
}

/** Focuses the row whose id is `key`, or the element `selector` names inside it. The selection
 *  bar asks for `"nearest"` to show its checkbox whole without the engine's focus scroll. */
export type FocusRow = (
  key: string,
  selector?: string,
  placement?: "nearest"
) => HTMLElement | null;

/**
 * The keys a reader walks the table's rows with, under the ids and words the caller gives, the
 * same ids and palette rows the list they replace had: two keys move between rows, two open the
 * row in hand, and, in a table that selects, one marks it. A row the virtualizer has not drawn
 * yet is scrolled to first and focused when it lands.
 *
 * It hands back to a row (`FocusRow`) the same way. For a nearest-edge hand-off, keep the box's
 * starting scroll position so the draw of a virtual row does not decide its final placement.
 */
export function useRowKeymap<TData>({
  displayRows,
  keymap,
  landNearest,
  linkAttribute,
  onMark,
  rowAttribute,
  rowsById,
  scrollRef,
  scrollToIndex,
}: {
  displayRows: readonly { row: Row<TData> }[];
  keymap: RowKeymap;
  /** Shows a measured target whole, from the box's position before any virtual scroll. */
  landNearest: (element: HTMLElement, from?: number) => void;
  /** The attribute on the link inside a row that opening the row follows. */
  linkAttribute: string;
  /** Told the row `x` marks, as it marks it; never one it unmarks. */
  onMark?: (key: string) => void;
  /** The attribute each drawn row carries, whose value is the row's id. */
  rowAttribute: string;
  rowsById: Record<string, Row<TData>>;
  scrollRef: RefObject<HTMLDivElement | null>;
  scrollToIndex: (index: number) => void;
}): FocusRow {
  /** The row and control that take the focus once drawn, with the origin of a nearest-edge hand-off. */
  const pendingFocus = useRef<{ key: string; selector?: string; from?: number } | null>(null);
  // The row in hand: the row of this table that holds the focus, or one of its controls does. A
  // row of another table on the page, under the same attribute, is none of this table's.
  const focusedRow = () => {
    const row = closestMatching(document.activeElement, `[${rowAttribute}]`);
    return row !== null && scrollRef.current?.contains(row) === true ? row : null;
  };
  const rowElement = (key: string) =>
    scrollRef.current?.querySelector<HTMLElement>(`[${rowAttribute}="${CSS.escape(key)}"]`);
  // What it focused, or null: a row the virtualizer has not drawn is focused once it lands, and a
  // key no row of this page holds moves nothing.
  const focusRow: FocusRow = (key, selector, placement) => {
    const row = rowElement(key);
    if (row) {
      pendingFocus.current = null;
      const element = selector === undefined ? row : row.querySelector<HTMLElement>(selector);
      if (element !== null) {
        if (placement === "nearest") landNearest(element);
        else element.focus();
      }
      return element;
    }
    const index = displayRows.findIndex(({ row }) => row.id === key && !row.getIsGrouped());
    if (index !== -1) {
      const from = placement === "nearest" ? scrollRef.current?.scrollTop : undefined;
      pendingFocus.current = { key, selector, from };
      scrollToIndex(index);
    }
    return null;
  };
  const moveFocus = (direction: 1 | -1) => {
    const leaves = displayRows.filter(({ row }) => !row.getIsGrouped());
    const current = leaves.findIndex(
      ({ row }) => row.id === focusedRow()?.getAttribute(rowAttribute)
    );
    const target =
      leaves[
        current < 0
          ? direction === 1
            ? 0
            : leaves.length - 1
          : Math.max(0, Math.min(leaves.length - 1, current + direction))
      ];
    if (target) focusRow(target.row.id);
  };
  useLayoutEffect(() => {
    const pending = pendingFocus.current;
    if (pending === null) return;
    const row = rowElement(pending.key);
    if (!row) return;
    pendingFocus.current = null;
    const element =
      pending.selector === undefined ? row : row.querySelector<HTMLElement>(pending.selector);
    if (element === null) return;
    if (pending.from !== undefined) landNearest(element, pending.from);
    else element.focus({ preventScroll: true });
  });
  const openFocusedRow = () =>
    focusedRow()?.querySelector<HTMLElement>(`[${linkAttribute}]`)?.click();
  // Where the moving keys apply (`RowKeymap.moveFrom`): one of this table's rows must hold the
  // focus for keys the page answers itself everywhere else; for the others the table need only
  // hold a row, and the focus be in no other table, whose own keys are its to answer. Another
  // table is its box, or the frame its page draws around its control row, box and pager
  // (`TABLE_FRAME_ATTRIBUTE`), so a key pressed on an issue table's Columns menu or its Next ›
  // on the Priorities page walks no priority. This table's own frame holds its box.
  const canMove =
    keymap.moveFrom === "row"
      ? () => focusedRow() !== null
      : () => {
          const around = closestMatching(
            document.activeElement,
            `[${TABLE_FRAME_ATTRIBUTE}], [data-control="${CONTROL.dataTable}"]`
          );
          return (
            (around === null ||
              (scrollRef.current !== null && around.contains(scrollRef.current))) &&
            displayRows.some(({ row }) => !row.getIsGrouped())
          );
        };
  const { select } = keymap;
  useKeymap(keymap.scope, [
    {
      ...keymap.next,
      palette: false,
      run: () => moveFocus(1),
      when: canMove,
    },
    {
      ...keymap.previous,
      palette: false,
      run: () => moveFocus(-1),
      when: canMove,
    },
    {
      ...keymap.open,
      run: openFocusedRow,
      when: () => focusedRow() !== null,
    },
    {
      ...keymap.openOnEnter,
      palette: false,
      run: openFocusedRow,
      when: () =>
        document.activeElement?.matches(`[${rowAttribute}]`) === true && focusedRow() !== null,
    },
    ...(select === undefined
      ? []
      : [
          {
            // Selecting walks the list with the row in hand, as the moving keys do, so it is no
            // palette row.
            ...select,
            palette: false,
            run: () => {
              const key = focusedRow()?.getAttribute(rowAttribute);
              const row = key === null || key === undefined ? undefined : rowsById[key];
              if (row === undefined) return;
              if (!row.getIsSelected()) onMark?.(row.id);
              row.toggleSelected();
            },
            when: () => focusedRow() !== null,
          },
        ]),
  ]);
  return focusRow;
}
