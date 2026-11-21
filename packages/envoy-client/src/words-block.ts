/**
 * A words block: the words of another author, drawn after a text's fields in a block of their
 * own. The block names the author and the time where the fields of the text do not, nothing else
 * shares it, and nothing else in the text can open or close an element of its tag. The Legion
 * daemon draws the blocks of its texts by the same rule (`Render`, and `InFront` for notes put in
 * front of a text, `packages/daemon/internal/texts`), so one reader reads the blocks of a card and
 * of a daemon text:
 *
 *     <answer by="user:sami" at="2026-11-14T22:44:10Z">
 *     Use a structured message here, not prose.
 *     </answer>
 *
 * The tag of a block is the first of `name`, `name-1`, `name-2` and on, where `name` is the name
 * of the field that holds the words, such that neither the text before the blocks nor the words
 * of any block of the text hold its opening tag `<tag` or its closing tag `</tag`, and no earlier
 * block of the text took it. The text before the blocks is a card's fields, which hold other
 * authors' words too: an ask's question, an issue's title. So nothing in the text opens or closes
 * the tag of a block but that block's own lines. The test is on the plain text, so text that
 * holds `<answers` or `<answer-12` also moves the tag of `answer` on, past `answer-1` and
 * `answer-12` too. The opening tag carries the attributes `by`, `at`, `field`, `rest` and `more`,
 * in that order, each only where it is set. Each value escapes `&`, `<`, `>`, `"` and each control
 * character below U+0020 as an XML character reference, so no value ends its attribute or its
 * tag. A new line, the words exactly as given, a new line and the closing tag follow the opening
 * tag, and one new line joins a block to the next.
 */

/** The words of one block, and what its opening tag says of them. */
export interface Words {
  /** The words exactly as their event carried them. */
  readonly text: string;
  /** The author, where the fields of the text do not name the author. */
  readonly by?: string | undefined;
  /** The time of the act, as its source wrote it, where the fields of the text do not give it. */
  readonly at?: string | undefined;
  /** The path of the words in their payload, such as `kept[1].answer`, where they are a field
   *  inside a list or an object of the payload. */
  readonly field?: string | undefined;
  /** The count of UTF-16 units that a bound cut off the words. */
  readonly rest?: number | undefined;
  /** The event marked its text as cut, so more follows and the count of the rest is not known. */
  readonly more?: boolean | undefined;
}

/** One block of a text: the name of the field that holds the words, and the words. */
export interface NamedWords {
  /** A name a tag can have (`TAG_NAME`): a letter or `_`, then letters, digits and `_`. */
  readonly name: string;
  readonly words: Words;
}

/** One block as a reader found it. */
export interface ReadBlock extends Words {
  /** The tag as written: the name, or the name with the suffix the words made it take. */
  readonly tag: string;
  /** The name of the field that holds the words. */
  readonly name: string;
}

/** A text split at its first block. */
export interface SplitText {
  /** All of the text before its first block, less the new line that joins it to the block. For
   *  a card with fields and blocks this is TOON that TOON's `decode` reads. The whole text where
   *  it holds no block: a card with a diff after its fields, and a card that is one sentence, are
   *  returned whole. */
  readonly head: string;
  /** The blocks that end the text, in their order. */
  readonly blocks: readonly ReadBlock[];
}

/** The blocks of one text, in their order, each joined to the next by one new line, that follow
 *  `before`: the whole of the text ahead of them, such as a card's fields, which the caller joins
 *  to the first block with one new line. Each block takes the tag the rule gives it against
 *  `before` and the words of every block (`blockTags`). A block whose name no tag can have
 *  (`TAG_NAME`) throws, since what a text holds of tags is read as the runs of the characters of
 *  a tag's name (`HeldTags`). */
