import { NO_LIVE_SESSION, shortSessionId } from "@legion/contracts";

import { AGENT_REGISTRY_POLL_MS } from "../web/src/features/refs/agent-registry";
import {
  BROADCAST_REFUSED,
  type FakeSession,
  getSentMessages,
  QUEUED,
  refuseBroadcasts,
  setLiveSessions,
  setSessionRecord,
  setSessionSendState,
  setSessionStopped,
} from "./agents";
import { createIssue, createMessage, createProject, queuedEnvelopesOf } from "./api";
import { sessionIds } from "./session-ids";
import { expect, type Locator, type Page, test } from "./test";
import { asUser } from "./users";

/**
 * INFRA-626. A session the Envoy listener knows and that is not running is a recipient, not
 * a refusal: the listener stores the message for it and hands it over when it runs again, and
 * Dispatch records that attempt `queued`. This spec reads that word (`QUEUED`) on each of the
 * three surfaces a person sees it on - a message aimed at one session, a mention in a comment, and
 * a broadcast recipient. A session Envoy knows nothing of is still refused as `no live session` to
 * a mention, and a message aimed at it is queued by Dispatch itself (INFRA-3740), under the same
 * word.
 */

const planner: FakeSession = {
  capabilities: ["aside", "btw", "steer"],
  dir: "/workspaces/planner",
  machine_id: "queued-host",
  roles: [],
  session_id: sessionIds.planner,
  title: "Planner",
};

const observer: FakeSession = {
  capabilities: ["aside", "btw", "steer"],
  dir: "/workspaces/observer",
  machine_id: "queued-host",
  roles: [],
  session_id: sessionIds.observer,
  title: "Observer",
};

/** A top-level Conversation turn containing `text` exactly. */
function turn(page: Page, text: string): Locator {
  return page
    .getByRole("list", { name: "Conversation turns" })
    .locator(":scope > li")
    .filter({ has: page.getByText(text, { exact: true }) });
}

/** The ids every send the fake listener took was aimed at, in order. */
async function sentTo(): Promise<unknown[]> {
  return (await getSentMessages()).map((send) => send.target_session);
}

/** An issue of its own, so one row's conversation is never another's. */
async function issueFor(title: string): Promise<string> {
  await createProject({ key: "CORE", name: "Core" });
  return (await createIssue({ project: "CORE", title })).key;
}

test("a person's message to a stopped but known session is queued for it, and the listener takes it", async ({
  browser,
}) => {
  await setLiveSessions([planner]);
  const issue = await issueFor("Message to a stopped session");
  await setSessionStopped(planner.session_id, true);
  const body = "Report status when you are back.";
  await createMessage(issue, { body, delivery: "steer", target: `session:${planner.session_id}` });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue}/conversation`);
    // The registry no longer holds the session, so the card names it by the short form of its
    // id, as it names every session that has left.
    await expect(turn(page, body)).toContainText(QUEUED(shortSessionId(planner.session_id)));
    await expect(turn(page, body)).not.toContainText(NO_LIVE_SESSION);
    // Queued is the listener having taken it, so the send is real and Retry is not offered: a
    // second copy is exactly what the stored message makes unnecessary.
    expect(await sentTo()).toEqual([planner.session_id]);
    await expect(turn(page, body).getByRole("button", { name: "Retry" })).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

// The send's own answer decides, not the registry read before it: a session whose entry was
// still in place when Dispatch resolved it, and whose process had gone by the send, is queued
// too - the crash that leaves a registration behind.
test("a message whose registry read found the session live and whose send answered stopped is queued", async ({
  browser,
}) => {
  await setLiveSessions([planner]);
  const issue = await issueFor("Message to a session that crashed");
  await setSessionSendState(planner.session_id, "stopped");
  const body = "Did you survive the deploy?";
  await createMessage(issue, { body, delivery: "steer", target: `session:${planner.session_id}` });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue}/conversation`);
    await expect(turn(page, body)).toContainText(QUEUED("Planner"));
    expect(await sentTo()).toEqual([planner.session_id]);
  } finally {
    await alice.close();
  }
});

