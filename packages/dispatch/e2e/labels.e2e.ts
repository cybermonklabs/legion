import { createIssue, createProject, getIssue, patchIssue } from "./api";
import { filterPicker, pickFilterOption } from "./filters";
import { expect, type Locator, rowKey, test } from "./test";
import { asUser } from "./users";

async function expectTouchTarget(locator: Locator): Promise<void> {
  const box = await locator.boundingBox();
  expect(box?.height ?? 0).toBeGreaterThanOrEqual(44);
}

// @pr: A keyed write a person makes: labels edited from the multi-select, and the project list filtered by every one picked.
test("edits issue labels from a searchable multi-select and filters project issues by every selected label @pr", async ({
  browser,
}) => {
  const core = rowKey("CORE");
  await createProject({ key: core, name: "Core" });
  const edited = await createIssue({ project: core, title: "Editable labels" });
  const matching = await createIssue({ project: core, title: "Frontend documentation" });
  const other = await createIssue({ project: core, title: "Backend documentation" });
  await patchIssue(edited.key, { labels: ["Frontend", "api"] });
  await patchIssue(matching.key, { labels: ["Frontend", "docs"] });
  await patchIssue(other.key, { labels: ["backend", "docs"] });

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto(`/issues/${edited.key}`);
    const editLabels = page.getByRole("button", { name: "Edit labels" });
    await editLabels.click();
    const labelSearch = page.getByRole("combobox", { name: "Search or create label" });
    await expect(page.getByRole("option", { name: "Frontend", exact: true })).toHaveAttribute(
      "aria-selected",
      "true"
    );
    await expect(page.getByRole("option", { name: "api", exact: true })).toHaveAttribute(
      "aria-selected",
      "true"
    );
    await expect(page.getByRole("option", { name: "backend", exact: true })).toHaveAttribute(
      "aria-selected",
      "false"
    );
    await labelSearch.fill("urgent");
    await expect(page.getByRole("option", { name: 'Create "urgent"' })).toBeVisible();
    await labelSearch.press("Enter");
    await expect(page.getByRole("option", { name: "urgent", exact: true })).toHaveAttribute(
      "aria-selected",
      "true"
    );

    const shots = process.env.DISPATCH_LABEL_SCREENSHOT_DIR;
    if (shots !== undefined) {
      await page.screenshot({
        fullPage: true,
        path: `${shots}/labels-${page.viewportSize()?.width}.png`,
      });
    }

    await editLabels.click();
    await expect
      .poll(() => getIssue(edited.key))
      .toMatchObject({
        labels: ["Frontend", "api", "urgent"],
      });

    await page.goto(`/projects/${core}?label=docs`);
    await expect(page).toHaveURL(new RegExp(`/projects/${core}/issues\\?label=docs$`));
    await filterPicker(page, "Labels").click();
    await expect(page.getByRole("option", { exact: true, name: "docs, 2 issues" })).toHaveAttribute(
      "aria-selected",
      "true"
    );
    await expect(page.getByText("Frontend documentation", { exact: true })).toBeVisible();
    await expect(page.getByText("Backend documentation", { exact: true })).toBeVisible();
    await expect(page.getByText("Editable labels", { exact: true })).toHaveCount(0);
    await pickFilterOption(page, "Labels", "docs");
    await expect(page).toHaveURL(
      (url) => url.pathname === `/projects/${core}/issues` && !url.searchParams.has("label")
    );
    await pickFilterOption(page, "Labels", "Frontend");
    await expect(page.getByText("Editable labels", { exact: true })).toBeVisible();
    await expect(page.getByText("Frontend documentation", { exact: true })).toBeVisible();
    await expect(page.getByText("Backend documentation", { exact: true })).toHaveCount(0);
    await pickFilterOption(page, "Labels", "docs");
    await expect(page.getByText("Frontend documentation", { exact: true })).toBeVisible();
    await expect(page.getByText("Editable labels", { exact: true })).toHaveCount(0);
    await expect(page.getByText("Backend documentation", { exact: true })).toHaveCount(0);
  } finally {
    await context.close();
  }
});

