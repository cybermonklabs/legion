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
