/** A claim chip whose holder's name is free text of any length, drawn from one fixed answer: the
 *  chip ellipsizes the name and the issue page never grows a scrollbar, at a phone's width and
 *  at a desktop's. The row asserts a width and a layout, which a recording answers
 *  (packages/dispatch/README.md, "The stubbed layer").
 *
 *  The chip also draws the claim's age. A replay that read that age against today's clock would
 *  lay out a longer text on every run after the recording, so the row pins the page's clock to
 *  the moment the claim was made, and every run lays out the same text. */
import { expect } from "@playwright/test";

import { setLiveSessions } from "./agents";
import { claimIssue, createIssue, createProject, fixedKey, mintAgentToken } from "./api";
import { stubbed } from "./stub/stub-test";

/** The key this file's recordings were made under. */
const CORE = fixedKey("CORE", "this file's recordings were made under it");

const test = stubbed(async () => {
  await createProject({ key: CORE, name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Claimed by a long name" });
  const token = await mintAgentToken("claim-e2e", "alice");
  const holder = "Implementer whose session title runs on and on and on".padEnd(120, "!");
  await setLiveSessions([{ session_id: "claim-e2e-session", title: holder }]);
  await claimIssue(issue.key, {
    actor: { id: "claim-e2e-session", kind: "session", origin: { session_title: holder } },
    as: "agent",
    token,
  });
  return { claimedAt: new Date().toISOString(), issue: issue.key };
});

// A session title is free text of any length, and the claim chip renders it on the issue
// header, so a long holder name must not widen the page. The long-payload shape is
// labels.e2e.ts's; the width assertion is this row's own, below: the page's `scrollWidth` held
// to its `clientWidth` at each of the two widths.
test("a long holder name never widens the issue page @pr", async ({ page, scene }, testInfo) => {
  await page.clock.install({ time: new Date(scene.claimedAt) });
  for (const { width, height } of [
    { height: 844, width: 390 },
    { height: 900, width: 1280 },
  ]) {
    await page.setViewportSize({ height, width });
    await page.goto(`/issues/${scene.issue}`);
    const chip = page.getByTestId("issue-claim");
    await expect(chip).toBeVisible();
    await expect(chip).toHaveAttribute("title", /Claimed by Implementer whose session/);
    // The holder ellipsizes inside the chip; the page itself never grows a scrollbar.
    const pageWidths = await page.evaluate(() => ({
      clientWidth: document.documentElement.clientWidth,
      scrollWidth: document.documentElement.scrollWidth,
    }));
    expect(pageWidths.scrollWidth).toBe(pageWidths.clientWidth);
    expect(pageWidths.clientWidth).toBeLessThanOrEqual(width);
    expect(await chip.evaluate((element) => getComputedStyle(element).flexShrink)).toBe("1");
    const shot = testInfo.outputPath(`claim-long-holder-${width}.png`);
    await page.screenshot({ path: shot });
    await testInfo.attach(`long holder name (${width}px)`, {
      contentType: "image/png",
      path: shot,
    });
  }
});
