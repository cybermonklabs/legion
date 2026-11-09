/** The agent component as the page draws one fixed answer: the ask card's asker on a phone,
 *  the width a claimed Conversation keeps, the keys and taps that open a claimed issue or its
 *  agent, and a name with no space in it.
 *
 *  Each reads what the browser does - a box, a width, an address - so each runs against a
 *  recording rather than against a harness (packages/dispatch/AGENTS.md, "The stubbed layer").
 *  The rows that read the margin on the live document, that count the registry's reads, and
 *  that watch a rename and a stop arrive stayed in `agent-name.e2e.ts`.
 */
import { expect } from "@playwright/test";
import {
  agentIn,
  livePath,
  messageTurn,
  runCurrent,
  runId,
  seed,
  stopId,
  writer,
} from "./agent-name";
import { setLiveSessions } from "./agents";
import { createIssue, createMessage, createProject } from "./api";
import { stubbed } from "./stub/stub-test";

/** The file's own three sessions: one running, one stopped, one untitled. */
const test = stubbed(seed);

/** One session whose title has no space in it, on an issue of two turns. */
const longNameTest = stubbed(async () => {
  const title = "INFRA_440_implementer_round_2_resumed_ok";
  await createProject({ key: "AGT", name: "Agents" });
  const issue = await createIssue({ project: "AGT", title: "Long agent name" });
  await setLiveSessions([{ session_id: runId, title }]);
  const root = await createMessage(issue.key, { body: "Root turn" }, writer(runId, title));
  const reply = await createMessage(
    issue.key,
    { body: "Reply turn", in_reply_to: root.id },
    writer(runId, title)
  );
  return { issue: issue.key, reply: reply.id, root: root.id, title };
});

test("on a phone the ask card's asker still opens its copy handles, and the live link beside it opens the conversation @phone @pr", async ({
  page,
  scene,
}) => {
  await page.goto("/");
  const card = page.getByTestId(`ask-${scene.runAsk}`);
  const asker = card.getByRole("button", { expanded: false, name: /Running agent \(current\)/ });
  const live = card.getByRole("link", {
    exact: true,
    name: `Open live conversation with ${runCurrent}`,
  });
  await expect(asker).toBeVisible();
  await expect(live).toBeVisible();
  const [askerBox, liveBox] = await Promise.all([asker.boundingBox(), live.boundingBox()]);
  if (askerBox === null || liveBox === null) {
    throw new Error("the asker button or the live link has no box");
  }
  const overlaps =
    askerBox.x < liveBox.x + liveBox.width &&
    liveBox.x < askerBox.x + askerBox.width &&
    askerBox.y < liveBox.y + liveBox.height &&
    liveBox.y < askerBox.y + askerBox.height;
  expect(overlaps).toBe(false);
  expect(liveBox.height).toBeGreaterThanOrEqual(44);
  const widths = await page.evaluate(() => ({
    client: document.documentElement.clientWidth,
    scroll: document.documentElement.scrollWidth,
  }));
  expect(widths.scroll).toBe(widths.client);

  await asker.tap();
  await expect(
    card.getByRole("button", { name: `Copy session ID ${runId}`, exact: true })
  ).toBeVisible();
  await live.tap();
  await expect(page).toHaveURL(new RegExp(`${livePath(runId)}$`));
});

// The narrow-screen half of the registry walk in `agent-name.e2e.ts`: the claim chip and the
// agent turns carry a session's whole name, and on a phone that name is the widest thing the
// Conversation draws, so this is where one that cannot wrap would widen the page.
test("a claimed issue's Conversation fits a phone screen with its agent turns @phone @pr", async ({
  page,
  scene,
}) => {
  await page.goto(`/issues/${scene.runIssue}/conversation`);
  await expect(agentIn(page.getByTestId("issue-claim"), runId)).toHaveAttribute(
    "data-agent-state",
    "running"
  );
  await expect(agentIn(messageTurn(page, scene.stopMessage), stopId)).toBeVisible();
  const widths = await page.evaluate(() => ({
    client: document.documentElement.clientWidth,
    scroll: document.documentElement.scrollWidth,
  }));
  expect(widths.scroll).toBe(widths.client);
});

