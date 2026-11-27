/** Ends a press still armed (`components/focus-place.ts`'s `armHandOff`) as a case ends, as the
 *  reader's next pointer press on the page does, so that none outlives its case into the next
 *  one: a case that installs the fake clock would run that press's end on it, which is not that
 *  case's to move. */
export function disarm(): void {
  document.body.dispatchEvent(new PointerEvent("pointerdown", { bubbles: true }));
}
