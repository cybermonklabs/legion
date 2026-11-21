import {
  agentIn,
  expectAgent,
  livePath,
  messageTurn,
  openMarginThread,
  runCurrent,
  runId,
  runStamped,
  seed,
  stopId,
  stopShort,
  stopStamped,
  untitledId,
  untitledShort,
} from "./agent-name";
import { setLiveSessions, setSessionLive } from "./agents";
import { askCard } from "./inbox-pane";
import { devices, expect, test } from "./test";
import { asUser } from "./users";

/**
 * One agent component draws every agent the app shows (dispatch://INFRA-440). What is left here
 * is the rows a recording cannot answer: the margin thread on the live document, the registry's
 * own read count, and a rename and a stop arriving while the page watches. What the component
 * draws over one fixed answer is in `agent-name.stub.e2e.ts`, and the three sessions both files
 * are read over are in `agent-name.ts`.
 */

// SLOW(>=2s): the seed's session writes and four page views - the issue, the Conversation, the
// Inbox and the margin - which are the four places the row holds the component to.
test("Claimed by, a Conversation turn, an Inbox ask card and a margin comment show the agent's current name, whether it runs, and a live link", async ({
  browser,
}) => {
  const seeded = await seed();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();

    // Claimed by: the running holder by its current title with the owner suffix, the stopped
    // holder by the title it stamped.
    await page.goto(`/issues/${seeded.runIssue}`);
    const runClaim = page.getByTestId("issue-claim");
    await expectAgent(runClaim, runId, { hidden: runStamped, name: runCurrent, running: true });
    await expect(agentIn(runClaim, runId)).toContainText(`${runCurrent} (for alice)`);
    await page.goto(`/issues/${seeded.stopIssue}`);
    await expectAgent(page.getByTestId("issue-claim"), stopId, {
      name: stopStamped,
      running: false,
    });

    // Conversation turns, the untitled agent by its short id.
    await page.goto(`/issues/${seeded.runIssue}/conversation`);
    await expectAgent(messageTurn(page, seeded.runMessage), runId, {
      hidden: runStamped,
      name: runCurrent,
      running: true,
    });
    await expectAgent(messageTurn(page, seeded.stopMessage), stopId, {
      name: stopStamped,
      running: false,
    });
    await expectAgent(messageTurn(page, seeded.untitledMessage), untitledId, {
      name: untitledShort,
      running: false,
    });

    // Inbox ask cards: the asker's copy-handles button stays, and the live link sits beside it,
    // never inside it. From 1280 px the card is the reading pane's (INFRA-575), so each
    // question is read on the card that width draws for it.
    await page.goto("/");
    for (const [askId, id, name, running] of [
      [seeded.runAsk, runId, runCurrent, true],
      [seeded.stopAsk, stopId, stopStamped, false],
    ] as const) {
      const card = await askCard(page, askId);
      await expectAgent(card, id, { name, running });
      const live = card.getByRole("link", {
        exact: true,
        name: `Open live conversation with ${name}`,
      });
      await expect(live).toHaveAttribute("href", livePath(id));
      expect(await live.evaluate((link) => link.closest("button") === null)).toBe(true);
      await expect(
        card.getByRole("button", {
          expanded: false,
          name: new RegExp(name.replace(/[\\^$.*+?()[\]{}|]/g, "\\$&")),
        })
      ).toBeVisible();
    }

    // Margin comments, each in its opened thread.
    await page.goto(`/issues/${seeded.runIssue}/spec`);
    const runThread = await openMarginThread(page, seeded.runComment);
    await expectAgent(runThread, runId, { hidden: runStamped, name: runCurrent, running: true });
    const stopThread = await openMarginThread(page, seeded.stopComment);
    await expectAgent(stopThread, stopId, { name: stopStamped, running: false });
  } finally {
    await alice.close();
  }
});

