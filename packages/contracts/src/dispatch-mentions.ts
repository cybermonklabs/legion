/**
 * The mention rule, the one place every reader of a record and the
 * document editor take it from. A mention of a party - an agent's session, a role or a person -
 * is a reference written into the text as a link whose words open with the mention sign and
 * whose address names the party: `[@Planner](dispatch://agent/…)`. Such a link reads as the sign
 * and the party's current name wherever it is shown - a rendered body, the plain-text
 * projection, the document editor's titles and a draft's chips - so a session renamed since the
 * mention was written reads as it is called now. Every other worded link keeps its author's
 * words, and an address standing on its own, or a link whose words are that address, reads as
 * the target's current name.
 *
 * Formatting decides this much, the same way for every reader (`WordsFormatting`): code
 * anywhere in the words keeps the words; emphasis, strong or strikethrough over all of the
 * link's words is a mention drawn in that formatting; formatting over some of the words makes
 * them the author's words, and so does a hard break among them (`HARD_BREAK_PIECE`); every check
 * is local to the link. In the parsed document `[**@x**](…)` and `**[@x](…)**` are one text -
 * the same words under a strong mark and a link - so a mark over all of a link's words cannot be
 * read as the author formatting the words, and a mention inside a bold sentence keeps its
 * current name.
 *
 * Every reader reads a link out of a parsed text - the rendered body's elements, the plain-text
 * projection's and the document editor's nodes, and a draft's chips, which parse each link's own
 * Markdown with the same engine (`parsed-references.ts` in the dashboard) - so the words each one
 * reads are the parser's: escapes undone, entities decoded, formatting paired as the parser pairs
 * it. No reader reads a link's words out of the characters a writer typed.
 *
 * Which address names a party is the reader's own grammar, so each caller says that much
 * (`WrittenReference.party`); everything else about the rule is here, so a reader that drew a
 * mention one way and an editor that offered it another cannot both compile. The sign and the
 * words a mention draws (`MENTION_SIGN`, `mentionName`) are a module of their own
 * (`dispatch-mention-sign.ts`), which a writer of mentions reads without the rest of this one.
 */
import { MENTION_SIGN, mentionName } from "./dispatch-mention-sign";

/** What a reference reads as, by the one rule every record is read under (R8):
 *  - `name`: the target's current name - an address standing on its own, and a link whose words
 *    are that same address, which is what a Markdown autolink is stored as;
 *  - `mention`: the mention sign and the party's current name - a party's link whose words open
 *    with that sign (R4);
 *  - `words`: the words its author wrote, for every other link with words of its own. */
export type ReferenceReading = "mention" | "name" | "words";

/** How a link's words are formatted, which is all of their formatting a reading depends on:
 *  - `plain`: none of them is code, and whatever emphasis, strong or strikethrough there is
 *    covers all of them alike, which is formatting around the link rather than of its words;
 *  - `code`: all of them are code;
 *  - `mixed`: some of them are formatted as the rest are not. */
export type WordsFormatting = "plain" | "code" | "mixed";

/** One stretch of a link's words with the formatting over it, as a reader of a parsed text
 *  meets it: whether it is code, and `formatting`, the emphasis, strong and strikethrough over
 *  it in any spelling that is the same for two stretches formatted alike. A mark that is no
 *  formatting of the author's - a search hit's highlight, a comment's anchor - is left out. */
export interface WordsPiece {
  readonly code: boolean;
  readonly formatting: string;
}

/** The marks of Proof's schema that format words, beside its code mark
 *  (`packages/envoy/internal/dispatch/pmdoc/schema/blocks.json`). */
const WORD_FORMATTING_MARKS: readonly string[] = ["emphasis", "strong", "strike_through"];

/** The stretch of words a text node under `marks` is, for a reader of a document in Proof's
 *  schema - the document editor and the dashboard's plain-text projection - so the two tell
 *  formatting from every other mark by one list. */
