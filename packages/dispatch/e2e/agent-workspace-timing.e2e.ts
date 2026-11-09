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
 * hundred sessions with a median paint inside 100 ms and a median longest task under 150 - so
 * the whole file runs in the
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

// R7, k12's one timed claim: across three fresh loads of the Agents page of six hundred
// sessions, the median first paint is inside 100 milliseconds and the median longest task
// inside 150. The spec measured that on its prototype - drawing every row cost 177 to 374 ms
// with a longest task of up to 290, and the windowed list 23 to 40 ms with no long task - so
// what is timed here is the same thing: the page's own work drawing its list of six hundred,
// with the whole list folded away and then drawn again. The app's cold boot is no part of it,
// since no requirement here changed what a cold boot costs: this pod takes about 140 ms to
// start the page whether it lists ten sessions or six hundred.
//
// Three loads and a median, because one reading judges the machine rather than the page: the
// same bytes have painted in 40 ms and in 212 ms on this pod, so a single reading says as much
// about what else the machine was doing as about the page. Each of the three loads is a page
// of its own, fresh from its own load, with one timed redraw and then closed - never a second
// timed draw on a page that has already been timed - so a page warmed by an earlier timing
// cannot pull the median under a bound a fresh one misses.
//
// The 150 ms is this pod's own gap between a windowed draw and an un-windowed one, measured on
// 2026-11-09 with the machine idle. Windowed, three runs of three loads: the median longest
// task read 0, 57 and 60 ms, and the worst single load 113. Un-windowed - the same measurement
// on this change's base, 1ffc8dec, where the page draws all six hundred rows - it read 305,
// 202 and 257 ms, and the lowest single load 190. So 150 sits in the gap: two and a half times
// the worst windowed median, and a quarter under the lowest un-windowed reading, which fails
// it, as that page fails the paint too at a median load of 202 to 306 ms. The 50 ms that makes
// a task long to the browser is not the bound, because this two-CPU pod is about twice as slow
// as the prototype the spec measured: a draw of 50 to 90 ms here is that same 23 to 40 ms
// render, not a regression.
//
// The press is driven from inside the page, in one `evaluate`, and armed in a timer rather
// than pressed on the `evaluate`'s own line. A Playwright click takes a DOM snapshot for the
// trace either side of itself (`trace: "retain-on-failure"`), and that snapshotting is the
// harness's work, not the page's - with the press driven from the browser it added 25 ms to
// this window and a task of its own. The timer is what leaves a long task to read: React draws
// the rows in a microtask of the task that pressed the toggle, so a press from the `evaluate`
// puts the whole draw inside the driver's one task, which is no task of the page's and is
// never reported as a long one. Every load's two figures are kept on the result, with both
// medians, whether or not the row passes.
// SLOW(>=2s): six hundred sessions seeded, then three pages that each list them all before the
// one draw each of them is timed for.
test("the Agents page paints six hundred sessions with a median paint inside 100 ms and a median longest task under 150", async ({
  browser,
}) => {
  await setLiveSessions(workers());
  const context = await asUser(browser, "alice");
  try {
    // One load: a page of its own, which lists all six hundred, folds them away and then draws
    // them once. The page is closed when its reading is taken, so nothing this row measures is
    // a second draw on a page that has drawn the list already.
    const paintOnce = async (load: number) => {
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
          // The press is armed in a timer rather than pressed here: this `evaluate` is the
          // driver's own task, and React draws the rows in a microtask of whatever task pressed
          // the toggle, so a press on this line puts the whole draw inside the driver's task,
          // which the browser never counts as a task of the page's. From the timer the press,
          // the draw and the mutation that ends the window are one task of the page's own.
          let startedAt = 0;
          setTimeout(() => {
            startedAt = performance.now();
            toggle.click();
          });
          const rowsAt = await rows.promise;
          // The browser hands a long task to the observer in a task of its own, once the task
          // that ran it has ended, so a read taken inside the draw's own task finds nothing
          // however long the page held the thread: that is what made this reading inert. Two
          // turns of the event loop have carried every entry read here, and the wait is ten of
          // them rather than a deadline in milliseconds: a page that is still drawing lets no
          // timer run, so a deadline would be spent by the very draw this reads - at the
          // un-windowed base a quarter of a second ran out before the entry arrived and the
          // row read 0 for a task of 221 ms.
          let held: { duration: number; end: number; start: number }[] = [];
          for (let turn = 0; turn < 10 && held.length === 0; turn += 1) {
            await new Promise((resolve) => setTimeout(resolve, 0));
            held = paint.longTasks.filter((task) => task.end > startedAt && task.start < rowsAt);
          }
          return {
            drawn: document.querySelectorAll("[data-agent-row]").length,
            held,
            paintMs: rowsAt - startedAt,
          };
        });
        const longest = Math.max(0, ...measured.held.map((task) => task.duration));
        test.info().annotations.push(
          {
            description: `${Math.round(measured.paintMs)} ms, ${measured.drawn} rows drawn of 600`,
            type: `600 sessions painted, load ${load} of 3`,
          },
          {
            description: `${Math.round(longest)} ms`,
            type: `longest task while they painted, load ${load} of 3`,
          }
        );
        // The draw was of the whole list: every session is still counted, and the window drew
        // the rows the screen holds rather than a handful left over from the fold.
        await expect(page.getByTestId("agents-group-count-idle")).toHaveText("(600)");
        expect(measured.drawn, `load ${load} of 3 drew ${measured.drawn} rows`).toBeGreaterThan(5);
        return { longest, paintMs: measured.paintMs };
      } finally {
        await page.close();
      }
    };

    const loads = [await paintOnce(1), await paintOnce(2), await paintOnce(3)];
    const readings = loads.map((load) => `${Math.round(load.paintMs)} ms`).join(", ");
    const median = [...loads].sort((first, second) => first.paintMs - second.paintMs)[1];
    const held = loads.map((load) => `${Math.round(load.longest)} ms`).join(", ");
    const heldMedian = [...loads].sort((first, second) => first.longest - second.longest)[1]
      .longest;
    test.info().annotations.push(
      {
        description: `${Math.round(median.paintMs)} ms of ${readings}`,
        type: "median of three fresh loads",
      },
      {
        description: `${Math.round(heldMedian)} ms of ${held}`,
        type: "median longest task of three fresh loads",
      }
    );

    // Soft, so a run the page held the thread through reports its paint too, and the median
    // below is read.
    expect
      .soft(
        heldMedian,
        `the longest task while they painted was ${held} over three fresh loads, a median of ` +
          `${Math.round(heldMedian)} ms`
      )
      .toBeLessThan(150);
    expect(
      median.paintMs,
      `six hundred sessions painted in ${readings} over three fresh loads, a median of ` +
        `${Math.round(median.paintMs)} ms`
    ).toBeLessThan(100);
  } finally {
    await context.close();
  }
});
