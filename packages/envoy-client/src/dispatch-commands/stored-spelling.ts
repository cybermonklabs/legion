import { PATH_KEEPS_BYTES, pathKeepsBytesOf } from "@legion/contracts";
import {
  type DirectiveAttribute,
  runOf,
  type TypedOpener,
  typedFenceBrace,
  typedOpenerAt,
} from "../typed-block-opener";

/**
 * A typed block's opening line as Dispatch reads it (TypedOpener), cut where the attribute-order
 * check and the refusal read it: `head` up to and with its `{` (a quote or list marker before the
 * fence, the fence, the name), then the attributes before the `}` that ends the line: each as
 * written, the whitespace around them, and the block's id.
 */
interface HeadedOpener extends TypedOpener {
  readonly head: string;
}

/** `line` read as a typed block's opening line (typedOpenerAt, which reads the frame and its
 *  attributes as Dispatch does; `renderTypedAttributes` in pmdoc's schema.go writes such a line)
 *  from its first fence: the first run of three colons or more that a name and a `{` follow
 *  (typedFenceBrace). Its head is the line up to and with that `{`, the shortest start of the
 *  line that ends in such a fence. Undefined for a line that holds no fence, and for one that is
 *  no opener from its first fence: the scan does not go on to a later one. A run of colons is
 *  passed whole and any other character once, so no line costs more than its length. */
function headedOpener(line: string): HeadedOpener | undefined {
  for (let at = 0; at < line.length; at += Math.max(1, runOf(line, at, ":"))) {
    const brace = typedFenceBrace(line, at);
    if (brace === -1) continue;
    const opener = typedOpenerAt(line, at);
    return opener === undefined ? undefined : { ...opener, head: line.slice(0, brace + 1) };
  }
  return undefined;
}

/** The names of `attributes` as a map to each one's written text, or undefined when a name
 *  stands twice, since such a line can be put in no one order. */
function attributesByName(
  attributes: readonly DirectiveAttribute[]
): ReadonlyMap<string, string> | undefined {
  const byName = new Map(attributes.map((attribute) => [attribute.name, attribute.text]));
  return byName.size === attributes.length ? byName : undefined;
}

/**
 * The line `given` with its attributes put in the order `stored` writes them: each attribute in
 * its own spelling, and the whitespace between them where it stood. Undefined unless the two hold
 * the same names, each once. The order is the one Dispatch's render wrote, read off its own line,
 * so no copy of that order is kept here.
 */
function inStoredOrder(given: HeadedOpener, stored: HeadedOpener): string | undefined {
  const written = attributesByName(given.attributes);
  if (
    written === undefined ||
    attributesByName(stored.attributes) === undefined ||
    stored.attributes.length !== given.attributes.length
  ) {
    return undefined;
  }
  let line = `${given.head}${given.gaps[0] ?? ""}`;
  for (const [index, attribute] of stored.attributes.entries()) {
    const text = written.get(attribute.name);
    if (text === undefined) return undefined;
    line += `${text}${given.gaps[index + 1] ?? ""}`;
  }
  return `${line}}`;
}

/** Whether the written opening line `given` differs from the stored line `held`, read as the
 *  opener `stored`, only in the order of its attributes: `given` with them put in `stored`'s
 *  order is `held`. False when either line is no typed block's opening line (headedOpener, which
 *  gives undefined). The upload and the refusal both pass over such a line by this rule. */
function onlyReordered(
  given: HeadedOpener | undefined,
  held: string,
  stored: HeadedOpener | undefined
): boolean {
  return given !== undefined && stored !== undefined && inStoredOrder(given, stored) === held;
}

/** What the answer of an upload says of a block whose attributes it put in Dispatch's order, read
 *  off `opener`, its opening line as Dispatch stores it: the block by its name, which is its type,
 *  and its id as Dispatch reads it (directiveAttributes), its line (counted from 1), and that
 *  order. */
function reorderedBlockLine(line: number, opener: HeadedOpener): string {
  const named = `${opener.name} block${opener.id === undefined ? "" : ` ${opener.id}`}`;
  const order = opener.attributes.map((attribute) => attribute.name).join(", ");
  return `This upload put the attributes of ${named} (line ${line}) in the order Dispatch stores: ${order}.`;
}

