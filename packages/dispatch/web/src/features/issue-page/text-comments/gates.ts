/**
 * What the base this branch stands on holds of the parts and the server work that comments on text
 * wait for (INFRA-5706's plan, "The gates"). A line is `false` until the commit that merges its
 * gate is on the branch's base; that commit flips the line and adds the rows it owes. A case or a
 * reading owed on a gate is written `test.skipIf(!GATES.NAME)`, and no code probes a capability
 * at run time. The browser suite's own table (`e2e/issue-page-margin-gates.ts`) moves with this
 * one.
 */

/** One name for each gate that a part or a server capability needs, in the order of the table. */
export type GateName =
  | "ANCHOR_OCCURRENCE"
  | "COMPOSER"
  | "DASHBOARD"
  | "NODE_ANCHOR"
  | "OTHER_ISSUE_COMMENTS"
  | "PART_J"
  | "PART_M"
  | "PART_N"
  | "PART_U"
  | "PART_X"
  | "PART_Y"
  | "REPLY_QUOTE"
  | "THREADS"
  | "TREE_ARTIFACT_ANCHOR";

/** The type keeps each value a `boolean`, so code behind a gate type-checks as reachable. */
export const GATES: Readonly<Record<GateName, boolean>> = {
  // G3b: `occurrence` on the tree read's document `anchor` (INFRA-1253).
  ANCHOR_OCCURRENCE: false,
  // G5: pull request 3125 on the base: the control, its composer and every send, and the stacking.
  COMPOSER: false,
  // G8: INFRA-5702, the Dashboard's items.
  DASHBOARD: false,
  // G2 and G5: INFRA-1253's node anchor, and the shell margin's comments read that asks for it.
  NODE_ANCHOR: false,
  // G3c: the comments on a node of another issue in each answer that draws that node.
  OTHER_ISSUE_COMMENTS: false,
  // G6: part J (INFRA-3939), the Spec view's rows.
  PART_J: false,
  // G6: part M (INFRA-3948), `address-target.ts` at its `:200`.
  PART_M: false,
  // G7: part N (INFRA-3940), the opened document.
  PART_N: false,
  // G6: part U (INFRA-4097), `DecisionsRow.tsx`.
  PART_U: false,
  // G6: part X (INFRA-4063), `NodeRow.tsx` and the Plan view's rows.
  PART_X: false,
  // G6: part Y (INFRA-4066), the Verification view's rows and the block editor's caret.
  PART_Y: false,
  // G4: INFRA-5703's reply quote (`quote` and `occurrence` beside `reply_to_block`).
  REPLY_QUOTE: false,
  // G8: INFRA-5704, the opened thread in Threads.
  THREADS: false,
  // G3a: INFRA-1253's anchor on the issue's `tree` artifact.
  TREE_ARTIFACT_ANCHOR: false,
};
