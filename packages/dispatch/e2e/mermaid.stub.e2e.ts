/** A mermaid block in a comment, drawn from one fixed answer: the Conversation's turn shows the
 *  diagram the browser's own Mermaid worker draws from the body the API returned, with its
 *  source closed behind `Mermaid source`. The row reads what the page draws and nothing the
 *  server decides, so it runs against a recording (packages/dispatch/README.md, "The stubbed
 *  layer"). The family's other rows stay in `e2e/mermaid.e2e.ts`: the document rows, which draw
 *  inside the live document editor or a past version's view; the comment row that reads an edit
 *  reaching the page after it loaded; and the comment rows of the time limit and of hostile
 *  markup. */
import { expect } from "@playwright/test";

import { createComment, createIssue, createProject, fixedKey } from "./api";
import { expectDrawnDiagram, flowchart, openCommentThread } from "./mermaid-helpers";
import { stubbed } from "./stub/stub-test";

/** The key this file's recordings were made under. */
const CORE = fixedKey("CORE", "this file's recordings were made under it");

const test = stubbed(async () => {
  await createProject({ key: CORE, name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Comment diagram" });
  const comment = await createComment(issue.key, { body: flowchart("Flow:", "Start", "Finish") });
  return { comment: comment.id, issue: issue.key };
});

test("a comment holding a mermaid block draws a diagram, its source behind Mermaid source @pr", async ({
  page,
  scene,
}) => {
  await page.goto(`/issues/${scene.issue}/conversation`);
  const card = await openCommentThread(page, scene.comment, "Flow:");
  await expect(card).toContainText("Flow:");
  await expectDrawnDiagram(card, ["Start", "Finish"]);
});
