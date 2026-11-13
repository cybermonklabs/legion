import { type ReactNode, useLayoutEffect, useRef } from "react";

import { focusOnDocument } from "../features/shell/roving";
import {
  chipStripFade,
  controlHoverBorder,
  focusVisibleRing,
  textSecondaryOnCanvas,
} from "../theme/classes";
import { restingChip } from "./Chip";
import { CONTROL, type ControlSize } from "./control-names";
import { useScrollEnds } from "./useScrollEnds";

/** **Clear filters**: the one control of the row that is not a box of its own, since it undoes
 *  the chips rather than narrowing anything. It keeps the row's touch target. */
const CLEAR_FILTERS_CLASS = `min-h-11 shrink-0 rounded-lg px-2 text-xs font-medium whitespace-nowrap outline-none focus-visible:ring-2 ${focusVisibleRing} ${textSecondaryOnCanvas}`;

/** A scroll button of the chips line, laid over one end of the strip: a round 44 px control in
 *  a resting chip's own fill, border and text (`restingChip`), so it reads the same on whatever
 *  surface the row is drawn on, centred in the line and above the strip, whose fade makes it a
 *  layer of its own. The strip fades under it, so it sits over faded chips. */
const SCROLL_BUTTON_CLASS = `absolute inset-y-0 z-10 my-auto flex h-11 w-11 items-center justify-center rounded-full border text-lg ${restingChip} ${controlHoverBorder}`;

/** The row's own controls at each size. The compact size gives the touch target up only from
 *  `md`, where a mouse is: Clear filters and the scroll buttons are then as tall as that line's
 *  32 px chips. */
const ROW_CONTROL: Record<ControlSize, { clear: string; scroll: string }> = {
  comfortable: { clear: CLEAR_FILTERS_CLASS, scroll: SCROLL_BUTTON_CLASS },
  compact: {
    clear: `${CLEAR_FILTERS_CLASS} md:min-h-8`,
    scroll: `${SCROLL_BUTTON_CLASS} md:h-8 md:w-8`,
  },
};

/** Each end of the strip a scroll button is laid over: its place, its name and its glyph, the
 *  Pager's `‹` and `›`. */
const SCROLL_SIDE = {
  left: { glyph: "‹", label: "Scroll filters left", place: "left-0" },
  right: { glyph: "›", label: "Scroll filters right", place: "right-0" },
} as const;

type ScrollSide = keyof typeof SCROLL_SIDE;

/** One slot of the row's first line: its own single line from `sm` up, which cannot wrap, so
 *  whatever a page puts in it grows sideways and never downward. Every slot but the search box
 *  keeps its width; the search box is the one that gives way where the window is narrow, as it
 *  does on every page today. Below `sm` a slot the page fills itself may wrap inside itself
 *  (`wraps`), because a phone is 390 px wide and a row of the page's own controls - the
 *  Artifacts tab's staged upload, say - runs past that edge otherwise, where the reader cannot
 *  reach it. A wrap that is there before any narrowing adds no height when a narrowing goes on,
 *  which is R1's rule. */
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
  const width = gives ? "min-w-0 flex-1 basis-64" : wraps ? "min-w-0 sm:shrink-0" : "shrink-0";
  const wrap = wraps ? "flex-wrap sm:flex-nowrap" : "flex-nowrap";
  const own = `flex items-center gap-2 ${wrap} ${width}`;
  return <div className={className === undefined ? own : `${own} ${className}`}>{children}</div>;
}

/**
 * Scrolls the chips strip one page towards `side`, never past a chip: the first chip on that
 * side that is not wholly in the part of the strip the scroll buttons leave uncovered (its
 * `scroll-padding`) is brought to the far end of that part, so each press shows at least that
 * chip whole and no chip is stepped over. A chip wider than that whole part is brought in by its
 * far end instead, so the next press moves on to the chip after it.
 */
function scrollPage(strip: HTMLElement, side: ScrollSide): void {
  const style = getComputedStyle(strip);
  const box = strip.getBoundingClientRect();
  const from = box.left + (Number.parseFloat(style.scrollPaddingLeft) || 0);
  const to = box.right - (Number.parseFloat(style.scrollPaddingRight) || 0);
  const chips = Array.from(strip.children, (chip) => chip.getBoundingClientRect());
  let by: number;
  if (side === "right") {
    const next = chips.find((chip) => chip.right > to + 1);
    if (next === undefined) by = strip.scrollWidth;
    else if (next.left - from > 1) by = next.left - from;
    else by = next.right - to;
  } else {
    const previous = chips.filter((chip) => chip.left < from - 1).at(-1);
    if (previous === undefined) by = -strip.scrollWidth;
    else if (previous.right - to < -1) by = previous.right - to;
    else by = previous.left - from;
  }
  strip.scrollBy({ behavior: "smooth", left: by });
}

/** One scroll button of the chips line, at its `side` of the strip. It takes no Tab stop: the
 *  Tab key reaches every chip already and scrolls it into view, and this is the pointer's way. */
