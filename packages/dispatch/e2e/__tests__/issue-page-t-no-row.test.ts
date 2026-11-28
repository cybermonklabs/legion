import { expect, test } from "bun:test";

import type { IssueTreeNode, IssueTreeRead } from "../../web/src/api/types";
import type { TreeIdReads } from "../../web/src/features/issue-page/tree-read/tree-nodes";
import { nodesNoRowDraws } from "../issue-page-t-rows";

const KEY = "LAY-1";

/** The spec's place, as the issue read and a block's node name it. */
const SPEC = { artifact_id: "spec-artifact", slug: "spec" };

/** The statement the agent's decision shaped, by its read id. */
const STATEMENT = `${KEY}/spec#r1`;

/** A `decision` block of the spec, under the answered ask `ask-1`, whose `from_ask` names that
 *  ask and whose `shaped` ties (`decision.links`) name `shaped`. */
function decisionUnderAsk(blockId: string, shaped: readonly string[]): IssueTreeNode {
  return {
    block_id: blockId,
    decision: { asks: ["ask-1"], links: shaped.map((id) => ({ to: { id } })) },
    document: { ...SPEC, position: 1 },
    id: `${KEY}/spec#${blockId}`,
    kind: "block",
    parent: { id: "ask-1", kind: "ask", supertag: "ask" },
    supertag: "decision",
  } as unknown as IssueTreeNode;
}

// Part T's rows 5 and 6 read an outline's rows as the answer's nodes but those no outline draws
// (`nodesNoRowDraws`). Under INFRA-1196's decisions 513 and 521 part U draws no row for a
// decision that restates a person's answer - a `decision` block whose `from_ask` names an ask a
// person answered and that has no `shaped` tie of its own (`restatingSections`) - so it is one of
// those nodes. A decision under the same answered ask that shaped a statement is an agent's
// decision, which the outline draws as a row.
test("nodesNoRowDraws leaves out a decision that restates a person's answer and keeps one under the answered ask that shaped a statement", () => {
  const answer = {
    nodes: [
      { id: KEY, kind: "issue", parent: null, supertag: "issue" },
      {
        id: "ask-1",
        kind: "ask",
        parent: { id: KEY, kind: "issue", supertag: "issue" },
        supertag: "ask",
      },
      {
        block_id: "r1",
        document: { ...SPEC, position: 0 },
        id: STATEMENT,
        kind: "block",
        parent: { id: KEY, kind: "issue", supertag: "issue" },
        supertag: "requirement",
      },
      decisionUnderAsk("d-restates", []),
      decisionUnderAsk("d-shaped", [STATEMENT]),
    ],
    tuples: [
      {
        from: { id: `${KEY}/spec#d-shaped`, kind: "decision" },
        kind: "shaped",
        to: { id: STATEMENT, kind: "requirement" },
      },
    ],
  } as unknown as IssueTreeRead;
  const reads = {
    asks: [{ id: "ask-1", state: "answered" }],
    issue: { artifacts: [{ id: SPEC.artifact_id, slug: SPEC.slug }], key: KEY },
  } as unknown as TreeIdReads;

  expect([...nodesNoRowDraws({ answer, reads }, new Set(), undefined)]).toEqual([
    `${KEY}/spec#d-restates`,
  ]);
});
