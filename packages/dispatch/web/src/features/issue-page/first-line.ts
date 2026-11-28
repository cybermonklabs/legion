import type { DispatchReferenceRoute } from "../refs/routes";
import { referenceTargetKind } from "../refs/target-id";
import { chipWords } from "./node-row-chip";

/**
 * The pure rules of a node's one line, which the outline and every place that draws one line of
 * a node read: where its first sentence ends (`firstSentenceLength`), where its first line ends
 * (`firstLineOf`), how it is drawn with its ellipsis (`drawnLine`), and the words a chip names
 * its target by (`chipName`).
 *
 * They hold no React, make no request, and import nothing of the page's model (`issue-tree.ts`,
 * `tree-drafts.ts`), so a row's text cuts a chip by the one rule the page cuts it by without
 * pulling the model into its import closure.
 */

/** How many characters a row's first line may hold (R1). */
export const FIRST_LINE_LIMIT = 100;

/**
 * A leading label: a first word of at most five characters that ends in `.` and holds a digit,
 * as `R1.`, `12.` or `1.2.`. Its dot is read as part of the line and no sentence end, so a line
 * that opens with one runs on to the first sentence end after it (`firstLineOf`). Every place
 * that reads where a line's first sentence ends reads its label by this one rule. It carries no
 * `g` or `y` flag, so `exec` keeps no state between the places that share it.
 */
export const LEADING_LABEL = /^(?=\S*\d)\S{0,4}\.(?=\s|$)/u;

/** A closing quote or bracket, which belongs to the sentence whose end mark it follows. */
const CLOSING_MARK = /[)\]"'\u201d\u2019]/u;

/** A numbered label at the start of a word: digits and one full stop, before a space or where
 *  the text stops. */
const NUMBERED_LABEL = /^\d+\.(?=\s|$)/u;

/**
 * The abbreviations a number may follow inside a sentence, each written in lower case and read
 * in any case (`firstSentenceLength`): a full stop after one of these words, before a word that
 * opens with a digit, ends no sentence, as in `see p. 4`, `See Fig. 3` or `Keep v. 1.2`. A text
 * that uses another abbreviation before a number becomes a new row of the table that pins the
 * rule (`SENTENCE_TABLE` in `sentence-table.ts`), not a new rule.
 */
const NUMBER_ABBREVIATIONS: ReadonlySet<string> = new Set([
  "art",
  "cf",
  "ch",
  "eq",
  "eqs",
  "fig",
  "figs",
  "no",
  "nos",
  "p",
  "para",
  "pp",
  "sec",
  "v",
  "vol",
  "vs",
]);

/** A letter or a decimal digit: what a whole word is made of, so that the word before a full
 *  stop starts where the text does or after a character that is neither. */
const WORD_CHARACTER = /[\p{L}\p{Nd}]/u;

/**
 * How many characters of `text` its first sentence takes, its leading label included, or
 * undefined where `text` holds no sentence end. Every place that splits words into sentences
 * reads it: the one line drawn for a node (`firstLineOf`), the issue's three sentences
 * (`IssueThreeThings.tsx`) and a person's quoted first sentence (`spec-decided-by.ts`'s
 * `firstSentenceOf`), so the same words are split one way wherever the page draws them.
 *
 * A leading label (`LEADING_LABEL`) opens the first sentence and ends none, so the scan starts
 * after it. A sentence end is a full stop, a question mark or an exclamation mark, with whatever
 * closing quote or bracket the writer put after it, which belongs to the sentence it ends. A
 * mark ends the sentence where the text stops after it, trailing whitespace aside, and ends none
 * where no whitespace follows it, inside a token such as `x.yyyy`, `1.2` or `example.com`. A
 * question or exclamation mark before a word ends the sentence whatever the word is, in lower
 * case too. A full stop before a word in lower case ends an abbreviation (`e.g. in`), not the
 * sentence. A full stop before a word that opens with a digit ends the sentence, unless the word
 * before the full stop is one of the abbreviations a number follows (`NUMBER_ABBREVIATIONS`),
 * read in any case and standing as a whole word, as in `(see p. 4)`: there the number is part
 * of the sentence and opens no label, and the scan goes on. A full stop before any other word,
 * one in upper case or one that opens with a symbol, ends the sentence. A colon ends no
 * sentence, and a numbered label (`NUMBERED_LABEL`) right after a colon and whitespace is a
 * label: its full stop ends nothing, and the scan goes on after it, so `Steps: 1. Open it. 2.`
 * ends at `it.`.
 *
 * One table pins this rule: each row of `SENTENCE_TABLE` (`sentence-table.ts`) is one text and
 * the first sentence the page must find in it, and every reader of the rule is tested over every
 * row. The rule has two accepted misses. `The answer is no. 3 rows changed.` runs as one
 * sentence, since `no.` before a number is far more often "number" than a sentence's last word.
 * `Ask Dr. Smith. Then more.` ends at `Ask Dr.`, since the list holds abbreviations before a
 * number only, and a full stop before a word in upper case ends the sentence whatever word
 * stands before it.
 */
