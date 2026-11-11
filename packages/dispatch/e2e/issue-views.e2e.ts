import { expect, type Locator, test } from "@playwright/test";

import { createProject } from "./api";
import { seedLegionFixture, seedLegionMetricsCorpus, startFakeLegionDaemon } from "./legion-seed";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

test.beforeEach(async () => {
  await resetDatabase();
});

const tabCount = async (tab: Locator) => {
  const value = await tab.locator("[data-tab-count]").getAttribute("data-tab-count");
  if (value === null) throw new Error("The selected view has no count");
  return Number(value.split("/")[0]);
};

test("each view's tab count equals its row count @legion @phone", async ({ browser }) => {
  const daemon = await startFakeLegionDaemon({});
  await seedLegionFixture(daemon.url);
  await seedLegionMetricsCorpus();
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto("/projects/ACME/issues");
    const tabs = page.locator('[role="tab"][id^="view-"]');
    await expect(tabs).toHaveCount(7);
    for (let index = 0; index < 7; index++) {
      const tab = tabs.nth(index);
      await tab.click();
      await expect(page.getByTestId("issues-table-scroll")).toHaveAttribute(
        "data-row-count",
        /\d+/
      );
      expect(await tabCount(tab)).toBe(
        Number(await page.getByTestId("issues-table-scroll").getAttribute("data-row-count"))
      );
    }
  } finally {
    await context.close();
    await daemon.close();
  }
});

test("default columns fit desktop views and phone titles use two lines @legion @phone", async ({
  browser,
}) => {
  const daemon = await startFakeLegionDaemon({});
  await seedLegionFixture(daemon.url);
  await seedLegionMetricsCorpus();
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ width: 1440, height: 900 });
    for (const view of ["open", "triage", "waiting", "in-flight", "slots", "finished", "mine"]) {
      await page.goto(`/projects/ACME/issues?view=${view}`);
      const scroll = page.getByTestId("issues-table-scroll");
      await expect(scroll).toBeVisible();
      expect(await scroll.evaluate((element) => element.scrollWidth <= element.clientWidth)).toBe(
        true
      );
    }

    await page.setViewportSize({ width: 390, height: 844 });
    for (const view of ["open", "triage", "waiting", "in-flight", "slots", "finished", "mine"]) {
      await page.goto(`/projects/ACME/issues?view=${view}`);
      const title = page.locator("[data-grid-title-row]").first();
      await expect(title).toBeVisible();
      expect(await title.locator("span").first().getAttribute("class")).toContain("line-clamp-2");
    }
  } finally {
    await context.close();
    await daemon.close();
  }
});

test("an override marks the view changed and Reset view clears it @legion", async ({ browser }) => {
  const daemon = await startFakeLegionDaemon({});
  await seedLegionFixture(daemon.url);
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/projects/ACME/issues?view=in-flight&f.stage=phase%3Aimplementing");
    await expect(page.locator("#view-in-flight-tab [data-tab-marked]")).toBeVisible();
    await page.getByRole("button", { name: "Reset view" }).click();
    await expect(page).toHaveURL(/\/projects\/ACME\/issues\?view=in-flight$/u);
    await expect(page.locator("#view-in-flight-tab [data-tab-marked]")).toHaveCount(0);
  } finally {
    await context.close();
    await daemon.close();
  }
});

test("a saved view survives a reload and remains in its reader's browser @legion", async ({
  browser,
}) => {
  const daemon = await startFakeLegionDaemon({});
  await seedLegionFixture(daemon.url);
  const first = await asUser(browser, "alice");
  const second = await asUser(browser, "alice");
  try {
    const page = await first.newPage();
    await page.goto("/projects/ACME/issues?view=triage");
    await page.getByRole("button", { name: "Save view" }).click();
    await page.getByRole("textbox", { name: "Name" }).fill("My triage");
    await page.getByRole("button", { name: "Save", exact: true }).click();
    await expect(page.locator("#view-saved-0-tab")).toHaveText("My triage");
    await page.reload();
    await expect(page.locator("#view-saved-0-tab")).toHaveText("My triage");

    const other = await second.newPage();
    await other.goto("/projects/ACME/issues");
    await expect(other.locator("#view-saved-0-tab")).toHaveCount(0);
  } finally {
    await first.close();
    await second.close();
    await daemon.close();
  }
});

test("columns overrides do not persist, while a visibility change saves its view layout @legion", async ({
  browser,
}) => {
  const daemon = await startFakeLegionDaemon({});
  await seedLegionFixture(daemon.url);
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/projects/ACME/issues?view=open&cols=key,title");
    await expect(page.getByRole("columnheader", { name: /^Key/ })).toBeVisible();
    await expect(page.getByRole("columnheader", { name: /^Title/ })).toBeVisible();
    expect(
      await page.evaluate(() => localStorage.getItem("dispatch.project-table.ACME"))
    ).toBeNull();

    await page.goto("/projects/ACME/issues?view=finished&cols=key,title,closed");
    await page.getByRole("button", { name: "Columns" }).click();
    await page.getByRole("checkbox", { name: "Closed" }).uncheck();
    await expect(page).not.toHaveURL(/cols=/u);
    await expect
      .poll(() => page.evaluate(() => localStorage.getItem("dispatch.project-table.ACME.finished")))
      .toContain('"closed":false');
    expect(
      await page.evaluate(() => localStorage.getItem("dispatch.project-table.ACME"))
    ).toBeNull();
  } finally {
    await context.close();
    await daemon.close();
  }
});

test("a project without Legion exposes five views and treats a Legion URL as All open", async ({
  browser,
}) => {
  await createProject({ key: "PLAIN", name: "Plain" });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/projects/PLAIN/issues?view=slots");
    await expect(page.locator('[role="tab"][id^="view-"]')).toHaveCount(5);
    await expect(page.locator("section[data-issues-tab]")).toHaveAttribute("data-view", "open");
  } finally {
    await context.close();
  }
});

test("the closed range filter and exclude-tree chip update the URL @legion", async ({
  browser,
}) => {
  const daemon = await startFakeLegionDaemon({});
  await seedLegionFixture(daemon.url);
  await seedLegionMetricsCorpus();
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/projects/ACME/issues?view=finished");
    await page.getByRole("button", { name: "Filter Closed" }).click();
    await page.getByRole("button", { name: "7 days" }).click();
    await expect(page).toHaveURL(/f\.closed=7d/u);

    await page.goto("/projects/ACME/issues?view=finished&exclude_tree=ACME-1");
    await expect(page.locator('[data-exclude-tree="ACME-1"]')).toContainText(
      "Without ACME-1's tree"
    );
    await page.getByRole("button", { name: "Include ACME-1's tree" }).click();
    await expect(page).not.toHaveURL(/exclude_tree/u);
  } finally {
    await context.close();
    await daemon.close();
  }
});
