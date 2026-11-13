import type { Row } from "@tanstack/react-table";
import { type RefObject, useLayoutEffect, useRef } from "react";
import { type KeymapScope, useKeymap } from "../features/shell/keymap";
import { closestMatching } from "../features/shell/roving";

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
  /** Opens the row in hand; the one binding of the five the palette lists. */
  readonly open: RowKeyBinding;
  /** Opens the row in hand from Enter, which only a focused row answers. */
  readonly openOnEnter: RowKeyBinding;
  readonly select: RowKeyBinding;
}

/** Focuses the row whose id is `key`, or the element `selector` names inside it. */
export type FocusRow = (key: string, selector?: string) => HTMLElement | null;

/**
 * The keys a reader walks the table's rows with, under the ids and words the caller gives, the
 * same ids and palette rows the list they replace had: two keys move between rows, two open the
 * row in hand, one selects it. A row the virtualizer has not drawn yet is scrolled to first and
 * focused when it lands.
 *
 * It hands back that same way to a row (`FocusRow`), for whatever else has to put the focus on
 * one: the row now where it is drawn, and once it lands where it is not.
 */
export function useRowKeymap<TData>({
  displayRows,
  keymap,
  linkAttribute,
  onMark,
  rowAttribute,
  rowsById,
  scrollRef,
  scrollToIndex,
}: {
  displayRows: readonly { row: Row<TData> }[];
  keymap: RowKeymap;
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
  /** The row that takes the focus once the virtualizer draws it, and what inside it does. */
  const pendingFocus = useRef<{ key: string; selector: string | undefined } | null>(null);
  const focusedRow = () => closestMatching(document.activeElement, `[${rowAttribute}]`);
  const rowElement = (key: string) =>
    scrollRef.current?.querySelector<HTMLElement>(`[${rowAttribute}="${CSS.escape(key)}"]`);
  // What it focused, or null: a row the virtualizer has not drawn is focused once it lands, and a
  // key no row of this page holds moves nothing.
  const focusRow: FocusRow = (key, selector) => {
    const row = rowElement(key);
    if (row) {
      pendingFocus.current = null;
      const element = selector === undefined ? row : row.querySelector<HTMLElement>(selector);
      element?.focus();
      return element;
    }
    const index = displayRows.findIndex(({ row }) => row.id === key && !row.getIsGrouped());
    if (index !== -1) {
      pendingFocus.current = { key, selector };
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
    element?.focus({ preventScroll: true });
  });
  const openFocusedRow = () =>
    focusedRow()?.querySelector<HTMLElement>(`[${linkAttribute}]`)?.click();
  useKeymap(keymap.scope, [
    {
      ...keymap.next,
      palette: false,
      run: () => moveFocus(1),
      when: () => displayRows.some(({ row }) => !row.getIsGrouped()),
    },
    {
      ...keymap.previous,
      palette: false,
      run: () => moveFocus(-1),
      when: () => displayRows.some(({ row }) => !row.getIsGrouped()),
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
      when: () => document.activeElement?.matches(`[${rowAttribute}]`) === true,
    },
    {
      // Selecting walks the list with the row in hand, as the moving keys do, so it is no
      // palette row.
      ...keymap.select,
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
  ]);
  return focusRow;
}
