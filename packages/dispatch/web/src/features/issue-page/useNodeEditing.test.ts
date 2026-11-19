import { expect, test } from "bun:test";

import type { Artifact } from "../../api/types";
import { artifact, KEY } from "./decisions-filter/decisions-fixtures";
import { documentOfNode } from "./issue-artifacts";
import { nodeEditable } from "./useNodeEditing";

// Which rows of the outline take typing, and so draw the cue that they do: Enter's own rule, a
// block of one of the issue's documents (`documentOfNode`), on an issue that is open.

const ARTIFACTS: ReadonlyMap<string, Artifact> = new Map(
  ["spec", "verification", "decisions"].map((slug) => [
    slug,
    artifact(`artifact-${slug}`, slug, slug === "spec"),
  ])
);

const OPEN = false;
const CLOSED = true;

test("a block of one of the issue's documents takes typing: a statement, a check, a decision", () => {
  expect(nodeEditable(documentOfNode("spec#b-r1", ARTIFACTS), OPEN), "a statement").toBe(true);
  expect(nodeEditable(documentOfNode("verification#b-c1", ARTIFACTS), OPEN), "a check").toBe(true);
  expect(nodeEditable(documentOfNode("decisions#b-d1", ARTIFACTS), OPEN), "a decision").toBe(true);
});

test("a comment, a message, an ask, a result, a child issue and the issue take no typing", () => {
  expect(nodeEditable(documentOfNode("comment-1", ARTIFACTS), OPEN), "a comment").toBe(false);
  expect(nodeEditable(documentOfNode("message-1", ARTIFACTS), OPEN), "a message").toBe(false);
  expect(nodeEditable(documentOfNode("ask-1", ARTIFACTS), OPEN), "an ask").toBe(false);
  expect(nodeEditable(documentOfNode("result-1", ARTIFACTS), OPEN), "a result").toBe(false);
  expect(nodeEditable(documentOfNode("CORE-2", ARTIFACTS), OPEN), "a child issue").toBe(false);
  expect(nodeEditable(documentOfNode(KEY, ARTIFACTS), OPEN), "the issue").toBe(false);
});

test("a block of a document this issue does not hold, such as an issue above's, takes no typing", () => {
  expect(nodeEditable(documentOfNode("parent-spec#b-r1", ARTIFACTS), OPEN)).toBe(false);
});

test("on a closed issue no block takes typing: its documents are read-only", () => {
  expect(nodeEditable(documentOfNode("spec#b-r1", ARTIFACTS), CLOSED), "a statement").toBe(false);
  expect(nodeEditable(documentOfNode("verification#b-c1", ARTIFACTS), CLOSED), "a check").toBe(
    false
  );
  expect(nodeEditable(documentOfNode("decisions#b-d1", ARTIFACTS), CLOSED), "a decision").toBe(
    false
  );
});
