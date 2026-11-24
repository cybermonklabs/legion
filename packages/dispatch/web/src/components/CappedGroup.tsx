import { Children, isValidElement, type ReactNode, useId, useState } from "react";

import {
  focusVisibleRing,
  surfaceMutedStrongBg,
  textSecondaryHoverToPrimary,
} from "../theme/classes";

/** The most entries a group whose length comes from data shows before its "+N" control, the
 *  layout rule's cap (`packages/dispatch/web/src/AGENTS.md`, Layout; INFRA-1196's R13). */
export const LAYOUT_GROUP_MAX = 10;

/** Who keeps whether the group is open: its host, which passes both props, or the group itself,
 *  given neither. A host that passed only `open` would draw a "+N" that nothing could open. */
export type CappedGroupOpenState =
  | { open: boolean; onOpenChange: (open: boolean) => void }
  | { open?: never; onOpenChange?: never };

/**
 * A group whose length comes from data - labels, components, links, checks, subscribers,
 * options - drawn as the layout rule asks: at most `max` entries (10 unless given), then one
 * button reading "+N" that shows the rest in place in one click. Open, the same button reads
 * "Show fewer" and folds them back, so the reader decides which way the group is drawn. The
 * button stays where it was in the DOM while the rest are inserted before it, so it keeps the
 * focus across the press.
 *
 * Each direct child of the group element is one entry, except the one "+N" control, which is
 * marked `data-layout-more`: that is what `e2e/layout-measure.ts` counts against the cap. So
 * each child must be one chip. A child that draws several chips - a Fragment, or a component
 * that renders a few - is still one entry, and the group can show more chips than its cap and
 * pass. The group element carries `data-layout-group={name}`, and `data-layout-max` whenever
 * its cap is not 10, so the measure sees a cap above the rule's too; while the reader has it open
 * it carries `data-layout-open` too, since then it shows more than its cap by the reader's own
 * choice.
 *
 * `label` is the group in words, a plural noun ("labels"), which ends the button's name. Closed,
 * the name opens with the visible "+N", so a voice reader who says what the button reads presses
 * it (WCAG 2.5.3), then counts the whole group, which is at least two: "+3, show all 13 labels".
 * Open, it reads "Show fewer labels". `className` lays the group out; `contents`
 * lets its entries flow in the row around it, beside a control the host draws after them.
 *
 * Given `list`, the group is a list named by it: a `ul` with each entry in an `li`, and the
 * control in an `li` of its own, which then carries `data-layout-more`, since the measure skips
 * only a direct child that carries it.
 *
 * Given `open` and `onOpenChange`, whether the group is open is its host's: a host whose group
 * can be drawn again from nothing - a row a windowed list unmounts when it scrolls out and
 * mounts when it scrolls back - keeps the reader's choice there, so the group comes back as the
 * reader left it. Without them the group keeps the choice itself.
 */
export function CappedGroup({
  children,
  className = "",
  label,
  list,
  max = LAYOUT_GROUP_MAX,
  name,
  onOpenChange,
  open: heldOpen,
}: {
  children: ReactNode;
  className?: string;
  label: string;
  list?: string;
  max?: number;
  name: string;
} & CappedGroupOpenState): ReactNode {
  const [ownOpen, setOwnOpen] = useState(false);
  const open = heldOpen ?? ownOpen;
  const setOpen = (next: boolean) => {
    if (heldOpen === undefined) setOwnOpen(next);
    onOpenChange?.(next);
  };
  const groupId = useId();
  const entries = Children.toArray(children);
  const hidden = entries.length - max;
  const shown = open || hidden <= 0 ? entries : entries.slice(0, max);
  const more =
    hidden <= 0 ? null : (
      <button
        aria-controls={groupId}
        aria-expanded={open}
        aria-label={
          open ? `Show fewer ${label}` : `+${hidden}, show all ${entries.length} ${label}`
        }
        className={`inline-flex min-h-11 min-w-11 shrink-0 items-center justify-center rounded-full outline-none focus-visible:ring-2 ${textSecondaryHoverToPrimary} ${focusVisibleRing}`}
        data-layout-more={list === undefined ? "" : undefined}
        onClick={() => setOpen(!open)}
        type="button"
      >
        <span
          className={`rounded-full px-2 py-0.5 text-xs font-medium whitespace-nowrap ${surfaceMutedStrongBg}`}
        >
          {open ? "Show fewer" : `+${hidden}`}
        </span>
      </button>
    );
  const attributes = {
    className,
    "data-layout-group": name,
    "data-layout-max": max === LAYOUT_GROUP_MAX ? undefined : max,
    "data-layout-open": open && hidden > 0 ? "" : undefined,
    id: groupId,
  };
  if (list === undefined) {
    return (
      <div {...attributes}>
        {shown}
        {more}
      </div>
    );
  }
  return (
    <ul aria-label={list} {...attributes}>
      {shown.map((entry, index) => (
        <li key={isValidElement(entry) && entry.key !== null ? entry.key : index}>{entry}</li>
      ))}
      {more === null ? null : <li data-layout-more="">{more}</li>}
    </ul>
  );
}
