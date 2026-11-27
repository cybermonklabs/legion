import {
  type KeyboardEvent,
  type ReactNode,
  useEffect,
  useId,
  useLayoutEffect,
  useRef,
  useState,
} from "react";

import { inputMethodKey } from "../lib/input-method-key";
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
import { TruncatedText } from "./TruncatedText";
import { useTooltips } from "./tooltips";

function fold(value: string | undefined): string {
  return (value ?? "").trim().toLocaleLowerCase();
}

/** What an open keeps shown whole (`MultiSelect`'s `opening`): a single pick's option it starts
 *  on, under the search it starts with, from the open until the reader moves on in the list or it
 *  closes; the rows it last looked for that option among, each by its value and the text it draws
 *  (`among`), null before the commit that draws that search; and what watches the list's box for a
 *  change of its size (`resized`), null until the open's effects have run. */
interface Opening {
  among: readonly string[] | null;
  readonly query: string;
  resized: ResizeObserver | null;
  readonly value: string;
}

/** Ends what an open keeps shown whole (`Opening`), and stops watching the list's box for it. */
function letGo(opening: { current: Opening | null }): void {
  opening.current?.resized?.disconnect();
  opening.current = null;
}

/** What four changes of the reader's wait for before the list marks active an option it shows
 *  whole (`MultiSelect`'s `awaiting`): the commit that draws the search they typed in its text
 *  box, or the empty search a multi-select's Create row starts (`query`); for the value a
 *  multi-select's Enter or press ticked or unticked (`value`), the commit in which the host's pick
 *  holds that value otherwise than it did at the press (`picked`), with whether a pointer pressed
 *  its row (`pointer`), which scrolls nothing; or, for a multi-select's Clear, the commit in which
 *  the host's pick holds no value (`cleared`). No other change waits. */
type Awaiting =
  | { readonly query: string }
  | { readonly picked: boolean; readonly pointer: boolean; readonly value: string }
  | { readonly cleared: true };

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
   *  rather than looking actionable and doing nothing. */
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
   *  focus back to its trigger is no arrival and calls nothing: a pick, Escape, Tab, or a press
   *  of the trigger while the list is open. So the record outlives the reader's use of the list. */
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

/** One value: a pick hands it to `onPick`, closes the list and puts the focus on the trigger. */
interface SingleProps extends SharedProps {
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
 * Enter and Space, which press it, ArrowDown and ArrowUp, with Alt or without, and a typed
 * character, which goes into the text box and narrows the list by it.
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
  // The option marked active: undefined while none is marked, which leaves the first row active,
  // and null where a pointer's press left no option whole in sight to mark (`awaiting`), which
  // leaves no row active until an arrow or a word typed marks one.
  const [activeValue, setActiveValue] = useState<string | null | undefined>(undefined);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const wasOpen = useRef(false);
  // What an open reads of this render: the value it starts on.
  const opensOn = useRef<string | undefined>(undefined);
  // What the next open puts in the text box: the character typed on the closed trigger, if any.
  const opensWith = useRef("");
  // What an open keeps shown whole (`Opening`), null while it keeps nothing.
  const opening = useRef<Opening | null>(null);
  // What a change of the reader's that `Awaiting` names waits for before the list shows its
  // active option whole, null while none waits.
  const awaiting = useRef<Awaiting | null>(null);

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
  const activeRow =
    activeValue === null
      ? undefined
      : activeValue !== undefined && rows.includes(activeValue)
        ? activeValue
        : rows[0];
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
  // A single pick puts the focus on the trigger before it hands the value over, so a pick that
  // takes the trigger away - the search bar's Project list, gone once its narrowing is on - takes
  // it from the trigger's own place, from which the panel around it hands the focus on
  // (`useFocusPlace`), rather than from a list portalled outside that panel. A multi-select's pick
  // waits for the host's answer (`awaiting`), with whether a pointer pressed the row (`pointer`).
  const choose = (value: string, pointer: boolean) => {
    setActiveValue(value);
    if (props.mode === "single") {
      onOpenChange(false, "pick");
      triggerRef.current?.focus();
      props.onPick(value);
      return;
    }
    const picked = isPicked(value);
    awaiting.current = { picked, pointer, value };
    props.onChange(
      picked ? selected.filter((current) => fold(current) !== fold(value)) : [...selected, value]
    );
  };
  // A multi-select's row adds the typed value and starts a fresh search, which shows the option
  // it then marks active whole once it is drawn (`awaiting`). A single pick's row hands it to the
  // host, which answers whether it took it: taken, the list closes as a pick does; refused, the
  // list stays open with the text in its box for the reader to correct.
  const create = (value: string) => {
    setActiveValue(value);
    if (props.mode !== "single") {
      props.onCreate?.(value);
      setQuery("");
      awaiting.current = { query: "" };
    } else if (props.onCreate?.(value) === true) {
      onOpenChange(false, "pick");
      triggerRef.current?.focus();
    }
  };

