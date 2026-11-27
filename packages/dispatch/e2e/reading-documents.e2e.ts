/**
 * The reading list's walk (`e2e/reading-rows.ts`) across a document and its
 * margin: the reason a reviewer gave in a document's Reviews dialog, a decision card beside the
 * spec block it shaped, a conversation the reader pinned to the margin, and a thread card's
 * three drawings - its comment opened, its collapsed preview, and a suggestion's two sides.
 * Each row writes the one reading text into the record its surface shows, through the API as an
 * agent or a person writes it, reaches the surface the way its row of
 * `DISPATCH_READING_SURFACES` says, and reads there what the surface's host makes of every form
 * a text names a node by (R6).
 *
 * Every host here but one is a record a reader reads, so each reference is a link. The one that
 * is not is the suggestion's diff, which shows the exact characters it swaps (`ReferenceHostIs
 * host="diff"` in `ThreadCard.tsx`), so its replacement reads as the text was written.
 *
 * The Reviews dialog and the thread cards are reached on a project document, as their rows say.
 * The decision card and the Pinned tab are an issue's alone: the margin reads the decisions that
 * shaped the issue's spec only beside that spec (`showsDecisions`, `useMarginItems.ts`), and it
 * offers the Pinned tab only on an issue (`MarginSheet.tsx`, the tabs a document owner gets). So
 * those two rows reach the host issue's spec and its Conversation, which is where the margin
 * draws them.
 */
import {
  createArtifactComment,
  createIssueArtifact,
  createMessage,
  createProjectDocument,
  editArtifact,
  reviewArtifact,
} from "./api";
import { blockId, connectedDot, documentEditor, openedThreadCard, openSpec } from "./editor";
import {
  expectReadsReferences,
  readingQuestionBlock,
  readingText,
  scopedTo,
  seedReadingWorld,
} from "./reading-rows";
import {
  collapsedMarginThread,
  conversationTurn,
  openMarginThread,
  RENDERED_BODY,
} from "./reference-surfaces";
import { expect, type Page, test } from "./test";

/** The agent session these rows write as, so a write is never the reader's own. */
const readingAgent = {
  actor: { id: "e2e-reading-agent", kind: "session" as const },
  as: "agent" as const,
};

/** The paragraph of the project document every margin thread here is anchored to. */
const ANCHOR = "The reading anchor sits here.";

/** The spec block the decision row's decision shapes, and the heading that decision is under. */
const SHAPED = "The reading host stores each notice.";
const DECISION = "Store the notices";

/** Makes the project document the margin rows are reached on: one paragraph, the anchor, and
 *  the question block. A review of the document and a comment anchored to it are read against
 *  the document itself, so it declares the question a `#id` in them names, and a row whose text
 *  is one of those records takes its world through `scopedTo`. */
async function seedReadingDocument(): Promise<{ id: string; slug: string }> {
  const { artifact } = await createProjectDocument(
    "CORE",
    { content: `${ANCHOR}\n\n${readingQuestionBlock()}\n`, name: "reading.md" },
    readingAgent
  );
  return { id: artifact.id, slug: artifact.slug };
}

/** Opens the project document and waits until its editor shows the anchor and its document is
 *  connected, so a margin card the row reaches for next stands at its mark. */
async function openReadingDocument(page: Page, slug: string): Promise<void> {
  await page.goto(`/projects/CORE/documents/${slug}`);
  await expect(documentEditor(page)).toContainText(ANCHOR);
  await expect(connectedDot(page)).toHaveText("connected");
}

/** A margin thread on the project document, anchored to its one paragraph. */
function anchoredThread(document: { id: string; slug: string }, body: string) {
  return createArtifactComment(
    document.id,
    { anchor: { artifact: document.slug, quote: ANCHOR }, body },
    readingAgent
  );
}

// SLOW(>=2s): the reading world, a project document reviewed, and its page drawn with the
// Reviews dialog open.
test("approval-review-reason: a reviewer's reason in the Reviews dialog draws every form of a stored reference through the renderer", async ({
  page,
}) => {
  const seeded = await seedReadingWorld();
  const document = await seedReadingDocument();
  const world = await scopedTo(seeded, document.id);
  await reviewArtifact(
    document.id,
    { reason: readingText(world), state: "changes_requested" },
    { login: "bob" }
  );

  await openReadingDocument(page, document.slug);
  // The header's chip opens the dialog that lists every review, each with its reason drawn
  // beneath it as a body of its own.
  await page.getByRole("button", { name: "Changes requested" }).click();
  const reviews = page.getByRole("dialog", { name: "Reviews" });
  await expectReadsReferences(reviews.locator(RENDERED_BODY), world, "link");
});

