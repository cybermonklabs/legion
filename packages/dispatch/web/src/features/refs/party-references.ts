import { trimReference } from "@legion/contracts/reference-spans";

import type { OutgoingDraft, SentDraft } from "../conversation/send-request";
import { codeRanges } from "./code-ranges";
import { deliveryAddressOf } from "./party-mention";
import type { DispatchReferenceRoute, PartyRoute } from "./routes";
import { readTextReferences, type TextReference } from "./text-references";

/**
 * Who a draft addresses, read out of its own words. A mention is a reference: the party is
 * written into the text as a link whose words are the mention sign and its name and whose
 * address names it, and nothing that names a party is kept beside the text. So every surface that
 * asks who a draft names - the delivery chips under a box, the composer's delivery notices,
 * whether a typed `/btw` is a command, and the one answer a send keeps beside its text, which
 * the request Send makes reads (`outgoingDraft`) - asks here, and this asks the one reader of
 * what a text cites (`readTextReferences`), the twin of the server's own. What the writer is
 * told and what the server will read can therefore never be two readings of the same draft. The
 * server reads the address alone, never a link's words, so the words decide only what a reader
 * sees (`@legion/contracts`'s `referenceReading`).
 *
 * A party reference a code context holds - a fenced code block, an indented code block or a code
 * span (`TextReference.inCode`) - names nobody, as the server reads it
 * (`api.commentPartiesOf`): a draft writing one is showing the address rather than addressing
 * its party, which is how every renderer of the record draws it. So it is no target, it makes no
 * draft one that names somebody, and the composer leaves a typed `/btw` in front of it the
 * reader's own first word. The same party named outside code is named by that occurrence, and a
 * worded link's address (`[@Planner](dispatch://agent/...)`) is no code context at all.
 */

/** Whether a reference names a party: an agent's session, a role, or a person. */
export function isPartyRoute(route: DispatchReferenceRoute): route is PartyRoute {
  return route.kind === "agent-session" || route.kind === "role" || route.kind === "person";
}

/** `draft` as the held-send store holds it to send (`OutgoingDraft`): with whether its text
 *  names a party (`draftParties`), read here once, as the composer hands the draft over - at the
 *  press, and as it keeps what the reader wrote over a refusal - and by the kinds chunk for a
 *  send an older build kept (`kinds.ts`). A caller that has read that text's parties already
 *  hands their answer in (`named`), so the text is not parsed twice. The send reads that answer,
 *  so the entry chunk, which makes a kept send's attempt and a Retry's, holds none of the
 *  grammar. */
export function outgoingDraft(
  { body, replacement }: SentDraft,
  named = draftParties(body).named
): OutgoingDraft {
  return { body, namesParty: named, replacement };
}

/** What a draft's text says about who it addresses. */
export interface DraftParties {
  /** Every delivering party the text names, in the order it names them and each once: who the
   *  write is delivered to, and what the delivery notices are drawn from. */
  readonly targets: readonly string[];
  /** Whether the text names a party at all, a person among them. Nothing is delivered to a
   *  person, but naming one is still addressing somebody: it is what makes a typed `/btw` the
   *  command it looks like rather than the reader's own first word. */
  readonly named: boolean;
}

/** The parties `body` names, each as the reference naming it stands in the text: a party
 *  written in a code context names nobody and is neither. */
export function draftParties(body: string): DraftParties {
  const targets: string[] = [];
  const seen = new Set<string>();
  let named = false;
  for (const found of readTextReferences(body).references) {
    const route = found.route;
    if (found.inCode || route === undefined || !isPartyRoute(route)) continue;
    named = true;
    const target = deliveryAddressOf(route);
    if (target === undefined || seen.has(target)) continue;
    seen.add(target);
    targets.push(target);
  }
  return { named, targets };
}

/** One inline link a text writes - `[words](address "title")` - as offsets into it: its `[`, the
 *  `]` its words end at, its address without the angle brackets it may stand in, and one past its
 *  `)`; `picture` where a `!` before the `[` makes it a picture's syntax (`![caption](address)`),
 *  which a renderer draws as the picture rather than as a link. */
export interface InlineLink {
  readonly open: number;
  readonly wordsEnd: number;
  readonly addressStart: number;
  readonly addressEnd: number;
  readonly close: number;
  readonly picture: boolean;
}

/** What follows a line break where the next line is blank, which ends the paragraph and every
 *  bracket open in it. */
const BLANK_LINE_AFTER = /[ \t]*\n/y;

/** The characters a backslash escapes: ASCII punctuation, as CommonMark and goldmark read an
 *  escape. Before any other character - a space, a letter, a line break - a backslash is a
 *  backslash, and the character after it is read as itself. */