export function wordsBlocks(before: string, blocks: readonly NamedWords[]): string {
  for (const { name } of blocks) {
    if (!TAG_NAME.test(name))
      throw new Error(`a block's name is no tag name: ${JSON.stringify(name)}`);
  }
  const tags = blockTags(
    new HeldTags([before]),
    [],
    blocks.map(({ name, words }) => ({ name, text: words.text }))
  );
  return blocks
    .map(({ words }, index) => {
      let attributes = "";
      if (words.by !== undefined && words.by !== "") attributes += attribute("by", words.by);
      if (words.at !== undefined && words.at !== "") attributes += attribute("at", words.at);
      if (words.field !== undefined && words.field !== "") {
        attributes += attribute("field", words.field);
      }
      if (words.rest !== undefined && words.rest > 0) {
        attributes += attribute("rest", String(words.rest));
      }
      if (words.more === true) attributes += attribute("more", "true");
      return drawBlock(tags[index] ?? "", attributes, words.text);
    })
    .join("\n");
}

/**
 * `parts` and then `text`, each joined to the next by a blank line: what a host puts in front of
 * a card it hands on (a line that says where messages came from, a label, the messages before
 * it), and the card. A part can hold another author's words, so the blocks of `text`
 * (`drawnBlocks`) are drawn again, each with its attributes as written and its words as they
 * were, under the tag the rule gives it against the whole of what is sent: the parts, the text of
 * `text` before and after its blocks, and the words of every block. Nothing in front of a block
 * then opens or closes its tag, as nothing does in the card alone (`wordsBlocks`). Where neither
 * the parts nor the text after the blocks hold a tag the blocks could take, each block keeps its
 * tag and the result is the parts and `text` joined as they are; so is a text with no drawn
 * block. The Legion daemon puts notes in front of a task by the same rule (`InFront`,
 * `packages/daemon/internal/texts/words.go`). A host that puts several cards one after another
 * builds the whole with `TextInFront`, which gives each card the text this gives it.
 */
export function inFront(parts: readonly string[], text: string): string {
  const [first, ...rest] = parts;
  if (first === undefined) return text;
  const sent = new TextInFront(first);
  for (const part of rest) sent.part(part);
  sent.card(text);
  return sent.text();
}

/**
 * A text a host builds of parts and cards, each joined to the one before by a blank line: the
 * Codex inbox's hook text, claude-envoy's prompt at the rows a turn never read. A part is joined
 * as it is. A card is joined as `inFront` joins it behind the whole text before it, every earlier
 * card as it was drawn there included, so the text is byte for byte the one that joining each
 * part and taking `inFront([text so far], card)` for each card gives.
 *
 * What that draw scans the text before a card for is the tags the card's blocks could take, and
 * all that a part or a card adds to it is its own text. So the text keeps what it holds of tags
 * (`HeldTags`): each part and card is scanned once, when it is joined, whatever the names of the
 * blocks after it, and each block's tag is a lookup of its name in what was kept. The whole is
 * drawn in time and memory linear in its length however many names its cards' blocks have and
 * however long a run of a tag name's characters it holds, where taking `inFront` of the text so
 * far for each card scans all of it again at every card, every earlier card's words included.
 */
export class TextInFront {
  readonly #held: HeldTags;

  /** A text that starts with `first`, as it is. */
  constructor(first: string) {
    this.#held = new HeldTags([first]);
  }

  /** Joins `text` after a blank line, as it is. */
  part(text: string): void {
    this.#held.add(`\n\n${text}`);
  }

