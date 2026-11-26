/**
 * Brings `target` into the window once the column has placed it (INFRA-5706's R2 and R5). A card
 * the column draws stands at the top of its region until the stack has measured it and set its
 * top, which is after the commit that mounts it; the browser's own scroll to a field or a card
 * that takes the focus in that commit goes to that top, so the reader finds the page moved to the
 * head of the column and the focused card out of sight. So the focus takes no scroll
 * (`preventScroll`), and this brings the card to the window in the next frame, by when every
 * synchronous pass of the stack has run: `nearest` moves the page only as far as the card needs,
 * and not at all where it is in the window, which is where a reader who has just selected words
 * or sent a comment finds it; `center` is for an address, which lands a reader from elsewhere.
 * Answers the function that cancels it, for a card that goes before the frame.
 */
export function revealAfterPlacement(
  target: HTMLElement,
  block: ScrollLogicalPosition = "nearest"
): () => void {
  const frame = requestAnimationFrame(() => {
    if (target.isConnected) target.scrollIntoView?.({ block });
  });
  return () => cancelAnimationFrame(frame);
}
