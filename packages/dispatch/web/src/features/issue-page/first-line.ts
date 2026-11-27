import type { DispatchReferenceRoute } from "../refs/routes";
import { referenceTargetKind } from "../refs/target-id";
import { chipWords } from "./node-row-chip";

/**
 * The pure rules of a node's one line, which the outline and every place that draws one line of
 * a node read (R1): where its first line ends (`firstLineOf`), how it is drawn with its ellipsis
 * (`drawnLine`), and the words a chip names its target by (`chipName`).
 *
 * They hold no React, make no request, and import no code of the tree's model - `tree-drafts.ts`,
 * `issue-tree.ts`, `useIssueTree.ts` - so a node's words drawn with `RowText` on Dashboard cut a
 * chip by the one rule the tree cuts it by, without carrying the tree in Dashboard's chunk (R14).
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

/**
 * The node's own text to the first sentence end or `FIRST_LINE_LIMIT` characters, whichever
 * comes first, cut at a word wherever the limit falls near one. A leading label - a first word
 * of at most five characters that ends in `.` and holds a digit, as `R1.`, `12.` or `1.2.` - is
 * part of the line and no sentence end, so a statement's line runs on to the first sentence end
 * after its label. `lead` is the page's own word the line opens with, before the node's own
 * words `text` and none of them - the issue's key before its title (`lineReading`) - which is
 * written before them here and counts towards the limit, and the label is read at the start of
 * `text`: `CORE-1 1. Migrate the table` keeps its `1.`. A line held to the limit that holds no
 * space past its label - one unbroken token, a URL or an identifier - is cut at the limit
 * itself, because there is no word boundary to cut at and the limit is the row's whole budget,
 * and one unit short of it where the limit falls inside a surrogate pair, as `textHead` cuts.
 * `cut` says whether the text carries on past the line, and `sentence` whether the line ends at
 * a sentence end, the first one after any label: the two are what the row draws its ellipsis by
 * (`drawnLine`). A line the limit cut, at a word or at the limit itself, ends at none, whatever
 * its last character: a token cut at the limit may end on a `.`, `!` or `?` of its own, as
 * `x.yyyy` cut after its dot does.
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
  const rest = /^.*?[.!?](?=\s|$)/u.exec(whole.slice(head.length))?.[0];
  const first = rest === undefined ? undefined : `${head}${rest}`;
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

/** The words a chip draws for a target `name` names (build decision 5): a block's first line, as a
 *  row draws one (R1, `firstLineOf`, `drawnLine`), and any other target's name, each cut to a
 *  chip's room (`chipWords`). The page publishes every name as a link to its target reads it
 *  (R8), so a tie's chip (`NodeRow.tsx`), a block or a document drawn as a chip in a node's words
 *  (`row-references.ts`), the Plan chart's needs and serves (`plan-filter/PlanFilterBody.tsx`)
 *  and a check's "Also under" line (`verification-filter/VerificationFilterBody.tsx`) each cut it
 *  again here, where they draw it, and draw one target alike. */
export function chipName(route: DispatchReferenceRoute | undefined, name: string): string {
  const block = route !== undefined && referenceTargetKind(route) === "block";
  return chipWords(block ? drawnLine(firstLineOf(name)) : name);
}
