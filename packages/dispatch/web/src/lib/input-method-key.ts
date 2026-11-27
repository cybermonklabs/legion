/** Whether `event` is an input method's key, which acts on nothing of the page's: one pressed in
 *  a composition, or the one that ends it, which Safari sends after its compositionend with
 *  `isComposing` false and keyCode 229 (WebKit bug 165004), as the title's editor reads it
 *  (`IssueTitleRow.tsx`). */
export function inputMethodKey(event: KeyboardEvent): boolean {
  return event.isComposing || event.keyCode === 229;
}