function ScrollButton({
  className,
  side,
  strip,
}: {
  className: string;
  side: ScrollSide;
  strip: HTMLElement | null;
}): ReactNode {
  const { glyph, label, place } = SCROLL_SIDE[side];
  return (
    <button
      aria-label={label}
      className={`${place} ${className}`}
      onClick={() => {
        if (strip !== null) scrollPage(strip, side);
      }}
      tabIndex={-1}
      type="button"
    >
      <span aria-hidden="true">{glyph}</span>
    </button>
  );
}

/** A removal a click has started, which hands the keyboard focus on once it has committed:
 *  `from` is what was clicked - a strip's child, or **Clear filters** - and `index` is that
 *  child's place among the strip's children, or -1 for **Clear filters**, whose focus goes to
 *  the row's filter trigger. */
interface HandOff {
  readonly from: Element;
  readonly index: number;
}

/** What takes the focus in a chip: a chip is a button or a link itself, and a page's own chip
 *  may be a box holding one (the Inbox's agent chip holds its links). */
const FOCUSABLE = "a[href], button";

/** The hand-off a click at `target` starts: the strip's child that holds it - a chip, or the box
 *  a page drew its own chip in - and that child's place among the strip's children; null when
 *  `target` is the strip itself. */
function chipHolding(strip: Element, target: EventTarget): HandOff | null {
  if (!(target instanceof Node)) return null;
  const children = Array.from(strip.children);
  const index = children.findIndex((child) => child.contains(target));
  const from = children[index];
  return from === undefined ? null : { from, index };
}

/** The element of a chip the focus goes to: the chip itself, or the first link or button in the
 *  box a page drew its own chip in. */
function focusableOf(chip: Element): HTMLElement | null {
  if (chip instanceof HTMLElement && chip.matches(FOCUSABLE)) return chip;
  return chip.querySelector<HTMLElement>(FOCUSABLE);
}

