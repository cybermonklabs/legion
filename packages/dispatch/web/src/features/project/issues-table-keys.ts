import type { Row } from "@tanstack/react-table";
import { type RefObject, useLayoutEffect, useRef } from "react";
import { useKeymap } from "../shell/keymap";
import { closestMatching } from "../shell/roving";
import type { Item } from "./issues-table-model";

/**
 * The keys a reader walks the table's rows with, the same ids and palette rows the list they
 * replace had: `j` and `k` move between rows, `o` and Enter open the row in hand, `x` selects
 * it. A row the virtualizer has not drawn yet is scrolled to first and focused when it lands.
 */
export function useRowKeymap({
  displayRows,
  rowsById,
  scrollRef,
  scrollToIndex,
}: {
  displayRows: readonly { row: Row<Item> }[];
  rowsById: Record<string, Row<Item>>;
  scrollRef: RefObject<HTMLDivElement | null>;
  scrollToIndex: (index: number) => void;
}): void {
  const pendingFocus = useRef<string | null>(null);
  const focusedRow = () => closestMatching(document.activeElement, "[data-issue-row]");
  const rowElement = (key: string) =>
    scrollRef.current?.querySelector<HTMLElement>(`[data-issue-row="${CSS.escape(key)}"]`);
  const moveFocus = (direction: 1 | -1) => {
    const leaves = displayRows.filter(({ row }) => !row.getIsGrouped());
    const current = leaves.findIndex(
      ({ row }) => row.id === focusedRow()?.getAttribute("data-issue-row")
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
    focusedRow()?.querySelector<HTMLElement>("[data-issue-link]")?.click();
  useKeymap("project", [
    {
      id: "list-next",
      keys: "j",
      label: "Next issue",
      palette: false,
      run: () => moveFocus(1),
      when: () => displayRows.some(({ row }) => !row.getIsGrouped()),
    },
    {
      id: "list-previous",
      keys: "k",
      label: "Previous issue",
      palette: false,
      run: () => moveFocus(-1),
      when: () => displayRows.some(({ row }) => !row.getIsGrouped()),
    },
    {
      id: "list-open",
      keys: "o",
      label: "Open issue",
      run: openFocusedRow,
      when: () => focusedRow() !== null,
    },
    {
      id: "list-open-enter",
      keys: "Enter",
      label: "Open the focused issue",
      palette: false,
      run: openFocusedRow,
      when: () => document.activeElement?.matches("[data-issue-row]") === true,
    },
    {
      id: "list-select",
      keys: "x",
      label: "Select or deselect the focused issue",
      palette: false,
      run: () => {
        const key = focusedRow()?.getAttribute("data-issue-row");
        if (key !== null && key !== undefined) rowsById[key]?.toggleSelected();
      },
      when: () => focusedRow() !== null,
    },
  ]);
}
