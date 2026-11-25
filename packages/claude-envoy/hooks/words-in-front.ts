/**
 * `inFront` and `TextInFront` of `@legion/envoy-client/words-block`, written a second time for the
 * mod's prompt at the rows a turn never read (`unreadRowPrompt`, `./mod-protocol.ts`). A mod
 * imports nothing from outside `hooks/` (`./mod-protocol.ts` says why), so it cannot take the
 * shared ones; `../tests/words-in-front.test.ts` checks that the two give the same text for the
 * same input. Change the two together. Like `./mod-protocol.ts`, this file stays import-free:
 * plain functions and classes over their arguments that read no global.
 */

/**
 * `parts` and then `text`, each joined to the next by a blank line: the lines and the messages
 * the prompt puts in front of a card, and the card. A card draws another author's words after its
 * fields, each in a block of their own under a tag that nothing else in the card opens or closes.
 * A part can hold another author's words too, so the blocks of `text` (`drawnBlocks`) are drawn
 * again, each with its attributes as written and its words as they were, under the tag the rule
 * gives it against the whole of what is sent: the first of its name, `name-1`, `name-2` and on
 * that neither a part, nor the text of `text` before and after its blocks, nor the words of any
 * block hold as `<tag` or `</tag`, and that no earlier block took. Where neither the parts nor the
 * text after the blocks hold a tag the blocks could take, each block keeps its tag and the result
 * is the parts and `text` joined as they are; so is a text with no drawn block. A prompt of
 * several cards is built with `TextInFront`, which gives each card the text this gives it.
 */
export function inFront(parts: readonly string[], text: string): string {
  const [first, ...rest] = parts
  if (first === undefined) return text
  const sent = new TextInFront(first)
  for (const part of rest) sent.part(part)
  sent.card(text)
  return sent.text()
}

/**
 * A text built of parts and cards, each joined to the one before by a blank line: the prompt at
 * the rows a turn never read. A part is joined as it is. A card is joined as `inFront` joins it
 * behind the whole text before it, every earlier card as it was drawn there included, so the text
 * is byte for byte the one that joining each part and taking `inFront([text so far], card)` for
 * each card gives.
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
  readonly #held: HeldTags

  /** A text that starts with `first`, as it is. */
  constructor(first: string) {
    this.#held = new HeldTags([first])
  }

  /** Joins `text` after a blank line, as it is. */
  part(text: string): void {
    this.#held.add(`\n\n${text}`)
  }

  /** Joins `text`, a card, after a blank line, with its blocks (`drawnBlocks`) drawn again, each
   *  with its attributes as written and its words as they were, under the tag the rule gives it
   *  against the whole text before the card and the card (`inFront`). */
  card(text: string): void {
    const found = drawnBlocks(text)
    const first = found[0]
    const last = found[found.length - 1]
    if (first === undefined || last === undefined) {
      this.part(text)
      return
    }
    const head = text.slice(0, first.start)
    const tail = text.slice(last.end)
    const tags = blockTags(this.#held, [head, tail], found)
    const blocks = found.map(
      ({ attributes, text: words }, index) =>
        `<${tags[index] ?? ""}${attributes}>\n${words}\n</${tags[index] ?? ""}>`,
    )
    this.part(`${head}${blocks.join("\n")}${tail}`)
  }

  /** The text joined so far. */
  text(): string {
    return this.#held.texts.join("")
  }
}

/** The opening line of a block as a card draws it: the tag, then each attribute, whose value
 *  holds no double quote and no angle bracket. */
