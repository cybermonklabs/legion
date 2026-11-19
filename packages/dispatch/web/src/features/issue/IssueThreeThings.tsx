import type { ReactNode } from "react";

import {
  pageInkBg,
  pageInkText,
  pageLineBorder,
  pageMutedText,
  pageQuietText,
} from "../../theme/classes";
import type { TreeNode } from "./issue-tree";
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
 * matters, done when (dispatch://INFRA-1196 R8, the architect's ruling 5).
 *
 * Where the approved pictures draw them: with no filter on, at the head of the Contents pane from
 * 1024 px up (`look="pane"`) and in the Outline sheet below that (`look="sheet"`, never above
 * the outline, where they cost a phone's first screen 160 px to 239 px); under the Spec filter,
 * as its first group (`look="spec"`, `spec-filter/`). Under Verification, Plan and Decisions
 * they are not drawn. The two rows the spec named under them, "How it gets done" and "What you
 * will be shown", are not drawn at any width: the Plan and Verification controls' signals say
 * the same (ruling 5).
 *
 * The sentences are the spec's summary, as the spec stores them: the summary node's own text,
 * read as the sentences it holds, and at most the three the requirement names. An author who
 * wrote two sentences gets two, with nothing drawn in the place of a third. The text is the
 * node's own (`specSummary`), which the tree the page already draws carries, so the head reads no
 * document and costs no request of its own (R20). That text is the server's stored excerpt of
 * the block, and no read the first load makes sends the server's reading of it, so the head reads
 * the excerpt's own characters for three things and no more: the block's fence, a list item's
 * marker and the emphasis around words (`spokenSentences`). It parses no stored markdown. A text
 * that ends mid-sentence - the excerpt is cut at `BlockExcerptRunes`, 480 runes
 * (`packages/envoy/internal/dispatch/refs/excerpts.go`) - ends in a fragment nobody wrote, so it
 * is left undrawn rather than drawn cut off. Who wrote the sentences and when ("from your comment
 * of 5 Nov" in the pictures) is dispatch://INFRA-1278, and the model holds neither for any issue,
 * so the head draws no author.
 *
 * Until the reads that carry the summary answer, the head says nothing about it: the tree is
 * drawn before the spec's blocks arrive, and its summary node before the batched reference read
 * names it, and a summary not read yet is no spec without one.
 */
export function IssueThreeThings({
  look,
  reading,
  summary,
}: {
  /** Where it is drawn: the Contents pane, the Outline sheet, or the Spec filter's first group. */
  look: "pane" | "sheet" | "spec";
  /** Whether the spec's blocks are still being read, which is what says whether the spec holds
   *  a summary node at all. */
  reading: boolean;
  /** The spec's summary node, or undefined for a spec that carries none. */
  summary: TreeNode | undefined;
}): ReactNode {
  const { left, sentences } = spokenSentences(summary?.text ?? "");
  // A summary node with no text is one whose words the batched reference read has not named
  // yet - a stored block always holds its own fence - so it says no more than a summary whose
  // blocks are still out.
  const known = !reading && (summary === undefined || summary.text !== "");
  const spec = look === "spec";
  return (
    <section
      className={`flex min-w-0 shrink-0 flex-col ${
        spec ? "mb-4" : `border-b ${pageLineBorder} px-3.5 pt-3 pb-2.5`
      }`}
      data-testid="issue-three-things"
    >
      <h2
        className={`text-xs leading-4 font-semibold tracking-[0.08em] uppercase ${pageQuietText}`}
        {...pageChrome}
      >
        {CONTROL_LABELS.in_short}
      </h2>
      {sentences.length > 0 ? (
        <ul className={`mt-1.5 mb-1 flex min-w-0 flex-col ${spec ? "gap-0.5" : ""}`}>
          {sentences.map((sentence, index) => (
            <li
              className={`relative break-words ${pageInkText} ${
                spec ? "pl-5 text-[15px] leading-[26px]" : "mb-1 pl-3 text-sm leading-5"
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
                    spec ? "top-[9px] left-0.5 size-[7px]" : "top-2 left-px size-1"
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
      ) : known ? (
        // A summary whose words hold no whole sentence is a summary all the same, so the line
        // says why none is drawn rather than that the spec has none.
        <p className={`mt-1.5 mb-1 text-sm leading-5 ${pageMutedText}`} {...pageChrome}>
          {left === "cut"
            ? EMPTY_STATE_LINES.spec_summary_cut
            : left === "unended"
              ? EMPTY_STATE_LINES.spec_summary_unended
              : EMPTY_STATE_LINES.spec_summary}
        </p>
      ) : null}
    </section>
  );
}

/** How many bullets the head draws at most: the three the requirement names (R5). */
const BULLETS = 3;

/** A whole sentence ends at a sentence end, with whatever closing quote or bracket the writer
 *  put after it. Only the last sentence of a split can fail this, and it fails exactly when the
 *  text it was split out of stops mid-sentence. */
const ENDS_A_SENTENCE = /[.!?][)\]"'\u201d\u2019]*$/u;

/** The space after a sentence end, which is where a run of words is split into its sentences:
 *  the end may carry a closing quote or bracket, and the emphasis that closes around it. */
const SENTENCE_BREAK = /(?<=[.!?][)\]"'\u201d\u2019*_]*)\s+/u;

/** Where a list item opens: its marker - `-`, `*` or `+`, or a number and `.` or `)` - and the
 *  space after it. The marker is the list's syntax; the words after it are the item. */
const LIST_ITEM = /^\s{0,3}(?:[-*+]|\d{1,9}[.)])\s+/u;

/** The emphasis around an author's words: a run of `*` or `_` that opens a word or closes one.
 *  One inside a word (`snake_case`), or standing between two spaces (`2 * 3`), is a character
 *  the author wrote, and stays. */
const EMPHASIS =
  /(^|[\s([{"'\u201c\u2018])[*_]{1,3}(?=[^\s*_])|(?<=[^\s*_])[*_]{1,3}(?=$|[\s.,;:!?)\]}"'\u201d\u2019])/gu;

/**
 * The sentences the head draws: the summary's own, each one the words its author wrote, and at
 * most `BULLETS` of them (R5).
 *
 * The text is the block's stored excerpt, which is its source, so three things in it are the
 * document's syntax and no part of what anybody wrote: the `:::` fence around the block, the
 * marker that opens a list item, and the `*` and `_` around emphasised words. Each is left out;
 * nothing else is read, and nothing is added, reordered or reflowed. The words are read in the
 * runs they were written in - each list item one, and each paragraph one - and each run is
 * split at its sentence ends. A list item is a sentence of its own whether or not its author
 * ended it with a full stop, because the item ends where the next one starts.
 *
 * A trailing fragment is dropped. The excerpt is cut at `BlockExcerptRunes` wherever the block
 * is longer, so a text that stops mid-sentence ends in words no author ever spoke as a
 * sentence; it is left out rather than drawn as a bullet of its own. A last list item is whole
 * where the excerpt is - it holds the block's closing fence - and a fragment like any other
 * where the cut fell inside it.
 *
 * `left` says why words were left with no sentence drawn for them, where the fragment was all
 * the text held: `cut`, the excerpt stopped inside its first sentence, and `unended`, the whole
 * block ends in no sentence end. It is undefined where a sentence is drawn, and where the text
 * holds no words at all.
 */
function spokenSentences(text: string): {
  readonly left?: "cut" | "unended";
  readonly sentences: readonly string[];
} {
  const lines = text.split("\n");
  if (lines[0]?.startsWith(":::") === true) lines.shift();
  // The excerpt holds the closing fence exactly when it holds the whole block: a longer block
  // is cut wherever `BlockExcerptRunes` falls, and its fence goes with the rest.
  const whole = lines.at(-1)?.trim() === ":::";
  if (whole) lines.pop();
  // A line after a list item or a paragraph's line, with no blank line between, carries it on.
  const runs: { readonly item: boolean; words: string }[] = [];
  let open = false;
  for (const line of lines) {
    const marker = LIST_ITEM.exec(line);
    const last = runs.at(-1);
    if (marker !== null) {
      runs.push({ item: true, words: line.slice(marker[0].length) });
      open = true;
    } else if (line.trim() === "") {
      open = false;
    } else if (open && last !== undefined) {
      last.words = `${last.words} ${line}`;
    } else {
      runs.push({ item: false, words: line });
      open = true;
    }
  }
  const sentences = runs.flatMap(({ item, words }) =>
    words
      .replace(/\s+/gu, " ")
      .trim()
      .split(SENTENCE_BREAK)
      .map((sentence) => sentence.replace(EMPHASIS, "$1").trim())
      .filter((sentence) => sentence !== "")
      .map((sentence) => ({ item, sentence }))
  );
  const last = sentences.at(-1);
  const ended = last === undefined || ENDS_A_SENTENCE.test(last.sentence) || (whole && last.item);
  const spoken = (ended ? sentences : sentences.slice(0, -1))
    .slice(0, BULLETS)
    .map(({ sentence }) => sentence);
  if (spoken.length > 0 || ended) return { sentences: spoken };
  return { left: whole ? "unended" : "cut", sentences: spoken };
}

/** The issue's summary: the first `callout` block of its spec, which is the nearest thing
 *  today's block schema holds to a summary node (dispatch://INFRA-1278 adds one). */
export function specSummary(root: TreeNode, specSlug: string): TreeNode | undefined {
  const prefix = `${specSlug}#b-`;
  const walk = (node: TreeNode): TreeNode | undefined => {
    if (node.type === "callout" && node.id.startsWith(prefix)) return node;
    for (const child of node.children) {
      const found = walk(child);
      if (found !== undefined) return found;
    }
    return undefined;
  };
  return specSlug === "" ? undefined : walk(root);
}

/** Every node of `type` the tree holds, in reading order. */
export function nodesOfType(root: TreeNode, type: TreeNode["type"]): TreeNode[] {
  const found: TreeNode[] = [];
  const walk = (node: TreeNode): void => {
    if (node.type === type) found.push(node);
    for (const child of node.children) walk(child);
  };
  walk(root);
  return found;
}
