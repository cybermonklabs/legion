import {
  type KeyboardEvent,
  type ReactNode,
  useEffect,
  useId,
  useLayoutEffect,
  useRef,
  useState,
} from "react";

import { focusBack, focusBackOverPress, stepIndex } from "../features/shell/roving";
import { ControlKeyHint, digitKbdClass, keyshortcutsOf } from "../features/shell/ShortcutHelp";
import { inputMethodKey } from "../lib/input-method";
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
import { AnchoredPanel, type PanelDismissal, scrollToShow, shownSpan } from "./AnchoredPanel";
import { type ControlName, type ControlSize, controlPadding } from "./control-names";
import { ChevronIcon } from "./DisclosureToggle";
import { focusLost } from "./focus-place";
import { MENU_TRIGGER_ATTRIBUTE, typedOnMenuTrigger } from "./key-targets";
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

/** The mark a menu's trigger carries (`MENU_TRIGGER_ATTRIBUTE`), by which the keymap reads a
 *  character typed on it as the page's key (`typedOnMenuTrigger`). */
const MENU_TRIGGER_MARK = { [MENU_TRIGGER_ATTRIBUTE]: "" };

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

/** A key of the digit row, by which an open list that takes D5's rules writes a choice at once
 *  (`ListMenu`). */
export type MenuDigit = "0" | "1" | "2" | "3" | "4" | "5" | "6" | "7" | "8" | "9";

function isMenuDigit(key: string): key is MenuDigit {
  return key.length === 1 && key >= "0" && key <= "9";
}

/** The digits of `keys` whose choice `options` holds now, as a menu's foot line names them: a
 *  run ("1–4", "0–3"), else each; empty where it holds none. */
function digitsText(keys: ListMenu["keys"], options: readonly string[]): string {
  const digits = Object.entries(keys)
    .flatMap(([digit, value]) =>
      value !== undefined && options.includes(value) ? [Number(digit)] : []
    )
    .sort((a, b) => a - b);
  const first = digits[0];
  const last = digits.at(-1);
  if (first === undefined || last === undefined) return "";
  if (first === last) return `${first}`;
  return digits.every((digit, index) => digit === first + index)
    ? `${first}–${last}`
    : digits.join(" ");
}

/** Whether the reader's primary pointer is coarse, a finger on a phone's screen, where a text box
 *  that takes the focus opens the on-screen keyboard over the choices under it. */
function coarsePointer(): boolean {
  return window.matchMedia("(pointer: coarse)").matches;
}

/** Scrolls the list's box - the panel its sticky text box `head` stands at the top of - the least
 *  that shows the row of id `rowId` whole between that text box and `foot`, the panel's foot while
 *  one is drawn, or the box's own bottom where none is or where the foot follows the rows (a menu
 *  on a coarse pointer), so a row already whole scrolls nothing. Only the box scrolls, never the
 *  window (`scrollToShow`). The list's one scroll of its own: `MultiSelect`'s layout effect that
 *  shows the highlighted row calls it, and so does what watches the box for an open that keeps
 *  that row shown (`Opening`). */
function showRow(
  rowId: string | undefined,
  head: HTMLElement | null,
  foot: HTMLElement | null
): void {
  const row = rowId === undefined ? null : document.getElementById(rowId);
  const box = head?.parentElement ?? null;
  if (row === null || head === null || box === null) return;
  const top = head.getBoundingClientRect().bottom;
  const boxBottom = shownSpan(box).bottom;
  const bottom = Math.min(foot?.getBoundingClientRect().top ?? boxBottom, boxBottom);
  scrollToShow(box, row.getBoundingClientRect(), { bottom, top });
}

/** What an open shows whole and keeps shown (`MultiSelect`'s `opening`): the highlight and the
 *  search it starts with (`active`, `query`); the rows it last showed that highlight among, each
 *  by its value and the text it draws (`among`), null until the commit that draws that search and
 *  highlight, since the open's own commit still draws the ones the list last closed on; and what
 *  watches the list's box for a change of its size (`resized`), null until the open's effects have
 *  run. From that commit the open keeps its highlighted row shown until the reader moves on in
 *  the list or it closes. An open that highlights no row of its own keeps nothing: a
 *  multi-select's, a single pick's with no value, and a typed character's, whose highlight falls
 *  to the first row it matches. */
interface Opening {
  readonly active: string | undefined;
  among: readonly string[] | null;
  readonly query: string;
  resized: ResizeObserver | null;
}

