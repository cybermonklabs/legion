import { type ReactNode, useMemo } from "react";

import type { DisplayTree } from "../../api/types";
import { pageInkBg, pageInkText, pageLineBorder, pageMutedText } from "../../theme/classes";
import { wordsOf } from "../refs/tree-fragment";
import { drawnLine, firstLineOf, type TreeNode } from "./issue-tree";
import { CAP, LEADING_LABEL, referenceStandIns } from "./one-line";
import { authorText, pageChrome } from "./page-structure";
import { CONTROL_LABELS, EMPTY_STATE_LINES } from "./page-words";

/** Each sentence's label, in the order the summary holds them (R8). */
const LABELS = [
  CONTROL_LABELS.in_short_what,
  CONTROL_LABELS.in_short_why,
  CONTROL_LABELS.in_short_done,
] as const;

/**
 * The issue's three sentences under "In short", each after its label - what it is, why it
 * matters, done when (R1, R8).
 *
 * Where the approved pictures draw them: with no view on, in `slots/contents.tsx`
 * (`look="contents"`), which the page's frame draws at the head of the Contents pane from
 * 1024 px up and in the Outline sheet below that (never above the outline, where they cost a
 * phone's first screen 160 px to 239 px); under the Spec view, as its first group
 * (`look="spec"`, `spec-filter/`). Under Verification, Plan and Decisions they are not drawn.
 * The two rows the spec named under them, "How it gets done" and "What you will be shown", are
 * not drawn at any width: the Plan and Verification controls' signals say the same.
 *
 * The sentences are the spec's summary as its author wrote them, read from the server's reading
 * of the summary node's whole text (`TreeNode.tree`, the batched reference read's `text_tree`)
 * and never from the node's stored excerpt (`TreeNode.text`), which is the block's source cut at
 * 480 runes: the reading holds no fence, list marker or emphasis mark to leave out and ends
 * nowhere a cut fell. The tree the page already draws carries it (`specSummary`), so the head
 * reads no document and costs no request of its own (R20). It draws at most the three sentences
 * the requirement names, and an author who wrote two gets two, with nothing drawn in the place
 * of a third (`summarySentences`). A reference the sentences cite is named in plain words, by
 * the one name the page gives it everywhere (`referenceStandIns`, with the `nameOf` its slot
 * makes once over the whole tree), once the sentences are split. Who wrote the sentences and
 * when ("from your comment of 5 Nov" in the pictures) waits on a read that holds them, and the
 * model holds neither for any issue, so the head draws no author.
 *
 * Until the reads that carry the summary answer, the head says nothing about it: the tree is
 * drawn before the spec's blocks arrive, and its summary node before the batched reference read
 * sends that node's reading, and a summary not read yet is no spec without one. Only a spec the
 * reads have answered for with no summary node says it has none, and an issue with no spec at
 * all says nothing of a summary.
 */
export function IssueThreeThings({
  hasSpec,
  look,
  nameOf,
  reading,
  summary,
}: {
  /** Whether the issue has a primary spec, which is what can hold a summary: an issue with none
   *  has no summary to say is missing. */
  hasSpec: boolean;
  /** Where it is drawn: the Contents slot, which the pane and the Outline sheet draw alike, or
   *  the Spec view's first group. */
  look: "contents" | "spec";
  /** The page's name for a reference the sentences cite (`useReferenceNames`). */
  nameOf: (reference: string) => string | undefined;
  /** Whether the spec's blocks, which hold its summary, are not held yet, which is what says
   *  whether the spec holds a summary node at all. */
  reading: boolean;
  /** The spec's summary node, or undefined for a spec that carries none. */
  summary: TreeNode | undefined;
}): ReactNode {
  const tree = reading ? undefined : summary?.tree;
  const sentences = useMemo(
    () => (tree === undefined ? [] : summarySentences(tree, nameOf)),
    [nameOf, tree]
  );
  const spec = look === "spec";
  return (
    <section
      className={`flex min-w-0 shrink-0 flex-col ${
        spec ? "mb-4" : `border-b ${pageLineBorder} px-3.5 pt-3 pb-2.5`
      }`}
      data-testid="issue-three-things"
    >
      <h2 className={CAP} {...pageChrome}>
        {CONTROL_LABELS.in_short}
      </h2>
      {sentences.length > 0 ? (
        <ul className={`mt-1.5 mb-1 flex min-w-0 flex-col ${spec ? "gap-0.5" : ""}`}>
          {sentences.map((sentence, index) => (
            <li
              className={`relative break-words ${pageInkText} ${
                spec ? "pl-5 text-[15px] leading-6" : "mb-1 pl-3 text-sm leading-5"
              }`}
              // biome-ignore lint/suspicious/noArrayIndexKey: a summary may say one sentence twice, and its place is what tells the two apart.
              key={`${index}:${sentence}`}
            >
              {/* The Spec filter's group is one statement of three lines, one bullet (the
                  approved Spec picture); the pane's and the sheet's are three bullets. */}
              {spec && index > 0 ? null : (
                <span
                  aria-hidden="true"
                  className={`absolute rounded-full ${pageInkBg} ${
                    spec ? "top-2 left-0.5 size-[7px]" : "top-2 left-px size-1"
                  }`}
                />
              )}
              <span className="font-semibold" {...pageChrome}>
                {LABELS[index]}
              </span>{" "}
              <span {...authorText}>{sentence}</span>
            </li>
          ))}
        </ul>
      ) : hasSpec && !reading && summary === undefined ? (
        // A summary node whose reading has not come, or holds no words, is a summary all the
        // same, so only a spec with no summary node says it has none.
        <p className={`mt-1.5 mb-1 text-sm leading-5 ${pageMutedText}`} {...pageChrome}>
          {EMPTY_STATE_LINES.spec_summary}
        </p>
      ) : null}
    </section>
  );
}