test("a mention of a stopped but known session is queued for it, and the listener takes it", async ({
  browser,
}) => {
  await setLiveSessions([planner]);
  const issue = await issueFor("Mention of a stopped session");

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue}/conversation`);
    const field = page.getByRole("form", { name: "Comment composer" }).getByLabel("Comment");
    await field.fill("/btw @");
    await page
      .getByRole("listbox", { name: "Mention suggestions" })
      .getByRole("option", { name: "Planner" })
      .click();
    // The composer offered the mention off the live list, and the session stops between that
    // read and the send: the listener still knows it, so the mention is stored for it.
    await setSessionStopped(planner.session_id, true);
    await page
      .getByRole("form", { name: "Comment composer" })
      .getByRole("button", { name: "Send" })
      .click();
    const deliveries = page.getByRole("list", { name: "Mention deliveries" });
    await expect(deliveries).toContainText(QUEUED("Planner"));
    await expect(deliveries).not.toContainText(NO_LIVE_SESSION);
    await expect(deliveries.getByRole("button", { name: "Retry" })).toHaveCount(0);
    await expect.poll(sentTo).toEqual([planner.session_id]);
  } finally {
    await alice.close();
  }
});

test("a broadcast keeps a session that stopped after it was ticked, and queues for it", async ({
  browser,
}) => {
  test.skip(test.info().project.name !== "chromium", "one browser proves the broadcast flow");
  // The composer learns of a departure on its 15 s agent poll, which the page's own clock spends.
  await setLiveSessions([planner, observer]);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.clock.install();
    await page.goto("/agents");
    for (const title of ["Planner", "Observer"]) {
      await page.getByRole("checkbox", { name: `Select ${title} for broadcast` }).check();
    }
    const composer = page.getByRole("region", { name: "Broadcast" });
    await composer.getByRole("combobox", { name: "Delivery mode" }).selectOption("steer");
    await composer.getByRole("textbox", { name: "Broadcast message" }).fill("Report status.");

    // The Observer stops. The composer does not leave a session out for having left the live
    // list: Envoy may still know it, and only the server can say. The chip reads the short form
    // of its id, since the registry no longer holds its title.
    await setSessionStopped(observer.session_id, true);
    await page.clock.fastForward(AGENT_REGISTRY_POLL_MS);
    await expect(
      composer.getByRole("button", {
        exact: true,
        name: `${shortSessionId(observer.session_id)} ✕`,
      })
    ).toBeVisible();
    await expect(composer.getByText(/^Excluded:/)).toHaveCount(0);
    const send = composer.getByRole("button", { name: "Send to 2" });
    await expect(send).toBeEnabled();
    await send.click();

    await page.waitForURL(/\/agents\/broadcasts\/[0-9a-f-]+$/);
    const view = page.getByRole("region", { name: "Broadcast" });
    await expect(view.getByRole("article", { name: "Planner" })).toContainText(
      "Sent to Planner (Send)"
    );
    await expect(
      view.getByRole("article", { name: shortSessionId(observer.session_id) })
    ).toContainText(QUEUED(shortSessionId(observer.session_id)));
    await expect(view.getByText(/^Excluded:/)).toHaveCount(0);
    await expect
      .poll(async () => (await sentTo()).sort())
      .toEqual([observer.session_id, planner.session_id].sort());
  } finally {
    await alice.close();
  }
});

// The strip over the composer is where the sender watches the broadcast they just made, and a
// copy the listener is holding for a stopped session is not a copy delivered. The strip counts
// those on their own, so it does not read "Sent to 2 agents — delivered" while one of the two is
// still waiting for its session to run.
test("the broadcast strip counts a queued copy rather than calling the broadcast delivered", async ({
  browser,
}) => {
  test.skip(test.info().project.name !== "chromium", "one browser proves the strip");
  // The composer learns of a departure on its 15 s agent poll, which the page's own clock spends.
  await setLiveSessions([planner, observer]);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.clock.install();
    await page.goto("/agents");
    const composer = page.getByRole("region", { name: "Broadcast" });
    const sends = page.getByRole("region", { name: "Sends" });
    const select = async () => {
      for (const title of ["Planner", "Observer"]) {
        await page.getByRole("checkbox", { name: `Select ${title} for broadcast` }).check();
      }
      await composer.getByRole("combobox", { name: "Delivery mode" }).selectOption("steer");
    };

    // One refused send first, which stays in the strip: with a second row the page keeps the
    // Agents page rather than opening the broadcast it made, so the strip stays in view.
    const refusal = await refuseBroadcasts(page);
    await select();
    await composer.getByRole("textbox", { name: "Broadcast message" }).fill("Refused.");
    await composer.getByRole("button", { name: "Send to 2" }).click();
    const refused = sends.getByRole("listitem").filter({ hasText: "Refused." });
    await expect(refused.getByRole("status")).toHaveText(
      `Could not send to 2 agents: ${BROADCAST_REFUSED}`
    );
    refusal.allow();

    await select();
    await composer.getByRole("textbox", { name: "Broadcast message" }).fill("Report status.");
    // The Observer stops before the press: Envoy still knows it, so it stays a recipient and its
    // copy is queued for it.
    await setSessionStopped(observer.session_id, true);
    await page.clock.fastForward(AGENT_REGISTRY_POLL_MS);
    await expect(
      composer.getByRole("button", {
        exact: true,
        name: `${shortSessionId(observer.session_id)} ✕`,
      })
    ).toBeVisible();
    await composer.getByRole("button", { name: "Send to 2" }).click();

    const sent = sends.getByRole("listitem").filter({ hasText: "Report status." });
    await expect(sent.getByRole("status")).toHaveText(
      "Sent to 2 agents — queued for 1 agent not running",
      { timeout: 30_000 }
    );
    await expect(page).toHaveURL(/\/agents$/);
  } finally {
    await alice.close();
  }
});

// A session Envoy knows nothing of - neither its registry nor its known-session store holds it -
// gets no send from Dispatch: the registry read answers `no live session`. A message aimed at it
// is queued all the same, by Dispatch publishing it on the session's agent subject itself, so the
// card reads the one queued sentence and no failure. (A mention of such a session is still refused
// as `no live session`, `mention-composer.e2e.ts`; a broadcast recipient is excluded,
// `broadcast.e2e.ts`.)
test("a message to a session Envoy does not know is queued by Dispatch itself, with no send to the listener", async ({
  browser,
}) => {
  await setLiveSessions([planner]);
  const issue = await issueFor("Message to a session nobody knows");
  await setSessionRecord(planner.session_id, "absent");
  const body = "Are you there at all?";
  const message = await createMessage(issue, {
    body,
    delivery: "steer",
    target: `session:${planner.session_id}`,
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue}/conversation`);
    await expect(turn(page, body)).toContainText(QUEUED(shortSessionId(planner.session_id)));
    await expect(turn(page, body)).not.toContainText(NO_LIVE_SESSION);
    await expect(turn(page, body).getByRole("button", { name: "Retry" })).toHaveCount(0);
    expect(await sentTo()).toEqual([]);
    const queued = await queuedEnvelopesOf(message.id);
    expect(queued).toEqual([
      expect.objectContaining({
        dedupe_key: `agent.${planner.session_id}.${message.id}:steer`,
        source: "dispatch",
        subject: `notifications.agent.${planner.session_id}`,
      }),
    ]);
  } finally {
    await alice.close();
  }
});
