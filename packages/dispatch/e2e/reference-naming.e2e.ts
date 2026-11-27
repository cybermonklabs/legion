import { type Body, type Result, routePath } from "../web/src/api/paths";
import { agentMention, agentSessions, setLiveSessions } from "./agents";
import {
  baseUrl,
  createAgentMessage,
  createComment,
  createIssue,
  createIssueArtifact,
  createProject,
  editArtifact,
  getArtifactBlocks,
  getArtifactText,
  getIssueArtifactText,
  patchIssue,
  userHeaders,
} from "./api";
import { documentEditor } from "./editor";
import { commentField, pickReference } from "./ref-picker";
import {
  personReference,
  RENDERED_BODY,
  ROLE_TOKEN,
  referenceAgent,
  referencePerson,
  UNKNOWN_PERSON_LOGIN,
  UNKNOWN_PERSON_REFERENCE,
} from "./reference-surfaces";
import { expect, type Locator, type Page, rowKey, rowSessionId, test } from "./test";
import { asUser } from "./users";

/**
 * The naming rule: one text, read the same way wherever it is shown.
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
  const core = rowKey("CORE");
  await createProject({ key: core, name: "Core" });
  const host = await createIssue({ project: core, title: "Reference host" });
  const target = await createIssue({ project: core, title: "Warm the cache" });
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
  const core = rowKey("CORE");
  await createProject({ key: core, name: "Core" });
  const host = await createIssue({ project: core, title: "Reference host" });
  const target = await createIssue({ project: core, title: "Warm the cache" });
  // An issue, its spec, an item of it and a project that do not exist, beside one that does,
  // so the row reads the class and knows the page drew its references before it counts. The
  // issue is one past the two the row made, and the project is a key no row makes (`rowKey`).
  const missing = [
    `dispatch://${core}-99`,
    `dispatch://${core}-99/spec`,
    `dispatch://${core}-99/comment/00000000-0000-4000-8000-000000000000`,
    `dispatch://${rowKey("NOPE")}-1`,
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
  const core = rowKey("CORE");
  await createProject({ key: core, name: "Core" });
  const host = await createIssue({ project: core, title: "Reference host" });
  const target = await createIssue({ project: core, title: "Warm the cache" });
  // Each read answers 404: the document by slug (`ARTIFACT_NOT_FOUND`), the architecture of a
  // project with no source (`SOURCE_NOT_FOUND`). The session id is the row's own
  // (`rowSessionId`), which no agent ever held.
  const missing = [
    `dispatch://${target.key}/artifact/no-such-doc-md`,
    `dispatch://${core}/component/nope`,
    `dispatch://agent/${rowSessionId("neverheld")}`,
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

// The read route finds a document by its name when no slug is it (`resolveDocumentReference`),
// and a correction of a document's title moves no slug (`renameArtifact`), so a document renamed
// to a slug-shaped name opens at `artifact/<its name>`. The reference written so names it: it
// links and reads as that name, in a comment and in the document editor alike, and its link opens
// the page at that name, which finds the document as the read route does (`artifactAtAddress`)
// rather than answering that the issue holds no such artifact. The name is the document read's
// alone: a block of the document named through it is read by the block read and recorded by the
// reference graph, which each resolve only the slug, so that reference names no block they hold
// and reads as its words with no link, as any reference whose target does not answer does, while
// the same block named through the slug links. The picker writes only the slug.
// SLOW(>=2s): four page loads, each waiting for the reference to draw the document's name or for
// the document it opens to draw.
test("a document's address written with its current name, which is not its slug, links, reads as that name and opens the document, in a comment as in a document, and a block named through it reads as its words", async ({
  browser,
}) => {
  const core = rowKey("CORE");
  await createProject({ key: core, name: "Core" });
  const host = await createIssue({ project: core, title: "Reference host" });
  const target = await createIssue({ project: core, title: "Warm the cache" });
  const uploaded = await createIssueArtifact(target.key, {
    content: "# Notes\n\nThe cache warms first.\n",
    name: "notes.md",
  });
  expect(uploaded.artifact.slug).toBe("notes-md");
  const renamed = "launch-notes";
  const rename: Body<"/api/v1/artifacts/{id}", "patch"> = { name: renamed };
  const correction = await fetch(
    new URL(routePath("patch", "/api/v1/artifacts/{id}", { id: uploaded.artifact.id }), baseUrl),
    {
      body: JSON.stringify(rename),
      headers: { "Content-Type": "application/json", ...(await userHeaders("alice")) },
      method: "PATCH",
    }
  );
  expect(correction.status).toBe(200);
  // The precondition: the read route opens the document by the name the reference writes.
  expect((await getIssueArtifactText(target.key, renamed)).markdown).toContain("warms first");

  const address = `dispatch://${target.key}/artifact/${renamed}`;
  const { markdown } = await getArtifactText(uploaded.artifact.id);
  const line = (await getArtifactBlocks(uploaded.artifact.id)).find((block) =>
    markdown.slice(block.from, block.to).startsWith("The cache warms first.")
  );
  if (line === undefined) {
    throw new Error("setup: the document holds no block for its first line");
  }
  const blockByName = `${address}#b-${line.id}`;
  const blockBySlug = `dispatch://${target.key}/artifact/notes-md#b-${line.id}`;
  const body = `The notes are ${address} now. Their first line is ${blockByName}, which ${blockBySlug} names too.\n`;
  await editArtifact(
    host.primary_artifact_id,
    { ops: [{ after: "end", markdown: body, op: "insert" }] },
    referenceAgent
  );
  const comment = await createComment(host.key, { body }, referenceAgent);
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    const expectDocumentName = async (scope: Locator) => {
      await expect(scope.getByRole("link", { exact: true, name: renamed })).toHaveCount(1);
      await expect(scope.getByRole("link", { exact: true, name: address })).toHaveCount(0);
      // The block named through the slug is a link; the same block named through the name is
      // its words, with no link at all.
      await expect(scope.getByRole("link", { name: /The cache warms first/ })).toHaveCount(1);
      await expect(scope).toContainText(blockByName);
      await expect(scope.getByRole("link", { name: blockByName })).toHaveCount(0);
    };
    // The link's page is the document's: the Artifacts tab open on it, its name as the heading
    // and its text drawn.
    const openAndExpectDocument = async (scope: Locator) => {
      await scope.getByRole("link", { exact: true, name: renamed }).click();
      await expect(page).toHaveURL(`/issues/${target.key}/artifacts/${renamed}`);
      await expect(page.getByRole("tab", { name: "Artifacts" })).toHaveAttribute(
        "aria-selected",
        "true"
      );
      const panel = page.locator("#issue-artifacts-panel");
      await expect(panel.getByRole("heading", { exact: true, name: renamed })).toBeVisible();
      await expect(panel).toContainText("The cache warms first.");
      await expect(page.getByText(`No artifact ${renamed} on ${target.key}.`)).toHaveCount(0);
    };

    await page.goto(`/issues/${host.key}/conversation`);
    const commentBody = (await expandedComment(page, comment.id)).locator(RENDERED_BODY).first();
    await expectDocumentName(commentBody);
    await openAndExpectDocument(commentBody);

    await page.goto(`/issues/${host.key}/spec`);
    await expectDocumentName(documentEditor(page));
    await openAndExpectDocument(documentEditor(page));
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
  const core = rowKey("CORE");
  const { planner } = agentSessions(rowSessionId);
  await createProject({ key: core, name: "Core" });
  const host = await createIssue({ project: core, title: "Reference host" });
  await setLiveSessions([planner]);
  const ended = crypto.randomUUID();
  await createAgentMessage(ended, {
    body: "Before you go: write the hand-off.",
    delivery: "aside",
  });
  const neverHeld = rowSessionId("neverheld");
  const listed = `dispatch://agent/${planner.session_id}`;
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
      await expect(scope.getByRole("link", { exact: true, name: planner.title })).toHaveCount(1);
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

// R4: a mention is written with the name the party had when it was picked, and reads as `@` and
// the name it has now wherever it is shown: in the composer's pill before the send, and in the
// record the send stores. A link to the same party whose words its author wrote another way keeps
// those words through the rename (R8).
// SLOW(>=2s): a pick, a rename that reaches the open page on the shell's 15 s registry poll, and
// a send.
test("a session renamed after it was picked reads as its new name in the pill and the stored record, and a link in other words keeps them", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  const core = rowKey("CORE");
  const { planner } = agentSessions(rowSessionId);
  await createProject({ key: core, name: "Core" });
  const host = await createIssue({ project: core, title: "Reference host" });
  await setLiveSessions([planner]);
  const address = `dispatch://agent/${planner.session_id}`;
  const renamed = "Planner renamed";
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${host.key}/conversation`);
    const field = commentField(page);
    await pickReference(field, "@", planner.title);
    await expect(field).toHaveValue(agentMention(planner));
    await field.press("End");
    await field.pressSequentially(` and [${WORDS}](${address})`);
    const composer = page.getByRole("form", { name: "Comment composer" });
    // One pill for the one party, at its first appearance: the mention.
    const pills = composer.getByRole("region", { name: "References" }).getByRole("link");
    await expect(pills).toHaveText([`@${planner.title}`]);

    // The session is renamed while the draft is open: the pill follows it before the send.
    await setLiveSessions([{ ...planner, title: renamed }]);
    await expect(pills).toHaveText([`@${renamed}`], { timeout: 30_000 });

    const sent = page.waitForResponse(
      (response) =>
        response.request().method() === "POST" &&
        new URL(response.url()).pathname === `/api/v1/issues/${host.key}/comments`
    );
    await composer.getByRole("button", { exact: true, name: "Send" }).click();
    const stored = (await (await sent).json()) as Result<"/api/v1/issues/{key}/comments", "post">;
    // The text keeps the name the pick wrote; only what it reads as follows the party.
    expect(stored.body).toBe(`${agentMention(planner)} and [${WORDS}](${address})`);
    const card = await expandedComment(page, stored.id);
    await expect(card.getByRole("link", { exact: true, name: `@${renamed}` })).toHaveCount(1);
    await expect(card.getByRole("link", { exact: true, name: WORDS })).toHaveCount(1);
    await expect(card.getByRole("link", { exact: true, name: `@${planner.title}` })).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

// R5 and R8: a bare person reads as their login, with no link, where Dispatch knows the login,
// and a login it does not know is the words written; a bare role a live session holds reads as
// its token, linked to its page. The comment and the document editor read the one answer, so
// they agree.
// SLOW(>=2s): two page loads, each waiting for the role's title before it reads the rest.
test("a bare known person reads as their login with no link, an unknown one as written, and a bare role as its token, linked", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  const core = rowKey("CORE");
  const { reviewer } = agentSessions(rowSessionId);
  const personLogin = referencePerson();
  await createProject({ key: core, name: "Core" });
  const host = await createIssue({ project: core, title: "Reference host" });
  // Dana signs in once, which is what makes her a person Dispatch knows.
  await userHeaders(personLogin);
  // A role is named while a session the registry lists holds it.
  await setLiveSessions([{ ...reviewer, roles: [ROLE_TOKEN] }]);
  const person = personReference();
  const body = `Ask ${person}, ${UNKNOWN_PERSON_REFERENCE} and dispatch://role/${ROLE_TOKEN}.`;
  await editArtifact(
    host.primary_artifact_id,
    { ops: [{ after: "end", markdown: body, op: "insert" }] },
    referenceAgent
  );
  const comment = await createComment(host.key, { body }, referenceAgent);
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    const roleLink = (scope: Locator) => scope.getByRole("link", { exact: true, name: ROLE_TOKEN });

    await page.goto(`/issues/${host.key}/conversation`);
    const card = (await expandedComment(page, comment.id)).locator(RENDERED_BODY).first();
    await expect(roleLink(card)).toHaveAttribute("href", `/agents?role=${ROLE_TOKEN}`);
    await expect(card).toContainText(
      `Ask ${personLogin}, ${UNKNOWN_PERSON_REFERENCE} and ${ROLE_TOKEN}.`
    );
    await expect(card.getByRole("link", { name: personLogin })).toHaveCount(0);
    await expect(card.getByRole("link", { name: UNKNOWN_PERSON_LOGIN })).toHaveCount(0);

    await page.goto(`/issues/${host.key}/spec`);
    const editor = documentEditor(page);
    await expect(roleLink(editor)).toHaveCount(1);
    // The editor draws the known login over the address it stores, as words and no link.
    await expect(editor.locator("span.dispatch-reference")).toHaveText([personLogin]);
    await expect(editor.getByRole("link", { name: personLogin })).toHaveCount(0);
    await expect(editor).toContainText(UNKNOWN_PERSON_REFERENCE);
    await expect(editor.getByRole("link", { name: UNKNOWN_PERSON_LOGIN })).toHaveCount(0);
  } finally {
    await alice.close();
  }
});