/**
 * What the answer of an upload says of the typed blocks whose attributes `given` writes in another
 * order than `stored`, the text Dispatch's own round trip (POST /api/v1/markdown/round-trip)
 * answers, one line for each block (reorderedBlockLine), when that is the whole difference between
 * the two: `given` with each such opener's attributes put in the stored opener's order is
 * `stored`, but for the final line feed the store adds. Undefined when anything else differs. An
 * attribute's position carries no meaning, so such a text is stored in Dispatch's order rather
 * than refused, and the order is Dispatch's render's, never a copy of it.
 */
function attributesReordered(given: string, stored: string): string[] | undefined {
  const written = withoutFinalLineFeed(given).split("\n");
  const holds = withoutFinalLineFeed(stored).split("\n");
  if (written.length !== holds.length) return undefined;
  const lines: string[] = [];
  for (const [index, held] of holds.entries()) {
    const wrote = written[index];
    if (wrote === held) continue;
    const opener = headedOpener(held);
    if (
      wrote === undefined ||
      opener === undefined ||
      !onlyReordered(headedOpener(wrote), held, opener)
    ) {
      return undefined;
    }
    lines.push(reorderedBlockLine(index + 1, opener));
  }
  return lines;
}

/**
 * How much of one line, or of one word of a line, a refusal quotes. A line of a document is
 * bounded by the document itself, and a word with no space in it (a URL, a data URI, a table's
 * separator row) by its line, so either can be as long as the whole text, and a quote nobody can
 * read to the end is one nobody acts on — the same reason STORED_TEXT_PRINTED bounds the stored
 * text below, and the cut is named the same way.
 */
const STORED_LINE_QUOTED = 200;

/** The characters (code points) of a line, or of one word of a line, as a refusal quotes them
 *  from the one at index `from`: the JSON spelling of at most STORED_LINE_QUOTED of them, and
 *  what is cut before and after them named after it. Quoted from its first character, a text at
 *  most that long is quoted whole, and a longer one is cut after its first STORED_LINE_QUOTED. */
function quotedStoredLine(characters: readonly string[], from: number): string {
  const shown = characters.slice(from, from + STORED_LINE_QUOTED);
  const quote = JSON.stringify(shown.join(""));
  const cutAfter = from + shown.length < characters.length;
  if (from === 0 && !cutAfter) return quote;
  const cut = from === 0 ? "after" : cutAfter ? "before and after" : "before the last";
  return `${quote} (cut ${cut} ${shown.length} of its ${characters.length} characters)`;
}

/** One word of a line as the refusal names it: an attribute of a typed block's opening line, or
 *  a run of other characters or of whitespace. */
interface LineWord {
  readonly text: string;
  readonly attribute: boolean;
}

/** `line` cut into its words, `opener` being that line read as a typed block's opening line
 *  (headedOpener), undefined for any other line. The words joined are the line. */
function lineWords(line: string, opener: HeadedOpener | undefined): LineWord[] {
  const plain = (text: string): LineWord[] =>
    (text.match(/\s+|\S+/g) ?? []).map((word) => ({ text: word, attribute: false }));
  if (opener === undefined) return plain(line);
  const words = plain(opener.head);
  for (const [index, gap] of opener.gaps.entries()) {
    if (gap !== "") words.push({ text: gap, attribute: false });
    const attribute = opener.attributes[index];
    if (attribute !== undefined) words.push({ text: attribute.text, attribute: true });
  }
  words.push({ text: "}", attribute: false });
  return words;
}

/** A word as the refusal names it: an attribute as it is written, whole, and any other word by
 *  its `characters` (code points) from the one at index `from`, quoted as quotedStoredLine quotes
 *  them. */
function wordText(word: LineWord, from = 0, characters = Array.from(word.text)): string {
  if (word.attribute) return `the attribute ${word.text}`;
  return `${/\S/.test(word.text) ? "the word" : "the space"} ${quotedStoredLine(characters, from)}`;
}

/**
 * Where two different lines, cut into the words `written` and `holds` (lineWords), first differ:
 * by the word that differs in each and the character it starts at (code points, counted from 1),
 * a word at most STORED_LINE_QUOTED characters long named whole. A word longer than that in
 * either spelling is quoted in each from the first character where the two differ, which is then
 * the character named, with the cut named. When one spelling of such a word is the start of the
 * other, nothing of the shorter stands from that character, so the shorter is quoted up to its
 * end, the longer from that character, and the refusal says which of the two ends there. An
 * attribute is named whole however long it is, and a word beside one is quoted from its start.
 */
