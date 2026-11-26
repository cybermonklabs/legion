/** One row of the table that fixes where the page ends a first sentence (`SENTENCE_TABLE`). */
export interface SentenceRow {
  /** The row's id, which every test names in its message. */
  readonly id: string;
  /** The text as written, `\n` a line break. */
  readonly text: string;
  /** The first sentence the function finds in the text as written: `firstSentenceOf`'s answer,
   *  and `firstLineOf`'s line, since every text here is under the line's limit. Undefined where
   *  the first sentence depends on the blocks the text is stored as: the function reads plain
   *  text, where a blank line is only a space, so it has no answer of its own to pin there. */
  readonly first: string | undefined;
  /** The first sentence a reader of the stored text draws: a person's quote of it as their
   *  answer, a comment's Contents entry and the first of the issue's three sentences. It differs
   *  from `first` only where the text opens with a list marker, which markdown reads as no word,
   *  or is stored as more blocks than one, since a quote and a Contents entry read the words the
   *  stored text opens with (`issue-tree.ts`'s `firstBlockWordsRaw`). */
  readonly stored: string;
  /** Whether the issue's three sentences are tested over the row: the table's column for them,
   *  where a row the table marks "n/a" is `false`. */
  readonly summary: boolean;
}

/**
 * Where the page ends a first sentence, as one table: each row one text and the first sentence
 * every reader of `first-line.ts`'s `firstSentenceLength` must find in it. A text the rule reads
 * wrong, or a text that uses an abbreviation the rule does not list, becomes a new row here, not
 * a new rule. Each reader runs every row: the function itself (`firstSentenceOf`, `firstLineOf`)
 * where the row gives its answer, a person's quote, a comment's Contents entry, and the issue's
 * three sentences where the text stands as a summary's. It stands beside the rule, apart from
 * any view's fixtures, since the tests of every view that reads the rule read it.
 *
 * Row r1 is not here: a quote of an answer that cites a labelled statement ends where the
 * person's sentence ends, however the name it is drawn by opens, and that row needs a page that
 * names the statement. The case "a person's quote that cites a labelled statement ends where
 * their sentence ends" in `spec-filter/spec-decided-by.test.ts` pins it.
 */
