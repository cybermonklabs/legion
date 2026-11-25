import {
  type FocusEvent,
  type KeyboardEvent,
  type ReactNode,
  type RefObject,
  useLayoutEffect,
  useRef,
  useState,
} from "react";

import { focusOnDocument } from "../features/shell/roving";
import { observeStrip } from "../hooks/observe-strip";
import { borderDefault, borderTransparent, card } from "../theme/classes";
import { AnchoredPanel } from "./AnchoredPanel";
import { BarAction, type BarActionProps, type ViewPanelProps } from "./BarAction";
import { barButton, FUNNEL_GLYPH, INFO_GLYPH, SLIDERS_GLYPH } from "./bar-look";
import { Chip } from "./Chip";
import { ColumnsMenu, type ColumnsMenuProps } from "./ColumnsMenu";
import {
  CONTROL,
  type ControlSize,
  controlIn,
  controlSidePadding,
  NARROW_BELOW,
} from "./control-names";
import { DisplayMenu, type DisplayMenuProps } from "./DisplayMenu";
import { type FilterChip, filterChips } from "./filter-chips";
import { ARM_ON_PRESS, landWhole, useFocusPlace } from "./focus-place";
import { MoreMenu } from "./MoreMenu";
import { PANEL_FRAME, PanelTrigger, type PanelTriggerProps } from "./PanelTrigger";
import { SEARCH_HINT_ROOM, SearchBox, type SearchBoxProps } from "./SearchBox";
import { SegmentedControl, type SegmentedControlProps } from "./SegmentedControl";
import { ViewPanel } from "./ViewPanel";

/** The panel `+N more` opens: a card over the page, as a filter panel is (`PANEL_FRAME`), that
 *  lists the chips the line has no room for one under another, each at its own width. */
const MORE_PANEL_CLASS = `${PANEL_FRAME} items-start gap-2 p-2 ${card}`;

/** How far past its room a line of chips may reach and still fit. The widths read are rounded to
 *  the layout's own units, so their sum can miss the laid-out line by a fraction of a pixel. */
const SLACK_PX = 0.5;

/** One slot of the bar: its own single line, which cannot wrap, so whatever it holds grows
 *  sideways and never downward. `data-row-slot` marks it as the container a focused control in
 *  it is counted in, for the bar's focus hand-off. */
function Slot({ children }: { children: ReactNode }): ReactNode {
  if (children === undefined || children === null) return null;
  return (
    <div className="flex shrink-0 flex-nowrap items-center gap-2" data-row-slot="">
      {children}
    </div>
  );
}

/** The chips line as the row last read it. */
interface LineLayout {
  /** The width the line's box gives the chips and `+N more` together. */
  readonly room: number;
  /** The gap between two chips, and between the last chip and `+N more`. */
  readonly gap: number;
  /** `+N more`'s width as last drawn, or 0 before it ever was. */
  readonly more: number;
  /** Each chip's width as last drawn in the line, by its key, kept while the panel holds that
   *  chip. A chip the line has not drawn since it came has none. */
  readonly widths: ReadonlyMap<string, number>;
}

/** What one read of the chips line found: the widths of the chips it draws alone, and `+N more`'s
 *  only while it is drawn. */
type LineReading = Omit<LineLayout, "more"> & { readonly more: number | undefined };

/**
 * Reads the chips line as the page lays it out now: `box` holds the strip of the chips the line
 * draws, whose keys are `drawn` in order, and `more`, `+N more` while it is drawn. Null where the
 * page lays the box out nowhere (no room): a test DOM, or a line in a box the page does not draw,
 * which draws every chip until it has room to share out.
 *
 * A chip's width is its own, not the width `max-w-full` cuts a chip wider than the line to: the
 * strip is read as wide as all it holds, for this read alone.
 */
function readLine(
  box: HTMLElement,
  more: HTMLElement | null,
  drawn: readonly string[]
): LineReading | null {
  const room = box.getBoundingClientRect().width;
  const strip = box.firstElementChild;
  if (room === 0 || !(strip instanceof HTMLElement)) return null;
  // Read before the strip is widened, since neither the gap nor `+N more`, which never shrinks,
  // changes with the strip's width: read after it, they would lay the page out once more.
  const gap = Number.parseFloat(getComputedStyle(strip).columnGap) || 0;
  const moreWidth = more?.getBoundingClientRect().width;
  strip.style.flex = "none";
  strip.style.width = "max-content";
  const widths = new Map<string, number>();
  for (const [index, key] of drawn.entries()) {
    const chip = strip.children[index];
    if (chip !== undefined) widths.set(key, chip.getBoundingClientRect().width);
  }
  strip.style.flex = "";
  strip.style.width = "";
  return { gap, more: moreWidth, room, widths };
}