// The label controls' phone half: the header's Edit labels and its search box, and the project
// list's Labels picker with its options and Clear, are what a finger presses, so each holds the
// 44 px touch target and the filtered list fits the screen. What those controls do is the row
// above, which holds at any width.
test("the label controls keep touch-sized targets and the filtered list fits the screen @phone @phone-only", async ({
  browser,
}) => {
  const core = rowKey("CORE");
  await createProject({ key: core, name: "Core" });
  const edited = await createIssue({ project: core, title: "Editable labels" });
  const matching = await createIssue({ project: core, title: "Frontend documentation" });
  await patchIssue(edited.key, { labels: ["Frontend", "api"] });
  await patchIssue(matching.key, { labels: ["Frontend", "docs"] });

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto(`/issues/${edited.key}`);
    const editLabels = page.getByRole("button", { name: "Edit labels" });
    await editLabels.click();
    const labelSearch = page.getByRole("combobox", { name: "Search or create label" });
    await expect(labelSearch).toBeVisible();
    await expectTouchTarget(editLabels);
    await expectTouchTarget(labelSearch);
    const shots = process.env.DISPATCH_LABEL_SCREENSHOT_DIR;
    if (shots !== undefined) {
      await page.screenshot({
        fullPage: true,
        path: `${shots}/labels-${page.viewportSize()?.width}.png`,
      });
    }
    await editLabels.click();

    await page.goto(`/projects/${core}?label=docs`);
    await expect(page.getByText("Frontend documentation", { exact: true })).toBeVisible();
    await filterPicker(page, "Labels").click();
    await expectTouchTarget(filterPicker(page, "Labels"));
    await expectTouchTarget(page.getByRole("option", { exact: true, name: "docs, 1 issue" }));
    await expectTouchTarget(page.getByRole("button", { name: "Clear labels" }));
    await expect(
      page.evaluate(
        () => document.documentElement.scrollWidth <= document.documentElement.clientWidth
      )
    ).resolves.toBe(true);
  } finally {
    await context.close();
  }
});

// The header's labels are a group of at most ten, then "+N" that shows the rest in place
// (dispatch://INFRA-5200, the layout rule's 3), and long labels wrap onto further lines inside
// the header rather than scroll or widen the page.
test("twelve thirty-character labels wrap in the header, ten then +2, without widening the page", async ({
  browser,
}, testInfo) => {
  const core = rowKey("CORE");
  await createProject({ key: core, name: "Core" });
  const issue = await createIssue({ project: core, title: "Long labels" });
  const labels = Array.from({ length: 12 }, (_, index) => `label-${index}-`.padEnd(30, "x"));
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    const title = page
      .getByTestId("issue-header")
      .getByRole("heading", { level: 1, name: "Long labels" });
    for (const [width, height] of [
      [1280, 900],
      [390, 844],
    ]) {
      await page.setViewportSize({ height, width });
      await patchIssue(issue.key, { labels: [] });
      await page.goto(`/issues/${issue.key}`);
      await expect(page.getByRole("button", { name: "Edit labels" })).toBeVisible();
      const titleBefore = await title.boundingBox();

      await patchIssue(issue.key, { labels });
      await page.reload();
      await expect(page.getByText(labels[9] ?? "", { exact: true })).toBeVisible();
      // Ten labels, then the control that shows the other two.
      await expect(page.getByText(labels[10] ?? "", { exact: true })).toHaveCount(0);
      const more = page.getByRole("button", { name: "+2, show all 12 labels" });
      await expect(more).toHaveText("+2");
      expect((await more.boundingBox())?.height ?? 0).toBeGreaterThanOrEqual(44);

      // The title box is untouched, the labels take more than one line, and nothing scrolls
      // sideways: no box of the header, and not the document.
      expect(await title.boundingBox()).toEqual(titleBefore);
      const tops = await page
        .getByTestId("issue-labels")
        .locator("span")
        .filter({ hasText: /^label-/ })
        .evaluateAll((chips) => chips.map((chip) => Math.round(chip.getBoundingClientRect().top)));
      expect(new Set(tops).size).toBeGreaterThan(1);
      const scrolls = await page.getByTestId("issue-header").evaluate((header) =>
        [header, ...header.querySelectorAll("*")].some((element) => {
          const overflowX = getComputedStyle(element).overflowX;
          return (
            (overflowX === "auto" || overflowX === "scroll") &&
            element.scrollWidth > element.clientWidth + 1
          );
        })
      );
      expect(scrolls).toBe(false);
      const pageWidths = await page.evaluate(() => ({
        clientWidth: document.documentElement.clientWidth,
        scrollWidth: document.documentElement.scrollWidth,
      }));
      expect(pageWidths.scrollWidth).toBe(pageWidths.clientWidth);
      expect(pageWidths.clientWidth).toBeLessThanOrEqual(width);

      // The control shows the other two in place, and folds them back.
      await more.click();
      await expect(page.getByText(labels[11] ?? "", { exact: true })).toBeVisible();
      await page.getByRole("button", { name: "Show fewer labels" }).click();
      await expect(page.getByText(labels[11] ?? "", { exact: true })).toHaveCount(0);
      const shot = testInfo.outputPath(`labels-wrapped-${width}.png`);
      await page.screenshot({ path: shot });
      await testInfo.attach(`twelve long labels (${width}px)`, {
        contentType: "image/png",
        path: shot,
      });
    }
  } finally {
    await context.close();
  }
});
