/**
 * The reading list's walk (`e2e/reading-rows.ts`) over what a person wrote on an
 * ask or in a thread and reads back on their own pages: the Answers page's four texts - the
 * question a row answers, the label of an option the answer chose, the answer written on it, and
 * a reply written in its thread - the subject line the Inbox's reading column opens a Reply
 * thread under, which is where `/replies` lands, and the first line of a record on the person's
 * own page. Each is a stored record, and each draws every form a text names a node by through
 * the one renderer (R6): as its node's current name, the party as `@` and the session's name,
 * and none left as the characters written. None of the six hosts is a link or a button - an
 * Answers row's Open is a link beside its texts, never around them, a chosen option is a pill,
 * the reading column's subject line is a heading, and a person page row's Open is a link below
 * its first line - so each reference is a link there.
 *
 * Each row writes `readingText` into the one record its surface shows and plain words into any
 * other record the surface needs: the question the reading session asks and the options it
 * offers, the answer and the reply alice writes on it, and the comments alice writes, the root
 * of a thread the session answers or a record of her own page.
 */
import { answerAsk, createAsk, createComment } from "./api";
import { inboxPane, selectRow } from "./inbox-pane";
import { replyRowId } from "./inbox-threads";
import {
  expectReadsReferences,
  firstLineForms,
  READING_SESSION_ID,
  readingText,
  seedReadingWorld,
} from "./reading-rows";
import { answeredQuestionPreview, RENDERED_BODY } from "./reference-surfaces";
import { expect, type Locator, type Page, test } from "./test";

/** The reading session as an author: it asks every question here and answers alice's thread,
 *  as an agent session writes. */
const asReadingSession = {
  actor: { id: READING_SESSION_ID, kind: "session" as const },
  as: "agent" as const,
};

/** The options every ask here offers, so that alice's answer picks one. */
const OPTIONS = [{ label: "Yes" }, { label: "No" }];

/** The question of a row whose surface is its answer or its reply: it names nothing, so the one
 *  text in the row that names anything is the one the row reads. */
const PLAIN_QUESTION = "Which one goes first?";

/** The rows of the Answers page that name one ask. */
function answerRow(page: Page, askId: string): Locator {
  return page.locator(`[data-answer-row="${askId}"]`);
}

// SLOW(>=2s): the reading world, an ask asked and answered, and the Answers page drawn.
test("answers-subject: an Answers row's question draws every form of a stored reference through the renderer", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  // The session asks and alice answers: that is what puts the ask on her Answers page, under the
  // question the session wrote.
  const ask = await createAsk(
    world.host.key,
    { options: OPTIONS, question: readingText(world) },
    asReadingSession
  );
  await answerAsk(ask.id, { expected_edited_at: null, selected: ["Yes"] });

  await page.goto("/answers");
  // The row's subject line is the question's preview, drawn in the row itself rather than in its
  // Open link, so its references are links.
  await expectReadsReferences(answeredQuestionPreview(page, ask.id), world, "link");
});

// SLOW(>=2s): the reading world, an ask asked and answered with one of its options, and the
// Answers page drawn.
test("answers-chosen-options: an Answers row's chosen option draws every form of a stored reference through the renderer", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  const label = readingText(world);
  const ask = await createAsk(
    world.host.key,
    { options: [{ label }, { label: "Later" }], question: PLAIN_QUESTION },
    asReadingSession
  );
  await answerAsk(ask.id, { expected_edited_at: null, selected: [label] });

  await page.goto("/answers");
  // The answer chose that option and wrote no text, so the row's one rendered body is the
  // option's pill, below the question's preview: its label is drawn from the tree the row
  // carries beside it, in the row itself rather than in its Open link, so its references are
  // links.
  await expectReadsReferences(answerRow(page, ask.id).locator(RENDERED_BODY), world, "link");
});

