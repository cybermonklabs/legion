import { expect, test } from "@playwright/test";

import { type FakeSession, setLiveSessions } from "./agents";
import { publishAgentStreamFrame } from "./api";
import { COST_RIG_IDS, expectConversationInFront, seedPinnedRigsAtLimit } from "./live-view-cost";
import { watchViewRestore } from "./live-views";
import { openAgentFromRail } from "./rail";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/**
 * What the agent surfaces cost in time (INFRA-812). Every row here reads a wall clock - a
 * sleeping live view at the stream's limit comes back inside 1.2 s, the Agents page paints six
 * hundred sessions inside 100 ms with no task longer than 50 - so the whole file runs in the
 * `chromium-alone` project, with the machine to itself (D14, `playwright.config.ts`'s
 * `clockSpecs` and `packages/dispatch/AGENTS.md`). Four lanes put four browsers, four Dispatch
 * servers and four Postgres servers on one machine, and a second of wall clock there holds
 * what the other three lanes took as surely as the work the row measured. The untimed half of
 * each of those checks runs in `chromium` and `iphone` as any other row does: the live view's
 * cost in `agent-view-cost.e2e.ts`, and the windowed list's in `agents-window.e2e.ts`.
 */

declare global {
  interface Window {
    /** Every task the browser called long since the Agents-page row below cleared this, which
     *  is how that row reads what the page held the main thread for while it drew. */
    __agentsPaint?: { longTasks: { duration: number; end: number; start: number }[] };
  }
}

/** The six hundred sessions of R7's own measured case, all `Idle`: no pin, no open ask and no
 *  Dispatch activity, which is the page a person opens. */
function workers(): FakeSession[] {
  const seen = Date.now() - 5_000;
  return Array.from({ length: 600 }, (_, index) => ({
    capabilities: ["aside", "btw"],
    dir: `/workspaces/worker-${index}`,
    last_seen: seen,
    machine_id: "window-host",
    roles: [],
    session_id: `01a0e812-0c12-7000-8000-${String(index).padStart(12, "0")}`,
    title: `Worker ${String(index).padStart(3, "0")}`,
  }));
}

const planner: FakeSession = {
  capabilities: ["aside", "btw", "steer"],
  dir: "/workspaces/planner",
  machine_id: "planner-host",
  roles: [],
  session_id: "01a0e0c1-0812-7000-8000-00000000d14a",
  title: "Planner",
};

/** The session's replay: one turn, which is what a painted view shows. */
const replay = {
  frames: [
    {
      kind: "message",
      message: {
        at: 1_000,
        id: "u1000",
        parts: [{ text: "Read the release notes", type: "text" }],
        role: "user",
        streaming: false,
      },
      seq: 1,
      v: 1,
    },
  ],
  session_id: planner.session_id,
  v: 1,
};

test.beforeEach(async () => {
  await resetDatabase();
  await setLiveSessions([planner]);
});

// No clock is read here: this row is what the timed ones measure, so a project that cannot open
// a live view at all fails on this rather than on a number.
// SLOW(>=2s): a cold live view, opened on the session's replay and waited for whole.
test("the live view opens on the session's transcript", async ({ browser }) => {
  await publishAgentStreamFrame(planner.session_id, replay, "replay");
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto(`/agents/${planner.session_id}/live`);
    await expect(page.getByTestId("agent-conversation")).toBeVisible();
    await expect(page.getByTestId("agent-message-user")).toContainText("Read the release notes");
  } finally {
    await context.close();
  }
});

// R2, k11's one timed claim: a sleeping view at the stream's worst case comes back inside 1.2
// seconds. It is the worst case on purpose - eight agents pinned, each seeded with a
// conversation at the stream's limit (`e2e/live-view-cost.ts`, the rig the untimed half of k11
// reads in `chromium`) - so the view this row brings back subscribes again, is answered with
// two hundred turns of the session's own replay, and draws them. The bound is the sum that wake
// pays: the re-subscribe at about 0.2 s, the replay read off the socket at 0.1 to 0.4 s and the
// render of a limit-sized transcript at 0.25 to 0.36 s, which is the largest part; measured 0.66
// to 0.82 s on an idle `chromium-alone` machine, with 1.2 s the bound over that measured worst
// with margin for run-to-run variance. An ordinary conversation, far below the limit, comes
// back well under half a second.
//
// The clock is the page's own (`watchViewRestore`, installed before the press): the first
// `pointerdown`, taken in the capture phase before any handler of the press runs, to the first
// mutation in which that session's view is in front with a turn drawn in it. Nothing of the
// driver is in the window - a Playwright click's trace snapshots either side of itself are the
// harness's work, not the page's - and the figure is kept on the result whether or not the row
// passes, with the three parts it divides into: the re-subscribe, the session's replay
// arriving, and the view drawing it.
// SLOW(>=2s): eight live views opened in turn, each rebuilding a two-hundred-turn transcript at
// the stream's limit, before the sleeping one is brought back.
test("a sleeping view at the stream's worst case comes back inside 1.2 seconds", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  await seedPinnedRigsAtLimit();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto("/");
    for (const sessionID of COST_RIG_IDS) {
      await openAgentFromRail(page, sessionID);
      await expectConversationInFront(page, sessionID);
    }
    // The first of the eight has been asleep since the third was opened: nothing of it is
    // mounted, it holds no subscription, and coming back to it is the session's replay again.
    await expect(page.locator(`[data-session-id="${COST_RIG_IDS[0]}"]`)).toHaveCount(0);

    const restored = await watchViewRestore(page, COST_RIG_IDS[0]);
    await openAgentFromRail(page, COST_RIG_IDS[0]);
    await expectConversationInFront(page, COST_RIG_IDS[0]);
    const restore = await restored();
    const part = (ms: number | null) => (ms === null ? "not measured" : `${Math.round(ms)} ms`);
    // Where the wait went, kept on the result whether or not the row passes: the three parts
    // are what calibrating this bound is argued from, and reading them after the fact costs a
    // run of eight heavy views.
    test.info().annotations.push(
      { description: `${Math.round(restore.totalMs)} ms`, type: "a sleeping view came back" },
      {
        description:
          `${part(restore.resubscribeMs)} to re-subscribe, ` +
          `${part(restore.replayMs)} to the session's replay, ` +
          `${part(restore.renderMs)} to draw it`,
        type: "where that time went",
      }
    );

    expect(
      restore.totalMs,
      `the sleeping view came back in ${Math.round(restore.totalMs)} ms`
    ).toBeLessThan(1_200);
  } finally {
    await context.close();
  }
});

