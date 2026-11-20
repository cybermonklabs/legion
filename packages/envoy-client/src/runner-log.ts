/**
 * Text as a command prints it into the log of a GitHub Actions runner, so that no text the command
 * did not write (an argument, a variable, a file's name, an answer of Dispatch or of the secrets
 * broker) starts a line of its own or names a workflow command. It is the rule of Go's
 * `runnerlog.Printable` (packages/envoy/internal/runnerlog), so the two print one form, and a
 * change of one is a change of the other.
 *
 * A GitHub runner ends a line of the log at \n and at \r, and reads a workflow command in two
 * forms: a line that starts with "::" once its leading white space is trimmed, and a "##["
 * anywhere in a line. The runner's own reasons for each step of the rule are in runnerlog's doc.
 *
 * Where the two differ:
 * - A string here holds UTF-16 code units, never bytes, so there is no byte that is not UTF-8 to
 *   write as `\xNN`: the command reads its files and its answers as UTF-8, which gives U+FFFD for
 *   each such byte. The nearest case, a lone surrogate, which a JSON answer can carry as `\ud800`,
 *   is written as its code unit, `\ud800`.
 * - Which characters are format characters (Cf) is the JavaScript engine's Unicode version here
 *   and Go's `unicode` package there, so a character a later version adds can differ.
 * - `printableLine` also writes a line's leading "::" as `\x3a:`, for a line whose first words are
 *   not the command's own. runnerlog needs no such step: each line `Say` writes starts with the
 *   command's fixed prefix.
 * - `printableJson` writes in JSON's own escapes what Printable writes in Go's, so the JSON reads
 *   back as it was. Go's JSON lines (cmd/ci-report, `spec-check checks`) write the "[" of a "##["
 *   as `\u005b` as it does, and they leave DEL, C1 and format characters but U+2028 and U+2029 as
 *   they are, which `printableJson` escapes.
 */

/** The characters `printable` writes as an escape: each control character (Cc: C0, DEL and C1),
 *  each format character (Cf), the line separator (Zl), the paragraph separator (Zp), and each
 *  lone surrogate (Cs). */
const ESCAPED = /[\p{Cc}\p{Cf}\p{Zl}\p{Zp}\p{Cs}]/gu;

/** The characters JSON.stringify writes as they are that `printable` escapes: DEL, the C1
 *  controls, each format character and the two separators. It writes C0 and a lone surrogate as
 *  escapes already. */
const JSON_ESCAPED = /[\u007f-\u009f\p{Cf}\p{Zl}\p{Zp}]/gu;

/** The escapes strconv.Quote writes as a backslash and a letter. */
const SHORT_ESCAPES: Readonly<Partial<Record<string, string>>> = {
  "\u0007": "\\a",
  "\b": "\\b",
  "\f": "\\f",
  "\n": "\\n",
  "\r": "\\r",
  "\t": "\\t",
  "\v": "\\v",
};

function hex(value: number, digits: number): string {
  return value.toString(16).padStart(digits, "0");
}

/** `character` as a Go string literal writes it (strconv.Quote): a short escape such as `\n`, else
 *  `\xNN` for the rest of C0 and DEL, `\uNNNN` in the basic plane and `\UNNNNNNNN` past it. */
function goEscape(character: string): string {
  const short = SHORT_ESCAPES[character];
  if (short !== undefined) return short;
  const point = character.codePointAt(0) ?? 0;
  if (point < 0x20 || point === 0x7f) return `\\x${hex(point, 2)}`;
  return point > 0xffff ? `\\U${hex(point, 8)}` : `\\u${hex(point, 4)}`;
}

/**
 * `text` as a command prints it in a line of a runner's log, as runnerlog.Printable writes it:
 * each control character, format character, line or paragraph separator and lone surrogate as a
 * Go string literal writes it (`\n`, `\r`, `\x1b`, `\u0085`, `\u200d`), so no line break of `text`
 * reaches the log, and then the "[" of every "##[" as `\x5b`, so no line holds a "##[". Text that
 * holds none of these is `text` itself.
 */
export function printable(text: string): string {
  return text.replace(ESCAPED, goEscape).replaceAll("##[", "##\\x5b");
}

/**
 * A whole line whose first words are text the command did not write, such as Dispatch's refusal
 * `<CODE>: <message>`: `printable(text)`, with the first ":" of a "::" that starts it, after its
 * leading white space, written as `\x3a`, so the line starts no workflow command. The white space
 * left after `printable` is a space of category Zs, which the runner trims as it trims a space.
 */
export function printableLine(text: string): string {
  return printable(text).replace(/^(\s*):(?=:)/u, "$1\\x3a");
}

/**
 * `json`, JSON text such as JSON.stringify writes, as a command prints it in a runner's log: each
 * character `printable` escapes is written as JSON's `\uNNNN` (DEL, C1, a format character, U+2028,
 * U+2029; JSON.stringify already escapes C0 and a lone surrogate), and the "[" of every "##[" as
 * `\u005b`. A JSON reader reads it back as the same value. A "#" or a raw such character is only
 * ever inside a string of JSON text, where an escape is valid.
 */
export function printableJson(json: string): string {
  return json
    .replace(JSON_ESCAPED, (character) =>
      Array.from(
        { length: character.length },
        (_, unit) => `\\u${hex(character.charCodeAt(unit), 4)}`
      ).join("")
    )
    .replaceAll("##[", "##\\u005b");
}
