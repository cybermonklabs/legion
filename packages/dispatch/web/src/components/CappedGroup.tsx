import { Children, type ReactNode, useId, useState } from "react";

import {
  focusVisibleRing,
  surfaceMutedStrongBg,
  textSecondaryHoverToPrimary,
} from "../theme/classes";

/** The most entries a group whose length comes from data shows before its "+N" control, the
 *  layout rule's cap (`packages/dispatch/AGENTS.md`, Layout; INFRA-1196's R13). */
export const LAYOUT_GROUP_MAX = 10;

/**
 * A group whose length comes from data - labels, components, links, checks, subscribers,
 * options - drawn as the layout rule asks: at most `max` entries (10 unless given), then one
 * button reading "+N" that shows the rest in place in one click. Open, the same button reads
 * "Show fewer" and folds them back, so the reader decides which way the group is drawn. The
 * button stays where it was in the DOM while the rest are inserted before it, so it keeps the
 * focus across the press.
 *
 * Each child is one entry and draws one element: the group element's direct children are what
 * `e2e/layout-measure.ts` counts against the cap, leaving out the control marked
 * `data-layout-more`. The group element carries `data-layout-group={name}`, and
 * `data-layout-max` when its cap is below 10; while the reader has it open it carries
 * `data-layout-open` too, since then it shows more than its cap by the reader's own choice.
 *
 * `label` is the group in words, a plural noun ("labels"), which ends the button's name: "Show 3
 * more labels", and "Show fewer labels" while open. `className` lays the group out; `contents`
 * lets its entries flow in the row around it, beside a control the host draws after them.
 */
export function CappedGroup({
  children,
  className = "",
  label,
  max = LAYOUT_GROUP_MAX,
  name,
}: {
  children: ReactNode;
  className?: string;
  label: string;
  max?: number;
  name: string;
}): ReactNode {
  const [open, setOpen] = useState(false);
  const groupId = useId();
  const entries = Children.toArray(children);
  const hidden = entries.length - max;
  return (
    <div
      className={className}
      data-layout-group={name}
      data-layout-max={max < LAYOUT_GROUP_MAX ? max : undefined}
      data-layout-open={open && hidden > 0 ? "" : undefined}
      id={groupId}
    >
      {open || hidden <= 0 ? entries : entries.slice(0, max)}
      {hidden <= 0 ? null : (
        <button
          aria-controls={groupId}
          aria-expanded={open}
          aria-label={open ? `Show fewer ${label}` : `Show ${hidden} more ${label}`}
          className={`inline-flex min-h-11 min-w-11 shrink-0 items-center justify-center rounded-full outline-none focus-visible:ring-2 ${textSecondaryHoverToPrimary} ${focusVisibleRing}`}
          data-layout-more=""
          onClick={() => setOpen((current) => !current)}
          type="button"
        >
          <span
            className={`rounded-full px-2 py-0.5 text-xs font-medium whitespace-nowrap ${surfaceMutedStrongBg}`}
          >
            {open ? "Show fewer" : `+${hidden}`}
          </span>
        </button>
      )}
    </div>
  );
}
