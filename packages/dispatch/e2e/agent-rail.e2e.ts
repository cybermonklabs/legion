import { expect, type Page, test } from "@playwright/test";

import { agentRow, type FakeSession, setLiveSessions } from "./agents";
import { createAgentMessage, getAgentStates, putAgentState, replyToMessageDelivery } from "./api";
import {
  closeRail,
  expectRailAgents,
  openRail,
  RAIL_AGENTS_ID,
  railAgentIds,
  railAgentRow,
  railAgentRows,
  railAgents,
  railAgentsFold,
  railIsSheet,
} from "./rail";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/**
 * The rail's Agents list (INFRA-812, R1; the browser cases of the Verification document's k1,
 * dispatch://INFRA-812/artifact/verification-md).
 *
 * The heading stays the link to the Agents page and folds the rows under it; the rows are the
 * person's pinned agents, oldest pin first, each with its running state, its name, its unread
 * count and the control that unpins it. The pin belongs to the person, not to this browser, so
 * it is made and unmade from three places, it reaches the person's other browser from the event
 * the write announces, and the list a browser kept under the old local-storage key becomes the
 * person's own at the first load after the change.
 */

const ALPHA = "01a0e200-4848-7473-acc5-00000000000a";
const BETA = "01a0e200-4848-7473-acc5-00000000000b";
const GAMMA = "01a0e200-4848-7473-acc5-00000000000c";

/** The empty rail's one line, which says where the pin is (`Sidebar.tsx`). */
const NO_PINS_TEXT = "Pin an agent on the Agents page, or in its live view, to keep it here.";

/** A live session for the rail to list. `last_seen` is taken when the row seeds, never at this
 *  module's scope, which a worker evaluates once and every later spec of that worker ages. */
function agent(sessionId: string, title: string): FakeSession {
  return {
    capabilities: ["aside", "btw", "steer"],
    dir: `/workspaces/${title.toLowerCase()}`,
    last_seen: Date.now() - 5_000,
    machine_id: "rail-host",
    roles: [],
    session_id: sessionId,
    title,
  };
}

/** The three sessions the rail rows pick from: two pinned and one not, in most rows. */
function railSessions(): FakeSession[] {
  return [agent(ALPHA, "Alpha"), agent(BETA, "Beta"), agent(GAMMA, "Gamma")];
}

/** `count` replies of `sessionId`'s to the viewer, which is what the rail row's badge counts. */
async function seedUnreadReplies(sessionId: string, count: number): Promise<void> {
  const asked = await createAgentMessage(sessionId, {
    body: "Where did the build go?",
    delivery: "aside",
  });
  for (let index = 0; index < count; index += 1) {
    await replyToMessageDelivery(
      asked.id,
      { attempt: 1, body: `Answer ${index}` },
      { id: sessionId, kind: "session" },
      { followUp: index > 0 }
    );
  }
}

/** The pin control on a session's Agents-page row, by what it offers to do. */
function agentsPagePin(page: Page, sessionId: string, label: string) {
  return agentRow(page, sessionId).getByRole("button", { exact: true, name: label });
}

/** The pin control in a session's live view header, by what it offers to do. */
function liveViewPin(page: Page, label: string) {
  return page.getByTestId("agent-view-header").getByRole("button", { exact: true, name: label });
}

/** Opens a session's live view from its Agents-page row, which is the way in that row offers. */
async function openLiveViewFromAgentsPage(page: Page, sessionId: string, title: string) {
  await agentRow(page, sessionId)
    .getByRole("link", { name: `Open live conversation with ${title}` })
    .click();
  await expect(page.getByTestId("agent-view-header")).toBeVisible();
}

test.beforeEach(async () => {
  await resetDatabase();
});

