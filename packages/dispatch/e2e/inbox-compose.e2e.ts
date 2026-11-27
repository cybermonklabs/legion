import { setTimeout as sleep } from "node:timers/promises";

import type { BroadcastCreated, Comment, Message, MessageRead } from "../web/src/api/types";
import {
  agentRow,
  type FakeSession,
  isBroadcastInput,
  setLiveSessions,
  setSessionRecord,
  setSessionSendState,
  setSessionState,
} from "./agents";
import {
  createIssue,
  createProject,
  getBroadcast,
  queuedEnvelopesOf,
  replyToMessageDelivery,
} from "./api";
import { controlRow, listTrigger } from "./filters";
import { describeFocus, frameAndTask, holdsFocus } from "./focus";
import { inboxList, inboxPane } from "./inbox-pane";
import { agentRowId, seedAgentThread } from "./inbox-threads";
import { keyedWrite } from "./keyed-writes";
import { sessionIds } from "./session-ids";
import { INBOX_TOP, readBar, resetScroll, topOf } from "./table-controls";
import { expect, type Locator, type Page, type Request, test } from "./test";
import { asUser } from "./users";

/**
 * The Inbox's compose dialog: a message to one or more agents, which the control row's **New
 * message** and `n` open, sent through the Agents page's own composers and requests, with each
 * recipient's state before Send and each one's outcome after it, and the focus rules of the
 * dialog's own hand-off for its held Send and its Done.
 *
 * Everything here is made up: this repository is public, so nothing names a real person, host or
 * session.
 */

/** The margin an absence - a request the page must not make - is read after. */
const ABSENCE_MARGIN_MS = 150;

/** How long a read of the agent registry may take to show a change of the fake listener's:
 *  Dispatch's copy of the list is up to 5 s old. */
const REGISTRY_MS = 30_000;

/** **New message** in the control bar of `within` - the page, or the drawer: the bar's main
 *  action, found by its role and its name, which it keeps where it shows its icon alone. */
function newMessage(within: Locator | Page): Locator {
  return controlRow(within).getByRole("button", { exact: true, name: "New message" });
}

/** Holds that **New message** is the last control of the control bar in `within`, as the bar's
 *  main action stands at its end (R6, R8), each control read by the name a screen reader hears
 *  (`readBar`). */
async function expectNewMessageLast(within: Locator | Page): Promise<void> {
  const names = (await readBar(controlRow(within))).controls.map((one) => one.name);
  expect(names.at(-1), `the control bar's controls: ${names.join(", ")}`).toBe("New message");
}

/** A session that takes every delivery mode, so a plain message to it is a Send. */
function session(key: keyof typeof sessionIds, title: string): FakeSession {
  return {
    capabilities: ["aside", "btw", "steer"],
    dir: `/workspaces/${key}`,
    machine_id: "build-host",
    roles: [],
    session_id: sessionIds[key],
    title,
  };
}

const builder = session("planner", "Builder");
const checker = session("tester", "Checker");
const scribe = session("reviewer", "Scribe");

/** The compose dialog. */
function composeDialog(page: Page): Locator {
  return page.getByRole("dialog", { name: "New message" });
}

/** The state the dialog draws for `sessionId` before Send. */
function stateOf(page: Page, sessionId: string): Locator {
  return composeDialog(page).locator(`[data-compose-state="${sessionId}"]`);
}

/** The outcome the dialog draws for `sessionId` after Send. */
function outcomeOf(page: Page, sessionId: string): Locator {
  return composeDialog(page).locator(`[data-compose-outcome="${sessionId}"]`);
}

/** The Recipients list, once its trigger opened it. */
function recipientsList(page: Page): Locator {
  return page.getByRole("listbox", { name: "Recipients options" });
}

/** Opens the Recipients list. */
async function openRecipients(page: Page): Promise<Locator> {
  await composeDialog(page)
    .getByRole("button", { name: /^Recipients/ })
    .click();
  const list = recipientsList(page);
  await expect(list).toBeVisible();
  return list;
}

/** Closes the Recipients list with Escape, which is the list's own. */
async function closeRecipients(page: Page): Promise<void> {
  await page.keyboard.press("Escape");
  await expect(recipientsList(page)).toHaveCount(0);
  await expect(composeDialog(page)).toBeVisible();
}

/** Ticks each of `titles` in the Recipients list, in that order, once the registry lists it. */
async function pickRecipients(page: Page, titles: readonly string[]): Promise<void> {
  const list = await openRecipients(page);
  for (const title of titles) {
    const option = list.getByRole("option", { exact: true, name: title });
    await expect(option).toBeVisible({ timeout: REGISTRY_MS });
    await option.click();
  }
  await closeRecipients(page);
}

/** Opens and closes the Recipients list until `check` holds: the list reads the registry again as
 *  it opens. */
