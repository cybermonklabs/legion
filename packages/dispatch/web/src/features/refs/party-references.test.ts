import { expect, test } from "bun:test";

import { draftParties, partyMentionSpan } from "./party-references";

const SESSION = "01a1058e-f14f-7684-87eb-3dc885955551";
const AGENT = `dispatch://agent/${SESSION}`;

// Who a draft addresses is in its own words, and a code context is where words are shown rather
// than written to somebody: the server drops a party it holds before any delivery rule reads the
// set (`api.commentPartiesOf`), and the composer must say the same of the draft in front of the
// reader. The three contexts below are the three the shared reader knows (`code-ranges.ts`).

test("a party written as a worded link's address is delivered to", () => {
  expect(draftParties(`Over to [Planner](${AGENT}) now.`)).toEqual({
    named: true,
    targets: [`session:${SESSION}`],
  });
});

test("a party in a fenced code block is no target and names nobody", () => {
  expect(draftParties(`Run this:\n\n\`\`\`sh\ndispatch send ${AGENT}\n\`\`\`\n`)).toEqual({
    named: false,
    targets: [],
  });
});

test("a party in an indented code block is no target and names nobody", () => {
  expect(draftParties(`Pasted from the handbook:\n\n    ${AGENT}\n`)).toEqual({
    named: false,
    targets: [],
  });
});

test("a party in a code span is no target and names nobody", () => {
  expect(draftParties("Run `dispatch message --to dispatch://role/reviewer` first.")).toEqual({
    named: false,
    targets: [],
  });
});

// A person is named but never delivered to (R5), and naming one is what makes a typed `/btw` a
// command: in code it does neither, so the draft's first word stays the reader's own.
test("a person in a code span leaves a draft naming nobody", () => {
  expect(draftParties("Mail `dispatch://person/alice@example.com` about it.")).toEqual({
    named: false,
    targets: [],
  });
});

test("a person named outside code names the draft without delivering to one", () => {
  expect(draftParties("Mail dispatch://person/alice@example.com about it.")).toEqual({
    named: true,
    targets: [],
  });
});

// Each occurrence stands on its own, as it does for the server: the one outside code names the
// party, whatever a fence elsewhere in the draft shows.
test("a party a draft also writes outside code is delivered to", () => {
  expect(
    draftParties("Run `dispatch://role/reviewer` and ask [the reviewer](dispatch://role/reviewer).")
  ).toEqual({ named: true, targets: ["role:reviewer"] });
});

test("a draft naming parties lists each once, in the order it names them", () => {
  expect(
    draftParties(`[Reviewer](dispatch://role/reviewer), [Planner](${AGENT}) and ${AGENT} again.`)
  ).toEqual({ named: true, targets: ["role:reviewer", `session:${SESSION}`] });
});

// The mention a channel takes out of its draft when the reader leaves it (`seededDraft`): the
// link's words with its address, and nothing at all where the only occurrence is in code - that
// text is the reader's own, not an address this channel owes.
test("a party's mention is found with the words that name it", () => {
  const body = `Over to [Planner](${AGENT}) now.`;

  expect(partyMentionSpan(body, `session:${SESSION}`)).toEqual({
    end: body.indexOf(") now.") + 1,
    start: body.indexOf("[Planner]"),
  });
});

test("a party written only in code is mentioned nowhere", () => {
  expect(partyMentionSpan(`Run \`dispatch send ${AGENT}\` first.`, `session:${SESSION}`)).toBe(
    undefined
  );
});
