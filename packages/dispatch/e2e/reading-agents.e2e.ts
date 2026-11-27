/**
 * The reading list's walk (`e2e/reading-rows.ts`) over the agents' world: the
 * two surfaces of the Agents page that show a stored message, the two parts of a session's live
 * view that show its own turn, the header's line quoting the session's answer to a switch, and
 * the viewer's own message the live view holds while Dispatch has not made it. Each row stores
 * `readingText` where its surface shows it - a message a person sends the session, the session's
 * reply to one, a turn of the replay the session answers a viewer with, or a draft the viewer
 * sends - reaches the surface as its row's `reach` says, and holds it to R6: each form the text
 * names a node by reads as that node's current name and opens it, the party as `@` and its
 * current name, and none is left as the characters written.
 *
 * All six hosts are a record a reader reads (`ReferenceHost` "record"): neither a turn of the
 * Agents page nor a part of the live view, its header's line among them, sits inside a link or a
 * button, so each reference is a link. A live turn is no record of the reference graph (the
 * reading list's reason for both of its rows), and neither is a message Dispatch has not made
 * yet, which costs each its `#id` links and nothing else: every other form draws there through
 * the same renderer as it does in a stored body. The session's answer to a switch is a message
 * on no issue, which belongs to no document, so its `#id` names nothing either.
 *
 * An Agents row shows a message sent to its session on an issue and one sent to it alone, on no
 * issue, which is each row's `direct-message` arm: such a message, and a reply to it, is a
 * record of no document, so its `#id` names nothing, and its driver holds the other five forms.
 *
 * The text names the session whose row and live view show it as its party, which is the one
 * session the reading world lists.
 */
import type { Message } from "../web/src/api/types";
import { agentRow, getSentMessages } from "./agents";
import {
  createAgentMessage,
  createMessage,
  getAgentConversation,
  publishAgentStreamFrame,
  replyToMessageDelivery,
} from "./api";
import { frontLiveView } from "./live-views";
import {
  expectReadsReferences,
  READING_SESSION_ID,
  readingText,
  seedReadingWorld,
} from "./reading-rows";
import { MESSAGE_REFUSED, refusePosts } from "./sends";
import { expect, type Locator, type Page, test } from "./test";

/** Opens the Agents page and the reading session's row. A row opens collapsed, and its
 *  conversation is on screen only once it is open. */
async function openReadingRow(page: Page): Promise<Locator> {
  await page.goto("/agents");
  const row = agentRow(page, READING_SESSION_ID);
  const toggle = row.locator("[data-agent-toggle]");
  await toggle.click();
  await expect(toggle).toHaveAttribute("aria-expanded", "true");
  return row;
}

/** A message a person sends the reading session on the host issue, which the session's row
 *  shows like any message sent to it. It is the issue's message, so a `#id` in it and in a
 *  reply to it is read against the issue's spec: a message sent to a session on no issue
 *  belongs to no document, and a `#id` there names nothing (`refs.ScopeOf`). */
function messageTheSession(issue: string, body: string) {
  return createMessage(issue, {
    body,
    delivery: "aside",
    target: `session:${READING_SESSION_ID}`,
  });
}
/** Sets what the reading session answers a viewer's replay request with: one finished turn of
 *  its own, made of `parts`. The harness runs Dispatch with NATS disabled, so the test hook
 *  stands in for the session; answering a replay makes it a responder, as a running session
 *  with a streaming plugin is. */
async function replayTurn(parts: readonly { text: string; type: "reasoning" | "text" }[]) {
  await publishAgentStreamFrame(
    READING_SESSION_ID,
    {
      frames: [
        {
          kind: "message",
          message: { at: 1_000, id: "a1000", parts, role: "assistant", streaming: false },
          seq: 1,
          v: 1,
        },
      ],
      session_id: READING_SESSION_ID,
      v: 1,
    },
    "replay"
  );
}

// SLOW(>=2s): the reading world's listener and API writes, a message and the session's reply to
// it, then the Agents page's first draw and the row's conversation read once it opens; measured
// at 4.9 s on one run in an issue pod on one worker.
test("agents-row-reply: a row's reply turn draws every form of a stored reference through the renderer", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  // The question is plain words, so the quote of it the reply turn draws above its own body
  // holds no reference to be read in place of the reply's.
  const asked = await messageTheSession(world.host.key, "What should I read next?");
  const reply = (await replyToMessageDelivery(
    asked.id,
    { attempt: 1, body: readingText(world) },
    { id: READING_SESSION_ID, kind: "session" }
  )) as Message;
  const row = await openReadingRow(page);
  // The reply's body alone: the turn also names its author through `AgentName` and quotes the
  // question in a preview, and neither is the stored reply.
  const body = row.locator(`li[data-reply][data-turn="message:${reply.id}"] div.dispatch-markdown`);
  await expectReadsReferences(body, world, "link");
});

