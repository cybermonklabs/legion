/**
 * The reading list's walk (`e2e/reading-rows.ts`) over the surfaces that show a
 * record through another record's reference to it, and over the search palette: the cards under
 * a body that is nothing but references, the two lines a Dispatch card draws of its target, the
 * hover card over a reference to a comment and to an ask, the palette's hits, and the excerpt a
 * backlink panel lists a citing record by. Each is a stored record drawn by the one renderer
 * (R6), so every form a text names a node by draws in it as its node's current name, the party
 * as `@` and the session's name, and none is left as the characters written.
 *
 * Where each reference is a link is the host's to say. A body in the Conversation is a record
 * a reader reads, so its references and the cards after it are links, and so is a backlink's
 * excerpt, which the panel draws beside the link to its source rather than inside it. A card,
 * the hover card and a palette row are each one link or one button already, so what they draw
 * of a record links nothing of its own: the references there are inert names.
 *
 * Each row writes `readingText` through the API into the record its surface shows - a message,
 * a comment or an ask's question as an agent, the ask's answer as alice - and reaches the surface
 * the way its row of `DISPATCH_READING_SURFACES` says. The cards row writes a body of references
 * alone, since that is the only body that has cards, and says so where it does.
 */
import { answerAsk, createAsk, createComment, createMessage } from "./api";
import {
  expectReadsForms,
  expectReadsReferences,
  formReadings,
  type ReadingWorld,
  readingAuthor,
  readingText,
  seedReadingWorld,
} from "./reading-rows";
import { conversationTurn, RENDERED_BODY } from "./reference-surfaces";
import { expect, type Locator, type Page, test } from "./test";

/** Opens the search palette with Ctrl+K, as a reader does on any route, searches for the word
 *  the running row's reading text opens with, and answers the palette. Only this row writes
 *  that word. The text is no more than nine words, and an ask is searched by its question
 *  and then its answer, eighteen:
 *  either is inside the 24 words of the headline's window, so a hit's snippet is its whole text
 *  (`pmdoc.SearchSnippet`) whichever of its words matched. */
async function searchPalette(page: Page, lead: string): Promise<Locator> {
  await page.goto("/");
  await expect(page.getByRole("button", { name: /^search/i })).toBeVisible();
  await page.locator("body").focus();
  await page.keyboard.press("Control+k");
  const dialog = page.getByRole("dialog", { name: "Search" });
  await expect(dialog).toBeVisible();
  await page.getByRole("combobox", { name: "Search" }).fill(lead);
  return dialog;
}

/** Writes a message on the host that names `reference` in prose, hovers the link it draws for
 *  it, and answers the hover card that opens. The prose beside the reference keeps the message
 *  from being a body of references alone, which would draw a card under it as well. */
async function hoverCard(page: Page, world: ReadingWorld, reference: string): Promise<Locator> {
  const referring = await createMessage(
    world.host.key,
    { body: `Before the rollout, see ${reference} first.` },
    readingAuthor
  );
  await page.goto(`/issues/${world.host.key}/conversation`);
  const link = conversationTurn(page, "message", referring.id).locator(
    `a[data-dispatch-ref="${reference}"]`
  );
  await expect(link).toBeVisible();
  await link.hover();
  const card = page.locator("#ref-preview");
  await expect(card).toBeVisible();
  return card;
}

// SLOW(>=2s): the reading world and the host's Conversation drawn with a message's body and the
// card for each reference after it.
test("body-reference-cards: a body of nothing but references draws each form as its node's name, and a card for each after it", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  // A body has cards only when it is its references and nothing else (`Unfurl.tsx`'s
  // `referencesOnly`), so this one is not `readingText`: the picker's party is a link whose words
  // stand beside its address, and the prose around the others is words of its own. It holds the
  // same forms with only spaces between them, the party written as its bare address, which
  // reads as the session's current name alone: the `@` of `readingText` is the words the picker
  // wrote.
  const forms = formReadings(world).map((reading) =>
    reading.form === "party"
      ? { ...reading, reads: reading.name, unread: undefined, written: reading.reference }
      : reading
  );
  const body = forms.map((reading) => reading.written).join(" ");
  const message = await createMessage(world.host.key, { body }, readingAuthor);

  await page.goto(`/issues/${world.host.key}/conversation`);
  const turn = conversationTurn(page, "message", message.id);
  // The body is a record a reader reads, so each reference is a link reading as its node's
  // current name, and none is left as written.
  await expectReadsForms(turn.locator(RENDERED_BODY), forms, "link");

  // The cards after it: one for each reference, in the order the body writes them, each a link
  // to its node reading as the node's own name, which for an issue is its title alone.
  const region = turn.getByRole("region", { name: "References" });
  const cards = region.getByRole("link");
  await expect(cards).toHaveCount(forms.length);
  for (const [index, { name, opens }] of forms.entries()) {
    await expect(cards.nth(index)).toHaveAttribute("href", opens);
    await expect(cards.nth(index).locator("span").first()).toHaveText(name);
  }
  await expect(region).not.toContainText("dispatch://");
});