export function wordsPieceUnder(
  marks: readonly {
    readonly type: { readonly name: string; readonly spec: { readonly code?: boolean } };
  }[]
): WordsPiece {
  return {
    code: marks.some((mark) => mark.type.spec.code === true),
    formatting: marks
      .map((mark) => mark.type.name)
      .filter((name) => WORD_FORMATTING_MARKS.includes(name))
      .sort()
      .join(" "),
  };
}

/** How the words `pieces` make up are formatted (`WordsFormatting`): mixed where two of them
 *  differ, and otherwise whatever every one of them is. */
export function wordsFormatting(pieces: readonly WordsPiece[]): WordsFormatting {
  const [first, ...rest] = pieces;
  if (first === undefined) {
    return "plain";
  }
  if (rest.some((piece) => piece.code !== first.code || piece.formatting !== first.formatting)) {
    return "mixed";
  }
  return first.code ? "code" : "plain";
}

/** The stretch of a link's words a hard break is, where a hard break stands among them: a part
 *  of the words formatted as no other part is, so words a hard break cuts read as their author
 *  wrote them (`wordsFormatting` answers `mixed`). A parsed text draws such a link as one run of
 *  words on each side of the break, with the same address, and every reader takes the runs on
 *  both sides as the one link they are. */
export const HARD_BREAK_PIECE: WordsPiece = { code: false, formatting: "hard break" };

/** A reference as its text writes it, which is everything its reading depends on. */
export interface WrittenReference {
  /** The address as its author wrote it: a link's destination, or the reference standing on its
   *  own. */
  readonly address: string;
  /** Whether the address names a party, by the caller's own reading of it. */
  readonly party: boolean;
  /** The words of the link that holds the address, as a reader of the parsed text sees them, or
   *  undefined for an address standing on its own. */
  readonly words?: string;
  /** How those words are formatted (`WordsFormatting`); plain where it is left out. */
  readonly wordsFormatting?: WordsFormatting;
}

/** Whether a link's words are its own address: the form a document stores an autolink in, and
 *  the bare `www.` address a Markdown autolink gives an `http://` destination. */
export function wordsAreAddress(words: string, address: string): boolean {
  return address === words || address === `http://${words}`;
}

/** Whether a link's words make it a mention, where its address names a party: they open with the
 *  mention sign, and they are plain - no code among them, which is the code's characters and
 *  never a sign its writer wrote, and no formatting over a part of them. */
export function isMentionWords(words: string, formatting: WordsFormatting = "plain"): boolean {
  return formatting === "plain" && words.startsWith(MENTION_SIGN);
}

/** What `reference` reads as (`ReferenceReading`). Words formatted in part are their author's
 *  words whatever they spell; words that are all code still name their target where they are
 *  its address, the form a document stores an autolink written as code in. */
export function referenceReading({
  address,
  party,
  words,
  wordsFormatting: formatting = "plain",
}: WrittenReference): ReferenceReading {
  if (words === undefined) {
    return "name";
  }
  if (formatting === "mixed") {
    return "words";
  }
  if (wordsAreAddress(words, address)) {
    return "name";
  }
  return party && isMentionWords(words, formatting) ? "mention" : "words";
}

/** What a party's target answered, as far as a mention of it reads: its current name, and
 *  whether Dispatch answered that the party exists with no name of its own - a session it holds
 *  a record of that the registry does not title. */
export interface MentionedParty {
  readonly title: string | undefined;
  readonly known: boolean;
}

/** What a mention of `party` draws in place of its words, in every place a mention is shown:
 *  the mention sign and the party's current name; for a party that exists with no name, the
 *  sign and `shortForm`, its address written short, so the words its writer chose
 *  (`[@Alice](…)`) never pass for a name Dispatch holds; and undefined while the party has not
 *  answered, where the mention keeps its writer's words. */
export function mentionDrawn(party: MentionedParty, shortForm: string): string | undefined {
  if (party.title !== undefined) {
    return mentionName(party.title);
  }
  return party.known ? mentionName(shortForm) : undefined;
}
