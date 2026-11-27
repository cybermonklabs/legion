/**
 * The reading list's walk (`e2e/reading-rows.ts`) over the four places a
 * broadcast shows text: a recipient's reply and the broadcast's own body on the broadcast's page,
 * the one-line body of its row on the Broadcasts list, and the row its send leaves on the Agents
 * page. The reply is a message, a record of this Dispatch, though one on no issue, so it
 * belongs to no document; the other three are the broadcast, which is no record of the
 * reference graph (the reading list's `BROADCAST_REASON`). So a `#id` in any of the four names
 * nothing, as each row of the list says (`cannotCarry`). That changes nothing about the five
 * other forms a text names a node by, which each of the four draws through the one renderer
 * (R6): as its node's current name, the party as `@` and the session's name, and none left as
 * the characters written.
 *
 * The host decides whether those are links. The broadcast's page draws the reply and the body
 * in its own article and header, and a send row draws its text beside its status and its
 * buttons, never inside them, so all three are links there. A Broadcasts row's body is inside the
 * row's link to its broadcast (`ReferenceHostIs host="control"`, `BroadcastsPage.tsx`), so there
 * each draws as its name and links nothing.
 *
 * Every broadcast goes to the session the text names as its party, the one session the world
 * lists: it is the recipient alice selects and the session whose reply the broadcast's page shows.
 */
import { agentRow, BROADCAST_REFUSED, refuseBroadcasts } from "./agents";
import { createBroadcast, replyToMessageDelivery } from "./api";
import {
  asReadingSession,
  expectReadsReferences,
  READING_SESSION_TITLE,
  readingSessionId,
  readingText,
  seedReadingWorld,
} from "./reading-rows";
import { RENDERED_BODY } from "./reference-surfaces";
import { expect, test } from "./test";

/** A broadcast alice sends to the reading session alone, as the composer would send it. */
async function broadcastToReadingSession(body: string) {
  const sent = await createBroadcast({
    body,
    delivery: "steer",
    session_ids: [readingSessionId()],
  });
  expect(sent.excluded).toEqual([]);
  return sent;
}

// SLOW(>=2s): the reading world, a broadcast sent and answered, and the broadcast's page drawn
// with each reference's title read.
test("broadcast-recipient-reply: a recipient's reply on the broadcast's page draws every form but a question id through the renderer", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  // The broadcast names nothing, so the one text on the recipient's row that names anything is
  // the reply the row reads.
  const sent = await broadcastToReadingSession("Report status.");
  const [recipient] = sent.recipients ?? [];
  if (recipient === undefined) throw new Error("setup: the broadcast reached no recipient");
  await replyToMessageDelivery(
    recipient.message.id,
    { attempt: 1, body: readingText(world) },
    asReadingSession.actor
  );

  await page.goto(`/agents/broadcasts/${sent.id}`);
  const row = page
    .getByRole("region", { name: "Broadcast" })
    .getByRole("article", { name: READING_SESSION_TITLE });
  await expectReadsReferences(row.locator(RENDERED_BODY), world, "link");
});

// SLOW(>=2s): the reading world, a broadcast sent, and the broadcast's page drawn with each
// reference's title read.
test("broadcast-sent-body: the broadcast's body under its page's heading draws every form but a question id through the renderer", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  const sent = await broadcastToReadingSession(readingText(world));

  await page.goto(`/agents/broadcasts/${sent.id}`);
  const header = page.getByRole("region", { name: "Broadcast" }).locator("header");
  await expectReadsReferences(header.locator(RENDERED_BODY), world, "link");
});

// SLOW(>=2s): the reading world, the Agents page drawn, a recipient ticked, the text typed and
// sent, and the refused send's row drawn.
test("broadcast-send-row: a send's row on the Agents page draws every form but a question id through the renderer", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  // A send Dispatch refuses keeps its row with its text until Retry or Restore draft takes it; a
  // send that succeeds opens its broadcast and takes the row with the page.
  await refuseBroadcasts(page);
  await page.goto("/agents");
  await agentRow(page, readingSessionId())
    .getByRole("checkbox", { name: `Select ${READING_SESSION_TITLE} for broadcast` })
    .check();
  const composer = page.getByRole("region", { name: "Broadcast" });
  await composer.getByRole("textbox", { name: "Broadcast message" }).fill(readingText(world));
  await composer.getByRole("button", { name: "Send to 1" }).click();

  const row = page.getByRole("region", { name: "Sends" }).getByRole("listitem");
  await expect(row.getByRole("status")).toHaveText(
    `Could not send to 1 agent: ${BROADCAST_REFUSED}`
  );
  await expectReadsReferences(row.locator("[data-markdown-preview]"), world, "link");
});

test("broadcasts-list-body: a Broadcasts row's one-line body draws every form but a question id as a name inside the row's link", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  const sent = await broadcastToReadingSession(readingText(world));

  await page.goto("/agents/broadcasts");
  const link = page
    .getByRole("region", { name: "Broadcasts" })
    .locator(`a[href="/agents/broadcasts/${sent.id}"]`);
  await expectReadsReferences(link.locator("[data-markdown-preview]"), world, "inert");
});