/** Ends what an open keeps shown (`Opening`), and stops watching the list's box for it. */
function letGo(opening: { current: Opening | null }): void {
  opening.current?.resized?.disconnect();
  opening.current = null;
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
 * The rules of a single pick that stands in a menu's place (dispatch://INFRA-2160, its decision
 * D5, and R4, R5 and R15): the Inbox's Snooze, and the priority and the status, which a key opens
 * (`b`, `h`, `p`, `s`) and each of whose picks is a write. One prop carries every rule, so a host
 * takes all of them or none; a host that passes none keeps the plain pick.
 *
 * - The list opens highlighted on its first choice or on the value held (`opensOn`), whatever
 *   opened it: a key, a click, a tap, Enter or Space.
 * - The closed trigger is a menu's button: Enter, Space, ArrowDown and ArrowUp open its list, and
 *   a character typed on it is the page's key and no text (`typedOnMenuTrigger`), so `b` there
 *   opens the Snooze menu of the item in hand, `p` the priority menu and `s` the status menu, as
 *   from the row or the page, each with nothing typed in its box.
 * - ArrowDown, ArrowUp, Home and End move the highlight and write nothing; ArrowLeft and
 *   ArrowRight still move the text box's caret.
 * - A digit writes its own choice at once, whatever the text box has narrowed (`keys`); a digit no
 *   choice has writes nothing, and no digit is typed into the box. A letter narrows the list,
 *   Enter writes the highlighted choice, and a click or a tap writes the choice it hits.
 * - A held key follows the page's rule (`Keymap.captureKeyDown`, which `KeymapProvider` binds):
 *   the repeats of the key whose press opened the list type nothing in the box, a held digit or
 *   Enter writes once, a held letter types on each repeat in the box that had the focus before
 *   its press, and a held arrow walks the highlight.
 * - Escape, Tab, Shift+Tab and a press outside close the list and write nothing, and every close,
 *   a pick's among them, gives the focus back to the element that had it as the list opened: a
 *   key's press of the trigger (a click with no pointer, `detail` 0: the page's binding pressing it
 *   from a row, or Enter or Space on it) leaves that where the key was pressed - the row, or the
 *   page itself where nothing had the focus - and a pointer's press leaves it on the trigger. An
 *   element gone from the page since gives it to the trigger. A press outside gives it back in the
 *   panel's capture-phase `pointerdown` that closes the list, before the browser's own focus on
 *   the press, so a press on another control focuses that control; a press on nothing that takes
 *   the focus has it given back again once the press is over (`focusBackOverPress`). The focus a
 *   press outside gives back scrolls nothing, so the press acts where it was made.
 * - Each choice with a digit draws it, hidden from assistive technology so the option's name stays
 *   its words and named as the option's `aria-keyshortcuts`, and a foot line draws the list's keys.
 * - On a coarse pointer (a phone) opening focuses no text box, so no on-screen keyboard covers the
 *   choices: the list takes the focus, and a tap on the box gives the box the focus. The panel is
 *   tall enough for a menu's choices, each 44 px high, under the box and over the foot line: there
 *   it takes no height cap of its own, only the room the window leaves it beside its trigger, and
 *   the foot line follows the choices rather than standing over them, so each choice the panel
 *   shows takes a tap where it is drawn - the status's eight among them - and the rest scroll in.
 * - While the host's write is out (`ariaDisabled`), a digit, Enter or a press of a choice writes
 *   nothing and the list stays open, and the closed trigger opens on nothing, a press included.
 */
export interface ListMenu {
  /** The value each digit writes, by its digit: the snooze's `1` to `4` and Un-snooze's `0`, the
   *  priority's `0` to `3`, the status's none. A digit whose value the options do not hold now -
   *  Un-snooze on an item not snoozed - writes nothing, and its row and the foot line draw no
   *  digit for it. */
  readonly keys: Readonly<Partial<Record<MenuDigit, string>>>;
  /** Which row the list opens highlighted: its first choice (a snooze, whose choices are acts),
   *  or the value the pick holds (a priority, a status). */
  readonly opensOn: "first" | "value";
}

/**
 * Each colour the picker draws inside `NoTooltips`, the new issue page's, by the role `main`'s
 * colour has: the trigger is the page's header control (its rule and muted words on the
 * paper); the popover is the page's menu (the prototype's `#menu`: the hairline rule on the
 * paper), whose rows are its items, in the ink with the mark grey under the pointer and on the
 * active row; an empty list is quiet words, a refusal the failed red, and Clear the page's pane
 * with muted words. A detail is muted words too, not quiet ones: it is drawn on its row's mark
 * grey under the pointer and on the active row, where the quiet grey reads 4.11:1 in the light
 * scheme, under the 4.5:1 its words need; and so are a menu's digits, on the same rows, and its
 * foot line of keys, on the paper (`ListMenu`). The search box keeps `main`'s focus rule, the one
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
   *  trigger keeps its focus and its Tab stop. No key on the closed trigger opens the list then
   *  (`openFromKey`), and a press still does, unless the host refuses the open in
   *  `onOpenChange`: the Agents page's Issue list, and each host whose write is out
   *  (`AssigneeControl`, `RepoControl`, `AskHandOn`). A single pick in a menu's place (`menu`)
   *  opens on no press either, and while its list stands open no pick writes. */
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
  /** The focus arriving on the trigger from outside the list, with the element it came from
   *  (the focus event's `relatedTarget`, null where it came from nowhere): a host that keeps
   *  where the reader came from, for a way back there, records it here. The list handing the
   *  focus back to its trigger is no arrival and calls nothing: a pick, Escape, Tab, a press
   *  of the trigger while the list is open, or a menu's press outside, once it is over. So the
   *  record outlives the reader's use of the list. */
  readonly onTriggerFocus?: (from: Element | null) => void;
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
   *  a bare count, or `undefined` for that text itself, which is what it says by default. A row
   *  both answer `undefined` for draws no detail and is named by its label alone. */
  readonly optionDetailLabel?: (value: string) => string | undefined;
  readonly searchLabel: string;
  /** The keys that also press the trigger where its host binds them and draws them (the Inbox's
   *  `b` and `p`, in the reading pane's head): named in the trigger's `aria-keyshortcuts`, and the
   *  first drawn inside it after what it draws (`ControlKeyHint`), outside the words that name it. */
  readonly shortcutKeys?: readonly string[];
  /** How much room the trigger takes (`controlPadding`): the page's size, which the page passes.
   *  The default trigger outside `NoTooltips` takes the comfortable size where none is passed. A
   *  `triggerClassName` draws a host's own look, and the size passed with it is added to it, so a
   *  host's trigger is sized as the page's other controls are; one passed without a size keeps
   *  the room it writes itself. */
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

