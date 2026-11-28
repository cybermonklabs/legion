import { FOCUSABLE } from "../RowText";

/** The item containing the focus, if it belongs to this tree. */
export function focusedTreeItem(tree: HTMLElement): HTMLElement | null {
  const item = document.activeElement?.closest<HTMLElement>('[role="treeitem"]') ?? null;
  return tree.contains(item) ? item : null;
}

/** Which row's words a press hit - the element `selector` names, a statement's words in this view
 *  or a row's in the outline (`IssueTree.tsx`, R18) - excluding their links, controls and chips. */
export function statementWordsAt(target: EventTarget, selector: string): HTMLElement | null {
  if (!(target instanceof Element)) return null;
  const words = target.closest<HTMLElement>(selector);
  const control = target.closest(
    `${FOCUSABLE}, summary, [role="button"], [role="link"], [data-node-trail]`
  );
  return words !== null && !words.contains(control) ? words : null;
}
