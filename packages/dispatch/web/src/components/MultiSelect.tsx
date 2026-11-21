import {
  type KeyboardEvent,
  type ReactNode,
  useEffect,
  useId,
  useLayoutEffect,
  useRef,
  useState,
} from "react";

import { stepIndex } from "../features/shell/roving";
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
import { AnchoredPanel, type PanelDismissal } from "./AnchoredPanel";
import { type ControlName, type ControlSize, controlPadding } from "./control-names";
import { ChevronIcon } from "./DisclosureToggle";
import { focusLost } from "./focus-place";
import { TruncatedText } from "./TruncatedText";
import { useTooltips } from "./tooltips";

function fold(value: string | undefined): string {
  return (value ?? "").trim().toLocaleLowerCase();
}

/** How many rows Page Down and Page Up move the highlight, as the WAI-ARIA select-only combobox
 *  moves it. */
const PAGE_ROWS = 10;

/** The keys that move the highlight, which a closed trigger that takes the list's keys opens the
 *  list with (`keysOnTrigger`). */
const MOVE_KEYS: Readonly<Record<string, true>> = {
  ArrowDown: true,
  ArrowUp: true,
  End: true,
  Home: true,
  PageDown: true,
  PageUp: true,
};

/** A key a closed trigger that takes the list's keys opens the list with as its search: one
 *  letter or digit, in any script. Punctuation (`?`, `/`, `]`) is no type-ahead and opens nothing. */
const TYPED_KEY = /^[\p{L}\p{N}]$/u;

/** Where `key` moves the highlight in `rows` from `from`, or undefined for a key that moves
 *  nothing or rows that hold nothing. The arrows wrap at the ends and Home and End go to them, as
 *  a menu's choices step (`stepIndex`); Page Down and Page Up go `PAGE_ROWS` rows on and stop
 *  there. From no row, or one the rows do not hold, ArrowDown and Page Down count from before the
 *  first and ArrowUp from the first. */
function stepTo(
  key: string,
  rows: readonly string[],
  from: string | undefined
): string | undefined {
  if (rows.length === 0) return undefined;
  const index = from === undefined ? -1 : rows.indexOf(from);
  if (key === "PageDown") return rows[Math.min(index + PAGE_ROWS, rows.length - 1)];
  if (key === "PageUp") return rows[Math.max(index - PAGE_ROWS, 0)];
  const at = key === "ArrowUp" ? Math.max(index, 0) : index;
  const step = stepIndex(key, at, rows.length, "ArrowUp", "ArrowDown");
  return step === undefined ? undefined : rows[step];
}

/** What closed the list, which `onOpenChange` hands its host with the close: a single pick's
 *  pick, its trigger pressed again, or what dismisses the panel it stands in - Escape, Tab or
 *  Shift+Tab, a press outside it, its trigger gone (`PanelDismissal`). */
export type ListClosing = PanelDismissal | "pick" | "trigger";

/** `data-*` attributes a host writes on the list's own elements, for its own code to find
 *  them by: the trigger, and the text box, which the list portals out of the host's tree. */