test("a claimed List row and Board card open their issue by keyboard, and the claim's agent link opens the agent @pr", async ({
  page,
  scene,
}) => {
  await page.setViewportSize({ height: 900, width: 1920 });
  await page.goto("/projects/AGT");
  await page.getByRole("button", { name: "Board" }).click();
  const card = page.locator(`[data-board-card="${scene.runIssue}"]`);
  const cardAgent = agentIn(card, runId);
  await expect(cardAgent).toHaveAttribute("data-agent-state", "running");
  const cardLink = cardAgent.locator(`a[href="${livePath(runId)}"]`);
  await card.focus();
  await page.keyboard.press("o");
  await expect(page).toHaveURL(new RegExp(`/issues/${scene.runIssue}$`));
  await page.goBack();
  await card.focus();
  await page.keyboard.press("Enter");
  await expect(page).toHaveURL(new RegExp(`/issues/${scene.runIssue}$`));
  await page.goBack();
  await cardLink.click();
  await expect(page).toHaveURL(new RegExp(`${livePath(runId)}$`));

  await page.goto("/projects/AGT");
  await page.getByRole("button", { name: "List" }).click();
  const row = page.locator(`[data-issue-row="${scene.runIssue}"]`);
  await expect(agentIn(row, runId)).toHaveAttribute("data-agent-state", "running");
  await row.focus();
  await page.keyboard.press("o");
  await expect(page).toHaveURL(new RegExp(`/issues/${scene.runIssue}$`));
  await page.goBack();
  await row.focus();
  await page.keyboard.press("Enter");
  await expect(page).toHaveURL(new RegExp(`/issues/${scene.runIssue}$`));
  await page.goBack();
  await agentIn(row, runId)
    .locator(`a[href="${livePath(runId)}"]`)
    .click();
  await expect(page).toHaveURL(new RegExp(`${livePath(runId)}$`));
});

// A board card is itself a drag handle, so the agent link inside it has to navigate under a
// finger rather than lifting the card. The keyboard walk above cannot see that; only a real tap
// can, so it is a phone row of its own.
test("a tap on a board card's agent link opens the agent rather than dragging the card @phone @pr", async ({
  page,
  scene,
}) => {
  await page.goto("/projects/AGT");
  await page.getByRole("button", { name: "Board" }).click();
  const card = page.locator(`[data-board-card="${scene.runIssue}"]`);
  const cardLink = agentIn(card, runId).locator(`a[href="${livePath(runId)}"]`);
  await expect(cardLink).toBeVisible();
  await cardLink.tap();
  await expect(page).toHaveURL(new RegExp(`${livePath(runId)}$`));
});

// A session title is free text (claim.e2e.ts), and a title with no space in it - an id, a
// snake_case name - is common. Where the component draws the author of a Conversation turn or
// reply, such a name must wrap or shorten inside the turn and never widen the page, as the claim
// chip, the Inbox ask card and the margin already hold at both widths.
longNameTest(
  "a long agent name with no space never widens the Conversation tab @pr",
  async ({ page, scene }) => {
    for (const { height, width } of [
      { height: 844, width: 390 },
      { height: 900, width: 1280 },
    ]) {
      await page.setViewportSize({ height, width });
      await page.goto(`/issues/${scene.issue}/conversation`);
      for (const id of [scene.root, scene.reply]) {
        const agent = agentIn(messageTurn(page, id), runId);
        await expect(agent).toHaveAttribute("data-agent-state", "running");
        await expect(agent.getByRole("link", { name: scene.title })).toBeVisible();
      }
      const widths = await page.evaluate(() => ({
        clientWidth: document.documentElement.clientWidth,
        scrollWidth: document.documentElement.scrollWidth,
      }));
      expect(widths.scrollWidth, `page width at ${width}px`).toBe(widths.clientWidth);
      expect(widths.clientWidth, `layout viewport at ${width}px`).toBeLessThanOrEqual(width);
    }
  }
);