const ESCAPABLE = /^[!-/:-@[-`{-~]$/u;

/**
 * Where each step of reading a link's tail - its address and title, after the `(` - stops, from
 * every offset of one text: built once, the first time the text is read for a link's tail
 * (`readInlineLinks`), so a tail is a few lookups whatever its length. Read to its end instead,
 * every `](` of a draft holding thousands that open no link - parentheses that never balance, a
 * title that never closes - is read across the rest of its paragraph, in time that grows with the
 * square of the draft's length; and a bound on each tail read a real link whose address runs
 * past it - a dashboard URL with a long query - as no link, where both CommonMark parsers read
 * it as one. A backslash escapes the character after it where that is punctuation (`ESCAPABLE`),
 * as the tail's reader takes it, so an escaped character stops nothing, while a space or a line
 * break after a backslash stops what it would stop anywhere. A stop past every character is the
 * text's length.
 */
interface TailStops {
  /** The first offset at or after each that holds neither a space nor a tab (`pastSpace`). */
  readonly filled: Int32Array;
  /** Where an address not in angle brackets that starts at each offset ends, or -1 where it
   *  reads none: at the first space, tab, line break or control character, or at the first `)`
   *  that closes more parentheses than open after the offset, which is the link's own, whichever
   *  comes first. A `(` the address leaves open before a space ends nothing, as goldmark's link
   *  parser reads it, and so the display tree the page draws (`pmdoc`): `[x](a(b "T")` is a link
   *  to `a(b`, where CommonMark's own rule would read no link there. */
  readonly bare: Int32Array;
  /** The first `<`, `>` or line break at or after each: an address in angle brackets ends at a
   *  `>` and reads as none at the other two. */
  readonly angled: Int32Array;
  /** The first `"`, `'` and `)` at or after each, by that character: where a title ends. */
  readonly closers: Readonly<Record<string, Int32Array>>;
  /** The first `(` at or after each: a title in parentheses holds none, so one standing before
   *  its `)` makes it no title, and the tail no link, as CommonMark and goldmark read it. */
  readonly opens: Int32Array;
  /** The line break that ends the first blank line starting at or after each: a title that has
   *  not closed before it ends with its paragraph, and so does the link. */
  readonly blank: Int32Array;
}

/** Every stop of `body` (`TailStops`), in one walk of it from its end. The parentheses an address
 *  closes are counted from the end too - only the count between two offsets matters - and an
 *  address that starts at an offset ends at the nearest later one where fewer stand open than at
 *  its start, which a stack of the offsets standing lower than all those before them answers. */
function tailStops(body: string): TailStops {
  const length = body.length;
  const escaped = new Uint8Array(length + 1);
  for (let at = 0; at < length; at += 1) {
    if (body.charAt(at) === "\\" && ESCAPABLE.test(body.charAt(at + 1))) {
      escaped[at + 1] = 1;
      at += 1;
    }
  }
  const table = () => new Int32Array(length + 1).fill(length);
  const quote = table();
  const apostrophe = table();
  const parenthesis = table();
  const stops = {
    angled: table(),
    bare: table(),
    blank: table(),
    closers: { '"': quote, "'": apostrophe, ")": parenthesis },
    filled: table(),
    opens: table(),
  };
  stops.bare[length] = -1;
  const lower = new Int32Array(length + 1);
  const lowerOpen = new Int32Array(length + 1);
  let lowers = 1;
  lower[0] = length;
  let open = 0;
  let space = length;
  let lineBreak = length;
  let blankAfter = false;
  for (let at = length - 1; at >= 0; at -= 1) {
    const character = body.charAt(at);
    const plain = escaped[at] === 0;
    const after = at + 1;
    if (plain && character === "(") open -= 1;
    else if (plain && character === ")") open += 1;
    stops.filled[at] = character === " " || character === "\t" ? stops.filled[after] : at;
    stops.angled[at] = plain && "<>\n".includes(character) ? at : stops.angled[after];
    quote[at] = plain && character === '"' ? at : quote[after];
    apostrophe[at] = plain && character === "'" ? at : apostrophe[after];
    parenthesis[at] = plain && character === ")" ? at : parenthesis[after];
    stops.opens[at] = plain && character === "(" ? at : stops.opens[after];
    if (plain && character === "\n") {
      stops.blank[at] = blankAfter ? lineBreak : stops.blank[after];
      lineBreak = at;
      blankAfter = true;
    } else {
      stops.blank[at] = stops.blank[after];
      if (character !== " " && character !== "\t") blankAfter = false;
    }
    if (plain && character <= " ") space = at;
    while (lowers > 0 && lowerOpen[lowers - 1] >= open) lowers -= 1;
    const closes = lowers > 0 ? lower[lowers - 1] - 1 : length;
    stops.bare[at] = closes < space ? closes : space < length ? space : -1;
    lower[lowers] = at;
    lowerOpen[lowers] = open;
    lowers += 1;
  }
  return stops;
}

/** `at` moved past the spaces, tabs and one line break that stand there; undefined at a second
 *  line break, since a link's parts are never more than a line apart and a blank line ends the
 *  paragraph, and so the link. */
function pastSpace(body: string, stops: TailStops, at: number): number | undefined {
  const filled = stops.filled[at];
  if (body.charAt(filled) !== "\n") return filled;
  const next = stops.filled[filled + 1];
  return body.charAt(next) === "\n" ? undefined : next;
}

/** What closes a link's title, by what opens it. */
const TITLE_CLOSERS: Readonly<Record<string, string>> = { '"': '"', "'": "'", "(": ")" };

/** The address and the end of an inline link whose `(` stands just before `from`, read as the
 *  display tree the page draws reads a link's destination and title (goldmark's link parser, under
 *  `pmdoc`), however long: an address in angle brackets, or one with no space whose parentheses
 *  close none it did not open (`TailStops.bare`), then, after a space, a title in double quotes,
 *  single quotes or parentheses, then the `)`, all within the paragraph. Undefined where no link
 *  ends there. */
function linkTail(
  body: string,
  stops: TailStops,
  from: number
): Pick<InlineLink, "addressEnd" | "addressStart" | "close"> | undefined {
  const start = pastSpace(body, stops, from);
  if (start === undefined) return undefined;
  const angled = body.charAt(start) === "<";
  const addressStart = angled ? start + 1 : start;
  const addressEnd = angled ? stops.angled[addressStart] : stops.bare[addressStart];
  if (addressEnd < 0 || (angled && body.charAt(addressEnd) !== ">")) return undefined;
  const past = angled ? addressEnd + 1 : addressEnd;
  const spaced = pastSpace(body, stops, past);
  if (spaced === undefined) return undefined;
  const closer = spaced > past ? TITLE_CLOSERS[body.charAt(spaced)] : undefined;
  let at = spaced;
  if (closer !== undefined) {
    const closed = stops.closers[closer][spaced + 1];
    // A title may run over a line break, never over a blank line, and one in parentheses holds
    // no `(` of its own.
    if (closed >= body.length || stops.blank[spaced + 1] < closed) return undefined;
    if (closer === ")" && stops.opens[spaced + 1] < closed) return undefined;
    const after = pastSpace(body, stops, closed + 1);
    if (after === undefined) return undefined;
    at = after;
  }
  return body.charAt(at) === ")" ? { addressEnd, addressStart, close: at + 1 } : undefined;
}

/**
 * Every inline link `body` writes, in the order their words close, read as CommonMark and the
 * display tree the page draws read them, a link's address read as that tree reads it
 * (`linkTail`): a backslash takes the punctuation character after it as a literal one
 * (`ESCAPABLE`), and before anything else - a line break among them - is a backslash, brackets
 * that balance stand inside a link's words (`[@Planner [2]](…)`), a link holds no link - once one
 * closes, no `[` before it opens one, a picture's caption aside - and a blank line ends every
 * bracket open before it. Code holds no link syntax, so what the text's code contexts hold
 * (`codeRanges`) is passed over. Every tail is read through the one set of stops the text has
 * (`tailStops`), built at its first `](`, so a text that writes none builds none.
 */
function readInlineLinks(body: string): readonly InlineLink[] {
  const code = codeRanges(body);
  const links: InlineLink[] = [];
  const openers: { at: number; picture: boolean }[] = [];
  // Every opener below this index that is not a picture's was open when a link closed, and so
  // opens no link: one mark for all of them, rather than a walk over them at every link.
  let closedBelow = 0;
  let nextCode = 0;
  let stops: TailStops | undefined;
  // The character the last backslash escaped: a `!` there opens no picture, where `\\![` is an
  // escaped backslash and a picture.
  let escaped = -1;
  for (let at = 0; at < body.length; at += 1) {
    while (nextCode < code.length && (code[nextCode]?.end ?? 0) <= at) nextCode += 1;
    const range = code[nextCode];
    if (range !== undefined && range.start <= at) {
      at = range.end - 1;
      continue;
    }
    const character = body.charAt(at);
    if (character === "\\" && ESCAPABLE.test(body.charAt(at + 1))) {
      at += 1;
      escaped = at;
    } else if (character === "\n") {
      BLANK_LINE_AFTER.lastIndex = at + 1;
      if (BLANK_LINE_AFTER.test(body)) {
        openers.length = 0;
        closedBelow = 0;
      }
    } else if (character === "[") {
      const picture = body.charAt(at - 1) === "!" && escaped !== at - 1;
      closedBelow = Math.min(closedBelow, openers.length);
      openers.push({ at, picture });
    } else if (character === "]") {
      const opener = openers.pop();
      const opens = opener !== undefined && (opener.picture || openers.length >= closedBelow);
      let tail: Pick<InlineLink, "addressEnd" | "addressStart" | "close"> | undefined;
      if (opens && body.charAt(at + 1) === "(") {
        stops ??= tailStops(body);
        tail = linkTail(body, stops, at + 2);
      }
      if (opener === undefined || tail === undefined) continue;
      links.push({ ...tail, open: opener.at, picture: opener.picture, wordsEnd: at });
      if (!opener.picture) closedBelow = openers.length;
      at = tail.close - 1;
    }
  }
  return links;
}

/** The last text `inlineLinks` read, and the links it found there. */
let lastRead: { readonly body: string; readonly links: readonly InlineLink[] } | undefined;

/** Every inline link `body` writes (`readInlineLinks`). A draft's pills ask about each reference
 *  of one text in turn (`linkOf`, `inLinkWords`), so the text last read is read once, not once
 *  for every reference it holds. */
export function inlineLinks(body: string): readonly InlineLink[] {
  if (lastRead === undefined || lastRead.body !== body) {
    lastRead = { body, links: readInlineLinks(body) };
  }
  return lastRead.links;
}

/** The inline link whose address, trimmed as the shared span reader trims it, stands at `[start, end)`. */
function linkWithAddress(body: string, start: number, end: number): InlineLink | undefined {
  return inlineLinks(body).find(
    (link) =>
      link.addressStart === start &&
      start + trimReference(body.slice(start, link.addressEnd)).length === end
  );
}

/** `[start, end)` widened over the inline link that wraps it, where one does. A mention's
 *  address is only half of what a reader sees, so a caller taking one out of the text must take
 *  its words with it rather than leave `[@Planner](` behind. */
function linkAround(body: string, start: number, end: number): { end: number; start: number } {
  const link = linkWithAddress(body, start, end);
  return link === undefined ? { end, start } : { end: link.close, start: link.open };
}

/** The inline link `body` writes a reference's address in: the link's own Markdown, from the
 *  `[` that opens its words to the `)` that closes its address, which a reader of its words
 *  parses (`parsed-references.ts`'s `parsedLinkWords`) rather than reads out of the characters,
 *  and whether it is a picture's syntax (`![caption](address)`), which a renderer draws as the
 *  picture rather than as a link. */
export interface WrittenLink {
  readonly picture: boolean;
  readonly markdown: string;
}

/** The inline link whose address `reference` is (`WrittenLink`), or undefined where the
 *  reference stands on its own: what a draft's pill reads, as the rendered record reads the
 *  same text. */
export function linkOf(
  body: string,
  reference: Pick<TextReference, "end" | "start">
): WrittenLink | undefined {
  const link = linkWithAddress(body, reference.start, reference.end);
  return link === undefined
    ? undefined
    : { markdown: body.slice(link.open, link.close), picture: link.picture };
}

/** Whether `reference` stands in an inline link's words rather than on its own or as a link's
 *  address: `CORE-1` in `[see CORE-1 here](https://x)`, and in `[CORE-1](dispatch://CORE-2)`,
 *  whose address `CORE-2` is not in its words. The renderer links no reference there - its link
 *  pass reads no text a link, a code span or a code block holds (`RefLink.tsx`'s
 *  `excludedRefAncestorTags` and `linkPassReads`), and a link's words are text its `<a>` holds -
 *  so what reads a draft as the record will be drawn, a draft's pills among them, links none
 *  there either. */
export function inLinkWords(
  body: string,
  reference: Pick<TextReference, "end" | "start">
): boolean {
  return inlineLinks(body).some(
    (link) => link.open < reference.start && reference.end <= link.wordsEnd
  );
}

/** Where `body` first mentions the party at `target`, taken whole - the link's words with its
 *  address - or undefined where it never does. A reference a code context holds mentions
 *  nobody, so none is found there: the one caller takes the mention it finds out of the draft
 *  (`seededDraft`), and the text inside a fence is the reader's own, not an address this
 *  channel owes. */
export function partyMentionSpan(
  body: string,
  target: string
): { end: number; start: number } | undefined {
  for (const found of readTextReferences(body).references) {
    const route = found.route;
    if (
      found.inCode ||
      route === undefined ||
      !isPartyRoute(route) ||
      deliveryAddressOf(route) !== target
    ) {
      continue;
    }
    return linkAround(body, found.start, found.end);
  }
  return undefined;
}
