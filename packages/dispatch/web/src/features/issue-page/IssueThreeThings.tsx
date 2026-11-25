import { type ReactNode, useId, useMemo } from "react";
import { useNavigate } from "react-router-dom";

import type { DisplayTree } from "../../api/types";
import {
  pageInkBg,
  pageInkText,
  pageLineBorder,
  pageMutedText,
  pageStateText,
} from "../../theme/classes";
import { wordsOf } from "../refs/tree-fragment";
import { presetPath } from "./issue-address";
import {
  drawnLine,
  firstLineOf,
  firstSentenceLength,
  isCheck,
  LEADING_LABEL,
  type TreeNode,
} from "./issue-tree";
import { NodeGlyph } from "./NodeGlyph";
import { referenceStandIns } from "./one-line";
import { authorText, pageChrome } from "./page-structure";
import {
  type CheckState,
  CONTROL_LABELS,
  EMPTY_STATE_LINES,
  FILTER_SIGNAL_WORDS,
  GLYPH_TOKENS,
  NODE_STATE_WORDS,
  type NodeState,
  VERIFICATION_WORDS,
} from "./page-words";
import { filterSignals, type PresetSignal, presetView } from "./presets";
import { CAP, queryTyped } from "./verification-filter/VerificationFilterBody";

/** Each sentence's label, in the order the summary holds them (R8). */
const LABELS = [
  CONTROL_LABELS.in_short_what,
  CONTROL_LABELS.in_short_why,
  CONTROL_LABELS.in_short_done,
] as const;

// A copy of `VerificationFilterBody.tsx`'s private `CHECK_STATES`, its doc included: that file
// may export nothing but `CAP` in this change. The copy goes once that file exports its own,
// which this file then imports.
/** The seven states a check can be in, in the order the page declares them. */
const CHECK_STATES = Object.keys(NODE_STATE_WORDS.check) as CheckState[];

/** What the two rows under the sentences count (`briefCounts`). */
export interface BriefCounts {
  /** The issue's checks: how many there are, each once, and how many stand in each state that
   *  holds one, in `NODE_STATE_WORDS.check`'s order, so the counts add up to the number. Undefined
   *  where the issue holds no check. */
  readonly checks:
    | {
        readonly count: number;
        readonly states: readonly { readonly count: number; readonly state: CheckState }[];
      }
    | undefined;
  /** The Plan control's own signal: its count, and the noun the count is in. Undefined where the
   *  plan holds no task. */
  readonly plan: PresetSignal | undefined;
}

/**
 * What the two rows count, over the tree and the words filter the view row counts its signals over
 * (`filterSignals`), or undefined while the tree may not be counted yet. The view row waits until
 * every read the tree is built from holds its answer (`readsHeld`), and under a words filter the
 * names too (`namesHeld`): a read still out leaves nodes out of the tree, and the names give a
 * plan task its words, so a count taken sooner would read 0 for what the page has not read yet.
 * The rows wait as it does, and send no request of their own.
 *
 * "How it gets done" takes the Plan control's own signal. "What you will be shown" counts the
 * checks the Verification view draws, each once however many statements it serves, each in the
 * state that view draws it in, which is the check node's own `state` (`filterSignals` reads it the
 * same way for its flag); a block that is no check (`isCheck`) is counted nowhere. A count of 0
 * with no words typed says that the plan holds no task or the issue no check, which the rows
 * draw as the page's line for it; under a words filter that leaves none, the count is 0, as the
 * control says, since the plan or the issue may hold others that hold no such words.
 */
export function briefCounts(
  reads: { namesHeld: boolean; readsHeld: boolean; root: TreeNode },
  query: string
): BriefCounts | undefined {
  const { namesHeld, readsHeld, root } = reads;
  const filtered = queryTyped(query);
  if (!readsHeld || (filtered && !namesHeld)) return undefined;
  const inState = new Map<NodeState, number>();
  const seen = new Set<string>();
  const walk = (node: TreeNode): void => {
    const { state } = node;
    if (state !== undefined && isCheck(node) && !seen.has(node.id)) {
      seen.add(node.id);
      inState.set(state, (inState.get(state) ?? 0) + 1);
    }
    for (const child of node.children) walk(child);
  };
  walk(presetView(root, "verification", query).root);
  const { plan } = filterSignals(root, query);
  return {
    checks:
      seen.size === 0 && !filtered
        ? undefined
        : {
            count: seen.size,
            states: CHECK_STATES.flatMap((state) => {
              const count = inState.get(state) ?? 0;
              return count === 0 ? [] : [{ count, state }];
            }),
          },
    plan: plan.count === 0 && !filtered ? undefined : plan,
  };
}

