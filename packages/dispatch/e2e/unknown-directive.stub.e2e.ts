import { expect } from "@playwright/test";

import { createAsk, createComment, createIssue, createMessage, createProject } from "./api";
import { stubbed } from "./stub/stub-test";

// The one family of this suite the markdown fixture table cannot hold, so this file keeps bodies
// of its own instead of reading `pmdoc/testdata/fixtures.json`. Every body below carries a
// `:::note`, `:::a` or `::::ask` container the block schema does not know, or a typed block whose
// content breaks its rule. The table's generator parses each corpus file with the writing parser,
// which refuses such a document outright (`unknown typed block "note"`), because the document
// service refuses it too: a document is stored only where both readers read it alike, and these
// shapes are exactly the ones they do not. A Conversation body is never stored as a document -
// it is drawn by the read-only engine, which draws an unknown container in place - so no corpus
// file can hold these, and no reading of the table stands in for what this row reads: the
// `strong` nodes the body still formats, the fence lines kept as text, and the count of
// `[data-markdown-fallback]` stand-ins, which must be none.
//
// One row covers every shape the file held, on one issue and one Conversation: an unknown
// container directive in a comment, a message and an ask; a typed block whose content breaks its
// rule, in a message and in an ask; a lone carriage return ending the fence lines; and a leading
// byte-order mark, which the parser drops before it counts offsets. Each card is found by its own
// turn id, so the bodies do not have to be told apart by their content.
//
// The row reads what the Conversation draws from bodies the server already stored, so it runs
// against a recording rather than against a harness (packages/dispatch/AGENTS.md, "The stubbed
// layer").

const agent = {
  actor: { id: "e2e-directive-agent", kind: "session" as const },
  as: "agent" as const,
};

/** A body whose `:::note` container directive no Dispatch block type claims. */
const unknownContainer = "Some **bold** text\n\n:::note\nA note with **inner** words\n:::\n";

/** Bodies whose typed block is one the schema knows and whose content breaks its rule: an ask
 *  takes `paragraph+ bullet_list?`, so the drawn `:::note`'s lines after the list break it, and so
 *  does a heading. The browser editor's parser drops such a block with its text, so every line
 *  must still be drawn. */
const brokenTypedBlocks = {
  ask: {
    body: "Lead **bold**\n\n:::ask{#a1}\n# A heading\n\nBody text\n:::\n",
    lines: [":::ask{#a1}", "A heading", "Body text"],
  },
  message: {
    body: "Lead **bold**\n\n::::ask\nWhich option?\n\n- first\n\n:::note\nA note line\n:::\n::::\n",
    lines: ["::::ask", "Which option?", "first", ":::note", "A note line"],
  },
};

const test = stubbed(async () => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Unknown directive" });
  const container = {
    ask: (await createAsk(issue.key, { question: unknownContainer }, agent)).id,
    comment: (await createComment(issue.key, { body: unknownContainer }, agent)).id,
    message: (await createMessage(issue.key, { body: unknownContainer }, agent)).id,
  };
  // Each broken body with the lines its card must still draw, so the loop below looks nothing up
  // a second time. The ask is created before the message, which is the order their turns stand
  // in the Conversation.
  const brokenAsk = await createAsk(issue.key, { question: brokenTypedBlocks.ask.body }, agent);
  const brokenMessage = await createMessage(
    issue.key,
    { body: brokenTypedBlocks.message.body },
    agent
  );
  // A message body is stored as sent, so a lone carriage return still ends its lines and a
  // leading U+FEFF still reaches the parser: the drawn fence lines must be cut from the same text,
  // or the body is shown twice.
  const loneCarriageReturn = await createMessage(
    issue.key,
    { body: "Lead **bold**\n\n:::a\rbody words\r:::\n\nAfter" },
    agent
  );
  const byteOrderMark = await createMessage(
    issue.key,
    { body: "\ufeffLead **bold**\n\n:::a\nbody words\n:::\n\nAfter" },
    agent
  );
  return {
    broken: [
      { lines: brokenTypedBlocks.ask.lines, turn: `ask:${brokenAsk.id}` },
      { lines: brokenTypedBlocks.message.lines, turn: `message:${brokenMessage.id}` },
    ],
    container,
    issue: issue.key,
    stored: [loneCarriageReturn.id, byteOrderMark.id],
  };
});

// SLOW(>=2s): 2.7 s on replay - seven cards are read behind one page load: a comment, a message
// and an ask holding the unknown container directive, a message and an ask whose typed block
// breaks its rule, a lone-carriage-return message and a byte-order-mark message.
test("Conversation draws an unknown or broken block as formatted markdown with its fence lines as text, in a comment, a message and an ask, after a lone carriage return and after a byte-order mark @pr", async ({
  page,
  scene,
}) => {
  await page.goto(`/issues/${scene.issue}/conversation`);

  // Each card is found by its turn id, not by its formatted content: a body that fell back to
  // raw text renders `**bold**` and no `strong`, and these assertions then fail.
  for (const [kind, id] of Object.entries(scene.container)) {
    const card = page.locator(`[data-turn="${kind}:${id}"]`);
    await expect(card).toHaveCount(1);
    await expect(card.locator("strong").filter({ hasText: /^bold$/ })).toBeVisible();
    await expect(card.locator("strong").filter({ hasText: /^inner$/ })).toBeVisible();
    // A collapsed comment thread shows its body as a one-line preview, so the fences are
    // asserted as text rather than as paragraphs of their own.
    await expect(card).toContainText(":::note");
    await expect(card).toContainText(/words\s*:::/);
    await expect(card).not.toContainText("**");
    await expect(card.locator("[data-markdown-fallback]")).toHaveCount(0);
  }

  for (const { lines, turn } of scene.broken) {
    const card = page.locator(`[data-turn="${turn}"]`);
    await expect(card).toHaveCount(1);
    await expect(card.locator("strong").filter({ hasText: /^bold$/ })).toBeVisible();
    for (const line of lines) {
      await expect(card).toContainText(line);
    }
    await expect(card.locator("[data-markdown-fallback]")).toHaveCount(0);
  }

  for (const message of scene.stored) {
    const card = page.locator(`[data-turn="message:${message}"]`);
    await expect(card).toHaveCount(1);
    await expect(card.locator("strong").filter({ hasText: /^bold$/ })).toBeVisible();
    for (const line of [":::a", "body words", ":::", "After"]) {
      await expect(card.locator("p").filter({ hasText: new RegExp(`^${line}$`) })).toHaveCount(1);
    }
    await expect(card.locator("[data-markdown-fallback]")).toHaveCount(0);
  }
});
