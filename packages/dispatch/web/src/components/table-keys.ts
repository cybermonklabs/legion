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

/**
 * The keys a reader walks the table's rows with, under the ids and words the caller gives, the
 * same ids and palette rows the list they replace had: two keys move between rows, two open the
 * row in hand, one selects it. A row the virtualizer has not drawn yet is scrolled to first and
 * focused when it lands.
 */
export function useRowKeymap<TData>({
  displayRows,
  keymap,
  linkAttribute,
  rowAttribute,
  rowsById,
  scrollRef,
  scrollToIndex,
}: {
  displayRows: readonly { row: Row<TData> }[];
  keymap: RowKeymap;
  /** The attribute on the link inside a row that opening the row follows. */
  linkAttribute: string;
  /** The attribute each drawn row carries, whose value is the row's id. */
  rowAttribute: string;
  rowsById: Record<string, Row<TData>>;
  scrollRef: RefObject<HTMLDivElement | null>;
  scrollToIndex: (index: number) => void;
}): void {
  const pendingFocus = useRef<string | null>(null);
  const focusedRow = () => closestMatching(document.activeElement, `[${rowAttribute}]`);
  const rowElement = (key: string) =>
    scrollRef.current?.querySelector<HTMLElement>(`[${rowAttribute}="${CSS.escape(key)}"]`);
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
    if (!target) return;
    pendingFocus.current = target.row.id;
    const element = rowElement(target.row.id);
    if (element) {
      element.focus();
      pendingFocus.current = null;
    } else scrollToIndex(displayRows.indexOf(target));
  };
  useLayoutEffect(() => {
    if (!pendingFocus.current) return;
    const element = rowElement(pendingFocus.current);
    if (element) {
      element.focus({ preventScroll: true });
      pendingFocus.current = null;
    }
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
        if (key !== null && key !== undefined) rowsById[key]?.toggleSelected();
      },
      when: () => focusedRow() !== null,
    },
  ]);
}
