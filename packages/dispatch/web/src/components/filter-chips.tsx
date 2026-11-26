import type { Column } from "@tanstack/react-table";
import type { ReactNode } from "react";

import { Chip } from "./Chip";
import type { ControlSize } from "./control-names";

/** One narrowing that is on, as a page's control row draws it. */
export interface FilterChip {
  /** Tells two chips apart; the label, where a page never draws one label twice. */
  readonly key?: string;
  /** The chip's own text: the facet and its value as the page writes them - `Label: docs`,
   *  `Author alice`, `Needs you`. */
  readonly label: string;
  /** What the chip shows where it says more than its label, which still names it: the List's
   *  `only=` chip adds how many of its issues are listed, `Showing: <title> · 2 of 3 issues`,
   *  and is named for its label alone. The label where unset. */
  readonly text?: string;
  /** Takes this narrowing off, and no other. */
  readonly remove: () => void;
}

/**
 * The chips of the narrowings that are on, for a `ControlRow`, on every page that has a row: one
 * removable chip per narrowing, whose press takes that narrowing off. Each is named for what the
 * press does, `Remove <label> filter`, the same words on every page, so a screen reader hears
 * one name for one act and a browser row finds a chip the same way everywhere.
 *
 * A chip here stands for a narrowing that is on, so it takes the selected fill; a resting chip
 * would read as a narrowing the reader could still switch on. Its text is cut where the row is
 * too narrow, with the whole of it as its `title`, since a search can be 1,000 characters long.
 *
 * Answers undefined for no narrowing, which is what the row's `chips` takes for no chips.
 */
export function filterChips(
  chips: readonly FilterChip[],
  size?: ControlSize
): ReactNode[] | undefined {
  if (chips.length === 0) return undefined;
  return chips.map(({ key, label, remove, text }) => (
    <Chip
      aria-label={`Remove ${label} filter`}
      key={key ?? label}
      onClick={remove}
      removable
      selected
      size={size}
      title={text ?? label}
    >
      {text ?? label}
    </Chip>
  ));
}

/** What a table column's filter holds, as its chip names it: the values picked, or the text
 *  typed as one value; none while it holds nothing. */
export function columnFilterPicks(value: unknown): string[] {
  if (Array.isArray(value)) return value.map(String);
  return value === undefined || value === null || value === "" ? [] : [String(value)];
}

/**
 * The chip of one table column's filter: `<header>: <values>`, each value in the words the
 * header's own filter lists it in (the column's `show`), so the row and the header name one
 * narrowing alike. A column the reader hid with the columns menu keeps its filter, and this chip
 * is then the one place the reader sees it and takes it off. Its press clears
 * the column's filter, unless the page says what taking it off leaves (`remove`).
 */
export function columnFilterChip<TData>(
  column: Column<TData, unknown>,
  picks: readonly string[],
  remove: () => void = () => column.setFilterValue(undefined)
): FilterChip {
  const show = column.columnDef.meta?.show ?? ((value: string) => value);
  return {
    key: `column:${column.id}`,
    label: `${String(column.columnDef.header)}: ${picks.map(show).join(", ")}`,
    remove,
  };
}

/** The chip of every column filter of a table that holds anything, in the table's column order
 *  and whether its column is shown or hidden (`columnFilterChip`). */
export function columnFilterChips<TData>(columns: readonly Column<TData, unknown>[]): FilterChip[] {
  return columns.flatMap((column) => {
    const picks = columnFilterPicks(column.getFilterValue());
    return picks.length === 0 ? [] : [columnFilterChip(column, picks)];
  });
}
