import { expect, test } from "@playwright/test";

import { createIssue, createProject, createProjectDocument } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/**
 * The shell's layout rows that need the real stack, with the reason they are not stubbed rows
 * below. What this file held before - the Conversation's geometry at three widths and the
 * compact review sheet a comment deep link leaves reachable - is in `layout.stub.e2e.ts`.
 *
 * The rows here read a whole page load on each of the two routes that draw a margin, and one of
 * those routes is a project document, whose page is the live document's: the margin sits on
 * that document's room, which no recording replays (packages/dispatch/AGENTS.md, "The stubbed
 * layer").
 */

test.beforeEach(async () => {
  await resetDatabase();
});

declare global {
  interface Window {
    /** The box `<main>` was laid out at in the commit that first put it on the page, recorded
     *  from before the page's first script by the row below. */
    __mainAtFirstCommit?: string;
  }
}

// The shell's `<main>` is the column every route's content is laid out in, and on a route that
// draws a margin it is only as wide as it is because the margin's column stands beside it.
//
// The margin's own chunk waits for the route to be drawn (`TheRouteIsDrawn` in
// `web/src/app.tsx`), since a boundary suspended beside the route's own costs the page a
// 300 ms fallback throttle. `theRouteIsDrawn` is turned on by an effect, so the commit that
// first puts the shell on the page is always one commit short of it, and until the shell held
// the reserved column itself that commit laid `<main>` out across the margin's column. Whether
// the browser painted it was the scheduler's to decide: on one sweep of this pod every margin
// route painted 72 to 149 ms of it, and on another none did, so what this row reads is the
// commit rather than a frame. Anything the shell draws in it - the route's own skeleton, and
// on an issue document the blocks the server sent - is laid out at one width and re-wrapped at
// another whenever that paint does happen (dispatch://INFRA-341, R11).
//
// A `MutationObserver` is what reads it: its callback runs as a microtask at the end of the
// task React commits in, before the task React flushes its passive effects in, so the box it
// reads is the one that commit laid out whether or not a frame was ever drawn from it.
//
// Both of `routeHasMargin`'s answers are read here, an issue's tab and a project document,
// because the reservation is drawn from that one rule.
for (const route of ["the issue's Conversation tab", "a project document"] as const) {
  // SLOW(>=2s): a whole page load, read from before the page's first script until its margin
  // has arrived.
  test(`the shell lays main out at the box it keeps in its first commit on ${route}`, async ({
    browser,
  }) => {
    await createProject({ key: "CORE", name: "Core" });
    const issue = await createIssue({
      project: "CORE",
      spec: "Review this issue.",
      title: "A margin route",
    });
    await createProjectDocument("CORE", {
      content: "# Ask blocks design\n\nThe ask is a node.\n",
      name: "ask-blocks-design.md",
    });
    const path =
      route === "the issue's Conversation tab"
        ? `/issues/${issue.key}/conversation`
        : "/projects/CORE/documents/ask-blocks-design-md";
    const context = await asUser(browser, "alice");
    const page = await context.newPage();
    try {
      await page.setViewportSize({ height: 900, width: 1280 });
      // From before the page's own first script: `addInitScript` runs at document start, ahead
      // of the inline scripts of `index.html` and of the entry chunk.
      await page.addInitScript(() => {
        const watch = new MutationObserver(() => {
          const main = document.querySelector('[data-testid="main-content"]');
          if (main === null) return;
          const box = main.getBoundingClientRect();
          window.__mainAtFirstCommit = `${Math.round(box.x)}..${Math.round(box.x + box.width)}`;
          watch.disconnect();
        });
        // `document` rather than `document.documentElement`: this runs before the parser has
        // created `<html>`, so the element is not there to observe yet.
        watch.observe(document, { childList: true, subtree: true });
      });
      await page.goto(path);

      // The page settled: the margin is in the column rather than the box held open for it.
      await expect(page.getByRole("complementary", { name: "Review margin" })).toBeVisible();
      await expect(page.locator("[data-margin-pending]")).toHaveCount(0);
      const settled = await page.locator('[data-testid="main-content"]').boundingBox();
      if (settled === null) {
        throw new Error("the shell's main content is not visible");
      }
      const kept = `${Math.round(settled.x)}..${Math.round(settled.x + settled.width)}`;
      const first = await page.evaluate(() => window.__mainAtFirstCommit);

      expect(
        first,
        "main never reached the page, so this run says nothing about the shell's first commit"
      ).toBeDefined();
      expect(
        first,
        "the shell laid main out across the margin's column in the commit that first drew it, " +
          "so everything in that commit is re-wrapped under the reader as soon as the browser " +
          "paints it (`web/src/app.tsx`, the reservation beside `MarginColumn`)"
      ).toBe(kept);
    } finally {
      await context.close();
    }
  });
}
