import { GOING_EVENT_END, GOING_EVENT_TEXT, type GraphFrame } from "@legion/contracts";
import type { Locator } from "@playwright/test";

import type { GraphReferences } from "../web/src/api/types";
import { buildRunPath } from "../web/src/features/refs/routes";
import { issueProject } from "../web/src/lib/issue-key";
import {
  answerAsk,
  createAsk,
  createComment,
  createHandoff,
  createIssueArtifact,
  createMessage,
} from "./api";
import { indexedBlockAsk } from "./editor";
import { nodeRow, openIssue, READER, rowWords } from "./issue-page-g";
import { daemonOf, historyItem, historyPanel } from "./issue-page-m";
import { withIssuePage } from "./issue-page-option";
import { mainContent, runAs, runReference } from "./issue-page-z";
import {
  expectReadsReferences,
  formReadings,
  READING_LEAD,
  READING_SESSION_ID,
  READING_SESSION_TITLE,
  type ReadingWorld,
  readingQuestionBlock,
  readingText,
  scopedTo,
  seedReadingWorld,
} from "./reading-rows";
import { conversationTurn, RENDERED_BODY } from "./reference-surfaces";
import { expect, test } from "./test";

/**
 * The reading list's walk (`e2e/reading-rows.ts`) across an issue's own pages:
 * the Activity tab's event body and an event's line, the three turns of the Conversation tab, the
 * quote above a reply and a handoff card's body there, a decision on the Decisions tab, and a
 * comment's row on the new issue page. Each row writes the one reading text into the record its
 * surface shows, through the API as an agent or a person writes it, reaches the surface the way
 * its row of `DISPATCH_READING_SURFACES` says, and reads there every form a text names a node by,
 * drawn through the one renderer (R6).
 *
 * Every surface here is a record a reader reads, so each reference is a link, but for the quote
 * above a reply: the quote is the link to its parent's turn, so the references inside it draw as
 * their names and link nothing. A handoff is no record of the reference graph, so a `#id` in
 * its words names nothing, and its driver holds the other five forms. A row of the new page reads
 * no question map, so a `#id` in its words is drawn as written, and its driver holds the rest.
 *
 * Two drivers read the new page's Details history: a turn of a thread the history holds, which
 * reads every form, and a handoff's opened fields, which read all but a question id, since a
 * handoff is no record of the reference graph.
 *
 * One driver reads a command bar run's own page, which the new issue page draws for the person
 * who asked the run alone: the run's answer, drawn through the renderer, which is no record of
 * the reference graph, so its driver holds every form but a question id. The stack has no model,
 * so a run it records answers one frame, its error (`e2e/issue-page-z.e2e.ts`), and no write of
 * the API writes a run's frames: the driver answers the page's read of the run as the server
 * answered it, with the reading text as the answer's text frame in place of that error.
 */

/** The agent session these rows write as, so a write is never the reader's own. */
const readingAgent = {
  actor: { id: "e2e-reading-agent", kind: "session" as const },
  as: "agent" as const,
};

/** The question of the one decision the decision row writes, and the block it is asked in: a
 *  person's answer to it is that decision. */
const DECISION = "Which references does the reading name?";
const DECISION_BLOCK = "d-read";

// SLOW(>=2s): seeds the reading world and a comment, loads the Activity tab, pins the comment's
// row, and opens the margin's Pinned tab, which reads the pinned event back before it draws it.
test("activity-event-body: a pinned Activity event draws every form of a stored reference through the renderer", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  await createComment(world.host.key, { body: readingText(world) }, readingAgent);

  // The Activity tab draws an event's line and no body; the margin's Pinned tab is where it draws
  // an event's body (`EventBody`), so the comment's row is pinned first.
  await page.goto(`/issues/${world.host.key}/activity`);
  const row = page
    .getByRole("list", { name: "Issue activity" })
    .getByRole("listitem")
    .filter({ hasText: "commented on" });
  await expect(row).toHaveCount(1);
  await row.getByRole("button", { name: "Pin" }).click();
  await expect(row.getByRole("button", { name: "Unpin" })).toHaveAttribute("aria-pressed", "true");
  const margin = page.getByTestId("margin-sheet");
  await margin.getByRole("tab", { name: "Pinned" }).click();

  await expectReadsReferences(margin.locator("article").locator(RENDERED_BODY), world, "link");
});