  /** Joins `text`, a card, after a blank line, with its blocks (`drawnBlocks`) drawn again, each
   *  with its attributes as written and its words as they were, under the tag the rule gives it
   *  against the whole text before the card and the card (`inFront`). */
  card(text: string): void {
    const found = drawnBlocks(text);
    const first = found[0];
    const last = found[found.length - 1];
    if (first === undefined || last === undefined) {
      this.part(text);
      return;
    }
    const head = text.slice(0, first.start);
    const tail = text.slice(last.end);
    const tags = blockTags(this.#held, [head, tail], found);
    const blocks = found.map(({ attributes, text: words }, index) =>
      drawBlock(tags[index] ?? "", attributes, words)
    );
    this.part(`${head}${blocks.join("\n")}${tail}`);
  }

  /** The text joined so far. */
  text(): string {
    return this.#held.texts.join("");
  }
}

/** One block of a text as `drawnBlocks` found it: where its opening line starts and where its
 *  closing tag ends, the name of the field that holds the words (its tag without the `-n` a step
 *  past held tags added), the attributes of its opening tag as written, and its words. */
interface DrawnBlock {
  readonly start: number;
  readonly end: number;
  readonly name: string;
  readonly attributes: string;
  readonly text: string;
}

/**
 * The blocks of `text`, a card `wordsBlocks` drew, in their order. No line of a card ahead of its
 * blocks starts with `<`, since its fields are TOON lines, so its first such line opens its first
 * block. A block closes on the first line after its opening that is its closing tag, since no
 * words of a block hold that tag (`blockTags`), and the next block opens on the line after. The
 * run ends at the first line that opens no block. It is empty where the first line that starts
 * with `<` opens none, or opens a block that never closes, as in a text no card drew, which
 * `inFront` then joins as it is. One pass over `text`.
 */
function drawnBlocks(text: string): DrawnBlock[] {
  let at = 0;
  if (!text.startsWith("<")) {
    const line = text.indexOf("\n<");
    if (line < 0) return [];
    at = line + 1;
  }
  const blocks: DrawnBlock[] = [];
  while (text[at] === "<") {
    const opened = text.indexOf("\n", at);
    if (opened < 0) break;
    const opening = OPENING.exec(text.slice(at, opened));
    if (opening === null) break;
    const name = opening[1] ?? "";
    const closing = `\n</${opening[2] === undefined ? name : `${name}-${opening[2]}`}>`;
    const closed = text.indexOf(closing, opened + 1);
    if (closed < 0) break;
    const end = closed + closing.length;
    if (end < text.length && text[end] !== "\n") break;
    blocks.push({
      start: at,
      end,
      name,
      attributes: opening[3] ?? "",
      text: text.slice(opened + 1, closed),
    });
    at = end + 1;
  }
  return blocks;
}

/** The block of `text` under `tag`: the opening tag with `attributes` as written, a new line, the
 *  words, a new line and the closing tag. */
function drawBlock(tag: string, attributes: string, text: string): string {
  return `<${tag}${attributes}>\n${text}\n</${tag}>`;
}

/**
 * A text in pieces, and what it holds of tags: for each name, whether it holds `<name` or
 * `</name`, and each n for which it holds `<name-n` or `</name-n`. A tag's name is a run of the
 * characters [A-Za-z0-9_], so the text holds `<name` or `</name` where the name starts the
 * longest run of them that follows a `<` or a `</`, and `<name-n` or `</name-n` where that run is
 * the name and no more and is followed by `-` and digits that start with the digits of n, with no
 * leading zero: `<answer-12` holds the tags of `a`, `an` and on to `answer`, and `answer-1` and
 * `answer-12`. Each piece is scanned once, when it is added, whatever the names asked of after,
 * and each run it holds is kept as where it is, at the root of a trie of the runs' characters
 * (`HeldName`). A name asked of walks the trie, and each node it passes hands the runs it keeps on
 * to the node of their next character; a run that ends at a node adds its suffixes to it there.
 * So a run goes down the trie only as far as a name asked of shares its characters, never past
 * the longest such name, and a run no name starts as builds nothing: the trie holds a node for a
 * character of a run only where a name asked of reached it. That keeps the memory, and the time
 * of every walk together, linear in the pieces' length, however long a run is. A piece is scanned
 * on its own, which finds what the joined text holds, since a tag holds no new line.
 */
class HeldTags {
  readonly texts: string[] = [];
  readonly #runs: HeldName = { next: undefined, runs: undefined, suffixes: undefined, least: 0 };

  constructor(texts: readonly string[]) {
    for (const text of texts) this.add(text);
  }

  /** Adds `text` after the pieces, and the runs it holds. One pass over `text`. */
  add(text: string): void {
    const piece = this.texts.push(text) - 1;
    for (let at = text.indexOf("<"); at !== -1; at = text.indexOf("<", at)) {
      at++;
      if (text.charCodeAt(at) === SLASH) at++;
      const start = at;
      while (at < text.length && isNameUnit(text.charCodeAt(at))) at++;
      if (at === start) continue;
      this.#runs.runs ??= [];
      this.#runs.runs.push(piece, start, at);
    }
  }

