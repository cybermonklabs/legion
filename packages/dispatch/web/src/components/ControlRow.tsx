import {
  Children,
  cloneElement,
  Fragment,
  isValidElement,
  type KeyboardEvent,
  type ReactElement,
  type ReactNode,
  useLayoutEffect,
  useRef,
  useState,
} from "react";

import { focusOnDocument } from "../features/shell/roving";
import { observeStrip } from "../hooks/observe-strip";
import { card, focusVisibleRing, textSecondaryOnCanvas } from "../theme/classes";
import { AnchoredPanel } from "./AnchoredPanel";
import { Chip } from "./Chip";
import { CONTROL, type ControlSize, controlIn } from "./control-names";
import { ARM_ON_PRESS, landWhole, useFocusPlace } from "./focus-place";
import { PANEL_FRAME } from "./PanelTrigger";

/** **Clear filters**: the one control of the row that is not a box of its own, since it undoes
 *  the chips rather than narrowing anything. It keeps the row's touch target. */
const CLEAR_FILTERS_CLASS = `min-h-11 shrink-0 rounded-lg px-2 text-xs font-medium whitespace-nowrap outline-none focus-visible:ring-2 ${focusVisibleRing} ${textSecondaryOnCanvas}`;

/** **Clear filters** at each size. The compact size gives the touch target up only from `md`,
 *  where a mouse is: it is then as tall as that line's 32 px chips. The toolbar size stands at
 *  the toolbar's one height from 1280 px up, as its controls do (`controlPadding`). */
const CLEAR_FILTERS: Record<ControlSize, string> = {
  comfortable: CLEAR_FILTERS_CLASS,
  compact: `${CLEAR_FILTERS_CLASS} md:min-h-8`,
  toolbar: `${CLEAR_FILTERS_CLASS} xl:h-8.5 xl:min-h-0`,
};

/** The panel `+N more` opens: a card over the page, as a filter panel is (`PANEL_FRAME`), that
 *  lists the chips the line has no room for one under another, each at its own width. */
const MORE_PANEL_CLASS = `${PANEL_FRAME} items-start gap-2 p-2 ${card}`;

/** How far past its room a line of chips may reach and still fit. The widths read are rounded to
 *  the layout's own units, so their sum can miss the laid-out line by a fraction of a pixel. */
const SLACK_PX = 0.5;

/** One slot of the row's first line: its own single line in a row 40rem wide or more (the row's
 *  own width, not the window's), which cannot wrap, so whatever a page puts in it grows sideways
 *  and never downward. Every slot but the search box keeps its width; the search box is the one
 *  that gives way where the row is narrow, as it does on every page today. In a narrower row a
 *  slot the page fills itself may wrap inside itself (`wraps`), because a phone is 390 px wide
 *  and a row of the page's own controls - the Artifacts tab's staged upload, say - runs past that
 *  edge otherwise, where the reader cannot reach it. A wrap that is there before any narrowing
 *  adds no height when a narrowing goes on, which is R1's rule. `data-row-slot` marks it as the
 *  container a focused control in it is counted in, for the row's focus hand-off. */
function Slot({
  children,
  className,
  gives = false,
  wraps = false,
}: {
  children: ReactNode;
  className?: string;
  gives?: boolean;
  wraps?: boolean;
}): ReactNode {
  if (children === undefined || children === null) return null;
  const width = gives
    ? "min-w-0 flex-1 basis-64"
    : wraps
      ? "min-w-0 @min-[40rem]:shrink-0"
      : "shrink-0";
  const wrap = wraps ? "flex-wrap @min-[40rem]:flex-nowrap" : "flex-nowrap";
  const own = `flex items-center gap-2 ${wrap} ${width}`;
  return (
    <div className={className === undefined ? own : `${own} ${className}`} data-row-slot="">
      {children}
    </div>
  );
}

/** The chips a page handed the row, one element each, in its order: a fragment's chips are its
 *  children, so a page may hand an array, a fragment, or both (the Inbox's agent chip before the
 *  chips `filterChips` draws). Each keeps a key of its own, made from its fragment's, so a chip
 *  the line hands to the panel stays the same chip. Anything that is not an element draws
 *  nothing: a chip is a control the row measures and hands the focus to. */
