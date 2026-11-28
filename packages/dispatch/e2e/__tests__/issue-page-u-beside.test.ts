import { expect, test } from "bun:test";

import type { IssueTreeNode } from "../../web/src/api/types";
import { decisionRowId, type SpecColumnRow, statementsBeside } from "../issue-page-u";

// Row 1 of `e2e/issue-page-u.e2e.ts`, clause (3) of c-decisions: under Spec, each decision node
// stands beside each statement its `shaped` tie names, by the cell whose bracket spans the
// statement or by a link of the statement's line (`statementsBeside`). A statement that two
// decisions shaped draws the first as its cell and names the other at the end of its own line,
// as a link to that decision's block (`spec-decided-by.ts`, "Each `shaped` tie"), whose address
// is the row id the page gives the block, `<slug>#b-<block id>`. The row names a decision node by
// that row id and by the tree read's own id for it (`decisionNames`), so the second decision of
// a statement must be found on the statement's line by those names.
test("a decision a statement's line names as one that informed it stands beside that statement, by the names row 1 gives a decision node", () => {
  const informing = {
    block_id: "d5",
    document: { artifact_id: "spec-artifact", slug: "spec" },
    id: "LAY-2/tree#d5",
    supertag: "decision",
  } as unknown as IssueTreeNode;
  const column: SpecColumnRow[] = [
    {
      cell: ["spec#b-d2", "spec#b-d2"],
      id: "spec#b-r4",
      links: ["/issues/LAY-2/artifacts/spec#b-d2", "/issues/LAY-2/artifacts/spec#b-d5"],
    },
    { cell: [], id: "spec#b-r6", links: [] },
  ];

  expect(statementsBeside(column, [decisionRowId(informing), informing.id])).toEqual(["spec#b-r4"]);
});
