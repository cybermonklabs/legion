/**
 * Text that another author wrote, as a host prints it on lines of its own: on one line
 * (`oneLine`), or cut to its first units without splitting a character (`headUnits`).
 */

/**
 * The characters other than a line feed and a carriage return that a reader splitting lines the
 * Unicode way breaks a line at, and that JSON and TOON write as they are: NEXT LINE (U+0085), LINE
 * SEPARATOR (U+2028) and PARAGRAPH SEPARATOR (U+2029). Global: give it to `replace`, which reads
 * every match whatever `lastIndex` holds, or to `search`.
 */
export const UNESCAPED_LINE_BREAKS = /[\u0085\u2028\u2029]/g;

/** Whether the UTF-16 unit `unit` is one of `UNESCAPED_LINE_BREAKS`: each is one unit. */
export function isUnescapedLineBreak(unit: number): boolean {
  return unit === 0x0085 || unit === 0x2028 || unit === 0x2029;
}

/**
 * `text` with each run of white space, each line break among it, made one space: what a host
 * prints where another author's words sit on one of its own lines, so no line they start can stand
 * as one of the host's. JavaScript's `\s` holds the white space of Unicode and every line break of
 * ECMAScript, LINE SEPARATOR (U+2028) and PARAGRAPH SEPARATOR (U+2029) among them, but not NEXT
 * LINE (U+0085), which Unicode counts as both white space and a line break, so U+0085 is named on
 * its own. A value among a card's or a notice's TOON fields needs none of this to stay on its line:
 * the one encode of those fields writes a space for each of `UNESCAPED_LINE_BREAKS` in it
 * (`encodeFields`, `notice-text.ts`), and TOON escapes every other line break.
 */
export function oneLine(text: string): string {
  return text.replace(/[\s\u0085]+/g, " ");
}

/**
 * The first `length` UTF-16 units of `text`, or one fewer where the unit before the cut is a high
 * surrogate (0xD800-0xDBFF): a cut never splits a character outside the Basic Multilingual Plane,
 * which would leave half of its surrogate pair alone, and a reader decoding the text as UTF-8 then
 * gets U+FFFD in its place. So the units cut off are whole characters. A text no longer than
 * `length` is itself.
 */
export function headUnits(text: string, length: number): string {
  if (text.length <= length) return text;
  if (length <= 0) return "";
  const before = text.charCodeAt(length - 1);
  return text.slice(0, before >= 0xd800 && before <= 0xdbff ? length - 1 : length);
}
