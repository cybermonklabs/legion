import { expect, test } from "bun:test";

import type { Artifact } from "../../api/types";
import { artifact, KEY } from "./decisions-filter/decisions-fixtures";
import type { DocumentAdmissions } from "./document-admission";
import { documentOfNode } from "./issue-artifacts";
import { nodeEditable } from "./useNodeEditing";

// Which rows of the outline take typing, and so draw the cue that they do: Enter's own rule, a
// block of one of the issue's documents (`documentOfNode`), on an issue that is open, unless that
// document's room admitted the page read-only.

const ARTIFACTS: ReadonlyMap<string, Artifact> = new Map(
  ["spec", "verification", "decisions"].map((slug) => [
    slug,
    artifact(`artifact-${slug}`, slug, slug === "spec"),
  ])
);

const OPEN = false;
const CLOSED = true;

/** Whether the row of `nodeId` takes typing (`nodeEditable`), where `admissions` is what each
 *  document's room admitted the page to: by default no room has answered. */
function editable(
  nodeId: string,
  isClosed: boolean,
  admissions: DocumentAdmissions = new Map()
): boolean {
  return nodeEditable(documentOfNode(nodeId, ARTIFACTS), isClosed, admissions);
}

test("a block of one of the issue's documents takes typing: a statement, a check, a decision", () => {
  expect(editable("spec#b-r1", OPEN), "a statement").toBe(true);
  expect(editable("verification#b-c1", OPEN), "a check").toBe(true);
  expect(editable("decisions#b-d1", OPEN), "a decision").toBe(true);
});

test("a comment, a message, an ask, a result, a child issue and the issue take no typing", () => {
  expect(editable("comment-1", OPEN), "a comment").toBe(false);
  expect(editable("message-1", OPEN), "a message").toBe(false);
  expect(editable("ask-1", OPEN), "an ask").toBe(false);
  expect(editable("result-1", OPEN), "a result").toBe(false);
  expect(editable("CORE-2", OPEN), "a child issue").toBe(false);
  expect(editable(KEY, OPEN), "the issue").toBe(false);
});

test("a block of a document this issue does not hold, such as an issue above's, takes no typing", () => {
  expect(editable("parent-spec#b-r1", OPEN)).toBe(false);
});

test("on a closed issue no block takes typing: its documents are read-only", () => {
  expect(editable("spec#b-r1", CLOSED), "a statement").toBe(false);
  expect(editable("verification#b-c1", CLOSED), "a check").toBe(false);
  expect(editable("decisions#b-d1", CLOSED), "a decision").toBe(false);
});

test("a block of a document its room admitted read-only takes no typing, and another document's still does", () => {
  const admissions: DocumentAdmissions = new Map([
    ["artifact-spec", true],
    ["artifact-verification", false],
  ]);
  expect(
    editable("spec#b-r1", OPEN, admissions),
    "a statement of a document its room admitted read-only"
  ).toBe(false);
  expect(
    editable("verification#b-c1", OPEN, admissions),
    "a check of a document its room admitted to write"
  ).toBe(true);
  expect(
    editable("decisions#b-d1", OPEN, admissions),
    "a decision of a document no room has answered for"
  ).toBe(true);
});