function chipsOf(chips: ReactNode, within = ""): ReactElement[] {
  return Children.toArray(chips).flatMap((chip) => {
    if (!isValidElement<{ children?: ReactNode }>(chip)) return [];
    const key = `${within}${String(chip.key)}`;
    if (chip.type === Fragment) return chipsOf(chip.props.children, `${key}/`);
    return [within === "" ? chip : cloneElement(chip, { key })];
  });
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
 * fit its room, else as many as fit with `+N more` and its gap after them. The first is drawn
 * whatever its width, cut short as `Chip` cuts its words, so the line never shows `+N more`
 * alone. A chip with no width yet is drawn, so the next read measures it (`probe`), save while
 * the panel is open: a chip the panel holds stays there.
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
    if (width === undefined) return probe ? count + 1 : Math.max(count, 1);
    end += gap + width;
    if (end > limit + SLACK_PX) break;
    count += 1;
  }
  return Math.max(count, 1);
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

/** What takes the focus in a chip: a chip is a button or a link itself, and a page's own chip
 *  may sit in a box with other controls (the Inbox's agent chip, with the agent's live link
 *  after it). */
const FOCUSABLE = "a[href], button";

/** The chip a click at `target` landed in: the child of `chips` - the line's strip, or the
 *  panel's list - that holds it, a chip or the box a page drew its own chip in, and that child's
 *  place among them; null when `target` is `chips` itself. */
function chipHolding(chips: Element, target: EventTarget): { from: Element; index: number } | null {
  if (!(target instanceof Node)) return null;
  const children = Array.from(chips.children);
  const index = children.findIndex((child) => child.contains(target));
  const from = children[index];
  return from === undefined ? null : { from, index };
}

/** Delete takes a focused chip off as Enter and Space do, which a button does by itself. It
 *  presses a chip's own control alone - a child of the strip or the panel's list itself, or a
 *  `Chip` - and never another control a page drew in its chip's box, such as the agent's live
 *  link beside the Inbox's agent chip. */
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
 *  box a page drew its own chip in. */
function focusableOf(chip: Element): HTMLElement | null {
  if (chip instanceof HTMLElement && chip.matches(FOCUSABLE)) return chip;
  return chip.querySelector<HTMLElement>(FOCUSABLE);
}

/** Where the focus goes once nothing of its own place is left: the row's filter trigger, or the
 *  search box in a row with no trigger. */
function triggerOf(root: Element): HTMLElement | null {
  return controlIn(root, CONTROL.panelTrigger) ?? controlIn(root, CONTROL.searchBox);
}

export interface ControlRowProps {
  /** The chips of the narrowings that are on, each an element; a fragment's chips are its
   *  children. While there is any, they take ONE line of their own under the row's first line,
   *  whatever their number - at most 52 px, a 44 px control and the row's 8 px gap. A chip that
   *  does not fit that line is listed in the panel its `+N more` opens, never wrapped onto a
   *  further line. With none the row is its first line alone. */
  readonly chips?: ReactNode;
  /** The page's own controls, such as the Inbox's **Mark all read**. */
  readonly children?: ReactNode;
  /** The caller's outer layout alone - where the row sits on the page, and what space it keeps
   *  from what is above and below it. The order inside it is the row's. */
  readonly className?: string;
  /** The display menu, drawn at the right end of the row's first line. */
  readonly display?: ReactNode;
  /** What **Clear filters** does: takes every narrowing the page counts off at once. The row
   *  draws the button only while this is given - at the end of the chips line while there is a
   *  chip, and right after the trigger while there is none (the project List's search text and
   *  the open statuses it opens on draw no chip in the row). */
  readonly onClear?: () => void;
  /** The page's search box, at the head of the row. */
  readonly search?: ReactNode;
  /** Whether the search box takes a line of its own, above the row's other controls, in a row
   *  narrower than 61rem (976 px) by the row's own width, rather than give way beside them: the
   *  project List's, whose placeholder needs about 406 px and the controls after it about 557 px
   *  (dispatch://INFRA-2947, R15). Unset, the first line is one line from 40rem up. */
  readonly searchLine?: boolean;
  /** How much room the row's own controls take (`ControlSize`): **Clear filters** and `+N more`.
   *  The page's controls carry their own size. */
  readonly size?: ControlSize;
  /** The box that opens the page's filter panel - one `PanelTrigger`, never the panel itself. */
  readonly trigger?: ReactNode;
}

