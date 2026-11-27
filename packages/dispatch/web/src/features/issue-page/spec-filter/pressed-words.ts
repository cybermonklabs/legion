import { FOCUSABLE } from "../RowText";

/** The item containing the focus, if it belongs to this tree. */
export function focusedTreeItem(tree: HTMLElement): HTMLElement | null {
  const item = document.activeElement?.closest<HTMLElement>('[role="treeitem"]') ?? null;
  return tree.contains(item) ? item : null;
}

/** Which statement's words a press hit, excluding their links, controls and chips. */
export function statementWordsAt(target: EventTarget): HTMLElement | null {
  if (!(target instanceof Element)) return null;
  const words = target.closest<HTMLElement>("[data-spec-statement] [data-node-words]");
  const control = target.closest(
    `${FOCUSABLE}, summary, [role="button"], [role="link"], [data-node-trail]`
  );
  return words !== null && !words.contains(control) ? words : null;
}
