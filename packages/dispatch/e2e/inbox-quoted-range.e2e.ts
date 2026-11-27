import {
  createArtifactComment,
  createComment,
  createIssue,
  createProject,
  createProjectDocument,
  editArtifact,
  meInboxThreads,
} from "./api";
import { inboxList, inboxPane, selectRow } from "./inbox-pane";
import { asSession, holdReadMarks, replyRowId } from "./inbox-threads";
import { expect, type Locator, type Page, test } from "./test";
import { asUser } from "./users";

/**
 * The reading pane shows a comment on document text inside its document, with the quoted range
 * marked (the in-document pane, R3, check c-quoted-range). A comment on an issue's spec and a
 * comment on a project document each open in their document, at the anchored block, with the
 * thread under that block and the comment's own mark lit and in view. A comment whose words were
 * deleted from the spec draws the thread with its quote and the document page's sentence. The
 * pane reads a document only for a row the reader selected whose thread opens in it.
 *
 * Everything here is made up: this repository is public, so nothing names a real person, host or
 * project.
 */

/** The spec's first paragraph. Its second sentence holds the words the third comment is on,
 *  which the test deletes. */
const ORPHAN_SENTENCE = "The legacy pager rota ends on Friday.";
const ORPHAN_QUOTE = "legacy pager rota";
const OPENING = `The canary starts in the east region. ${ORPHAN_SENTENCE}`;

/** The spec's second paragraph: long enough to run far past the pane's foot, with the quoted
 *  words at its end, so the pane must bring the mark into view itself. */
const SPEC_QUOTE = "the mirror replays one window";
const LONG_PARAGRAPH = [
  ...Array.from(
    { length: 90 },
    (_, index) =>
      `Retry note ${index + 1}: each lease keeps a clock of its own and a budget of its own.`
  ),
  `At the end of a cold start, ${SPEC_QUOTE} and then stops.`,
].join(" ");

/** The project document's one paragraph. */
const DOCUMENT_QUOTE = "three attempts per lease";
const DOCUMENT_LINE = `The replay budget is ${DOCUMENT_QUOTE}, and then the mirror gives up.`;

/** Where the mark stands against the box the pane scrolls in, as the reader sees it: `inside`
 *  says whether it stands wholly inside, and the numbers say where each edge is, for a failure
 *  to show. */
function markInPaneBox(page: Page, markId: string) {
  return inboxPane(page).evaluate((pane, id) => {
    const scroller = pane.querySelector('[data-testid="inbox-pane-scroller"]');
    const mark = pane.querySelector(`[data-id="${CSS.escape(id)}"]`);
    if (scroller === null || mark === null) return { inside: false, missing: true };
    const box = scroller.getBoundingClientRect();
    const rect = mark.getBoundingClientRect();
    return {
      box: [box.top, box.bottom],
      inside: rect.height > 0 && rect.top >= box.top && rect.bottom <= box.bottom,
      mark: [rect.top, rect.bottom],
      scrollTop: scroller.scrollTop,
    };
  }, markId);
}

/** Whether the thread's slot is drawn directly after the block it is anchored to. */
function threadUnderBlock(anchored: Locator, blockId: string): Promise<boolean> {
  return anchored.evaluate((frame, id) => {
    const slot = frame.querySelector(`[data-dispatch-thread-slot="${CSS.escape(id)}"]`);
    const block = frame.querySelector(`[data-block-id="${CSS.escape(id)}"]`);
    return slot !== null && block !== null && slot.previousElementSibling === block;
  }, blockId);
}

/** A thread somebody else started on a range, which the reader replied in and that person
 *  answered: the reader's own reply takes the thread out of their Inbox (R1), and the answer
 *  brings it back. */
async function seedIssueThread(issueKey: string, quote: string, body: string) {
  const root = await createComment(
    issueKey,
    { anchor: { artifact: "spec", quote }, body },
    { login: "bob" }
  );
  await createComment(issueKey, { body: "Which window do you mean?", reply_to: root.id });
  await createComment(
    issueKey,
    { body: "The last one before the lease ends.", reply_to: root.id },
    { login: "bob" }
  );
  return root;
}

