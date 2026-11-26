import { SEARCH_QUERY_MAX } from "@legion/contracts/dispatch-tools";
import type { KeyboardEvent, ReactNode, Ref } from "react";

import { focusVisibleRing, inputClasses, kbdHint } from "../theme/classes";
import { CONTROL, type ControlSize, controlPadding } from "./control-names";

export interface SearchBoxProps {
  /** The caller's layout alone — a width, or how it shares its row. The fill, the border and the
   *  radius belong to the box, so one page's search never looks like another's. Where the box
   *  draws a `hint`, it is the layout of the box and its hint together. */
  readonly className?: string;
  /** The rows the typing walks, for the one box that drives a list of its own: the command bar,
   *  whose arrows move a highlight the box names in `aria-activedescendant` while focus stays
   *  here. A box without it is a plain field and claims no list. */
  readonly combobox?: {
    /** The highlighted row's id, or undefined while no row is highlighted. */
    readonly activeId?: string;
    /** The id of the listbox the box drives. */
    readonly controls: string;
    /** Whether that list is showing. */
    readonly expanded: boolean;
  };
  /** For a caller keeping a test id that a browser row already names. */
  readonly "data-testid"?: string;
  /** Hides the box and keeps it mounted, the words in it and the caret with it: the Inbox's,
   *  while a reading view stands in the place of its list (`Inbox.tsx`'s header line). */
  readonly hidden?: boolean;
  /** The key that puts the reader in the box, drawn inside the box at its end - the Inbox's `f`
   *  - and told to a screen reader as the box's shortcut (`aria-keyshortcuts`). The box keeps
   *  its `data-control`; the hint is a mark beside the text, drawn over the box's own room. */
  readonly hint?: string;
  /** The box's accessible name: it carries no visible label of its own. */
  readonly label: string;
  /** Defaults to what the server accepts, so no reader types a query it would refuse. `null`
   *  for the one box that lets a reader overrun it: the command bar names the limit instead of
   *  cutting the words they typed. */
  readonly maxLength?: number | null;
  /** For a host that takes what was typed once the reader leaves the box, such as the command
   *  bar's author filter, which no half-typed login may search on. */
  readonly onBlur?: () => void;
  readonly onChange: (value: string) => void;
  /** For a host that answers a key in the box, such as the reference picker taking Enter. */
  readonly onKeyDown?: (event: KeyboardEvent<HTMLInputElement>) => void;
  readonly placeholder?: string;
  /** The input itself, for a page whose shortcut puts the reader in it (the Inbox's `f`). */
  readonly ref?: Ref<HTMLInputElement>;
  readonly size?: ControlSize;
  readonly value: string;
}

/** The room a box with a `hint` leaves at its end for the hint, so no typed word runs under it;
 *  the control bar's placeholder sizer leaves it too (`ControlRow`). */
export const SEARCH_HINT_ROOM = "pr-8";

/**
 * The one search box: every page that narrows a list by typed words draws this, so the field a
 * reader meets on the Inbox is the field they meet on a project (`MultiSelect`'s own option
 * search is the one other `type="search"`, and it belongs to that panel). It carries the focus
 * ring as well as the border, because a page whose shortcut lands the reader here must show
 * them where they are.
 */
export function SearchBox({
  className,
  combobox,
  "data-testid": testId,
  hidden,
  hint,
  label,
  maxLength = SEARCH_QUERY_MAX,
  onBlur,
  onChange,
  onKeyDown,
  placeholder,
  ref,
  size = "comfortable",
  value,
}: SearchBoxProps): ReactNode {
  const own = `block min-w-0 rounded-lg border text-sm outline-none focus-visible:ring-2 ${controlPadding[size]} ${inputClasses(true)} ${focusVisibleRing}`;
  // The combobox's own attributes, which only a box driving a list may carry: a plain search
  // box has no rows to claim, and `aria-expanded` on one is an attribute its role refuses.
  const drives =
    combobox === undefined
      ? {}
      : {
          "aria-activedescendant": combobox.activeId,
          "aria-controls": combobox.controls,
          "aria-expanded": combobox.expanded,
          role: "combobox" as const,
        };
  // With a hint, the box fills a wrapper that takes the caller's layout, and leaves room at its
  // end for the hint.
  const boxClass =
    hint === undefined
      ? className === undefined
        ? own
        : `${own} ${className}`
      : `${own} w-full ${SEARCH_HINT_ROOM}`;
  const box = (
    <input
      aria-keyshortcuts={hint}
      aria-label={label}
      className={boxClass}
      data-control={CONTROL.searchBox}
      data-testid={testId}
      // The box itself where it is the whole control; with a hint, the wrapper below.
      hidden={hint === undefined ? hidden : undefined}
      maxLength={maxLength === null ? undefined : maxLength}
      onBlur={onBlur}
      onChange={(event) => onChange(event.target.value)}
      onKeyDown={onKeyDown}
      placeholder={placeholder}
      ref={ref}
      type="search"
      value={value}
      {...drives}
    />
  );
  if (hint === undefined) return box;
  return (
    <span
      className={`relative block min-w-0${className === undefined ? "" : ` ${className}`}`}
      hidden={hidden}
    >
      {box}
      {/* Hidden from a screen reader, which hears the key as the box's `aria-keyshortcuts`. */}
      <span
        aria-hidden="true"
        className={`pointer-events-none absolute top-1/2 right-2 -translate-y-1/2 rounded px-1.5 text-xs font-medium ${kbdHint}`}
      >
        <kbd className="font-sans">{hint}</kbd>
      </span>
    </span>
  );
}
