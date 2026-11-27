import type { Column } from "@tanstack/react-table";
import type { ReactElement } from "react";
import { Link } from "react-router-dom";

import { focusVisibleRing, linkHoverText, linkText } from "../theme/classes";
import { Chip } from "./Chip";
import { type ControlSize, glyphTarget } from "./control-names";

/** A link a chip carries beside it, outside its remove control: the Inbox's agent chip carries
 *  the agent's live conversation. */
export interface FilterChipLink {
  /** The link's accessible name and its tooltip, since it draws a glyph: `Open live conversation
   *  with Planner`. */
  readonly label: string;
  /** Where it goes: an address of the app. */
  readonly to: string;
}

/** One narrowing that is on, as a page's control row draws it. */
export interface FilterChip {
  /** Tells two chips apart; the label, where a page never draws one label twice. */
  readonly key?: string;
  /** The chip's own text: the facet and its value as the page writes them - `Label: docs`,
   *  `Author alice`, `Needs you`. */
  readonly label: string;
  /** A link drawn beside the chip's remove control and outside it, so a press on the link goes
   *  where it says and takes nothing off, and Delete on it takes nothing off either
   *  (`ControlRow`). */
  readonly link?: FilterChipLink;
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
 * A chip given a link is drawn in a box with the link after it: two controls side by side, the
 * chip's remove control and the link, neither inside the other. The box is never wider than its
 * line (`max-w-full`), and the chip's own box gives way inside it (`min-w-0`), so a long chip is
 * cut short by its own `max-w-full` and the link stays in the line.
 */
export function filterChips(
  chips: readonly FilterChip[],
  size: ControlSize = "comfortable"
): ReactElement[] {
  return chips.map(({ key, label, link, remove, text }) => {
    const chip = (
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
    );
    if (link === undefined) return chip;
    return (
      <span className="flex max-w-full items-center" key={key ?? label}>
        <span className="flex min-w-0">{chip}</span>
        <Link
          aria-label={link.label}
          className={`inline-flex shrink-0 items-center justify-center rounded px-1 outline-none focus-visible:ring-2 ${glyphTarget[size]} ${focusVisibleRing} ${linkText} ${linkHoverText}`}
          title={link.label}
          to={link.to}
        >
          <span aria-hidden="true">↗</span>
        </Link>
      </span>
    );
  });
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
