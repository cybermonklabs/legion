/** Where keyboard focus sits on the Board: a column or rail by its id, and the card at `index` in
 *  it, or `null` on the column or rail itself. */
export interface BoardFocus {
  readonly column: string;
  readonly index: number | null;
}

/** One column as the keys walk it, in the Board's order: a rail draws no card to focus. */
export interface KeyColumn {
  readonly id: string;
  readonly rail: boolean;
  readonly size: number;
}

export type RoveKey = "j" | "k" | "h" | "l";
export type MoveDirection = "down" | "up" | "prev" | "next";

/** The card at `index` of `column`, clamped into the column; the column itself when it is empty
 *  or a rail. */
function landIn(column: KeyColumn, index: number | null): BoardFocus {
  if (column.size === 0 || column.rail) return { column: column.id, index: null };
  return { column: column.id, index: Math.min(Math.max(index ?? 0, 0), column.size - 1) };
}

/**
 * The next focus target for a roving key, over the columns the Board draws in its order. `j`/`k`
 * clamp within the column; `h`/`l` step to the next column (rails included) keeping the index,
 * clamped into the shorter column, and land on the column or rail itself when it holds no card to
 * focus. At the Board's ends the focus stays. With nothing focused, `j`/`l` pick the first card
 * of the first column that draws one and `k`/`h` the last card of the last; with no card to pick
 * they land on the first or last column itself. Undefined on a Board with no column.
 */
export function focusTarget(
  columns: readonly KeyColumn[],
  current: BoardFocus | null,
  key: RoveKey
): BoardFocus | undefined {
  const first = key === "j" || key === "l";
  if (current === null) {
    const ordered = first ? columns : [...columns].reverse();
    const column = ordered.find((entry) => entry.size > 0 && !entry.rail);
    if (column !== undefined) return { column: column.id, index: first ? 0 : column.size - 1 };
    const edge = ordered[0];
    return edge === undefined ? undefined : { column: edge.id, index: null };
  }
  const position = columns.findIndex((entry) => entry.id === current.column);
  const here = columns[position];
  if (here === undefined) return undefined;
  switch (key) {
    case "j":
    case "k":
      return current.index === null
        ? current
        : landIn(here, current.index + (key === "j" ? 1 : -1));
    case "h":
    case "l": {
      const adjacent = columns[position + (key === "l" ? 1 : -1)];
      return adjacent === undefined ? current : landIn(adjacent, current.index);
    }
  }
}

/**
 * Where a key move sends the card at `from`, as a place in the target column counted without the
 * card. `down`/`up` shift it one place and stop at the column's ends; `prev`/`next` put it at the
 * top of the column before or after it in the Board's order, rails included (Linear's rule for
 * keyboard status moves), and stop at the Board's ends. Undefined where there is nothing to do.
 */
export function keyboardMove(
  columns: readonly KeyColumn[],
  from: { readonly column: string; readonly index: number },
  direction: MoveDirection
): { column: string; index: number } | undefined {
  const position = columns.findIndex((entry) => entry.id === from.column);
  const here = columns[position];
  if (here === undefined) return undefined;
  switch (direction) {
    case "down":
      return from.index >= here.size - 1 ? undefined : { column: here.id, index: from.index + 1 };
    case "up":
      return from.index <= 0 ? undefined : { column: here.id, index: from.index - 1 };
    case "prev":
    case "next": {
      const adjacent = columns[position + (direction === "next" ? 1 : -1)];
      return adjacent === undefined ? undefined : { column: adjacent.id, index: 0 };
    }
  }
}

/** What the live region reads after a move: `"CORE-12 → Todo, position 2 of 5"`, the place the
 *  card lands in its column, or `"CORE-12 → Done, closed"`, since a card moved into Done is
 *  closed, and one that leaves the view has no place in it. */
export function announceMove(
  key: string,
  column: string,
  title: string,
  position: { readonly at: number; readonly of: number } | undefined
): string {
  return column === "done" || position === undefined
    ? `${key} → ${title}${column === "done" ? ", closed" : ""}`
    : `${key} → ${title}, position ${position.at} of ${position.of}`;
}
