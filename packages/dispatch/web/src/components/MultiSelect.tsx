import { type KeyboardEvent, type ReactNode, useEffect, useId, useRef, useState } from "react";

import { useFocusHandOff } from "../hooks/useFocusHandOff";
import {
  borderDefault,
  borderStrong,
  card,
  cardHoverBorder,
  dangerText,
  focusBorder,
  inputClasses,
  pageFailedText,
  pageHairBorder,
  pageInkText,
  pageLineBorder,
  pageMarkBg,
  pageMutedText,
  pagePaneBg,
  pagePaperBg,
  pageQuietText,
  pageRowHoverBg,
  selectedCardBg,
  selectedCardBorder,
  surfaceMutedBg,
  textMutedOnSurface,
  textSecondaryOnCanvas,
  textSecondaryOnSurface,
} from "../theme/classes";
import { AnchoredPanel } from "./AnchoredPanel";
import { TruncatedText } from "./TruncatedText";
import { useTooltips } from "./tooltips";

function fold(value: string | undefined): string {
  return (value ?? "").trim().toLocaleLowerCase();
}

/**
 * Each colour the picker draws inside `NoTooltips`, the new issue page's, by the role `main`'s
 * colour has: the trigger is the page's header control (its rule and muted words on the
 * paper); the popover is the page's menu (the prototype's `#menu`: the hairline rule on the
 * paper), whose rows are its items, in the ink with the mark grey under the pointer and on the
 * active row; an empty list is quiet words, a refusal the failed red, and Clear the page's pane
 * with muted words. A detail is muted words too, not quiet ones: it is drawn on its row's mark
 * grey under the pointer and on the active row, where the quiet grey reads 4.11:1 in the light
 * scheme, under the 4.5:1 its words need. The search box keeps `main`'s focus rule, the one
 * focus indicator of the application.
 *
 * They stand in this table, not beside `main`'s classes, because `theme/text-on-background.test.ts`
 * pairs each text class with the nearest enclosing ground of the same file across both branches
 * of a ternary: a page colour written beside `main`'s would pair `main`'s words with the page's
 * paper and the page's words with `main`'s card. Each pair here is one `theme/classes.ts`
 * registers for the page, which `shared-controls.tooltips.test.tsx` holds.
 */
const ON_PAGE = {
  checkbox: pageHairBorder,
  clear: `${pagePaneBg} ${pageMutedText}`,
  detail: pageMutedText,
  empty: pageQuietText,
  failure: `${pageLineBorder} ${pageFailedText}`,
  panel: `${pageHairBorder} ${pagePaperBg}`,
  search: `${pageHairBorder} ${pagePaperBg} ${pageInkText} placeholder:text-page-quiet ${focusBorder}`,
  trigger: `min-h-11 rounded-lg border px-3 py-2 text-sm font-semibold ${pageLineBorder} ${pagePaperBg} ${pageMutedText}`,
} as const;

export interface MultiSelectProps {
  /** Trigger content; defaults to the label with the selection count (`Labels · 2`). */
  readonly children?: ReactNode;
  readonly disabled?: boolean;
  /** The trigger is busy with what the last close committed: it stays focusable, and says so
   *  rather than looking actionable and doing nothing. */
  readonly ariaDisabled?: boolean;
  /** Shown under the list, e.g. a host's validation message for a rejected `onCreate`. */
  readonly error?: string | null;
  /** Shown in place of the list when nothing is left to choose or nothing matches the search. */
  readonly emptyMessage: string;
  /** The concept the control picks from: the default trigger text, `Clear <label>`, and the
   *  listbox's accessible name. */
  readonly label: string;
  readonly onChange: (selected: string[]) => void;
  /** Enables the `Create "<query>"` row for a search that matches no option. */
  readonly onCreate?: (value: string) => void;
  readonly onOpenChange: (open: boolean) => void;
  /** Every keystroke in the search input, so a host can drop a message it showed for the
   *  previous query (a rejected create). */
  readonly onQueryChange?: (query: string) => void;
  readonly open: boolean;
  /** The values the host stores (URL keys, label strings) in list order. */
  readonly options: readonly string[];
  /** Row text for a value; the search matches this text. Defaults to the value itself. */
  readonly optionLabel?: (value: string) => string;
  /** Muted text at the end of a value's row, such as how many items carry it, or `undefined`
   *  for a row with no detail at all. The row's accessible name reads it after the label and a
   *  comma ("api, 3"), never run together. */
  readonly optionDetail?: (value: string) => string | undefined;
  /** What the row's accessible name says in place of `optionDetail`'s text, such as "3 PRs" for
   *  a bare count. Defaults to that text. */
  readonly optionDetailLabel?: (value: string) => string;
  readonly searchLabel: string;
  readonly selected: readonly string[];
  readonly triggerAriaLabel?: string;
  readonly triggerClassName?: string;
}

