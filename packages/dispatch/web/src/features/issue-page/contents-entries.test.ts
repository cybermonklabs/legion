import { expect, test } from "bun:test";

import { contentsEntries, isContentsEntry } from "./contents-entries";
import { node } from "./verification-filter/verification-fixtures";

const READER = { id: "alice", kind: "user" as const };
const OTHER = { id: "bob", kind: "user" as const };

// The Contents list's one rule, which its count in the view row and the list itself read: an
// ask; a comment or a message the reader wrote that begins a thread; never a reply inside a
// thread, another reader's thread, or a transcluded copy.
test("an entry of the Contents list is an ask or a thread the reader began, and the count reads the same rule", () => {
  const ask = node({ id: "ask", type: "ask" });
  const begun = node({ author: READER, id: "begun", type: "comment" });
  const reply = node({ author: READER, id: "reply", type: "comment" });
  const thread = node({ author: OTHER, children: [reply], id: "thread", type: "comment" });
  const message = node({ author: READER, id: "message", type: "message" });
  const copy = node({ author: READER, id: "copy", transcluded: true, type: "comment" });
  const root = node({
    children: [ask, begun, thread, message, copy],
    id: "ISSUE-1",
    type: "issue",
  });

  expect(isContentsEntry(root, ask, "alice"), "an ask is no entry").toBe(true);
  expect(isContentsEntry(root, begun, "ALICE"), "the reader's thread is no entry").toBe(true);
  expect(isContentsEntry(root, message, "alice"), "the reader's message is no entry").toBe(true);
  expect(isContentsEntry(root, thread, "alice"), "another reader's thread is an entry").toBe(false);
  expect(isContentsEntry(thread, reply, "alice"), "a reply inside a thread is an entry").toBe(
    false
  );
  expect(isContentsEntry(root, copy, "alice"), "a transcluded copy is an entry").toBe(false);
  expect(isContentsEntry(root, begun, undefined), "a thread with no reader is an entry").toBe(
    false
  );
  expect(contentsEntries(root, "alice"), "the count does not read the entries' rule").toBe(3);
});
