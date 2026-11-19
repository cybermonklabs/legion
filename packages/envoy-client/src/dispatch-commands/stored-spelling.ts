import { PATH_KEEPS_BYTES, pathKeepsBytesOf } from "@legion/contracts";

/** One attribute of a typed block's opening line as it is written (`#id`, `.class`, `name` or
 *  `name="value"`), under the name Dispatch reads it by: `id` for `#…` and `class` for `.…`. */
interface WrittenAttribute {
  readonly name: string;
  readonly text: string;
}

/**
 * A typed block's opening line, cut where the attribute-order check and the refusal read it:
 * `head` up to and with its `{` (a quote or list marker before the fence, the fence, the type),
 * each attribute as written, and the whitespace around them (`gaps`, one more than the
 * attributes). The line ends with the `}` after them.
 */
interface TypedOpener {
  readonly head: string;
  readonly type: string;
  readonly attributes: readonly WrittenAttribute[];
  readonly gaps: readonly string[];
}

/** A typed block's opening line: three colons or more, the type, then its attributes between
 *  `{` and the `}` that ends the line (`renderTypedAttributes` in pmdoc's schema.go writes it). */
const TYPED_OPENER = /^(.*?:{3,}([A-Za-z0-9_-]+)\{)(.*)\}$/;

/** The whitespace between two attributes, as Go's `unicode.IsSpace` reads an ASCII byte. */
const ATTRIBUTE_SPACE = /[\t-\r ]/;

/** `line` read as a typed block's opening line, or undefined for any other line and for one
 *  whose quoted value never closes. A quoted value runs to its closing quote, past a quote a
 *  backslash escapes, so a space inside it splits no attribute. */
function typedOpener(line: string): TypedOpener | undefined {
  const match = TYPED_OPENER.exec(line);
  if (match === null) return undefined;
  const [, head = "", type = "", inside = ""] = match;
  const attributes: WrittenAttribute[] = [];
  const gaps: string[] = [];
  let at = 0;
  for (;;) {
    const gap = at;
    while (at < inside.length && ATTRIBUTE_SPACE.test(inside.charAt(at))) at += 1;
    gaps.push(inside.slice(gap, at));
    if (at === inside.length) break;
    const start = at;
    while (at < inside.length && !ATTRIBUTE_SPACE.test(inside.charAt(at))) {
      if (inside.charAt(at) !== '"') {
        at += 1;
        continue;
      }
      at += 1;
      while (at < inside.length && inside.charAt(at) !== '"') {
        at += inside.charAt(at) === "\\" ? 2 : 1;
      }
      if (at >= inside.length) return undefined;
      at += 1;
    }
    const text = inside.slice(start, at);
    const name = text.startsWith("#")
      ? "id"
      : text.startsWith(".")
        ? "class"
        : (text.split("=", 1)[0] ?? text);
    attributes.push({ name, text });
  }
  return { head, type, attributes, gaps };
}

/** The names of `attributes` as a map to each one's written text, or undefined when a name
 *  stands twice, since such a line can be put in no one order. */