/** `reading` with `before`'s widths kept for the chips of `keys` it did not draw, or `before`
 *  itself where nothing changed, so a line read again unchanged draws nothing again. */
function merged(
  before: LineLayout | null,
  reading: LineReading | null,
  keys: readonly string[]
): LineLayout | null {
  if (reading === null) return null;
  const widths = new Map<string, number>();
  for (const key of keys) {
    const width = reading.widths.get(key) ?? before?.widths.get(key);
    if (width !== undefined) widths.set(key, width);
  }
  const more = reading.more ?? before?.more ?? 0;
  const same =
    before !== null &&
    before.room === reading.room &&
    before.gap === reading.gap &&
    before.more === more &&
    before.widths.size === widths.size &&
    [...widths].every(([key, width]) => before.widths.get(key) === width);
  return same ? before : { gap: reading.gap, more, room: reading.room, widths };
}

/**
 * How many of the chips of `keys`, from the first, the line draws: every one where all of them
 * fit its room, else as many as fit whole with `+N more` and its gap after them - none, where the
 * room holds less than one whole chip beside it, so the line shows `+N more` alone and never a
 * chip cut short. A chip with no width yet is drawn, so the next read measures it (`probe`), save
 * while the panel is open: a chip the panel holds stays there.
 */
function chipsThatFit(keys: readonly string[], line: LineLayout, probe: boolean): number {
  const { gap, more, room, widths } = line;
  let end = -gap;
  for (const key of keys) end += gap + (widths.get(key) ?? Number.POSITIVE_INFINITY);
  if (end <= room + SLACK_PX) return keys.length;
  const limit = room - gap - more;
  let count = 0;
  end = -gap;
  for (const key of keys) {
    const width = widths.get(key);
    if (width === undefined) return probe ? count + 1 : count;
    end += gap + width;
    if (end > limit + SLACK_PX) break;
    count += 1;
  }
  return count;
}

/** A removal a click has started, which hands the keyboard focus on once it has committed:
 *  `from` is what was clicked - a chip, or **Clear filters** - and `index` is that chip's place
 *  among all the row's chips, the line's first and then the panel's, or -1 for **Clear
 *  filters**, whose focus goes to the row's filter trigger. `panelPlace` is its place among the
 *  panel's chips, for a chip clicked in the panel, else null. */
interface HandOff {
  readonly from: Element;
  readonly index: number;
  readonly panelPlace: number | null;
}

/** What takes the focus in a chip: a chip is a button or a link itself, and a chip may sit in a
 *  box with other controls (a chip given a link, the Inbox's agent chip with the agent's live
 *  link after it). */
const FOCUSABLE = "a[href], button";

/** The chip a click at `target` landed in: the child of `chips` - the line's strip, or the
 *  panel's list - that holds it, a chip or the box a chip is drawn in, and that child's place
 *  among them; null when `target` is `chips` itself. */
function chipHolding(chips: Element, target: EventTarget): { from: Element; index: number } | null {
  if (!(target instanceof Node)) return null;
  const children = Array.from(chips.children);
  const index = children.findIndex((child) => child.contains(target));
  const from = children[index];
  return from === undefined ? null : { from, index };
}

/** Delete takes a focused chip off as Enter and Space do, which a button does by itself. It
 *  presses a chip's own control alone - a child of the strip or the panel's list itself, or a
 *  `Chip` - and never another control drawn in its chip's box, such as a chip's link. */
function deleteChip(event: KeyboardEvent<HTMLElement>): void {
  const { currentTarget: chips, key, target } = event;
  if (key !== "Delete" || !(target instanceof HTMLElement)) return;
  const own =
    (target.parentElement === chips && target.matches(FOCUSABLE)) ||
    target.getAttribute("data-control") === CONTROL.chip;
  if (!own) return;
  event.preventDefault();
  target.click();
}

/** The element of a chip the focus goes to: the chip itself, or the first link or button in the
 *  box its chip is drawn in. */
function focusableOf(chip: Element): HTMLElement | null {
  if (chip instanceof HTMLElement && chip.matches(FOCUSABLE)) return chip;
  return chip.querySelector<HTMLElement>(FOCUSABLE);
}

/** Where the focus goes once nothing of its own place is left: the row's filter trigger, or the
 *  search box in a row with no trigger. */
function triggerOf(root: Element): HTMLElement | null {
  return controlIn(root, CONTROL.panelTrigger) ?? controlIn(root, CONTROL.searchBox);
}