const OPENING = /^<([A-Za-z_][A-Za-z0-9_]*)(?:-([1-9][0-9]*))?((?: [a-z]+="[^"<>]*")*)>$/

/** One block of a text as `drawnBlocks` found it: where its opening line starts and where its
 *  closing tag ends, the name of the field that holds the words (its tag without the `-n` a step
 *  past held tags added), the attributes of its opening tag as written, and its words. */
interface DrawnBlock {
  readonly start: number
  readonly end: number
  readonly name: string
  readonly attributes: string
  readonly text: string
}

/**
 * The blocks of `text`, a card, in their order. No line of a card ahead of its blocks starts with
 * `<`, since its fields are TOON lines, so its first such line opens its first block. A block
 * closes on the first line after its opening that is its closing tag, since no words of a block
 * hold that tag, and the next block opens on the line after. The run ends at the first line that
 * opens no block. It is empty where the first line that starts with `<` opens none, or opens a
 * block that never closes, as in a text no card drew, which `inFront` then joins as it is. One
 * pass over `text`.
 */
function drawnBlocks(text: string): DrawnBlock[] {
  let at = 0
  if (!text.startsWith("<")) {
    const line = text.indexOf("\n<")
    if (line < 0) return []
    at = line + 1
  }
  const blocks: DrawnBlock[] = []
  while (text[at] === "<") {
    const opened = text.indexOf("\n", at)
    if (opened < 0) break
    const opening = OPENING.exec(text.slice(at, opened))
    if (opening === null) break
    const name = opening[1] ?? ""
    const closing = `\n</${opening[2] === undefined ? name : `${name}-${opening[2]}`}>`
    const closed = text.indexOf(closing, opened + 1)
    if (closed < 0) break
    const end = closed + closing.length
    if (end < text.length && text[end] !== "\n") break
    blocks.push({
      start: at,
      end,
      name,
      attributes: opening[3] ?? "",
      text: text.slice(opened + 1, closed),
    })
    at = end + 1
  }
  return blocks
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
  readonly texts: string[] = []
  readonly #runs: HeldName = { next: undefined, runs: undefined, suffixes: undefined, least: 0 }

  constructor(texts: readonly string[]) {
    for (const text of texts) this.add(text)
  }

  /** Adds `text` after the pieces, and the runs it holds. One pass over `text`. */
  add(text: string): void {
    const piece = this.texts.push(text) - 1
    for (let at = text.indexOf("<"); at !== -1; at = text.indexOf("<", at)) {
      at++
      if (text.charCodeAt(at) === SLASH) at++
      const start = at
      while (at < text.length && isNameUnit(text.charCodeAt(at))) at++
      if (at === start) continue
      this.#runs.runs ??= []
      this.#runs.runs.push(piece, start, at)
    }
  }

  /** What the text holds of the tags of `name`: undefined where it holds none. */
  of(name: string): HeldName | undefined {
    let held: HeldName | undefined = this.#runs
    for (let depth = 0; held !== undefined && depth < name.length; depth++) {
      this.#handOn(held, depth)
      held = held.next?.get(name.charCodeAt(depth))
    }
    return held === this.#runs ? undefined : held
  }

  /** Hands each run that `held`, the node at `depth`, keeps on to the node of the run's character
   *  at `depth`, which it builds where there is none: a run that ends at that node adds its
   *  suffixes to it, and any other is kept there. */
  #handOn(held: HeldName, depth: number): void {
    const runs = held.runs
    if (runs === undefined) return
    held.runs = undefined
    held.next ??= new Map()
    for (let index = 0; index < runs.length; index += 3) {
      const piece = runs[index] ?? 0
      const start = runs[index + 1] ?? 0
      const end = runs[index + 2] ?? 0
      const text = this.texts[piece] ?? ""
      const unit = text.charCodeAt(start + depth)
      let next = held.next.get(unit)
      if (next === undefined) {
        next = { next: undefined, runs: undefined, suffixes: undefined, least: 1 }
        held.next.set(unit, next)
      }
      if (start + depth + 1 < end) {
        next.runs ??= []
        next.runs.push(piece, start, end)
      } else holdSuffixes(next, text, end)
    }
  }
}

/** A node of the trie of `HeldTags`: the name that a run the text holds starts with, whose own
 *  tag, suffix 0, the text therefore holds. */