// SLOW(>=2s): the reading world's seeding and an ask, then one cold load of the Activity tab;
// measured at 4.4 s on one run in an issue pod on one worker.
test("event-line: an Activity event's line draws every form of the record's text it quotes through the renderer", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  // An ask's question is the text of the record its `asked “…”` line quotes. The ask is the
  // host's, so a `#id` in its question is read against the host's spec, which declares it.
  await createAsk(world.host.key, { question: readingText(world) }, readingAgent);

  await page.goto(`/issues/${world.host.key}/activity`);
  // The question block the host's spec declares is an ask of its own, with an `asked “…”` line
  // too; the reading text is the one that opens with the lead word.
  const line = page
    .getByRole("list", { name: "Issue activity" })
    .getByRole("listitem")
    .filter({ hasText: `asked “${READING_LEAD}` });
  await expect(line).toHaveCount(1);

  // The line is in no link or button, so the question it quotes draws inline through the
  // renderer with its references linked (`SentenceParts`).
  await expectReadsReferences(line.locator(".dispatch-markdown"), world, "link");
});

// SLOW(>=2s): seeds the reading world - a project, the fake registry's session, four issues, a
// comment and a settled question - and a two-message thread before one cold load of the
// Conversation.
test("conversation-reply: a reply turn draws every form of a stored reference through the renderer", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  const root = await createMessage(world.host.key, { body: "Where does the reading start?" });
  const reply = await createMessage(
    world.host.key,
    { body: readingText(world), in_reply_to: root.id },
    readingAgent
  );

  await page.goto(`/issues/${world.host.key}/conversation`);
  // The reply's turn holds the quote of its parent as well, which is a preview and no body.
  const turn = conversationTurn(page, "message", reply.id);
  await expect(turn).toHaveAttribute("data-reply", "");

  await expectReadsReferences(turn.locator(RENDERED_BODY), world, "link");
});

test("conversation-message: a message turn draws every form of a stored reference through the renderer", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  const message = await createMessage(world.host.key, { body: readingText(world) }, readingAgent);

  await page.goto(`/issues/${world.host.key}/conversation`);

  await expectReadsReferences(
    conversationTurn(page, "message", message.id).locator(RENDERED_BODY),
    world,
    "link"
  );
});

test("conversation-targeted-message: a message sent to a session draws every form of a stored reference through the renderer", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  // A person's message sent to the live session the world lists, which the Conversation draws as
  // a targeted card, with a line naming whom it was sent to, rather than as a plain message turn.
  const message = await createMessage(world.host.key, {
    body: readingText(world),
    delivery: "steer",
    target: `session:${READING_SESSION_ID}`,
  });

  await page.goto(`/issues/${world.host.key}/conversation`);
  const card = conversationTurn(page, "message", message.id);
  await expect(card).toContainText(`Sent to ${READING_SESSION_TITLE} (Send)`);

  await expectReadsReferences(card.locator(RENDERED_BODY), world, "link");
});

test("reply-quote: the quote above a reply draws every form of its parent's references as a name inside its link", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  const parent = await createMessage(world.host.key, { body: readingText(world) }, readingAgent);
  const reply = await createMessage(world.host.key, {
    body: "Reading it now.",
    in_reply_to: parent.id,
  });

  await page.goto(`/issues/${world.host.key}/conversation`);
  // The quote is the one link to the parent's turn (`ReplyQuote`'s `ReferenceHostIs` control),
  // so the parent's references inside it are names that link nothing.
  const quote = conversationTurn(page, "message", reply.id).locator(
    "a[href] [data-markdown-preview]"
  );

  await expectReadsReferences(quote, world, "inert");
});