// SLOW(>=2s): the reading world, a spec block and the decision that shaped it, and the spec
// drawn with its margin.
test("margin-decision-passage: a margin decision card's passage draws every form of a stored reference through the renderer", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  const spec = world.host.primary_artifact_id;
  await editArtifact(
    spec,
    { ops: [{ after: "end", markdown: SHAPED, op: "insert" }] },
    readingAgent
  );
  const shaped = await blockId(spec, SHAPED);
  // A decision stands beside a spec block it links, so its passage is the reading text and the
  // link to that block; the link draws as its block's name, which leaves no address as text.
  // The passage is a record of `decisions.md`, so a `#id` in it is read against that document
  // (the card hands the renderer the document as the passage's source): the document declares
  // the question block itself, ahead of its first decision, and the world is scoped to it.
  const { artifact } = await createIssueArtifact(
    world.host.key,
    {
      content: `${readingQuestionBlock()}\n\n## ${DECISION}\n\n${readingText(world)} Shapes dispatch://${world.host.key}/spec#b-${shaped}.\n`,
      name: "decisions.md",
    },
    readingAgent
  );
  const scoped = await scopedTo(world, artifact.id);

  await openSpec(page, world.host.key, SHAPED);
  // The card is an article a click selects, never a link or a button, and its passage is drawn
  // in it, so each reference is a link.
  const card = page.getByRole("article", { name: `Decision: ${DECISION}` });
  await expectReadsReferences(card.locator("[data-decision-text]"), scoped, "link");
});

// SLOW(>=2s): the reading world, a message, and the Conversation drawn while the reader pins it
// and opens the margin's Pinned tab.
test("margin-pinned-conversation: a conversation pinned to the margin draws every form of a stored reference through the renderer", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  const message = await createMessage(world.host.key, { body: readingText(world) }, readingAgent);

  // The reader pins the conversation from its turn, as a person does, and the margin's Pinned
  // tab draws the pinned card's own body.
  await page.goto(`/issues/${world.host.key}/conversation`);
  const turn = conversationTurn(page, "message", message.id);
  await turn.getByRole("button", { name: "Pin" }).click();
  await expect(turn.getByRole("button", { name: "Unpin" })).toHaveAttribute("aria-pressed", "true");
  const margin = page.getByTestId("margin-sheet");
  await margin.getByRole("tab", { name: "Pinned" }).click();
  const pinned = margin
    .getByRole("article")
    .filter({ has: page.getByRole("button", { name: "Unpin" }) });
  await expect(pinned).toHaveCount(1);

  await expectReadsReferences(pinned.locator(RENDERED_BODY), world, "link");
});

// SLOW(>=2s): the reading world, a project document and a suggestion on it, and its page drawn
// with the suggestion's card collapsed and then opened.
test("margin-suggestion-diff: a suggestion card's replacement draws every form but a question id as the characters written", async ({
  page,
}, testInfo) => {
  const world = await seedReadingWorld();
  const document = await seedReadingDocument();
  const suggestion = await createArtifactComment(
    document.id,
    {
      anchor: { artifact: document.slug, quote: ANCHOR },
      body: "Suggested replacement.",
      suggestion: { replace_with: readingText(world) },
    },
    readingAgent
  );

  await openReadingDocument(page, document.slug);
  // The collapsed card and the opened one draw the same diff, clamped and whole: what accepting
  // would take out, and what it would write in its place, each as its exact characters.
  const collapsed = await collapsedMarginThread(page, testInfo.project.name, suggestion.id);
  await expect(collapsed.locator("del")).toHaveText(ANCHOR);
  await expectReadsReferences(collapsed.locator("ins"), world, "written");

  // A suggestion's collapsed card holds no quote above its diff, so the click that opens it lands
  // on the side it would take out, which holds nothing a click could follow.
  await collapsed.locator("del").click();
  const card = openedThreadCard(page, testInfo.project.name, suggestion.id);
  await expect(card.locator('button[aria-expanded="false"]')).toHaveCount(0);
  await expect(card.locator("del")).toHaveText(ANCHOR);
  await expectReadsReferences(card.locator("ins"), world, "written");
});

// SLOW(>=2s): the reading world, a project document and a thread on it, and its page drawn with
// the thread's card opened.
test("margin-comment-body: a thread card's comment draws every form of a stored reference through the renderer", async ({
  page,
}, testInfo) => {
  const seeded = await seedReadingWorld();
  const document = await seedReadingDocument();
  const world = await scopedTo(seeded, document.id);
  const root = await anchoredThread(document, readingText(world));

  await openReadingDocument(page, document.slug);
  const card = await openMarginThread(page, testInfo.project.name, root.id);
  await expectReadsReferences(card.locator(RENDERED_BODY), world, "link");
});

// SLOW(>=2s): the reading world, a project document and a thread on it, and its page drawn with
// the thread's card collapsed.
test("margin-thread-preview: a collapsed thread card's preview draws every form of a stored reference through the renderer", async ({
  page,
}, testInfo) => {
  const seeded = await seedReadingWorld();
  const document = await seedReadingDocument();
  const world = await scopedTo(seeded, document.id);
  const root = await anchoredThread(document, readingText(world));

  await openReadingDocument(page, document.slug);
  const collapsed = await collapsedMarginThread(page, testInfo.project.name, root.id);
  // The collapsed card is the toggle that opens its thread, and its host is still a record a
  // reader reads - the Conversation's whole default view of a turn is this card - so its
  // references stay links (`ThreadCard.tsx`, the comment above the collapsed card's button).
  await expectReadsReferences(collapsed.locator("[data-markdown-preview]"), world, "link");
});