export interface ListMarks {
  readonly search?: Readonly<Record<`data-${string}`, string>>;
  readonly trigger?: Readonly<Record<`data-${string}`, string>>;
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

interface SharedProps {
  /** Trigger content. A multi-select's defaults to the label with the selection count
   *  (`Labels · 2`), a single pick's to the picked value's row text, else to the label. */
  readonly children?: ReactNode;
  /** Whether the trigger ends in a chevron, as it does by default: the sign a sighted reader
   *  reads that the trigger opens a list (`aria-haspopup` tells a screen reader alone), with
   *  the trigger's text in front of it ending in an ellipsis where the trigger is too narrow for
   *  it. A trigger whose children are a look of their own passes `false` and draws them as they
   *  are: the tinted badge an ask card's hand-on and the issue header's Assignee show
   *  (`badge-select.ts`), each of which replaced a badge over an invisible select, and a table
   *  header's funnel, whose text-filter twin is a `PanelTrigger` that draws none either. */
  readonly chevron?: boolean;
  /** The text of the row `onCreate` adds for a search that matches no option, drawn from the
   *  typed value. Defaults to `Create "<value>"`. */
  readonly createLabel?: (value: string) => string;
  readonly disabled?: boolean;
  /** Which control of the vocabulary this is, written into the trigger's `data-control`: a
   *  column filter is a multi-select under its own name, while a multi-select a page draws for
   *  itself is a part of the vocabulary rather than one of its controls, and carries nothing. */
  readonly control?: ControlName;
  /** The trigger is busy with what the last close committed: it stays focusable, and says so
   *  rather than looking actionable and doing nothing. It only sets `aria-disabled`, so the
   *  trigger keeps its focus and its Tab stop and a press still opens the list, unless the host
   *  refuses the open in `onOpenChange`: the Agents page's Issue list, and each host whose write
   *  is out (`AssigneeControl`, `RepoControl`, `AskHandOn`). */
  readonly ariaDisabled?: boolean;
  /** Shown under the list, e.g. a host's validation message for a rejected `onCreate`. */
  readonly error?: string | null;
  /** Shown in place of the list when nothing is left to choose or nothing matches the search. A
   *  single pick shows it over its `onCreate` row too: that row takes the typed value as it is,
   *  and the message says no option holds it. */
  readonly emptyMessage: string;
  /** Whether opening moves the focus into the text box, as it does by default. A host that
   *  opens the list before its options have come passes `false`, and hands the focus in itself
   *  once they have. Read as the list opens. */
  readonly focusOnOpen?: boolean;
  /** The concept the control picks from: the default trigger text, `Clear <label>`, the
   *  listbox's accessible name, and the start of a single pick's trigger's accessible name. */
  readonly label: string;
  /** Marks the host's own code finds the trigger and the text box by. */
  readonly marks?: ListMarks;
  /** The trigger pressed, a pick or a dismissal: `true` to open, `false` with what closed it. */
  readonly onOpenChange: (open: boolean, closedBy?: ListClosing) => void;
  /** Every keystroke in the search input, so a host can drop a message it showed for the
   *  previous query (a rejected create). */
  readonly onQueryChange?: (query: string) => void;
  readonly open: boolean;
  /** The values the host stores (URL keys, label strings) in list order. */
  readonly options: readonly string[];
  /** Row text for a value, or `undefined` for a value the host has no text for, which then
   *  reads as itself; the search matches this text, and a single pick's trigger shows it for the
   *  picked value. Defaults to the value itself. */
  readonly optionLabel?: (value: string) => string | undefined;
  /** Muted text at the end of a value's row, such as how many items carry it, or `undefined`
   *  for a row with no detail at all. The row's accessible name reads it after the label and a
   *  comma ("api, 3"), never run together. */
  readonly optionDetail?: (value: string) => string | undefined;
  /** What the row's accessible name says in place of `optionDetail`'s text, such as "3 PRs" for
   *  a bare count. Defaults to that text. */
  readonly optionDetailLabel?: (value: string) => string;
  readonly searchLabel: string;
  /** How much room the default trigger takes outside `NoTooltips`; a `triggerClassName` decides
   *  for itself. */
  readonly size?: ControlSize;
  /** A multi-select trigger's accessible name in place of its text; a single pick's trigger's
   *  name starts with it in place of `label`, before what the trigger shows. */
  readonly triggerAriaLabel?: string;
  readonly triggerClassName?: string;
  /** Classes for the element that holds the trigger's text, beside the chevron: where a trigger
   *  stands in a `<select>`'s place and has to take its box, the text's line is the place to
   *  say so (`features/settings/classes.ts`, `settingsListText`). */
  readonly triggerTextClassName?: string;
  /** The `title` of the element that holds the trigger's text, beside the chevron, in place of
   *  a single pick's picked row text: for a host whose children mean more than that row says, as
   *  the issue header's Repository badge names the repository an issue inherits where its picked
   *  row reads Inherit. An empty string shows no tooltip. */
  readonly triggerTitle?: string;
}

/** Any number of values: a row ticks or unticks its value, and a `Clear` foot empties them. */
interface MultipleProps extends SharedProps {
  readonly mode?: "multiple";
  readonly onChange: (selected: string[]) => void;
  /** Enables the `createLabel` row for a search that matches no option, which adds the typed
   *  value. */
  readonly onCreate?: (value: string) => void;
  readonly selected: readonly string[];
}

/** One value: a pick hands it to `onPick`, closes the list and puts the focus on the trigger. */
interface SingleProps extends SharedProps {
  /** The closed trigger takes the list's keys, as a focused `<select>` does: ArrowDown, ArrowUp,
   *  Home, End, Page Down and Page Up open the list on the value picked and move the highlight by
   *  that key, and a letter or a digit opens it with that key in the text box (`TYPED_KEY`). Each
   *  is the trigger's own (`preventDefault`), so the page's keymap, which skips a handled key,
   *  never reads it; any other key, `?` among them, opens nothing and reaches the keymap, which
   *  reads a list's trigger as editable and fires no single key of the page on it. A host whose
   *  value is written the moment it is picked passes it (the assignee, the repository, an ask's
   *  hand-on), so a key moves only the highlight and only Enter or a click writes. Those three
   *  are this list rather than native selects (INFRA-2160's R6, and its departure D2): a select
   *  cannot write nothing on Escape, since Chromium keeps the highlighted option when Escape
   *  closes its open list, which the keyboard-step rule of the live header's selects
   *  (`useKeySteps` in `agent-view/SessionSettings.tsx`) cannot change. Without `keysOnTrigger`
   *  the closed trigger takes Enter, Space and a click, and every other key stays the page's:
   *  `]` on the Version picker is the page's next tab. */
  readonly keysOnTrigger?: boolean;
  readonly mode: "single";
  /** Enables the `createLabel` row for a search that matches no option, which hands the typed
   *  value, trimmed, to the host: `true` takes it, and the list closes as a pick does; `false`
   *  refuses it, and the list stays open with the text in its box. */
  readonly onCreate?: (value: string) => boolean;
  readonly onPick: (value: string) => void;
  /** The value picked now, which the list marks selected and the trigger shows; absent while
   *  the host holds none. */
  readonly value?: string;
}

export type MultiSelectProps = MultipleProps | SingleProps;

/**
 * The one searchable list a reader picks from: a trigger button and a portalled popover holding
 * a `combobox` search input that narrows the listbox under it. Arrow keys move the active option
 * and Enter takes it; Escape and Tab close with nothing taken, an outside pointer press closes
 * too, and closing returns the focus to the trigger from the list it closed (a focus the reader
 * or the host has put somewhere else stays there). The host owns `open`, so it can stage a draft
 * on open and commit on close, and every close tells it what closed the list (`ListClosing`).
 *
 * By default it picks many: the listbox is `aria-multiselectable`, a row (or Enter) ticks or
 * unticks its value in the checkbox drawn beside it, `onCreate` adds a row for a search that
 * matches no option, and a `Clear` foot empties the selection. `mode="single"` picks one: the
 * rows are plain options, the picked one `aria-selected` and marked, and a pick hands its one
 * value to `onPick`, closes the list and returns the focus to the trigger. Its `onCreate` row
 * hands a typed value no option holds to the host, which takes it, and the list closes as a pick
 * does, or refuses it, and the list stays open with the text in its box. It opens with its
 * picked value active, as a `<select>` and the WAI-ARIA select-only combobox do, so Enter there
 * keeps the value and the arrows move on from it; Escape picks nothing, changes no value and
 * returns the focus to the trigger. Its trigger is named for the list (`label`) and then for what
 * it shows, so the words a reader sees on it are in its name and a screen reader hears the list
 * and the value staged in it, as a `<select>` reads. While the list is open the trigger
 * names the panel holding it (`aria-controls`), the text box and the listbox both. The trigger
 * ends in a chevron, as a `<select>` ends in its arrow, unless its host draws a look of its own
 * (`chevron`). A single pick that passes `keysOnTrigger` also opens from its closed trigger by the
 * keys a `<select>` takes there, so those keys, too, only move the highlight.
 */
export function MultiSelect(props: MultiSelectProps): ReactNode {
  const {
    ariaDisabled = false,
    children,
    chevron = true,
    control,
    createLabel = (value: string) => `Create "${value}"`,
    disabled = false,
    emptyMessage,
    error = null,
    focusOnOpen = true,
    label,
    marks,
    onOpenChange,
    onQueryChange,
    open,
    options,
    optionDetail,
    optionDetailLabel,
    optionLabel = (value: string) => value,
    searchLabel,
    size = "comfortable",
    triggerAriaLabel,
    triggerClassName,
    triggerTextClassName = "",
    triggerTitle,
  } = props;
  const single = props.mode === "single";
  // What is picked now, a single pick's value as a list of one, so both modes read it alike.
  const selected =
    props.mode !== "single" ? props.selected : props.value === undefined ? [] : [props.value];
  const id = useId();
  const panelId = `${id}-panel`;
  const tooltips = useTooltips();
  const trigger =
    triggerClassName ??
    (tooltips
      ? `rounded-lg border text-sm font-semibold ${controlPadding[size]} ${borderDefault} ${surfaceMutedBg} ${textSecondaryOnCanvas}`
      : ON_PAGE.trigger);
  const listboxId = `${id}-options`;
  const nameId = `${id}-name`;
  const shownId = `${id}-shown`;
  const optionId = (value: string) => `${id}-option-${encodeURIComponent(value)}`;
  const [query, setQuery] = useState("");
  const [activeValue, setActiveValue] = useState<string | undefined>(undefined);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const wasOpen = useRef(false);
  // The panel's sticky text box at its top, and a multi-select's sticky Clear foot at its bottom,
  // which a row scrolled into sight is kept clear of.
  const searchHostRef = useRef<HTMLDivElement>(null);
  const clearHostRef = useRef<HTMLDivElement>(null);
  // What an open reads of this render: the value it starts on, and the options a move key that
  // opened it steps through. A host may build its options only while the list stands open (an
  // ask's hand-on), so the step is taken in the open render's options, not the closed trigger's.
  const opensOn = useRef<string | undefined>(undefined);
  const opensWith = useRef<readonly string[]>([]);
  // What a key on the closed trigger opened the list with, read as it opens: the move key whose
  // step picks the row it highlights, and the text it puts in the search box. A press of the
  // trigger opens it as a click does, on the value picked with an empty search.
  const openedBy = useRef<{ readonly key?: string; readonly query: string } | undefined>(undefined);
  const keysOnTrigger = props.mode === "single" && props.keysOnTrigger === true;

  const searchText = query.trim();
  const needle = fold(searchText);
  // A host's label function can answer nothing for a value it does not know — a `?status=` of
  // another deploy, say — and a value with no label is still an option, read as itself.
  const labelOf = (option: string) => optionLabel(option) ?? option;
  // The rows exist only in the open panel, so a closed list narrows nothing.
  const visibleOptions = open
    ? options.filter((option) => fold(labelOf(option)).includes(needle))
    : [];
  // A search that names an option, by its value or by the text its row shows, creates nothing.
  const createValue =
    open &&
    props.onCreate !== undefined &&
    searchText !== "" &&
    !options.some((option) => fold(option) === needle || fold(labelOf(option)) === needle)
      ? searchText
      : undefined;
  const rows = createValue === undefined ? visibleOptions : [...visibleOptions, createValue];
  const activeRow = activeValue !== undefined && rows.includes(activeValue) ? activeValue : rows[0];
  // The highlighted row's element, which the text box names (`aria-activedescendant`); none while
  // the list is closed, which draws no rows.
  const activeId = activeRow === undefined ? undefined : optionId(activeRow);
  // The empty message stands where no option is left. A multi-select's create row stands in its
  // place, since it adds an option; a single pick's row takes the typed value as it is, so the
  // message stays over it.
  const showsEmpty = visibleOptions.length === 0 && (single || createValue === undefined);

  // What the reader picked, compared as the server compares a label: a URL that lower-cased
  // `Epic` still ticks the option the project spells `Epic`, and ticking it again removes it
  // rather than adding a second spelling.
  const isPicked = (value: string) => selected.some((current) => fold(current) === fold(value));
  // A single pick opens on the option its value picks - in the option's own spelling, which is
  // the one the arrows find in the list - or on the value itself while no option has come yet.
  // Only the render that opens the list reads it.
  opensOn.current =
    open && props.mode === "single" ? (options.find(isPicked) ?? props.value) : undefined;
  opensWith.current = options;
  // A single pick puts the focus on the trigger before it hands the value over, so a pick that
  // takes the trigger away - the search bar's Project list, gone once its narrowing is on - takes
  // it from the trigger's own place, from which the panel around it hands the focus on
  // (`useFocusPlace`), rather than from a list portalled outside that panel.
  const choose = (value: string) => {
    setActiveValue(value);
    if (props.mode === "single") {
      onOpenChange(false, "pick");
      triggerRef.current?.focus();
      props.onPick(value);
      return;
    }
    props.onChange(
      isPicked(value)
        ? selected.filter((current) => fold(current) !== fold(value))
        : [...selected, value]
    );
  };
  // A multi-select's row adds the typed value and starts a fresh search. A single pick's row
  // hands it to the host, which answers whether it took it: taken, the list closes as a pick
  // does; refused, the list stays open with the text in its box for the reader to correct.
  const create = (value: string) => {
    setActiveValue(value);
    if (props.mode !== "single") {
      props.onCreate?.(value);
      setQuery("");
    } else if (props.onCreate?.(value) === true) {
      onOpenChange(false, "pick");
      triggerRef.current?.focus();
    }
  };

  // Opening starts a fresh search on the value a single pick holds - or on what the key that
  // opened it says; the panel hands the focus to the search unless the host hands it in itself
  // (`focusOnOpen`). Closing returns the focus to the trigger when the close left it nowhere - the
  // list that held it is gone - and leaves a focus that is somewhere. A focus still on the text
  // box the close took away is nowhere (`focusLost`): an engine that moves the focus off a
  // removed element only at its next rendering update still names the box focused when this
  // effect runs, before that update, and a Tab out of the box would otherwise leave the focus
  // there to the page's rescue rather than on the trigger. A press outside moves the focus after
  // this runs, so the focus ends where that press put it.
  useEffect(() => {
    if (open) {
      const by = openedBy.current;
      openedBy.current = undefined;
      setQuery(by?.query ?? "");
      if (by === undefined) setActiveValue(opensOn.current);
      else if (by.key === undefined) setActiveValue(undefined);
      else setActiveValue(stepTo(by.key, opensWith.current, opensOn.current));
    } else if (wasOpen.current && focusLost()) {
      triggerRef.current?.focus();
    }
    wasOpen.current = open;
  }, [open]);

  // The highlighted row stays in sight in the panel, whatever moved the highlight - the open on
  // the value picked, a move key on the closed trigger, an arrow in the text box, a search - since
  // the panel holds about five rows and the highlight is drawn on the row alone: End, Page Down
  // or ArrowUp's wrap would otherwise highlight a row below the fold, which Enter then picks
  // unseen. `nearest` scrolls only as far as the row needs to stand inside the panel's box, where
  // the sticky text box at its top, or a multi-select's sticky Clear foot at its bottom, can still
  // stand over it, so the panel scrolls on until the row stands clear of both. Laid out before the
  // browser paints, so the row is never drawn out of sight first.
  useLayoutEffect(() => {
    const row = activeId === undefined ? null : document.getElementById(activeId);
    const panel = document.getElementById(panelId);
    if (row === null || panel === null) return;
    row.scrollIntoView({ block: "nearest" });
    const { bottom, top } = row.getBoundingClientRect();
    const underSearch = (searchHostRef.current?.getBoundingClientRect().bottom ?? top) - top;
    if (underSearch > 0) panel.scrollTop -= underSearch;
    const underClear = bottom - (clearHostRef.current?.getBoundingClientRect().top ?? bottom);
    if (underClear > 0) panel.scrollTop += underClear;
  }, [activeId, panelId]);

  // A key on the closed trigger of a single pick that takes the list's keys: a move key opens the
  // list on the row it moves to from the value picked, and a letter or a digit opens it with that
  // key as the search, whose first match is then the active row - the type-ahead a `<select>` does
  // (R6: "a typed letter"). A key held with Ctrl, Alt or Meta is a chord, and Space presses the
  // trigger, so both stay the browser's and the page's.
  const openFromKey = (event: KeyboardEvent<HTMLButtonElement>) => {
    if (!keysOnTrigger || open || event.altKey || event.ctrlKey || event.metaKey) return;
    if (event.nativeEvent.isComposing) return;
    const typed = TYPED_KEY.test(event.key);
    if (!typed && MOVE_KEYS[event.key] !== true) return;
    event.preventDefault();
    openedBy.current = typed ? { query: event.key } : { key: event.key, query: "" };
    if (typed) onQueryChange?.(event.key);
    onOpenChange(true);
  };

  const handleKeyDown = (event: KeyboardEvent<HTMLInputElement>) => {
    // A key that ends or moves an IME composition is the input method's: Enter commits the
    // composed text into the box and picks nothing, and Tab leaves the list open.
    if (event.nativeEvent.isComposing) return;
    if (event.key === "ArrowDown" || event.key === "ArrowUp") {
      const next = stepTo(event.key, rows, activeRow);
      if (next !== undefined) {
        event.preventDefault();
        setActiveValue(next);
      }
      return;
    }
    if (event.key === "Enter" && activeRow !== undefined) {
      event.preventDefault();
      if (activeRow === createValue) {
        create(activeRow);
      } else {
        choose(activeRow);
      }
      return;
    }
    // Tab and Shift+Tab close the list and leave the reader on its trigger, as Escape does. The
    // close puts the focus back on the trigger before the browser's own Tab would run, which
    // would carry it on past the trigger: out of a filter panel altogether from its first
    // control, where a list inside the panel is to be left for its trigger (`useNestedPanels`).
    if (event.key === "Tab") {
      event.preventDefault();
      onOpenChange(false, "tab");
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
  // The picked value's row text, which a single pick's trigger shows unless its host passes
  // words of its own.
  const pickedLabel = single && selected[0] !== undefined ? labelOf(selected[0]) : undefined;
  const triggerText = single
    ? (children ?? pickedLabel ?? label)
    : (children ?? (selected.length === 0 ? label : `${label} · ${selected.length}`));
  // A single pick's trigger is named for its list and then for what it shows - "Assignee of
  // CORE-1 alice", "Merge into another epic Choose an epic…" - so the words on it, the ones a
  // sighted reader reads and a speech-input reader says to press it, are in its name (WCAG 2.5.3,
  // Label in Name), and a screen reader hears the list and the value staged in it. The list's
  // name is a hidden element beside the trigger, outside it so the trigger's text stays the words
  // it shows, and what it shows is the element that holds that text, so words a host hides from
  // assistive technology there - a badge beside the `sr-only` words that read it - stay out of the
  // name. A trigger that shows the list's own name is named by that name once.
  const triggerName = triggerAriaLabel ?? label;
  const namedBy = !single
    ? undefined
    : triggerText === triggerName
      ? nameId
      : `${nameId} ${shownId}`;

  return (
    <>
      {single ? (
        <span hidden id={nameId}>
          {triggerName}
        </span>
      ) : null}
      <button
        aria-controls={open ? panelId : undefined}
        aria-disabled={ariaDisabled || undefined}
        aria-expanded={open}
        aria-haspopup="listbox"
        aria-label={single ? undefined : triggerAriaLabel}
        aria-labelledby={namedBy}
        className={trigger}
        data-control={control}
        {...marks?.trigger}
        disabled={disabled}
        onClick={() => {
          openedBy.current = undefined;
          if (open) onOpenChange(false, "trigger");
          else onOpenChange(true);
        }}
        onKeyDown={openFromKey}
        ref={triggerRef}
        type="button"
      >
        {chevron ? (
          // The text and the chevron in a flex box of their own, whatever display the trigger
          // gets: a block from its host's classes, or the flex box `styles.css` makes every
          // button below 1280 px, whose own text ends in no ellipsis (`TruncatedText`). The text
          // takes the room the chevron leaves and ends in an ellipsis where that is too little
          // (a single pick's whole value is its `title`, unless its host passes its own), and the
          // chevron stays whole at the trigger's end, at the right edge of a trigger as wide as
          // its box. Inside `NoTooltips` (the new issue page) no `title` can give back what an
          // ellipsis cut, so the text wraps there instead and is read whole.
          <span className="flex min-w-0 grow items-center gap-1" id={single ? shownId : undefined}>
            {tooltips ? (
              <TruncatedText
                className={`min-w-0 grow ${triggerTextClassName}`}
                title={triggerTitle ?? pickedLabel}
              >
                {triggerText}
              </TruncatedText>
            ) : (
              <span className={`min-w-0 grow [overflow-wrap:anywhere] ${triggerTextClassName}`}>
                {triggerText}
              </span>
            )}
            <ChevronIcon expanded={open} />
          </span>
        ) : single ? (
          <span id={shownId}>{triggerText}</span>
        ) : (
          triggerText
        )}
      </button>
      {open ? (
        <AnchoredPanel
          anchorRef={triggerRef}
          className={`max-h-72 w-64 max-w-[calc(100vw-1rem)] overflow-auto rounded-lg border shadow-lg ${tooltips ? `${card} ${borderDefault}` : ON_PAGE.panel}`}
          focusOnOpen={focusOnOpen}
          id={panelId}
          onDismiss={(how) => onOpenChange(false, how)}
        >
          <div
            className={`sticky top-0 border-b p-2 ${tooltips ? card : ON_PAGE.panel}`}
            ref={searchHostRef}
          >
            <input
              aria-activedescendant={activeId}
              aria-autocomplete="list"
              aria-controls={listboxId}
              aria-expanded="true"
              aria-label={searchLabel}
              {...marks?.search}
              className={`block min-h-11 w-full rounded-lg px-3 py-2 text-sm outline-none ${tooltips ? inputClasses(true) : ON_PAGE.search}`}
              disabled={disabled}
              onChange={(event) => {
                setQuery(event.target.value);
                setActiveValue(undefined);
                onQueryChange?.(event.target.value);
              }}
              onKeyDown={handleKeyDown}
              placeholder={searchLabel}
              role="combobox"
              type="search"
              value={query}
            />
          </div>
          <div
            aria-label={`${label} options`}
            aria-multiselectable={single ? undefined : "true"}
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
                  onClick={() => choose(option)}
                  onMouseDown={(event) => event.preventDefault()}
                  role="option"
                  type="button"
                >
                  <span
                    aria-hidden="true"
                    className={
                      single
                        ? "w-4 shrink-0 text-center"
                        : `grid size-4 shrink-0 place-items-center rounded border text-xs leading-none ${tooltips ? borderStrong : ON_PAGE.checkbox}`
                    }
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
            {showsEmpty ? (
              <p className={`px-3 py-2 text-sm ${tooltips ? textMutedOnSurface : ON_PAGE.empty}`}>
                {emptyMessage}
              </p>
            ) : null}
            {createValue === undefined ? null : (
              <button
                aria-selected="false"
                className={rowClassName(activeRow === createValue)}
                disabled={disabled}
                id={optionId(createValue)}
                onClick={() => create(createValue)}
                onMouseDown={(event) => event.preventDefault()}
                role="option"
                type="button"
              >
                <span aria-hidden="true" className="w-4 shrink-0 text-center">
                  +
                </span>
                {createLabel(createValue)}
              </button>
            )}
          </div>
          {error === null ? null : (
            <p
              className={`border-t px-3 py-2 text-sm ${tooltips ? `${borderDefault} ${dangerText}` : ON_PAGE.failure}`}
            >
              {error}
            </p>
          )}
          {props.mode === "single" || selected.length === 0 ? null : (
            <div
              className={`sticky bottom-0 border-t p-2 ${tooltips ? card : ON_PAGE.panel}`}
              ref={clearHostRef}
            >
              <button
                className={`min-h-11 w-full rounded-lg px-3 py-2 text-sm font-medium ${tooltips ? `${surfaceMutedBg} ${textSecondaryOnSurface}` : ON_PAGE.clear}`}
                disabled={disabled}
                onClick={() => props.onChange([])}
                onMouseDown={(event) => event.preventDefault()}
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
