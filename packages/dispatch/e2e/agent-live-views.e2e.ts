import { expect, type Locator, type Page, test } from "@playwright/test";

import { type FakeSession, setLiveSessions } from "./agents";
import {
  createAgentMessage,
  publishAgentStreamFrame,
  putAgentState,
  replyToMessageDelivery,
} from "./api";
import { FRONT_LIVE_VIEW, frontLiveView, readLiveViews } from "./live-views";
import {
  closeRail,
  expectRailAgents,
  expectRailViewState,
  openAgentFromRail,
  openInboxFromRail,
  openRail,
  railAgentRow,
} from "./rail";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/**
 * The live views a person has open (INFRA-812, R2; the browser cases of the Verification
 * document's k2, dispatch://INFRA-812/artifact/verification-md).
 *
 * A person moves between their pinned agents from the rail, and nothing they were doing is
 * closed or lost: the view they left keeps its unsent draft, its place in the transcript and the
 * unread count it opened with. One view is in front and live, the one left last lingers, and
 * every other sleeps, holding no subscription and drawing nothing; a view that slept comes back
 * from the session's own replay. The two gap rows of the plan are here too: Back walks the pages
 * the reader came from rather than the agents they looked at, and a live view hard-loaded for an
 * agent nobody pinned is a page like any other.
 *
 * Every read of what a view is doing is the page's own report (`e2e/live-views.ts`) or the word
 * its rail row shows; the suite never counts frames to guess at it.
 */

/** The eight sessions these rows pin, by id. Each is a UUID: Dispatch's agent routes refuse
 *  anything else, so a session named otherwise would list and have no live view. */
const RIG_IDS = [
  "01a0e300-4848-7473-acc5-000000000001",
  "01a0e300-4848-7473-acc5-000000000002",
  "01a0e300-4848-7473-acc5-000000000003",
  "01a0e300-4848-7473-acc5-000000000004",
  "01a0e300-4848-7473-acc5-000000000005",
  "01a0e300-4848-7473-acc5-000000000006",
  "01a0e300-4848-7473-acc5-000000000007",
  "01a0e300-4848-7473-acc5-000000000008",
];

/** The transcript every rig's replay carries, long enough that the thread scrolls at any
 *  viewport: each turn names the session it belongs to, so a view that came back with another
 *  session's conversation would be read as the failure it is. */
const TURNS = 40;

/** A live session for the rail to pin. `last_seen` is taken when the row seeds, never at this
 *  module's scope, which a worker evaluates once and every later spec of that worker ages. */
function rig(index: number): FakeSession {
  return {
    capabilities: ["aside", "btw", "steer"],
    dir: `/workspaces/rig-${index + 1}`,
    last_seen: Date.now() - 5_000,
    machine_id: "view-host",
    roles: [],
    session_id: RIG_IDS[index],
    title: `Rig ${index + 1}`,
  };
}

/**
 * What a session answers the next viewer's replay request with: `TURNS` turns of prose naming
 * this rig, and, where `context` says so, the newest `context` frame the session published. The
 * hook keeps the answer, so every subscription of every waking view is answered with it again,
 * which is what a sleeping view comes back from.
 */
function seedTranscript(index: number, context?: { contextWindow: number; tokens: number }) {
  const frames: object[] = Array.from({ length: TURNS }, (_, turn) => ({
    kind: "message",
    message: {
      at: 1_000 + turn * 1_000,
      id: `m${turn}`,
      parts: [
        {
          text: `Rig ${index + 1} turn ${turn}: ${"the relay carries this turn to the viewer. ".repeat(4)}`,
          type: "text",
        },
      ],
      role: turn % 2 === 0 ? "user" : "assistant",
      streaming: false,
    },
    seq: turn + 1,
    v: 1,
  }));
  if (context !== undefined) {
    frames.push({ ...context, kind: "context", seq: TURNS + 1, v: 1 });
  }
  return publishAgentStreamFrame(
    RIG_IDS[index],
    { frames, session_id: RIG_IDS[index], v: 1 },
    "replay"
  );
}

