/** The Conversation's geometry at three widths, and the compact review sheet a comment deep link
 *  leaves reachable. Both rows read what the page draws at a width - which controls are shown, how
 *  wide the Conversation is, which turn is current, and whether the sheet's toggle is hit-testable
 *  - and none of it is a claim about the server, so both run on the stubbed layer
 *  (packages/dispatch/AGENTS.md, "The stubbed layer"). */
import { expect } from "@playwright/test";

import { createComment, createIssue, createProject } from "./api";
import { stubbed } from "./stub/stub-test";

const test = stubbed(async () => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "Review this tablet issue.",
    title: "Review at tablet width",
  });
  const comment = await createComment(issue.key, { body: "Open this review item." });
  return { comment: comment.id, issue: issue.key };
});

test("tablet keeps the Conversation readable and exposes the review sheet @pr", async ({
  page,
  scene,
}, testInfo) => {
  await page.setViewportSize({ height: 1024, width: 800 });
  await page.goto(`/issues/${scene.issue}/conversation`);

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
  await page.goto(`/issues/${scene.issue}/comments/${scene.comment}`);
  await expect(
    page
      .getByRole("list", { name: "Conversation turns" })
      .locator(`li[data-turn="comment:${scene.comment}"][aria-current="true"]`)
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
});

// An unanchored comment deep link focuses its Conversation turn, rather than opening a
// review-sheet comment. The compact sheet must remain visibly openable and pointer-reachable
// on that route.
test("a comment deep link keeps the compact review-panel toggle hit-testable @pr", async ({
  page,
  scene,
}) => {
  await page.setViewportSize({ height: 1024, width: 800 });
  await page.goto(`/issues/${scene.issue}/conversation`);
  await expect(page.getByRole("button", { name: /Open review panel/ })).toBeVisible();

  await page.setViewportSize({ height: 768, width: 1024 });
  await page.goto(`/issues/${scene.issue}/comments/${scene.comment}`);
  await expect(page.getByRole("tab", { name: "Conversation" })).toHaveAttribute(
    "aria-selected",
    "true"
  );
  await expect(
    page.locator(`li[data-turn="comment:${scene.comment}"][aria-current="true"]`)
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
        elementAtCenter !== null && (elementAtCenter === button || button.contains(elementAtCenter))
      );
    });
  await expect.poll(hitTest).toBe(true);
});
