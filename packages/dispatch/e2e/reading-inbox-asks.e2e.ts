/**
 * The reading list's walk (`e2e/reading-rows.ts`) over an ask as the Inbox
 * draws it: an open ask's row and its card's question, its options and its earlier wording, and
 * the record an answered and a resolved ask's card becomes, with its earlier answers and the
 * options they chose. Each is a record a reader meets on `/`, so each must draw every form a
 * stored text names a node by through the one renderer (R6): as its node's current name, the
 * party as `@` and its current name, and none left as the characters written.
 *
 * Every one of these surfaces is a record a reader reads, so each reference in it is a link. The
 * card draws its texts in plain boxes; an option's text sits in the `<label>` of its radio, which
 * is neither a link nor a button; and an ask's short row is a list item a click selects, which
 * leaves a click on a link inside it to the link (`Inbox.tsx`'s `ROW_CONTROL_SELECTOR`).
 *
 * An open ask is an ask row of the reader's Inbox, which the first four rows read. An answered
 * or a resolved ask is no longer one, so the last four reach it the way the Inbox does: a thread
 * the reader wrote in - their answer, or their reply under the ask - that the asking session has
 * written in since is a Reply row, and the reading column draws that row's ask as its card
 * (`ThreadBody.tsx`'s `AskThreadBody`), which for a closed ask is its record.
 *
 * Every row runs at the `chromium` project's 1280 px, where the Inbox draws its reading column
 * (`e2e/inbox-pane.ts`'s `READING_PANE_WIDTH`) and an ask's row is the short row that draws its
 * question as one line. No row carries `@phone`: the `iphone` project's 390 px never draws the
 * column.
 *
 * Everything here is made up: this repository is public, so nothing names a real person, host or
 * project.
 */
import { answerAsk, createAsk, createComment, editAsk, resolveAsk } from "./api";
import { askCard, inboxPane, inboxRow, selectRow } from "./inbox-pane";
import { replyRowId } from "./inbox-threads";
import { expectReadsReferences, readingText, seedReadingWorld } from "./reading-rows";
import { RENDERED_BODY } from "./reference-surfaces";
import { expect, test } from "./test";

/** The session that asks every question here, answers the reader under the closed ones and
 *  resolves one. It is a session of its own rather than the reading world's, so the one party
 *  any scope here holds is the one the stored text names, never a card's own line naming who
 *  asked or who reworded. */
const asker = {
  actor: { id: "e2e-reading-asks", kind: "session" as const },
  as: "agent" as const,
};

/** The question of a row whose stored text is somewhere else in the ask: it names nothing, so
 *  the one text in the row that names anything is the one the row reads. */
const PLAIN_QUESTION = "Which one goes first?";

// SLOW(>=2s): a database reset, the reading world, an open ask, and the Inbox drawn with it.
test("inbox-ask-row: an ask row's one-line question draws every form of a stored reference through the renderer", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  const ask = await createAsk(world.host.key, { question: readingText(world) }, asker);

  await page.goto("/");
  // The row's one line is the renderer's preview of the question. The owner link under it names
  // the host issue, which the stored text does not, so the scope is the preview alone.
  const line = inboxRow(page, ask.id).locator("[data-markdown-preview]");
  await expect(line).toHaveCount(1);
  await expectReadsReferences(line, world, "link");
});

// SLOW(>=2s): a database reset, the reading world, an open ask, and the Inbox opened on its card.
test("ask-card-question: an ask card's question draws every form of a stored reference through the renderer", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  const ask = await createAsk(world.host.key, { question: readingText(world) }, asker);

  await page.goto("/");
  const card = await askCard(page, ask.id);
  // An ask anchored to nothing and asked in no block opens on its question, so the question is
  // the first body the card draws.
  await expectReadsReferences(card.locator(RENDERED_BODY).first(), world, "link");
});

// SLOW(>=2s): a database reset, the reading world, an open ask with two options, and the Inbox
// opened on its card.
test("ask-option-text: an ask option's label and description draw every form of a stored reference through the renderer", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  const ask = await createAsk(
    world.host.key,
    {
      options: [{ description: readingText(world), label: readingText(world) }, { label: "Later" }],
      question: PLAIN_QUESTION,
    },
    asker
  );

  await page.goto("/");
  const card = await askCard(page, ask.id);
  // The first option's row holds the label and then the description, each drawn by the
  // renderer on its own, so each is read on its own.
  const option = card.getByRole("group", { name: "Answer options" }).locator("label").first();
  const texts = option.locator(RENDERED_BODY);
  await expect(texts).toHaveCount(2);
  await expectReadsReferences(texts.first(), world, "link");
  await expectReadsReferences(texts.last(), world, "link");
});

// SLOW(>=2s): a database reset, the reading world, an open ask reworded once, and the Inbox
// opened on its card with its earlier wording shown.
test("ask-edit-history: an ask card's earlier wording draws every form of a stored reference through the renderer", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  const ask = await createAsk(world.host.key, { question: readingText(world) }, asker);
  // The session rewords its question, so the stored text is the wording the card keeps as the
  // version before.
  await editAsk(ask.id, { question: PLAIN_QUESTION }, asker);

  await page.goto("/");
  const card = await askCard(page, ask.id);
  await card.getByText("Show 1 previous version").click();
  const history = card.locator("details").filter({ hasText: "Show 1 previous version" });
  await expectReadsReferences(history, world, "link");
});