/** Seeds `count` rigs, live and pinned in order, each with a transcript to come back to. */
async function seedPinnedRigs(count: number): Promise<void> {
  await setLiveSessions(Array.from({ length: count }, (_, index) => rig(index)));
  for (let index = 0; index < count; index += 1) {
    await seedTranscript(index);
    // One at a time: the rail's order is the order the pins were made in.
    await putAgentState(RIG_IDS[index], { pinned: true });
  }
}

/** One of `sessionId`'s replies to a message the viewer sent it, which counts unread until a
 *  view the viewer is looking at shows it. */
async function seedUnreadReply(sessionId: string, body: string): Promise<void> {
  const asked = await createAgentMessage(sessionId, { body: "How far along?", delivery: "aside" });
  await replyToMessageDelivery(asked.id, { attempt: 1, body }, { id: sessionId, kind: "session" });
}

/** The front view's transcript, which is the one the reader is looking at: a page-wide locator
 *  would also match the view lingering behind it. */
function frontThread(page: Page): Locator {
  return frontLiveView(page).getByTestId("agent-thread");
}

/** The front view's message box. */
function frontComposer(page: Page): Locator {
  return frontLiveView(page).getByTestId("agent-composer").locator("textarea");
}

/** Waits until `sessionId`'s view is the one in front with its conversation drawn. */
async function expectInFront(page: Page, index: number): Promise<void> {
  await expect(frontLiveView(page)).toHaveAttribute("data-session-id", RIG_IDS[index]);
  await expect(frontThread(page)).toContainText(`Rig ${index + 1} turn 0:`);
}

/** Which turn the reader is looking at and where it sits on their screen: the topmost turn the
 *  front view's thread shows any part of, by the key it carries, with its top edge relative to
 *  the thread's. That pair is what "the page left me where I was" means to a reader. */
function topTurn(page: Page): Promise<{ id: string; top: number }> {
  return page.evaluate((front) => {
    const thread = document.querySelector<HTMLElement>(`${front} [data-testid="agent-thread"]`);
    if (thread === null) throw new Error("no thread in the view in front");
    const edge = thread.getBoundingClientRect().top;
    for (const turn of thread.querySelectorAll<HTMLElement>("[data-message-id]")) {
      const box = turn.getBoundingClientRect();
      if (box.bottom > edge && box.top < edge + thread.clientHeight) {
        return { id: turn.getAttribute("data-message-id") ?? "", top: Math.round(box.top - edge) };
      }
    }
    throw new Error("no turn in view");
  }, FRONT_LIVE_VIEW);
}

/** Scrolls the front view's thread the way a reader does, and waits for the scroll event to be
 *  delivered: the thread follows its end only once that event has told it where the reader is. */
async function scrollThreadTo(page: Page, top: number): Promise<void> {
  await frontThread(page).evaluate((thread, to) => {
    const scrolled = Promise.withResolvers<void>();
    if (Math.abs(thread.scrollTop - to) < 1) {
      scrolled.resolve();
      return scrolled.promise;
    }
    thread.addEventListener("scroll", () => scrolled.resolve(), { once: true });
    thread.scrollTo(0, to);
    return scrolled.promise;
  }, top);
}

test.beforeEach(async () => {
  await resetDatabase();
});