async function untilRegistryReads(page: Page, check: () => Promise<void>): Promise<void> {
  await expect(async () => {
    await openRecipients(page);
    await closeRecipients(page);
    await check();
  }).toPass({ timeout: REGISTRY_MS });
}

/** Takes the focus off whatever holds it, so the next key is pressed with nothing focused. */
function blurFocus(page: Page): Promise<void> {
  return page.evaluate(() => {
    if (document.activeElement instanceof HTMLElement) document.activeElement.blur();
  });
}

/** Whether a body the page posted is a comment's input. */
function isCommentInput(body: unknown): body is { body: string } {
  return (
    typeof body === "object" && body !== null && "body" in body && typeof body.body === "string"
  );
}

/** Every write the page sends to an agent from here on: each message to one session and each
 *  broadcast, by its method and path. */
function watchAgentWrites(page: Page): Request[] {
  const writes: Request[] = [];
  page.on("request", (request) => {
    const path = new URL(request.url()).pathname;
    if (
      request.method() === "POST" &&
      (path === "/api/v1/broadcasts" || /^\/api\/v1\/agents\/[^/]+\/messages$/.test(path))
    ) {
      writes.push(request);
    }
  });
  return writes;
}

/** What `writes` sent, for a failure: each request's path and body. */
function described(writes: readonly Request[]): string {
  return writes
    .map((request) => `${new URL(request.url()).pathname} ${request.postData() ?? ""}`)
    .join("\n");
}

/** The answer to the page's send to `path`, a POST, once it lands. Its name marks each call as a
 *  send, so a send to a session's messages reads as the send it is beside its path
 *  (`names_a_send` in `acceptance/one-inbox-callers.sh`). */
function sendAnswer(page: Page, path: string) {
  return page.waitForResponse(
    (response) =>
      response.request().method() === "POST" && new URL(response.url()).pathname === path
  );
}

/** The page's next read of one message, `GET /api/v1/messages/{id}`, once it lands. It is set
 *  before Send: the dialog reads the message as soon as the answer lands, so a wait set after the
 *  answer could miss that one read. */
function nextMessageRead(page: Page) {
  return page.waitForResponse(
    (response) =>
      response.request().method() === "GET" &&
      /^\/api\/v1\/messages\/[^/]+$/.test(new URL(response.url()).pathname)
  );
}

/** A broadcast's copies, once each has an attempt that took. */
async function deliveredCopies(id: string): Promise<BroadcastCreated["recipients"]> {
  let recipients: BroadcastCreated["recipients"] = [];
  await expect
    .poll(
      async () => {
        recipients = (await getBroadcast(id)).recipients ?? [];
        return recipients.map((recipient) => recipient.message.deliveries?.at(-1)?.state);
      },
      { timeout: 15_000 }
    )
    .toEqual(["sent", "sent", "sent"]);
  return recipients;
}

/** The words of `sessionId`'s outcome in the broadcast `read`: left out with the reason it gives,
 *  or what the newest attempt of its copy says, none yet being a copy on its way. */
function outcomeWordsIn(read: BroadcastCreated, sessionId: string): string {
  const left = (read.excluded ?? []).find((item) => item.session_id === sessionId);
  if (left !== undefined) return `Left out: ${left.reason}`;
  const copy = (read.recipients ?? []).find((recipient) => recipient.session_id === sessionId);
  const attempt = copy?.message.deliveries?.at(-1);
  switch (attempt?.state) {
    case undefined:
    case "pending":
      return "On its way";
    case "queued":
      return "Stored for later: the session is stopped; it gets the message if it runs again within 72 hours";
    case "failed":
      return `Failed: ${attempt.error ?? "no reason was recorded"}`;
    default:
      return "Sent";
  }
}

