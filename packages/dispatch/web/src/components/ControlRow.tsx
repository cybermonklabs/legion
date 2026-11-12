import type { ReactNode } from "react";

import { scrollFadeTrailing } from "../theme/classes";
import { CONTROL } from "./control-names";
import { useTrailingScroll } from "./useTrailingScroll";

/** One slot of the row: its own single line from `sm` up, which cannot wrap, so whatever a page
 *  puts in it grows sideways and never downward. Every slot but the search box keeps its width;
 *  the search box is the one that gives way where the window is narrow, as it does on every page
 *  today. Below `sm` a slot the page fills itself may wrap inside itself (`wraps`), because a
 *  phone is 390 px wide and a row of the page's own controls - the Artifacts tab's staged
 *  upload, say - runs past that edge otherwise, where the reader cannot reach it. A wrap that is
 *  there before any narrowing adds no height when a narrowing goes on, which is R1's rule. */
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

export interface ControlRowProps {
  /** The chips of the narrowings that are on. They take ONE line, whatever their number: from `sm`
   *  up inside the row's own line, and below it on one line of their own after the rest of the
   *  row. A chip that does not fit is scrolled to, never wrapped onto a further line. */
  readonly chips?: ReactNode;
  /** The page's own controls, such as the Inbox's **Mark all read**. */
  readonly children?: ReactNode;
  /** The caller's outer layout alone - where the row sits on the page, and what space it keeps
   *  from what is above and below it. The order inside it is the row's. */
  readonly className?: string;
  /** The display menu, drawn at the right end of the row. */
  readonly display?: ReactNode;
  /** The page's search box, at the head of the row. */
  readonly search?: ReactNode;
  /** The box that opens the page's filter panel - one `PanelTrigger`, never the panel itself. */
  readonly trigger?: ReactNode;
}

/**
 * The one row a page's controls sit in: its search box, the button that opens its filter panel,
 * the chips of the narrowings that are on, the page's own controls, and its display menu at the
 * right end - in that order, on every page, so a reader who learns one page's controls has
 * learned them all.
 *
 * The row is also where the height rule is kept: applying a narrowing, opening a panel, opening
 * the display menu or selecting rows must each leave the top of the list's first row where it
 * was, and the chips of the narrowings that are on may take one more line at most, 52 px, however
 * many narrowings are on. The whole arithmetic is these points:
 *
 * - from `sm` up the row is one flex line (`flex-nowrap`), so no slot is ever pushed under
 *   another; below `sm` it wraps, and that wrap is there before any narrowing;
 * - every slot but the chips lays its own nodes out in one line from `sm` up, so a slot handed
 *   more to draw grows sideways, never downward;
 * - the chips sit in one box that never wraps (`flex-nowrap`) and scrolls sideways when they do
 *   not fit (`overflow-x-auto`). From `sm` up that box shares the row's line with the search box
 *   (`flex-1 basis-0`), so the chips add no height at all there. Below `sm` it takes a whole line
 *   of its own and is placed after every other slot (`basis-full order-last`), so it adds that one
 *   line - a 44 px control and the row's 8 px gap, 52 px - and nothing else moves;
 * - a chip the box has scrolled away stays reachable: the Tab key reaches it and the browser
 *   scrolls it into view, a pointer or a finger scrolls the box, and every narrowing is also in
 *   the page's filter panel. The scroll bar is not drawn, because on a phone its height would be
 *   more than the one line the rule allows; the box keeps 2 px of room round each chip so a
 *   focused chip's ring is not cut off by the box's edge. While chips are scrolled away to the
 *   right, the box fades at that edge (`scrollFadeTrailing`), so the cut reads as a scroll.
 *
 * It draws its slots and nothing else. The panel a trigger opens is not in it (`AnchoredPanel`
 * portals to the body and is placed `fixed`, so it covers the page rather than taking room), and
 * neither is the selection bar (`SelectionBar` draws over the list for the same reason): either
 * one drawn in the row would move every row of the list beneath it.
 */
export function ControlRow({
  chips,
  children,
  className,
  display,
  search,
  trigger,
}: ControlRowProps): ReactNode {
  const [setChips, chipsScrollable] = useTrailingScroll();
  // One line from `sm` up, which is where R1's two widths are; below it the row wraps, because
  // a search box holding three characters beside a trigger is no search box. A wrap that is
  // there before any narrowing adds no height when a narrowing goes on, which is the rule.
  const own = "flex min-w-0 flex-wrap items-start gap-2 sm:flex-nowrap";
  return (
    <div
      className={className === undefined ? own : `${own} ${className}`}
      data-control={CONTROL.controlRow}
    >
      <Slot gives>{search}</Slot>
      <Slot>{trigger}</Slot>
      {chips === undefined || chips === null ? null : (
        // `[&>*]:shrink-0`: a strip that scrolls never squeezes a chip. A page's own chip that
        // could shrink (the Inbox's agent chip is a link, not a `Chip`) would otherwise be drawn
        // narrower than its text, and the next chip would sit over its end and take its clicks.
        <div
          className={`order-last -mx-0.5 -my-0.5 flex min-w-0 basis-full flex-nowrap items-center gap-2 overflow-x-auto px-0.5 py-0.5 [scrollbar-width:none] sm:order-none sm:flex-1 sm:basis-0 [&::-webkit-scrollbar]:hidden [&>*]:shrink-0 ${chipsScrollable ? scrollFadeTrailing : ""}`}
          ref={setChips}
        >
          {chips}
        </div>
      )}
      <Slot wraps>{children}</Slot>
      <Slot className="ml-auto">{display}</Slot>
    </div>
  );
}
