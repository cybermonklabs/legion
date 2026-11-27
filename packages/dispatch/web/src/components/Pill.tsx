import { type ReactNode, type RefObject, useLayoutEffect, useRef, useState } from "react";

import type { ArtifactApproval } from "../api/types";
import {
  approvalPill,
  pageFailedText,
  pageHairBorder,
  pageInkText,
  pageLabelChip,
  pageMarkBg,
  pageMutedText,
  pagePaperBg,
  pageStatusDoneText,
  pageWaitingChip,
  statusPill,
  statusPillDot,
  surfaceMutedBg,
  surfaceMutedStrongBg,
  textMutedOnSurfaceMuted,
  textSecondaryOnSurface,
} from "../theme/classes";
import { usePageWords } from "./tooltips";

export type PillTone = "label" | "selected-label" | "status";

const pillTones: Record<PillTone, string> = {
  label: `${surfaceMutedBg} ${textMutedOnSurfaceMuted}`,
  "selected-label": `${surfaceMutedStrongBg} ${textSecondaryOnSurface}`,
  status: `${statusPill.bg} ${statusPill.text}`,
};

/** Each tone in the new issue page's colours, for a pill drawn inside `NoTooltips`, where every
 *  colour a shared control draws is one of that page's: a label and a status as the page draws
 *  a label (the prototype's `.lchip`), and a selected label, the held claim's tone, in the ink
 *  on the page's mark grey, one step stronger as `main`'s selected label is. */
const pagePillTones: Record<PillTone, string> = {
  label: pageLabelChip,
  "selected-label": `${pageMarkBg} ${pageInkText}`,
  status: pageLabelChip,
};

/** The pill's shape alone, with no colour of its own, for a host that paints the text itself:
 *  stacking a second text colour after `pillClassName` leaves which of the two paints to the
 *  CSS source order rather than the attribute order, so a pass, a fail or a gap mark could come
 *  out muted. */
export const pillShapeClassName =
  "inline-flex shrink-0 items-center rounded-full px-2 py-1 text-xs font-medium whitespace-nowrap";

/** A pill's colours alone, for a host that keeps the tone but not the one-line shape: a claim
 *  chip stacked on a Board card. */
export function pillToneClassName(tone: PillTone): string {
  return pillTones[tone];
}

/** The pill's full class string, for a host that must stay one interactive element (a parent
 *  link with a border) while looking identical - a border on a wrapper around a `Pill` would
 *  outline a bigger shape than the fill. `onPage` is a host's inside `NoTooltips`, which draws
 *  the tone in the new issue page's colours, in the shape that wraps; `wraps` draws that shape
 *  in either set of colours.
 *
 *  The shape that wraps is `pillShapeClassName` for a pill that holds a value which must be read
 *  whole (a label, a claim's holder): it keeps `max-w-full`, wraps and breaks anywhere, and
 *  neither keeps its width nor holds its line. Every pill inside `NoTooltips` (the new issue
 *  page) is drawn so, and so is a claim in a table that wraps its titles (`ClaimChip`'s
 *  `wraps`) and a label in an issue table's Labels column (`LabelPill`'s `wraps`). On one line
 *  it is a pill, its corners the full radius; on more than one (`wrapped`,
 *  `usePillWrapped`) its corners are 8 px (`rounded-lg`), since the full radius of a box several
 *  lines tall draws an oval, which leaves the words of its first and last lines outside its
 *  fill. */
export function pillClassName(
  tone: PillTone,
  onPage = false,
  wraps = onPage,
  wrapped = false
): string {
  const shape = wraps
    ? `inline-flex min-w-0 max-w-full flex-wrap items-center ${wrapped ? "rounded-lg" : "rounded-full"} px-2 py-1 text-xs font-medium [overflow-wrap:anywhere]`
    : pillShapeClassName;
  return `${shape} ${onPage ? pagePillTones[tone] : pillToneClassName(tone)}`;
}

/**
 * Whether a pill drawn in the shape that wraps (`pillClassName`'s `wraps`) takes more than one
 * line, for its corners, read off the drawn pill (`ref`): no CSS reads how many lines an
 * element's own words take, so its content box's height is read against one of its lines, and
 * more than one and a half lines is more than one. Read before the first paint, and again each
 * time the pill's size changes - its column narrowed or widened, a name or an age arrived - in
 * the render that change schedules, batched with the page's own rather than forced inside the
 * frame: forcing it would draw, in that frame, every other update the page has waiting too. A
 * pill not in that shape (`wraps` false) is on one line, and so is one where nothing is laid
 * out: happy-dom, which has no `ResizeObserver` either.
 */
export function usePillWrapped(wraps: boolean): {
  readonly ref: RefObject<HTMLSpanElement | null>;
  readonly wrapped: boolean;
} {
  const ref = useRef<HTMLSpanElement>(null);
  const [wrapped, setWrapped] = useState(false);
  useLayoutEffect(() => {
    const pill = ref.current;
    if (!wraps || pill === null) return;
    const measure = () => {
      const style = getComputedStyle(pill);
      const edges = [
        style.paddingTop,
        style.paddingBottom,
        style.borderTopWidth,
        style.borderBottomWidth,
      ].reduce((sum, value) => sum + (Number.parseFloat(value) || 0), 0);
      setWrapped(
        pill.getBoundingClientRect().height - edges > Number.parseFloat(style.lineHeight) * 1.5
      );
    };
    measure();
    if (typeof ResizeObserver === "undefined") return;
    const sizes = new ResizeObserver(measure);
    sizes.observe(pill);
    return () => sizes.disconnect();
  }, [wraps]);
  return { ref, wrapped: wraps && wrapped };
}

