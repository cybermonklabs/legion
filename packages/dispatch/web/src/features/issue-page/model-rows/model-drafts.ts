import type { Actor, CheckResult } from "../../../api/types";
import { blockIdOfNode } from "../decision-review";
import type { CheckState, NodeType } from "../page-words";
import { type CheckStateReads, checkStanding, type Draft } from "../tree-drafts";
import type { TreeDraftContext, TreeDraftStep } from "../tree-read/tree-nodes";
import type { TreeAnswer, TreeAnswerNode } from "../tree-read/tree-read";

/**
 * The model's rows from the tree read: the steps this part puts first in T's `treeDraftSteps`
 * (`tree-read/tree-nodes.ts`), each answering only kinds no step of T's answers.
 *
 * - **A criterion of this issue** stands under the conversation node the read names as its
 *   parent, with its author and time (R1).
 * - **A borrowed check or plan task** - another issue's, which the read reaches through its
 *   `serves` - stands under each statement of this issue it serves, in the order its tuples name
 *   them, and never under the child issue's node; one that serves none of this issue's
 *   statements is drawn nowhere (R4). A borrowed check stands as F's one reading of a check reads
 *   it (`checkStanding`), over the result nodes the read answers under it (`borrowedStanding`).
 * - **A borrowed result** stands under the borrowed check it was recorded against, with who
 *   recorded it and when.
 *
 * A reading is T's own step (`draftReading`), which draws its author too (R3): no step here
 * drafts one.
 *
 * T's four helpers are handed in (`DraftParts`), and this file imports only types from
 * `tree-read/tree-nodes.ts`, so T's file importing this one closes no import cycle (`biome.json`'s
 * `noImportCycles`, which ignores type imports).
 */

/** T's own helpers a step here drafts with (`tree-read/tree-nodes.ts`): a draft as every kind
 *  starts one, whether the read marks a node as another issue's, whether a node is a statement of
 *  this issue, and a node's author and time. */
export interface DraftParts {
  readonly draftOf: (node: TreeAnswerNode, type: NodeType, context: TreeDraftContext) => Draft;
  readonly isForeign: (node: TreeAnswerNode, key: string) => boolean;
  readonly isStatementNode: (node: TreeAnswerNode, context: TreeDraftContext) => boolean;
  readonly madeBy: (draft: Draft, node: TreeAnswerNode) => void;
}

/** The steps this part adds to `treeDraftSteps`, drafting with T's helpers `parts`. */
export function modelDraftSteps({
  draftOf,
  isForeign,
  isStatementNode,
  madeBy,
}: DraftParts): readonly TreeDraftStep[] {
  /** A criterion of this issue: its words and its author and time, under the parent the read
   *  gives it (`placeOf`), a statement whose read parent it is under it. Written into the issue,
   *  it carries no `document`, so it keeps its bare read id and no address. */
  const draftCriterion: TreeDraftStep = (node, _answer, context) => {
    if (node.supertag !== "criterion" || isForeign(node, context.key)) return undefined;
    const draft = draftOf(node, "criterion", context);
    madeBy(draft, node);
    return draft;
  };

  /** A check or a plan task of another issue: under each statement of this issue its `serves`
   *  tuples name, in their order, with its address in its own issue (`draftOf`) and no author or
   *  time, as this issue's own checks draw none; none where it serves no statement of this
   *  issue. A check stands as its result nodes say (`borrowedStanding`). */
  const draftBorrowed: TreeDraftStep = (node, answer, context) => {
    if (
      (node.supertag !== "check" && node.supertag !== "plan_task") ||
      !isForeign(node, context.key)
    ) {
      return undefined;
    }
    const statements = context.tuplesFrom(node.id).flatMap((tuple) => {
      const target = tuple.kind === "serves" ? context.nodeOf(tuple.to.id) : undefined;
      return target !== undefined && isStatementNode(target, context)
        ? [context.treeIdOf(target.id)]
        : [];
    });
    if (statements.length === 0) return undefined;
    const draft = draftOf(node, node.supertag, context);
    draft.parents = [...new Set(statements)];
    const state = node.supertag === "check" ? borrowedStanding(node, answer) : undefined;
    if (state !== undefined) draft.state = state;
    return draft;
  };

  /** A result of another issue whose read parent is a borrowed check drafted here: who recorded
   *  it and when, under that check (`placeOf`). T drafts no check of another issue, so a draft
   *  standing for such a check is this part's. */
  const draftBorrowedResult: TreeDraftStep = (node, _answer, context) => {
    if (node.supertag !== "result" || !isForeign(node, context.key) || node.parent === null) {
      return undefined;
    }
    const check = context.nodeOf(node.parent.id);
    if (
      check === undefined ||
      check.supertag !== "check" ||
      !isForeign(check, context.key) ||
      !context.drafted(check.id)
    ) {
      return undefined;
    }
    const draft = draftOf(node, "result", context);
    madeBy(draft, node);
    return draft;
  };

  return [draftCriterion, draftBorrowed, draftBorrowedResult];
}

