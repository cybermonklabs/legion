import { plannerSession, setLiveSessions } from "./agents";
import {
  createAgentMessage,
  createComment,
  createIssue,
  createProject,
  editArtifact,
  patchIssue,
} from "./api";
import { documentEditor } from "./editor";
import {
  RENDERED_BODY,
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

// SLOW(>=2s): two page loads - the Conversation and the document - each waiting for the
// references it draws to settle before it counts links.
test("an address whose target does not exist reads as its writer's words with no link, in a comment as in a document", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const host = await createIssue({ project: "CORE", title: "Reference host" });
  const target = await createIssue({ project: "CORE", title: "Warm the cache" });
  // An issue, its spec, an item of it and a project that do not exist, beside one that does,
  // so the row reads the class and knows the page drew its references before it counts.
  const missing = [
    "dispatch://CORE-99",
    "dispatch://CORE-99/spec",
    "dispatch://CORE-99/comment/00000000-0000-4000-8000-000000000000",
    "dispatch://NOPE-1",
  ];
  const body = `${missing.map((address, index) => `Line ${index + 1} names ${address} here.`).join("\n\n")}\n\nAnd dispatch://${target.key} is real.\n`;
  await editArtifact(
    host.primary_artifact_id,
    { ops: [{ after: "end", markdown: body, op: "insert" }] },
    referenceAgent
  );
  const comment = await createComment(host.key, { body }, referenceAgent);
  const named = `${target.key} ${target.title}`;
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();

    // R8: a reference whose target Dispatch cannot resolve reads as the words the writer wrote,
    // with no link - in a document and in a comment alike (`c-naming-rule`'s negative control).
    const expectWordsWithNoLink = async (scope: Locator) => {
      await expect(scope.getByRole("link", { exact: true, name: named })).toHaveCount(1);
      for (const address of missing) {
        await expect(scope).toContainText(address);
        await expect(scope.getByRole("link", { exact: true, name: address })).toHaveCount(0);
      }
    };

    await page.goto(`/issues/${host.key}/conversation`);
    await expectWordsWithNoLink(await expandedComment(page, comment.id));

    await page.goto(`/issues/${host.key}/spec`);
    await expectWordsWithNoLink(documentEditor(page));
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): two page loads, each waiting for the present reference to draw its title before
// the row reads the three others.
test("a document its issue does not hold, a component of a project with no architecture and a session Dispatch does not know read as their words with no link", async ({
  browser,
}) => {
  // Each soft read waits its full 15 s while the defect stands, so the row reads both surfaces.
  test.setTimeout(240_000);
  await createProject({ key: "CORE", name: "Core" });
  const host = await createIssue({ project: "CORE", title: "Reference host" });
  const target = await createIssue({ project: "CORE", title: "Warm the cache" });
  // Each read answers 404: the document by slug (`ARTIFACT_NOT_FOUND`), the architecture of a
  // project with no source (`SOURCE_NOT_FOUND`). The session id is one no agent ever held.
  const missing = [
    `dispatch://${target.key}/artifact/no-such-doc-md`,
    "dispatch://CORE/component/nope",
    "dispatch://agent/00000000-0000-4000-8000-000000000000",
  ];
  const body = `${missing.map((address, index) => `Line ${index + 1} names ${address} here.`).join("\n\n")}\n\nAnd dispatch://${target.key} is real.\n`;
  await editArtifact(
    host.primary_artifact_id,
    { ops: [{ after: "end", markdown: body, op: "insert" }] },
    referenceAgent
  );
  const comment = await createComment(host.key, { body }, referenceAgent);
  const named = `${target.key} ${target.title}`;
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();

    // R8: each reads as the words its writer wrote, with no link, and the document is not drawn
    // under its issue's title, which would name a node the reference does not name.
    const expectWordsWithNoLink = async (scope: Locator) => {
      await expect(scope.getByRole("link", { exact: true, name: named })).toHaveCount(1);
      for (const address of missing) {
        await expect.soft(scope).toContainText(address);
        await expect.soft(scope.getByRole("link", { exact: true, name: address })).toHaveCount(0);
      }
      await expect
        .soft(scope.getByRole("link", { exact: true, name: target.title }))
        .toHaveCount(0);
    };

    await page.goto(`/issues/${host.key}/conversation`);
    await expectWordsWithNoLink(await expandedComment(page, comment.id));

    await page.goto(`/issues/${host.key}/spec`);
    await expectWordsWithNoLink(documentEditor(page));
  } finally {
    await alice.close();
  }
});

// R8 for the one kind of target the registry alone cannot answer: a session. A session that held
// a role and ended has left the registry but keeps its conversation, which Dispatch holds a
// record of (here, a message sent to it), so its reference still links; an id no agent ever held
// reads as its words; and a session the registry lists reads as its title. The comment and the
// editor read the one answer (`reference-target.ts`'s `useReferenceData`, the registry and the
// batched `GET /api/v1/agents/known`), so they agree.
// SLOW(>=2s): two page loads, each waiting for the listed session's title before it reads.
test("a session Dispatch holds a record of links though it has ended, a listed one reads as its title, and one nobody held reads as its words", async ({
  browser,
}) => {
  test.setTimeout(240_000);
  await createProject({ key: "CORE", name: "Core" });
  const host = await createIssue({ project: "CORE", title: "Reference host" });
  await setLiveSessions([plannerSession]);
  const ended = crypto.randomUUID();
  await createAgentMessage(ended, {
    body: "Before you go: write the hand-off.",
    delivery: "aside",
  });
  const neverHeld = "00000000-0000-4000-8000-000000000001";
  const listed = `dispatch://agent/${plannerSession.session_id}`;
  const endedAddress = `dispatch://agent/${ended}`;
  const neverHeldAddress = `dispatch://agent/${neverHeld}`;
  const body = `The listed one is ${listed} now.\n\nThe ended one was ${endedAddress} once.\n\nAnd ${neverHeldAddress} never was.\n`;
  await editArtifact(
    host.primary_artifact_id,
    { ops: [{ after: "end", markdown: body, op: "insert" }] },
    referenceAgent
  );
  const comment = await createComment(host.key, { body }, referenceAgent);
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    const expectSessions = async (scope: Locator) => {
      await expect(
        scope.getByRole("link", { exact: true, name: plannerSession.title })
      ).toHaveCount(1);
      await expect
        .soft(scope.getByRole("link", { exact: true, name: endedAddress }))
        .toHaveCount(1);
      await expect.soft(scope).toContainText(neverHeldAddress);
      await expect
        .soft(scope.getByRole("link", { exact: true, name: neverHeldAddress }))
        .toHaveCount(0);
    };

    // The comment's own body: its card also names its author, the same session, in its header.
    await page.goto(`/issues/${host.key}/conversation`);
    await expectSessions((await expandedComment(page, comment.id)).locator(RENDERED_BODY).first());

    await page.goto(`/issues/${host.key}/spec`);
    await expectSessions(documentEditor(page));
  } finally {
    await alice.close();
  }
});