// SLOW(>=2s): the reading world, two messages, and the host's Conversation drawn with the card
// under the second.
test("dispatch-reference-card: a card's two lines draw every form of its target's stored references as names that link nothing", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  const target = await createMessage(world.host.key, { body: readingText(world) }, readingAuthor);
  // A message's card draws the message's whole text under its title (`useReferenceTarget`), and
  // a body that is the reference alone is what draws the card.
  const referring = await createMessage(
    world.host.key,
    { body: `dispatch://${world.host.key}/message/${target.id}` },
    readingAuthor
  );

  await page.goto(`/issues/${world.host.key}/conversation`);
  const card = conversationTurn(page, "message", referring.id)
    .getByRole("region", { name: "References" })
    .getByRole("link");
  await expect(card).toHaveCount(1);
  // The card is one link to the message, so what it draws of the message links nothing.
  await expectReadsReferences(card.locator("[data-markdown-preview]"), world, "inert");
});

// SLOW(>=2s): the reading world, a comment and a message, and the host's Conversation drawn
// before the hover card opens over it.
test("reference-hover-card: the hover card over a reference to a comment draws every form of its stored references as names that link nothing", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  const comment = await createComment(world.host.key, { body: readingText(world) }, readingAuthor);
  const card = await hoverCard(page, world, `dispatch://${world.host.key}/comment/${comment.id}`);
  // The card is one link to the comment, so what it draws of the comment links nothing.
  await expectReadsReferences(card, world, "inert");
});

// SLOW(>=2s): the reading world, an ask and a message, and the host's Conversation drawn before
// the hover card opens over it.
test("reference-hover-card-ask: the hover card over a reference to an ask draws every form of its stored references as names that link nothing", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  const ask = await createAsk(world.host.key, { question: readingText(world) }, readingAuthor);
  const card = await hoverCard(page, world, `dispatch://${world.host.key}/ask/${ask.id}`);
  // The card heads with the ask's question, inside the one link to the ask.
  await expectReadsReferences(card, world, "inert");
});

// SLOW(>=2s): the reading world, an ask answered by alice, and the palette's search answered by
// Postgres's headline.
test("search-ask-status: an ask hit draws every form of its stored question's and answer's references as names that link nothing", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  const ask = await createAsk(world.host.key, { question: readingText(world) }, readingAuthor);
  const answered = await answerAsk(ask.id, {
    expected_edited_at: null,
    selected: [],
    text: readingText(world),
  });
  expect(answered.answer).not.toBeNull();

  const dialog = await searchPalette(page, world.lead);
  const hit = dialog.locator(`#search-option-ask-${ask.id}`);
  await expect(hit).toContainText("Answered");
  // The hit's two rendered texts: its snippet, which is the question and then the answer the ask
  // is searched by, and the start of the latest answer under its status line (`AskHitStatus`).
  // The row is one button, a `control` host, so neither links anything.
  const previews = hit.locator("[data-markdown-preview]");
  await expect(previews).toHaveCount(2);
  await expectReadsReferences(previews.nth(0), world, "inert");
  await expectReadsReferences(previews.nth(1), world, "inert");
});

// SLOW(>=2s): the reading world, an ask answered by alice with one of its options, and the
// palette's search answered by Postgres's headline.
test("search-ask-chosen-options: an ask hit's chosen option draws every form of a stored reference as a name that links nothing", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  const label = readingText(world);
  // The question opens with the word the palette searches for and names nothing, so the hit is
  // found by it and the one text of the hit's status line that names anything is the label.
  const ask = await createAsk(
    world.host.key,
    { options: [{ label }, { label: "Later" }], question: `${world.lead} which reading first?` },
    readingAuthor
  );
  await answerAsk(ask.id, { expected_edited_at: null, selected: [label] });

  const dialog = await searchPalette(page, world.lead);
  const hit = dialog.locator(`#search-option-ask-${ask.id}`);
  await expect(hit).toContainText("Chose");
  // The hit's snippet is a preview, and the answer wrote no text to preview, so the one body the
  // hit renders is the label `Chose …` names, drawn from the tree the hit carries beside it. The
  // row is one button, a `control` host, so it links nothing.
  await expectReadsReferences(hit.locator(RENDERED_BODY), world, "inert");
});

// SLOW(>=2s): the reading world, a comment, and the palette's search answered by Postgres's
// headline.
test("search-result: a result row's snippet draws every form of a stored reference as a name that links nothing", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  const comment = await createComment(world.host.key, { body: readingText(world) }, readingAuthor);

  const dialog = await searchPalette(page, world.lead);
  const hit = dialog.locator(`#search-option-comment-${comment.id}`);
  await expect(hit).toBeVisible();
  // The row is one button, a `control` host, so the snippet links nothing.
  await expectReadsReferences(hit.locator("[data-markdown-preview]"), world, "inert");
});

// SLOW(>=2s): the reading world, a comment, and the cited issue's page drawn with its header's
// backlink panel open.
test("referenced-by-excerpt: a backlink's excerpt draws every form of a stored reference through the renderer", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  // The comment cites the issue its text names by address, so it is one of that issue's
  // backlinks, and the panel lists it by the head of its own body.
  await createComment(world.host.key, { body: readingText(world) }, readingAuthor);

  await page.goto(`/issues/${world.addressed.key}`);
  const header = page.getByTestId("issue-header");
  await header.getByRole("button", { name: /^Referenced by/ }).click();
  const panel = header.getByRole("region", { name: "Referenced by" });
  await expect(panel).toContainText(`Comment · ${world.host.key}`);
  // The one excerpt the renderer draws here: the issue's other backlink is its own spec
  // document, which is listed by its name, a line of text. A head short enough to show whole is
  // a body and a longer one a clamped preview, and both are the renderer's.
  await expectReadsReferences(panel.locator(".dispatch-markdown"), world, "link");
});
