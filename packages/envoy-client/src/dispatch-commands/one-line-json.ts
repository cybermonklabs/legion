/** The characters other than a line feed and a carriage return that a reader splitting lines the
 *  Unicode way breaks a line at, and JSON leaves as they are: NEXT LINE, LINE SEPARATOR and
 *  PARAGRAPH SEPARATOR. (JSON escapes every other one, each below U+0020.) */
const unescapedLineBreaks = /[\u0085\u2028\u2029]/g;

/** `text` as a JSON string that holds no line break: JSON's own escapes, and the `\u` escape of
 *  each of `unescapedLineBreaks`, which reads back as the same string. A command that prints one
 *  line per record writes through this each field another writer filled, so no value of one can
 *  end its line early or add a line nobody wrote: `dispatch plans` a retro's reason, and
 *  `dispatch steps` the id of the check a fix fixes. */
export function oneLineJson(text: string): string {
  return JSON.stringify(text).replace(
    unescapedLineBreaks,
    (character) => `\\u${character.charCodeAt(0).toString(16).padStart(4, "0")}`
  );
}