export function firstSentenceLength(text: string): number | undefined {
  const label = LEADING_LABEL.exec(text)?.[0] ?? "";
  for (let at = label.length; at < text.length; at += 1) {
    const mark = text.charAt(at);
    if (mark === ":") {
      let word = at + 1;
      while (/\s/u.test(text.charAt(word))) word += 1;
      const numbered = word === at + 1 ? null : NUMBERED_LABEL.exec(text.slice(word));
      if (numbered !== null) at = word + numbered[0].length - 1;
      continue;
    }
    if (mark !== "." && mark !== "?" && mark !== "!") continue;
    let end = at + 1;
    while (CLOSING_MARK.test(text.charAt(end))) end += 1;
    let word = end;
    while (/\s/u.test(text.charAt(word))) word += 1;
    if (word === text.length) return end;
    if (word === end) continue;
    if (mark !== ".") return end;
    const next = text.slice(word);
    if (/^\p{Ll}/u.test(next)) continue;
    if (/^\d/u.test(next) && abbreviationEndsAt(text, at)) continue;
    return end;
  }
  return undefined;
}

/** Whether the whole word that ends at `at` in `text`, before the full stop there, is one of the
 *  abbreviations a number follows (`NUMBER_ABBREVIATIONS`), in any case. */
function abbreviationEndsAt(text: string, at: number): boolean {
  let start = at;
  while (start > 0 && WORD_CHARACTER.test(text.charAt(start - 1))) start -= 1;
  return NUMBER_ABBREVIATIONS.has(text.slice(start, at).toLowerCase());
}

/**
 * The node's own text, its white space collapsed, to the end of its first sentence, which ends
 * where `firstSentenceLength` says, or to `FIRST_LINE_LIMIT` characters, whichever comes first,
 * cut at a word wherever the limit falls near one. `lead` is the page's own word the line opens
 * with, before the node's own words `text` and none of them - the issue's key before its title
 * (`lineReading`) - which is written before them here and counts towards the limit, and the
 * first sentence and its leading label (`LEADING_LABEL`) are read from the start of `text`:
 * `CORE-1 1. Migrate the table` keeps its `1.`. A line held to the limit that holds no space past
 * its leading label - one unbroken token, a URL or an identifier - is cut at the limit itself,
 * because there is no word boundary to cut at and the limit is the row's whole budget, and one
 * unit short of it where the limit falls inside a surrogate pair, as `textHead` cuts. `cut` says
 * whether the text carries on past the line, and `sentence` whether the line ends at its first
 * sentence's end: the two are what the row draws its ellipsis by (`drawnLine`). A line the limit
 * cut, at a word or at the limit itself, ends at none, whatever its last character: a token cut
 * at the limit may end on a `.`, `!` or `?` of its own, as `x.yyyy` cut after its dot does.
 */
export function firstLineOf(
  text: string,
  lead = ""
): {
  readonly cut: boolean;
  readonly line: string;
  readonly sentence: boolean;
} {
  const own = text.replace(/\s+/gu, " ").trim();
  const opening = lead === "" ? "" : `${lead} `;
  const whole = `${opening}${own}`.trimEnd();
  const head = `${opening}${LEADING_LABEL.exec(own)?.[0] ?? ""}`;
  const sentenceLength = firstSentenceLength(own);
  const first =
    sentenceLength === undefined ? undefined : whole.slice(0, opening.length + sentenceLength);
  if (first !== undefined && first.length <= FIRST_LINE_LIMIT) {
    return { cut: first.length < whole.length, line: first, sentence: true };
  }
  if (whole.length <= FIRST_LINE_LIMIT) {
    return { cut: false, line: whole, sentence: false };
  }
  const window = whole.slice(0, FIRST_LINE_LIMIT + 1);
  const lastSpace = window.lastIndexOf(" ");
  if (lastSpace > head.length) {
    return { cut: true, line: window.slice(0, lastSpace), sentence: false };
  }
  const last = whole.charCodeAt(FIRST_LINE_LIMIT - 1);
  const end = last >= 0xd800 && last <= 0xdbff ? FIRST_LINE_LIMIT - 1 : FIRST_LINE_LIMIT;
  return { cut: true, line: whole.slice(0, end), sentence: false };
}

/**
 * A first line as a place that draws one line for a node draws it (R1): an ellipsis where the
 * text carries on past a line the cut ended inside a sentence, and none
 * where the line is a whole sentence, whose own mark already ends it - "Thread 1 opens here.…"
 * reads as four dots. What follows a whole sentence is the node's next sentence, which its row
 * or its body holds. Whether the line ends at a sentence end is `firstLineOf`'s `sentence`, and
 * every caller hands it in with the line, since a line cut inside a sentence can end on a `.`,
 * `!` or `?` that ends none: nothing here reads the line's last character to decide it. A line
 * cut just after an ellipsis of the author's own (`wait…`), which `firstLineOf` reads as no
 * sentence end since it often stands inside one, draws no second: `wait……` reads as six dots.
 */
export function drawnLine({
  cut,
  line,
  sentence,
}: {
  readonly cut: boolean;
  readonly line: string;
  readonly sentence: boolean;
}): string {
  return cut && !sentence && !line.endsWith("…") ? `${line}…` : line;
}

/** The words a chip draws for a target `name` names: a block's first line, as a row draws one
 *  (R1, `firstLineOf`, `drawnLine`), and any other target's name, each cut to a
 *  chip's room (`chipWords`). The page publishes every name as a link to its target reads it
 *  (R8), so a tie's chip (`NodeRow.tsx`), a block or a document drawn as a chip in a node's words
 *  (`row-references.ts`), the Plan chart's needs and serves (`plan-filter/PlanFilterBody.tsx`)
 *  and a check's "Also under" line (`verification-filter/VerificationFilterBody.tsx`) each cut it
 *  again here, where they draw it, and draw one target alike. */
export function chipName(route: DispatchReferenceRoute | undefined, name: string): string {
  const block = route !== undefined && referenceTargetKind(route) === "block";
  return chipWords(block ? drawnLine(firstLineOf(name)) : name);
}
