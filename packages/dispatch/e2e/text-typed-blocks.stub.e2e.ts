/** A decision block (`:::ask`) written into a comment's body, drawn as the decision card in the
 *  Conversation: the header naming it a decision, the urgency pill, the options as the card's
 *  rows, the source list folded away, the urgency accent and the line saying the question is
 *  text. Every assertion reads what the read-only engine draws from one fixed comment body, so
 *  the row runs against a recording rather than against a harness
 *  (packages/dispatch/AGENTS.md, "The stubbed layer"). The rest of `text-typed-blocks.e2e.ts`
 *  stayed where it is: those rows assert inside the live document editor.
 *
 *  A decision block is no shape the API takes in a body - the server refuses such a body with
 *  400 DECISION_BLOCK_IN_BODY - so the seeding writes the comment straight into `comments`
 *  (`e2e/decision-bodies.ts`), as the row did before the move. That seeding runs in record mode
 *  alone, and replay reads the comment's id from the scene recorded beside the HAR.
 */
import { expect } from "@playwright/test";

import { createIssue, createProject } from "./api";
import { expectTextOnlyDecisions, seedDecisionBody } from "./decision-bodies";
import { stubbed } from "./stub/stub-test";

const author = { id: "e2e-text-blocks-agent", kind: "session" as const };

const test = stubbed(async () => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Decision in a comment" });
  const comment = await seedDecisionBody({
    author,
    body: ':::ask{urgency="high"}\nWhich one?\n\n- A: first\n- B: second\n:::\n',
    issueKey: issue.key,
  });
  return { comment, issue: issue.key };
});

test("a comment's ask block shows the decision card @pr", async ({ page, scene }) => {
  await page.goto(`/issues/${scene.issue}/conversation`);

  // The thread, where the comment's body is drawn whole: a collapsed turn shows a two-line
  // preview.
  const thread = page
    .getByRole("list", { name: "Conversation turns" })
    .locator(`:scope > li[data-turn="comment:${scene.comment}"]`);
  await thread.getByRole("button", { name: "Expand thread" }).click();
  await expect(thread.getByRole("button", { name: "Collapse thread" })).toBeVisible();

  const decision = thread.locator('section[data-proof-block-type="ask"]');
  await expect(decision.getByText("Which one?", { exact: true })).toBeVisible();

  // The header names the block a decision and its urgency.
  const header = decision.locator("[data-dispatch-ask-header]");
  await expect(header).toBeVisible();
  await expect(header).toContainText("Decision");
  await expect(header.getByText("Decision", { exact: true })).toBeVisible();
  const pill = decision.locator('[data-dispatch-ask-pill="high"]');
  await expect(pill).toBeVisible();
  await expect(pill).toHaveText("High");

  // The options are the card's rows, label and description each.
  const record = decision.locator("[data-dispatch-ask-record]");
  await expect(record).toBeVisible();
  const rows = record.locator('ul[aria-label="Options"] > li');
  await expect(rows).toHaveCount(2);
  for (const [index, label, description] of [
    [0, "A", "first"],
    [1, "B", "second"],
  ] as const) {
    const row = rows.nth(index);
    await expect(row).toBeVisible();
    await expect(row).toContainText(label);
    await expect(row).toContainText(description);
  }

  // The source "- Option: description" list is there, folded behind the rows.
  const source = decision.locator("[data-proof-block-content] > ul");
  await expect(source).toHaveCount(1);
  await expect(source).toBeHidden();

  // The card's urgency accent: a 4px left border.
  await expect(decision).toHaveCSS("border-left-width", "4px");

  // The one card the thread drew says the question is text, and offers nothing to answer it
  // with.
  await expectTextOnlyDecisions(thread, 1);
});