// SLOW(>=2s): the reading world, an ask asked and answered, and the Answers page drawn.
test("answers-answer: an Answers row's answer draws every form of a stored reference through the renderer", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  const ask = await createAsk(
    world.host.key,
    { options: OPTIONS, question: PLAIN_QUESTION },
    asReadingSession
  );
  // The answer chooses no option, so no option's pill is a second rendered body in its row
  // (`answers-chosen-options` reads that one).
  await answerAsk(ask.id, {
    expected_edited_at: null,
    selected: [],
    text: readingText(world),
  });

  await page.goto("/answers");
  // An answer's row draws one rendered body, the text written with the answer, below the
  // question's preview.
  await expectReadsReferences(answerRow(page, ask.id).locator(RENDERED_BODY), world, "link");
});

// SLOW(>=2s): the reading world, an ask and a reply in its thread, and the Answers page drawn.
test("answers-reply: an Answers row's reply draws every form of a stored reference through the renderer", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  const ask = await createAsk(
    world.host.key,
    { options: OPTIONS, question: PLAIN_QUESTION },
    asReadingSession
  );
  // A comment alice writes in the ask's thread is a reply of hers on it, which the Answers page
  // lists as a row of its own although she gave no answer.
  await createComment(world.host.key, { ask_id: ask.id, body: readingText(world) });

  await page.goto("/answers");
  const row = answerRow(page, ask.id);
  await expect(row).toContainText("Replied:");
  // A reply's row draws one rendered body, the reply, after its "Replied:" label.
  await expectReadsReferences(row.locator(RENDERED_BODY), world, "link");
});

// SLOW(>=2s): the reading world, a comment thread and its reply for each group of forms a first
// line holds, and the Inbox drawn with each thread open in its reading column.
test("my-reply-subject: a Reply thread's subject line draws every form of a stored reference through the renderer", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  // Alice's root and the session's reply under it make the thread a Reply row of hers, and its
  // subject line is the root's first line as the server cuts it: one thread for each group of
  // forms a first line holds whole (`firstLineForms`).
  const groups = firstLineForms(world);
  const roots: string[] = [];
  for (const forms of groups) {
    const root = await createComment(world.host.key, { body: readingText(world, forms) });
    await createComment(world.host.key, { body: "On it.", reply_to: root.id }, asReadingSession);
    roots.push(root.id);
  }

  // `/replies` lands on the Inbox's Reply rows. The address is read parameter by parameter, since
  // the reading column writes the row it selects into it the moment it opens (`?item=`). At this
  // project's width the Inbox draws that column, which opens the selected thread under its
  // subject line: a heading, not a control, so its references are links.
  await page.goto("/replies");
  await expect(page).toHaveURL(
    (url) => url.pathname === "/" && url.searchParams.get("type") === "reply"
  );
  for (const [index, forms] of groups.entries()) {
    await selectRow(page, replyRowId(roots[index] ?? ""));
    const subject = inboxPane(page).locator("[data-inbox-thread-subject]");
    await expectReadsReferences(subject, world, "link", forms);
  }
});

// SLOW(>=2s): the reading world, a comment of alice's for each group of forms a first line holds,
// and her own page drawn.
test("person-node-title: a person page row's first line draws every form of a stored reference through the renderer", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  // A comment alice writes is a record she authored, so it is a row of her page, under its first
  // line as the server cuts it: one comment for each group of forms a first line holds whole
  // (`firstLineForms`).
  const groups = firstLineForms(world);
  const comments: string[] = [];
  for (const forms of groups) {
    comments.push((await createComment(world.host.key, { body: readingText(world, forms) })).id);
  }

  await page.goto("/people/alice");
  for (const [index, forms] of groups.entries()) {
    // The row's first line is its preview, drawn above its Open link rather than in it, so its
    // references are links.
    const row = page.locator(`[data-person-node="comment:${comments[index] ?? ""}"]`);
    await expectReadsReferences(row.locator("[data-markdown-preview]"), world, "link", forms);
  }
});
