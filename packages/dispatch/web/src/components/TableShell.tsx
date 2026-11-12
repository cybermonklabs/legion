import {
  createContext,
  type HTMLAttributes,
  type ReactNode,
  type TdHTMLAttributes,
  type ThHTMLAttributes,
  useContext,
  useMemo,
} from "react";

import {
  borderDefault,
  card,
  dangerText,
  successText,
  surfaceBg,
  surfaceMutedBg,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../theme/classes";
import { CONTROL, type ControlSize } from "./control-names";

/** Everything one density of the shell draws, so the two sit side by side in one place. */
interface TableRecipe {
  /** The text colour every row takes unless a row or a cell says otherwise. */
  readonly body: string;
  readonly cell: string;
  /** The cell of the line drawn in place of the rows: roomier than a row of data. */
  readonly empty: string;
  readonly head: string;
  readonly headCell: string;
  readonly row: string;
  /** What a head takes on top of `head` when it stays at the top of the scrolling box. The
   *  comfortable head already paints its own background; the compact one has none to stick
   *  over the rows with. */
  readonly stickyHead: string;
  readonly table: string;
  /** `relative` is load-bearing: a head cell's `sr-only` span is absolutely positioned, and with
   *  no positioned ancestor inside the scroll box its containing block is the page, so it sits
   *  at the scrolled table's own x and widens the document past the viewport. `overflow-x-auto`
   *  computes `overflow-y: auto` as well, so a caller that caps the box's height (the delivery
   *  measures panel's `max-h-48`) scrolls its rows under a sticky head with no overflow class of
   *  its own. */
  readonly wrapper: string;
}

const RECIPE: Record<ControlSize, TableRecipe> = {
  comfortable: {
    body: "",
    cell: "px-4 py-3",
    empty: `px-4 py-5 ${textMutedOnSurface}`,
    head: `border-b ${surfaceMutedBg} ${borderDefault} ${textSecondaryOnSurface}`,
    headCell: "px-4 py-3 font-semibold",
    row: `border-b last:border-0 ${borderDefault}`,
    stickyHead: "sticky top-0",
    table: "w-full text-left text-sm",
    wrapper: `relative overflow-x-auto rounded-xl border ${card}`,
  },
  compact: {
    body: textSecondaryOnSurface,
    cell: "px-2 py-1",
    empty: `px-2 py-1 ${textMutedOnSurface}`,
    head: textMutedOnSurface,
    headCell: "px-2 py-1 font-normal",
    row: `border-t ${borderDefault}`,
    stickyHead: `sticky top-0 ${surfaceBg}`,
    table: "w-full text-xs tabular-nums",
    wrapper: "relative overflow-x-auto",
  },
};

/** How a row's or a cell's text reads. It is the one say a caller has over a colour here:
 *  `default` keeps the colour of the body or the row around it. */
export type TableTone = "danger" | "default" | "muted" | "primary" | "secondary" | "success";

const TONE: Record<TableTone, string> = {
  danger: dangerText,
  default: "",
  muted: textMutedOnSurface,
  primary: textPrimaryOnSurface,
  secondary: textSecondaryOnSurface,
  success: successText,
};

/** How heavy a cell's text is: a figure a reader looks for first is set against the rest. */
export type TableWeight = "medium" | "normal" | "semibold";

const WEIGHT: Record<TableWeight, string> = {
  medium: "font-medium",
  normal: "",
  semibold: "font-semibold",
};

/** Which edge a column's text sits against. A head cell always says it, since a `th` with no
 *  text-align of its own is centred by the browser. */
export type TableAlign = "left" | "right";

const ALIGN: Record<TableAlign, string> = { left: "text-left", right: "text-right" };

/** The shell's classes for one part, with the tone, weight and alignment its caller chose; a
 *  choice that adds no class contributes nothing. */
function joined(...parts: readonly string[]): string {
  return parts.filter((part) => part !== "").join(" ");
}

interface TableStyle {
  readonly size: ControlSize;
  readonly stickyHead: boolean;
}

/** What the shell tells its head, rows and cells, so a caller never passes the density down. */
const TableStyleContext = createContext<TableStyle>({ size: "comfortable", stickyHead: false });

export interface TableShellProps {
  /** The head and the body, in that order. */
  readonly children: ReactNode;
  /** The caller's outer placement alone - a maximum width, a maximum height, a margin. The look
   *  of the table inside it is the shell's. */
  readonly className?: string;
  /** The table's accessible name. */
  readonly label: string;
  /** How much room a row takes. */
  readonly size?: ControlSize;
  /** Whether the head stays at the top of the box while the rows scroll under it. */
  readonly stickyHead?: boolean;
}

/**
 * The scroll box and the `<table>` in it: the one table of a page that offers no search, no
 * sorting and no filtering. `DataTable` is the other one - the TanStack table whose columns
 * sort, filter and resize - and between them they are every table under `web/src`, so no page
 * builds one out of `<table>` markup of its own.
 *
 * A caller decides where the box sits (`className`, its own outer placement), what the table is
 * called (`label`), how much room a row takes (`size`) and whether the head stays put while the
 * rows scroll under it (`stickyHead`). Every colour, border, padding, radius and font class is
 * this file's, keyed by the density, which is why no part of it takes a `className`: a cell says
 * what its text *is* - a key, a figure, a count nobody needs to read first - and the shell says
 * what that looks like.
 *
 * Not to be confused with `features/project/issues-table-rows.tsx`'s `TableHeaderCell`, which is
 * the sorting, filtering, resizable header cell `DataTable` draws.
 */
export function TableShell({
  children,
  className,
  label,
  size = "comfortable",
  stickyHead = false,
}: TableShellProps): ReactNode {
  const recipe = RECIPE[size];
  const style = useMemo(() => ({ size, stickyHead }), [size, stickyHead]);
  return (
    <div
      className={className === undefined ? recipe.wrapper : `${recipe.wrapper} ${className}`}
      data-control={CONTROL.tableShell}
    >
      <table aria-label={label} className={recipe.table}>
        <TableStyleContext value={style}>{children}</TableStyleContext>
      </table>
    </div>
  );
}

export interface TableHeadProps {
  /** The head cells of the one header row. */
  readonly children: ReactNode;
}

/** The head, and the single header row in it. */
export function TableHead({ children }: TableHeadProps): ReactNode {
  const { size, stickyHead } = useContext(TableStyleContext);
  const recipe = RECIPE[size];
  return (
    <thead className={stickyHead ? `${recipe.stickyHead} ${recipe.head}` : recipe.head}>
      <tr>{children}</tr>
    </thead>
  );
}

export interface TableHeadCellProps
  extends Omit<ThHTMLAttributes<HTMLTableCellElement>, "className"> {
  readonly align?: TableAlign;
}

/** One column's header. `scope="col"` unless the caller names another. */
export function TableHeadCell({
  align = "left",
  children,
  scope = "col",
  ...rest
}: TableHeadCellProps): ReactNode {
  const { size } = useContext(TableStyleContext);
  return (
    <th className={joined(RECIPE[size].headCell, ALIGN[align])} scope={scope} {...rest}>
      {children}
    </th>
  );
}

export interface TableBodyProps {
  /** The rows, and the empty state in their place when there are none. */
  readonly children: ReactNode;
}

/** The rows' own group, which carries the text colour a row reads in. */
export function TableBody({ children }: TableBodyProps): ReactNode {
  const { size } = useContext(TableStyleContext);
  const body = RECIPE[size].body;
  return <tbody className={body === "" ? undefined : body}>{children}</tbody>;
}

export interface TableRowProps extends Omit<HTMLAttributes<HTMLTableRowElement>, "className"> {
  /** How the whole row reads: `muted` is the row whose numbers are not final. */
  readonly tone?: TableTone;
}

/** One row of data. */
export function TableRow({ children, tone = "default", ...rest }: TableRowProps): ReactNode {
  const { size } = useContext(TableStyleContext);
  return (
    <tr className={joined(RECIPE[size].row, TONE[tone])} {...rest}>
      {children}
    </tr>
  );
}

export interface TableCellProps extends Omit<TdHTMLAttributes<HTMLTableCellElement>, "className"> {
  readonly align?: TableAlign;
  /** Figures that line up column-wise, such as a key or a commit. */
  readonly mono?: boolean;
  readonly tone?: TableTone;
  readonly weight?: TableWeight;
}

/** One cell of a row. */
export function TableCell({
  align = "left",
  children,
  mono = false,
  tone = "default",
  weight = "normal",
  ...rest
}: TableCellProps): ReactNode {
  const { size } = useContext(TableStyleContext);
  return (
    <td
      className={joined(
        RECIPE[size].cell,
        ALIGN[align],
        mono ? "font-mono" : "",
        WEIGHT[weight],
        TONE[tone]
      )}
      {...rest}
    >
      {children}
    </td>
  );
}

export interface TableEmptyProps {
  /** What the table says when it lists nothing. */
  readonly children: ReactNode;
  /** How many columns the line spans: every one of them. */
  readonly colSpan: number;
}

/** The one line a table draws in place of its rows when it has none. */
export function TableEmpty({ children, colSpan }: TableEmptyProps): ReactNode {
  const { size } = useContext(TableStyleContext);
  return (
    <tr>
      <td className={RECIPE[size].empty} colSpan={colSpan}>
        {children}
      </td>
    </tr>
  );
}
