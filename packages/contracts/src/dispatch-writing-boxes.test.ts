import { expect, test } from "bun:test";
import { DISPATCH_WRITING_HINT_QUIET } from "./dispatch-writing-boxes";

// R4: a box whose write tells nobody it names says how to open the picker, that a pick tells
// nobody, and then what does tell. What does is the write's own doing - a reply goes to its
// thread, an ask to the assignee's Inbox, and for an edit only a new comment tells anyone - so
// each such write has words of its own. Words copied from another write say what tells for the
// wrong write; words that leave out that nobody is told promise a delivery the write never makes;
// and words without the doors leave the reader no way to open the picker. Each box's line is
// these words, looked up by its row's fields, so they are what this holds.
test("each write that tells nobody it names has words of its own, which say nobody is told", () => {
  const quiet = Object.entries(DISPATCH_WRITING_HINT_QUIET);
  const writesOf = new Map<string, string[]>();
  for (const [write, words] of quiet) writesOf.set(words, [...(writesOf.get(words) ?? []), write]);

  expect(
    [...writesOf.values()].filter((writes) => writes.length > 1),
    "words several writes share"
  ).toEqual([]);
  expect(
    quiet
      .filter(([, words]) => !words.includes("Nobody you pick is told."))
      .map(([write]) => write),
    "a write that tells nobody not saying so"
  ).toEqual([]);
  const doors = "Type @ for an agent, a role or a person, # for an issue, [[ for anything else.";
  expect(
    quiet.filter(([, words]) => !words.startsWith(`${doors} `)).map(([write]) => write),
    "a write whose words do not say how to open the picker"
  ).toEqual([]);
});
