/** The issue landing at both widths: Spec primary with the open asks in Needs you at a desktop
 *  width, and the review panel toggle the phone layout draws instead. Each reads only what the
 *  page draws, so each runs against a recording rather than against a harness
 *  (packages/dispatch/AGENTS.md, "The stubbed layer"). The phone row runs under `stub-phone`,
 *  which is the one project that takes the `@phone` tag here. */
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

// Below 768 px the review panel is not beside the document at all: it sits behind a toggle the
// phone layout alone renders, and opening it has to bring the open asks up without pushing the
// page sideways. The screen is the project's own, so the claim is read at the width a phone
// actually gives it.
test("on a phone the issue's open asks come up behind the review panel toggle @phone @pr", async ({
  page,
  scene,
}, testInfo) => {
  await page.goto(`/issues/${scene.issue}`);
  const toggle = page.getByRole("button", { name: "Open review panel (2 open asks)" });
  await expect(toggle).toBeVisible();
  const toggleScreenshot = testInfo.outputPath("spec-primary-phone-toggle.png");
  await page.screenshot({ path: toggleScreenshot, fullPage: true });
  await testInfo.attach("spec primary phone toggle", {
    contentType: "image/png",
    path: toggleScreenshot,
  });
  await toggle.click();
  await expect(
    needsYouCards(page).filter({ hasText: "Approve the document-first layout?" })
  ).toBeVisible();
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(
    true
  );
  const needsYouScreenshot = testInfo.outputPath("spec-primary-phone-needs-you.png");
  await page.screenshot({ path: needsYouScreenshot, fullPage: true });
  await testInfo.attach("spec primary phone Needs you", {
    contentType: "image/png",
    path: needsYouScreenshot,
  });
});