  /** What the text holds of the tags of `name`: undefined where it holds none. */
  of(name: string): HeldName | undefined {
    let held: HeldName | undefined = this.#runs;
    for (let depth = 0; held !== undefined && depth < name.length; depth++) {
      this.#handOn(held, depth);
      held = held.next?.get(name.charCodeAt(depth));
    }
    return held === this.#runs ? undefined : held;
  }

  /** Hands each run that `held`, the node at `depth`, keeps on to the node of the run's character
   *  at `depth`, which it builds where there is none: a run that ends at that node adds its
   *  suffixes to it, and any other is kept there. */
  #handOn(held: HeldName, depth: number): void {
    const runs = held.runs;
    if (runs === undefined) return;
    held.runs = undefined;
    held.next ??= new Map();
    for (let index = 0; index < runs.length; index += 3) {
      const piece = runs[index] ?? 0;
      const start = runs[index + 1] ?? 0;
      const end = runs[index + 2] ?? 0;
      const text = this.texts[piece] ?? "";
      const unit = text.charCodeAt(start + depth);
      let next = held.next.get(unit);
      if (next === undefined) {
        next = { next: undefined, runs: undefined, suffixes: undefined, least: 1 };
        held.next.set(unit, next);
      }
      if (start + depth + 1 < end) {
        next.runs ??= [];
        next.runs.push(piece, start, end);
      } else holdSuffixes(next, text, end);
    }
  }
}

/** A node of the trie of `HeldTags`: the name that a run the text holds starts with, whose own
 *  tag, suffix 0, the text therefore holds. */
interface HeldName {
  /** The node of each character that follows the name in a run handed on past this node. */
  next: Map<number, HeldName> | undefined;
  /** The runs longer than the name that start with it and that no walk has handed on yet: for
   *  each, its piece, where it starts and where it ends, one after another. */
  runs: number[] | undefined;
  /** Each n of a `-n` that follows a run that is the name and no more. */
  suffixes: Set<number> | undefined;
  /** A suffix every one below which the text holds: one at least, since it holds suffix 0. */
  least: number;
}

/** Adds to `held` the suffix of each tag of its name that `text` holds after a run that is the
 *  name and no more and ends at `end`: each n whose digits begin the digits after a `-` there. */
function holdSuffixes(held: HeldName, text: string, end: number): void {
  if (text.charCodeAt(end) !== DASH) return;
  let suffix = 0;
  for (let index = end + 1; index < text.length; index++) {
    const digit = text.charCodeAt(index) - 48;
    // A tag's suffix has no leading zero, so `-0` holds none.
    if (digit < 0 || digit > 9 || (suffix === 0 && digit === 0)) break;
    suffix = suffix * 10 + digit;
    if (suffix > SUFFIX_CAP) break;
    held.suffixes ??= new Set();
    held.suffixes.add(suffix);
  }
}

/** `/`, which a closing tag's `</` holds. */
const SLASH = 0x2f;
/** `-`, which joins a tag's suffix to its name. */
const DASH = 0x2d;

/** Whether `unit` is a character a tag's name holds: [A-Za-z0-9_]. */
function isNameUnit(unit: number): boolean {
  return (
    (unit >= 0x30 && unit <= 0x39) ||
    (unit >= 0x41 && unit <= 0x5a) ||
    (unit >= 0x61 && unit <= 0x7a) ||
    unit === 0x5f
  );
}

/** Whether `held`, what a text holds of the tags of one name, holds the tag of `suffix`. */
function holds(held: HeldName | undefined, suffix: number): boolean {
  return held !== undefined && (suffix === 0 || held.suffixes?.has(suffix) === true);
}

/** The least suffix whose tag `held`, what a text holds of the tags of one name, may not hold:
 *  it holds the tag of every suffix below this one. */
