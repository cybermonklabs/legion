import { roleToken } from "@legion/contracts";

import {
  getPublishedMessages,
  getSentMessages,
  legionAgentEntry,
  setLegionAgents,
  setLiveSessions,
} from "./agents";
import { createComment, createIssue, createMessage, createProject } from "./api";
import { expect, type Locator, type Page, test } from "./test";
import { asUser } from "./users";

/**
 * cybermonklabs/legion#2230. A role whose holder is gone because the Legion daemon released the
 * role's idle issue pod - its newest registry entry is `released`, and its issue still runs - is
 * coming back: the daemon resumes that session on the role's next wake. A person's message to it
 * is kept for that next holder: Dispatch publishes it on the role's topic through the listener's
 * publish route, which keeps it on the role's kept lane for an hour and wakes the role, and
 * records the attempt taken. The card says so, and offers no Retry. A role whose entry is not
 * released keeps the listener's no-holder failure, as it did before.
 *
 * The sentence is spelt out here rather than imported from the SPA, so a change to the SPA's
 * wording fails a row here rather than passing with it.
 */
const KEPT = (role: string) =>
  `Kept for ${role}: the role's next session gets it if one takes the role within one hour`;

/** The Legion project token every registry entry here is written under. */
const LEGION_PROJECT = "e2elegion";

/** A top-level Conversation turn containing `text` exactly. */
function turn(page: Page, text: string): Locator {
  return page
    .getByRole("list", { name: "Conversation turns" })
    .locator(":scope > li")
    .filter({ has: page.getByText(text, { exact: true }) });
}

/** An issue of its own, whose planner role's newest registry entry is released or not, and that
 *  role's token. The planner's session is not live, so no live session holds the role. */
async function plannerRoleOf(title: string, released: boolean): Promise<[string, string]> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = (await createIssue({ project: "CORE", title })).key;
  await setLiveSessions([]);
  await setLegionAgents([
    legionAgentEntry({
      issue,
      legionProject: LEGION_PROJECT,
      released,
      role: "planner",
      sessionId: "e2e-released-planner",
      startedAt: "2026-11-16T00:00:00Z",
    }),
  ]);
  return [issue, roleToken(LEGION_PROJECT, issue, "planner")];
}

test("a person's message to a role whose pod was released is kept for the role's next session", async ({
  browser,
}) => {
  const [issue, token] = await plannerRoleOf("Message to a released role", true);
  const role = `role:${token}`;
  const body = "Pick this back up when you wake.";
  await createMessage(issue, { body, delivery: "steer", target: role });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue}/conversation`);
    await expect(turn(page, body)).toContainText(KEPT(role));
    await expect(turn(page, body)).not.toContainText("no holder");
    await expect(turn(page, body)).not.toContainText("is stopped");
    // Kept is the listener having taken it for the role, so the publish is real and Retry is not
    // offered: a second copy is exactly what the kept message makes unnecessary.
    await expect(turn(page, body).getByRole("button", { name: "Retry" })).toHaveCount(0);
    const published = await getPublishedMessages();
    expect(published).toHaveLength(1);
    expect(published[0]).toMatchObject({
      keep: true,
      message: body,
      source: "dispatch",
      topic: `notifications.role.${token}`,
    });
    expect(await getSentMessages()).toEqual([]);
  } finally {
    await alice.close();
  }
});

test("a mention of a role whose pod was released is kept for the role's next session", async ({
  browser,
}) => {
  const [issue, token] = await plannerRoleOf("Mention of a released role", true);
  const role = `role:${token}`;
  await createComment(issue, {
    body: "Can the planner explain this when it wakes?",
    delivery: "btw",
    mentions: [{ target: role }],
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue}/conversation`);
    const deliveries = page.getByRole("list", { name: "Mention deliveries" });
    await expect(deliveries).toContainText(KEPT(role));
    await expect(deliveries).not.toContainText("no holder");
    await expect(deliveries.getByRole("button", { name: "Retry" })).toHaveCount(0);
    await expect
      .poll(async () => (await getPublishedMessages()).map((publish) => publish.topic))
      .toEqual([`notifications.role.${token}`]);
    expect(await getSentMessages()).toEqual([]);
  } finally {
    await alice.close();
  }
});

// The other half of the rule: a role whose newest entry is not released has no holder coming
// back on a wake of Dispatch's making, and its message keeps the listener's own no-holder answer,
// with nothing published and nothing sent.
test("a message to a role whose entry is not released still fails with no holder", async ({
  browser,
}) => {
  const [issue, token] = await plannerRoleOf("Message to an unheld role", false);
  const body = "Is anyone planning this?";
  await createMessage(issue, { body, delivery: "steer", target: `role:${token}` });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue}/conversation`);
    await expect(turn(page, body)).toContainText(`Failed: no holder for role ${token}`);
    await expect(turn(page, body)).not.toContainText("Kept for");
    expect(await getPublishedMessages()).toEqual([]);
    expect(await getSentMessages()).toEqual([]);
  } finally {
    await alice.close();
  }
});