/**
 * The chips at the top of the trigger's panel, each with its remove control. The list is a focus
 * place of its own (`useFocusPlace`), as the bar's line is: a chip taken off hands the focus to the
 * chip now at its place, else to the one before it, rather than to the panel's next field, which
 * `PanelTrigger`'s own hand-off, counting the whole panel, would land on (D26). Its check runs
 * before the panel's, a child's effect before its parent's, so it lands first. The list goes with
 * its last chip, and the panel's hand-off then lands on the control now at its place, the panel's
 * first field. A chip's press arms the hand-off at the list's root (`armOnPress`), as every
 * component that hands its focus off arms it at its own, so its going is the reader's. A chip that
 * takes the focus stands whole in the panel's box, as every control in an anchored panel does
 * (`AnchoredPanel`).
 */
function PanelChips({
  chips,
  size,
}: {
  chips: readonly FilterChip[];
  size: ControlSize;
}): ReactNode {
  const list = useRef<HTMLDivElement>(null);
  const noteFocus = useFocusPlace(
    () => list.current,
    () => null
  );
  return (
    // biome-ignore lint/a11y/noStaticElementInteractions: nothing here is made interactive - the focus handler only notes which chip of the list took the focus, for the hand-off when a commit takes that chip away (`useFocusPlace`), and the capture handlers only arm that hand-off at a press inside the list (`armOnPress`), so the list needs no role of its own.
    <div
      className="flex w-full flex-col items-start gap-2"
      {...ARM_ON_PRESS}
      onFocus={noteFocus}
      ref={list}
    >
      {filterChips(chips, size)}
    </div>
  );
}

/** The bar's own width, read as it is laid out and whenever it changes size: the window
 *  resized, the drawer opened. 0 where the page lays it out nowhere, a test DOM. */
function useOwnWidth(ref: RefObject<HTMLElement | null>): number {
  const [width, setWidth] = useState(0);
  useLayoutEffect(() => {
    const bar = ref.current;
    if (bar === null) return;
    const read = () => setWidth(bar.getBoundingClientRect().width);
    read();
    if (typeof ResizeObserver === "undefined") return;
    const sizes = new ResizeObserver(read);
    sizes.observe(bar);
    return () => sizes.disconnect();
  }, [ref]);
  return width;
}

/** Whether `action` stands in **More** at every width: kept there by the page, or a check item. */
function keptInMore(action: BarActionProps): boolean {
  return action.more === true || action.checked !== undefined;
}

const NO_ACTIONS: readonly BarActionProps[] = [];

export interface ControlRowProps<TData = unknown> {
  /** The page's actions, in its order: each a quiet button of the bar's line after its empty
   *  space, and an item of **More** where the page keeps it there (`more`, `checked`) and below
   *  640 px of the bar's width. */
  readonly actions?: readonly BarActionProps[];
  /** The chips of the narrowings that are on, in the bar's line after the trigger, behind one
   *  **+N more** where they do not fit, and at the top of the trigger's panel at every width. */
  readonly chips?: readonly FilterChip[];
  /** The caller's outer layout alone - where the bar sits on the page, and what space it keeps
   *  from what is above and below it. The order inside it is the bar's. */
  readonly className?: string;
  /** What **Clear filters** does: sets the narrowings back to the page's own. Given only while
   *  something its press sets back differs from the page's own, and drawn only where the bar is
   *  640 px wide or wider. */
  readonly clear?: () => void;
  /** A table's columns menu. */
  readonly columns?: ColumnsMenuProps<TData>;
  /** The display menu, under the label its page gives it (**View**, **Group by**), which marks
   *  the current sort and grouping. */
  readonly display?: DisplayMenuProps;
  /** The page's (i) hint, a `view-panel` trigger in the bar. */
  readonly hint?: ViewPanelProps;
  /** The page's one main action, the bar's last control and its one filled one. */
  readonly main?: BarActionProps;
  /** The bar's narrow display rule, which the Inbox and its drawer set: below 640 px of the
   *  bar's width the display menu is an item of **More** rather than a control of the line,
   *  because the Inbox's bar has no room for it there (the measurement of 2026-11-24). **More**
   *  is then drawn while it holds the display menu alone. */
  readonly narrowDisplayInMore?: boolean;
  /** The page's search box, the bar's first control. */
  readonly search?: SearchBoxProps;
  /** How much room the bar's controls take (`ControlSize`): the bar hands it to every control
   *  it draws, whatever size a slot's own props name. */
  readonly size?: ControlSize;
  /** The switch (`SegmentedControl`) and a divider after it. */
  readonly switch?: SegmentedControlProps;
  /** **Filter**, the page's panel trigger, which counts the narrowings that differ from the
   *  page's own (`count`). */
  readonly trigger?: Omit<PanelTriggerProps, "control">;
}