/**
 * The one row a page's controls sit in. Its first line holds the search box, the button that
 * opens the filter panel, the page's own controls and the display menu at the right end - in
 * that order, on every page, so a reader who learns one page's controls has learned them all.
 * While a narrowing is on, a second line under it holds the chips of the narrowings that are on
 * and **Clear filters**.
 *
 * The row is also where the height rule is kept: applying a narrowing, opening a panel, opening
 * the display menu or selecting rows must each leave the top of the list's first row where it
 * was, and the chips of the narrowings that are on may take one more line at most, 52 px, however
 * many narrowings are on. The whole arithmetic is these points:
 *
 * - the row is a column of its lines, 8 px apart (`gap-2`);
 * - in a row 40rem wide or more (Tailwind's `sm`, 640 px, read off the row's own box by a
 *   container query rather than off the window) the first line is one flex line
 *   (`flex-nowrap`), so no slot is ever pushed under another; in a narrower row it wraps, and
 *   that wrap is there before any narrowing, since no chip is in that line;
 * - every slot of the first line lays its own nodes out in one line in a row that wide, so a
 *   slot handed more to draw grows sideways, never downward;
 * - the chips line is drawn only while there is a chip, and it never wraps: its tallest item is
 *   one control, 44 px (32 px from `md` at the compact size), so it adds that line and the
 *   row's 8 px gap - 52 px - and nothing else moves. The line draws the chips that fit it, from
 *   the first, and a chip that does not fit is not drawn in it: one `+N more` chip at the end of
 *   the chips stands for every chip left out. **Clear filters** sits after them, outside the box
 *   the chips and `+N more` share, so no number of chips pushes it out of view.
 *
 * The row reads its own width, not the window's: it is a container (`@container`), and its first
 * line and the slots in it switch at 40rem of the row's own box (`@min-[40rem]:`). The Inbox
 * drawer's row is 415 px wide in a 1440 px window, and a first line kept to one line by the
 * window's width ran 34 px past the drawer's edge. A container's width never comes from what it
 * holds, so a page puts the row where its box takes its width from around it - a block, or a
 * stretched flex item - as every page does.
 *
 * Nothing in the row scrolls sideways, at any width. What fits is measured, not guessed: the box
 * the chips and `+N more` share, less `+N more`'s own width and its gap, against each chip's own
 * width and the gaps between them. Where every chip fits the whole box, no `+N more` is drawn.
 * The first chip is drawn whatever its width, cut short as `Chip` cuts its words, so the line
 * never shows `+N more` alone. A chip's width is read when the line draws it and kept while the
 * panel holds it, so the line is drawn right at once when a chip is taken off: a chip after it
 * that now fits moves into the line, and `+N` counts down, or goes once every chip fits. The line
 * is read again after a commit that changed what it draws, and whenever its box, the strip or
 * `+N more` changes size (`observeStrip`): a window resized, a chip's name arriving.
 *
 * `+N more` is named for what it opens (`2 more filters`), and opens one panel (`AnchoredPanel`)
 * that lists the chips the line left out, as the same elements the page handed the row, each
 * with its own remove action. Each chip is drawn once, in the line or in the panel, so no id is
 * drawn twice. The panel's focus follows every panel's rules: it opens with the focus on its
 * first chip, and Escape, or Tab or Shift+Tab past its last or first chip, close it and hand the
 * focus back to `+N more`.
 *
 * A chip or **Clear filters** that is taken off hands the keyboard focus on rather than drop it
 * onto the page, where a keyboard reader would start again from the page's first link for each
 * chip. A chip taken off - by Enter, Space or Delete, or by a pointer, under the same rule - hands
 * it to the chip now at its place, else to the chip before it, else to the row's filter trigger
 * (`data-control="panel-trigger"`, or the search box in a row with no trigger), and **Clear
 * filters** hands it to the trigger. A chip's place is among all the row's chips, the line's
 * first and then the panel's, so a chip that moved into the line in its place takes it. A chip
 * taken off in the panel while the panel stays hands it to the panel's chip now at its place, else
 * to the one before it, since the panel is where the reader is. The panel's last chip taken off
 * closes the panel, and the focus goes by the line's rule: to the chip now at its place, else to
 * the last chip of the line. Delete presses a chip's own control alone - the child of the strip
 * or of the panel's list itself, or a `Chip` - and never another control a page drew in its
 * chip's box, such as the agent's live link beside the Inbox's agent chip. The hand-off waits for
 * the removal to commit, whatever holds the focus meanwhile (a pointer's click in Safari focuses
 * no button, so the focus stays on the body), and then moves the focus only if it is on the
 * page's body, so a reader who moved it elsewhere while the chip was going keeps it there. It
 * lands the control through `landWhole`: with no scroll of the engine's own, and shown whole at
 * its nearest edge whether the reader can see a part of it or none, at once and again once the
 * commit is done, since the Inbox list's anchor puts this row back where the commit found it
 * after the hand-off has run (`ViewportAnchor`'s `keepPressedRow`).
 *
 * Any other focused control the reader's own press takes out of a slot of the first line while
 * the row stays - a page's own control that removes itself, as the Inbox's **Mark all read** does
 * once nothing is left unread - hands the focus on too: to the control now at its place in that
 * slot, else to the nearest one before it, passing over every control that takes typed text,
 * else to the trigger, each only while the reader can see it, else to the own stop of the region
 * the row is in - the search bar's dialog or the Inbox drawer, else the main content - with no
 * scroll. With nothing left before it in the slot and no trigger, the focus walks back from the
 * slot's place in that region instead, since the search box takes typed text (`useFocusPlace`).
 * **Clear filters**' own rule, the trigger, comes first wherever it sits. A control of a dialog, a
 * panel or a selection bar a page draws in a slot - the Delivery page's Session groups dialog - is
 * that region's and none of the row's, so the row hands its focus nowhere, and never to a control
 * of the row outside that region. In a selection bar, the bar's own hand-off (`SelectionBar`)
 * answers a loss the reader caused.
 *
 * Beside its slots it draws **Clear filters**, `+N more` and the panel `+N more` opens, and
 * nothing else. Neither that panel nor the one a trigger opens takes room in the row
 * (`AnchoredPanel` portals to the body and is placed `fixed`, so it covers the page), and
 * neither does the selection bar (`SelectionBar` draws over the list for the same reason): any
 * of them drawn in the row would move every row of the list beneath it.
 */