/** How many bullets the head draws at most: the three the requirement names (R5). */
const BULLETS = 3;

/** A sentence end: its mark, with whatever closing quote or bracket the writer put after it,
 *  where the words stop, or where a space and a word that does not start in lower case follow:
 *  a mark before a word in lower case ends an abbreviation (`e.g. tester`), not a sentence. */
const SENTENCE_END = /[.!?][)\]"'\u201d\u2019]*(?=$|\s+[^\s\p{Ll}])/u;

/** How many characters of `words` its leading label takes: a first word that opens its
 *  sentence and ends none (`LEADING_LABEL`). */
function labelLength(words: string): number {
  return LEADING_LABEL.exec(words)?.[0].length ?? 0;
}

/** `words` split into its sentences, each to its sentence end with the leading label it opens
 *  with, and the last to where the words stop whether or not it has an end. */
function sentencesOf(words: string): string[] {
  const sentences: string[] = [];
  let rest = words;
  while (rest !== "") {
    const label = labelLength(rest);
    const end = SENTENCE_END.exec(rest.slice(label));
    const length = end === null ? rest.length : label + end.index + end[0].length;
    sentences.push(rest.slice(0, length));
    rest = rest.slice(length).trimStart();
  }
  return sentences;
}

/**
 * The sentences the head draws out of the summary's reading, and at most `BULLETS` of them
 * (R1).
 *
 * The words are read in the runs they were written in: each paragraph and each heading one, and
 * each item of a bulleted or numbered list one, its own list inside it included. A paragraph's
 * or a heading's run is split at its sentence ends (`sentencesOf`), of which a leading label and
 * a mark before a word in lower case are none. A list item is a sentence of its own whether or
 * not its author ended it with a mark, because the item ends where the next one starts, so a
 * list's last item is drawn too. A run's words are the reading's own (`wordsOf`), with each
 * reference in them named in plain words once the sentences are split (`referenceStandIns`), so
 * a name that opens with a label ("R1.") ends no sentence; nothing else is added, reordered or
 * reflowed.
 *
 * A summary whose runs hold no sentence end and no list item has no sentence to split out, so
 * its first line, by the one-line rule every place that draws one line for a node keeps
 * (`firstLineOf`, `drawnLine`, read past its leading label), is its one sentence.
 */
function summarySentences(
  tree: DisplayTree,
  nameOf: (reference: string) => string | undefined
): readonly string[] {
  const references = referenceStandIns(nameOf);
  const runs: { readonly item: boolean; readonly words: string }[] = [];
  const walk = (node: DisplayTree, inList: boolean): void => {
    const item = inList && node.type === "list_item";
    if (item || node.type === "paragraph" || node.type === "heading") {
      const words = references.standIn(wordsOf(node)).replace(/\s+/gu, " ").trim();
      if (words !== "") runs.push({ item, words });
      return;
    }
    const children: readonly DisplayTree[] = Array.isArray(node.content) ? node.content : [];
    const list = node.type === "bullet_list" || node.type === "ordered_list";
    for (const child of children) walk(child, list);
  };
  walk(tree, false);
  if (!runs.some(({ item, words }) => item || SENTENCE_END.test(words.slice(labelLength(words))))) {
    const whole = runs.map(({ words }) => words).join(" ");
    const label = whole.slice(0, labelLength(whole));
    const first = firstLineOf(whole.slice(label.length));
    // The label goes before the line and leaves its end where `first` read it, so the line ends
    // at its sentence's end exactly where `first` says it does (`sentence`).
    const line = drawnLine({ ...first, line: `${label} ${first.line}`.trim() });
    return line === "" ? [] : [references.named(line)];
  }
  return runs
    .flatMap(({ item, words }) => (item ? [words] : sentencesOf(words)))
    .slice(0, BULLETS)
    .map((sentence) => references.named(sentence));
}
