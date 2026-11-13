import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import type { Artifact } from "../../api/types";
import {
  borderDefault,
  linkHoverText,
  linkText,
  textMutedOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { presetPath } from "./issue-address";
import type { TreeNode } from "./issue-tree";
import { NodeGlyph } from "./NodeGlyph";
import { authorText, pageChrome } from "./page-structure";
import {
  approvalStateWord,
  type CheckState,
  CONTROL_LABELS,
  EMPTY_STATE_LINES,
  GAP_WORDS,
  GLYPH_TOKENS,
  NODE_STATE_WORDS,
  PRESET_NAMES,
} from "./page-words";

/**
 * The head of the page: the issue's three sentences, and the two rows under them
 * (dispatch://INFRA-1196 R5, `dispatch://INFRA-1196/artifact/plan-md#b-t10`).
 *
 * It is drawn above the presets and on all five, because what the issue is does not change
 * with the query a reader is running over it.
 *
 * The sentences are the spec's summary, as the spec stores them: the summary node's own text,
 * read as the sentences it holds, and at most the three the requirement names - what this is,
 * why it matters, and what is different when it is done. An author who wrote two sentences gets
 * two, with nothing drawn in the place of a third. The text is the node's own (`specSummary`),
 * which the tree the page already draws carries, so the head reads no document and costs no
 * request of its own (R20). That text is the server's stored excerpt of the block, and no read
 * the first load makes sends the server's reading of it (`ReferenceTarget` carries `name` and
 * nothing beside it), so the head reads the excerpt's own characters for three things and no
 * more: the block's fence, a list item's marker and the emphasis around words
 * (`spokenSentences`). It parses no stored markdown. A text that ends mid-sentence - the
 * excerpt is cut at `BlockExcerptRunes`, 480 runes (`packages/envoy/internal/dispatch/refs/excerpts.go`)
 * - ends in a fragment nobody wrote, and a fragment is not a spoken sentence, so it is left
 * undrawn rather than drawn cut off. Who wrote the bullets and when they were last read back are
 * dispatch://INFRA-1278 and the model holds neither, so the head says so in two gaps and draws
 * no author at all - it never guesses one from the version the summary last changed in.
 *
 * Until the reads that carry the summary answer, the head says nothing about it: the tree is
 * drawn before the spec's blocks arrive, and its summary node before the batched reference read
 * names it, and a summary not read yet is no spec without one.
 *
 * The two rows are the page's own words and hold no author's text, so the sentences above them
 * are the only thing a reader of the head quotes. Each names one source and no second: the
 * Plan preset with how the plan stands with its reviewers, and the Checks preset with the
 * state every criterion's latest result is in. The count behind each of those states is in the
 * preset itself, one row per criterion, which is one press away.
 */
export function IssueThreeThings({
  checks,
  issueKey,
  plan,
  planTasks,
  reading,
  summary,
}: {
  /** Every check of the issue, as the tree holds them: each one's state is its latest result. */
  checks: readonly TreeNode[];
  issueKey: string;
  /** The issue's plan document, where it has one. */
  plan: Artifact | undefined;
  /** How many plan tasks the Plan preset holds. */
  planTasks: number;
  /** Whether the spec's blocks are still being read, which is what says whether the spec holds
   *  a summary node at all. */
  reading: boolean;
  /** The spec's summary node, or undefined for a spec that carries none. */
  summary: TreeNode | undefined;
}): ReactNode {
  const sentences = spokenSentences(summary?.text ?? "");
  // A summary node with no text is one whose words the batched reference read has not named
  // yet - a stored block always holds its own fence - so it says no more than a summary whose
  // blocks are still out.
  const known = !reading && (summary === undefined || summary.text !== "");
  const states = Object.keys(NODE_STATE_WORDS.check).filter((state) =>
    checks.some((check) => check.state === state)
  ) as CheckState[];
  const approval = plan?.approval?.state;
  return (
    <section
      className={`mb-0.5 flex min-w-0 flex-col gap-1 border-b pb-0.5 ${borderDefault}`}
      data-testid="issue-three-things"
    >
      {sentences.length > 0 ? (
        <ul className="flex min-w-0 flex-col">
          {sentences.map((sentence) => (
            <li
              className={`text-sm leading-tight break-words ${textSecondaryOnCanvas}`}
              key={sentence}
              {...authorText}
            >
              {sentence}
            </li>
          ))}
        </ul>
      ) : known ? (
        <p className={`text-sm ${textMutedOnCanvas}`} {...pageChrome}>
          {EMPTY_STATE_LINES.spec_summary}
        </p>
      ) : null}
      {/* The two rows and the two gaps share one wrapping line. Each keeps its own box, because
          each names one source and no second (R5), but a line apiece cost the head 60 px before
          the tree and R6 puts the first open question on the first screen. The gap words are
          what the model does not hold (dispatch://INFRA-1278): the chips name it, and the
          sentences that spelled it out are gone with the line they cost. */}
      <div className="flex min-w-0 flex-wrap items-baseline gap-x-4 gap-y-0.5">
        <p
          className={`flex min-w-0 flex-wrap items-baseline gap-x-2 text-sm ${textMutedOnCanvas}`}
          data-testid="issue-plan-row"
        >
          <span {...pageChrome}>{CONTROL_LABELS.how_it_gets_done}</span>
          {/* `dispatch-inline-link`: below 1280 px every other link is a 44 px control box
              (`styles.css`), and two of them here cost the head two lines the first open
              question needs (R6). A link inside a line of text keeps its line box and grows
              its hit area by padding instead. */}
          <Link
            className={`dispatch-inline-link underline ${linkText} ${linkHoverText}`}
            to={presetPath(issueKey, "plan")}
          >
            <span {...pageChrome}>{PRESET_NAMES.plan}</span>
          </Link>
          {planTasks === 0 ? (
            <span {...pageChrome}>{EMPTY_STATE_LINES.plan}</span>
          ) : approval === undefined ? null : (
            <span {...pageChrome}>{approvalStateWord(approval)}</span>
          )}
        </p>
        <p
          className={`flex min-w-0 flex-wrap items-baseline gap-x-2 text-sm ${textMutedOnCanvas}`}
          data-testid="issue-checks-row"
        >
          <span {...pageChrome}>{CONTROL_LABELS.what_you_will_be_shown}</span>
          <Link
            className={`dispatch-inline-link underline ${linkText} ${linkHoverText}`}
            to={presetPath(issueKey, "checks")}
          >
            <span {...pageChrome}>{PRESET_NAMES.checks}</span>
          </Link>
          {states.length === 0 ? (
            <span {...pageChrome}>{EMPTY_STATE_LINES.checks}</span>
          ) : (
            states.map((state) => (
              <span className="flex items-center gap-1" key={state} {...pageChrome}>
                <NodeGlyph token={GLYPH_TOKENS.states.check[state]} />
                <span>{NODE_STATE_WORDS.check[state]}</span>
              </span>
            ))
          )}
        </p>
        <span className="flex min-w-0 flex-wrap items-baseline gap-2 text-xs">
          {(["no_summary_author", "no_read_back"] as const).map((gap) => (
            <span
              className={`rounded border px-1 ${borderDefault} ${textMutedOnCanvas}`}
              data-gap=""
              data-gap-kind={gap}
              key={gap}
              {...pageChrome}
            >
              {GAP_WORDS[gap]}
            </span>
          ))}
        </span>
      </div>
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
 */
function spokenSentences(text: string): readonly string[] {
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
  return (ended ? sentences : sentences.slice(0, -1))
    .slice(0, BULLETS)
    .map(({ sentence }) => sentence);
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
