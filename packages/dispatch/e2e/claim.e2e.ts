import { setLiveSessions } from "./agents";
import { claimIssue, createIssue, createProject, getIssue, mintAgentToken } from "./api";
import { expect, test } from "./test";
import { asUser } from "./users";

// A session title is free text of any length, and the claim chip renders it on the issue
// header, so a long holder name must not widen the page. The long-payload shape is
// labels.e2e.ts's; the width assertion is the one above.
// @pr: The claim chip's only width row: a session title is free text of any length and must not widen the issue page.
test("a long holder name never widens the issue page @pr", async ({ browser }, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Claimed by a long name" });
  const token = await mintAgentToken("claim-e2e", "alice");
  const holder = "Implementer whose session title runs on and on and on".padEnd(120, "!");
  await setLiveSessions([{ session_id: "claim-e2e-session", title: holder }]);
  await claimIssue(issue.key, {
    actor: { id: "claim-e2e-session", kind: "session", origin: { session_title: holder } },
    as: "agent",
    token,
  });

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    for (const { width, height } of [
      { height: 844, width: 390 },
      { height: 900, width: 1280 },
    ]) {
      await page.setViewportSize({ height, width });
      await page.goto(`/issues/${issue.key}`);
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
  } finally {
    await context.close();
  }
});

// Release sits beside the claim chip in the header's Attention group, at every width, and a
// press of it frees the issue on the server and takes the chip and the control off the page.
test("Release beside the claim on today's issue header frees the issue", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Claimed, then released" });
  const token = await mintAgentToken("claim-e2e", "alice");
  await setLiveSessions([{ session_id: "claim-e2e-session", title: "Implementer" }]);
  await claimIssue(issue.key, {
    actor: { id: "claim-e2e-session", kind: "session", origin: { session_title: "Implementer" } },
    as: "agent",
    token,
  });

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.setViewportSize({ height: 844, width: 390 });
    await page.goto(`/issues/${issue.key}`);
    const attention = page.getByTestId("issue-attention");
    const chip = attention.getByTestId("issue-claim");
    const release = attention.getByRole("button", { name: `Release the claim on ${issue.key}` });
    await expect(chip).toBeVisible();
    await expect(release).toBeVisible();
    // The chip and its Release share a line.
    const [chipBox, releaseBox] = await Promise.all([chip.boundingBox(), release.boundingBox()]);
    expect(
      Math.abs(
        (chipBox?.y ?? 0) +
          (chipBox?.height ?? 0) / 2 -
          (releaseBox?.y ?? 0) -
          (releaseBox?.height ?? 0) / 2
      )
    ).toBeLessThan(4);

    await release.click();
    await expect.poll(async () => (await getIssue(issue.key)).claim).toBeNull();
    await expect(chip).toHaveCount(0);
    await expect(release).toHaveCount(0);
  } finally {
    await context.close();
  }
});
