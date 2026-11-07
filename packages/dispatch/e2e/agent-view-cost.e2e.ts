import { expect, test } from "@playwright/test";

import { publishAgentStreamFrame } from "./api";
import {
  COST_RIG_IDS,
  COST_RIG_TURNS,
  expectConversationInFront,
  frontTurns,
  seedPinnedRigsAtLimit,
} from "./live-view-cost";
import {
  drawnLiveViews,
  heapBytes,
  keepEveryViewAwake,
  openAgentSubscriptions,
  readLiveViews,
} from "./live-views";
import { openAgentFromRail } from "./rail";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/**
 * What a person's open agents cost the page (INFRA-812, R2; the untimed half of the
 * Verification document's k11, dispatch://INFRA-812/artifact/verification-md). Eight agents are
 * pinned, each seeded with a conversation at the stream's limit, and each is opened in turn
 * through the rail; what the page then says about itself is the check: how many agent
 * subscriptions its one live socket holds, how many conversations it has drawn, and what its
 * heap reads. At most two subscriptions and at most two conversations, whichever of the eight
 * the reader is on.
 *
 * The negative control is here too, as the check words it: the same case with the sleep rule
 * turned off by its test hook, where eight subscriptions stay open and eight conversations are
 * drawn - the two readings that make the row above fail.
 *
 * k11's one timed claim, that a sleeping view comes back inside half a second, is not here: it
 * runs in `chromium-alone`, with the machine to itself (D14, `e2e/agent-workspace-timing.e2e.ts`).
 *
 * The rig both halves seed from is `e2e/live-view-cost.ts`, and every reading is the page's own
 * report (`e2e/live-views.ts`) rather than anything this suite counts for itself.
 */

// `performance.memory` is a quantised constant in a Chromium launched without this switch: a
// page read the same 131,000,000 bytes with no view open and with eight, which is no reading of
// what a view costs. With it, the same page read 13.5 MB before any view and 213.6 MB with
// eight open. The row that reads the heap asks for the switch itself, as `heapBytes` says.
test.use({ launchOptions: { args: ["--enable-precise-memory-info"] } });

test.beforeEach(async () => {
  await resetDatabase();
});

/** One turn a session publishes to whoever is subscribed to it now, past the end of the replay
 *  it answers a new viewer with. */
function publishedTurn(sessionID: string, seq: number, text: string): Promise<{ ok: boolean }> {
  return publishAgentStreamFrame(sessionID, {
    kind: "message",
    message: {
      at: 1_000 + seq * 1_000,
      id: `published-${seq}`,
      parts: [{ text, type: "text" }],
      role: "assistant",
      streaming: false,
    },
    seq,
    v: 1,
  });
}

// k11's untimed half: eight agents at the stream's limit, opened one after another, and what
// the page holds at every step of that walk. The counts are read the moment each view is in
// front, so the bound is on every one of the eight opens rather than on the last.
// SLOW(>=2s): eight live views opened in turn, each rebuilding a two-hundred-turn transcript at
// the stream's limit.
test("eight agents opened in turn cost the page two live views, not eight", async ({ browser }) => {
  test.setTimeout(180_000);
  await seedPinnedRigsAtLimit();
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/");
    // The heap of the shell with its rail and no live view at all, which is what the eight
    // opens below are measured against.
    const heapBefore = await heapBytes(page);

    const subscriptions: number[] = [];
    const drawn: number[] = [];
    for (const sessionID of COST_RIG_IDS) {
      await openAgentFromRail(page, sessionID);
      await expectConversationInFront(page, sessionID);
      subscriptions.push(await openAgentSubscriptions(page));
      drawn.push(await drawnLiveViews(page).count());
    }
    const heapAfter = await heapBytes(page);

    test.info().annotations.push(
      { description: subscriptions.join(", "), type: "agent subscriptions after each open" },
      { description: drawn.join(", "), type: "conversations drawn after each open" },
      {
        description:
          heapBefore === null || heapAfter === null
            ? "this browser does not report performance.memory"
            : `${(heapBefore / 1e6).toFixed(1)} MB before any view, ` +
              `${(heapAfter / 1e6).toFixed(1)} MB with eight agents open`,
        type: "heap",
      }
    );

    expect(
      Math.max(...subscriptions),
      `the socket held ${subscriptions.join(", ")} agent subscriptions after the eight opens`
    ).toBeLessThanOrEqual(2);
    expect(
      Math.max(...drawn),
      `the page had drawn ${drawn.join(", ")} conversations after the eight opens`
    ).toBeLessThanOrEqual(2);
    // Eight were opened and none was closed: every one of them still has an entry, and the two
    // the shell mounts are the one in front and the one left last.
    const views = (await readLiveViews(page)).views;
    expect(Object.keys(views).sort()).toEqual([...COST_RIG_IDS].sort());
    expect(Object.values(views).filter((view) => view.state !== "asleep")).toHaveLength(2);
  } finally {
    await context.close();
  }
});