/**
 * The one searchable multi-select: a trigger button and a portalled popover holding a
 * `combobox` search input over an `aria-multiselectable` listbox of checkbox rows. Arrow keys
 * move the active option, Enter toggles it (or creates the typed value), Escape and Tab close;
 * an outside pointer press closes too, and closing returns focus to the trigger. The host owns
 * `open`, so it can stage a draft on open and commit on close.
 */
export function MultiSelect({
  ariaDisabled = false,
  children,
  disabled = false,
  emptyMessage,
  error = null,
  label,
  onChange,
  onCreate,
  onOpenChange,
  onQueryChange,
  open,
  options,
  optionDetail,
  optionDetailLabel,
  optionLabel = (value) => value,
  searchLabel,
  selected,
  triggerAriaLabel,
  triggerClassName,
}: MultiSelectProps): ReactNode {
  const id = useId();
  const tooltips = useTooltips();
  const trigger =
    triggerClassName ??
    (tooltips
      ? `min-h-11 rounded-lg border px-3 py-2 text-sm font-semibold ${borderDefault} ${surfaceMutedBg} ${textSecondaryOnCanvas}`
      : ON_PAGE.trigger);
  const listboxId = `${id}-options`;
  const optionId = (value: string) => `${id}-option-${encodeURIComponent(value)}`;
  const [query, setQuery] = useState("");
  const [activeValue, setActiveValue] = useState<string | undefined>(undefined);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const wasOpen = useRef(false);
  /** Whether the reader has pressed the Create row, or Clear, while it is drawn: each press takes
   *  its own element away. */
  const createPressed = useRef(false);
  const clearPressed = useRef(false);

  const searchText = query.trim();
  const needle = fold(searchText);
  // A host's label function can answer nothing for a value it does not know — a `?status=` of
  // another deploy, say — and a value with no label is still an option, read as itself.
  const labelOf = (option: string) => optionLabel(option) ?? option;
  const visibleOptions = options.filter((option) => fold(labelOf(option)).includes(needle));
  const createValue =
    onCreate !== undefined &&
    searchText !== "" &&
    !options.some((option) => fold(option) === fold(searchText))
      ? searchText
      : undefined;
  const rows = createValue === undefined ? visibleOptions : [...visibleOptions, createValue];
  const activeRow = activeValue !== undefined && rows.includes(activeValue) ? activeValue : rows[0];

  // What the reader picked, compared as the server compares a label: a URL that lower-cased
  // `Epic` still ticks the option the project spells `Epic`, and ticking it again removes it
  // rather than adding a second spelling.
  const isPicked = (value: string) => selected.some((current) => fold(current) === fold(value));
  const toggle = (value: string) => {
    setActiveValue(value);
    onChange(
      isPicked(value)
        ? selected.filter((current) => fold(current) !== fold(value))
        : [...selected, value]
    );
  };
  const create = (value: string) => {
    if (onCreate === undefined) return;
    onCreate(value);
    setQuery("");
    setActiveValue(value);
  };

  // Opening starts a fresh search and focuses it; closing returns focus to the trigger.
  useEffect(() => {
    if (open) {
      setQuery("");
      setActiveValue(undefined);
      inputRef.current?.focus();
    } else if (wasOpen.current) {
      triggerRef.current?.focus();
    }
    wasOpen.current = open;
  }, [open]);

  // The Create row and Clear are buttons, as the rows are, so the focus can reach one, as a screen
  // reader's cursor puts it there. A press of either takes it away: Clear as the pick empties, the
  // Create row as its value is made and the search empties. Where it held the focus, the browser
  // would drop that to the body, where Escape no longer reaches the popover, so the search box
  // takes it. Each notes its own press, which is forgotten as that element leaves, with the focus
  // or not (`useFocusHandOff`'s `pressed`): a pointer press leaves the focus in the search box
  // (`onMouseDown` below), and its element's going then answers no later loss. A Create row the
  // options take away - a read of them coming to name the value it offers - holds no press, and
  // its focus stays on the page's body, as for every change the reader did not make. A popover
  // that closes takes the search box along, and the close above gives the focus back to the
  // trigger instead.
  const createLeft = useFocusHandOff(() => inputRef.current, createPressed);
  const clearLeft = useFocusHandOff(() => inputRef.current, clearPressed);

  const handleKeyDown = (event: KeyboardEvent<HTMLInputElement>) => {
    if (event.key === "ArrowDown" && rows.length > 0) {
      event.preventDefault();
      const index = activeRow === undefined ? -1 : rows.indexOf(activeRow);
      setActiveValue(rows[(index + 1) % rows.length]);
      return;
    }
    if (event.key === "ArrowUp" && rows.length > 0) {
      event.preventDefault();
      const index = activeRow === undefined ? 0 : rows.indexOf(activeRow);
      setActiveValue(rows[(index - 1 + rows.length) % rows.length]);
      return;
    }
    if (event.key === "Enter" && activeRow !== undefined) {
      event.preventDefault();
      if (activeRow === createValue) {
        create(activeRow);
      } else {
        toggle(activeRow);
      }
      return;
    }
    if (event.key === "Tab") {
      onOpenChange(false);
    }
  };

  const rowClassName = (active: boolean) =>
    tooltips
      ? `flex min-h-11 w-full items-center gap-2 border-b px-3 py-2 text-left text-sm last:border-b-0 ${borderDefault} ${textSecondaryOnSurface} ${cardHoverBorder} ${
          active ? `${selectedCardBg} ${selectedCardBorder}` : ""
        }`
      : `flex min-h-11 w-full items-center gap-2 border-b px-3 py-2 text-left text-sm last:border-b-0 ${pageLineBorder} ${pageInkText} ${pageRowHoverBg} ${
          active ? pageMarkBg : ""
        }`;

  return (
    <>
      <button
        aria-disabled={ariaDisabled || undefined}
        aria-expanded={open}
        aria-haspopup="listbox"
        aria-label={triggerAriaLabel}
        className={trigger}
        disabled={disabled}
        onClick={() => onOpenChange(!open)}
        ref={triggerRef}
        type="button"
      >
        {children ?? (selected.length === 0 ? label : `${label} · ${selected.length}`)}
      </button>
      {open ? (
        <AnchoredPanel
          anchorRef={triggerRef}
          className={`z-50 max-h-72 w-64 max-w-[calc(100vw-1rem)] overflow-auto rounded-lg border shadow-lg ${tooltips ? `${card} ${borderDefault}` : ON_PAGE.panel}`}
          onDismiss={() => onOpenChange(false)}
        >
          <div className={`sticky top-0 border-b p-2 ${tooltips ? card : ON_PAGE.panel}`}>
            <input
              aria-activedescendant={activeRow === undefined ? undefined : optionId(activeRow)}
              aria-autocomplete="list"
              aria-controls={listboxId}
              aria-expanded="true"
              aria-label={searchLabel}
              className={`block min-h-11 w-full rounded-lg px-3 py-2 text-sm outline-none ${tooltips ? inputClasses(true) : ON_PAGE.search}`}
              disabled={disabled}
              onChange={(event) => {
                setQuery(event.target.value);
                setActiveValue(undefined);
                onQueryChange?.(event.target.value);
              }}
              onKeyDown={handleKeyDown}
              placeholder={searchLabel}
              ref={inputRef}
              role="combobox"
              type="search"
              value={query}
            />
          </div>
          <div
            aria-label={`${label} options`}
            aria-multiselectable="true"
            id={listboxId}
            role="listbox"
          >
            {visibleOptions.map((option) => {
              const isSelected = isPicked(option);
              const label = labelOf(option);
              const detail = optionDetail?.(option);
              const spokenDetail = optionDetailLabel?.(option) ?? detail;
              return (
                <button
                  aria-label={spokenDetail === undefined ? undefined : `${label}, ${spokenDetail}`}
                  aria-selected={isSelected}
                  className={rowClassName(activeRow === option)}
                  disabled={disabled}
                  id={optionId(option)}
                  key={option}
                  onClick={() => toggle(option)}
                  // A host can order the rows by the pick (`IssueLabels`, `ComponentPicker`), so a
                  // press can move its row, and React's move drops a focus on the row to the body.
                  // A press of a row that holds the focus, as a screen reader's cursor puts it
                  // there, gives it to the search box first; a pointer press focuses no row
                  // (`onMouseDown` below), and leaves the focus where it is.
                  onClickCapture={(event) => {
                    if (event.currentTarget === document.activeElement) inputRef.current?.focus();
                  }}
                  onMouseDown={(event) => event.preventDefault()}
                  role="option"
                  type="button"
                >
                  <span
                    aria-hidden="true"
                    className={`grid size-4 shrink-0 place-items-center rounded border text-xs leading-none ${tooltips ? borderStrong : ON_PAGE.checkbox}`}
                  >
                    {isSelected ? "✓" : ""}
                  </span>
                  {tooltips ? (
                    <TruncatedText className="min-w-0 flex-1" title={label}>
                      {label}
                    </TruncatedText>
                  ) : (
                    // Inside `NoTooltips` (the new issue page) no `title` can give back what an
                    // ellipsis cut, so the label wraps instead and is read whole.
                    <span className="min-w-0 flex-1 [overflow-wrap:anywhere]">{label}</span>
                  )}
                  {detail === undefined ? null : (
                    <span
                      className={`shrink-0 text-xs tabular-nums ${tooltips ? textMutedOnSurface : ON_PAGE.detail}`}
                    >
                      {detail}
                    </span>
                  )}
                </button>
              );
            })}
            {createValue === undefined ? null : (
              <button
                aria-selected="false"
                className={rowClassName(activeRow === createValue)}
                disabled={disabled}
                id={optionId(createValue)}
                onClick={() => create(createValue)}
                onClickCapture={() => {
                  createPressed.current = true;
                }}
                onMouseDown={(event) => event.preventDefault()}
                ref={createLeft}
                role="option"
                type="button"
              >
                <span aria-hidden="true" className="w-4 shrink-0 text-center">
                  +
                </span>
                Create &quot;{createValue}&quot;
              </button>
            )}
            {rows.length === 0 ? (
              <p className={`px-3 py-2 text-sm ${tooltips ? textMutedOnSurface : ON_PAGE.empty}`}>
                {emptyMessage}
              </p>
            ) : null}
          </div>
          {error === null ? null : (
            <p
              className={`border-t px-3 py-2 text-sm ${tooltips ? `${borderDefault} ${dangerText}` : ON_PAGE.failure}`}
            >
              {error}
            </p>
          )}
          {selected.length === 0 ? null : (
            <div className={`sticky bottom-0 border-t p-2 ${tooltips ? card : ON_PAGE.panel}`}>
              <button
                className={`min-h-11 w-full rounded-lg px-3 py-2 text-sm font-medium ${tooltips ? `${surfaceMutedBg} ${textSecondaryOnSurface}` : ON_PAGE.clear}`}
                disabled={disabled}
                onClick={() => onChange([])}
                onClickCapture={() => {
                  clearPressed.current = true;
                }}
                onMouseDown={(event) => event.preventDefault()}
                ref={clearLeft}
                type="button"
              >
                Clear {label.toLocaleLowerCase()}
              </button>
            </div>
          )}
        </AnchoredPanel>
      ) : null}
    </>
  );
}