interface HeldName {
  /** The node of each character that follows the name in a run handed on past this node. */
  next: Map<number, HeldName> | undefined
  /** The runs longer than the name that start with it and that no walk has handed on yet: for
   *  each, its piece, where it starts and where it ends, one after another. */
  runs: number[] | undefined
  /** Each n of a `-n` that follows a run that is the name and no more. */
  suffixes: Set<number> | undefined
  /** A suffix every one below which the text holds: one at least, since it holds suffix 0. */
  least: number
}

/** Adds to `held` the suffix of each tag of its name that `text` holds after a run that is the
 *  name and no more and ends at `end`: each n whose digits begin the digits after a `-` there. */
function holdSuffixes(held: HeldName, text: string, end: number): void {
  if (text.charCodeAt(end) !== DASH) return
  let suffix = 0
  for (let index = end + 1; index < text.length; index++) {
    const digit = text.charCodeAt(index) - 48
    // A tag's suffix has no leading zero, so `-0` holds none.
    if (digit < 0 || digit > 9 || (suffix === 0 && digit === 0)) break
    suffix = suffix * 10 + digit
    if (suffix > SUFFIX_CAP) break
    held.suffixes ??= new Set()
    held.suffixes.add(suffix)
  }
}

/** `/`, which a closing tag's `</` holds. */
const SLASH = 0x2f
/** `-`, which joins a tag's suffix to its name. */
const DASH = 0x2d

/** Whether `unit` is a character a tag's name holds: [A-Za-z0-9_]. */
function isNameUnit(unit: number): boolean {
  return (
    (unit >= 0x30 && unit <= 0x39) ||
    (unit >= 0x41 && unit <= 0x5a) ||
    (unit >= 0x61 && unit <= 0x7a) ||
    unit === 0x5f
  )
}

/** Whether `held`, what a text holds of the tags of one name, holds the tag of `suffix`. */
function holds(held: HeldName | undefined, suffix: number): boolean {
  return held !== undefined && (suffix === 0 || held.suffixes?.has(suffix) === true)
}

/** The least suffix whose tag `held`, what a text holds of the tags of one name, may not hold:
 *  it holds the tag of every suffix below this one. */
function leastFree(held: HeldName | undefined): number {
  if (held === undefined) return 0
  while (held.suffixes?.has(held.least) === true) held.least++
  return held.least
}

/** The tag of each of `blocks`, the blocks of one text in their order, behind `front`, the text
 *  in front of that one, where `beside` is the rest of that text: the first of `name`, `name-1`,
 *  `name-2` and on whose `<tag` and `</tag` neither `front`, nor a text of `beside`, nor the
 *  words of any block hold, and that no earlier block took. Each text here is scanned once, and
 *  each block's tag is a lookup of its name in what `front` and these texts hold. */
function blockTags(
  front: HeldTags,
  beside: readonly string[],
  blocks: readonly DrawnBlock[],
): string[] {
  const here = new HeldTags([...beside, ...blocks.map(({ text }) => text)])
  // For each name, what the text in front and the texts here hold of its tags, the suffixes a
  // block took, and the least that may be free: every suffix below it is held, in front or here,
  // or taken.
  const named = new Map<
    string,
    {
      readonly ahead: HeldName | undefined
      readonly held: HeldName | undefined
      readonly taken: Set<number>
      next: number
    }
  >()
  return blocks.map(({ name }) => {
    let own = named.get(name)
    if (own === undefined) {
      const ahead = front.of(name)
      const held = here.of(name)
      own = { ahead, held, taken: new Set(), next: Math.max(leastFree(ahead), leastFree(held)) }
      named.set(name, own)
    }
    while (holds(own.ahead, own.next) || holds(own.held, own.next) || own.taken.has(own.next)) {
      own.next++
    }
    const suffix = own.next
    own.taken.add(suffix)
    return suffix === 0 ? name : `${name}-${suffix}`
  })
}

/** The largest suffix a scan keeps. A block takes the least suffix that is neither held nor
 *  taken, so never one above the count of those held and taken, and each suffix held takes a
 *  unit of the text and each one taken a block. No text comes near this many units, so no suffix
 *  a block could take lies past it, and stopping here keeps every number exact. */
const SUFFIX_CAP = Number.MAX_SAFE_INTEGER