export interface ControlRowProps {
  /** The chips of the narrowings that are on. While there is any, they take ONE line of their
   *  own under the row's first line, whatever their number - at most 52 px, a 44 px control and
   *  the row's 8 px gap - and a chip that does not fit is scrolled to, never wrapped onto a
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
   *  chip, and right after the trigger while there is none (the project List's column filters
   *  and search text draw no chip in the row). */
  readonly onClear?: () => void;
  /** The page's search box, at the head of the row. */
  readonly search?: ReactNode;
  /** How much room the row's own controls take (`ControlSize`): **Clear filters** and the two
   *  scroll buttons. The page's controls carry their own size. */
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
 * - from `sm` up the first line is one flex line (`flex-nowrap`), so no slot is ever pushed
 *   under another; below `sm` it wraps, and that wrap is there before any narrowing, since no
 *   chip is in that line;
 * - every slot of the first line lays its own nodes out in one line from `sm` up, so a slot
 *   handed more to draw grows sideways, never downward;
 * - the chips line is drawn only while there is a chip, and it never wraps: its tallest item is
 *   one control, 44 px (32 px from `md` at the compact size), so it adds that line and the
 *   row's 8 px gap - 52 px - and nothing else moves. The chips sit in one strip that scrolls
 *   sideways when they do not fit (`overflow-x-auto`), and **Clear filters** sits after the
 *   strip, outside it, so no number of chips scrolls it out of view.
 *
 * A chip the strip has scrolled away stays reachable, and is shown whole when it is reached:
 *
 * - the Tab key reaches it, and the strip scrolls a chip that takes the focus wholly into view,
 *   clear of the fade and of the scroll button at either end (`scroll-padding`), so a reader
 *   always sees which narrowing Enter takes off;
 * - a pointer has a scroll button at each end of the strip, drawn only while a chip is out of
 *   view on that side, which pages the strip one screen of chips that way. A mouse's plain
 *   wheel scrolls the page, not the strip (Shift and the wheel, or a sideways wheel or swipe,
 *   do scroll it), so the buttons are a pointer's way to every chip. They are laid over the
 *   strip's two ends rather than beside it, so one appearing never narrows the strip and moves
 *   the chips under the pointer, and they take no Tab stop, since Tab reaches every chip
 *   already;
 * - every narrowing is also in the page's filter panel.
 *
 * The scroll bar is not drawn, because on a phone its height would be more than the one line the
 * rule allows; the strip keeps 2 px of room round each chip so a focused chip's ring is not cut
 * off by its edge. While chips are out of view beyond either end, the strip fades over 48 px at
 * that end (`chipStripFade`), so the cut reads as a scroll and the round button laid over that
 * end sits over faded chips rather than over the opaque end of one.
 *
 * A chip or **Clear filters** that is taken off hands the keyboard focus on rather than drop it
 * onto the page, where a keyboard reader would start again from the page's first link for each
 * chip. A chip taken off - by Enter, Space or Delete, or by a pointer, under the same rule - hands
 * it to the chip now at its place, else to the chip before it, else to the row's filter trigger
 * (`data-control="panel-trigger"`, or the search box in a row with no trigger), and **Clear
 * filters** hands it to the trigger. Delete presses a chip's own control alone - the strip's child
 * itself, or a `Chip` - and never another link a page drew inside its chip, such as the Inbox
 * agent chip's link to the live view. A chip that takes the focus is scrolled whole into view
 * as any focused chip is, and the trigger is in the first line. The hand-off waits for the removal
 * to commit, whatever holds the focus meanwhile (a pointer's click in Safari focuses no button,
 * so the focus stays on the body), and then moves the focus only if it is on the page's body, so
 * a reader who moved it elsewhere while the chip was going keeps it there.
 *
 * Beside its slots it draws **Clear filters** and the two scroll buttons, and nothing else. The
 * panel a trigger opens is not in it (`AnchoredPanel` portals to the body and is placed `fixed`,
 * so it covers the page rather than taking room), and neither is the selection bar
 * (`SelectionBar` draws over the list for the same reason): either one drawn in the row would
 * move every row of the list beneath it.
 */
export function ControlRow({
  chips,
  children,
  className,
  display,
  onClear,
  search,
  size = "comfortable",
  trigger,
}: ControlRowProps): ReactNode {
  const { leading, node, ref, trailing } = useScrollEnds();
  const rowRef = useRef<HTMLDivElement>(null);
  const handOff = useRef<HandOff | null>(null);
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
    // The strip as it is drawn now, read through the row: the node `useScrollEnds` holds is the
    // strip last drawn, which taking the last chip off has unmounted.
    const remaining = Array.from(root.querySelector("[data-chip-strip]")?.children ?? []);
    const chip =
      pending.index === -1 ? undefined : (remaining[pending.index] ?? remaining[pending.index - 1]);
    const next =
      chip === undefined
        ? (root.querySelector<HTMLElement>(`[data-control="${CONTROL.panelTrigger}"]`) ??
          root.querySelector<HTMLElement>(`[data-control="${CONTROL.searchBox}"]`))
        : focusableOf(chip);
    next?.focus();
  });
  const controls = ROW_CONTROL[size];
  const chipsOn = chips !== undefined && chips !== null;
  const clear =
    onClear === undefined ? null : (
      <button
        className={controls.clear}
        onClick={(event) => {
          handOff.current = { from: event.currentTarget, index: -1 };
          onClear();
        }}
        type="button"
      >
        Clear filters
      </button>
    );
  let fade = "";
  if (leading && trailing) fade = chipStripFade.both;
  else if (leading) fade = chipStripFade.leading;
  else if (trailing) fade = chipStripFade.trailing;
  const own = "flex min-w-0 flex-col gap-2";
  return (
    <div
      className={className === undefined ? own : `${own} ${className}`}
      data-control={CONTROL.controlRow}
      ref={rowRef}
    >
      {/* One line from `sm` up, which is where R1's two widths are; below it the line wraps,
          because a search box holding three characters beside a trigger is no search box. */}
      <div className="flex min-w-0 flex-wrap items-start gap-2 sm:flex-nowrap">
        <Slot gives>{search}</Slot>
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
          <div className="relative flex min-w-0 flex-1">
            {leading ? <ScrollButton className={controls.scroll} side="left" strip={node} /> : null}
            {/* `[&>*]:shrink-0`: a strip that scrolls never squeezes a chip. A page's own chip
                that could shrink (the Inbox's agent chip is a link, not a `Chip`) would
                otherwise be drawn narrower than its text, and the next chip would sit over its
                end and take its clicks. `scroll-px-12` keeps a chip brought into view 48 px
                from either end: past the 44 px button laid over that end, and the 48 px fade
                under it. */}
            {/* biome-ignore lint/a11y/noStaticElementInteractions: nothing here is made interactive - the handlers only act on a chip of the strip, which is a button or a link of its own: they scroll to where the focus has already gone, note which chip a click takes off, and press the focused chip on Delete, so the strip needs no role of its own. */}
            <div
              className={`-mx-0.5 -my-0.5 flex min-w-0 flex-1 scroll-px-12 flex-nowrap items-center gap-2 overflow-x-auto px-0.5 py-0.5 [scrollbar-width:none] [&::-webkit-scrollbar]:hidden [&>*]:shrink-0 ${fade}`}
              data-chip-strip=""
              onClickCapture={(event) => {
                const held = chipHolding(event.currentTarget, event.target);
                if (held !== null) handOff.current = held;
              }}
              onFocus={(event) =>
                event.target.scrollIntoView({ block: "nearest", inline: "nearest" })
              }
              // Delete takes a focused chip off as Enter and Space do, which a button does by
              // itself.
              onKeyDown={(event) => {
                const { currentTarget: strip, key, target } = event;
                if (key !== "Delete" || !(target instanceof HTMLElement)) return;
                const own =
                  (target.parentElement === strip && target.matches(FOCUSABLE)) ||
                  target.getAttribute("data-control") === CONTROL.chip;
                if (!own) return;
                event.preventDefault();
                target.click();
              }}
              ref={ref}
            >
              {chips}
            </div>
            {trailing ? (
              <ScrollButton className={controls.scroll} side="right" strip={node} />
            ) : null}
          </div>
          {clear}
        </div>
      ) : null}
    </div>
  );
}