/** One value: a pick hands it to `onPick`, closes the list and puts the focus on the trigger, or
 *  where a menu's goes (`menu`). */
interface SingleProps extends SharedProps {
  /** The closed trigger takes the list's keys, as a focused `<select>` does: ArrowDown, ArrowUp,
   *  Home, End, Page Down and Page Up open the list on the value picked and move the highlight by
   *  that key. Without it the arrows open the list as a click does and the other four open
   *  nothing; a typed character opens any list but a menu's (`menu`) with that character in the
   *  text box. Each is the trigger's own (`preventDefault`), so the page's keymap, which skips a
   *  handled key, never reads it. A host whose value is written the moment it is picked passes it
   *  (the assignee, the repository, an ask's hand-on), so a key moves only the highlight and only
   *  Enter or a click writes. Those three are this list rather than native selects (INFRA-2160's
   *  R6, and its departure D2): a select cannot write nothing on Escape, since Chromium keeps the
   *  highlighted option when Escape closes its open list, which the keyboard-step rule of the
   *  live header's selects (`useKeySteps` in `agent-view/SessionSettings.tsx`) cannot change. */
  readonly keysOnTrigger?: boolean;
  /** D5's rules, for a pick that stands in a menu's place: every one of them, or, absent, none
   *  (`ListMenu`). A host that passes it passes no `keysOnTrigger`, whose closed-trigger keys are
   *  R6's controls' (`MultiSelect.hosts.test.ts` holds who takes which). */
  readonly menu?: ListMenu;
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
 * (`chevron`).
 *
 * The closed trigger opens the list on the keys the WAI-ARIA select-only combobox opens it on:
 * Enter and Space, which press it, ArrowDown and ArrowUp, with Alt or without, and, on every list
 * but a menu's (below), a typed character, which goes into the text box and narrows the list by
 * it. A single pick that passes `keysOnTrigger` also takes the keys a `<select>` takes there -
 * ArrowDown, ArrowUp, Home, End, Page Down and Page Up open it with the highlight moved by that
 * key - so those keys, too, only move the highlight.
 *
 * A single pick that stands in a menu's place - the Inbox's Snooze, the priority, the status -
 * passes `menu`, which carries each rule of dispatch://INFRA-2160's D5 at once (`ListMenu`): it
 * opens on its first choice or on its value, its digits write their choices, a character typed on
 * its closed trigger is the page's key and no text, a held key acts once but for the arrows and the
 * letters typed in its box, every close gives the focus back where it was as the list opened, and
 * on a phone no text box takes the focus.
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
    onTriggerFocus,
    open,
    options,
    optionDetail,
    optionDetailLabel,
    optionLabel = (value: string) => value,
    searchLabel,
    shortcutKeys,
    size,
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
    triggerClassName === undefined
      ? tooltips
        ? `rounded-lg border text-sm font-semibold ${controlPadding[size ?? "comfortable"]} ${borderDefault} ${surfaceMutedBg} ${textSecondaryOnCanvas}`
        : ON_PAGE.trigger
      : size === undefined
        ? triggerClassName
        : `${controlPadding[size]} ${triggerClassName}`;
  const listboxId = `${id}-options`;
  const nameId = `${id}-name`;
  const shownId = `${id}-shown`;
  const optionId = (value: string) => `${id}-option-${encodeURIComponent(value)}`;
  const [query, setQuery] = useState("");
  const [activeValue, setActiveValue] = useState<string | undefined>(undefined);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const listboxRef = useRef<HTMLDivElement>(null);
  const wasOpen = useRef(false);
  // The panel's sticky text box at its top, and its foot while one is drawn - a multi-select's
  // Clear, a menu's line of keys - sticky at its bottom, but for a menu's on a coarse pointer, which
  // follows the rows (`ListMenu`): the rows scroll under the sticky ones.
  const headRef = useRef<HTMLDivElement>(null);
  const footRef = useRef<HTMLDivElement>(null);
  // What an open reads of this render: the value it starts on, and the options a move key that
  // opened it steps through. A host may build its options only while the list stands open (an
  // ask's hand-on), so the step is taken in the open render's options, not the closed trigger's.
  const opensOn = useRef<string | undefined>(undefined);
  const opensWith = useRef<readonly string[]>([]);
  // What a key on the closed trigger opened the list with, read as it opens: the move key whose
  // step picks the row it highlights, and the text it puts in the search box. A press of the
  // trigger opens it as a click does, on the value picked with an empty search.
  const openedBy = useRef<{ readonly key?: string; readonly query: string } | undefined>(undefined);
  // What an open shows whole and keeps shown (`Opening`): the search and the highlight it starts
  // with, from the open until the reader moves on in the list or it closes; null while it keeps
  // nothing.
  const opening = useRef<Opening | null>(null);
  // The highlighted row the layout effect below last showed whole, by its element's id.
  const shownRow = useRef<string | undefined>(undefined);
  const keysOnTrigger = props.mode === "single" && props.keysOnTrigger === true;
  // D5's rules, which a single pick takes whole or not at all (`ListMenu`).
  const menu = props.mode === "single" ? props.menu : undefined;
  // Where a menu's closes give the focus back: the element that had it as the list opened, read
  // by the press or the key that opened it (`ListMenu`), and forgotten as the list closes.
  const returnTo = useRef<HTMLElement | null>(null);
  // Whether the focus arriving on the trigger now is the list handing it back, which is no
  // arrival (`onTriggerFocus`).
  const handingBack = useRef(false);
  // Whether a menu's list opened on a coarse pointer, where no text box takes the focus as it
  // opens: read by the render that opens it, and held while it stands open.
  const opensBare = useRef(false);
  if (open && !wasOpen.current) opensBare.current = menu !== undefined && coarsePointer();
  const bare = open && opensBare.current;

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
  // the one the arrows find in the list - or on the value itself while no option has come yet;
  // a menu of acts on its first choice, whatever the pick holds. Only the render that opens the
  // list reads it.
  opensOn.current =
    open && props.mode === "single"
      ? menu?.opensOn === "first"
        ? options[0]
        : (options.find(isPicked) ?? props.value)
      : undefined;
  opensWith.current = options;
  // Where a close gives the focus back: the trigger; for a menu, the element that had the focus as
  // the list opened, while it is on the page.
  const returnTarget = (): HTMLElement | null => {
    const recorded = menu === undefined ? null : returnTo.current;
    return recorded?.isConnected ? recorded : triggerRef.current;
  };
  // A focus arriving on the trigger as it is handed back is no arrival (`handingBack`). `options`
  // are the focus's own; with none, the engine brings the element into view where it is out of
  // sight, as it does for Escape and Tab.
  const handBack = (options?: FocusOptions, to: HTMLElement | null = returnTarget()) =>
    focusBack(to, (element) => {
      handingBack.current = true;
      element.focus(options);
      handingBack.current = false;
    });
  // The hand-back of the latest render, for the close's effect below.
  const handBackNow = useRef(handBack);
  handBackNow.current = handBack;
  // A pick the list takes - a single pick's row, or a `createLabel` row its host took - closes the
  // list and gives the focus back with no scroll: the element held the focus as the list opened,
  // and a host whose pick takes it away hands the focus on itself - the Inbox's hand-off focuses
  // the row after the snoozed ones with no scroll while they still stand, and brings it into view
  // once they have gone (`pane-selection.ts`). The engine's own scroll of a card a key opened the
  // bar's Snooze from, partly out of sight, moved the list 230 px in card mode at the pick; at the
  // issue header's `Use "owner/repo"`, its Repository half out of sight, it moved a 390 px page
  // 22 px.
  const closeOnPick = () => {
    onOpenChange(false, "pick");
    handBack({ preventScroll: true });
  };
  // A single pick gives the focus back - to the trigger, or where a menu's goes - before it hands
  // the value over, so a pick that takes the trigger away - the search bar's Project list, gone
  // once its narrowing is on - takes it from the trigger's own place, from which the panel around
  // it hands the focus on (`useFocusPlace`), rather than from a list portalled outside that
  // panel; and a snooze whose write folds the row a key opened its menu from takes the focus from
  // that row, which the Inbox's hand-off reads (R8). A menu whose host's write is out writes
  // nothing and stays open (`ariaDisabled`).
  const choose = (value: string) => {
    if (menu !== undefined && ariaDisabled) return;
    setActiveValue(value);
    if (props.mode === "single") {
      closeOnPick();
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
  // hands it to the host, which answers whether it took it: taken, the list closes; refused, the
  // list stays open with the text in its box for the reader to correct.
  const create = (value: string) => {
    if (menu !== undefined && ariaDisabled) return;
    setActiveValue(value);
    if (props.mode !== "single") {
      props.onCreate?.(value);
      setQuery("");
    } else if (props.onCreate?.(value) === true) {
      closeOnPick();
    }
  };
  // A menu's press outside, in the panel's capture-phase `pointerdown` that closes the list
  // (`AnchoredPanel`): the focus goes back at once, and again once the press is over where it
  // left it on the document (`focusBackOverPress`), each time with no scroll. A scroll then would
  // move the page under the press between its `mousedown` and its `mouseup`: in card mode the
  // focus given back to a card far up the page scrolled the cards, so the press on the Later
  // band's toggle or on another card's Snooze, far down the page, released on another element and
  // did not act.
  const handBackOverPress = () => {
    const to = returnTarget();
    focusBackOverPress(() => handBack({ preventScroll: true }, to));
  };

  // Opening starts a fresh search on the value a single pick holds - or on what the key that
  // opened it says: the character typed on the closed trigger in the text box, with its first
  // match highlighted as typing into the box highlights it, or the row a move key steps to
  // (`keysOnTrigger`) - before the open paints: the search the list last closed on would otherwise
  // narrow the rows its first frame draws. The row it highlights is then shown whole, and kept
  // shown until the reader moves on in the list or the close ends that (`opening`, the layout
  // effect below). The panel hands the focus to the search unless the host hands it in itself
  // (`focusOnOpen`), and a menu opened on a coarse pointer gives it to its list rather than to the
  // box, with no scroll, here, before the panel's own effect starts to follow the focus
  // (`AnchoredPanel`): the layout effect below alone decides where the panel stands scrolled. The
  // engine's own scroll of the list box it focuses centred that box in the panel, which put the
  // status's first choice under the text box and its last under the foot line on a phone.
  useLayoutEffect(() => {
    if (!open) {
      letGo(opening);
      return;
    }
    const by = openedBy.current;
    openedBy.current = undefined;
    const opened: Opening = {
      active:
        by === undefined
          ? opensOn.current
          : by.key === undefined
            ? undefined
            : stepTo(by.key, opensWith.current, opensOn.current),
      among: null,
      query: by?.query ?? "",
      resized: null,
    };
    setQuery(opened.query);
    setActiveValue(opened.active);
    opening.current = opened;
    if (opensBare.current) listboxRef.current?.focus({ preventScroll: true });
  }, [open]);
  // Closing gives the focus back - to the trigger, or where a menu's goes - when the close left it
  // nowhere - the list that held it is gone - and leaves a focus that is somewhere. Whether it is
  // nowhere is the check for a lost focus a list asks (`focusLost`, whose doc says where each
  // engine leaves the focus of an element taken away), so a Tab out of the text box the close took
  // away puts the reader back rather than leave the focus to fall onto the page. A press outside
  // moves the focus after this runs, so the focus ends where that press put it.
  useEffect(() => {
    if (!open && wasOpen.current) {
      if (focusLost()) handBackNow.current();
      returnTo.current = null;
    }
    wasOpen.current = open;
  }, [open]);

  // The highlighted row stays in sight in the panel, whatever moved the highlight - the open on
  // the value picked, a move key on the closed trigger, an arrow, Home or End in the text box, a
  // search - since the panel holds about five rows and the highlight is drawn on the row alone:
  // End, Page Down or ArrowUp's wrap would otherwise highlight a row below the fold, which Enter
  // then picks unseen, as it took the issue header's Status on an issue in Testing, Needs review
  // or Retro, whose list opened with that status below the five rows its box shows. The panel
  // scrolls the least that shows the row whole between the sticky text box and the foot - the
  // sticky foot, or the panel's own bottom where the foot follows the rows (a menu on a coarse
  // pointer) - so a row already whole scrolls nothing. Only the panel's own box scrolls, never the
  // window (`showRow`). Laid out before the browser paints, so the row is never drawn out of
  // sight first. A row is shown as the highlight moves onto it (`shownRow`), and the row an open
  // highlights - the first choice of a menu of acts, else the value picked, or the row the key
  // that opened it names - in the commit that draws the open's own search and highlight
  // (`opening`), whether the highlight's row changed there or not: the open's own commit still
  // draws the ones the list last closed on, and the open's setting of its own draws them in the
  // commit right after it, before anything paints. So a list that closed on a search opens with
  // its picked row shown whole among every option again, not where it stood among the few that
  // search left. From that commit the open keeps its row shown until the reader moves on in the
  // list or the list closes: every later commit that changes the rows, or the text one of them
  // draws, shows the highlighted row whole again, whether its row changed there or not, and so
  // does a change of the list box's own size (the effect below). The issue's Assignee list reads
  // its people only once it is opened, so it opens on Unassigned and the assignee, and when the
  // people came the assignee moved to its place among everyone, still the highlighted row, below
  // the rows its box shows. A host's label or detail of a row can answer otherwise once a read has
  // come, with the same rows: a longer label wraps and makes its row taller while the box keeps
  // its size. Each move of the reader's in the list ends that before the row it moves to is shown
  // (`letGo`): an arrow, Home or End, a menu's digit, a word typed, a press or a tap in the list
  // outside its text box, and a scroll of their own (the effect below). An open that highlights no
  // row of its own keeps nothing (`Opening`).
  useLayoutEffect(() => {
    const opened = opening.current;
    // Each row by its value and the text it draws, the Create row's label or an option's label and
    // detail, written as JSON so that two different rows never read the same; read only while an
    // open keeps its row shown.
    const drawn =
      opened === null
        ? []
        : rows.map((row) =>
            JSON.stringify(
              row === createValue
                ? [row, createLabel(row)]
                : [row, labelOf(row), optionDetail?.(row)]
            )
          );
    if (opened !== null && opened.among === null) {
      if (opened.query !== query || opened.active !== activeValue) return;
      if (opened.active === undefined) letGo(opening);
      else opened.among = drawn;
    } else if (
      opened !== null &&
      (opened.among?.length !== drawn.length || opened.among.some((row, at) => row !== drawn[at]))
    ) {
      opened.among = drawn;
    } else if (activeId === shownRow.current) return;
    shownRow.current = activeId;
    showRow(activeId, headRef.current, footRef.current);
  });

  // The closed trigger opens the list on the keys the WAI-ARIA select-only combobox opens it on:
  // ArrowDown and ArrowUp, with Alt or without, open it as a click does, on the value picked, and
  // a typed character opens it with that character in the text box, and the caret after it, so
  // the options are already narrowed by it - on every list but a menu's, whose trigger is a menu's
  // button and leaves a typed character to the page, as its row does (`typedOnMenuTrigger`). A
  // single pick that takes the list's keys (`keysOnTrigger`) takes the keys a `<select>` takes
  // there: ArrowDown, ArrowUp, Home, End, Page Down and Page Up, with no Alt, open it on the row
  // the key moves to from the value picked (R6: "an arrow key, Home, End, Page Up, Page Down or a
  // typed letter only changes the choice that the control shows"). The key's own action is
  // cancelled, so the page's keymap, which skips a handled key, never reads it, and the character
  // is not typed a second time into the text box the focus moves to. Space is left to the button,
  // which opens the list with an empty search. A disabled or busy trigger opens on none of these
  // keys; a key held with Ctrl or Meta is the browser's, and a key that ends or moves an IME
  // composition is the input method's. A menu's closes give the focus back to the trigger a key
  // on it opened the list from.
  const openFromKey = (event: KeyboardEvent<HTMLButtonElement>) => {
    if (open || disabled || ariaDisabled || event.ctrlKey || event.metaKey) return;
    if (inputMethodKey(event.nativeEvent)) return;
    if (typedOnMenuTrigger(event.currentTarget, event)) return;
    const typed = !event.altKey && event.key.length === 1 && event.key !== " ";
    const steps = keysOnTrigger && !event.altKey && MOVE_KEYS[event.key] === true;
    if (!typed && !steps && event.key !== "ArrowDown" && event.key !== "ArrowUp") return;
    event.preventDefault();
    openedBy.current = typed
      ? { query: event.key }
      : steps
        ? { key: event.key, query: "" }
        : undefined;
    if (typed) onQueryChange?.(event.key);
    if (menu !== undefined) returnTo.current = event.currentTarget;
    onOpenChange(true);
  };

  // Every row, the Create row and Clear are buttons, so the focus can reach one, as a screen
  // reader's cursor puts it there, and a press of each can take that focus to the body, where
  // Escape no longer reaches the popover. A press of Clear or of the Create row that holds the
  // focus gives it to the search box at the press itself, before the press acts, so the move does
  // not depend on when the host applies the press. Clear goes as the pick empties. A multi-select's
  // Create row goes as its value is made and the search empties. A single pick's Create row goes
  // with the list where the host takes the value, and the focus goes on to the trigger. Where the
  // host refuses the value, the Create row stays, and the focus stays in the search box, where the
  // reader corrects the text. A row's press need not take the row away. It can move the row, since
  // a host can order the rows by the pick (`IssueLabels`, `ComponentPicker`), but a row the press
  // only moves keeps its focus: as React ends a commit that took the focus away, it focuses again
  // the element that held it before, where that element is still on the page (react-dom's
  // `flushMutationEffects`). A row the press takes off the page loses the focus, as in Details a
  // label only the open draft holds does as the reader unpicks it: `IssueLabels` lists the draft's
  // labels with the issue's and the project's, so that label is listed only while the draft holds
  // it. So a press of a row that holds the focus only notes it, and the commit that answers the
  // press gives the search box the focus only where that commit left the focus lost (`focusLost`,
  // the close's check above): on the body, or an element no longer on the page. It is the page's
  // one check for a lost focus, the one the close asks too, and its doc says where each engine
  // leaves the focus of an element taken away. A pointer press focuses none of them
  // (`onMouseDown` below), and leaves the focus where it is. A row or a Create row the options
  // take away holds no press, and its focus stays on the page's body, as for every change the
  // reader did not make. A popover that closes takes the search box along, and the close above
  // gives the focus back to the trigger instead.
  const focusSearchFirst = (event: { readonly currentTarget: EventTarget }) => {
    if (event.currentTarget === document.activeElement) inputRef.current?.focus();
  };
  // Whether a row was pressed while it held the focus, until the commit that answers the press:
  // the press asks for a render, so that commit comes even where the press changes nothing else.
  // The row's own click handler notes it, beside the change it hands the host, so the render it
  // asks for commits with the host's answer. Noted in the capture phase, Chromium committed that
  // render before the row's handler ran, between React's capture and bubble listeners, and the
  // note was spent on a commit that took nothing away (builds Job infra-2160-probe2-67cfcdb1, an
  // Enter on a focused row; Firefox ran the row's handler first).
  const focusedRowPressed = useRef(false);
  const [, renderAfterPress] = useState(0);
  const noteFocusedRow = (event: { readonly currentTarget: EventTarget }) => {
    if (event.currentTarget !== document.activeElement) return;
    focusedRowPressed.current = true;
    renderAfterPress((renders) => renders + 1);
  };
  // Read in the commit itself, by this component's own layout effect. React runs a child's layout
  // effects before its parent's, so this runs before the layout effect of a panel the list stands
  // in (`PanelTrigger`'s `useFocusPlace`), which, finding the row gone and the focus lost by the
  // reader's press, would otherwise hand the focus the pick took away to the control at the row's
  // place, the list's trigger, first - as on the Agents page's Filter, whose role and machine
  // lists hold a picked value no live session reports only while it is picked.
  useLayoutEffect(() => {
    const pressed = focusedRowPressed.current;
    focusedRowPressed.current = false;
    if (pressed && focusLost()) inputRef.current?.focus();
  });

  // While the open keeps its row shown (`opening`), a change of the list box's own size shows it
  // whole again, by the list's one scroll of its own (`showRow`): where the window has too little
  // room for the panel, the panel cuts its box to that room (`AnchoredPanel`) in a commit after the
  // one that showed the row, and a box cut shorter can leave the row cut at its bottom. What
  // watches the box stops as the open lets go of the row (`letGo`) and as the list closes.
  //
  // The reader's own scroll of the list ends what the open keeps shown, so a later change of the
  // rows or of the box leaves the list where they put it: a turn of the wheel or a touch dragged
  // over it; a press on it outside its text box - a tap on a row or anywhere in the list on a
  // phone, a press on its scrollbar, a middle button's autoscroll, a drag that selects its words
  // past its edge; a key that can scroll it - PageUp or PageDown, even from its text box, or any
  // key while the focus is in the list but not in its text box; and the focus reaching a row, as a
  // screen reader's cursor puts it there, which the panel scrolls into the box's view
  // (`AnchoredPanel`). Heard on the list's box in the capture phase, before anything inside it can
  // stop them, and passively, so no scroll waits on them. The list's own keys in its text box end
  // it in the handler that takes them (`handleKeyDown`), and a word typed in the box's
  // `onChange`. Nothing the open does itself ends it: a menu's focus on its list on a coarse
  // pointer (`opensBare`) and the panel's on the text box come before this effect listens, and no
  // scroll of the box is heard as one, the list's showing of its row and the panel's own alike.
  useEffect(() => {
    const head = headRef.current;
    const box = head?.parentElement ?? null;
    const kept = opening.current;
    if (!open || kept === null || kept.active === undefined || head === null || box === null) {
      return;
    }
    const resized = new ResizeObserver(() => showRow(shownRow.current, head, footRef.current));
    resized.observe(box);
    kept.resized = resized;
    const moved = () => letGo(opening);
    const outsideHead = (event: Event) => {
      if (!(event.target instanceof Node && head.contains(event.target))) moved();
    };
    const keyed = (event: globalThis.KeyboardEvent) => {
      const paging = event.key === "PageUp" || event.key === "PageDown";
      if (paging || event.target !== inputRef.current) moved();
    };
    const heard = { capture: true, passive: true };
    box.addEventListener("wheel", moved, heard);
    box.addEventListener("touchmove", moved, heard);
    box.addEventListener("pointerdown", outsideHead, heard);
    box.addEventListener("focusin", outsideHead, heard);
    box.addEventListener("keydown", keyed, heard);
    return () => {
      resized.disconnect();
      box.removeEventListener("wheel", moved, true);
      box.removeEventListener("touchmove", moved, true);
      box.removeEventListener("pointerdown", outsideHead, true);
      box.removeEventListener("focusin", outsideHead, true);
      box.removeEventListener("keydown", keyed, true);
    };
  }, [open]);

  // The text box's keys, and a menu's list's own while it holds the focus (on a coarse pointer).
  const handleKeyDown = (event: KeyboardEvent<HTMLElement>) => {
    // A key that ends or moves an IME composition is the input method's: Enter commits the
    // composed text into the box and picks nothing, and Tab leaves the list open.
    if (inputMethodKey(event.nativeEvent)) return;
    const plain = !event.altKey && !event.ctrlKey && !event.metaKey;
    // A menu's digit is the list's before the text box takes it: it writes its own choice at
    // once, whatever the box has narrowed, and a digit no choice has writes nothing. Either way
    // the reader has moved on from the row the list opened on, which is no longer kept shown
    // (`letGo`), so a list a digit leaves open stays where it is as its box or its rows change.
    if (menu !== undefined && plain && isMenuDigit(event.key)) {
      event.preventDefault();
      letGo(opening);
      const value = menu.keys[event.key];
      if (value !== undefined && options.includes(value)) choose(value);
      return;
    }
    // The arrows move the highlight, and in a menu Home and End do too, where they would move the
    // caret; ArrowLeft and ArrowRight stay the caret's. Each scrolls the list the least that shows
    // the option it marks (the layout effect above), and moves the reader on from the row the
    // list opened on, which is no longer kept shown (`letGo`), before the row it moves to is shown.
    if (
      event.key === "ArrowDown" ||
      event.key === "ArrowUp" ||
      (menu !== undefined &&
        plain &&
        !event.shiftKey &&
        (event.key === "Home" || event.key === "End"))
    ) {
      const next = stepTo(event.key, rows, activeRow);
      if (next !== undefined) {
        event.preventDefault();
        letGo(opening);
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
    // Tab and Shift+Tab close the list and give the focus back, as Escape does. The close puts
    // the focus back - on the trigger, or where a menu's goes - before the browser's own Tab would
    // run, which would carry it on past the trigger: out of a filter panel altogether from its
    // first control, where a list inside the panel is to be left for its trigger
    // (`useNestedPanels`).
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
  // A menu's digit of each choice, by its value, which the choice's row draws, and the digits its
  // foot line names: those whose choice the options hold now.
  const digitOf = new Map<string, string>();
  if (open && menu !== undefined) {
    for (const [digit, value] of Object.entries(menu.keys)) {
      if (value !== undefined) digitOf.set(value, digit);
    }
  }
  const menuDigits = open && menu !== undefined ? digitsText(menu.keys, options) : "";

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
        aria-keyshortcuts={keyshortcutsOf(shortcutKeys)}
        aria-label={single ? undefined : triggerAriaLabel}
        aria-labelledby={namedBy}
        className={trigger}
        data-control={control}
        {...(menu === undefined ? undefined : MENU_TRIGGER_MARK)}
        {...marks?.trigger}
        disabled={disabled}
        onClick={(event) => {
          openedBy.current = undefined;
          if (open) {
            onOpenChange(false, "trigger");
            if (menu !== undefined) handBack();
            return;
          }
          if (menu !== undefined) {
            if (ariaDisabled) return;
            // A click with no pointer is a key's - the page's binding pressing the trigger from a
            // row, or Enter or Space on it - so the focus is still where the key was pressed; a
            // pointer's click is on the trigger.
            const focused = document.activeElement;
            returnTo.current =
              event.detail === 0 && focused instanceof HTMLElement ? focused : event.currentTarget;
          }
          onOpenChange(true);
        }}
        onFocus={(event) => {
          // The focus comes back from the list while the list is open and until its close has
          // run the effect above, which is while `wasOpen` holds, and as a menu's press outside
          // is over (`handingBack`).
          if (!wasOpen.current && !handingBack.current) onTriggerFocus?.(event.relatedTarget);
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
        <ControlKeyHint keys={shortcutKeys} />
      </button>
      {open ? (
        // A menu on a coarse pointer takes no height cap of its own: `AnchoredPanel` cuts it to the
        // room the window leaves beside its trigger, so a list as long as the status's stands whole
        // under its text box wherever that room holds it, which the 384 px cap did not (`ListMenu`).
        <AnchoredPanel
          anchorRef={triggerRef}
          className={`${menu === undefined ? "max-h-72" : bare ? "max-h-none" : "max-h-96"} w-64 max-w-[calc(100vw-1rem)] overflow-auto rounded-lg border shadow-lg ${tooltips ? `${card} ${borderDefault}` : ON_PAGE.panel}`}
          focusOnOpen={focusOnOpen && !bare}
          id={panelId}
          onDismiss={(how) => {
            if (menu !== undefined && how === "press") handBackOverPress();
            onOpenChange(false, how);
          }}
          returnFocus={menu === undefined ? undefined : returnTarget}
        >
          <div
            className={`sticky top-0 border-b p-2 ${tooltips ? card : ON_PAGE.panel}`}
            ref={headRef}
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
                // A word typed moves the reader on from the option the list opened on, which is
                // no longer kept shown (`letGo`), even once the search is back where it began.
                letGo(opening);
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
            aria-activedescendant={bare ? activeId : undefined}
            aria-label={`${label} options`}
            aria-multiselectable={single ? undefined : "true"}
            id={listboxId}
            onKeyDown={bare ? handleKeyDown : undefined}
            ref={listboxRef}
            role="listbox"
            tabIndex={bare ? -1 : undefined}
          >
            {visibleOptions.map((option) => {
              const isSelected = isPicked(option);
              const label = labelOf(option);
              const detail = optionDetail?.(option);
              const spokenDetail = optionDetailLabel?.(option) ?? detail;
              const digit = digitOf.get(option);
              return (
                <button
                  aria-keyshortcuts={digit}
                  aria-label={spokenDetail === undefined ? undefined : `${label}, ${spokenDetail}`}
                  aria-selected={isSelected}
                  className={rowClassName(activeRow === option)}
                  disabled={disabled}
                  id={optionId(option)}
                  key={option}
                  onClick={(event) => {
                    noteFocusedRow(event);
                    choose(option);
                  }}
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
                  {digit === undefined ? null : (
                    // For the eye alone: assistive technology hears the digit as the option's
                    // `aria-keyshortcuts`, and the option's name stays its words.
                    <span aria-hidden="true" className="shrink-0">
                      <kbd
                        className={
                          tooltips
                            ? digitKbdClass
                            : `rounded px-1.5 font-mono text-xs ${ON_PAGE.detail}`
                        }
                      >
                        {digit}
                      </kbd>
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
                onClickCapture={focusSearchFirst}
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
          {menu === undefined ? null : (
            // The list's keys, for the eye: each choice's digit is named to assistive technology
            // as its own `aria-keyshortcuts`. On a coarse pointer the line follows the choices
            // rather than standing over the last of them, so a panel the window's room cuts still
            // draws each choice it shows where a tap reaches it, and the line scrolls in after.
            <div
              aria-hidden="true"
              className={`${bare ? "" : "sticky bottom-0 "}border-t px-3 py-1.5 text-xs ${tooltips ? `${card} ${textMutedOnSurface}` : `${ON_PAGE.panel} ${ON_PAGE.detail}`}`}
              ref={footRef}
            >
              ↑ ↓ Home End to move · Enter{menuDigits === "" ? "" : ` or ${menuDigits}`} to choose ·
              Esc to close
            </div>
          )}
          {props.mode === "single" || selected.length === 0 ? null : (
            <div
              className={`sticky bottom-0 border-t p-2 ${tooltips ? card : ON_PAGE.panel}`}
              ref={footRef}
            >
              <button
                className={`min-h-11 w-full rounded-lg px-3 py-2 text-sm font-medium ${tooltips ? `${surfaceMutedBg} ${textSecondaryOnSurface}` : ON_PAGE.clear}`}
                disabled={disabled}
                onClick={() => props.onChange([])}
                onClickCapture={focusSearchFirst}
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