function wordDifference(written: readonly LineWord[], holds: readonly LineWord[]): string {
  let before = 0;
  for (let index = 0; index < Math.max(written.length, holds.length); index += 1) {
    const given = written[index];
    const stored = holds[index];
    if (given !== undefined && stored !== undefined && given.text === stored.text) {
      before += Array.from(given.text).length;
      continue;
    }
    if (given === undefined && stored !== undefined) {
      return `ends after character ${before}, where Dispatch goes on with ${wordText(stored)}`;
    }
    if (given !== undefined && stored === undefined) {
      return `reads ${wordText(given)} at character ${before + 1}, where Dispatch ends the line`;
    }
    if (given !== undefined && stored !== undefined) {
      const wrote = Array.from(given.text);
      const held = Array.from(stored.text);
      let from = 0;
      if (
        !given.attribute &&
        !stored.attribute &&
        Math.max(wrote.length, held.length) > STORED_LINE_QUOTED
      ) {
        while (from < wrote.length && wrote[from] === held[from]) from += 1;
        const tail = Math.max(0, from - STORED_LINE_QUOTED);
        if (from === wrote.length) {
          return `ends ${wordText(given, tail, wrote)} after character ${before + from}, where Dispatch goes on with ${wordText(stored, from, held)}`;
        }
        if (from === held.length) {
          return `reads ${wordText(given, from, wrote)} at character ${before + from + 1}, where Dispatch ends ${wordText(stored, tail, held)}`;
        }
      }
      return `reads ${wordText(given, from, wrote)} at character ${before + from + 1}, where Dispatch stores ${wordText(stored, from, held)}`;
    }
  }
  return "differs from the line Dispatch stores";
}

/** The first attribute two opening lines of a typed block hold otherwise, by its name: one whose
 *  text differs, one Dispatch adds, or one it drops. Undefined when every attribute is the same
 *  in both, and when the given line names one attribute twice. */
function attributeDifference(
  line: number,
  given: HeadedOpener,
  stored: HeadedOpener
): string | undefined {
  const written = attributesByName(given.attributes);
  if (written === undefined) return undefined;
  for (const attribute of stored.attributes) {
    const text = written.get(attribute.name);
    if (text === undefined) return `Dispatch adds the attribute ${attribute.text} to line ${line}`;
    if (text !== attribute.text) {
      return `line ${line} holds the attribute ${text}, which Dispatch stores as ${attribute.text}`;
    }
  }
  const held = new Set(stored.attributes.map((attribute) => attribute.name));
  const dropped = given.attributes.find((attribute) => !held.has(attribute.name));
  return dropped === undefined
    ? undefined
    : `line ${line} holds the attribute ${dropped.text}, which Dispatch drops`;
}

/** How the line `wrote` and the stored line `held` first differ, `given` and `stored` being each
 *  read as a typed block's opening line (headedOpener), undefined for any other line. A typed
 *  block's opening line is read by its attributes first, then, with its attributes in the stored
 *  order, word by word. When only one of the two lines reads as an opener, both are cut as plain
 *  words: cut in two ways, the two lines would differ at a word the cuts made, not the text. */
function lineDifference(
  line: number,
  wrote: string,
  given: HeadedOpener | undefined,
  held: string,
  stored: HeadedOpener | undefined
): string {
  if (given === undefined || stored === undefined) {
    return `line ${line} ${wordDifference(lineWords(wrote, undefined), lineWords(held, undefined))}`;
  }
  const attribute = attributeDifference(line, given, stored);
  if (attribute !== undefined) return attribute;
  const reordered = inStoredOrder(given, stored);
  if (reordered !== undefined && reordered !== wrote) {
    const words = lineWords(reordered, headedOpener(reordered));
    return `line ${line}, with its attributes put in the order Dispatch stores, ${wordDifference(words, lineWords(held, stored))}`;
  }
  return `line ${line} ${wordDifference(lineWords(wrote, given), lineWords(held, stored))}`;
}

/**
 * The first line the store would write otherwise, which is the line a reader fixes first: a line
 * the store adds or drops, quoted (quotedStoredLine), or a changed line, by the word or the
 * attribute where it first differs (lineDifference). The line the store adds or drops is itself
 * the difference, so its quote cut after STORED_LINE_QUOTED characters hides nothing; a quote of
 * a changed line cut at a fixed length would hide a difference past the cut. A line whose
 * attributes alone stand in another order is passed over, since the upload puts them in
 * Dispatch's order once nothing else differs. Each line of a changed pair is read as a typed
 * block's opening line once, here.
 */