// What a person loses by looking at another agent: nothing. The draft they were writing, the
// place they had scrolled back to and the way back to the live end are the view's own, kept
// while two other agents and the Inbox were in front of them - which is longer than the view
// stays mounted, so the first view slept and came back from the session's replay.
// SLOW(>=2s): five page views of one row - three live views, the Inbox, and the first view
// again - each rebuilding a forty-turn transcript from the session's replay.
test("a pinned agent comes back with its draft, its place and its Jump to latest @phone", async ({
  browser,
}) => {
  await seedPinnedRigs(3);
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/");
    await openAgentFromRail(page, RIG_IDS[0]);
    await expectInFront(page, 0);

    await frontComposer(page).fill("the draft that outlives the view");
    await scrollThreadTo(page, 1_200);
    const place = await topTurn(page);
    await expect(frontLiveView(page).getByTestId("jump-to-latest")).toBeVisible();

    await openAgentFromRail(page, RIG_IDS[1]);
    await expectInFront(page, 1);
    await openAgentFromRail(page, RIG_IDS[2]);
    await expectInFront(page, 2);
    // Three views in, the first is asleep: it comes back from the session, not from the DOM.
    expect((await readLiveViews(page)).views[RIG_IDS[0]]?.state).toBe("asleep");
    await openInboxFromRail(page);

    await openAgentFromRail(page, RIG_IDS[0]);
    await expectInFront(page, 0);
    await expect(frontComposer(page)).toHaveValue("the draft that outlives the view");
    await expect.poll(() => topTurn(page)).toMatchObject({ id: place.id });
    expect(Math.abs((await topTurn(page)).top - place.top)).toBeLessThanOrEqual(2);
    await expect(frontLiveView(page).getByTestId("jump-to-latest")).toBeVisible();
  } finally {
    await context.close();
  }
});

// Eight pinned agents opened one after another: every one of them opens, none is refused, and
// none of the eight is closed behind the reader - each still has its entry in the page's own
// report of its live views when the last one is in front.
// SLOW(>=2s): eight live views opened in turn, each rebuilding a forty-turn transcript.
test("opening all eight pinned agents in turn closes none and refuses none", async ({
  browser,
}) => {
  await seedPinnedRigs(8);
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/");
    await expectRailAgents(page, RIG_IDS);

    for (let index = 0; index < RIG_IDS.length; index += 1) {
      await openAgentFromRail(page, RIG_IDS[index]);
      await expectInFront(page, index);
      // Nothing was refused on the way: the view that opened is reaching its session.
      await expect(page.getByTestId("agent-stream-unavailable")).toHaveCount(0);
    }

    // Nothing was closed: the rail still lists eight, and the page still holds an entry for
    // every one of them - the two mounted and the six asleep.
    await expectRailAgents(page, RIG_IDS);
    const views = (await readLiveViews(page)).views;
    expect(Object.keys(views).sort()).toEqual([...RIG_IDS].sort());
    expect(views[RIG_IDS[7]]?.state).toBe("live");
    expect(views[RIG_IDS[6]]?.state).toBe("live 30s");
    for (const asleep of RIG_IDS.slice(0, 6)) {
      expect(views[asleep]?.state).toBe("asleep");
    }
  } finally {
    await context.close();
  }
});

// Which view is which, in the rail's own words: the one in front is live, the one left last
// lingers out the watch window, and everything else sleeps. The third agent puts the first to
// sleep at once - the page says so before any assertion has waited for it.
// SLOW(>=2s): three live views opened in turn, each rebuilding a forty-turn transcript.
test("the rail reads live, live 30s and asleep for the views the person has open @phone", async ({
  browser,
}) => {
  await seedPinnedRigs(3);
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/");
    await openAgentFromRail(page, RIG_IDS[0]);
    await expectInFront(page, 0);
    await openRail(page);
    await expectRailViewState(page, RIG_IDS[0], "live");
    await expectRailViewState(page, RIG_IDS[1], "asleep");
    await closeRail(page);

    await openAgentFromRail(page, RIG_IDS[1]);
    await expectInFront(page, 1);
    await openRail(page);
    await expectRailViewState(page, RIG_IDS[1], "live");
    await expectRailViewState(page, RIG_IDS[0], "live 30s");
    await closeRail(page);

    await openAgentFromRail(page, RIG_IDS[2]);
    await expectInFront(page, 2);
    // At once, with nothing waited for: the page's own report, read the moment the third view
    // is in front, already has the first asleep rather than lingering a second time.
    const views = (await readLiveViews(page)).views;
    expect(views[RIG_IDS[0]]?.state).toBe("asleep");

    await openRail(page);
    await expectRailViewState(page, RIG_IDS[2], "live");
    await expectRailViewState(page, RIG_IDS[1], "live 30s");
    await expectRailViewState(page, RIG_IDS[0], "asleep");
  } finally {
    await context.close();
  }
});

