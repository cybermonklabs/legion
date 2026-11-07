import { expect, test } from "@playwright/test";

import { shownAgentRows } from "./agents";
import { getInbox } from "./api";
import { askCard, inboxRow } from "./inbox-pane";
import { resetDatabase } from "./seed";
import { navigateInApp } from "./sends";
import { asUser } from "./users";
import { seedWorkspace } from "./workspace";

test.beforeEach(async () => {
  await resetDatabase();
});

// Every surface the `person` scope opens has to come up non-empty from this one seed: an empty
// Inbox, Agents page or broadcast would prove nothing about a change to it.
// SLOW(>=2s): the whole workspace seed, the four surfaces it has to fill and three full-page
// screenshots of them.
test("the seeded workspace fills every surface the person scope opens", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "one browser proves the seeded workspace");
  const seeded = await seedWorkspace();
  expect(await getInbox({ login: "alice" })).toHaveLength(seeded.inboxRows);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    // The Inbox opens on the reader's own work: alice's issues' asks, with the snoozed one
    // folded under Later. From 1280 px the questions are short rows and only the selected one
    // is a card (INFRA-575), so a question's urgency and its author chip are read on the card
    // the pane draws for it: a session's ask names it by the title its write stamped, as a real
    // agent's does, rather than by its raw id.
    await page.goto("/");
    await expect(await askCard(page, seeded.asks.blocking)).toHaveAttribute(
      "aria-label",
      "Urgency: Blocking"
    );
    await expect(page.getByRole("button", { name: /^Later \(1\)/ })).toBeVisible();
    await expect(inboxRow(page, seeded.asks.snoozed)).toBeHidden();
    await expect(
      (await askCard(page, seeded.asks.rotate)).getByRole("button", {
        expanded: false,
        name: /Reviewer/,
      })
    ).toBeVisible();
    // The runbook's question is on a document, which nobody holds, so it is the one row of this
    // seed her own list leaves out; the Unassigned filter is where it is listed.
    await expect(inboxRow(page, seeded.asks.document)).toHaveCount(0);
    await page.screenshot({ path: testInfo.outputPath("workspace-inbox.png"), fullPage: true });
    await navigateInApp(page, "/?assignee=unassigned");
    await expect(inboxRow(page, seeded.asks.document)).toContainText("Runbook");

    // Every session with Dispatch activity is `Active`; the one Dispatch never heard from is
    // `Idle` and the one unseen for twenty minutes is `Quiet`. Every group is open when the
    // page loads, so the seed fills the list rather than two closed folds.
    await navigateInApp(page, "/agents");
    const agents = page.getByRole("region", { name: "Agents" });
    const idle = agents.getByTestId("agents-group-toggle-idle");
    const quiet = agents.getByTestId("agents-group-toggle-quiet");
    const shownTitles = shownAgentRows(page).locator("h2");
    await expect(idle).toHaveAccessibleName("Idle (1)");
    await expect(quiet).toHaveAccessibleName("Quiet (1)");
    await expect
      .poll(async () => (await shownTitles.allTextContents()).sort())
      .toEqual(["Archivist", "Observer", "Planner", "Reviewer", "Tester"]);
    // A closed group's rows leave the page; its heading stays, with its count.
    await idle.click();
    await quiet.click();
    await expect
      .poll(async () => (await shownTitles.allTextContents()).sort())
      .toEqual(["Planner", "Reviewer", "Tester"]);
    await idle.click();
    await quiet.click();
    const plannerCard = agents
      .locator("article")
      .filter({ has: page.getByRole("heading", { level: 2, name: "Planner" }) });
    await plannerCard.getByRole("button", { exact: true, name: "Planner" }).click();
    await expect(
      plannerCard.getByRole("list", { name: "Conversation with Planner" })
    ).toContainText("Half-way; the seed is next.");
    await page.screenshot({ path: testInfo.outputPath("workspace-agents.png"), fullPage: true });

    // The broadcast keeps the order it was sent in, which is not alphabetical, and shows the one
    // answer it has.
    await navigateInApp(page, `/agents/broadcasts/${seeded.broadcast.id}`);
    const broadcast = page.getByRole("region", { name: "Broadcast" });
    await expect(broadcast.getByRole("article")).toHaveText([/Tester/, /Observer/, /Planner/]);
    await expect(broadcast.getByText("1 of 3 answered")).toBeVisible();
    await page.screenshot({ path: testInfo.outputPath("workspace-broadcast.png"), fullPage: true });

    // The issue's Conversation holds the answered ask and, inside the mention's thread, the
    // mentioned session's reply.
    await navigateInApp(page, `/issues/${seeded.issues.broadcastOrder}/conversation`);
    const turns = page.getByRole("list", { name: "Conversation turns" });
    await expect(turns.getByText("Ship the recipient-order fix?")).toBeVisible();
    const mention = turns
      .locator('[data-turn^="comment:"]')
      .filter({ hasText: "@Tester please re-run the broadcast spec" });
    await mention.getByRole("button", { name: "Expand thread" }).click();
    await expect(mention.getByText("Re-ran: 2 passed.")).toBeVisible();
    await page.screenshot({
      path: testInfo.outputPath("workspace-conversation.png"),
      fullPage: true,
    });
  } finally {
    await alice.close();
  }
});