function firstStoredDifference(given: string, stored: string): string {
  const written = given.split("\n");
  const holds = stored.split("\n");
  // Each side is read as `string | undefined`, since a line past the end of its own text is
  // undefined and the strict packages that compile this file say so. The two are not equal
  // here, so at most one of them is undefined.
  for (let line = 0; line < Math.max(written.length, holds.length); line += 1) {
    const wrote = written[line];
    const held = holds[line];
    if (wrote === held) continue;
    if (wrote === undefined && held !== undefined) {
      return `the store writes a line ${line + 1}, ${quotedStoredLine(Array.from(held), 0)}`;
    }
    if (held === undefined && wrote !== undefined) {
      return `the store drops line ${line + 1}, ${quotedStoredLine(Array.from(wrote), 0)}`;
    }
    if (wrote !== undefined && held !== undefined) {
      const givenOpener = headedOpener(wrote);
      const storedOpener = headedOpener(held);
      if (onlyReordered(givenOpener, held, storedOpener)) continue;
      return lineDifference(line + 1, wrote, givenOpener, held, storedOpener);
    }
  }
  return "the stored text differs from the text given";
}

/**
 * How much of the stored text a refusal prints whole. Past it the refusal prints the first whole
 * lines that fit and says how much it cut: a document is up to 1 MiB, and a refusal nobody can
 * read to the end is one nobody acts on. The line a reader fixes first is named before the text
 * either way, so a cut refusal still says what to change.
 */
const STORED_TEXT_PRINTED = 2000;

/** The stored text as a refusal prints it, and what the refusal says of the cut: whole while it
 *  is short, and otherwise its first whole lines up to STORED_TEXT_PRINTED characters. */
function storedTextPrinted(stored: string): { readonly text: string; readonly cut: string } {
  if (stored.length <= STORED_TEXT_PRINTED) return { text: stored, cut: "" };
  const head = stored.slice(0, STORED_TEXT_PRINTED);
  const lastLineFeed = head.lastIndexOf("\n");
  const text = lastLineFeed === -1 ? head : head.slice(0, lastLineFeed + 1);
  return {
    text,
    cut: ` The text below is cut after ${text.length} of its ${stored.length} characters: change the line named above and send the text again.`,
  };
}

/**
 * The refusal of inline text the store would change: where the two first differ, the way out
 * this name has, and the text the store writes, so the writer can send exactly that text. The
 * markers hold the stored text apart from the prose around it. A name `--path` would store as
 * bytes has a second way out, which keeps the text as it was written; a Markdown name has none,
 * since `--path` parses and renders it the same way (`pathKeepsBytesOf`).
 */
function storedTextProblem(given: string, stored: string, name: string): string {
  const asBytes = pathKeepsBytesOf(name)
    ? ` To keep the text as you wrote it, send it as a file with ${PATH_KEEPS_BYTES}.`
    : "";
  const printed = storedTextPrinted(stored);
  return [
    `Dispatch stores this text under another spelling: ${firstStoredDifference(given, stored)}.`,
    ` Send the text Dispatch stores, which is between the two marker lines below.${asBytes}`,
    `${printed.cut}\n--- the text Dispatch stores ---\n${printed.text}`,
    "--- end of the text Dispatch stores ---",
  ].join("");
}

/** `text` without a final line feed, which is what the stored-text check compares. The store
 *  ends every document it holds with one, so text a writer gave without one comes back with one
 *  and nothing else changed (`markdown_round_trip_test.go` pins "one" stored as "one\n"). That
 *  one line feed is the only difference the check lets go up as written; a typed block's
 *  attributes in another order go up as Dispatch stores them (`attributesReordered`). */
function withoutFinalLineFeed(text: string): string {
  return text.endsWith("\n") ? text.slice(0, -1) : text;
}

/**
 * What an upload of the inline text `given` under the name `name` does, Dispatch's round trip
 * (POST /api/v1/markdown/round-trip) storing it as `stored`. It sends `given` when the two differ
 * in nothing but the final line feed the store adds (withoutFinalLineFeed). It sends `stored`,
 * with the answer's line for each block (attributesReordered), when they differ only in the
 * order of typed blocks' attributes. It is refused (storedTextProblem) for every other
 * difference.
 */
export function storedUpload(
  given: string,
  stored: string,
  name: string
): { readonly sent: string; readonly reordered: readonly string[] } | { readonly refusal: string } {
  if (withoutFinalLineFeed(stored) === withoutFinalLineFeed(given)) {
    return { sent: given, reordered: [] };
  }
  const reordered = attributesReordered(given, stored);
  return reordered === undefined
    ? { refusal: storedTextProblem(given, stored, name) }
    : { sent: stored, reordered };
}