export function ControlRow({
  chips,
  children,
  className,
  display,
  onClear,
  search,
  searchLine = false,
  size = "comfortable",
  trigger,
}: ControlRowProps): ReactNode {
  const rowRef = useRef<HTMLDivElement>(null);
  const handOff = useRef<HandOff | null>(null);
  // The box the chips and `+N more` share: state rather than a ref, so its observers start when
  // the chips line is drawn and stop when it goes.
  const [box, setBox] = useState<HTMLDivElement | null>(null);
  const moreRef = useRef<HTMLButtonElement>(null);
  const panelRef = useRef<HTMLDivElement>(null);
  const [layout, setLayout] = useState<LineLayout | null>(null);
  const [open, setOpen] = useState(false);
  const all = chipsOf(chips);
  const keys = all.map((chip) => String(chip.key));
  const shown = layout === null ? all.length : chipsThatFit(keys, layout, !open);
  const inLine = all.slice(0, shown);
  const inPanel = all.slice(shown);
  // `+N more` goes once every chip fits the line, and its panel with it: the next `+N more` opens
  // a panel afresh rather than one left open. The row draws again at once with the panel shut,
  // before anything is committed, so no drawn row holds the panel open with no chip in it.
  if (open && inPanel.length === 0) setOpen(false);
  const moreName = `${inPanel.length} more ${inPanel.length === 1 ? "filter" : "filters"}`;

  // Reads the line again, against what this commit drew. A read lays the page out, so a commit
  // reads it only when what the line draws, or what `+N more` counts, has changed; a change of
  // size the line's observers report reads it whatever it draws.
  const measure = useRef(() => {});
  const measuredFor = useRef("");
  useLayoutEffect(() => {
    const drawn = keys.slice(0, shown);
    measure.current = () => {
      if (box === null) return;
      const reading = readLine(box, moreRef.current, drawn);
      setLayout((before) => merged(before, reading, keys));
    };
    const drawing = `${inPanel.length}\n${drawn.join("\n")}`;
    if (box === null) {
      // A line drawn afresh is read afresh, whatever it draws.
      measuredFor.current = "";
      return;
    }
    if (drawing === measuredFor.current) return;
    measuredFor.current = drawing;
    measure.current();
  });
  useLayoutEffect(() => {
    if (box === null) return;
    return observeStrip(box, () => measure.current(), { childList: true });
  }, [box]);
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
  // A focused control a commit takes out of a slot of the first line hands the focus on too. Its
  // check runs after the chips' hand-off above, so that one's rule for Clear filters wins.
  const noteFocus = useFocusPlace(
    (element) => {
      const slot = element.closest("[data-row-slot]");
      return slot !== null && rowRef.current?.contains(slot) === true ? slot : null;
    },
    () => (rowRef.current === null ? null : triggerOf(rowRef.current))
  );
  const chipsOn = all.length > 0;
  const clear =
    onClear === undefined ? null : (
      <button
        className={CLEAR_FILTERS[size]}
        onClick={(event) => {
          handOff.current = { from: event.currentTarget, index: -1, panelPlace: null };
          onClear();
        }}
        type="button"
      >
        Clear filters
      </button>
    );
  const own = "@container flex min-w-0 flex-col gap-2";
  return (
    // biome-ignore lint/a11y/noStaticElementInteractions: nothing here is made interactive - the focus handler only notes which control of a slot took the focus, for the hand-off when a commit takes that control away (`useFocusPlace`), and the capture handlers only arm that hand-off at a press inside the row (`armOnPress`), so the row needs no role of its own.
    <div
      className={className === undefined ? own : `${own} ${className}`}
      data-control={CONTROL.controlRow}
      {...ARM_ON_PRESS}
      onFocus={noteFocus}
      ref={rowRef}
    >
      {/* One line in a row 40rem wide or more, by the row's own width; in a narrower row - a
          phone's, or the Inbox drawer's at any window - the line wraps, because a search box
          holding three characters beside a trigger is no search box. A row whose search box
          takes a line of its own (`searchLine`) wraps below 61rem, the search box above. */}
      <div
        className={`flex min-w-0 flex-wrap items-start gap-2 ${searchLine ? "@min-[61rem]:flex-nowrap" : "@min-[40rem]:flex-nowrap"}`}
      >
        <Slot className={searchLine ? "@max-[61rem]:basis-full" : undefined} gives>
          {search}
        </Slot>
        <Slot>
          {chipsOn || clear === null ? (
            trigger
          ) : (
            <>
              {trigger}
              {clear}
            </>
          )}
        </Slot>
        <Slot wraps>{children}</Slot>
        <Slot className="ml-auto">{display}</Slot>
      </div>
      {chipsOn ? (
        <div className="flex min-w-0 flex-nowrap items-center gap-2">
          <div className="flex min-w-0 flex-1 flex-nowrap items-center gap-2" ref={setBox}>
            {/* `[&>*]:shrink-0`: the line never squeezes a chip it draws. A box a page draws its
                chip in (the Inbox's agent chip and the agent's live link beside it) could
                otherwise shrink narrower than its text, and the next chip would sit over its end
                and take its clicks. The strip itself gives way, so a first `Chip` wider than the
                line is cut short by its own `max-w-full`. */}
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
                {/* The panel's chips, without a box of their own (`contents`), so the panel lays
                    them out one under another. */}
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
          {clear}
        </div>
      ) : null}
    </div>
  );
}
