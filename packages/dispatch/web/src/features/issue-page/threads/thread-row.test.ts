import { expect, test } from "bun:test";

import type { Actor } from "../../../api/types";
import { fixtureRecord as record } from "./thread-fixtures";
import { threadRow } from "./thread-row";

// The pages the server answered at the stack base (fixtures/README.md), on the project whose
// Legion daemon replied in the reader's remark.
const PROJECT = "PDFD720CB";

/** The page's name of an author stands here as the session's title, else the actor's id. */
const names = {
  author: (actor: Actor) => actor.origin?.session_title ?? actor.id,
  project: PROJECT,
};

test("a row names its record's kind in the page's words", () => {
  expect(threadRow(record("59ab65ed"), names).kind).toBe("Question");
  expect(threadRow(record("b5979723"), names).kind).toBe("Comment");
  expect(threadRow(record("c1d04870"), names).kind).toBe("Comment on text");
  expect(threadRow(record("b6099daa"), names).kind).toBe("Comment on text");
  expect(threadRow(record("99945b76"), names).kind).toBe("Message");
  expect(threadRow(record("b788d337"), names).kind).toBe("Message");
});

test("a message whose root targets an agent names that agent: a role by its token, a session as the row names its author", () => {
  expect(threadRow(record("2dc5b959"), names).kind).toBe("Message to planner");
  // The thread between agents targets the planner's session, which replied in it.
  expect(threadRow(record("a92a5f53"), names).kind).toBe("Message to Planner (threads)");
});

test("a row gives the agent a message's root targets apart from its kind's words, and none for any other record", () => {
  expect(threadRow(record("2dc5b959"), names).agent).toBe("planner");
  expect(threadRow(record("a92a5f53"), names).agent).toBe("Planner (threads)");
  expect(threadRow(record("99945b76"), names).agent).toBeUndefined();
  expect(threadRow(record("59ab65ed"), names).agent).toBeUndefined();
  expect(threadRow(record("b5979723"), names).agent).toBeUndefined();
});

test("a row names each author of its record once, in the order of their first write, and the Legion daemon by its words and its project", () => {
  expect(threadRow(record("73914aae"), names).people).toEqual([
    "alice",
    "Planner (threads)",
    "Legion daemon · PDFD720CB",
  ]);
  expect(threadRow(record("2dc5b959"), names).people).toEqual(["alice", "Planner (threads)"]);
  // The planner asked, the reader answered, then each replied.
  expect(threadRow(record("59ab65ed"), names).people).toEqual(["Planner (threads)", "alice"]);
});

test("a row of a record that a quoting reply begins names only that record's authors", () => {
  expect(threadRow(record("b6099daa"), names).people).toEqual([
    "Planner (threads)",
    "Reviewer (threads)",
  ]);
  expect(threadRow(record("18ddeb81"), names).people).toEqual(["alice", "bob"]);
});

test("a row's last activity is its own record's", () => {
  const post = record("18ddeb81");
  const quoting = record("b6099daa");
  expect(threadRow(post, names).activity).toBe(post.replies[0]?.created_at ?? "");
  expect(threadRow(quoting, names).activity).toBe(quoting.replies[0]?.created_at ?? "");
});
