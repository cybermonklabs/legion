import { SEARCH_QUERY_MAX } from "@legion/contracts/dispatch-tools";
import type { KeyboardEvent, ReactNode, Ref } from "react";

import { focusVisibleRing, inputClasses } from "../theme/classes";
import { CONTROL, type ControlSize, controlPadding } from "./control-names";

export interface SearchBoxProps {
  /** The caller's layout alone — a width, or how it shares its row. The fill, the border and the
   *  radius belong to the box, so one page's search never looks like another's. */
  readonly className?: string;
  /** For a caller keeping a test id that a browser row already names. */
  readonly "data-testid"?: string;
  /** The box's accessible name: it carries no visible label of its own. */
  readonly label: string;
  /** Defaults to what the server accepts, so no reader types a query it would refuse. */
  readonly maxLength?: number;
  readonly onChange: (value: string) => void;
  /** For a host that answers a key in the box, such as the reference picker taking Enter. */
  readonly onKeyDown?: (event: KeyboardEvent<HTMLInputElement>) => void;
  readonly placeholder?: string;
  /** The input itself, for a page whose shortcut puts the reader in it (the Inbox's `f`). */
  readonly ref?: Ref<HTMLInputElement>;
  readonly size?: ControlSize;
  readonly value: string;
}

/**
 * The one search box: every page that narrows a list by typed words draws this, so the field a
 * reader meets on the Inbox is the field they meet on a project (`MultiSelect`'s own option
 * search is the one other `type="search"`, and it belongs to that panel). It carries the focus
 * ring as well as the border, because a page whose shortcut lands the reader here must show
 * them where they are.
 */
export function SearchBox({
  className,
  "data-testid": testId,
  label,
  maxLength = SEARCH_QUERY_MAX,
  onChange,
  onKeyDown,
  placeholder,
  ref,
  size = "comfortable",
  value,
}: SearchBoxProps): ReactNode {
  const own = `block min-w-0 rounded-lg border text-sm outline-none focus-visible:ring-2 ${controlPadding[size]} ${inputClasses(true)} ${focusVisibleRing}`;
  return (
    <input
      aria-label={label}
      className={className === undefined ? own : `${own} ${className}`}
      data-control={CONTROL.searchBox}
      data-testid={testId}
      maxLength={maxLength}
      onChange={(event) => onChange(event.target.value)}
      onKeyDown={onKeyDown}
      placeholder={placeholder}
      ref={ref}
      type="search"
      value={value}
    />
  );
}