// A view that slept holds nothing on the page - no conversation drawn at all - and coming back
// to it is the session's replay again: its own conversation, and the page saying it is live
// once more, in the view's header, in the rail's word for it and in its own report.
// SLOW(>=2s): four live views opened in turn, each rebuilding a forty-turn transcript.
test("a view that slept comes back with its conversation and says that it came back", async ({
  browser,
}) => {
  await seedPinnedRigs(3);
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/");
    await openAgentFromRail(page, RIG_IDS[0]);
    await expectInFront(page, 0);
    await openAgentFromRail(page, RIG_IDS[1]);
    await openAgentFromRail(page, RIG_IDS[2]);
    await expectInFront(page, 2);

    // Asleep is nothing of it on the page: no view element of its own, drawn or hidden.
    await expect(page.locator(`[data-session-id="${RIG_IDS[0]}"]`)).toHaveCount(0);
    expect((await readLiveViews(page)).views[RIG_IDS[0]]?.state).toBe("asleep");

    await openAgentFromRail(page, RIG_IDS[0]);
    await expectInFront(page, 0);
    await expect(frontThread(page)).toContainText(`Rig 1 turn ${TURNS - 1}:`);
    await expect(
      frontLiveView(page).getByTitle("Connection to the session's conversation: Live")
    ).toBeVisible();
    await expect
      .poll(async () => (await readLiveViews(page)).views[RIG_IDS[0]]?.state)
      .toBe("live");
    await openRail(page);
    await expectRailViewState(page, RIG_IDS[0], "live");
  } finally {
    await context.close();
  }
});

// A sleeping view costs its row nothing: the rail still names the agent, still says whether it
// is running, and still counts the replies the person has not read.
// SLOW(>=2s): a seeded reply and two live views opened to put the first one to sleep.
test("a sleeping row keeps its name, its running mark and its unread count", async ({
  browser,
}) => {
  await seedPinnedRigs(3);
  await seedUnreadReply(RIG_IDS[0], "Still building.");
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/");
    await openAgentFromRail(page, RIG_IDS[1]);
    await expectInFront(page, 1);
    await openAgentFromRail(page, RIG_IDS[2]);
    await expectInFront(page, 2);

    await openRail(page);
    await expectRailViewState(page, RIG_IDS[0], "asleep");
    const row = railAgentRow(page, RIG_IDS[0]);
    await expect(row.getByRole("link", { exact: true, name: "Rig 1" })).toBeVisible();
    await expect(row.getByRole("img", { exact: true, name: "Running" })).toBeVisible();
    await expect(row.getByTitle("New reply 1")).toHaveText("1");
  } finally {
    await context.close();
  }
});

// Reading is looking: only the view the person is in front of marks its replies read. A reply
// that arrives for the agent lingering behind them, or for one asleep, is still theirs to read,
// and the count stands until they open it.
// SLOW(>=2s): three live views opened, three replies seeded through Dispatch, and a fourth
// view opened to read one of them.
test("a reply for a lingering or sleeping agent leaves its count standing until it is opened", async ({
  browser,
}) => {
  await seedPinnedRigs(3);
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/");
    await openAgentFromRail(page, RIG_IDS[0]);
    await expectInFront(page, 0);
    await openAgentFromRail(page, RIG_IDS[1]);
    await expectInFront(page, 1);
    await openAgentFromRail(page, RIG_IDS[2]);
    await expectInFront(page, 2);

    // One reply each: for the agent in front, for the one lingering behind it, and for one
    // asleep.
    await seedUnreadReply(RIG_IDS[2], "Answering the one in front.");
    await seedUnreadReply(RIG_IDS[1], "Answering the one lingering.");
    await seedUnreadReply(RIG_IDS[0], "Answering the one asleep.");

    await openRail(page);
    await expect(railAgentRow(page, RIG_IDS[1]).getByTitle("New reply 1")).toHaveText("1");
    await expect(railAgentRow(page, RIG_IDS[0]).getByTitle("New reply 1")).toHaveText("1");
    // The agent in front has its replies marked read as it does today.
    await expect(railAgentRow(page, RIG_IDS[2]).getByTitle(/^New repl/)).toHaveCount(0);

    // Opening the sleeping one is reading it.
    await openAgentFromRail(page, RIG_IDS[0]);
    await expectInFront(page, 0);
    await expect(frontThread(page)).toContainText("Answering the one asleep.");
    await openRail(page);
    await expect(railAgentRow(page, RIG_IDS[0]).getByTitle(/^New repl/)).toHaveCount(0);
  } finally {
    await context.close();
  }
});