/**
 * The issue's three sentences under "In short", each after its label - what it is, why it
 * matters, done when (R1) - and under them two rows, each a control that turns a filter on: "How
 * it gets done" the Plan filter, "What you will be shown" the Verification filter. The two rows
 * are drawn under the sentences in the Contents slot, which the page's frame draws at the head of
 * the Contents pane from 1024 px up and in the Outline sheet below that (`look="contents"`,
 * `slots/contents.tsx`), and in the Spec view's first group (`look="spec"`, `spec-filter/`).
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
 * sends that node's reading. A spec with no summary, and an issue with no spec, draw the two rows
 * alone, with no line about the summary.
 *
 * Under its label each row draws what the matching control of the view row draws (`counts`,
 * `briefCounts`): "How it gets done" the Plan control's count and noun, "What you will be shown"
 * the number of the issue's checks and, for each state that holds a check, its count beside the
 * state's word and shape. Where the plan holds no task or the issue no check, the row draws the
 * page's line for it. Until the counts may be drawn a row draws its label alone, so no 0 reads
 * as a count of what the page has not read.
 */
export function IssueThreeThings({
  counts,
  issueKey,
  look,
  nameOf,
  summary,
}: {
  /** What the two rows count, or undefined while the tree may not be counted yet
   *  (`briefCounts`). */
  counts: BriefCounts | undefined;
  /** The issue the two rows turn a filter on for. */
  issueKey: string;
  /** Where it is drawn: the Contents slot, which the pane and the Outline sheet draw alike, or
   *  the Spec view's first group. */
  look: "contents" | "spec";
  /** The page's name for a reference the sentences cite (`useReferenceNames`). */
  nameOf: (reference: string) => string | undefined;
  /** The spec's summary node, or undefined for a spec that carries none. */
  summary: TreeNode | undefined;
}): ReactNode {
  const tree = summary?.tree;
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
      ) : null}
      <div className="mt-2.5 grid gap-1.5">
        <BriefRow
          detail={counts === undefined ? undefined : <PlanCount plan={counts.plan} />}
          filter="plan"
          issueKey={issueKey}
          label={CONTROL_LABELS.in_short_how}
        />
        <BriefRow
          detail={counts === undefined ? undefined : <CheckCounts checks={counts.checks} />}
          filter="verification"
          issueKey={issueKey}
          label={CONTROL_LABELS.in_short_shown}
        />
      </div>
    </section>
  );
}

/**
 * One of the two rows (the prototype's `.brow`): its label, which is the control's name, a line of
 * its own in the ink and underlined, and under it what the row counts, in the muted ink. It is a
 * button, one tab stop, that turns `filter` on as the view row's control for it does
 * (`presetPath`), and is named by its label alone, with the counts as its description
 * (`aria-describedby`), as that control is, so the name a voice user says is the word they read.
 */
function BriefRow({
  detail,
  filter,
  issueKey,
  label,
}: {
  /** What stands under the label, or undefined while the counts may not be drawn. */
  detail: ReactNode | undefined;
  filter: "plan" | "verification";
  issueKey: string;
  label: string;
}): ReactNode {
  const id = useId();
  const navigate = useNavigate();
  return (
    <button
      aria-describedby={detail === undefined ? undefined : `${id}-counts`}
      aria-labelledby={`${id}-label`}
      // Below 1280 px every button takes `styles.css`'s 44 px floor, in no layer. A row is two
      // lines of 18 px, past the 24 px a target needs (WCAG 2.2's 2.5.8), so `min-h-0!` takes the
      // height off, and `items-start!` keeps the column's left edge, which the floor's
      // `align-items: center` would take.
      className={`group flex min-h-0! w-full flex-col items-start! text-left text-[13px] leading-[18px] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-page-ink ${pageMutedText}`}
      data-brief-row={filter}
      onClick={() => navigate(presetPath(issueKey, filter))}
      // A press leaves the focus where it is, as the view row's controls do.
      onMouseDown={(event) => event.preventDefault()}
      type="button"
    >
      <span
        className={`block font-semibold underline decoration-page-hair underline-offset-[3px] group-hover:decoration-page-ink group-focus-visible:decoration-page-ink ${pageInkText}`}
        id={`${id}-label`}
        {...pageChrome}
      >
        {label}
      </span>
      {detail === undefined ? null : (
        <span className="block min-w-0" data-brief-counts="" id={`${id}-counts`}>
          {detail}
        </span>
      )}
    </button>
  );
}

