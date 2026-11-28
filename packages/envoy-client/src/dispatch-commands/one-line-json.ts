import { printableJson } from "../runner-log";

/** `text` as a JSON string that holds no line break and no character that changes how the rest of
 *  its line reads: JSON's own escapes, and the `\u` escape of each character `printableJson`
 *  escapes, which JSON leaves as it is (DEL and the C1 controls, U+0085 NEXT LINE among them; each
 *  format character, such as a bidi override or a zero-width joiner; and the line and paragraph
 *  separators), with the `[` of a `##[` as `\u005b`. It reads back as the same string. A command
 *  that prints one line per record writes through this each field another writer filled, so no
 *  value of one can end its line early, add a line nobody wrote, or show its line in another
 *  order: `dispatch plans` a retro's reason, and `dispatch steps` the id of the check a fix fixes. */
export function oneLineJson(text: string): string {
  return printableJson(JSON.stringify(text));
}
