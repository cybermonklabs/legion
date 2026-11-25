/**
 * The quoted words a thread's card draws first (R4): their text up to the first line break, cut
 * in script at the last word end before the 80th character with an ellipsis, so that no style
 * clamps them. Built by t9; it answers the words as they are until then.
 */
export function quoteLine(quote: string): string {
  return quote;
}