// The rows under the heading are the person's pins and nothing else - an unpinned live session
// is in none of them - and each row carries what picking an agent needs: whether it is running,
// its name, and how many of its replies the person has not read. The chevron beside the heading
// folds them away, and the heading itself is still the link to the Agents page. At a phone width
// the whole rail, these rows with it, is the sheet behind the Menu control.
// SLOW(>=2s): a reset, two seeded replies and the shell's first load before the rail is read.
test("the Agents heading folds rows that are exactly the person's pinned agents @phone", async ({
  browser,
}) => {
  await setLiveSessions(railSessions());
  await seedUnreadReplies(ALPHA, 2);
  await putAgentState(ALPHA, { pinned: true });
  await putAgentState(BETA, { pinned: true });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/");
    await openRail(page);

    await expectRailAgents(page, [ALPHA, BETA]);
    // The session nobody pinned is listed on the Agents page and in no rail row.
    await expect(railAgentRow(page, GAMMA)).toHaveCount(0);

    const alphaRow = railAgentRow(page, ALPHA);
    await expect(alphaRow.locator("[data-agent-name]")).toHaveAttribute(
      "data-agent-state",
      "running"
    );
    await expect(alphaRow.getByRole("img", { exact: true, name: "Running" })).toBeVisible();
    await expect(alphaRow.getByRole("link", { exact: true, name: "Alpha" })).toBeVisible();
    await expect(alphaRow.getByTitle("New replies 2")).toHaveText("2");
    await expect(
      alphaRow.getByRole("button", { exact: true, name: "Unpin Alpha from the sidebar" })
    ).toBeVisible();
    // A row with nothing unread wears no badge at all, rather than a zero.
    await expect(railAgentRow(page, BETA).getByTitle(/^New repl/)).toHaveCount(0);

    // Below 1280 pixels the rail is the sheet the Menu control opened, and these are its rows.
    if (await railIsSheet(page)) {
      await expect(
        page.getByRole("dialog", { name: "Navigation" }).locator(`#${RAIL_AGENTS_ID} > li`)
      ).toHaveCount(2);
    }

    const fold = railAgentsFold(page);
    await expect(fold).toHaveAttribute("aria-expanded", "true");
    await expect(fold).toHaveAttribute("aria-controls", RAIL_AGENTS_ID);
    await fold.click();
    await expect(railAgents(page)).toHaveCount(0);
    await expect(page.getByRole("link", { name: "Agents" })).toHaveAttribute("href", "/agents");

    await fold.click();
    await expect(fold).toHaveAttribute("aria-expanded", "true");
    await expectRailAgents(page, [ALPHA, BETA]);
  } finally {
    await context.close();
  }
});

// The pin is one thing wherever it is pressed: the Agents page's row and the live view's header
// write the same pin, and the rail lists both at once without a reload of its own.
// SLOW(>=2s): the Agents page, one live view and the rail, each read after a pin of its own.
test("a pin from the Agents page and a pin from a live view are both under the heading @phone", async ({
  browser,
}) => {
  await setLiveSessions(railSessions());
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/agents");
    await openRail(page);
    await expect(railAgents(page)).toHaveText(NO_PINS_TEXT);
    await closeRail(page);

    await agentsPagePin(page, ALPHA, "Pin Alpha").click();
    await expect(agentsPagePin(page, ALPHA, "Unpin Alpha")).toBeVisible();
    await openRail(page);
    await expectRailAgents(page, [ALPHA]);
    await closeRail(page);

    await openLiveViewFromAgentsPage(page, BETA, "Beta");
    await liveViewPin(page, "Pin Beta").click();
    await expect(liveViewPin(page, "Unpin Beta")).toBeVisible();

    await openRail(page);
    await expectRailAgents(page, [ALPHA, BETA]);
  } finally {
    await context.close();
  }
});

// Three places unpin, and each leaves the rail with one row fewer: the rail row's own control,
// the Agents page's row, and the live view's header.
// SLOW(>=2s): three unpins, each from another surface, with a live view opened for the third.
test("the rail row, the Agents row and the live view each unpin @phone", async ({ browser }) => {
  await setLiveSessions(railSessions());
  await putAgentState(ALPHA, { pinned: true });
  await putAgentState(BETA, { pinned: true });
  await putAgentState(GAMMA, { pinned: true });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/agents");
    await openRail(page);
    await expectRailAgents(page, [ALPHA, BETA, GAMMA]);

    // From the rail row itself.
    await railAgentRow(page, ALPHA)
      .getByRole("button", { exact: true, name: "Unpin Alpha from the sidebar" })
      .click();
    await expect(railAgentRow(page, ALPHA)).toHaveCount(0);
    await closeRail(page);
    await expect(agentsPagePin(page, ALPHA, "Pin Alpha")).toBeVisible();

    // From the Agents page's row.
    await agentsPagePin(page, BETA, "Unpin Beta").click();
    await expect(agentsPagePin(page, BETA, "Pin Beta")).toBeVisible();
    await openRail(page);
    await expectRailAgents(page, [GAMMA]);
    await closeRail(page);

    // From the live view's header.
    await openLiveViewFromAgentsPage(page, GAMMA, "Gamma");
    await liveViewPin(page, "Unpin Gamma").click();
    await expect(liveViewPin(page, "Pin Gamma")).toBeVisible();
    await openRail(page);
    await expect(railAgents(page)).toHaveText(NO_PINS_TEXT);
  } finally {
    await context.close();
  }
});

