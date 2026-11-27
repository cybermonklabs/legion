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

/** The card of the thread `key` names in `container` - the column's list or a node's sheet - or
 *  null where it draws none. */
export function cardIn(container: Element | null, key: string): HTMLElement | null {
  return container?.querySelector<HTMLElement>(`[data-margin-item="${CSS.escape(key)}"]`) ?? null;
}

/** Gives `target` the focus, as a script gives it to an element out of the tab order: one with
 *  no `tabindex` takes `tabIndex = -1` first. The focus takes no scroll of the browser's own,
 *  which goes to where the card stood before the stack placed it, and the card is brought into
 *  the window once it is placed (`revealAfterPlacement`): the nearest edge, or `center` for an
 *  address. */
export function takeFocus(target: HTMLElement, reveal: ScrollLogicalPosition = "nearest"): void {
  if (!target.hasAttribute("tabindex")) target.tabIndex = -1;
  target.focus({ preventScroll: true });
  revealAfterPlacement(target, reveal);
}
