import { expect, test } from "bun:test";

import { isContentsEntry, producingMessages } from "./contents-entries";
import type { TreeNode } from "./issue-tree";
import { node } from "./verification-filter/verification-fixtures";

const READER = { id: "alice", kind: "user" as const };
const OTHER = { id: "bob", kind: "user" as const };

// An ask or a comment the reader wrote that begins a thread is an entry. A reply, another
// person's comment thread, and a transcluded copy are not.
test("an entry of the Contents list is an ask or a comment thread the reader began", () => {
  const ask = node({ id: "ask", type: "ask" });
  const begun = node({ author: READER, id: "begun", type: "comment" });
  const reply = node({ author: READER, id: "reply", type: "comment" });
  const thread = node({ author: OTHER, children: [reply], id: "thread", type: "comment" });
  const copy = node({ author: READER, id: "copy", transcluded: true, type: "comment" });
  const askCopy = node({ id: "ask-copy", transcluded: true, type: "ask" });
  const root = node({
    children: [ask, begun, thread, copy, askCopy],
    id: "ISSUE-1",
    type: "issue",
  });
  const producing = producingMessages(root);
  const entry = (parent: TreeNode, child: TreeNode, viewer: string | undefined) =>
    isContentsEntry(parent, child, viewer, producing);

  expect(entry(root, ask, "alice"), "an ask is not an entry").toBe(true);
  expect(entry(root, begun, "ALICE"), "the reader's thread is not an entry").toBe(true);
  expect(entry(root, thread, "alice"), "another reader's thread is an entry").toBe(false);
  expect(entry(thread, reply, "alice"), "a reply inside a thread is an entry").toBe(false);
  expect(entry(root, copy, "alice"), "a transcluded copy is an entry").toBe(false);
  expect(entry(root, askCopy, "alice"), "a transcluded ask is an entry").toBe(false);
  expect(entry(root, begun, undefined), "a thread with no reader is an entry").toBe(false);
});

// A thread of messages alone produced no node, so it has no entry; one under which the tree
// places a node that is no message - at any depth of the thread - is an entry. A transcluded copy
// is drawn at a second place, where it was not produced, so it makes no thread an entry.
test("a message thread the reader began is an entry only where a node that is no message sits under it", () => {
  const message = (id: string, children: readonly TreeNode[] = [], author = READER) =>
    node({ author, children, id, type: "message" });

  const alone = message("alone", [message("alone-reply", [message("alone-deeper")])]);
  const firstStatement = node({ id: "first-statement", statement: true, type: "requirement" });
  const underFirst = message("under-first", [firstStatement, message("first-reply")]);
  const replyStatement = node({ id: "reply-statement", statement: true, type: "requirement" });
  const reply = message("reply-one", [message("reply-two", [replyStatement])], OTHER);
  const underReply = message("under-reply", [reply]);
  const copy = { ...firstStatement, transcluded: true };
  const copied = message("copied", [copy]);
  const copiedReply = message("copied-reply-thread", [{ ...reply, transcluded: true }]);
  const other = message("other", [node({ id: "other-statement", type: "requirement" })], OTHER);
  const root = node({
    children: [alone, underFirst, underReply, copied, copiedReply, other],
    id: "ISSUE-1",
    type: "issue",
  });
  const producing = producingMessages(root);
  const entry = (parent: TreeNode, child: TreeNode, viewer: string | undefined) =>
    isContentsEntry(parent, child, viewer, producing);

  expect(entry(root, alone, "alice"), "a thread of messages alone is an entry").toBe(false);
  expect(
    entry(root, underFirst, "alice"),
    "a thread with a statement under its first message is not an entry"
  ).toBe(true);
  expect(
    entry(root, underReply, "alice"),
    "a thread with a statement under a reply in it is not an entry"
  ).toBe(true);
  expect(entry(root, copied, "alice"), "a copied statement made the thread an entry").toBe(false);
  expect(
    entry(root, copiedReply, "alice"),
    "a copied reply's branch made the thread an entry"
  ).toBe(false);
  expect(entry(root, other, "alice"), "another reader's thread is an entry").toBe(false);
  expect(entry(underReply, reply, "bob"), "a reply inside a thread is an entry").toBe(false);
  expect(
    entry(root, underFirst, undefined),
    "a thread is an entry before the reader's login has answered"
  ).toBe(false);
});
