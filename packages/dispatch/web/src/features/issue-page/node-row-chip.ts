/**
 * The words a chip names its target by, on the node's own line (the architect's ruling 5): the
 * target's short name where the model
 * holds one, and until then the target's first words, cut at a word within `CHIP_LIMIT`
 * characters, the ellipsis included. No address, slug or document name is drawn in its place
 * (R22): a target with no words of its own answers `""`, and its chip draws nothing to name it by.
 *
 * Pure, so a filter's own row (`decisions-filter/`, `spec-filter/`) and the browser suite name a
 * target the same way the outline's row does.
 */

/** How many characters a chip's words may hold, the ellipsis of a cut included. */
export const CHIP_LIMIT = 32;

/** The marks a cut never ends on before its ellipsis. */
const TRAILING_MARKS = /[\s,;:.!?·–—-]+$/u;

export function chipWords(name: string): string {
  const whole = name.replace(/\s+/gu, " ").trim();
  if (whole.length <= CHIP_LIMIT) return whole;
  // The ellipsis takes the last place. The character after the room says whether a word ends
  // exactly at its edge, which is a cut at a word as much as a space inside it is.
  const room = CHIP_LIMIT - 1;
  const window = whole.slice(0, room + 1);
  const space = window.lastIndexOf(" ");
  const kept = space <= 0 ? whole.slice(0, room) : whole.slice(0, space);
  const words = kept.replace(TRAILING_MARKS, "");
  return `${words === "" ? kept : words}…`;
}