// A pin belongs to the person, so their other browser shows it from the event the write
// announces (`user_agent_state.updated`), with nothing reloaded there.
// SLOW(>=2s): two browser contexts of one person, each signing in and loading the shell.
test("a second browser context of the same person shows the pin without a reload", async ({
  browser,
}) => {
  await setLiveSessions(railSessions());
  const first = await asUser(browser, "alice");
  const second = await asUser(browser, "alice");
  try {
    const pinning = await first.newPage();
    const watching = await second.newPage();
    await watching.goto("/");
    await expect(railAgents(watching)).toHaveText(NO_PINS_TEXT);

    await pinning.goto("/agents");
    await agentsPagePin(pinning, ALPHA, "Pin Alpha").click();
    await expect(agentsPagePin(pinning, ALPHA, "Unpin Alpha")).toBeVisible();

    // The other browser was never reloaded and never navigated.
    await expect(railAgentRow(watching, ALPHA)).toBeVisible();
    await expectRailAgents(watching, [ALPHA]);

    await agentsPagePin(pinning, ALPHA, "Unpin Alpha").click();
    await expect(railAgents(watching)).toHaveText(NO_PINS_TEXT);
  } finally {
    await Promise.all([first.close(), second.close()]);
  }
});

// The agents this browser kept for itself under `dispatch.agents.pinned:<login>` become the
// person's own pins at the first load after the change, and the key is gone.
test("pins kept in local storage are in the rail after the first load, and the key is gone", async ({
  browser,
}) => {
  await setLiveSessions(railSessions());
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.addInitScript(
      ({ ids, key }) => {
        window.localStorage.setItem(key, JSON.stringify(ids));
      },
      { ids: [ALPHA, BETA], key: "dispatch.agents.pinned:alice" }
    );
    await page.goto("/");

    await expect(railAgentRows(page)).toHaveCount(2);
    expect([...(await railAgentIds(page))].sort()).toEqual([ALPHA, BETA]);
    await expect
      .poll(() => page.evaluate(() => window.localStorage.getItem("dispatch.agents.pinned:alice")))
      .toBeNull();
    // The person's, not this browser's: Dispatch holds both pins.
    const states = await getAgentStates();
    expect(states[ALPHA]?.pinned_at).toBeDefined();
    expect(states[BETA]?.pinned_at).toBeDefined();
  } finally {
    await context.close();
  }
});

// Nothing pinned is still a heading, still the way to the Agents page, with one line under it
// saying where the pin is.
test("a person with no pins sees the Agents heading and one line saying where the pin is", async ({
  browser,
}) => {
  await setLiveSessions(railSessions());
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/");
    await expect(railAgents(page)).toHaveText(NO_PINS_TEXT);
    await expect(page.locator(`#${RAIL_AGENTS_ID}`)).toHaveJSProperty("tagName", "P");

    await page.getByRole("link", { name: "Agents" }).click();
    await expect(page.getByRole("heading", { level: 1, name: "Agents" })).toBeVisible();
    await expect(railAgents(page)).toHaveText(NO_PINS_TEXT);
  } finally {
    await context.close();
  }
});

// The rail's order is the order the pins were made in, oldest first (the plan's gap row for k1):
// three pins made in the reverse of both the ids' order and the titles', read top to bottom, and
// a pin made again goes to the end.
// SLOW(>=2s): five pin writes, each a round trip of its own, read back through the rail.
test("the rail lists the pinned agents oldest pin first", async ({ browser }) => {
  await setLiveSessions(railSessions());
  await putAgentState(GAMMA, { pinned: true });
  await putAgentState(BETA, { pinned: true });
  await putAgentState(ALPHA, { pinned: true });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/");
    await expectRailAgents(page, [GAMMA, BETA, ALPHA]);

    // Unpinned and pinned again, the oldest pin is now the newest and goes last.
    await putAgentState(GAMMA, { pinned: false });
    await expect(railAgentRow(page, GAMMA)).toHaveCount(0);
    await putAgentState(GAMMA, { pinned: true });
    await expect(railAgentRow(page, GAMMA)).toHaveCount(1);
    await expectRailAgents(page, [BETA, ALPHA, GAMMA]);
  } finally {
    await context.close();
  }
});