  // Opening starts a fresh search on the value a single pick holds, with the character typed on
  // the closed trigger in the text box, if one opened it, before the open paints: the search the
  // list last closed on would otherwise narrow the rows its first frame draws. The option it
  // starts on is then kept shown whole (`opening`) until the close ends that, and the panel hands
  // the focus to the search unless the host hands it in itself (`focusOnOpen`). The close ends
  // what a change of the reader's waits for too (`awaiting`), so the next open scrolls nothing
  // for it.
  useLayoutEffect(() => {
    if (!open) {
      letGo(opening);
      awaiting.current = null;
      return;
    }
    const value = opensOn.current;
    setQuery(opensWith.current);
    setActiveValue(value);
    opening.current =
      value === undefined ? null : { among: null, query: opensWith.current, resized: null, value };
    opensWith.current = "";
  }, [open]);
  // Closing returns the focus to the trigger when the close left it nowhere - the list that held
  // it is gone - and leaves a focus that is somewhere. A focus still on the text box the close
  // took away is nowhere (`focusLost`): an engine that moves the focus off a removed element only
  // at its next rendering update still names the box focused when this effect runs, before that
  // update, and a Tab out of the box would otherwise leave the focus there, to fall onto the page,
  // rather than on the trigger. A press outside moves the focus after this runs, so the focus ends
  // where that press put it.
  useEffect(() => {
    if (!open && wasOpen.current && focusLost()) triggerRef.current?.focus();
    wasOpen.current = open;
  }, [open]);