/**
 * The one control bar a page's controls sit in, laid out as established inboxes lay theirs out
 * (GitHub Notifications, Outlook on the web, Linear's Inbox, Front): one line directly above the
 * list, which never wraps onto a second line and never grows. From its start to its end it
 * holds the search box, the switch and a divider after it, **Filter** and **Clear filters**, the
 * chips of the narrowings that are on, empty space, the page's actions, the hint, the columns
 * menu, the display menu, **More** and the page's one main action - in that order on every page,
 * so a reader who learns one page's bar has learned them all. A page hands each control as that
 * control's props, in a slot of its own kind, and the bar draws each shared component itself,
 * at the page's size.
 *
 * Its buttons are quiet - no border and no fill at rest, as Primer's ActionBar and Apple's
 * toolbars draw theirs (`BarLook`) - but the chosen part of the switch, which has a filled
 * surface and a heavier label, and the main action, the one filled button, last, as Apple puts a
 * toolbar's one primary action on its trailing side. Each control writes its name into
 * `data-control`. Below 1280 px of the window's width each keeps the 44 px touch floor
 * `styles.css` sets for every button.
 *
 * **Filter** reads **Filter · N** while N narrowings differ from the page's own, on a tint, with
 * **Clear filters**, a × button, beside it in the same tinted group: two buttons, not one inside
 * the other. Its panel lists each narrowing that is on at its top, above the page's own fields,
 * each with its remove control, at every width: below 640 px it is where the reader takes one off.
 * **More** (⋯) is drawn only while it holds an action: the actions the page keeps there, check
 * items among them, and every action below 640 px of the bar's width.
 *
 * The bar reads its own width (a `ResizeObserver` on the bar, not the window), so a narrow window
 * and a narrow box, the Inbox drawer, take the same rule, and it has two layouts split at 640 px
 * of that width. From 640 px it is as above. Below it the chips leave the line, and **Filter**
 * shows their count as a badge on its glyph, with no **Clear filters** beside it. A bar with
 * chips and no **Filter** - the priorities table's - keeps one `+N more` chip in the line, and no
 * other chip, which lists them all. Every action moves into **More**, and **Filter**, the display
 * menu, **More** and the main action show their glyphs alone, each keeping its name for a screen
 * reader. The switch keeps its labels, but a part the page gives a short label draws it, its name
 * unchanged (the Inbox's **All** and **You · N**); and where the page sets the bar's narrow
 * display rule (`narrowDisplayInMore`, the Inbox and its drawer) the display menu is an item of
 * **More** too. A bar laid out nowhere, as in a test DOM, takes the wide layout. A panel open
 * when the bar crosses 640 px stays open on its trigger, which the bar keeps.
 *
 * The bar never grows, which is the height rule: applying a narrowing, opening a panel, opening
 * the display menu, selecting rows or showing an action each leave the top of the list's first
 * row where it was. The whole arithmetic is these points:
 *
 * - the bar is one flex line (`flex-nowrap`), its items centred, so its height is its tallest
 *   control's, one control at the bar's size;
 * - every slot lays its own nodes out in one line, so a slot handed more to draw grows sideways,
 *   never downward;
 * - the chips stand in that line, in the box the empty space is, which takes the line's room the
 *   rest of it leaves and gives way rather than push a control past the edge. The line draws the
 *   chips that fit that box, from the first, and a chip that does not fit is not drawn in it: one
 *   `+N more` chip at the end of the chips stands for every chip left out;
 * - the panels the bar's triggers open, the panel `+N more` opens and the selection bar take no
 *   room in it: `AnchoredPanel` portals to the body and is placed `fixed`, and `SelectionBar`
 *   draws over the list.
 *
 * The search box never narrows past a placeholder that names what it searches: its slot is never
 * narrower than the placeholder's own width in the box's own font.
 *
 * What fits the chips' box is measured, not guessed: the box the chips and `+N more` share, less
 * `+N more`'s own width and its gap, against each chip's own width and the gaps between them.
 * Where every chip fits the whole box, no `+N more` is drawn. Every chip the line draws is whole,
 * at every width: where the box has room for less than one whole chip, the line shows `+N more`
 * alone, which lists every chip - as the narrow layout of a bar with no trigger always does. A
 * chip's width is read when the line draws it and kept while the panel holds it, so the line is
 * drawn right at once when a chip is taken off: a chip after it that now fits moves into the
 * line, and `+N` counts down, or goes once every chip fits.
 * The line is read again after a commit that changed what it draws, and whenever its box, the
 * strip or `+N more` changes size (`observeStrip`): a window resized, a chip's name arriving.
 *
 * `+N more` is named for what it opens (`2 more filters`), and opens one panel (`AnchoredPanel`)
 * that lists the chips the line left out, as the same elements the line draws, each with its own
 * remove action. Each chip is drawn once, in the line or in that panel, so no id is drawn twice
 * there. The panel's focus follows every panel's rules: it opens with the focus on its first
 * chip, and Escape, or Tab or Shift+Tab past its last or first chip, close it and hand the focus
 * back to `+N more`.
 *
 * A chip or **Clear filters** that is taken off hands the keyboard focus on rather than drop it
 * onto the page, where a keyboard reader would start again from the page's first link for each
 * chip. A chip taken off - by Enter, Space or Delete, or by a pointer, under the same rule - hands
 * it to the chip now at its place, else to the chip before it, else to the bar's filter trigger
 * (`data-control="panel-trigger"`, or the search box in a bar with no trigger), and **Clear
 * filters** hands it to the trigger. A chip's place is among all the bar's chips, the line's
 * first and then the panel's, so a chip that moved into the line in its place takes it. A chip
 * taken off in the panel while the panel stays hands it to the panel's chip now at its place, else
 * to the one before it, since the panel is where the reader is. The panel's last chip taken off
 * closes the panel, and the focus goes by the line's rule: to the chip now at its place, else to
 * the last chip of the line. Delete presses a chip's own control alone - the child of the strip
 * or of the panel's list itself, or a `Chip` - and never another control drawn in its chip's box,
 * such as a chip's link. The hand-off waits for the removal to commit, whatever holds the focus
 * meanwhile (a pointer's click in Safari focuses no button, so the focus stays on the body), and
 * then moves the focus only if it is on the page's body, so a reader who moved it elsewhere while
 * the chip was going keeps it there. It lands the control through `landWhole`: with no scroll of
 * the engine's own, and shown whole at its nearest edge whether the reader can see a part of it
 * or none, at once and again once the commit is done, since the Inbox list's anchor puts this row
 * back where the commit found it after the hand-off has run (`ViewportAnchor`'s `keepPressedRow`).
 * A chip taken off in the trigger's panel hands it to the panel's chip now at its place, else to
 * the one before it, as the line's chips do: the panel's list of chips is a focus place of its own
 * (`PanelChips`). Its last chip takes the list with it, and `PanelTrigger` then hands the focus on
 * inside the panel, to its first field.
 *
 * Any other focused control the reader's own press takes out of a slot while the bar stays hands
 * the focus on too (`useFocusPlace`): to the control now at its place in that slot, else to the
 * nearest one before it, passing over every control that takes typed text, else to the trigger,
 * each only while the reader can see it, else to the own stop of the region the bar is in - the
 * search bar's dialog or the Inbox drawer, else the main content - with no scroll. With nothing
 * left before it in the slot and no trigger, the focus walks back from the slot's place in that
 * region instead, since the search box takes typed text. Everything after the empty space is one
 * slot, so an action that removes itself hands the focus to the control after it, and **More**
 * that goes once its last action leaves (**Mark all read** no longer offered) hands it to the main
 * action, else to the control before it. A loss is the reader's only while a press of theirs is
 * armed (`armHandOff`): every press inside the bar arms the hand-off at the bar's root
 * (`armOnPress`), a press in a panel the bar opens included, which React hands up through the bar,
 * and an item of **More** arms it on **More** itself, as the menu shuts and puts the reader back
 * there (`MoreMenu`). A control a live update or a refetch takes away, with nothing armed, leaves
 * the focus on the page with no scroll. An action of the line that holds the focus when the bar
 * narrows past 640 px, and so moves into **More**, hands the focus to **More**'s trigger, where
 * that action now is, whatever narrowed the bar: the focus follows the action and lands on no
 * other control. **Clear filters**' own rule, the trigger, comes first wherever it sits. A control
 * of a dialog, a panel or a selection bar a page draws in a slot is that region's and none of the
 * bar's, so the bar hands its focus nowhere, and never to a control of the bar outside that
 * region. In a selection bar, the bar's own hand-off (`SelectionBar`) answers a loss the reader
 * caused.
 */
