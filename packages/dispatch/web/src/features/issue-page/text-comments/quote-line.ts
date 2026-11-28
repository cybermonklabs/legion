/**
 * The quoted words a thread's card draws first (R4): their text up to the first line break, cut
 * in script at the last word end before the 80th character with an ellipsis, so that no style
 * clamps them. The card draws its `quoteLine` with no clamp of its own (`ThreadCard.tsx`), so the
 * cut is this function's alone; `TextThreadCard.tsx` passes each thread's quote through it, and
 * the card's suggestion diff draws the whole quote, the words Accept replaces.
 */

/** The most characters a quoted line keeps, its ellipsis included. */
const MOST = 80;

/** What marks a quoted line cut short. */
const ELLIPSIS = "\u2026";

/** A line break of any kind a stored text holds. */
const LINE_BREAK = /\r\n|[\n\r\u2028\u2029]/u;

/** One character of white space. */
const SPACE = /\s/u;

/**
 * `quote` up to its first line break, and where that line passes 80 characters, its words up to
 * the last one that ends before the 80th character, then an ellipsis: so the line is at most 80
 * characters, the ellipsis among them. A first word longer than that is cut inside itself. The
 * characters are counted as a reader counts them, a surrogate pair as one.
 */
export function quoteLine(quote: string): string {
  const line = quote.split(LINE_BREAK, 1)[0] ?? "";
  const characters = Array.from(line);
  if (characters.length <= MOST) return line;
  // The kept words end at `end`, before the 80th character, where a word's last character is
  // followed by white space.
  let end = 0;
  for (let at = 1; at < MOST; at += 1) {
    const before = characters[at - 1] ?? "";
    const after = characters[at] ?? "";
    if (!SPACE.test(before) && SPACE.test(after)) end = at;
  }
  const kept = end === 0 ? characters.slice(0, MOST - 1) : characters.slice(0, end);
  return `${kept.join("")}${ELLIPSIS}`;
}