// SLOW(>=2s): the reading world's listener and API writes and a message, then the Agents page's
// first draw and the row's conversation read once it opens; measured at 3.8 s on one run in an
// issue pod on one worker.
test("agents-row-message: a row's targeted message card draws every form of a stored reference through the renderer", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  const sent = await messageTheSession(world.host.key, readingText(world));
  const row = await openReadingRow(page);
  // The message's body alone: the card also names its author and the session it went to, each
  // through `AgentName`, and neither is the stored message. No reply is seeded, so the card
  // holds no reply's body either.
  const body = row.locator(`li[data-turn="message:${sent.id}"] div.dispatch-markdown`);
  await expectReadsReferences(body, world, "link");
});

/** A message a person sends the reading session alone, on no issue, which the session's row
 *  shows like any message sent to it: a record of no document (`direct-message`). */
function messageTheSessionAlone(body: string) {
  return createAgentMessage(READING_SESSION_ID, { body, delivery: "aside" });
}

// SLOW(>=2s): the reading world's listener and API writes, a message to the session and its
// reply, then the Agents page's first draw and the row's conversation read once it opens;
// measured at 6.5 s on one run in an issue pod on one worker.
test("agents-row-reply/direct-message: a row's reply to a message sent on no issue draws every form but a question id through the renderer", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  // The question is plain words, so the quote of it the reply turn draws above its own body
  // holds no reference to be read in place of the reply's.
  const asked = await messageTheSessionAlone("What should I read next?");
  const reply = (await replyToMessageDelivery(
    asked.id,
    { attempt: 1, body: readingText(world) },
    { id: READING_SESSION_ID, kind: "session" }
  )) as Message;
  const row = await openReadingRow(page);
  const body = row.locator(`li[data-reply][data-turn="message:${reply.id}"] div.dispatch-markdown`);
  await expectReadsReferences(body, world, "link");
});

// SLOW(>=2s): the reading world's listener and API writes and a message to the session, then
// the Agents page's first draw and the row's conversation read once it opens; measured at 4.3 s
// on one run in an issue pod on one worker.
test("agents-row-message/direct-message: a row's message sent on no issue draws every form but a question id through the renderer", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  const sent = await messageTheSessionAlone(readingText(world));
  const row = await openReadingRow(page);
  const body = row.locator(`li[data-turn="message:${sent.id}"] div.dispatch-markdown`);
  await expectReadsReferences(body, world, "link");
});

// SLOW(>=2s): the reading world's listener and API writes and the session's replay, then the
// live view's first draw, which waits on the live socket and the replay it asks for; measured at
// 3.1 s on one run in an issue pod on one worker.
test("agent-turn-text: a live turn's text part draws every form but a question id through the renderer", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  await replayTurn([{ text: readingText(world), type: "text" }]);
  await page.goto(`/agents/${READING_SESSION_ID}/live`);
  // The turn holds its one text part and nothing else: no model is reported and it is not
  // streaming, so neither line above the part is drawn.
  const turn = frontLiveView(page).getByTestId("agent-message-assistant");
  await expectReadsReferences(turn.locator("div.dispatch-markdown"), world, "link");
});

// SLOW(>=2s): the reading world's listener and API writes and the session's replay, then the
// live view's first draw, which waits on the live socket and the replay it asks for; measured at
// 2.7 s on one run in an issue pod on one worker.
test("agent-turn-reasoning: a live turn's reasoning draws every form but a question id through the renderer", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  // The reasoning sits above the turn's answer, as a session that thought publishes it.
  await replayTurn([
    { text: readingText(world), type: "reasoning" },
    { text: "Reading both now.", type: "text" },
  ]);
  await page.goto(`/agents/${READING_SESSION_ID}/live`);
  const reasoning = frontLiveView(page).getByTestId("agent-reasoning");
  await expectReadsReferences(reasoning, world, "link");
});

/** The model the reading session runs before a switch, and the one the header switches it to. */
const MODEL = "anthropic/model-a";
const SWITCHED_MODEL = "anthropic/model-b";

/** Sets what the reading session replays before any turn: the model and thinking level it runs,
 *  and the built-ins it runs typed input for, so the live view's header offers its pickers. The
 *  frames are spelt as the wire carries them, as `e2e/agent-view-settings.e2e.ts` spells them. */
