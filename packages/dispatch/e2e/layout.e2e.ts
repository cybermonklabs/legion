import { expect, test } from "@playwright/test";

import { createComment, createIssue, createProject, createProjectDocument } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

test.beforeEach(async () => {
  await resetDatabase();
});

test("tablet keeps the Conversation readable and exposes the review sheet", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "Review this tablet issue.",
    title: "Review at tablet width",
  });
  const comment = await createComment(issue.key, { body: "Open this review item." });
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await page.setViewportSize({ height: 1024, width: 800 });
    await page.goto(`/issues/${issue.key}/conversation`);

    await expect(page.getByRole("button", { name: "Open navigation" })).toBeVisible();
    await expect(page.getByRole("button", { name: /Open review panel/ })).toBeVisible();
    const conversationBounds = await page
      .getByRole("tabpanel", { name: "Conversation" })
      .boundingBox();
    expect(conversationBounds?.width ?? 0).toBeGreaterThanOrEqual(400);
    const tabletScreenshot = testInfo.outputPath("tablet-800.png");
    await page.screenshot({ path: tabletScreenshot, fullPage: true });
    await testInfo.attach("tablet layout (800px)", {
      contentType: "image/png",
      path: tabletScreenshot,
    });

    await page.setViewportSize({ height: 768, width: 1024 });
    await page.goto(`/issues/${issue.key}/comments/${comment.id}`);
    await expect(
      page
        .getByRole("list", { name: "Conversation turns" })
        .locator(`li[data-turn="comment:${comment.id}"][aria-current="true"]`)
    ).toBeVisible();

    await page.setViewportSize({ height: 1024, width: 1280 });
    await expect(page.getByRole("navigation", { name: "Navigation" })).toBeVisible();
    await expect(page.getByRole("complementary", { name: "Review margin" })).toBeVisible();
    const desktopScreenshot = testInfo.outputPath("desktop-1280.png");
    await page.screenshot({ path: desktopScreenshot, fullPage: true });
    await testInfo.attach("desktop layout (1280px)", {
      contentType: "image/png",
      path: desktopScreenshot,
    });
  } finally {
    await context.close();
  }
});

// An unanchored comment deep link focuses its Conversation turn, rather than opening a
// review-sheet comment. The compact sheet must remain visibly openable and pointer-reachable
// on that route.
test("a comment deep link keeps the compact review-panel toggle hit-testable", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "Review this tablet issue.",
    title: "Review at tablet width",
  });
  const comment = await createComment(issue.key, { body: "Open this review item." });
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await page.setViewportSize({ height: 1024, width: 800 });
    await page.goto(`/issues/${issue.key}/conversation`);
    await expect(page.getByRole("button", { name: /Open review panel/ })).toBeVisible();

    await page.setViewportSize({ height: 768, width: 1024 });
    await page.goto(`/issues/${issue.key}/comments/${comment.id}`);
    await expect(page.getByRole("tab", { name: "Conversation" })).toHaveAttribute(
      "aria-selected",
      "true"
    );
    await expect(
      page.locator(`li[data-turn="comment:${comment.id}"][aria-current="true"]`)
    ).toBeVisible();
    const openToggle = page.getByRole("button", { name: /Open review panel/ });
    await expect(openToggle).toBeVisible();
    await openToggle.click();
    const toggle = page.getByRole("button", { name: /Close review panel/ });
    await expect(toggle).toBeVisible();
    await expect(page.getByTestId("margin-sheet")).toHaveAttribute("data-expanded", "true");
    const hitTest = () =>
      toggle.evaluate((button) => {
        const rect = button.getBoundingClientRect();
        const elementAtCenter = document.elementFromPoint(
          rect.x + rect.width / 2,
          rect.y + rect.height / 2
        );
        return (
          elementAtCenter !== null &&
          (elementAtCenter === button || button.contains(elementAtCenter))
        );
      });
    await expect.poll(hitTest).toBe(true);
  } finally {
    await context.close();
  }
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