// SLOW(>=2s): the reading world's seeding and a handoff, then one cold load of the Conversation
// and the handoff's own read once its card opens; measured at 4.2 s on one run in an issue pod on
// one worker.
test("conversation-handoff-body: a handoff card's field of words draws every form but a question id through the renderer", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  // A handoff's body is the JSON object its phase wrote; a string field of it is the words its
  // session wrote, which the card draws from the reading the server sends for that field.
  await createHandoff(
    world.host.key,
    "implement",
    { phase: "implement", summary: readingText(world) },
    readingAgent.actor
  );

  await page.goto(`/issues/${world.host.key}/conversation`);
  const card = page
    .getByRole("list", { name: "Conversation turns" })
    .locator(":scope > li[data-turn]")
    .filter({ has: page.getByText("implement handoff, version 1", { exact: true }) });
  await card.getByRole("button", { name: "Show fields" }).click();

  // The card's fields are in no link or button, so the summary draws as a block of the renderer
  // with its references linked.
  await expectReadsReferences(
    card.locator('[data-handoff-field="summary"] .dispatch-markdown'),
    world,
    "link"
  );
});

test("issue-decision: a decision's paragraph draws every form of a stored reference through the renderer", async ({
  page,
}) => {
  const seeded = await seedReadingWorld();
  // A person's decision is the ask they answered, and the panel draws it by their answer, a
  // record of the document the ask's block stands in, so a `#id` in it is read against that
  // document: the document declares the question block itself beside the decision's own ask,
  // and the world is scoped to it.
  const choices = await createIssueArtifact(
    seeded.host.key,
    {
      content: `${readingQuestionBlock()}\n\n:::ask{#${DECISION_BLOCK} urgency="med"}\n${DECISION}\n:::\n`,
      name: "choices.md",
    },
    readingAgent
  );
  const world = await scopedTo(seeded, choices.artifact.id);
  const asked = await indexedBlockAsk(world.host.key, DECISION_BLOCK);
  await answerAsk(
    asked.id,
    { expected_edited_at: null, text: readingText(world) },
    { login: "bob" }
  );

  // A person's answer is their decision, so the panel lists it among the reader's own rather
  // than as an agent's waiting for review.
  await page.goto(`/issues/${world.host.key}/decisions`);
  const card = page.getByRole("region", { name: "Your decisions" }).locator("[data-decision]");
  await expect(card.getByRole("heading", { name: DECISION })).toBeVisible();

  // The card draws its heading through the renderer as well, as one line; its paragraphs are
  // its one block body, which is where the stored text is.
  await expectReadsReferences(card.locator(`div${RENDERED_BODY}`), world, "link");
});

// SLOW(>=2s): seeds the reading world and the reader's comment, then one cold load of the new
// issue page, whose names of what the comment cites come last (`useIssueTree.ts`).
test("node-row-words: a comment's row on the new issue page draws every form but a question id through the renderer", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  // A person's comment is a node of the outline; a thread no person wrote in is the history's
  // and draws no row (`agents-only.ts`), so the text is the reader's own.
  const comment = await createComment(world.host.key, { body: readingText(world) });
  // What each one-reference read asked for (`GET /api/v1/references?to=`): the row names a bare
  // key, an address and a short id from the page's one batched read, so none of them is asked
  // for on its own (`plain-text.tsx`'s `useReferenceNames`).
  const askedAlone: string[] = [];
  page.on("request", (request) => {
    const url = new URL(request.url());
    if (url.pathname === "/api/v1/references") askedAlone.push(url.searchParams.get("to") ?? "");
  });

  await withIssuePage(page.context(), READER);
  await openIssue(page, world.host.key);

  // The row's words are in no link or button, so the comment draws as a block of the renderer
  // with its references linked (`RowText.tsx`).
  await expectRowReadsForms(rowWords(nodeRow(page, comment.id)), world);
  const batched = formReadings(world)
    .filter(({ form }) => form === "bare-key" || form === "address" || form === "short-id")
    .flatMap(({ form, reference, written }) =>
      form === "short-id" ? [reference, written] : [reference]
    );
  expect(
    askedAlone.filter((to) => batched.includes(to)),
    "a reference the page's batched read names was read on its own"
  ).toEqual([]);
});

