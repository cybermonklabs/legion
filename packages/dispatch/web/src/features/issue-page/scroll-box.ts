/** The nearest box above `element` that scrolls its content, or null where the document does:
 *  the box the history's anchor keeps a place in (`IssueHistory.tsx`), and the box Details' hold
 *  of the place a key or an address landed the focus at claims (`details-more.tsx`). Both find
 *  their box by this one rule, so the hold claims the very box the anchor scrolls. */
export function scrollingAncestor(element: HTMLElement | null): HTMLElement | null {
  for (
    let node = element?.parentElement;
    node && node !== document.body;
    node = node.parentElement
  ) {
    const overflow = getComputedStyle(node).overflowY;
    if (overflow === "auto" || overflow === "scroll") return node;
  }
  return null;
}

/** The change of a box's `scrollTop` that brings an element at `at` into the box's view `view`
 *  by the least scroll: the scroll `scrollIntoView({ block: "nearest" })` makes. None where the
 *  element lies wholly in the view or spans the whole of it. Else an element no taller than the
 *  view comes in by its top from above the view and by its foot from below it, and a taller one
 *  by its other edge, so that it fills the view. */
export function nearestScroll(at: DOMRect, view: DOMRect): number {
  const above = at.top < view.top;
  const below = at.bottom > view.bottom;
  if (above === below) return 0;
  const fits = at.height <= view.height;
  return above === fits ? at.top - view.top : at.bottom - view.bottom;
}