/** A pill. Inside `NoTooltips` it takes the shape that wraps (`pillClassName`), and so does one
 *  whose host asks for it (`wraps`), in this page's colours: a value read whole in a column
 *  narrower than its words, as a label in an issue table's Labels column. */
export function Pill({
  children,
  className,
  tone = "label",
  wraps = false,
}: {
  children: ReactNode;
  className?: string;
  tone?: PillTone;
  wraps?: boolean;
}): ReactNode {
  const page = usePageWords();
  const wrapping = page !== null || wraps;
  const { ref, wrapped } = usePillWrapped(wrapping);
  return (
    <span
      className={`${pillClassName(tone, page !== null, wrapping, wrapped)} ${className ?? ""}`}
      ref={ref}
    >
      {children}
    </span>
  );
}

export function LabelPill({
  children,
  selected = false,
  wraps = false,
}: {
  children: ReactNode;
  selected?: boolean;
  /** Draw the label in the shape that wraps (`Pill`'s `wraps`). */
  wraps?: boolean;
}): ReactNode {
  return (
    <Pill tone={selected ? "selected-label" : "label"} wraps={wraps}>
      {children}
    </Pill>
  );
}

export function StatusPill({ children }: { children: ReactNode }): ReactNode {
  const page = usePageWords();
  return (
    <Pill className="gap-1.5 font-semibold tracking-wide uppercase" tone="status">
      {/* Inside `NoTooltips` the dot is the page's quiet grey, as neutral there as `main`'s
          slate is here: it names no state, the word beside it does. */}
      <span
        aria-hidden="true"
        className={`size-1.5 rounded-full ${page === null ? statusPillDot : "bg-page-quiet"}`}
      />
      {children}
    </Pill>
  );
}

/** Whether `state` is one of `approvalPill`'s own keys, which are every approval state this
 *  build has an outline for. */
function isApprovalTone(state: string): state is keyof typeof approvalPill {
  return Object.hasOwn(approvalPill, state);
}

/** `state` as the outline the chip wears. The route table carries the state as a plain string;
 *  one this build does not know wears the draft outline - the neutral one - while
 *  `approvalLabel` shows the word itself, so the chip is drawn rather than left unpainted. */
function approvalTone(state: string): keyof typeof approvalPill {
  return isApprovalTone(state) ? state : "draft";
}

/** Each approval outline in the new issue page's colours, for a chip drawn inside `NoTooltips`:
 *  the prototype's `.pill`, a rule in the colour of its words on the page's paper. Approved is
 *  the passed green and changes requested the failed red; a request that waits, or an approval
 *  the document has moved past, is the page's waiting chip, its one warning; a draft keeps its
 *  dashed rule, in the hairline grey, with the muted words. The words say the state; the
 *  colour repeats it. */
const pageApprovalPill: Record<keyof typeof approvalPill, string> = {
  approved: `border-page-passed ${pagePaperBg} ${pageStatusDoneText}`,
  awaiting: pageWaitingChip,
  changes_requested: `border-page-failed ${pagePaperBg} ${pageFailedText}`,
  draft: `border-dashed ${pageHairBorder} ${pagePaperBg} ${pageMutedText}`,
  stale: pageWaitingChip,
};

/** The approval pill's full class string, for hosts whose chip must stay an interactive
 * element (ApprovalChip's button opens the review history) while looking identical.
 *
 * It wraps and it shrinks, for every host that draws it: the longest label the chip can carry
 * (`Awaiting approval of spec.md v1 and verification.md v1`, one request covering a spec and
 * its Verification document) is wider than a 320 px phone, and a pill that could neither wrap
 * nor shrink pushed the whole page sideways. The label is never clamped, ellipsized or hidden
 * behind a `title`, so a phone reader still reads all of it; `[overflow-wrap:anywhere]` is what
 * lets a long document name break, and so drops the pill's min-content width to something a
 * narrow row can hold. Both utilities belong to this one string rather than being appended by a
 * host, for the reason `pillShapeClassName` records above: two conflicting Tailwind utilities
 * are settled by the generated CSS's order, not the attribute's.
 *
 * `compact` is the new issue page header's: the prototype's `.pill`, 12 px type on an 18 px
 * line inside its rule, and 24 px tall at every width, the height of every other control on
 * that line: the chip is a button, and a 24 px target holds at every width (WCAG 2.5.8).
 * `default` is every other host's. `onPage` is a host's inside `NoTooltips`, which draws the
 * outline in that page's colours (`pageApprovalPill`). */
export function approvalPillClassName(
  state: ArtifactApproval["state"],
  size: "compact" | "default" = "default",
  onPage = false
): string {
  const height = size === "compact" ? "min-h-6 py-0 leading-[18px]" : "py-1";
  return `inline-flex min-w-0 items-center rounded-full border px-2 ${height} text-center text-xs font-medium [overflow-wrap:anywhere] ${(onPage ? pageApprovalPill : approvalPill)[approvalTone(state)]}`;
}
