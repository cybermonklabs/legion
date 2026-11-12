import type { ReactNode } from "react";

import { CONTROL } from "./control-names";

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
  /** The chips of the narrowings that are on: the one part of the row that may take a line of its
   *  own, so the one part a page may let grow. */
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
 * was. Nothing here but the chips can add a line, and the whole arithmetic is these four points:
 *
 * - the row is one flex line (`flex-nowrap`), so no slot is ever pushed under another;
 * - every slot but the chips is `shrink-0` and lays its own nodes out in one line of its own, so
 *   a slot handed more to draw grows sideways, never downward;
 * - the chips sit in the one box that wraps and the one box that may shrink, so the chips are the
 *   one thing in the row whose height can change;
 * - a control is 44 px tall (`controlPadding`) and every gap here is `gap-2`, 8 px, which is the
 *   chips box's row gap as well: a second line of chips costs 44 + 8 = 52 px, the one exception
 *   the rule allows, and a third would cost 52 px more. Holding the chips to that one extra line
 *   is the page's part - they are what the reader turned on, and the row cannot shorten them.
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
        <div className="flex min-w-0 flex-wrap items-center gap-2">{chips}</div>
      )}
      <Slot wraps>{children}</Slot>
      <Slot className="ml-auto">{display}</Slot>
    </div>
  );
}