// The session's figures come back with the view: the ring a view drew from a `context` frame is
// there again when it wakes, from the session's own replay, with no frame published since.
// SLOW(>=2s): four live views opened in turn, each rebuilding a forty-turn transcript.
test("an agent whose view slept after a context frame shows its ring again when it wakes", async ({
  browser,
}) => {
  await seedPinnedRigs(3);
  await seedTranscript(0, { contextWindow: 200_000, tokens: 68_000 });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/");
    await openAgentFromRail(page, RIG_IDS[0]);
    await expectInFront(page, 0);
    await expect(frontLiveView(page).getByTestId("agent-context-share")).toContainText("34%");

    await openAgentFromRail(page, RIG_IDS[1]);
    await openAgentFromRail(page, RIG_IDS[2]);
    await expectInFront(page, 2);
    expect((await readLiveViews(page)).views[RIG_IDS[0]]?.state).toBe("asleep");

    // Nothing published between the sleep and the waking: the ring is drawn from what the
    // session answers the waking view's replay request with.
    await openAgentFromRail(page, RIG_IDS[0]);
    await expectInFront(page, 0);
    await expect(frontLiveView(page).getByTestId("agent-context-share")).toContainText("34%");
  } finally {
    await context.close();
  }
});

// A move between pinned agents from the rail is one step of the reader's, not a page of its
// own: Back leaves the live views for the page they came from (the plan's gap row for k2).
// SLOW(>=2s): two live views opened from the rail before the one Back this row reads.
test("Back walks the pages the reader came from, not the agents they looked at", async ({
  browser,
}) => {
  await seedPinnedRigs(3);
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/");
    await expect(page.getByRole("heading", { level: 1, name: "Inbox" })).toBeVisible();

    await openAgentFromRail(page, RIG_IDS[0]);
    await expectInFront(page, 0);
    await openAgentFromRail(page, RIG_IDS[1]);
    await expectInFront(page, 1);

    await page.goBack();
    await expect(page).toHaveURL(/\/$/);
    await expect(page.getByRole("heading", { level: 1, name: "Inbox" })).toBeVisible();
  } finally {
    await context.close();
  }
});

// A live view is a page like any other for an agent nobody pinned: hard-loaded, it draws its
// conversation and the row under its message box, and it is still there after the Inbox and
// back - while the rail, which lists pins alone, never names it (the plan's gap row for k2).
// SLOW(>=2s): a hard-loaded live view, the Inbox, and that view again.
test("a hard load of an unpinned agent's live view draws it, and the Inbox and back keeps it", async ({
  browser,
}) => {
  await setLiveSessions([rig(0), rig(1)]);
  await seedTranscript(1);
  await putAgentState(RIG_IDS[0], { pinned: true });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto(`/agents/${RIG_IDS[1]}/live`);
    await expectInFront(page, 1);
    const footer = frontLiveView(page).getByTestId("agent-composer-footer");
    await expect(footer.getByTestId("agent-footer-machine")).toHaveText("view-host");
    await expect(footer.getByTestId("agent-footer-dir")).toContainText("/workspaces/rig-2");
    // The rail lists the person's pins, and this session is not one of them.
    await expectRailAgents(page, [RIG_IDS[0]]);

    await openInboxFromRail(page);
    await expect(page.getByRole("heading", { level: 1, name: "Inbox" })).toBeVisible();
    await page.goBack();

    await expectInFront(page, 1);
    await expect(
      frontLiveView(page).getByTestId("agent-composer-footer").getByTestId("agent-footer-machine")
    ).toHaveText("view-host");
  } finally {
    await context.close();
  }
});