async function replaySettings() {
  await publishAgentStreamFrame(
    READING_SESSION_ID,
    {
      frames: [
        {
          kind: "settings",
          model: MODEL,
          models: [MODEL, SWITCHED_MODEL],
          reported: true,
          seq: 1,
          thinking: { level: "auto", levels: ["auto", "high"] },
          v: 1,
        },
        {
          commands: [
            { description: "Switch the session's model", name: "model", source: "builtin" },
            { description: "Set the thinking level", name: "effort", source: "builtin" },
          ],
          kind: "commands",
          seq: 2,
          v: 1,
        },
      ],
      session_id: READING_SESSION_ID,
      v: 1,
    },
    "replay"
  );
}

/** The stored id of the message `body` the page sent the reading session, and the delivery
 *  attempt the fake Envoy's frame for it carries. Dispatch stores the message, records the
 *  attempt, and sends the frame once that attempt is committed, so a reply naming the frame's
 *  attempt finds it, as a running session's reply to the frame it got does. */
async function sentDelivery(body: string): Promise<{ attempt: number; id: string }> {
  let found: { attempt: number; id: string } | undefined;
  await expect
    .poll(async () => {
      const frame = (await getSentMessages()).find((sent) => sent.message === body);
      const stored = (await getAgentConversation(READING_SESSION_ID)).find(
        (row) => row.message.body === body
      );
      if (frame === undefined || stored === undefined) return false;
      const { delivery } = JSON.parse(String(frame.payload)) as { delivery: { attempt: number } };
      found = { attempt: delivery.attempt, id: stored.message.id };
      return true;
    })
    .toBe(true);
  if (found === undefined) throw new Error(`setup: the page sent the session no ${body}`);
  return found;
}

// SLOW(>=2s): the reading world's listener and API writes and the session's replay, then the
// live view's first draw, the pick's send through the fake listener and Dispatch's store, and the
// session's reply, which the header reads on the thread's next fetch; measured at 7.6 s on one
// run in an issue pod on one worker.
test("session-switch-answer: the header's line after a model switch draws every form but a question id of the session's answer through the renderer", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  await replaySettings();
  await page.goto(`/agents/${READING_SESSION_ID}/live`);
  const model = page.getByRole("combobox", { name: "Model" });
  await expect(model).toHaveValue(MODEL);

  // A pick is the viewer's own Send of `/model`, and the session answers it with a message of
  // its conversation, which the header's line quotes.
  await model.selectOption(SWITCHED_MODEL);
  const status = page.getByRole("status").and(page.getByTestId("agent-switch-status"));
  await expect(status).toHaveText(`Switching model to ${SWITCHED_MODEL}…`);
  const delivery = await sentDelivery(`/model ${SWITCHED_MODEL}`);
  await replyToMessageDelivery(
    delivery.id,
    { attempt: delivery.attempt, body: readingText(world) },
    { id: READING_SESSION_ID, kind: "session" }
  );
  await expect(status).toContainText("The session answered:");

  // The line is a `<p role="status">`, in no link or button, so the answer draws inline through
  // the renderer with its references linked.
  await expectReadsReferences(status.locator(".dispatch-markdown"), world, "link");
});

// SLOW(>=2s): the reading world's listener and API writes and the session's replay, then the
// live view's first draw and a send held out until the row refuses it; measured at 8.7 s on one
// run in an issue pod on one worker.
test("agent-held-send: the live view's held message draws every form but a question id of the viewer's draft through the renderer", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  await replayTurn([{ text: "Ready for the next step.", type: "text" }]);
  // Every message the page posts to the session is held out, so Dispatch stores none of it and
  // the live view holds the draft as the viewer's own message, unsent.
  const refuse = await refusePosts(
    page,
    `**/api/v1/agents/${READING_SESSION_ID}/messages`,
    MESSAGE_REFUSED
  );
  await page.goto(`/agents/${READING_SESSION_ID}/live`);
  const view = frontLiveView(page);
  await expect(view.getByTestId("agent-message-assistant")).toContainText(
    "Ready for the next step."
  );

  await view.getByTestId("agent-composer").locator("textarea").fill(readingText(world));
  await view.getByRole("button", { name: "Send" }).click();
  const held = view.getByTestId("agent-send-held");
  await expect(held.getByRole("status")).toHaveText("Sending…");
  // The held turn is in no link or button, so the draft draws through the renderer, as the
  // message it becomes, with its references linked.
  await expectReadsReferences(held.locator(".dispatch-markdown"), world, "link");

  // Refused, the draft stays where it was, still unsent, for the viewer to send again.
  refuse();
  await expect(held.getByTestId("agent-send-error")).toBeVisible();
  await expectReadsReferences(held.locator(".dispatch-markdown"), world, "link");
});