// The negative control the check names: the same eight agents, opened the same way, with the
// sleep rule turned off before the page loads. Every view the reader opened stays mounted, so
// the socket holds eight agent subscriptions and the page has drawn eight conversations - which
// is the row above failing its first two assertions, since neither reading is at most two.
// SLOW(>=2s): eight live views opened in turn and all eight kept, each holding a two-hundred-turn
// transcript at the stream's limit.
test("with the sleep rule off, the same eight agents cost the page eight live views", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  await seedPinnedRigsAtLimit();
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await keepEveryViewAwake(page);
    await page.goto("/");
    for (const sessionID of COST_RIG_IDS) {
      await openAgentFromRail(page, sessionID);
      await expectConversationInFront(page, sessionID);
    }

    const subscriptions = await openAgentSubscriptions(page);
    const drawn = await drawnLiveViews(page).count();
    test
      .info()
      .annotations.push(
        { description: `${subscriptions}`, type: "agent subscriptions with the sleep rule off" },
        { description: `${drawn}`, type: "conversations drawn with the sleep rule off" }
      );
    expect(subscriptions).toBe(COST_RIG_IDS.length);
    expect(drawn).toBe(COST_RIG_IDS.length);
    expect(subscriptions, "a reading k11's first assertion refuses").toBeGreaterThan(2);
    expect(drawn, "a reading k11's second assertion refuses").toBeGreaterThan(2);
  } finally {
    await context.close();
  }
});

// A sleeping view leaves no session publishing (the plan's gap row for k11). There is no HTTP
// read of the relay's state, so the page is what says so: the live socket's agent subscription
// is the only thing re-arming a session's watch (`live_agent.go` re-arms `source.Watch` on a
// ticker for as long as that viewer's subscription is open), and a frame published to a session
// no viewer is subscribed to reaches nobody and is kept nowhere. So the turn published while
// the view was in front arrives on the page, the turn published while it slept is on no page
// ever, and the view that comes back is the session's replay and nothing else.
// SLOW(>=2s): four live views opened in turn, each rebuilding a two-hundred-turn transcript at
// the stream's limit.
test("a sleeping view leaves no session publishing", async ({ browser }) => {
  test.setTimeout(180_000);
  await seedPinnedRigsAtLimit();
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/");
    await openAgentFromRail(page, COST_RIG_IDS[0]);
    await expectConversationInFront(page, COST_RIG_IDS[0]);

    // While the viewer is subscribed, the relay carries what the session publishes to it.
    await publishedTurn(COST_RIG_IDS[0], COST_RIG_TURNS + 1, "Published while a viewer watched.");
    await expect(frontTurns(page)).toHaveCount(COST_RIG_TURNS + 1);
    await expect(page.getByText("Published while a viewer watched.")).toBeVisible();

    // Two more agents put the first to sleep: nothing of it is mounted, so the socket holds no
    // subscription of its own for it and nothing re-arms the relay's watch.
    await openAgentFromRail(page, COST_RIG_IDS[1]);
    await expectConversationInFront(page, COST_RIG_IDS[1]);
    await openAgentFromRail(page, COST_RIG_IDS[2]);
    await expectConversationInFront(page, COST_RIG_IDS[2]);
    const asleep = await readLiveViews(page);
    expect(asleep.views[COST_RIG_IDS[0]]?.state).toBe("asleep");
    expect(asleep.agentSubscriptions).toBe(2);
    await expect(page.locator(`[data-session-id="${COST_RIG_IDS[0]}"]`)).toHaveCount(0);

    // Published to a session nobody is watching, this frame reaches nobody and nothing keeps
    // it: the session's replay is unchanged, and that is all a waking view is handed.
    await publishedTurn(COST_RIG_IDS[0], COST_RIG_TURNS + 2, "Published while nobody watched.");
    await openAgentFromRail(page, COST_RIG_IDS[0]);
    await expectConversationInFront(page, COST_RIG_IDS[0]);
    await expect(page.getByText("Published while nobody watched.")).toHaveCount(0);
    await expect(page.getByText("Published while a viewer watched.")).toHaveCount(0);
  } finally {
    await context.close();
  }
});
