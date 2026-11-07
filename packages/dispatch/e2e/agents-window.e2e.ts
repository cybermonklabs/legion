import { expect, type Page, test } from "@playwright/test";

import {
  agentGroupToggle,
  agentRow,
  type FakeSession,
  openAgents,
  setLiveSessions,
  shownAgentRows,
} from "./agents";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/**
 * The Agents page at the size it really is (INFRA-812, R7; the browser cases of the
 * Verification document's k12, dispatch://INFRA-812/artifact/verification-md).
 *
 * Six hundred sessions is the page a person opens, and the list draws the rows the screen holds
 * and twelve either side of them, with its box as tall as all six hundred so the scrollbar
 * measures the list rather than the part of it drawn. Scrolling moves that window: a row that
 * has scrolled out of reach is in no part of the page, and a row scrolled to is drawn when it
 * is reached.
 *
 * Two of the plan's gap rows are here, because both are about what the window may not cost: a
 * draft the reader typed survives the row scrolling out of the window and back, and `j` reaches
 * a session the window has not drawn, since the keys walk the page's own list of sessions and
 * not the rows in the DOM.
 *
 * What the first paint of this page costs in time is `agent-workspace-timing.e2e.ts`, which has
 * the machine to itself (D14); every claim here is untimed and runs beside the rest of the
 * suite.
 */

/** The six hundred sessions of R7's own measured case. */
const COUNT = 600;

/** Rows drawn past each edge of the window (`features/delivery/lib/rowWindow.ts`'s
 *  `OVERSCAN_ROWS`), which is what this file holds the page to. */
const OVERSCAN = 12;

/** How far down the list the scrolling rows go, far enough that the list's own top edge is well
 *  above the viewport: there the window's coordinates and the screen's are the same, so the
 *  overscan either side can be counted exactly. */
const SCROLL_TO = 4_000;

function sessionID(index: number): string {
  return `01a0e812-0c12-7000-8000-${String(index).padStart(12, "0")}`;
}

/** The session's own name, zero-padded so that the page's order - by title, since none of these
 *  is pinned, owed anything or heard from in Dispatch - is the order they were made in. */
function title(index: number): string {
  return `Worker ${String(index).padStart(3, "0")}`;
}

/** The six hundred live sessions. `last_seen` is taken when the row seeds, never at this
 *  module's scope, which a worker evaluates once and every later spec of that worker ages; with
 *  no Dispatch activity at all, every one of them is `Idle`. */
function workers(): FakeSession[] {
  const seen = Date.now() - 5_000;
  return Array.from({ length: COUNT }, (_, index) => ({
    capabilities: ["aside", "btw"],
    dir: `/workspaces/worker-${index}`,
    last_seen: seen,
    machine_id: "window-host",
    roles: [],
    session_id: sessionID(index),
    title: title(index),
  }));
}

interface WindowReading {
  /** Drawn items wholly above the screen's top edge: the window's overscan above. */
  above: number;
  /** Drawn items wholly below its bottom edge: the overscan below. */
  below: number;
  /** Every item the list drew, headings and rows together. */
  drawn: number;
  /** The first and last session the list drew, in its own order. */
  firstRow: string | null;
  lastRow: string | null;
  /** The list box's own height: what the scrollbar measures. */
  listHeight: number;
  /** Drawn items the screen holds, whole or in part. */
  onScreen: number;
  /** How far the document itself scrolls. */
  pageHeight: number;
}

/** What the page's list is drawing now, and where each drawn item sits against the screen. */
function readWindow(page: Page): Promise<WindowReading> {
  return page.evaluate(() => {
    const list = document.querySelector("[data-agents-list]");
    if (list === null) throw new Error("the agents list is not on the page");
    const items = [...list.querySelectorAll<HTMLElement>("[data-row-key]")];
    const boxes = items.map((item) => item.getBoundingClientRect());
    const rows = items.flatMap((item) => {
      const row = item.querySelector<HTMLElement>("[data-agent-row]");
      return row === null ? [] : [row.dataset.agentRow ?? ""];
    });
    const height = window.innerHeight;
    return {
      above: boxes.filter((box) => box.bottom <= 0).length,
      below: boxes.filter((box) => box.top >= height).length,
      drawn: items.length,
      firstRow: rows[0] ?? null,
      lastRow: rows.at(-1) ?? null,
      listHeight: Math.round(list.getBoundingClientRect().height),
      onScreen: boxes.filter((box) => box.bottom > 0 && box.top < height).length,
      pageHeight: document.documentElement.scrollHeight,
    };
  });
}

test.beforeEach(async () => {
  await resetDatabase();
});