// R7, k12's one timed claim: the Agents page of six hundred sessions paints inside 100
// milliseconds with no task longer than 50. The spec measured that on its prototype - drawing
// every row cost 177 to 374 ms with a longest task of up to 290, and the windowed list 23 to 40
// ms with no long task - so what is timed here is the same thing: the page's own work drawing
// its list of six hundred, with the whole list folded away and then drawn again. The app's cold
// boot is no part of it, since no requirement here changed what a cold boot costs: this pod
// takes about 140 ms to start the page whether it lists ten sessions or six hundred.
//
// The press and the wait both happen inside the page, in one `evaluate`: a Playwright click
// takes a DOM snapshot for the trace either side of itself (`trace: "retain-on-failure"`), and
// that snapshotting is the harness's work, not the page's - with the press driven from the
// browser it added 25 ms to this window and a task of its own over the 50 ms bar. A task longer
// than 50 ms is what the browser itself calls a long task, so the second half of the claim is
// that the page runs none while it draws. Both figures are kept on the result whether or not
// the row passes.
// SLOW(>=2s): six hundred sessions seeded, then a page that lists them all before the draw is
// timed.
test("the Agents page paints six hundred sessions inside 100 ms with no task longer than 50", async ({
  browser,
}) => {
  await setLiveSessions(workers());
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    // Before the page's own scripts, so every long task of the page is on the record.
    await page.addInitScript(() => {
      const paint: NonNullable<Window["__agentsPaint"]> = { longTasks: [] };
      window.__agentsPaint = paint;
      new PerformanceObserver((list) => {
        for (const entry of list.getEntries()) {
          paint.longTasks.push({
            duration: entry.duration,
            end: entry.startTime + entry.duration,
            start: entry.startTime,
          });
        }
      }).observe({ entryTypes: ["longtask"] });
    });

    await page.goto("/agents");
    // All six hundred are listed, and the window has drawn the part of them the screen holds.
    await expect(page.getByTestId("agents-group-count-idle")).toHaveText("(600)");
    await expect(page.locator("[data-agent-row]").first()).toBeVisible();
    await page.getByTestId("agents-group-toggle-idle").click();
    await expect(page.locator("[data-agent-row]")).toHaveCount(0);

    const measured = await page.evaluate(async () => {
      const paint = window.__agentsPaint;
      const toggle = document.querySelector<HTMLElement>(
        '[data-testid="agents-group-toggle-idle"]'
      );
      if (paint === undefined || toggle === null) throw new Error("the page is not ready");
      const rows = Promise.withResolvers<number>();
      const watch = new MutationObserver(() => {
        if (document.querySelector("[data-agent-row]") === null) return;
        watch.disconnect();
        rows.resolve(performance.now());
      });
      watch.observe(document.body, { childList: true, subtree: true });
      paint.longTasks = [];
      const startedAt = performance.now();
      toggle.click();
      const rowsAt = await rows.promise;
      return {
        drawn: document.querySelectorAll("[data-agent-row]").length,
        held: paint.longTasks.filter((task) => task.end > startedAt && task.start < rowsAt),
        paintMs: rowsAt - startedAt,
      };
    });
    const longest = Math.max(0, ...measured.held.map((task) => task.duration));
    test.info().annotations.push(
      {
        description: `${Math.round(measured.paintMs)} ms, ${measured.drawn} rows drawn of 600`,
        type: "600 sessions painted",
      },
      { description: `${Math.round(longest)} ms`, type: "longest task while they painted" }
    );

    expect(
      measured.paintMs,
      `six hundred sessions took ${Math.round(measured.paintMs)} ms to paint`
    ).toBeLessThan(100);
    expect(
      measured.held,
      `a task of ${Math.round(longest)} ms held the main thread while they painted`
    ).toEqual([]);
    // The draw was of the whole list: every session is still counted, and the window drew the
    // rows the screen holds rather than a handful left over from the fold.
    await expect(page.getByTestId("agents-group-count-idle")).toHaveText("(600)");
    expect(measured.drawn).toBeGreaterThan(5);
  } finally {
    await context.close();
  }
});