// SLOW(>=2s): nine comments seeded over a spec and a project document, an edit polled until it
// orphans one anchor, then three rows read in the pane and a 600 ms wait for no further read.
test("the reading pane marks the quoted range of a comment on document text", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: `${OPENING}\n\n${LONG_PARAGRAPH}\n`,
    title: "Mirror cold start",
  });
  const specRoot = await seedIssueThread(issue.key, SPEC_QUOTE, "Is one window enough?");
  const orphanRoot = await seedIssueThread(issue.key, ORPHAN_QUOTE, "Who holds the pager then?");

  const document = await createProjectDocument("CORE", {
    content: `${DOCUMENT_LINE}\n`,
    name: "replay-budget.md",
  });
  const documentRoot = await createArtifactComment(
    document.artifact.id,
    { anchor: { artifact: document.artifact.slug, quote: DOCUMENT_QUOTE }, body: "Per lease?" },
    { login: "bob" }
  );
  await createArtifactComment(document.artifact.id, {
    body: "Per lease, yes.",
    reply_to: documentRoot.id,
  });
  await createArtifactComment(
    document.artifact.id,
    { body: "Then the budget holds.", reply_to: documentRoot.id },
    { login: "bob" }
  );

  // The third comment's words are deleted from the spec, which orphans its anchor.
  await editArtifact(
    issue.primary_artifact_id,
    { ops: [{ find: ORPHAN_SENTENCE, op: "delete" }], summary: "The pager rota goes" },
    asSession()
  );
  await expect
    .poll(async () => {
      const { rows } = await meInboxThreads({ state: "inbox" });
      return rows.find((row) => row.thread_id === orphanRoot.id)?.anchor?.orphaned;
    })
    .toBe(true);

  const specBlock = specRoot.anchor?.block_id;
  const specMark = specRoot.anchor?.mark_id;
  const documentBlock = documentRoot.anchor?.block_id;
  const documentMark = documentRoot.anchor?.mark_id;
  if (
    specBlock === null ||
    specBlock === undefined ||
    specMark === undefined ||
    documentBlock === null ||
    documentBlock === undefined ||
    documentMark === undefined
  ) {
    throw new Error("a seeded comment was anchored to no block");
  }

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.setViewportSize({ height: 900, width: 1440 });
    await holdReadMarks(page);
    // Every document read the page sends, `GET /api/v1/artifacts/{id}`, by the document's id.
    const reads: string[] = [];
    page.on("request", (request) => {
      const read = /^\/api\/v1\/artifacts\/([^/]+)$/.exec(new URL(request.url()).pathname);
      if (request.method() === "GET" && read !== null) reads.push(read[1] ?? "");
    });
    const readsOf = (id: string) => reads.filter((read) => read === id).length;

    // The spec's thread is the one the address selects, so no other row is selected first.
    await page.goto(`/?item=${encodeURIComponent(replyRowId(specRoot.id))}`);
    await expect(inboxList(page).locator('[data-inbox-row^="reply:"]')).toHaveCount(3);
    const pane = inboxPane(page);

    // The spec's thread: the spec drawn in the pane, the comment's mark lit and holding the
    // quoted words, the mark in the pane's visible box, and the thread under its block.
    const specAnchored = pane.locator(`[data-dispatch-thread-anchor="${specBlock}"]`);
    await expect(specAnchored).toBeVisible();
    await expect(specAnchored.locator(`[data-block-id="${specBlock}"]`)).toContainText(
      LONG_PARAGRAPH.slice(0, 40)
    );
    const specMarked = pane.locator(`[data-id="${specMark}"]`);
    await expect(specMarked).toHaveText(SPEC_QUOTE);
    await expect(specMarked.locator(".dispatch-mark-active")).toHaveCount(1);
    await expect.poll(() => markInPaneBox(page, specMark)).toMatchObject({ inside: true });
    await expect(specAnchored.locator("[data-inbox-thread-body]")).toBeVisible();
    expect(await threadUnderBlock(specAnchored, specBlock)).toBe(true);
    // One read of the spec, and none of the project document, whose row is not selected.
    expect(readsOf(issue.primary_artifact_id)).toBe(1);
    expect(readsOf(document.artifact.id)).toBe(0);

    // The project document's thread, drawn the same way, and the spec's mark no longer lit.
    await selectRow(page, replyRowId(documentRoot.id));
    const documentAnchored = pane.locator(`[data-dispatch-thread-anchor="${documentBlock}"]`);
    await expect(documentAnchored).toBeVisible();
    await expect(documentAnchored.locator(`[data-block-id="${documentBlock}"]`)).toHaveText(
      DOCUMENT_LINE
    );
    const documentMarked = pane.locator(`[data-id="${documentMark}"]`);
    await expect(documentMarked).toHaveText(DOCUMENT_QUOTE);
    await expect(documentMarked.locator(".dispatch-mark-active")).toHaveCount(1);
    await expect(pane.locator(`[data-id="${specMark}"] .dispatch-mark-active`)).toHaveCount(0);
    await expect.poll(() => markInPaneBox(page, documentMark)).toMatchObject({ inside: true });
    await expect(documentAnchored.locator("[data-inbox-thread-body]")).toBeVisible();
    expect(await threadUnderBlock(documentAnchored, documentBlock)).toBe(true);
    expect(readsOf(issue.primary_artifact_id)).toBe(1);
    expect(readsOf(document.artifact.id)).toBe(1);

    // The orphaned thread: the thread with its quote, as a card draws it, and the sentence.
    await selectRow(page, replyRowId(orphanRoot.id));
    const fallback = pane.locator("[data-inbox-pane-fallback]");
    await expect(fallback.locator("[data-inbox-thread-quote]")).toHaveText(ORPHAN_QUOTE);
    await expect(fallback).toContainText(
      "Text changed. The selected range no longer exists in this document."
    );
    await expect(fallback.getByText("The last one before the lease ends.")).toBeVisible();
    // Longer than the pane waits for the selection to stand still before it reads a document.
    await page.waitForTimeout(600);
    // One read for each selected row whose thread opens in its document, and nothing else.
    expect([...reads].sort()).toEqual([document.artifact.id, issue.primary_artifact_id].sort());
  } finally {
    await alice.close();
  }
});
