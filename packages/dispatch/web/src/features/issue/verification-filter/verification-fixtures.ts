import type { CheckBlock, CheckResult } from "../../../api/types";
import { checkFacts, resultFacts } from "../check-facts";
import type { TreeNode } from "../issue-tree";
import type { NodeType } from "../page-words";
import { type PresetView, presetView } from "../presets";
import { blockNodeId, checkState } from "../tree-drafts";

/**
 * The reads the Verification filter's body is drawn from, as the page holds them: checks of the
 * issue's Verification document with the results recorded against them, and the tree the filter
 * leaves over them (`tree-drafts.ts`, `presets.ts`). The model rows and the drawn rows read the
 * same fixtures, so what a row asserts about a check is asserted over the node the page would
 * really draw.
 */

export const ISSUE_KEY = "CORE-1";

/** The commit the page names in these cases: the one the newest standing result ran at. */
export const COMMIT = "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678";

/** Another commit, which a result recorded against it is out of date at. */
export const OTHER_COMMIT = "99887766554433221100ffeeddccbbaa99887766";

export const VERIFICATION_SLUG = "verification-md";
export const SPEC_SLUG = "spec";

/** One node of the tree, with whatever the case gives it. */
export function node(partial: Partial<TreeNode> & { id: string; type: NodeType }): TreeNode {
  return {
    alsoUnder: [],
    children: [],
    chips: [],
    facts: [],
    gaps: [],
    held: 0,
    question: false,
    replaced: false,
    text: partial.id,
    transcluded: false,
    waiting: false,
    ...partial,
  };
}

/** One check of the Verification document as the results read answers it. */
export function check(partial: Partial<CheckBlock> & { block_id: string }): CheckBlock {
  const blockId = partial.block_id;
  return {
    artifact_id: "artifact-verification",
    compared_with: [{ commit: COMMIT, kind: "named" }],
    issue_key: ISSUE_KEY,
    negative_control: "Break the row and the run fails.",
    pass_stands: { answer: "yes" },
    ref: `dispatch://${ISSUE_KEY}/artifact/${VERIFICATION_SLUG}#b-${blockId}`,
    results: [],
    run: "bun test web/src",
    sha256: "sum",
    surface: "browser",
    text: `The page draws ${blockId}.`,
    version: 1,
    when: "before_merge",
    ...partial,
  };
}

/** One result recorded against a check: a pass at the commit the page names, with whatever the
 *  case changes about it. */
export function result(partial: Partial<CheckResult> & { id: string }): CheckResult {
  return {
    actor: { id: "tester", kind: "session", origin: { session_title: "the tester" } },
    check_artifact_id: "artifact-verification",
    check_block_id: "c-one",
    check_issue_key: ISSUE_KEY,
    check_sha256: "sum",
    check_version: 1,
    check_words_moved: false,
    commit_sha: COMMIT,
    commit_stands: { answer: "at_basis" },
    created_at: "2026-11-14T00:58:00Z",
    excluded: 0,
    failed: 0,
    issue_key: ISSUE_KEY,
    negative_control: "Break the row and the run fails.",
    observed: "41 passed",
    role: "tester",
    run: "bun test web/src",
    screenshots: [],
    session: "session-1",
    skipped: 0,
    surface: "browser",
    verdict: "pass",
    ...partial,
  };
}

/** A check's node, as `tree-drafts.ts` builds one: the block's address as its reference, the
 *  state `checkState` reads, the four attributes its body draws, and one node per result. */
export function checkNode(block: CheckBlock): TreeNode {
  return node({
    children: (block.results ?? []).map((recorded) =>
      node({
        at: recorded.created_at,
        author: recorded.actor,
        facts: resultFacts(recorded),
        id: recorded.id,
        text: recorded.observed,
        type: "result",
      })
    ),
    facts: checkFacts(block),
    id: blockNodeId(VERIFICATION_SLUG, block.block_id),
    reference: block.ref,
    state: checkState(block),
    text: block.text,
    type: "check",
  });
}

/** A statement's node with the checks that serve it under it, as the tree places them. */
export function statementNode(
  blockId: string,
  text: string,
  checks: readonly TreeNode[] = []
): TreeNode {
  return node({
    children: checks,
    id: blockNodeId(SPEC_SLUG, blockId),
    reference: `dispatch://${ISSUE_KEY}/${SPEC_SLUG}#b-${blockId}`,
    text,
    type: "requirement",
  });
}

/** The Verification filter's own view (`presetView`) over the statements of the issue and
 *  whatever else sits under it: the tree it leaves, and the ids it draws as context, which is
 *  every statement. `named` is what the address names, which outranks the filter: a node of any
 *  kind it names is drawn (`presets.ts`). */
export function verificationPreset(
  under: readonly TreeNode[],
  named: readonly string[] = []
): PresetView {
  const issue = node({ children: under, id: ISSUE_KEY, text: "An issue", type: "issue" });
  return presetView(issue, "verification", "", new Set(named));
}

/** The tree as the Verification filter leaves it. */
export function verificationView(
  under: readonly TreeNode[],
  named: readonly string[] = []
): TreeNode {
  return verificationPreset(under, named).root;
}