export function ControlRow<TData = unknown>({
  actions = NO_ACTIONS,
  chips,
  className,
  clear,
  columns,
  display,
  hint,
  main,
  narrowDisplayInMore = false,
  search,
  size = "comfortable",
  switch: segments,
  trigger,
}: ControlRowProps<TData>): ReactNode {
  const rowRef = useRef<HTMLDivElement>(null);
  const handOff = useRef<HandOff | null>(null);
  // The box the chips and `+N more` share. The bar draws it in every layout, so a ref holds it
  // from the bar's first commit to its last, and its observers start and stop with the bar: a
  // bar taken away sets no state as it goes.
  const boxRef = useRef<HTMLDivElement>(null);
  const moreRef = useRef<HTMLButtonElement>(null);
  const panelRef = useRef<HTMLDivElement>(null);
  const [layout, setLayout] = useState<LineLayout | null>(null);
  const [open, setOpen] = useState(false);
  // The action of the bar's line that holds the focus, noted as it takes it, for the hand-off to
  // **More** when the bar narrows and the action moves there.
  const focusedAction = useRef<Element | null>(null);

  const width = useOwnWidth(rowRef);
  const narrowBar = width > 0 && width < NARROW_BELOW;

  // The chips as `filterChips` draws them, one element each, each keeping its own key, so a chip
  // the line hands to the panel stays the same chip. Below 640 px a trigger whose badge stands
  // for the chips takes them out of the line, and a bar with no trigger keeps every chip behind
  // one `+N more`.
  const all =
    chips === undefined || (narrowBar && trigger !== undefined) ? [] : filterChips(chips, size);
  const keys = all.map((chip) => String(chip.key));
  const shown = narrowBar ? 0 : layout === null ? all.length : chipsThatFit(keys, layout, !open);
  const inLine = all.slice(0, shown);
  const inPanel = all.slice(shown);
  // `+N more` goes once every chip fits the line, and its panel with it: the next `+N more` opens
  // a panel afresh rather than one left open. The row draws again at once with the panel shut,
  // before anything is committed, so no drawn row holds the panel open with no chip in it.
  if (open && inPanel.length === 0) setOpen(false);
  const moreName = `${inPanel.length} more ${inPanel.length === 1 ? "filter" : "filters"}`;

  const inMore = narrowBar ? actions : actions.filter(keptInMore);
  const inBar = narrowBar ? NO_ACTIONS : actions.filter((action) => !keptInMore(action));
  // The display menu, below 640 px, as **More**'s first item where the page sets the bar's narrow
  // display rule.
  const displayInMore = narrowBar && narrowDisplayInMore && display !== undefined;
  // The display menu as this render draws it, which the effect below reads as the layout changes.
  const displayNow = useRef(display);
  displayNow.current = display;

  // Reads the line again, against what this commit drew. A read lays the page out, so a commit
  // reads it only when what the line draws, or what `+N more` counts, has changed; a change of
  // size the line's observers report reads it whatever it draws.
  const measure = useRef(() => {});
  const measuredFor = useRef("");
  useLayoutEffect(() => {
    const drawn = keys.slice(0, shown);
    measure.current = () => {
      const box = boxRef.current;
      if (box === null) return;
      const reading = readLine(box, moreRef.current, drawn);
      setLayout((before) => merged(before, reading, keys));
    };
    const drawing = `${inPanel.length}\n${drawn.join("\n")}`;
    if (drawing === measuredFor.current) return;
    measuredFor.current = drawing;
    measure.current();
  });
  useLayoutEffect(() => {
    const box = boxRef.current;
    if (box === null) return;
    return observeStrip(box, () => measure.current(), { childList: true });
  }, []);
  // After every commit of the row: a removal a click started hands the focus on once it has
  // committed.
  useLayoutEffect(() => {
    const pending = handOff.current;
    if (pending === null) return;
    // What was clicked is still in the page: its removal commits later, whatever holds the
    // focus meanwhile.
    if (pending.from.isConnected) return;
    handOff.current = null;
    // Gone: a reader who has moved the focus elsewhere meanwhile keeps it there.
    if (!focusOnDocument()) return;
    const root = rowRef.current;
    if (root === null) return;
    // The chips as they are drawn now: the line's, read through the row, and the panel's while
    // it stands. A chip clicked in a panel that still stands is placed among the panel's chips,
    // and any other among all the row's.
    const line = Array.from(root.querySelector("[data-chip-strip]")?.children ?? []);
    const panel = Array.from(panelRef.current?.children ?? []);
    const panelPlace = panel.length > 0 ? pending.panelPlace : null;
    const among = panelPlace === null ? [...line, ...panel] : panel;
    const at = panelPlace ?? pending.index;
    const chip = at === -1 ? undefined : (among[at] ?? among[at - 1]);
    const next = chip === undefined ? triggerOf(root) : focusableOf(chip);
    if (next !== null) landWhole(next);
  });
  // An action of the line that held the focus as the bar narrowed has moved into **More**: the
  // focus goes to **More**'s trigger, where the action now is. It runs before the slots' hand-off
  // below, which then finds the focus placed.
  useLayoutEffect(() => {
    const held = focusedAction.current;
    if (held === null || held.isConnected) return;
    focusedAction.current = null;
    if (!narrowBar || !focusOnDocument() || rowRef.current === null) return;
    controlIn(rowRef.current, CONTROL.moreMenu)?.focus();
  }, [narrowBar]);
  // An open display menu whose trigger moves into **More** as the bar narrows closes with it -
  // its panel hung off a trigger that is gone, and opened again it would stand over **More**
  // unasked - and the focus it held goes to **More**'s trigger, where the menu now is, as an
  // action's does above.
  const displayWasInMore = useRef(displayInMore);
  useLayoutEffect(() => {
    if (displayWasInMore.current === displayInMore) return;
    displayWasInMore.current = displayInMore;
    const menu = displayNow.current;
    if (!displayInMore || menu === undefined || !menu.open) return;
    menu.onOpenChange(false);
    if (focusOnDocument() && rowRef.current !== null) {
      controlIn(rowRef.current, CONTROL.moreMenu)?.focus();
    }
  }, [displayInMore]);
  // A focused control a commit takes out of a slot hands the focus on too. Its check runs after
  // the chips' hand-off above, so that one's rule for Clear filters wins.
  const noteFocus = useFocusPlace(
    (element) => {
      const slot = element.closest("[data-row-slot]");
      return slot !== null && rowRef.current?.contains(slot) === true ? slot : null;
    },
    () => (rowRef.current === null ? null : triggerOf(rowRef.current))
  );
  const onFocus = (event: FocusEvent<HTMLDivElement>) => {
    noteFocus(event);
    const target = event.target;
    focusedAction.current =
      target instanceof Element && target.closest("[data-bar-actions]") !== null ? target : null;
  };

  // The chips, in the bar's empty space: the box takes the room the rest of the line leaves,
  // and gives way rather than push a control after it past the bar's edge.
  const chipsBox = (
    <div className="flex min-w-0 flex-1 flex-nowrap items-center gap-2" ref={boxRef}>
      {/* `[&>*]:shrink-0`: the line never squeezes a chip it draws. A box a chip is drawn in
          with its link could otherwise shrink narrower than its text, and the next chip would
          sit over its end and take its clicks. The line draws only the chips that fit it whole
          (`chipsThatFit`); the strip itself gives way, so a chip it draws for one read before
          its width is known is cut by its own `max-w-full` in no frame the page paints. */}
      {/* biome-ignore lint/a11y/noStaticElementInteractions: nothing here is made interactive - the handlers only act on a chip of the strip, which is a button or a link of its own: they note which chip a click takes off, and press the focused chip on Delete, so the strip needs no role of its own. */}
      <div
        className="flex min-w-0 flex-nowrap items-center gap-2 [&>*]:shrink-0"
        data-chip-strip=""
        onClickCapture={(event) => {
          const held = chipHolding(event.currentTarget, event.target);
          if (held !== null) handOff.current = { ...held, panelPlace: null };
        }}
        onKeyDown={deleteChip}
      >
        {inLine}
      </div>
      {inPanel.length === 0 ? null : (
        <Chip
          aria-expanded={open}
          aria-haspopup="dialog"
          aria-label={moreName}
          onClick={() => setOpen((was) => !was)}
          ref={moreRef}
          size={size}
        >
          {`+${inPanel.length} more`}
        </Chip>
      )}
      {open ? (
        <AnchoredPanel
          anchorRef={moreRef}
          className={MORE_PANEL_CLASS}
          label={moreName}
          onDismiss={() => setOpen(false)}
          role="dialog"
        >
          {/* The panel's chips, without a box of their own (`contents`), so the panel lays them
              out one under another. */}
          {/* biome-ignore lint/a11y/noStaticElementInteractions: nothing here is made interactive - the handlers only act on a chip of the panel, which is a button or a link of its own: they note which chip a click takes off, and press the focused chip on Delete, so the list needs no role of its own. */}
          <div
            className="contents"
            onClickCapture={(event) => {
              const held = chipHolding(event.currentTarget, event.target);
              if (held === null) return;
              handOff.current = {
                from: held.from,
                index: shown + held.index,
                panelPlace: held.index,
              };
            }}
            onKeyDown={deleteChip}
            ref={panelRef}
          >
            {inPanel}
          </div>
        </AnchoredPanel>
      ) : null}
    </div>
  );

  // The box's own placeholder, drawn with no height and seen by nobody, is what keeps the slot
  // from narrowing past it: its width in the box's font - `text-sm`, and the 16 px `styles.css`
  // gives every field below 1280 px - with the box's own side padding and border, and the room a
  // hint takes at the box's end.
  const searchSlot =
    search === undefined ? null : (
      <div className="flex flex-1 basis-64 flex-col" data-row-slot="">
        {search.placeholder === undefined ? null : (
          <span
            aria-hidden="true"
            className={`invisible h-0 border-x text-sm whitespace-pre max-xl:text-base ${controlSidePadding[size]} ${search.hint === undefined ? "" : SEARCH_HINT_ROOM} ${borderTransparent}`}
          >
            {search.placeholder}
          </span>
        )}
        <SearchBox {...search} className="w-full" size={size} />
      </div>
    );

  // **Filter** and **Clear filters**, one group. The trigger's panel lists the chips at its top,
  // above the page's own fields, each with its remove control.
  let triggerSlot: ReactNode = null;
  if (trigger !== undefined) {
    const tinted = trigger.count !== undefined && trigger.count > 0;
    const clearShown = clear !== undefined && !narrowBar;
    const fields = trigger.children;
    triggerSlot = (
      <div className="flex shrink-0 items-stretch" data-row-slot="">
        <PanelTrigger
          {...trigger}
          appearance={{
            kind: "bar",
            look: { icon: narrowBar ? FUNNEL_GLYPH : undefined, joined: clearShown },
          }}
          size={size}
        >
          {(close) => (
            <>
              {chips === undefined || chips.length === 0 ? null : (
                <PanelChips chips={chips} size={size} />
              )}
              {fields(close)}
            </>
          )}
        </PanelTrigger>
        {clearShown ? (
          // **Clear filters**: a × button beside the trigger, which hands the focus to the
          // trigger once its press has committed.
          <button
            aria-label="Clear filters"
            className={barButton(size, tinted ? "tint" : "quiet", true, "rounded-r-lg")}
            onClick={(event) => {
              handOff.current = { from: event.currentTarget, index: -1, panelPlace: null };
              clear();
            }}
            title="Clear filters"
            type="button"
          >
            <span aria-hidden="true">×</span>
          </button>
        ) : null}
      </div>
    );
  }

  // Everything after the empty space is one slot, for the focus hand-off: the actions of the
  // line, the hint, the columns menu, the display menu, **More** and the main action.
  const end: ReactNode[] = [];
  if (inBar.length > 0) {
    end.push(
      <span className="contents" data-bar-actions="" key="actions">
        {inBar.map((action) => (
          <BarAction {...action} key={action.label} size={size} />
        ))}
      </span>
    );
  }
  if (hint !== undefined) {
    end.push(
      <ViewPanel
        {...hint}
        appearance={{ kind: "bar", look: { icon: INFO_GLYPH } }}
        key="hint"
        size={size}
      />
    );
  }
  if (columns !== undefined) {
    end.push(
      <ColumnsMenu {...columns} appearance={{ kind: "bar", look: {} }} key="columns" size={size} />
    );
  }
  // The display menu, in its own place, or below 640 px as **More**'s first item.
  if (display !== undefined && !displayInMore) {
    end.push(
      <DisplayMenu
        {...display}
        appearance={{ kind: "bar", look: { icon: narrowBar ? SLIDERS_GLYPH : undefined } }}
        key="display"
        size={size}
      />
    );
  }
  if (inMore.length > 0 || displayInMore) {
    end.push(
      <MoreMenu
        actions={inMore}
        display={displayInMore ? display : undefined}
        key="more"
        size={size}
      />
    );
  }
  if (main !== undefined) {
    end.push(<BarAction {...main} filled iconOnly={narrowBar} key="main" size={size} />);
  }

  return (
    // biome-ignore lint/a11y/noStaticElementInteractions: nothing here is made interactive - the focus handler only notes which control of a slot took the focus, for the hand-off when a commit takes that control away (`useFocusPlace`), and the capture handlers only arm that hand-off at a press inside the bar (`armOnPress`), so the bar needs no role of its own.
    <div
      className={`flex min-w-0 flex-nowrap items-center gap-2${className === undefined ? "" : ` ${className}`}`}
      data-control={CONTROL.controlRow}
      {...ARM_ON_PRESS}
      onFocus={onFocus}
      ref={rowRef}
    >
      {searchSlot}
      {segments === undefined ? null : (
        <>
          <Slot>
            <SegmentedControl {...segments} narrow={narrowBar} size={size} />
          </Slot>
          <span
            aria-hidden="true"
            className={`h-6 shrink-0 border-l ${borderDefault}`}
            data-bar-divider=""
          />
        </>
      )}
      {triggerSlot}
      {chipsBox}
      <Slot>{end.length === 0 ? null : end}</Slot>
    </div>
  );
}