function leastFree(held: HeldName | undefined): number {
  if (held === undefined) return 0;
  while (held.suffixes?.has(held.least) === true) held.least++;
  return held.least;
}

/** The tag of each of `blocks`, the blocks of one text in their order, behind `front`, the text
 *  in front of that one, where `beside` is the rest of that text: the first of `name`, `name-1`,
 *  `name-2` and on whose `<tag` and `</tag` neither `front`, nor a text of `beside`, nor the
 *  words of any block hold, and that no earlier block took. Each text here is scanned once, and
 *  each block's tag is a lookup of its name in what `front` and these texts hold. */
function blockTags(
  front: HeldTags,
  beside: readonly string[],
  blocks: readonly { readonly name: string; readonly text: string }[]
): string[] {
  const here = new HeldTags([...beside, ...blocks.map(({ text }) => text)]);
  // For each name, what the text in front and the texts here hold of its tags, the suffixes a
  // block took, and the least that may be free: every suffix below it is held, in front or here,
  // or taken.
  const named = new Map<
    string,
    {
      readonly ahead: HeldName | undefined;
      readonly held: HeldName | undefined;
      readonly taken: Set<number>;
      next: number;
    }
  >();
  return blocks.map(({ name }) => {
    let own = named.get(name);
    if (own === undefined) {
      const ahead = front.of(name);
      const held = here.of(name);
      own = { ahead, held, taken: new Set(), next: Math.max(leastFree(ahead), leastFree(held)) };
      named.set(name, own);
    }
    while (holds(own.ahead, own.next) || holds(own.held, own.next) || own.taken.has(own.next)) {
      own.next++;
    }
    const suffix = own.next;
    own.taken.add(suffix);
    return suffix === 0 ? name : `${name}-${suffix}`;
  });
}

/** The largest suffix a scan keeps. A block takes the least suffix that is neither held nor
 *  taken, so never one above the count of those held and taken, and each suffix held takes a
 *  unit of the text and each one taken a block. No text comes near this many units, so no suffix
 *  a block could take lies past it, and stopping here keeps every number exact. */
const SUFFIX_CAP = Number.MAX_SAFE_INTEGER;

/** One attribute of an opening tag, with its value escaped. */
function attribute(key: string, value: string): string {
  let escaped = "";
  for (let index = 0; index < value.length; index++) {
    const unit = value.charCodeAt(index);
    switch (value[index]) {
      case "&":
        escaped += "&amp;";
        break;
      case "<":
        escaped += "&lt;";
        break;
      case ">":
        escaped += "&gt;";
        break;
      case '"':
        escaped += "&quot;";
        break;
      default:
        escaped += unit < 0x20 ? `&#${unit};` : value[index];
    }
  }
  return ` ${key}="${escaped}"`;
}