// R7. The page a person really opens: six hundred sessions, of which the list draws the ones the
// screen holds and twelve either side, in a box as tall as all six hundred. Scrolling moves that
// window, and the rows it leaves behind are in no part of the page - which is what the count of
// rows a screen holds is read against, so this row runs at both widths: a phone holds fewer rows
// and the window is that many narrower.
// SLOW(>=2s): six hundred sessions seeded, one page load of them, then a scroll waited out
// until the window has moved and settled.
test("the page holds six hundred sessions and draws the screen's rows and twelve either side @phone", async ({
  browser,
}) => {
  await setLiveSessions(workers());
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto("/agents");
    // Every one of them is listed and counted, whatever the list draws.
    await expect(agentGroupToggle(page, "idle")).toHaveAccessibleName(`Idle (${COUNT})`);
    await expect(agentRow(page, sessionID(0))).toBeVisible();

    const atTop = await readWindow(page);
    // Only a handful of rows is drawn, and the last session is not among them.
    expect(atTop.drawn).toBeLessThan(COUNT / 4);
    expect(atTop.onScreen).toBeGreaterThan(2);
    await expect(agentRow(page, sessionID(COUNT - 1))).toHaveCount(0);
    // The scrollbar measures the whole list: its box is as tall as six hundred rows, and the
    // document scrolls that far.
    const drawnHeight = atTop.onScreen * 40;
    expect(atTop.listHeight).toBeGreaterThan(COUNT * 30);
    expect(atTop.listHeight).toBeGreaterThan(drawnHeight * 4);
    expect(atTop.pageHeight).toBeGreaterThanOrEqual(atTop.listHeight);

    // Scrolling moves the window. Below the list's own top edge the window's coordinates are the
    // screen's, so what it draws past each edge is exactly the overscan.
    await page.evaluate((to) => window.scrollTo(0, to), SCROLL_TO);
    await expect.poll(async () => (await readWindow(page)).above).toBe(OVERSCAN);
    const scrolled = await readWindow(page);
    expect(scrolled.below).toBe(OVERSCAN);
    expect(scrolled.drawn).toBe(scrolled.above + scrolled.onScreen + scrolled.below);
    // Still the whole list: the box grows a little as each drawn row replaces the estimate the
    // undrawn ones stand at, and never shrinks to the part of the list on screen.
    expect(scrolled.listHeight).toBeGreaterThanOrEqual(atTop.listHeight);
    expect(scrolled.listHeight).toBeLessThan(atTop.listHeight * 1.1);

    // The window has moved with the reader: the rows it drew at the top are gone, and the ones
    // it draws now are further down the list.
    expect(scrolled.firstRow).not.toBe(atTop.firstRow);
    await expect(agentRow(page, sessionID(0))).toHaveCount(0);
    await expect(shownAgentRows(page).first()).toBeVisible();
  } finally {
    await context.close();
  }
});

// R7, and the plan's gap row for what the window may not cost: a row the reader has opened is
// drawn wherever it has scrolled to, so the draft in its composer survives the scroll that takes
// it out of the window and the scroll back. A row nobody opened is dropped by that same scroll,
// which is what says the window really moved past it.
// SLOW(>=2s): six hundred sessions seeded, a row opened and typed in, then two scrolls waited
// out.
test("a draft survives a scroll that takes its row out of the window and back", async ({
  browser,
}) => {
  await setLiveSessions(workers());
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto("/agents");
    const opened = agentRow(page, sessionID(1));
    await expect(opened).toBeVisible();
    await opened.getByRole("button", { exact: true, name: title(1) }).click();
    const composer = opened.getByRole("textbox", { name: "Comment" });
    await composer.fill("Half a message, left here.");
    await expect(composer).toHaveValue("Half a message, left here.");

    await page.evaluate((to) => window.scrollTo(0, to), SCROLL_TO);
    // A row nobody opened, beside the opened one, is dropped: the window has moved past both.
    await expect(agentRow(page, sessionID(2))).toHaveCount(0);
    // The opened row is still mounted, with its draft, wherever it has scrolled to.
    await expect(opened).toHaveCount(1);
    await expect(composer).toHaveValue("Half a message, left here.");

    await page.evaluate(() => window.scrollTo(0, 0));
    await expect(agentRow(page, sessionID(2))).toBeVisible();
    await expect(opened).toBeVisible();
    await expect(composer).toHaveValue("Half a message, left here.");
  } finally {
    await context.close();
  }
});

// R7, and the plan's gap row for the keys over a windowed list: `j` and `k` walk the page's own
// list of sessions, not the rows in the DOM, so a key reaches a session the window has not
// drawn and the page draws it, scrolls to it and focuses it. Read from the end of the list,
// where the first session is as far out of the window as a session gets.
// SLOW(>=2s): six hundred sessions seeded, one page load, and a scroll to the end of the list
// waited out before the key is pressed.
test("j reaches a session the window has not drawn", async ({ browser }) => {
  await setLiveSessions(workers());
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await openAgents(page);
    const first = agentRow(page, sessionID(0));
    await expect(first).toBeVisible();

    // At the end of the list the first session is not drawn at all - not hidden, not scrolled
    // past: absent.
    await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
    await expect(first).toHaveCount(0);
    await expect(agentRow(page, sessionID(COUNT - 1))).toBeVisible();

    // One press of `j` from outside the list takes the reader to the first session of the page's
    // own list, which the window then draws, scrolls to and focuses.
    await page.keyboard.press("j");
    await expect(first).toBeFocused();
    await expect(first).toBeInViewport();
    await expect(first.getByRole("button", { exact: true, name: title(0) })).toBeVisible();
  } finally {
    await context.close();
  }
});