/** The actor of a result node whose read names no author: `checkStanding` reads no actor. */
const NO_ACTOR: Actor = { id: "", kind: "" };

/** A result recorded against the check's words as they stand, which the page reads at its basis:
 *  no basis commit of another issue's check is read, so the newest such result decides. */
const AT_BASIS: NonNullable<CheckResult["commit_stands"]> = { answer: "at_basis" };

/** The result nodes of each answer by the read id of the node the read places each under. */
const resultsByParent = new WeakMap<TreeAnswer, ReadonlyMap<string, readonly TreeAnswerNode[]>>();

/** The live result nodes `answer` places under the node `parentId` names, in the read's order. */
function resultsUnder(answer: TreeAnswer, parentId: string): readonly TreeAnswerNode[] {
  let byParent = resultsByParent.get(answer);
  if (byParent === undefined) {
    const grouped = new Map<string, TreeAnswerNode[]>();
    for (const node of answer.nodes ?? []) {
      if (node.supertag !== "result" || node.state === "gone" || node.parent === null) continue;
      const group = grouped.get(node.parent.id);
      if (group === undefined) grouped.set(node.parent.id, [node]);
      else group.push(node);
    }
    byParent = grouped;
    resultsByParent.set(answer, byParent);
  }
  return byParent.get(parentId) ?? [];
}

/**
 * How a borrowed check stands: F's one reading of a check (`checkStanding`) over the result
 * nodes the read answers under it, each read as a `CheckResult` (`resultOf`), with the server's
 * `pass_stands` answered `yes` where a pass nobody withdrew was recorded against the check's
 * words as they stand, else `no`. So the newest result nobody withdrew over the check's current
 * words decides, a pass passed and a fail failed; where none is, the newest over moved words
 * reads out of date, every result withdrawn retracted, and no result no-result. A pass at a
 * commit the child's check no longer compares with still reads passed, since no basis is read
 * for another issue.
 *
 * Undefined where the read answers no words' sum on the check (`check`) or no `result` on one
 * of its result nodes (INFRA-1253 R8), so the row draws no standing word: a pass that may be
 * against old words must not read as current (decision 161).
 */
function borrowedStanding(check: TreeAnswerNode, answer: TreeAnswer): CheckState | undefined {
  const words = check.check?.text_sha256;
  if (words === undefined) return undefined;
  const results: CheckResult[] = [];
  for (const node of resultsUnder(answer, check.id)) {
    const result = resultOf(node, check, words);
    if (result === undefined) return undefined;
    results.push(result);
  }
  const passStands = results.some(
    (result) =>
      result.verdict === "pass" &&
      result.retracted === undefined &&
      result.check_words_moved !== true
  );
  const reads: CheckStateReads = { pass_stands: { answer: passStands ? "yes" : "no" }, results };
  return checkStanding(reads).state;
}

/** A count the read answers as a result node's value, as the text the store states it in. */
function countOf(value: string | undefined): number {
  const count = Number(value ?? "");
  return Number.isInteger(count) ? count : 0;
}

/**
 * The result `node`, recorded against the borrowed check `check` whose words now sum to `words`,
 * as the results read answers a result (`CheckResult`): its verdict, its check's version and
 * words' sum when recorded and its withdrawal from the read's `result`; what it ran and found
 * from its values (`attributes`, R12's); its id, author and time from the node; the child's key
 * as its issue and its check's; its check's document and block. Its words moved
 * (`check_words_moved`) where their sum is not the check's now, and else it is at the check's
 * basis (`AT_BASIS`). R12 keeps a result's session and screenshots as tuples, which
 * `checkStanding` reads neither of, so it carries none. Undefined where the read answers no
 * `result` on the node.
 */
function resultOf(
  node: TreeAnswerNode,
  check: TreeAnswerNode,
  words: string
): CheckResult | undefined {
  const { result } = node;
  if (result === undefined) return undefined;
  const values = node.attributes ?? {};
  const moved = result.check_sha256 !== words;
  return {
    actor: node.author?.[0] ?? NO_ACTOR,
    check_artifact_id: check.document?.artifact_id ?? "",
    check_block_id: blockIdOfNode(check),
    check_issue_key: check.issue ?? "",
    check_sha256: result.check_sha256,
    check_version: result.check_version,
    ...(moved ? { check_words_moved: true } : { commit_stands: AT_BASIS }),
    commit_sha: values.commit ?? "",
    created_at: node.made_at ?? "",
    excluded: countOf(values.excluded),
    failed: countOf(values.failed),
    id: node.id,
    issue_key: node.issue ?? check.issue ?? "",
    negative_control: values.negative_control ?? "",
    observed: values.observed ?? "",
    ...(result.retracted === undefined ? {} : { retracted: result.retracted }),
    role: values.role ?? "",
    run: values.run ?? "",
    screenshots: null,
    session: "",
    skipped: countOf(values.skipped),
    surface: values.surface ?? "",
    verdict: result.verdict,
  };
}