// SLOW(>=2s): a database reset, the reading world, an ask answered and replied under, and the
// Inbox opened on its Reply row.
test("answered-ask: an answered ask's question and answer draw every form of a stored reference through the renderer", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  const ask = await createAsk(world.host.key, { question: readingText(world) }, asker);
  // The reader's answer is their write in the ask's thread, and the session's reply is the write
  // since, which together make the thread a Reply row of the reader's Inbox.
  await answerAsk(ask.id, { expected_edited_at: null, selected: [], text: readingText(world) });
  await createComment(world.host.key, { ask_id: ask.id, body: "Reading both now." }, asker);

  await page.goto("/");
  await selectRow(page, replyRowId(ask.id));
  // The record draws the question and then the answer, and an ask with no options and no
  // earlier wording draws no other body.
  const bodies = inboxPane(page).getByTestId(`ask-${ask.id}`).locator(RENDERED_BODY);
  await expect(bodies).toHaveCount(2);
  await expectReadsReferences(bodies.first(), world, "link");
  await expectReadsReferences(bodies.last(), world, "link");
});

// SLOW(>=2s): a database reset, the reading world, an ask answered twice and replied under, and
// the Inbox opened on its Reply row with its earlier answer shown.
test("ask-answer-history: an ask card's earlier answer draws every form of a stored reference through the renderer", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  const ask = await createAsk(world.host.key, { question: PLAIN_QUESTION }, asker);
  // The reader answers with the stored text and then changes the answer, so the stored text is
  // the answer the record keeps as the earlier one.
  const first = await answerAsk(ask.id, {
    expected_edited_at: null,
    selected: [],
    text: readingText(world),
  });
  const answeredAt = first.answer?.at;
  if (answeredAt === undefined) {
    throw new Error("setup: the reader's first answer came back with no answer on the ask");
  }
  await answerAsk(ask.id, {
    expected_answer_at: answeredAt,
    expected_edited_at: null,
    selected: [],
    text: "The cache, then the queue.",
  });
  await createComment(world.host.key, { ask_id: ask.id, body: "Taking the cache first." }, asker);

  await page.goto("/");
  await selectRow(page, replyRowId(ask.id));
  const record = inboxPane(page).getByTestId(`ask-${ask.id}`);
  await record.getByText("Show 1 earlier answer").click();
  const history = record.locator("details").filter({ hasText: "Show 1 earlier answer" });
  await expectReadsReferences(history, world, "link");
});

// SLOW(>=2s): a database reset, the reading world, an ask answered with one option and then
// another and replied under, and the Inbox opened on its Reply row with its earlier answer shown.
test("earlier-chosen-option: the option an ask card's earlier answer chose draws every form of a stored reference through the renderer", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  const label = readingText(world);
  const ask = await createAsk(
    world.host.key,
    { options: [{ label }, { label: "Later" }], question: PLAIN_QUESTION },
    asker
  );
  // The reader chooses the option holding the stored text and then changes the answer to the
  // other, so the stored text is the option the record's earlier answer chose.
  const first = await answerAsk(ask.id, { expected_edited_at: null, selected: [label] });
  const answeredAt = first.answer?.at;
  if (answeredAt === undefined) {
    throw new Error("setup: the reader's first answer came back with no answer on the ask");
  }
  await answerAsk(ask.id, {
    expected_answer_at: answeredAt,
    expected_edited_at: null,
    selected: ["Later"],
  });
  await createComment(world.host.key, { ask_id: ask.id, body: "Taking the cache first." }, asker);

  await page.goto("/");
  await selectRow(page, replyRowId(ask.id));
  const record = inboxPane(page).getByTestId(`ask-${ask.id}`);
  await record.getByText("Show 1 earlier answer").click();
  // The earlier answer chose that option and wrote no text, so the one rendered body of the
  // list is the option's label, drawn from the tree the answer carries beside it. The record's
  // own options above the list are the ask's, and `ask-option-text` reads those.
  const history = record.locator("details").filter({ hasText: "Show 1 earlier answer" });
  await expectReadsReferences(history.locator(RENDERED_BODY), world, "link");
});

// SLOW(>=2s): a database reset, the reading world, an ask replied under and resolved, and the
// Inbox opened on its Reply row.
test("resolved-ask: a resolved ask's question and reason draw every form of a stored reference through the renderer", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  const ask = await createAsk(world.host.key, { question: readingText(world) }, asker);
  // The reader's reply under the ask is their write in its thread and the session's reply the
  // write since; once the session resolves the ask it is no open ask, and the Inbox lists that
  // thread as a Reply row.
  await createComment(world.host.key, { ask_id: ask.id, body: "Does the order matter here?" });
  await createComment(world.host.key, { ask_id: ask.id, body: "Settled elsewhere." }, asker);
  await resolveAsk(ask.id, { kind: "resolved", reason: readingText(world) }, asker);

  await page.goto("/");
  await selectRow(page, replyRowId(ask.id));
  const record = inboxPane(page).getByTestId(`ask-${ask.id}`);
  await expect(record.getByTestId("ask-resolution-badge")).toHaveText("Resolved");
  // The record draws the question and then, on its last line, the reason, and an ask with no
  // options and no earlier wording draws no other body.
  const bodies = record.locator(RENDERED_BODY);
  await expect(bodies).toHaveCount(2);
  await expectReadsReferences(bodies.first(), world, "link");
  await expectReadsReferences(bodies.last(), world, "link");
});