export const SENTENCE_TABLE: readonly SentenceRow[] = [
  // A full stop before a word in lower case ends an abbreviation.
  {
    first: "Keep the memo, e.g. in the pane.",
    id: "a1",
    stored: "Keep the memo, e.g. in the pane.",
    summary: true,
    text: "Keep the memo, e.g. in the pane. Then more.",
  },
  // A number after a listed abbreviation is part of the sentence and opens no label.
  {
    first: "Use the old page (see p. 4) for now.",
    id: "a2",
    stored: "Use the old page (see p. 4) for now.",
    summary: true,
    text: "Use the old page (see p. 4) for now. Then more.",
  },
  {
    first: "Draw it whole, as on p. 4.",
    id: "a3",
    stored: "Draw it whole, as on p. 4.",
    summary: true,
    text: "Draw it whole, as on p. 4. Then more words follow.",
  },
  {
    first: "Draw it whole, as on p. 1.",
    id: "a4",
    stored: "Draw it whole, as on p. 1.",
    summary: true,
    text: "Draw it whole, as on p. 1. Then more words follow.",
  },
  {
    first: "See fig. 3.",
    id: "a5",
    stored: "See fig. 3.",
    summary: true,
    text: "See fig. 3. It shows the cut.",
  },
  {
    first: "See fig. 1.",
    id: "a6",
    stored: "See fig. 1.",
    summary: true,
    text: "See fig. 1. It shows the cut.",
  },
  // A number that holds a full stop.
  {
    first: "Keep v. 1.2 for now.",
    id: "a7",
    stored: "Keep v. 1.2 for now.",
    summary: true,
    text: "Keep v. 1.2 for now. Then move.",
  },
  // The abbreviations are read without regard to case.
  {
    first: "See Fig. 3.",
    id: "a8",
    stored: "See Fig. 3.",
    summary: true,
    text: "See Fig. 3. It shows the cut.",
  },
  // A full stop before a number ends the sentence where no listed abbreviation stands before it.
  {
    first: "We shipped it in 2026.",
    id: "n1",
    stored: "We shipped it in 2026.",
    summary: true,
    text: "We shipped it in 2026. 3 items are left.",
  },
  // The rule's first accepted miss: "no." before a number is far more often "number" than a
  // sentence's last word, so the text runs as one sentence.
  {
    first: "The answer is no. 3 rows changed.",
    id: "n2",
    stored: "The answer is no. 3 rows changed.",
    summary: true,
    text: "The answer is no. 3 rows changed.",
  },
  // The rule's second accepted miss: the list holds abbreviations before a number only, so a
  // full stop after an abbreviation before a word in upper case ends the sentence.
  {
    first: "Ask Dr.",
    id: "n3",
    stored: "Ask Dr.",
    summary: true,
    text: "Ask Dr. Smith. Then more.",
  },
  // Each listed abbreviation before a number, a row of its own, so a list that lost any one of
  // them would fail a row.
  {
    first: "See art. 2 for it.",
    id: "b1",
    stored: "See art. 2 for it.",
    summary: true,
    text: "See art. 2 for it. Then more.",
  },
  {
    first: "See cf. 2 for it.",
    id: "b2",
    stored: "See cf. 2 for it.",
    summary: true,
    text: "See cf. 2 for it. Then more.",
  },
  {
    first: "See ch. 2 for it.",
    id: "b3",
    stored: "See ch. 2 for it.",
    summary: true,
    text: "See ch. 2 for it. Then more.",
  },
  {
    first: "See eq. 2 for it.",
    id: "b4",
    stored: "See eq. 2 for it.",
    summary: true,
    text: "See eq. 2 for it. Then more.",
  },
  {
    first: "See eqs. 2 for it.",
    id: "b5",
    stored: "See eqs. 2 for it.",
    summary: true,
    text: "See eqs. 2 for it. Then more.",
  },
  {
    first: "See figs. 2 for it.",
    id: "b6",
    stored: "See figs. 2 for it.",
    summary: true,
    text: "See figs. 2 for it. Then more.",
  },
  {
    first: "See nos. 2 for it.",
    id: "b7",
    stored: "See nos. 2 for it.",
    summary: true,
    text: "See nos. 2 for it. Then more.",
  },
  {
    first: "See para. 2 for it.",
    id: "b8",
    stored: "See para. 2 for it.",
    summary: true,
    text: "See para. 2 for it. Then more.",
  },
  {
    first: "See pp. 2 for it.",
    id: "b9",
    stored: "See pp. 2 for it.",
    summary: true,
    text: "See pp. 2 for it. Then more.",
  },
  {
    first: "See sec. 2 for it.",
    id: "b10",
    stored: "See sec. 2 for it.",
    summary: true,
    text: "See sec. 2 for it. Then more.",
  },
  {
    first: "See vol. 2 for it.",
    id: "b11",
    stored: "See vol. 2 for it.",
    summary: true,
    text: "See vol. 2 for it. Then more.",
  },
  {
    first: "See vs. 2 for it.",
    id: "b12",
    stored: "See vs. 2 for it.",
    summary: true,
    text: "See vs. 2 for it. Then more.",
  },
  // A closing quote or bracket after the mark is part of the sentence.
  {
    first: 'He said "go."',
    id: "q1",
    stored: 'He said "go."',
    summary: true,
    text: 'He said "go." Then he went.',
  },
  {
    first: "Keep it (as written.)",
    id: "q2",
    stored: "Keep it (as written.)",
    summary: true,
    text: "Keep it (as written.) Then more.",
  },
  {
    first: "She wrote \u201cdone.\u201d",
    id: "q3",
    stored: "She wrote \u201cdone.\u201d",
    summary: true,
    text: "She wrote \u201cdone.\u201d Then she left.",
  },
  {
    first: "Keep it 'as is.'",
    id: "q4",
    stored: "Keep it 'as is.'",
    summary: true,
    text: "Keep it 'as is.' Then more.",
  },
  // A question or exclamation mark ends the sentence before any word, in lower case too.
  {
    first: "Is this it?",
    id: "x1",
    stored: "Is this it?",
    summary: true,
    text: "Is this it? yes, it is. Then more.",
  },
  {
    first: "Stop!",
    id: "x2",
    stored: "Stop!",
    summary: true,
    text: "Stop! now it works. Then more.",
  },
  {
    first: "Does the built page work?",
    id: "x3",
    stored: "Does the built page work?",
    summary: true,
    text: "Does the built page work? Yes.",
  },
  // A leading label ends no sentence.
  {
    first: "R1. Every node is whole first.",
    id: "l1",
    stored: "R1. Every node is whole first.",
    summary: true,
    text: "R1. Every node is whole first. Then more.",
  },
  // A leading number at the start of a stored text is a list marker in markdown.
  {
    first: "12. The page draws it.",
    id: "l2",
    stored: "The page draws it.",
    summary: false,
    text: "12. The page draws it. Then more.",
  },
  // A full stop before a numbered label ends the sentence before it.
  {
    first: "Two steps follow.",
    id: "s1",
    stored: "Two steps follow.",
    summary: true,
    text: "Two steps follow. 1. Open it. 2. Close it.",
  },
  {
    first: "Two steps follow.",
    id: "s2",
    stored: "Two steps follow.",
    summary: true,
    text: "Two steps follow. 1. Open it.",
  },
  {
    first: "Keep the old page.",
    id: "s3",
    stored: "Keep the old page.",
    summary: true,
    text: "Keep the old page. 2. Then drop it.",
  },
  // A label may follow a question or exclamation mark as well.
  {
    first: "Two steps follow.",
    id: "s4",
    stored: "Two steps follow.",
    summary: true,
    text: "Two steps follow. 1. Open it! 2. Close it.",
  },
  // A number after a listed abbreviation opens no label.
  {
    first: "Read p. 1.",
    id: "s5",
    stored: "Read p. 1.",
    summary: true,
    text: "Read p. 1. Then read p. 2. Then stop.",
  },
  // A label may follow a colon, which ends no sentence.
  {
    first: "Steps: 1. Open it.",
    id: "s6",
    stored: "Steps: 1. Open it.",
    summary: true,
    text: "Steps: 1. Open it. 2. Close it.",
  },
  // A list typed at the start: the function reads its marker as a label, and a stored text's
  // readers read the list's first item, whose marker is no word.
  {
    first: "1. Keep the old page.",
    id: "t1",
    stored: "Keep the old page.",
    summary: false,
    text: "1. Keep the old page. 2. Then drop it.",
  },
  {
    first: "1. Keep the old page.",
    id: "t2",
    stored: "Keep the old page.",
    summary: false,
    text: "1. Keep the old page.\n2. Then drop it.",
  },
  {
    first: "3. Keep the old page.",
    id: "t3",
    stored: "Keep the old page.",
    summary: false,
    text: "3. Keep the old page. 4. Then drop it.",
  },
  {
    first: "1. Keep the old page.",
    id: "t4",
    stored: "Keep the old page.",
    summary: false,
    text: "1. Keep the old page. Then drop it.",
  },
  // A text stored as blocks, whose first sentence a quote and a Contents entry read from the words
  // the stored text opens with (`issue-tree.ts`'s `firstBlockWordsRaw`). A paragraph, then a list:
  // the function's answer is the paragraph's sentence too.
  {
    first: "Read this first.",
    id: "k1",
    stored: "Read this first.",
    summary: true,
    text: "Read this first.\n\n- one\n- two",
  },
  // Two paragraphs, and the first holds no sentence end.
  {
    first: undefined,
    id: "k2",
    stored: "First part",
    summary: true,
    text: "First part\n\nSecond part.",
  },
  // A first item that holds a list of its own.
  {
    first: undefined,
    id: "k3",
    stored: "one",
    summary: false,
    text: "- one\n  - inner\n- two",
  },
  // A first item that holds a later paragraph.
  {
    first: undefined,
    id: "k4",
    stored: "one",
    summary: false,
    text: "- one\n\n  more of one.\n- two",
  },
  // A list after a line with no blank line between opens a block of its own, and the colon that
  // ends the line ends no sentence.
  {
    first: undefined,
    id: "k5",
    stored: "Pick one:",
    summary: false,
    text: "Pick one:\n- keep it\n- drop it",
  },
  // A block quote of two paragraphs, read by its first paragraph alone.
  {
    first: undefined,
    id: "k6",
    stored: "Yes",
    summary: false,
    text: "> Yes\n>\n> Keep the old page.",
  },
];