/** A name a tag can have, which the tag of an opening line (`OPENING`) starts with. */
const TAG_NAME = /^[A-Za-z_][A-Za-z0-9_]*$/;
const OPENING = /^<([A-Za-z_][A-Za-z0-9_]*)(?:-([1-9][0-9]*))?((?: [a-z]+="[^"<>]*")*)>$/;
const ATTRIBUTE = / ([a-z]+)="([^"<>]*)"/g;
const ATTRIBUTE_ORDER = ["by", "at", "field", "rest", "more"] as const;
const REFERENCE = /&(?:(amp|lt|gt|quot)|#([0-9]+));/y;
const NAMED_REFERENCES: Readonly<Record<string, string>> = {
  amp: "&",
  lt: "<",
  gt: ">",
  quot: '"',
};

/**
 * `text` split into the part before its first block and the blocks that end it. The first block
 * opens at the first line that is an opening tag. From there the text holds blocks alone, each
 * joined to the next by one new line, to its end. A block closes at the first closing tag of its
 * tag after its opening, since no words of the text hold one. Each block's tag is the tag the
 * rule gives it against the text before the blocks and the words of every block (`blockTags`),
 * and its attributes are the rule's, in the rule's order. A text that breaks one of these throws
 * an error that names the break.
 */
export function splitWordsBlocks(text: string): SplitText {
  let start = -1;
  for (let line = 0; ; ) {
    const end = lineEnd(text, line);
    if (OPENING.test(text.slice(line, end))) {
      start = line;
      break;
    }
    if (end === text.length) break;
    line = end + 1;
  }
  if (start < 0) return { head: text, blocks: [] };

  const blocks: ReadBlock[] = [];
  for (let line = start; ; ) {
    const end = lineEnd(text, line);
    const opening = OPENING.exec(text.slice(line, end));
    if (opening === null) {
      throw new Error(`text after a block is no block: ${JSON.stringify(text.slice(line, end))}`);
    }
    const name = opening[1] ?? "";
    const tag = opening[2] === undefined ? name : `${name}-${opening[2]}`;
    if (end === text.length) throw new Error(`block <${tag}> ends at its opening tag`);
    const words = end + 1;
    const close = text.indexOf(`</${tag}`, words);
    if (close <= words || text[close - 1] !== "\n" || !text.startsWith(`</${tag}>`, close)) {
      throw new Error(`block <${tag}> has no closing tag on a line of its own`);
    }
    const after = close + tag.length + 3;
    if (after < text.length && text[after] !== "\n") {
      throw new Error(`block <${tag}> has text after its closing tag on the same line`);
    }
    const said = text.slice(words, close - 1);
    blocks.push({ tag, name, text: said, ...attributes(tag, opening[3] ?? "") });
    if (after >= text.length) break;
    line = after + 1;
  }
  const head = start === 0 ? "" : text.slice(0, start - 1);
  const tags = blockTags(new HeldTags([head]), [], blocks);
  for (const [index, { tag }] of blocks.entries()) {
    if (tags[index] !== tag) {
      throw new Error(`block <${tag}> has a tag the rule does not give it: <${tags[index]}>`);
    }
  }
  return { head, blocks };
}

/** The end of the line that starts at `start`: the index of its new line, or the text's end. */
function lineEnd(text: string, start: number): number {
  const end = text.indexOf("\n", start);
  return end < 0 ? text.length : end;
}

/** The attributes of the opening tag of `tag`, read from `written`. */
function attributes(tag: string, written: string): Omit<Words, "text"> {
  const read: { by?: string; at?: string; field?: string; rest?: number; more?: boolean } = {};
  let last = -1;
  for (const [, key = "", raw = ""] of written.matchAll(ATTRIBUTE)) {
    const place = ATTRIBUTE_ORDER.indexOf(key as (typeof ATTRIBUTE_ORDER)[number]);
    if (place < 0) throw new Error(`block <${tag}> has an attribute the rule has not: ${key}`);
    if (place <= last) throw new Error(`block <${tag}> has its attributes out of order`);
    last = place;
    const value = unescaped(tag, raw);
    switch (key) {
      case "rest":
        if (!/^[1-9][0-9]*$/.test(value)) throw new Error(`block <${tag}> has rest="${value}"`);
        read.rest = Number(value);
        break;
      case "more":
        if (value !== "true") throw new Error(`block <${tag}> has more="${value}"`);
        read.more = true;
        break;
      case "by":
      case "at":
      case "field":
        if (value === "") throw new Error(`block <${tag}> has an empty ${key}`);
        read[key] = value;
        break;
    }
  }
  return read;
}

/** An attribute's value with its character references read back. */
function unescaped(tag: string, raw: string): string {
  let value = "";
  for (let index = 0; index < raw.length; ) {
    const char = raw[index] ?? "";
    if (char.charCodeAt(0) < 0x20) {
      throw new Error(`block <${tag}> has a control character in an attribute`);
    }
    if (char !== "&") {
      value += char;
      index++;
      continue;
    }
    REFERENCE.lastIndex = index;
    const reference = REFERENCE.exec(raw);
    if (reference === null) throw new Error(`block <${tag}> has an & that starts no reference`);
    const named = reference[1];
    value +=
      named === undefined ? String.fromCharCode(Number(reference[2])) : NAMED_REFERENCES[named];
    index += reference[0].length;
  }
  return value;
}
