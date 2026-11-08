import { expect, type Locator, type Page, test } from "@playwright/test";

import { type FakeSession, setLiveSessions } from "./agents";
import { publishAgentStreamFrame } from "./api";
import { frontLiveView } from "./live-views";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/**
 * How full a session's context window is, drawn at the right edge of the message box (INFRA-812,
 * R3; the browser cases of the Verification document's k3,
 * dispatch://INFRA-812/artifact/verification-md).
 *
 * The figures are the session's own: one `context` frame carrying the tokens it holds and the
 * window it holds them in, and the share between them is the viewer's to draw. A session that
 * published no such frame, and one whose host reports a window of zero, have no share, and the
 * row under the message box draws nothing rather than a ring at nought percent — a figure the
 * session never gave.
 *
 * The frames are spelt here as the wire carries them, never imported from the SPA or the
 * contracts, so this file reads the same against the commit before the change and each row
 * fails there on its own assertion.
 */

/** The sessions these rows read, by id. Each is a UUID, which Dispatch's agent routes require,
 *  and each is this file's own: the relay's replay is process-global, so a session another spec
 *  seeded would carry that spec's frames into these rows. */
const RING_ID = "01a0e812-0003-7000-8000-000000000001";
const ZERO_ID = "01a0e812-0003-7000-8000-000000000002";
const SILENT_ID = "01a0e812-0003-7000-8000-000000000003";
const PHONE_ID = "01a0e812-0003-7000-8000-000000000004";

/** A live session for a row to open. `last_seen` is taken when the row seeds, never at this
 *  module's scope, which a worker evaluates once and every later spec of that worker ages. */
function session(sessionID: string, title: string, dir = "/workspaces/context"): FakeSession {
  return {
    capabilities: ["aside", "btw", "steer"],
    dir,
    last_seen: Date.now() - 5_000,
    machine_id: "context-host",
    roles: [],
    session_id: sessionID,
    title,
  };
}

/** The session's `context` frame: the two figures and nothing else. */
function contextFrame(seq: number, tokens: number, contextWindow: number): Record<string, unknown> {
  return { contextWindow, kind: "context", seq, tokens, v: 1 };
}

/** One settled turn, so the view opens on a conversation rather than on its empty line. */
function turnFrame(seq: number): Record<string, unknown> {
  return {
    kind: "message",
    message: {
      at: 1_000,
      id: "a1000",
      parts: [{ text: "Reading the release notes.", type: "text" }],
      role: "assistant",
      streaming: false,
    },
    seq,
    v: 1,
  };
}

/** What the session answers the next viewer's replay request with. */
function replayOf(sessionID: string, frames: Record<string, unknown>[]): Record<string, unknown> {
  return { frames, session_id: sessionID, v: 1 };
}

function livePath(sessionID: string): string {
  return `/agents/${sessionID}/live`;
}

/** The ring in the row under the message box of the view the reader is looking at: a page-wide
 *  locator would also match the view lingering behind it. */
function contextRing(page: Page): Locator {
  return frontLiveView(page).getByTestId("agent-context-ring");
}

interface Edges {
  left: number;
  right: number;
}

/** One element's left and right edges on screen, rounded to whole pixels. */
async function edgesOf(locator: Locator): Promise<Edges> {
  const box = await locator.boundingBox();
  expect(box).not.toBeNull();
  if (box === null) throw new Error("the element is not on the page");
  return { left: Math.round(box.x), right: Math.round(box.x + box.width) };
}

test.beforeEach(async () => {
  await resetDatabase();
});

// R3. The share is the session's figures and nothing else: 68,000 tokens of a 200,000-token
// window is 34 percent, and the panel behind the ring says which two figures that share came
// from. A second frame moves both where they stand - the same page, no reload, and the panel
// still open in front of the reader.
// SLOW(>=2s): a live view opened on the session's replay, the panel opened, and a second frame
// waited on as it reaches the page the reader is already looking at.
test("the context ring draws the session's share, and its panel the tokens and the window", async ({
  browser,
}) => {
  await setLiveSessions([session(RING_ID, "Ring")]);
  await publishAgentStreamFrame(
    RING_ID,
    replayOf(RING_ID, [turnFrame(1), contextFrame(2, 68_000, 200_000)]),
    "replay"
  );
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto(livePath(RING_ID));
    const ring = contextRing(page);
    await expect(ring).toBeVisible();
    await expect(ring).toContainText("34%");

    // At the right edge of the message box: past every fact in the row, and inside the box's
    // own right edge, which the question mark beside the ring shares.
    const box = await edgesOf(frontLiveView(page).getByTestId("agent-composer"));
    const facts = await edgesOf(frontLiveView(page).getByTestId("agent-composer-facts"));
    const drawn = await edgesOf(ring);
    expect(drawn.left).toBeGreaterThanOrEqual(facts.right);
    expect(drawn.right).toBeLessThanOrEqual(box.right);

    await ring.click();
    const panel = frontLiveView(page).getByTestId("agent-context-popover");
    await expect(panel).toContainText("68,000 of 200,000 tokens");
    await expect(panel).toContainText("34%");

    // The session fills more of the same window while the reader is looking at it.
    await publishAgentStreamFrame(RING_ID, contextFrame(3, 120_000, 200_000));
    await expect(ring).toContainText("60%");
    await expect(panel).toContainText("120,000 of 200,000 tokens");
    await expect(panel).toContainText("60%");
    expect(page.url()).toContain(livePath(RING_ID));
  } finally {
    await context.close();
  }
});

