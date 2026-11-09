/** The issue landing at a desktop width: Spec primary, the open asks in Needs you. It reads
 *  only what the page draws, so it runs against a recording rather than against a harness
 *  (packages/dispatch/AGENTS.md, "The stubbed layer"). The phone half of the pair stayed in
 *  `spec-primary.e2e.ts`: the `iphone` project runs no stubbed row. */
import { expect } from "@playwright/test";

import { createAsk, createIssue, createProject } from "./api";
import { needsYouCards } from "./editor";
import { stubbed } from "./stub/stub-test";

const session = {
  actor: { id: "e2e-spec-primary", kind: "session" as const },
  as: "agent" as const,
};

const test = stubbed(async () => {
  await createProject({ key: "SPEC", name: "Spec primary" });
  const issue = await createIssue({
    project: "SPEC",
    spec: "Primary document content.",
    title: "Keep the document central",
  });
  await createAsk(issue.key, { question: "Approve the document-first layout?" }, session);
  await createAsk(
    issue.key,
    {
      anchor: { artifact: "spec", quote: "Primary" },
      question: "Is this opening clear?",
    },
    session
  );
  return { issue: issue.key };
});

test("issue landing keeps Spec primary and puts open asks in Needs you @pr", async ({
  page,
  scene,
}, testInfo) => {
  await page.setViewportSize({ height: 900, width: 1440 });
  await page.goto(`/issues/${scene.issue}`);

  await expect(page.getByRole("tab", { name: "Spec" })).toHaveAttribute("aria-selected", "true");

  await expect(
    needsYouCards(page).filter({ hasText: "Approve the document-first layout?" })
  ).toBeVisible();
  await expect(needsYouCards(page).filter({ hasText: "Is this opening clear?" })).toBeVisible();
  const desktopScreenshot = testInfo.outputPath("spec-primary-desktop-1440.png");
  await page.screenshot({ path: desktopScreenshot, fullPage: true });
  await testInfo.attach("spec primary desktop 1440", {
    contentType: "image/png",
    path: desktopScreenshot,
  });
});