test("the live link opens that agent's conversation, running or not", async ({ browser }) => {
  const seeded = await seed();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${seeded.runIssue}`);
    const runLink = agentIn(page.getByTestId("issue-claim"), runId).locator(
      `a[href="${livePath(runId)}"]`
    );
    await expect(runLink).toBeVisible();
    await runLink.click();
    await expect(page).toHaveURL(new RegExp(`${livePath(runId)}$`));
    await expect(page.getByRole("heading", { level: 1 })).toHaveText(runCurrent);

    await page.goto(`/issues/${seeded.runIssue}/spec`);
    const stopThread = await openMarginThread(page, seeded.stopComment);
    const stopLink = agentIn(stopThread, stopId).locator(`a[href="${livePath(stopId)}"]`);
    await expect(stopLink).toBeVisible();
    await stopLink.click();
    await expect(page).toHaveURL(new RegExp(`${livePath(stopId)}$`));
    // The live page knows only the route's id and the registry, so a stopped agent reads by its
    // short id there; the id in the URL is what says the right conversation opened.
    await expect(page.getByRole("heading", { level: 1 })).toHaveText(stopShort);
  } finally {
    await alice.close();
  }
});

test("the page reads the agent list once, asks nothing per agent, and shows a rename and a stop without a reload", async ({
  browser,
}) => {
  // The open page learns of a rename and then of a stop only from the shell's 15 s registry
  // poll, which this row's page clock fires: no row-level timeout is needed for it.
  const seeded = await seed();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.clock.install();
    const registryReads: string[] = [];
    const perAgentReads: string[] = [];
    page.on("request", (request) => {
      const path = new URL(request.url()).pathname;
      if (path === "/api/v1/agents") {
        registryReads.push(path);
      } else if (path.startsWith("/api/v1/agents/")) {
        perAgentReads.push(path);
      }
    });
    await page.goto(`/issues/${seeded.runIssue}/conversation`);
    const claim = agentIn(page.getByTestId("issue-claim"), runId);
    await expect(claim).toHaveAttribute("data-agent-state", "running");
    await expect(agentIn(messageTurn(page, seeded.stopMessage), stopId)).toHaveAttribute(
      "data-agent-state",
      "not-running"
    );
    await expect(agentIn(messageTurn(page, seeded.untitledMessage), untitledId)).toBeVisible();
    expect(registryReads).toHaveLength(1);
    expect(perAgentReads).toEqual([]);

    // The listener renames the running agent, then drops it: the open page follows both. The page
    // learns of each only on the shell's 15 s registry poll, which the clock fires rather than
    // the row waiting it out; each jump also trips the live socket's own 15 s silence check, so
    // the tab reconnects, which changes neither count below (a reconnect reads no agent).
    await setLiveSessions([{ session_id: runId, title: "Running agent (renamed)" }]);
    await page.clock.fastForward(15_000);
    // TanStack's notifyManager batches its delivery on a zero-delay page timer.
    await page.clock.runFor(1);
    await expect(claim).toContainText("Running agent (renamed)");
    await expect(agentIn(messageTurn(page, seeded.runMessage), runId)).toContainText(
      "Running agent (renamed)"
    );
    await setSessionLive(runId, false);
    await page.clock.fastForward(15_000);
    await page.clock.runFor(1);
    await expect(claim).toHaveAttribute("data-agent-state", "not-running");
    await expect(claim.getByRole("img", { exact: true, name: "Not running" })).toBeVisible();
    expect(perAgentReads).toEqual([]);
  } finally {
    await alice.close();
  }
});

// The ask card's provenance line holds the asker's copy-handles button with the live link beside
// it, the time and the ask's reference; the turn is named there only where it is not the
// reader's own (INFRA-3231's R7). The line stays one line, so on a phone, beside the live link's
// 44 px touch box, the asker's name gives up width and is cut short, whole in its title: the
// design's own picture of the card on a phone draws it so (INFRA-3231's ask-card-14-phone-card).
// Before that design the line gave the name its room and this row read the name whole.
//
// It stayed on the real stack after a measurement, not by the exclusion rules: moved to the
// stubbed layer it read `scrollWidth` 144 against `clientWidth` 116 and failed, because the
// claim is a text measurement and a stubbed page paints before its web fonts have loaded, where
// the real stack's server round trips give them the time. A row whose claim is a width needs a
// settled font, and this layer is faster than that.
test("on a phone the ask card's asker shares one line with the live link and keeps its whole name in its title @phone", async ({
  browser,
}) => {
  const seeded = await seed();
  const alice = await asUser(browser, "alice", devices["iPhone 13"]);
  try {
    const page = await alice.newPage();
    await page.goto("/");
    const card = page.getByTestId(`ask-${seeded.runAsk}`);
    // The reader's own question names no turn: it is hers.
    await expect(card.getByTestId(`turn-${seeded.runAsk}`)).toHaveCount(0);
    const live = card.getByRole("link", {
      exact: true,
      name: `Open live conversation with ${runCurrent}`,
    });
    await expect(live).toBeVisible();
    const name = card
      .getByRole("button", { expanded: false, name: /Running agent \(current\)/ })
      .locator(`[title="${runCurrent} · ${runId}"]`);
    await expect(name).toHaveText(runCurrent);
    // One line: the name and the live link stand on the same row, and the line, cut short where
    // it must be, widens neither the card nor the page.
    const [nameBox, liveBox] = await Promise.all([name.boundingBox(), live.boundingBox()]);
    if (nameBox === null || liveBox === null)
      throw new Error("the name or the live link has no box");
    expect(nameBox.y + nameBox.height).toBeGreaterThan(liveBox.y);
    expect(nameBox.y).toBeLessThan(liveBox.y + liveBox.height);
    expect(
      await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)
    ).toBe(true);
  } finally {
    await alice.close();
  }
});