// R3. No ring at all where there is no share to draw: a host that reports a window of zero, and
// a session that has published no `context` frame. Each row then publishes a window the session
// really has, so the absence is read as the view's decision rather than as a view that draws no
// ring whatever it is told.
// SLOW(>=2s): two live views, each opened on its own session's replay and then waited on for a
// second frame to reach the open page.
test("a window of zero and a session that published no context frame draw no ring", async ({
  browser,
}) => {
  await setLiveSessions([session(ZERO_ID, "Zero"), session(SILENT_ID, "Silent")]);
  await publishAgentStreamFrame(
    ZERO_ID,
    replayOf(ZERO_ID, [turnFrame(1), contextFrame(2, 68_000, 0)]),
    "replay"
  );
  await publishAgentStreamFrame(SILENT_ID, replayOf(SILENT_ID, [turnFrame(1)]), "replay");
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    for (const [sessionID, seq] of [
      [ZERO_ID, 3],
      [SILENT_ID, 2],
    ] as const) {
      await page.goto(livePath(sessionID));
      const footer = frontLiveView(page).getByTestId("agent-composer-footer");
      await expect(footer).toBeVisible();
      await expect(contextRing(page)).toHaveCount(0);
      await expect(footer).not.toContainText("%");

      // The same view, the same page: a window above zero is a share, and the ring is drawn.
      await publishAgentStreamFrame(sessionID, contextFrame(seq, 68_000, 200_000));
      await expect(contextRing(page)).toContainText("34%");
    }
  } finally {
    await context.close();
  }
});

// R3. At a phone width the facts under the message box scroll sideways and the ring does not:
// it keeps the box's right edge, where a reader's thumb finds it, while the delivery mode, the
// machine and a long directory run under it. The page itself never scrolls sideways.
// SLOW(>=2s): a live view opened on the session's replay at a phone width, then the row under
// the message box scrolled and measured before and after.
test("at a phone width the ring keeps the right edge of the box while the facts scroll @phone", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name !== "iphone",
    "the row under the message box scrolls sideways only below sm"
  );
  await setLiveSessions([
    session(PHONE_ID, "Phone", "/workspaces/a-directory-long-enough-to-run-off-a-phone-screen"),
  ]);
  await publishAgentStreamFrame(
    PHONE_ID,
    replayOf(PHONE_ID, [turnFrame(1), contextFrame(2, 68_000, 200_000)]),
    "replay"
  );
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto(livePath(PHONE_ID));
    const ring = contextRing(page);
    await expect(ring).toContainText("34%");

    const facts = frontLiveView(page).getByTestId("agent-composer-facts");
    const scroll = await facts.evaluate((row) => ({
      clientWidth: row.clientWidth,
      scrollWidth: row.scrollWidth,
    }));
    // The facts really are wider than the screen leaves them, so scrolling them means something.
    expect(scroll.scrollWidth).toBeGreaterThan(scroll.clientWidth);

    const box = await edgesOf(frontLiveView(page).getByTestId("agent-composer"));
    const before = await edgesOf(ring);
    expect(before.right).toBeLessThanOrEqual(box.right);
    expect(before.left).toBeGreaterThan(box.left + (box.right - box.left) / 2);

    await facts.evaluate((row) => {
      row.scrollTo(row.scrollWidth, 0);
    });
    await expect.poll(() => facts.evaluate((row) => Math.round(row.scrollLeft))).toBeGreaterThan(0);

    // The facts moved under it; the ring is where it was.
    expect(await edgesOf(ring)).toEqual(before);
    await expect(ring).toContainText("34%");

    const widths = await page.evaluate(() => ({
      clientWidth: document.documentElement.clientWidth,
      scrollWidth: document.documentElement.scrollWidth,
    }));
    expect(widths.clientWidth).toBe(390);
    expect(widths.scrollWidth).toBe(widths.clientWidth);
  } finally {
    await context.close();
  }
});