  // ArrowDown and ArrowUp, with Alt or without, open the closed list as a click does. A typed
  // character opens it with that character in the text box, and the caret after it. The key's
  // own typing is cancelled, so the character is not typed a second time into the text box the
  // focus moves to. Space is left to the button, which opens the list with an empty search. A
  // disabled or busy trigger opens on none of these keys, and a key held with Ctrl or Meta is
  // the browser's.
  const handleTriggerKeyDown = (event: KeyboardEvent<HTMLButtonElement>) => {
    if (open || disabled || ariaDisabled || event.ctrlKey || event.metaKey) return;
    const arrow = event.key === "ArrowDown" || event.key === "ArrowUp";
    const typed = !event.altKey && event.key.length === 1 && event.key !== " ";
    if (!arrow && !typed) return;
    event.preventDefault();
    if (typed) {
      opensWith.current = event.key;
      onQueryChange?.(event.key);
    }
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
  // press gives the search box the focus only where the focus is lost then (`focusLost`): on the
  // body, or still on the removed row in an engine that moves the focus off a removed element only
  // at its next rendering update. A pointer press focuses none of them (`onMouseDown` below), and
  // leaves the focus where it is. A row or a Create row the options take away holds no press, and
  // its focus stays on the page's body, as for every change the reader did not make. A popover that
  // closes takes the search box along, and the close above gives the focus back to the trigger
  // instead.
  const focusSearchFirst = (event: { readonly currentTarget: EventTarget }) => {
    if (event.currentTarget === document.activeElement) inputRef.current?.focus();
  };
  // Whether a row was pressed while it held the focus, until the commit that answers the press:
  // the press asks for a render, so that commit comes even where the press changes nothing else.
  const focusedRowPressed = useRef(false);
  const [, renderAfterPress] = useState(0);
  const noteFocusedRow = (event: { readonly currentTarget: EventTarget }) => {
    if (event.currentTarget !== document.activeElement) return;
    focusedRowPressed.current = true;
    renderAfterPress((renders) => renders + 1);
  };
  useEffect(() => {
    const pressed = focusedRowPressed.current;
    focusedRowPressed.current = false;
    if (pressed && focusLost()) inputRef.current?.focus();
  });

  // The text box stuck to the list's top, and the Clear foot stuck to its bottom while it is
  // drawn: the rows scroll under both.
  const headRef = useRef<HTMLDivElement>(null);
  const footRef = useRef<HTMLDivElement>(null);
  // The part of the list's box, in the window, that shows a row whole: from the bottom of the text
  // box to the top of the Clear foot, or to the box's own bottom where no foot is drawn, which is
  // measured only then; null while no list is drawn.
  const rowsShown = () => {
    const head = headRef.current;
    const box = head?.parentElement ?? null;
    if (head === null || box === null) return null;
    const top = head.getBoundingClientRect().bottom;
    const bottom = footRef.current?.getBoundingClientRect().top ?? shownSpan(box).bottom;
    return { bottom, box, top };
  };
  // Scrolls the list the least that shows option `value` whole in that part (`rowsShown`), so an
  // option already whole scrolls nothing. Only the list's own box scrolls (`scrollToShow`).
  const showOption = (value: string) => {
    const option = document.getElementById(optionId(value));
    if (option === null) return;
    const shown = rowsShown();
    if (shown !== null) scrollToShow(shown.box, option.getBoundingClientRect(), shown);
  };
  // ArrowDown and ArrowUp show the option they mark active whole. The reader has moved on from the
  // option the list opened on, which is no longer kept shown (`letGo`), and from what a change of
  // theirs waited for (`awaiting`), which then scrolls nothing.
  const markActive = (value: string) => {
    letGo(opening);
    awaiting.current = null;
    setActiveValue(value);
    showOption(value);
  };
  // Four changes of the reader's show the option the list then marks active whole, as the arrows
  // do, in the commit that draws what answers them (`awaiting`), before it paints. A word typed in
  // the text box or cleared marks the first option it leaves active. The empty search a
  // multi-select's Create row starts marks active the value it made, where the host lists it by
  // then, else the first option. Without that scroll the box stayed where the arrows had scrolled
  // it: in the issue header's Status, Triage stood 39 px above what the box showed after "e". A
  // multi-select's Enter ticks or unticks the active option, and a key's press of a row that holds
  // the focus the row it presses, which it marks active. The host's pick that answers it can draw
  // the Clear foot over the bottom of the box - the first tick drew it over lab-07 in the project
  // List's Labels filter - or move the option, as a host that lists the picked values first does
  // (the Delivery page's facets): a tick took acme/r06 to the top of a box scrolled 83 px, and the
  // untick took it back below what the box showed. Clear keeps active the option it finds active.
  // Where that option is picked, such a host moves it from the top back to its own place, which
  // can stand below what the box shows. Neither answer need come in a commit of the list's own:
  // the host applies the pick in its own time, the Delivery page's through the address. So the
  // list waits for the commit in which the host's pick holds the value otherwise, or holds nothing
  // after Clear, which draws the foot and the order that come with it, and shows the option then.
  //
  // A pointer's press of a row ticks or unticks it as well, and marks it active, but scrolls
  // nothing: the reader picks among the rows the box shows, and a scroll moves those rows under
  // the pointer. Once the host's pick answers the press, the list keeps that option active where
  // it stands whole between the text box and the Clear foot (`rowsShown`), and else marks active
  // the first option that does, so the next Enter takes an option the reader sees rather than the
  // one the press moved out of sight. Where no option stands whole there, it marks none, and Enter
  // takes nothing until an arrow or a word typed marks one. Single picks and their Create rows
  // set no wait. Arrows and closing cancel the wait; another pick, Clear, or text edit replaces it.
  useLayoutEffect(() => {
    const awaited = awaiting.current;
    if (awaited === null) return;
    const answered =
      "query" in awaited
        ? awaited.query === query
        : "value" in awaited
          ? isPicked(awaited.value) !== awaited.picked
          : selected.length === 0;
    if (!answered) return;
    awaiting.current = null;
    if (activeRow === undefined) return;
    if (!("pointer" in awaited && awaited.pointer)) {
      showOption(activeRow);
      return;
    }
    const shown = rowsShown();
    if (shown === null) return;
    const whole = (row: string) => {
      const drawn = document.getElementById(optionId(row))?.getBoundingClientRect();
      return drawn !== undefined && drawn.top >= shown.top && drawn.bottom <= shown.bottom;
    };
    if (!whole(activeRow)) setActiveValue(rows.find(whole) ?? null);
  });
  // The list opens with the option it starts on shown whole (`opening`), a single pick's picked
  // value, as a `<select>` opens on it: the issue header's Status list on an issue in Testing,
  // Needs review or Retro opened with that status below the five rows its box shows, and Enter
  // took it unseen. A layout effect, in the commit that draws the rows of the open's own search:
  // the open's own commit where the list last closed on that search, else the one its setting of
  // the search makes right after it, before anything paints. So the first frame that paints those
  // rows already shows the option whole. Every later commit that changes the rows, or the text one
  // of them draws, shows it whole again, until the reader moves on in the list: the issue's
  // Assignee list reads its people only once it is opened, so it opens on Unassigned and the
  // assignee, and when the people came the assignee moved to its place among everyone, still the
  // active option, below the rows its box shows. A host's label or detail of a row can answer
  // otherwise once a read has come, with the same rows: a longer label wraps and makes its row
  // taller while the box keeps its size. A change of the list box's own size shows it whole again
  // too (below). An arrow (`markActive`), a word typed in the text box and a scroll of the
  // reader's own (below) each end that, and so does the close.
  useLayoutEffect(() => {
    const opened = opening.current;
    if (opened === null || opened.query !== query) return;
    // Each row by its value and the text it draws, the Create row's label or an option's label and
    // detail, written as JSON so that two different rows never read the same.
    const drawn = rows.map((row) =>
      JSON.stringify(
        row === createValue ? [row, createLabel(row)] : [row, labelOf(row), optionDetail?.(row)]
      )
    );
    const { among } = opened;
    if (among?.length === drawn.length && among.every((row, at) => row === drawn[at])) return;
    opened.among = drawn;
    if (rows.includes(opened.value)) showOption(opened.value);
  });
  // While the open keeps the option shown (`opening`), a change of the list box's own size shows
  // it whole again: where the window has too little room for the panel, the panel cuts its box to
  // that room (`AnchoredPanel`) in a commit after the one that showed the option, and a box cut
  // shorter can leave the option cut at its bottom. What watches the box stops as the open lets
  // go of the option (`letGo`) and as the list closes.
  //
  // The reader's own scroll of the list ends what the open keeps shown, so a later change of the
  // rows or of the box leaves the list where they put it: a turn of the wheel or a touch dragged
  // over it; a press on it outside its text box - on its scrollbar, a middle button's autoscroll,
  // a drag that selects its words past its edge; a key that can scroll it - PageUp or PageDown,
  // even from its text box, or any key while the focus is in the list but not in its text box;
  // and the focus reaching a row, as a screen reader's cursor puts it there, which the panel
  // scrolls into the box's view (`AnchoredPanel`). Heard on the list's box in the capture phase,
  // before anything inside it can stop them, and passively, so no scroll waits on them.
  // biome-ignore lint/correctness/useExhaustiveDependencies: `showOption` reads refs and the list's own id alone, so the open's copy shows the option as any later render's would; with it the effect would watch and listen anew at every render.
  useEffect(() => {
    const head = headRef.current;
    const box = head?.parentElement ?? null;
    const kept = opening.current;
    if (!open || kept === null || head === null || box === null) return;
    const resized = new ResizeObserver(() => showOption(kept.value));
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

  const handleKeyDown = (event: KeyboardEvent<HTMLInputElement>) => {
    // A key that ends or moves an IME composition is the input method's: Enter commits the
    // composed text into the box and picks nothing, and Tab leaves the list open.
    if (inputMethodKey(event.nativeEvent)) return;
    if (event.key === "ArrowDown" && rows.length > 0) {
      event.preventDefault();
      const index = activeRow === undefined ? -1 : rows.indexOf(activeRow);
      markActive(rows[(index + 1) % rows.length]);
      return;
    }
    if (event.key === "ArrowUp" && rows.length > 0) {
      event.preventDefault();
      const index = activeRow === undefined ? 0 : rows.indexOf(activeRow);
      markActive(rows[(index - 1 + rows.length) % rows.length]);
      return;
    }
    if (event.key === "Enter" && activeRow !== undefined) {
      event.preventDefault();
      if (activeRow === createValue) {
        create(activeRow);
      } else {
        choose(activeRow, false);
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
          if (open) onOpenChange(false, "trigger");
          else onOpenChange(true);
        }}
        onFocus={(event) => {
          // The focus comes back from the list while the list is open and until its close has
          // run the effect above, which is while `wasOpen` holds.
          if (!wasOpen.current) onTriggerFocus?.(event.relatedTarget);
        }}
        onKeyDown={handleTriggerKeyDown}
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
          // A pointer tick in a multi-select scrolls nothing. Firefox otherwise follows the moved
          // option through native scroll anchoring. Arrow and Enter reveal use scrollTop directly.
          overflowAnchor={single ? undefined : "none"}
        >
          <div
            className={`sticky top-0 border-b p-2 ${tooltips ? card : ON_PAGE.panel}`}
            ref={headRef}
          >
            <input
              aria-activedescendant={activeRow === undefined ? undefined : optionId(activeRow)}
              aria-autocomplete="list"
              aria-controls={listboxId}
              aria-expanded="true"
              aria-label={searchLabel}
              {...marks?.search}
              className={`block min-h-11 w-full rounded-lg px-3 py-2 text-sm outline-none ${tooltips ? inputClasses(true) : ON_PAGE.search}`}
              disabled={disabled}
              onChange={(event) => {
                // A word typed moves the reader on from the option the list opened on, which is
                // no longer kept shown (`letGo`), even once the search is back where it began, and
                // the option it marks active is shown whole once its search is drawn (`awaiting`).
                letGo(opening);
                awaiting.current = { query: event.target.value };
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
                  // A pointer's click counts its clicks (`detail` 1 or more); the click a key makes
                  // on a row that holds the focus counts none.
                  onClick={(event) => choose(option, event.detail > 0)}
                  onClickCapture={noteFocusedRow}
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
          {props.mode === "single" || selected.length === 0 ? null : (
            <div
              className={`sticky bottom-0 border-t p-2 ${tooltips ? card : ON_PAGE.panel}`}
              ref={footRef}
            >
              <button
                className={`min-h-11 w-full rounded-lg px-3 py-2 text-sm font-medium ${tooltips ? `${surfaceMutedBg} ${textSecondaryOnSurface}` : ON_PAGE.clear}`}
                disabled={disabled}
                onClick={() => {
                  // Clear keeps active the option it finds active, which it shows whole once the
                  // host's pick holds nothing (`awaiting`).
                  if (activeRow !== undefined) setActiveValue(activeRow);
                  awaiting.current = { cleared: true };
                  props.onChange([]);
                }}
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