/** The Plan control's count and noun as it draws them ("13 steps"), or the page's line for a
 *  plan with no task. */
function PlanCount({ plan }: { plan: PresetSignal | undefined }): ReactNode {
  if (plan === undefined) return <span {...pageChrome}>{EMPTY_STATE_LINES.plan}</span>;
  return (
    <>
      <span {...authorText}>{plan.count}</span>
      {plan.unit === undefined ? null : (
        <span {...pageChrome}> {FILTER_SIGNAL_WORDS[plan.unit]}</span>
      )}
    </>
  );
}

// A copy of `VerificationFilterBody.tsx`'s private `checkTone`: that file may export nothing but
// `CAP` in this change, and the two rows colour a state as its legend does only while the two
// stay the same. The copy goes once that file exports its own, which this file then imports.
/** How a check's state is coloured, as the Verification view colours it: the passed green and the
 *  failed red, and the quiet grey for the five states that are neither. */
function stateTone(state: CheckState): string {
  if (state === "passed") return pageStateText.passed;
  return state === "failed" ? pageStateText.failed : pageStateText.quiet;
}

/** The number of the issue's checks and its noun ("6 checks", "1 check"), then each state that
 *  holds a check as its shape, its count and its word, or the page's line for an issue with no
 *  check. */
function CheckCounts({ checks }: { checks: BriefCounts["checks"] }): ReactNode {
  if (checks === undefined) return <span {...pageChrome}>{EMPTY_STATE_LINES.verification}</span>;
  return (
    <span className="flex flex-wrap items-center gap-x-3.5 gap-y-0.5">
      <span data-brief-total="">
        <ChecksNumber count={checks.count} />
      </span>
      {checks.states.map(({ count, state }) => (
        <span className="inline-flex items-center gap-[5px]" data-brief-state={state} key={state}>
          <NodeGlyph
            className={`size-3 shrink-0 ${stateTone(state)}`}
            token={GLYPH_TOKENS.states.check[state]}
          />
          <span {...authorText}>{count}</span>{" "}
          <span {...pageChrome}>{NODE_STATE_WORDS.check[state]}</span>
        </span>
      ))}
    </span>
  );
}

/** A number of checks and its noun ("6 checks", "1 check"): the second row's total, and a
 *  statement's count of checks in the Spec view (`SpecFilterBody.tsx`'s `CheckCount`). */
export function ChecksNumber({ count }: { count: number }): ReactNode {
  return (
    <>
      <span {...authorText}>{count}</span>
      <span {...pageChrome}>
        {" "}
        {count === 1 ? VERIFICATION_WORDS.check_one : VERIFICATION_WORDS.check_many}
      </span>
    </>
  );
}

/** How many bullets the head draws at most: the three the requirement names (R5). */
const BULLETS = 3;

/** `words` split into its sentences, each to its sentence end with the leading label it opens
 *  with (`firstSentenceLength`), and the last to where the words stop whether or not it has an
 *  end. Each sentence's end is read from where that sentence starts, so the labels of a numbered
 *  list follow on from one sentence to the next. */
function sentencesOf(words: string): string[] {
  const sentences: string[] = [];
  let rest = words;
  while (rest !== "") {
    const length = firstSentenceLength(rest) ?? rest.length;
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
 * or a heading's run is split where `firstSentenceLength` ends each of its sentences
 * (`sentencesOf`). A list item is a sentence of its own whether or not its author ended it with a
 * mark, because the item ends where the next one starts, so a list's last item is drawn too. A
 * run's words are the reading's own (`wordsOf`), with each reference in them named in plain words
 * once the sentences are split (`referenceStandIns`), so a name that opens with a label ("R1.")
 * ends no sentence; nothing else is added, reordered or reflowed.
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
  if (!runs.some(({ item, words }) => item || firstSentenceLength(words) !== undefined)) {
    const whole = runs.map(({ words }) => words).join(" ");
    const label = LEADING_LABEL.exec(whole)?.[0] ?? "";
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
