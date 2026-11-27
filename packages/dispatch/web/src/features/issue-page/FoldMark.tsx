import type { ReactNode } from "react";

/** A disclosure control's mark: a triangle that points right while what the control opens is
 *  folded and down while it is drawn, one em square so it takes the size of the control's text, in
 *  its colour - the Decisions foot's control, and the Verification view's control of a check's
 *  facts. It is a shape and not text, as `IssuePresets.tsx`'s `MenuMark` is, because the page's
 *  typeface keeps neither triangle: as text, each is drawn in a face of the reader's own machine,
 *  at that face's width. The control's `aria-expanded` says whether it is open, so the shape is
 *  hidden from assistive technology. */
export function FoldMark({ open }: { open: boolean }): ReactNode {
  return (
    <svg
      aria-hidden="true"
      className="inline-block size-[1em] shrink-0 align-middle"
      focusable="false"
      viewBox="0 0 16 16"
    >
      <path d={open ? "M4.5 6.5h7L8 10.5z" : "M6.5 4.5v7L10.5 8z"} fill="currentColor" />
    </svg>
  );
}