function attributesByName(
  attributes: readonly WrittenAttribute[]
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
function inStoredOrder(given: TypedOpener, stored: TypedOpener): string | undefined {
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

/** The stored opening line `held` read as a typed block's opener, when the written line `wrote`
 *  differs from it only in the order of its attributes: `wrote` with them put in `held`'s order
 *  is `held`. Undefined for every other pair of lines. */
function storedInOtherOrder(wrote: string, held: string): TypedOpener | undefined {
  const given = typedOpener(wrote);
  const stored = typedOpener(held);
  if (given === undefined || stored === undefined) return undefined;
  return inStoredOrder(given, stored) === held ? stored : undefined;
}

/** A typed block whose attributes the upload put in the order Dispatch stores: its opening line
 *  (counted from 1) and that line as Dispatch stores it. */
export interface ReorderedBlock {
  readonly line: number;
  readonly opener: TypedOpener;
}

/**
 * The typed blocks whose attributes `given` writes in another order than `stored`, the text
 * Dispatch's own round trip (POST /api/v1/markdown/round-trip) answers, when that is the whole
 * difference between the two: `given` with each such opener's attributes put in the stored
 * opener's order is `stored`, but for the final line feed the store adds. Undefined when anything
 * else differs. An attribute's position carries no meaning, so such a text is stored in
 * Dispatch's order rather than refused, and the order is Dispatch's render's, never a copy of it.
 */
export function attributesReordered(given: string, stored: string): ReorderedBlock[] | undefined {
  const written = withoutFinalLineFeed(given).split("\n");
  const holds = withoutFinalLineFeed(stored).split("\n");
  if (written.length !== holds.length) return undefined;
  const reordered: ReorderedBlock[] = [];
  for (const [index, held] of holds.entries()) {
    const wrote = written[index];
    if (wrote === held) continue;
    const opener = wrote === undefined ? undefined : storedInOtherOrder(wrote, held);
    if (opener === undefined) return undefined;
    reordered.push({ line: index + 1, opener });
  }
  return reordered;
}

/** What the answer of an upload says of a block whose attributes it put in Dispatch's order: the
 *  block by its type and id, its line, and that order. */
export function reorderedBlockLine({ line, opener }: ReorderedBlock): string {
  const id = opener.attributes.find((attribute) => attribute.name === "id")?.text;
  const named =
    id === undefined
      ? `${opener.type} block`
      : `${opener.type} block ${id.startsWith("#") ? id.slice(1) : id.slice('id="'.length, -1)}`;
  const order = opener.attributes.map((attribute) => attribute.name).join(", ");
  return `This upload put the attributes of ${named} (line ${line}) in the order Dispatch stores: ${order}.`;
}

/** One word of a line as the refusal names it: an attribute of a typed block's opening line, or
 *  a run of other characters or of whitespace. */
interface LineWord {
  readonly text: string;
  readonly attribute: boolean;
}

/** `line` cut into its words. The words joined are the line. */
function lineWords(line: string): LineWord[] {
  const plain = (text: string): LineWord[] =>
    (text.match(/\s+|\S+/g) ?? []).map((word) => ({ text: word, attribute: false }));
  const opener = typedOpener(line);
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

/** A word as the refusal names it, whole: an attribute as it is written, any other word quoted. */
function wordText(word: LineWord): string {
  if (word.attribute) return `the attribute ${word.text}`;
  return `${/\S/.test(word.text) ? "the word" : "the space"} ${JSON.stringify(word.text)}`;
}

/** Where two different lines first differ, by the word that differs, named whole in each, and
 *  the character it starts at (code points, counted from 1). */
function wordDifference(wrote: string, held: string): string {
  const written = lineWords(wrote);
  const holds = lineWords(held);
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
      return `reads ${wordText(given)} at character ${before + 1}, where Dispatch stores ${wordText(stored)}`;
    }
  }
  return "differs from the line Dispatch stores";
}

/** The first attribute two opening lines of a typed block hold otherwise, by its name: one whose
 *  text differs, one Dispatch adds, or one it drops. Undefined when every attribute is the same
 *  in both, and when the given line names one attribute twice. */
function attributeDifference(
  line: number,
  given: TypedOpener,
  stored: TypedOpener
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

/** How the line `wrote` and the stored line `held` first differ. A typed block's opening line is
 *  read by its attributes first, then, with its attributes in the stored order, word by word. */
function lineDifference(line: number, wrote: string, held: string): string {
  const given = typedOpener(wrote);
  const stored = typedOpener(held);
  if (given !== undefined && stored !== undefined) {
    const attribute = attributeDifference(line, given, stored);
    if (attribute !== undefined) return attribute;
    const reordered = inStoredOrder(given, stored);
    if (reordered !== undefined && reordered !== wrote) {
      return `line ${line}, with its attributes put in the order Dispatch stores, ${wordDifference(reordered, held)}`;
    }
  }
  return `line ${line} ${wordDifference(wrote, held)}`;
}

/**
 * The first line the store would write otherwise, which is the line a reader fixes first: a line
 * the store adds or drops, quoted whole, or a changed line, by the word or the attribute where it
 * first differs, named whole in each spelling. A quote of the line cut at a fixed length would
 * hide a difference past the cut. A line whose attributes alone stand in another order is passed
 * over, since the upload puts them in Dispatch's order once nothing else differs.
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
      return `the store writes a line ${line + 1}, ${JSON.stringify(held)}`;
    }
    if (held === undefined && wrote !== undefined) {
      return `the store drops line ${line + 1}, ${JSON.stringify(wrote)}`;
    }
    if (wrote !== undefined && held !== undefined) {
      if (storedInOtherOrder(wrote, held) !== undefined) continue;
      return lineDifference(line + 1, wrote, held);
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
export function storedTextProblem(given: string, stored: string, name: string): string {
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
export function withoutFinalLineFeed(text: string): string {
  return text.endsWith("\n") ? text.slice(0, -1) : text;
}