// SLOW(>=2s): the Agents page's two sends and the dialog's, three sessions' replies, and the
// control row read at two window sizes.
test("the n key opens a message to one or more agents from the Inbox", async ({ browser }) => {
  // Two pages' sends, three replies and two window sizes: past the default minute on a slow
  // runner.
  test.setTimeout(150_000);
  await setLiveSessions([builder, checker, scribe]);
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.setViewportSize({ height: 900, width: 1440 });

    // The Agents page's own message and broadcast for the same input, which the dialog's are read
    // against field for field.
    const single = "Rebuild the cache, please.";
    const many = "Release at noon; hold your merges.";
    await page.goto("/agents");
    const row = agentRow(page, builder.session_id);
    await row.getByRole("button", { exact: true, name: builder.title }).click();
    const rowBox = row.getByRole("textbox", { name: "Comment" });
    await rowBox.fill(single);
    const agentsSingle = page.waitForRequest(
      (request) =>
        request.method() === "POST" &&
        new URL(request.url()).pathname === `/api/v1/agents/${builder.session_id}/messages`
    );
    await rowBox.press("Control+Enter");
    const agentsSingleInput = keyedWrite(await agentsSingle).input;
    for (const each of [builder, checker, scribe]) {
      await page.getByRole("checkbox", { name: `Select ${each.title} for broadcast` }).check();
    }
    const composer = page.getByRole("region", { name: "Broadcast" });
    await composer.getByRole("textbox", { name: "Broadcast message" }).fill(many);
    const agentsMany = page.waitForRequest(
      (request) =>
        request.method() === "POST" && new URL(request.url()).pathname === "/api/v1/broadcasts"
    );
    await composer.getByRole("button", { name: "Send to 3" }).click();
    const agentsManyInput = keyedWrite(await agentsMany).input;

    // The Inbox, with nothing focused and no text field active: `n` opens the dialog.
    await page.goto("/");
    await expect(newMessage(page)).toBeVisible();
    await blurFocus(page);
    await page.keyboard.press("n");
    const dialog = composeDialog(page);
    await expect(dialog).toBeVisible();

    // The registry the page reads holds the three sessions: the list offers each, and no other.
    // Its trigger ends in the shared chevron every list trigger shows.
    const trigger = dialog.getByRole("button", { name: /^Recipients/ });
    await expect(trigger.locator("svg")).toHaveCount(1);
    await expect(trigger.locator("svg")).toBeVisible();
    const offered = await openRecipients(page);
    await expect(offered.getByRole("option"), "the sessions the list offers").toHaveCount(3, {
      timeout: REGISTRY_MS,
    });
    for (const each of [builder, checker, scribe]) {
      await expect(offered.getByRole("option", { exact: true, name: each.title })).toHaveCount(1);
    }

    // One recipient, picked through the list's text box: one message, as the Agents page sends
    // it.
    const search = page.getByRole("combobox", { name: "Search agents" });
    await search.fill(builder.title);
    await expect(
      recipientsList(page).getByRole("option", { exact: true, name: builder.title })
    ).toBeVisible({ timeout: REGISTRY_MS });
    await search.press("Enter");
    await closeRecipients(page);
    await dialog.getByRole("textbox", { name: "Comment" }).fill(single);
    const writes = watchAgentWrites(page);
    const singleAnswer = sendAnswer(page, `/api/v1/agents/${builder.session_id}/messages`);
    await dialog.getByRole("button", { name: "Send" }).click();
    await singleAnswer;
    await expect(dialog.getByRole("region", { name: "Where your message went" })).toBeVisible();
    expect(writes.length, `the writes the dialog sent:\n${described(writes)}`).toBe(1);
    // `watchAgentWrites` keeps only POST requests: the one write is the send to that session.
    expect(new URL(writes[0]?.url() ?? "").pathname).toBe(
      `/api/v1/agents/${builder.session_id}/messages`
    );
    expect(writes.map((request) => keyedWrite(request).input)).toEqual([agentsSingleInput]);

    // Done, and the dialog again from the control: three recipients, one broadcast.
    await dialog.getByRole("button", { name: "Done" }).click();
    await expect(dialog).toHaveCount(0);
    await newMessage(page).click();
    await pickRecipients(page, [builder.title, checker.title, scribe.title]);
    await dialog.getByRole("textbox", { name: "Broadcast message" }).fill(many);
    writes.length = 0;
    await dialog.getByRole("button", { name: "Send to 3" }).click();
    // The send settles one way or the other - its outcomes drawn, or its refusal beside Send -
    // and the writes are read there, so a send other than the one broadcast fails here, naming
    // each request it made.
    const outcomes = dialog.getByRole("region", { name: "Where your message went" });
    await expect(outcomes.or(dialog.getByRole("alert"))).toBeVisible();
    expect(
      writes.map((request) => new URL(request.url()).pathname),
      `the writes the dialog sent for three recipients:\n${described(writes)}`
    ).toEqual(["/api/v1/broadcasts"]);
    await expect(outcomes).toBeVisible();
    const [broadcastWrite] = writes;
    if (broadcastWrite === undefined) throw new Error("the dialog sent no broadcast");
    expect(keyedWrite(broadcastWrite).input).toEqual(agentsManyInput);
    expect(keyedWrite(broadcastWrite, isBroadcastInput).input.session_ids).toEqual([
      builder.session_id,
      checker.session_id,
      scribe.session_id,
    ]);
    const created = (await (await broadcastWrite.response())?.json()) as BroadcastCreated;
    await dialog.getByRole("button", { name: "Done" }).click();
    await expect(dialog).toHaveCount(0);

    // Each session replies to its own copy, and with no reload the Inbox lists three Agent rows,
    // one for each, each holding that session's reply.
    const copies = await deliveredCopies(created.id);
    const documentMarker = `compose-${created.id}`;
    await page.evaluate((marker) => {
      Object.assign(window, { __noReload: marker });
    }, documentMarker);
    // A click on a row writes `?item=` into the address, a same-document navigation; only a
    // reload loads a new document, so the loads are what the no-reload read counts.
    const documentLoads: string[] = [];
    page.on("load", (loaded) => documentLoads.push(loaded.url()));
    const replies = new Map<string, string>();
    for (const copy of copies ?? []) {
      const reply = `${copy.session_id} has it in hand.`;
      replies.set(copy.message.id, reply);
      await replyToMessageDelivery(
        copy.message.id,
        { attempt: 1, body: reply },
        { id: copy.session_id, kind: "session" }
      );
    }
    for (const messageId of replies.keys()) {
      await expect(
        inboxList(page).locator(`[data-inbox-row="${agentRowId(messageId)}"]`)
      ).toBeVisible();
    }
    await expect(
      inboxList(page).locator(`[data-inbox-row^="${agentRowId("")}"]`),
      "the Inbox's Agent rows"
    ).toHaveCount(3);
    // Each row holds its session's reply: the reading pane draws the row's thread.
    for (const [messageId, reply] of replies) {
      const agentRowOf = inboxList(page).locator(`[data-inbox-row="${agentRowId(messageId)}"]`);
      await agentRowOf.locator("time").first().click();
      await expect(agentRowOf).toHaveAttribute("data-inbox-selected", "true");
      await expect(inboxPane(page)).toContainText(reply);
    }
    expect(
      await page.evaluate(() => Reflect.get(window, "__noReload")),
      "the Inbox document survives all three replies"
    ).toBe(documentMarker);
    expect(documentLoads, "documents loaded during the three replies").toEqual([]);

    // `n` in a text field is the letter.
    const box = page.getByRole("searchbox", { name: "Search asks" });
    await box.click();
    await page.keyboard.press("n");
    await expect(box).toHaveValue("n");
    await expect(dialog).toHaveCount(0);
    await box.fill("");

    // The help lists `n`, and the palette offers it.
    await blurFocus(page);
    await page.keyboard.press("?");
    const help = page.getByRole("dialog", { name: "Keyboard shortcuts" });
    await expect(
      help
        .getByRole("region", { name: "Inbox" })
        .getByRole("listitem")
        .filter({ hasText: "Write a message to one or more agents" })
        .locator("kbd")
    ).toHaveText(["n"]);
    await page.keyboard.press("Escape");
    await expect(help).toHaveCount(0);
    await blurFocus(page);
    await page.keyboard.press("Control+k");
    const actions = page
      .getByRole("dialog", { name: "Search" })
      .getByRole("group", { name: "Actions" });
    const composeAction = actions.getByRole("option", {
      name: "Write a message to one or more agents",
    });
    await expect(composeAction).toHaveCount(1);
    await expect(composeAction.locator("kbd"), "the palette's compose shortcut").toHaveText(["n"]);
    await page.keyboard.press("Escape");

    // New message, the control bar's last control, adds no height at either window size.
    const firstRow = page.locator("[data-inbox-row]").first();
    for (const size of [
      { height: 900, width: 1440 },
      { height: 800, width: 1280 },
    ]) {
      await page.setViewportSize(size);
      await page.goto("/");
      await expect(firstRow).toBeVisible();
      await expectNewMessageLast(page);
      await resetScroll(page, firstRow);
      const top = await topOf(firstRow);
      expect(
        top,
        `${size.width}x${size.height}: the first item starts ${top}px down`
      ).toBeLessThanOrEqual(INBOX_TOP);
    }
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): three sends, the registry read again for a session that ends, and a picked
// registry of 101.
test("the compose dialog shows where a message to each agent went", async ({ browser }) => {
  // Up to four reads of the registry after the listener changes, each up to Dispatch's 5 s copy
  // and a reopen of the list, and 101 picks.
  test.setTimeout(240_000);
  const running = builder;
  const stalled = checker;
  const unknown = scribe;
  await setLiveSessions([running, stalled, unknown]);
  // Past its heartbeats, and its process gone by the send: the listener stores the message for it.
  await setSessionState(stalled.session_id, "unresponsive");
  await setSessionSendState(stalled.session_id, "stopped");
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.setViewportSize({ height: 900, width: 1440 });
    await page.goto("/");
    const dialog = composeDialog(page);
    await newMessage(page).click();
    await pickRecipients(page, [running.title, stalled.title, unknown.title]);

    // The third session ends after it was picked: the listener no longer knows it, and the
    // registry's newest answer does not list it. The page cannot tell it from a stopped session
    // the listener still knows, so its words name both outcomes.
    await setSessionRecord(unknown.session_id, "absent");
    await untilRegistryReads(page, async () => {
      await expect(stateOf(page, unknown.session_id)).toContainText(
        "Not listed: the message is stored for it if the Envoy listener knows it; otherwise the send leaves it out",
        { timeout: 1_000 }
      );
    });
    await expect(stateOf(page, running.session_id)).toHaveText(/·\s*Running$/);
    await expect(stateOf(page, stalled.session_id)).toContainText("Unresponsive");
    await expect(stateOf(page, stalled.session_id)).toContainText(
      "It has not ended: messages sent to it are stored until it answers"
    );
    // An unresponsive session has not ended, so its line never says that it is stopped or not
    // running: those are the words of a queued attempt and of the registry's own state mark.
    const stalledLine = (await stateOf(page, stalled.session_id).textContent()) ?? "";
    expect(stalledLine, "the unresponsive recipient's line").not.toContain("stopped");
    expect(stalledLine, "the unresponsive recipient's line").not.toContain("Not running");
    // Three recipients: the broadcast composer's fields, and no Issue picker.
    await expect(listTrigger(dialog, "Delivery mode")).toBeVisible();
    const broadcastBox = dialog.getByRole("textbox", { name: "Broadcast message" });
    await expect(broadcastBox).toBeVisible();
    await expect(dialog.getByRole("button", { name: /^Choose issue/ })).toHaveCount(0);

    // The message stays from three to one and back.
    const body = "Freeze at five, please.";
    await broadcastBox.fill(body);
    await dialog.getByTitle(`Remove ${running.title} from this broadcast`).click();
    await dialog.getByTitle(`Remove ${stalled.title} from this broadcast`).click();
    const single = dialog.getByRole("textbox", { name: "Comment" });
    await expect(single, "the message after two recipients were taken off").toHaveValue(body);
    const picker = dialog.getByRole("button", { name: /^Choose issue/ });
    await expect(picker).toBeVisible();
    expect((await picker.boundingBox())?.y ?? 0).toBeLessThan((await single.boundingBox())?.y ?? 0);
    await pickRecipients(page, [running.title, stalled.title]);
    await expect(broadcastBox, "the message after they were added back").toHaveValue(body);

    // Send, with the broadcast's own read held until the answer's outcomes are read.
    const { promise: readHeld, resolve: releaseRead } = Promise.withResolvers<void>();
    await page.route(/\/api\/v1\/broadcasts\/[^/]+$/, async (route) => {
      await readHeld;
      await route.continue();
    });
    const answer = sendAnswer(page, "/api/v1/broadcasts");
    await dialog.getByRole("button", { name: "Send to 3" }).click();
    const created = (await (await answer).json()) as BroadcastCreated;
    await expect(dialog).toBeVisible();
    // Each outcome as the answer gives it, while the broadcast's read is still held.
    expect(outcomeWordsIn(created, unknown.session_id)).toBe("Left out: no live session");
    for (const each of [running, stalled, unknown]) {
      await expect(outcomeOf(page, each.session_id)).toContainText(
        outcomeWordsIn(created, each.session_id)
      );
    }
    const broadcastRead = page.waitForResponse(
      (response) => new URL(response.url()).pathname === `/api/v1/broadcasts/${created.id}`
    );
    releaseRead();
    await broadcastRead;
    await page.unroute(/\/api\/v1\/broadcasts\/[^/]+$/);
    await expect(outcomeOf(page, running.session_id)).toContainText("Sent");
    await expect(outcomeOf(page, stalled.session_id)).toContainText(
      "Stored for later: the session is stopped; it gets the message if it runs again within 72 hours"
    );
    await expect(outcomeOf(page, unknown.session_id)).toContainText("Left out: no live session");
    await dialog.getByRole("button", { name: "Done" }).click();
    await expect(dialog).toHaveCount(0);

    /** Picks the unknown session alone: listed again to be picked (a reseed, since the listener
     *  lists a session whose record came back only once its own route is read), then ended
     *  again, which the dialog reads as a session it does not list. A send to one leaves nobody
     *  out, so its words say only that the message is stored for it. */
    const pickUnknownAlone = async () => {
      await setLiveSessions([running, stalled, unknown]);
      await newMessage(page).click();
      await pickRecipients(page, [unknown.title]);
      await setSessionRecord(unknown.session_id, "absent");
      await untilRegistryReads(page, async () => {
        await expect(stateOf(page, unknown.session_id)).toContainText(
          "Not listed: the message is stored for it",
          { timeout: 1_000 }
        );
      });
      expect(await stateOf(page, unknown.session_id).textContent()).not.toContain("leaves it out");
      await dialog
        .getByRole("textbox", { name: "Comment" })
        .fill("Pick this up when you are back.");
    };

    // To the unknown session alone: the server records the publish on its subject (the test
    // hooks' recorder, as a bus would take it), so the attempt is queued and never left out.
    await pickUnknownAlone();
    const queuedAnswer = sendAnswer(page, `/api/v1/agents/${unknown.session_id}/messages`);
    const queuedReadAnswer = nextMessageRead(page);
    await dialog.getByRole("button", { name: "Send" }).click();
    const queuedMessage = (await (await queuedAnswer).json()) as Message;
    const queuedReadResponse = await queuedReadAnswer;
    expect(new URL(queuedReadResponse.url()).pathname).toBe(`/api/v1/messages/${queuedMessage.id}`);
    const queuedRead = (await queuedReadResponse.json()) as MessageRead;
    expect(queuedRead.message.deliveries?.at(-1)?.state).toBe("queued");
    const envelopes = await queuedEnvelopesOf(queuedMessage.id);
    expect(envelopes.map((envelope) => envelope.subject)).toEqual([
      `notifications.agent.${unknown.session_id}`,
    ]);
    const queuedOutcome = outcomeOf(page, unknown.session_id);
    await expect(queuedOutcome).toContainText(
      "Stored for later: the session is stopped; it gets the message if it runs again within 72 hours"
    );
    await expect(queuedOutcome).toContainText(unknown.session_id);
    expect(await queuedOutcome.textContent()).not.toContain("Left out");
    await dialog.getByRole("button", { name: "Done" }).click();
    await expect(dialog).toHaveCount(0);

    // Again, with the page's read of the message answering what Dispatch records where it has no
    // bus or the publish fails: the attempt failed with the listener's words.
    await pickUnknownAlone();
    const failure = `no live session ${unknown.session_id}`;
    await page.route(/\/api\/v1\/messages\/[^/]+$/, async (route) => {
      if (route.request().method() !== "GET") return route.continue();
      const response = await route.fetch();
      const read = (await response.json()) as MessageRead;
      const deliveries = (read.message.deliveries ?? []).map((delivery) => ({
        ...delivery,
        error: failure,
        state: "failed",
      }));
      await route.fulfill({
        json: { ...read, message: { ...read.message, deliveries } },
        response,
      });
    });
    await dialog.getByRole("button", { name: "Send" }).click();
    const failedOutcome = outcomeOf(page, unknown.session_id);
    await expect(failedOutcome).toContainText(`Failed: ${failure}`);
    const failedLabel = (await failedOutcome.textContent()) ?? "";
    expect(failedLabel, "the failed send's outcome").toContain(unknown.session_id);
    expect(failedLabel, "the failed send's outcome").not.toContain("Stored");
    expect(failedLabel, "the failed send's outcome").not.toContain("Left out");
    await page.unroute(/\/api\/v1\/messages\/[^/]+$/);
    await dialog.getByRole("button", { name: "Done" }).click();
    await expect(dialog).toHaveCount(0);

    // Last, 101 sessions from a registry of 101: Send refused in the dialog, and no request.
    const crowd = Array.from({ length: 101 }, (_, index): FakeSession => {
      const number = String(index + 1).padStart(3, "0");
      return {
        capabilities: ["aside", "btw", "steer"],
        dir: `/workspaces/crowd-${number}`,
        machine_id: "build-host",
        roles: [],
        session_id: `crowd-${number}-session`,
        title: `Crowd ${number}`,
      };
    });
    await setLiveSessions(crowd);
    await newMessage(page).click();
    let list = await openRecipients(page);
    await expect(async () => {
      await closeRecipients(page);
      list = await openRecipients(page);
      await expect(list.getByRole("option")).toHaveCount(101, { timeout: 1_000 });
    }).toPass({ timeout: REGISTRY_MS });
    for (let index = 0; index < 101; index++) await list.getByRole("option").nth(index).click();
    await closeRecipients(page);
    await expect(
      dialog.getByText("At most 100 recipients per broadcast; this one would reach 101.")
    ).toBeVisible();
    const crowdSend = dialog.getByRole("button", { name: "Send to 101" });
    await expect(crowdSend).toHaveAttribute("aria-disabled", "true");
    const crowdWrites = watchAgentWrites(page);
    await crowdSend.click({ force: true });
    // A refused Send makes no request, so there is nothing to wait on. The margin is the window
    // a POST made in the click's own task would show in.
    await sleep(ABSENCE_MARGIN_MS);
    expect(crowdWrites, `the writes a refused Send made:\n${described(crowdWrites)}`).toEqual([]);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): three sends, the first two each held two seconds by the test.
test("Send is held until the answer and Done returns the focus", async ({ browser }) => {
  await setLiveSessions([builder, checker]);
  await seedAgentThread({
    body: "Is the mirror warm?",
    replies: ["Warming it."],
    session: builder,
  });
  await seedAgentThread({
    body: "Is the index fresh?",
    replies: ["Rebuilding it."],
    session: checker,
  });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    // Below the reading column's width, where `j` puts the focus on the row it moves to.
    await page.setViewportSize({ height: 800, width: 1024 });
    await page.goto("/");
    const rows = inboxList(page).locator("[data-inbox-row]");
    await expect(rows).toHaveCount(2);
    const dialog = composeDialog(page);
    const send = dialog.locator('form button[type="submit"]');

    /** Picks the Builder, writes a body and presses Enter on Send. */
    const writeAndSend = async () => {
      await pickRecipients(page, [builder.title]);
      await dialog.getByRole("textbox", { name: "Comment" }).fill("Ping me when it is warm.");
      await send.focus();
      await page.keyboard.press("Enter");
    };

    // `j` puts a row in hand, then `n`.
    await blurFocus(page);
    await page.keyboard.press("j");
    const inHand = rows.first();
    expect(await holdsFocus(inHand), `the focus after j is on ${await describeFocus(page)}`).toBe(
      true
    );
    const scrolled = await page.evaluate(() => window.scrollY);
    await page.keyboard.press("n");
    await expect(dialog).toBeVisible();

    // The send's answer held two seconds: Send is held, and keeps the focus.
    await page.route(`**/api/v1/agents/${builder.session_id}/messages`, async (route) => {
      if (route.request().method() !== "POST") return route.continue();
      const response = await route.fetch();
      // The answer is held two seconds. The reads of the held Send and its focus below take a
      // frame and a task, far less than that.
      await sleep(2_000);
      await route.fulfill({ response });
    });
    const answer = sendAnswer(page, `/api/v1/agents/${builder.session_id}/messages`);
    await writeAndSend();
    await expect(send).toHaveAttribute("aria-disabled", "true");
    // A frame and a task on, while the answer is still held: the browser moves the focus off a
    // control that turns disabled at its next rendering update, so the focus is read past it.
    await frameAndTask(page);
    await expect(send).toHaveAttribute("aria-disabled", "true");
    expect(
      await holdsFocus(send),
      `while the send is out the focus is on ${await describeFocus(page)}`
    ).toBe(true);
    await (await answer).finished();
    await frameAndTask(page);
    await expect(dialog).toBeVisible();
    // The recipient's outcome in the place of the form: the form and its Send are gone.
    await expect(dialog.getByRole("region", { name: "Where your message went" })).toBeVisible();
    await expect(outcomeOf(page, builder.session_id)).toBeVisible();
    await expect(dialog.locator("form")).toHaveCount(0);
    await expect(dialog.getByRole("button", { name: /^Recipients/ })).toHaveCount(0);
    const done = dialog.getByRole("button", { name: "Done" });
    expect(
      await holdsFocus(done),
      `once the answer landed the focus is on ${await describeFocus(page)}`
    ).toBe(true);
    // The send's POST is no longer held from here on.
    await page.unroute(`**/api/v1/agents/${builder.session_id}/messages`);

    // Done: closed, the focus on the row in hand, and the page not scrolled.
    await page.keyboard.press("Enter");
    await expect(dialog).toHaveCount(0);
    expect(
      await holdsFocus(inHand),
      `after Done the focus is on ${await describeFocus(page)}`
    ).toBe(true);
    expect(await page.evaluate(() => window.scrollY)).toBe(scrolled);

    // From the New message control, to both sessions: one broadcast, its answer held two
    // seconds.
    const control = newMessage(page);
    await control.click();
    await expect(dialog).toBeVisible();
    await pickRecipients(page, [builder.title, checker.title]);
    await dialog
      .getByRole("textbox", { name: "Broadcast message" })
      .fill("Freeze at five, please.");
    const mode = listTrigger(dialog, "Delivery mode");
    await expect(mode).toHaveAccessibleName("Delivery mode BTW");
    await page.route("**/api/v1/broadcasts", async (route) => {
      if (route.request().method() !== "POST") return route.continue();
      const response = await route.fetch();
      // The answer is held two seconds. The click, the key and the reads of the held list below
      // take a frame and a task, far less than that.
      await sleep(2_000);
      await route.fulfill({ response });
    });
    const broadcastAnswer = sendAnswer(page, "/api/v1/broadcasts");
    await dialog.getByRole("button", { name: "Send to 2" }).click();
    // While it is held, the Delivery mode list is held with Send: a click and ArrowDown open
    // nothing, and the mode stays the one that went out.
    await expect(mode).toHaveAttribute("aria-disabled", "true");
    // Forced: Playwright waits for an aria-disabled control to turn enabled before it clicks,
    // and a person's click lands at once.
    await mode.click({ force: true });
    await page.keyboard.press("ArrowDown");
    // A frame and a task on, by when a list the click or the key opened is drawn. The answer is
    // still held there.
    await frameAndTask(page);
    await expect(mode).toHaveAttribute("aria-disabled", "true");
    // Read once: a read that waits passes at once on a list that has not opened yet.
    expect(
      await mode.getAttribute("aria-expanded"),
      "the held Delivery mode list after a click and ArrowDown"
    ).toBe("false");
    await expect(mode).toHaveAccessibleName("Delivery mode BTW");
    await (await broadcastAnswer).finished();
    await page.unroute("**/api/v1/broadcasts");

    // Done gives the focus back to the control.
    await done.click();
    await expect(dialog).toHaveCount(0);
    expect(
      await holdsFocus(control),
      `after Done the focus is on ${await describeFocus(page)}`
    ).toBe(true);

    // With nothing focused: Done leaves the focus on the page's body, in no text box.
    await blurFocus(page);
    await page.keyboard.press("n");
    await expect(dialog).toBeVisible();
    await writeAndSend();
    await done.click();
    await expect(dialog).toHaveCount(0);
    expect(await describeFocus(page)).toBe("the page's body");
  } finally {
    await alice.close();
  }
});

test("a message to one agent with an issue picked is the comment that mentions it", async ({
  browser,
}) => {
  await setLiveSessions([builder]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Warm the mirror" });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.setViewportSize({ height: 900, width: 1440 });
    await page.goto("/");
    await newMessage(page).click();
    const dialog = composeDialog(page);
    await pickRecipients(page, [builder.title]);
    await dialog.getByRole("button", { name: /^Choose issue/ }).click();
    await page
      .getByRole("listbox", { name: "Issue options" })
      .getByRole("option", { name: `${issue.key} · ${issue.title}` })
      .click();
    const mention = `[@${builder.title}](dispatch://agent/${builder.session_id})`;
    const body = "Is the mirror warm yet?";
    // The issue's channel writes the session's mention in front of the message; the person
    // writes after it.
    const box = dialog.getByRole("textbox", { name: "Comment" });
    await expect.poll(async () => (await box.inputValue()).trim()).toBe(mention);
    await box.press("End");
    await box.pressSequentially(body);
    const writes = watchAgentWrites(page);
    const commentAnswer = sendAnswer(page, `/api/v1/issues/${issue.key}/comments`);
    // The dialog reads the comment back as soon as the answer lands, so the wait for that read is
    // set before Send.
    const commentRead = page.waitForResponse(
      (read) =>
        read.request().method() === "GET" &&
        /^\/api\/v1\/comments\/[^/]+$/.test(new URL(read.url()).pathname)
    );
    await dialog.getByRole("button", { name: "Send" }).click();
    const response = await commentAnswer;
    const posted = keyedWrite(response.request(), isCommentInput).input;
    expect(posted.body).toContain(mention);
    expect(posted.body).toContain(body);
    expect(writes, `the writes to agents:\n${described(writes)}`).toEqual([]);
    const comment = (await response.json()) as Comment;
    // The outcome follows the comment's delivery to that session, which its read carries.
    expect(new URL((await commentRead).url()).pathname).toBe(`/api/v1/comments/${comment.id}`);
    await expect(outcomeOf(page, builder.session_id)).toContainText("Sent", { timeout: 15_000 });
    expect(await outcomeOf(page, builder.session_id).textContent()).not.toContain("Left out");
  } finally {
    await alice.close();
  }
});

test("the drawer opens the same dialog with n", async ({ browser }) => {
  await setLiveSessions([builder]);
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.setViewportSize({ height: 900, width: 1440 });
    await page.goto("/agents");
    // The key waits for the page's heading: pressed while the shell still waits on its whoami,
    // before it has registered its keys, `i` opens nothing.
    await expect(page.getByRole("heading", { level: 1, name: "Agents" })).toBeVisible();
    await blurFocus(page);
    await page.keyboard.press("i");
    const drawer = page.getByTestId("inbox-drawer");
    await expect(drawer).toBeVisible();
    const close = drawer.getByRole("button", { name: "Close" });
    await expect(close).toBeFocused();
    await expect(newMessage(drawer)).toBeVisible();

    await page.keyboard.press("n");
    const dialog = composeDialog(page);
    await expect(dialog).toBeVisible();
    await pickRecipients(page, [builder.title]);
    await dialog.getByRole("textbox", { name: "Comment" }).fill("Status of the mirror?");
    const answer = sendAnswer(page, `/api/v1/agents/${builder.session_id}/messages`);
    await dialog.getByRole("button", { name: "Send" }).click();
    await answer;
    await dialog.getByRole("button", { name: "Done" }).click();
    await expect(dialog).toHaveCount(0);
    await expect(drawer).toBeVisible();
    await expect(close).toBeFocused();
  } finally {
    await alice.close();
  }
});