/**
 * Holds a row's words on the new issue page to what R8 says of each form they carry: a link that
 * reads as its node's name and opens that node, and nothing left as written. The row names a bare
 * key, an address and a short id with a link pass of its own, from the page's one batched read
 * of what its references point at (`row-references.ts`), and marks each with the reference as it
 * reads it (`data-row-reference`) rather than the renderer's `data-dispatch-ref`, which the
 * reading's own links - a dashboard URL, a party, a worded link - carry; so each form is found by
 * its link's name, whichever drew it. A short id is linked as its address writes it, which the
 * dashboard's own route resolves to its comment (`e2e/prefix-links.e2e.ts`).
 */
async function expectRowReadsForms(scope: Locator, world: ReadingWorld): Promise<void> {
  for (const { form, opens, reads, unread, written } of formReadings(world)) {
    const link = scope.getByRole("link", { exact: true, name: reads });
    await expect(link, `the ${form} reads as its node's name`).toHaveCount(1);
    const prefix = written.slice(written.lastIndexOf("/") + 1);
    await expect(link, `the ${form} opens its node`).toHaveAttribute(
      "href",
      form === "short-id" ? `/issues/${world.keyed.key}/comments/${prefix}` : opens
    );
    if (unread !== undefined) {
      await expect(scope, `the ${form} is not left as written`).not.toContainText(unread);
    }
  }
  await expect(scope).not.toContainText("dispatch://");
}

// SLOW(>=2s): seeds the reading world and a Legion daemon's message, then loads the new issue
// page at `/activity`, which opens Details' History and resolves each reference the turn holds.
test("issue-page-history-turn: an agents' thread turn in the new page's history draws a stored key, an address and a party through the renderer", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  // A message the project's Legion daemon writes with no target opens a thread the new page's
  // history holds whole (`agents-only.ts`).
  const turn = await createMessage(
    world.host.key,
    { body: readingText(world) },
    daemonOf(issueProject(world.host.key))
  );

  await withIssuePage(page.context(), READER);
  await page.goto(`/issues/${world.host.key}/activity`);

  await expectReadsReferences(historyItem(page, turn.id).locator(RENDERED_BODY), world, "link");
});

// SLOW(>=2s): seeds the reading world and a handoff, then loads the new issue page at `/activity`,
// which opens Details' History and reads the handoff's fields and each reference in them.
test("issue-page-history-handoff-body: a handoff's field of words in the new page's history draws every form but a question id through the renderer", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  await createHandoff(
    world.host.key,
    "implement",
    { phase: "implement", summary: readingText(world) },
    readingAgent.actor
  );

  await withIssuePage(page.context(), READER);
  await page.goto(`/issues/${world.host.key}/activity`);

  await expectReadsReferences(
    historyPanel(page).locator('[data-handoff-field="summary"] .dispatch-markdown'),
    world,
    "link"
  );
});

// SLOW(>=2s): seeds the reading world and asks the command bar a question, then one cold load of
// the run's own page, which reads the run once and then the names of what its answer cites.
test("run-page-answer: a command bar run's answer on its own page draws every form but a question id through the renderer", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  const run = await runAs(READER, "What does the release need?", issueProject(world.host.key));
  // The page's one read of the run (`GET /api/v1/references?to=<the run's address>`), answered as
  // the server answered it, with the frames a model's answer writes: the reading text as one
  // text frame, and the end.
  const reference = runReference(run.id);
  await page.route(
    (url) => url.pathname === "/api/v1/references" && url.searchParams.get("to") === reference,
    async (route) => {
      const response = await route.fetch();
      const read = (await response.json()) as GraphReferences;
      const frames: GraphFrame[] = [
        { data: { text: readingText(world) }, name: GOING_EVENT_TEXT, sequence: 1 },
        { data: { items: 0, words: 0 }, name: GOING_EVENT_END, sequence: 2 },
      ];
      await route.fulfill({ json: { ...read, node: { ...read.node, frames } }, response });
    }
  );

  await withIssuePage(page.context(), READER);
  await page.goto(buildRunPath(run.id));

  await expectReadsReferences(mainContent(page).locator(RENDERED_BODY), world, "link");
});
