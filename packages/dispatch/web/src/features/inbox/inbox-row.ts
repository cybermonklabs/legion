/** The attribute every Inbox ask row carries, valued with its ask id: the list's keyboard, viewport
 *  and jump target. Group headers and band labels never carry it. */
export const ROW_ATTRIBUTE = "data-inbox-row";

/** Every Inbox ask row. */
export const ROW_SELECTOR = `[${ROW_ATTRIBUTE}]`;

/** The rows the list `root` draws, in the order the reader sees them; none while it is not
 *  drawn. */
export function rowsIn(root: HTMLElement | null): HTMLElement[] {
  return [...(root?.querySelectorAll<HTMLElement>(ROW_SELECTOR) ?? [])];
}

/** The row of the list `root` that `id` names: null where `id` is null, and null where the row it
 *  names has since left the list. */
export function rowIn(root: HTMLElement | null, id: string | null): HTMLElement | null {
  return id === null
    ? null
    : (root?.querySelector<HTMLElement>(`[${ROW_ATTRIBUTE}="${id}"]`) ?? null);
}
