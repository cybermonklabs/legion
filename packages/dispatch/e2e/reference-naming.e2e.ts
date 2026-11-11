import { createComment, createIssue, createProject, editArtifact, patchIssue } from "./api";
import { documentEditor } from "./editor";
import {
  referenceAgent,
  UNKNOWN_PERSON_LOGIN,
  UNKNOWN_PERSON_REFERENCE,
} from "./reference-surfaces";
import { expect, type Locator, type Page, test } from "./test";
import { asUser } from "./users";

/**
 * INFRA-1702's `c-naming-rule`: one text, read the same way wherever it is shown.
 *
 * An address standing on its own, and a link whose words are that same address, read as the
 * target's current name; a link whose words its author wrote keeps those words, because the
 * author has already said what the reference is there to say. Renaming the target moves the
 * first and leaves the second, with no reload.
 *
 * The text is seeded through the API before the page loads, which is the point: a row that
 * types it would pass against a renderer that applied the rule only to text written in front of
 * it. The same text is the host's spec and its one comment, so the rule is read on a rendered
 * body and in the document editor rather than on one surface alone.
 */

const WORDS = "the warm one";

/** The comment's Conversation card, expanded. A collapsed card draws the inert one-line
 *  preview, whose references are spans rather than links (`reference-titles.e2e.ts` reads the
 *  margin's collapsed card the same way); the comment itself, with live links, is what the
 *  expanded card holds. */
async function expandedComment(page: Page, id: string): Promise<Locator> {
  const card = page.locator(`[data-turn="comment:${id}"]`);
  await card.getByRole("button", { name: "Expand thread" }).click();
  await expect(card.getByRole("button", { name: "Collapse thread" })).toBeVisible();
  return card;
}

// SLOW(>=2s): two page loads - the Conversation and the document - and a rename that has to
// reach the open page through the event stream rather than a reload.
test("a bare address reads as the target's name, a worded link as its author's words, and a rename moves only the first", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const host = await createIssue({ project: "CORE", title: "Reference host" });
  const target = await createIssue({ project: "CORE", title: "Warm the cache" });
  const address = `dispatch://${target.key}`;
  // One text, three references to the one node and one to a login Dispatch has never held. It
  // is the host's spec and its one comment alike, so the rule is read on two surfaces.
  const body = `${address} and [${WORDS}](${address}) and [${address}](${address}) and ${UNKNOWN_PERSON_REFERENCE}.`;
  await editArtifact(
    host.primary_artifact_id,
    { ops: [{ after: "end", markdown: body, op: "insert" }] },
    referenceAgent
  );
  const comment = await createComment(host.key, { body }, referenceAgent);

  const named = (title: string) => `${target.key} ${title}`;
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${host.key}/conversation`);
    const card = await expandedComment(page, comment.id);

    // The address on its own and the link whose words are that address read as the node's
    // current name; the link whose words the author wrote keeps them.
    await expect(card.getByRole("link", { exact: true, name: named(target.title) })).toHaveCount(2);
    await expect(card.getByRole("link", { exact: true, name: WORDS })).toHaveCount(1);
    // A login Dispatch does not hold names nobody, so it is the words it was written with and
    // no link at all.
    await expect(card).toContainText(UNKNOWN_PERSON_LOGIN);
    await expect(card.getByRole("link", { name: UNKNOWN_PERSON_LOGIN })).toHaveCount(0);

    // The rename reaches the open page through the stream: the two addressed references follow
    // the node, the author's words do not.
    await patchIssue(target.key, { title: "Warm the cache first" });
    await expect(
      card.getByRole("link", { exact: true, name: named("Warm the cache first") })
    ).toHaveCount(2, { timeout: 30_000 });
    await expect(card.getByRole("link", { exact: true, name: WORDS })).toHaveCount(1);
    // Exactly the old name, which the new one holds as a prefix: the old reading is gone.
    await expect(
      card.getByRole("link", { exact: true, name: named("Warm the cache") })
    ).toHaveCount(0);

    // The same text in the host's own document reads the same way: the editor draws the two
    // addressed references as the node's name and leaves the author's words standing.
    await page.goto(`/issues/${host.key}/spec`);
    const editor = documentEditor(page);
    await expect(
      editor.getByRole("link", { exact: true, name: named("Warm the cache first") })
    ).toHaveCount(2);
    await expect(editor.getByRole("link", { exact: true, name: WORDS })).toHaveCount(1);
    await expect(editor).toContainText(UNKNOWN_PERSON_LOGIN);
    await expect(editor.getByRole("link", { name: UNKNOWN_PERSON_LOGIN })).toHaveCount(0);
  } finally {
    await alice.close();
  }
});
