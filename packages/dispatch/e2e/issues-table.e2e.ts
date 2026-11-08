import { expect, test } from "@playwright/test";
import type { Issue } from "../web/src/api/types";
import { SEND_DEADLINE_MS } from "../web/src/features/writes/keyed-write";
import { setLiveSessions } from "./agents";
import {
  createIssue,
  createMessage,
  createProject,
  getIssue,
  listIssueMessages,
  listIssues,
  patchIssue,
} from "./api";
import { filterPicker } from "./filters";
import { dropFirstAnswer, watchWrites } from "./keyed-writes";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

test.beforeEach(async () => {
  await resetDatabase();
});

test("the table sorts, searches facets and fuzzy text, groups, pages and receives a second page's edit live", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const epic = await createIssue({ project: "CORE", title: "Table epic" });
  await patchIssue(epic.key, { labels: ["epic"] });
  const rows = [];
  for (let index = 0; index < 30; index++) {
    const row = await createIssue({
      project: "CORE",
      parent: epic.key,
      title: `Searchable table work ${String(index).padStart(2, "0")}`,
    });
    rows.push(
      await patchIssue(row.key, {
        status: "todo",
        priority: ([0, 1, 2, 3] as const)[index % 4],
        labels: ["legion", "frontend"],
      })
    );
  }
  const backend = await createIssue({ project: "CORE", title: "Backend unrelated" });
  await patchIssue(backend.key, { labels: ["backend"] });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ width: 1920, height: 1080 });
    await page.goto("/projects/CORE/issues");
    const table = page.getByRole("table", { name: "Project issues table" });
    await expect(table).toBeVisible();
    const titleHeader = table
      .getByRole("columnheader")
      .filter({ has: page.getByRole("button", { name: /^Title(?: ▲| ▼)?$/ }) });
    await titleHeader.getByRole("button", { name: /^Title/ }).click();
    await expect(titleHeader).toHaveAttribute("aria-sort", "ascending");
    await expect(page.locator("[data-issue-row]").first()).toContainText("Backend unrelated");
    await titleHeader.getByRole("button", { name: /^Title/ }).click();
    await expect(titleHeader).toHaveAttribute("aria-sort", "descending");
    await expect(page.locator("[data-issue-row]").first()).toContainText(
      "Searchable table work 29"
    );

    await filterPicker(page, "Labels").click();
    await page.getByRole("combobox", { name: "Search Labels" }).fill("front");
    await expect(page.getByRole("option", { name: /^frontend,/ })).toBeVisible();
    await expect(page.getByRole("option", { name: /^backend,/ })).toHaveCount(0);
    await page.getByRole("option", { name: /^frontend,/ }).click();
    await page.keyboard.press("Escape");
    await expect(page.getByText("Backend unrelated", { exact: true })).toHaveCount(0);
    await expect(page.getByText("30 of 31 issues", { exact: true })).toBeVisible();

    const search = page.getByRole("searchbox", { name: "Search issues" });
    await search.fill("searchble work");
    await expect(page.getByText("30 of 31 issues", { exact: true })).toBeVisible();
    await search.fill("searchble impossible");
    await expect(page.getByText("No issues match these filters.")).toBeVisible();
    await search.fill("searchble work");
    await page.getByRole("combobox", { name: "Group by" }).selectOption("epic");
    await expect(page.getByText(new RegExp(`${epic.key} · Table epic`)).first()).toBeVisible();
    await expect(page.getByText(/next in queue #1/)).toBeVisible();
    await page.getByRole("combobox", { name: "Rows per page" }).selectOption("25");
    await page.getByRole("button", { name: "Next ›", exact: true }).click();
    await expect(page.getByText("Page 2 of 2", { exact: true })).toBeVisible();
    await expect(page.getByText("(continued)", { exact: true })).toBeVisible();
    await page.reload();
    await expect(page.getByText("Page 2 of 2", { exact: true })).toBeVisible();
    await expect(search).toHaveValue("searchble work");
    await expect(page.getByRole("combobox", { name: "Group by" })).toHaveValue("epic");

    // A different browser page makes an ordinary issue edit. The first page is never navigated
    // or reloaded after the edit; its existing live subscription must update the visible row.
    const targetKey = await page.locator("[data-issue-row]").first().getAttribute("data-issue-row");
    expect(targetKey).not.toBeNull();
    const target = rows.find((row) => row.key === targetKey);
    if (!target) throw new Error(`Visible issue ${targetKey} was not seeded`);
    const targetSelection = page.getByRole("checkbox", {
      name: `Select ${target.key}`,
      exact: true,
    });
    await targetSelection.check();
    const rowTop = await page
      .locator(`[data-issue-row="${target.key}"]`)
      .evaluate((element) => element.getBoundingClientRect().top);
    const second = await context.newPage();
    await second.goto(`/issues/${target.key}`);
    await expect(second.getByRole("heading", { level: 1, name: target.title })).toBeVisible();
    const priority = second.getByRole("combobox", { name: `Priority of ${target.key}` });
    const before = await priority.inputValue();
    const next = before === "0" ? "1" : "0";
    await priority.selectOption(next);
    await expect
      .poll(() => getIssue(target.key).then((issue) => issue.priority))
      .toBe(Number(next));
    await expect(
      page
        .locator(`[data-issue-row="${target.key}"]`)
        .getByRole("combobox", { name: `Priority of ${target.key}` })
    ).toHaveValue(next);
    await expect(page.getByText("Page 2 of 2", { exact: true })).toBeVisible();
    await expect(targetSelection).toBeChecked();
    expect(
      await page
        .locator(`[data-issue-row="${target.key}"]`)
        .evaluate((element) => element.getBoundingClientRect().top)
    ).toBe(rowTop);
    await page.screenshot({ path: testInfo.outputPath("issues-table-live.png"), fullPage: true });
    await page.getByRole("button", { name: "Clear selection", exact: true }).click();

    await page.getByRole("combobox", { name: "Group by" }).selectOption("");
    await page.getByRole("combobox", { name: "Rows per page" }).selectOption("50");
    const selected = rows[25];
    if (!selected) throw new Error("The queue seed did not create 26 issues");
    await page.getByRole("checkbox", { name: `Select ${selected.key}`, exact: true }).check();
    const scrollTop = await page
      .getByTestId("issues-table-scroll")
      .evaluate((element) => element.getBoundingClientRect().top);
    await page
      .getByRole("toolbar")
      .getByRole("button", { name: "Start next", exact: true })
      .click();
    await expect(
      page.getByRole("region", { name: "Project issues", exact: true }).getByRole("status")
    ).toContainText("now starts next");
    await expect
      .poll(() => getIssue(selected.key).then((issue) => issue.rank))
      .not.toBe(selected.rank);
    expect(
      await page
        .getByTestId("issues-table-scroll")
        .evaluate((element) => element.getBoundingClientRect().top)
    ).toBe(scrollTop);
    await expect(page.locator(`[data-issue-row="${selected.key}"]`)).toHaveAttribute(
      "data-moved",
      "true"
    );
    // The shared label parameter remains readable by the Board strip.
    await page.getByRole("button", { name: "Board", exact: true }).click();
    await expect(page.getByRole("button", { name: "Remove Label: frontend filter" })).toBeVisible();
    await page.getByRole("button", { name: "List", exact: true }).click();
    await expect(filterPicker(page, "Labels")).toHaveAttribute(
      "aria-label",
      "Filter Labels: 1 picked"
    );
  } finally {
    await context.close();
  }
});

test("merging an epic moves open descendants through closed intermediates and preserves closed history", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const source = await createIssue({ project: "CORE", title: "Source epic" });
  const target = await createIssue({ project: "CORE", title: "Target epic" });
  await patchIssue(source.key, { labels: ["epic"] });
  await patchIssue(target.key, { labels: ["epic"] });
  const open = await createIssue({ project: "CORE", parent: source.key, title: "Open child" });
  const closed = await createIssue({
    project: "CORE",
    parent: source.key,
    title: "Completed child",
  });
  const grandchild = await createIssue({
    project: "CORE",
    parent: closed.key,
    title: "Open below completed work",
  });
  await createMessage(closed.key, { body: "Completed this work." });
  const closedBefore = await patchIssue(closed.key, { status: "done" });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ width: 1600, height: 1000 });
    await page.goto("/projects/CORE/issues?group=epic");
    const group = page
      .getByRole("row")
      .filter({ has: page.getByRole("button", { name: /Source epic/ }) });
    await group.getByRole("button", { name: "Epic actions ▾" }).click();
    await page.getByRole("combobox", { name: "Merge into another epic" }).selectOption(target.key);
    await expect.poll(() => getIssue(source.key).then((issue) => issue.status)).toBe("done");
    expect((await getIssue(open.key)).parent).toBe(target.key);
    expect((await getIssue(grandchild.key)).parent).toBe(target.key);
    const closedAfter = await getIssue(closed.key);
    expect(closedAfter.parent).toBe(source.key);
    expect(closedAfter.closed_at).toBe(closedBefore.closed_at);
    expect((await listIssueMessages(source.key)).map((read) => read.message.body)).toContain(
      `Merged into ${target.key} (“${target.title}”) from the issues table.`
    );
  } finally {
    await context.close();
  }
});

test("selection actions move, remove and create an epic, whose name can then change", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const target = await createIssue({ project: "CORE", title: "Existing container" });
  await patchIssue(target.key, { labels: ["epic"] });
  const first = await createIssue({ project: "CORE", title: "Fix authentication cookies" });
  const second = await createIssue({ project: "CORE", title: "Audit CSS backgrounds" });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ width: 1600, height: 1000 });
    await page.goto("/projects/CORE/issues");
    const select = async (key: string) =>
      page.getByRole("checkbox", { name: `Select ${key}`, exact: true }).check();
    await select(first.key);
    await select(second.key);
    await page
      .getByRole("combobox", { name: "Move to epic", exact: true })
      .selectOption(target.key);
    await expect
      .poll(async () => [(await getIssue(first.key)).parent, (await getIssue(second.key)).parent])
      .toEqual([target.key, target.key]);
    await expect(page.getByRole("toolbar")).toHaveCount(0);
    await select(first.key);
    await page.getByRole("button", { name: "Take out of epic" }).click();
    await expect.poll(() => getIssue(first.key).then((issue) => issue.parent)).toBeNull();
    await expect(page.getByRole("toolbar")).toHaveCount(0);
    await select(first.key);
    await select(second.key);
    page.once("dialog", (dialog) => dialog.accept("Coordinate onboarding release"));
    await page.getByRole("button", { name: "New epic from these" }).click();
    await expect.poll(() => getIssue(first.key).then((issue) => issue.parent)).not.toBeNull();
    const epicKey = (await getIssue(first.key)).parent;
    if (!epicKey) throw new Error("The new epic was not assigned");
    await expect.poll(() => getIssue(second.key).then((issue) => issue.parent)).toBe(epicKey);
    expect((await getIssue(epicKey)).labels).toContain("epic");
    await page.getByRole("combobox", { name: "Group by" }).selectOption("epic");
    const group = page
      .getByRole("row")
      .filter({ has: page.getByRole("button", { name: /Coordinate onboarding release/ }) });
    await group.getByRole("button", { name: "Epic actions ▾" }).click();
    page.once("dialog", (dialog) => dialog.accept("Release coordination"));
    await page.getByRole("button", { name: "Rename", exact: true }).click();
    await expect
      .poll(() => getIssue(epicKey).then((issue) => issue.title))
      .toBe("Release coordination");
  } finally {
    await context.close();
  }
});

test("a live insertion above a vertically and horizontally scrolled reader keeps their row in place", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const epic = await createIssue({ project: "CORE", title: "Live root epic" });
  await patchIssue(epic.key, { labels: ["epic"] });
  const parent = await createIssue({
    project: "CORE",
    parent: epic.key,
    title: "Closed intermediate",
  });
  await createMessage(parent.key, { body: "Closed intermediate work." });
  await patchIssue(parent.key, { status: "done" });
  for (let index = 0; index < 80; index++)
    await createIssue({ project: "CORE", title: `B work ${String(index).padStart(3, "0")}` });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ width: 1000, height: 800 });
    await page.goto("/projects/CORE/issues?sort=title&size=250");
    await expect(page.getByText("80 of 80 issues", { exact: true })).toBeVisible();
    const scroller = page.getByTestId("issues-table-scroll");
    const original = await scroller.elementHandle();
    const ancestorRequested = Promise.withResolvers<void>();
    const ancestorReady = Promise.withResolvers<void>();
    await page.route(`**/api/v1/issues/${parent.key}`, async (route) => {
      ancestorRequested.resolve();
      await ancestorReady.promise;
      await route.continue();
    });
    await scroller.evaluate((element) => {
      element.scrollTop = 1600;
      element.scrollLeft = 600;
    });
    const visibleAnchor = () =>
      scroller.evaluate((element) => {
        const box = element.getBoundingClientRect();
        const row = document
          .elementFromPoint(box.left + box.width / 2, box.top + 130)
          ?.closest<HTMLElement>("[data-issue-row]");
        return row ? { key: row.dataset.issueRow, top: row.getBoundingClientRect().top } : null;
      });
    await expect.poll(visibleAnchor).not.toBeNull();
    const anchor = await visibleAnchor();
    if (!anchor?.key) throw new Error("No visible issue could be anchored after scrolling");
    expect(await scroller.evaluate((element) => element.scrollLeft)).toBeGreaterThan(0);
    await createIssue({ project: "CORE", parent: parent.key, title: "A newly inserted issue" });
    await ancestorRequested.promise;
    expect(await original?.evaluate((element) => element.isConnected)).toBe(true);
    expect(await scroller.evaluate((element) => element.scrollTop)).toBeGreaterThan(0);
    ancestorReady.resolve();
    await expect(page.getByText("81 of 81 issues", { exact: true })).toBeVisible();
    await expect
      .poll(() =>
        page
          .locator(`[data-issue-row="${anchor.key}"]`)
          .evaluate((element) => element.getBoundingClientRect().top)
      )
      .toBe(anchor.top);
  } finally {
    await context.close();
  }
});

test("a refused bulk write retries after reload while the other operations have already landed @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const epic = await createIssue({ project: "CORE", title: "Bulk source" });
  await patchIssue(epic.key, { labels: ["epic"] });
  const rows: Issue[] = [];
  for (const title of ["First move", "Second move", "Third move"])
    rows.push(await createIssue({ project: "CORE", parent: epic.key, title }));
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ width: 390, height: 844 });
    const second = rows[1];
    const third = rows[2];
    if (!second || !third) throw new Error("Missing bulk fixtures");
    await page.route(`**/api/v1/issues/${second.key}`, async (route) => {
      if (route.request().method() === "PATCH")
        await route.fulfill({
          status: 403,
          json: { error: "Bulk probe refusal", code: "FORBIDDEN" },
        });
      else await route.continue();
    });
    await page.goto("/projects/CORE/issues");
    for (const row of rows) {
      await page.getByRole("checkbox", { name: `Select ${row.key}`, exact: true }).check();
      const bar = await page.getByRole("toolbar").boundingBox();
      const box = await page.getByTestId("issues-table-scroll").boundingBox();
      if (!bar || !box) throw new Error("The selection toolbar or table is missing");
      expect(
        bar.y + bar.height <= box.y || bar.y >= box.y + box.height,
        "the phone selection toolbar never covers the working table"
      ).toBe(true);
    }
    await page.getByRole("button", { name: "Take out of epic" }).click();
    await expect.poll(() => getIssue(third.key).then((issue) => issue.parent)).toBeNull();
    await expect(page.getByRole("region", { name: "Pending changes" })).toContainText(
      `Update ${second.key}`
    );
    await expect(page.getByRole("region", { name: "Pending changes" })).not.toContainText(
      `Update ${third.key}`
    );
    await page.reload();
    await expect(page.getByRole("region", { name: "Pending changes" })).toContainText(
      `Update ${second.key}`
    );
    expect((await getIssue(third.key)).parent).toBeNull();
    await page.unroute(`**/api/v1/issues/${second.key}`);
    await page
      .getByRole("region", { name: "Pending changes" })
      .getByRole("button", { name: "Retry", exact: true })
      .click();
    await expect
      .poll(async () =>
        Promise.all(rows.map((row) => getIssue(row.key).then((issue) => issue.parent)))
      )
      .toEqual([null, null, null]);
    await expect(page.getByRole("region", { name: "Pending changes" })).toHaveCount(0);
  } finally {
    await context.close();
  }
});

test("phone headers, saved widths, popover focus and short-window paging stay usable @phone", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const epic = await createIssue({ project: "CORE", title: "Phone epic" });
  await patchIssue(epic.key, { labels: ["epic"] });
  await createIssue({ project: "CORE", parent: epic.key, title: "Phone member" });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/projects/CORE/issues");
    await page.evaluate(() =>
      localStorage.setItem(
        "dispatch.project-table.CORE",
        JSON.stringify({ sizing: { title: 450 } })
      )
    );
    await page.setViewportSize({ width: 390, height: 844 });
    await page.reload();
    const box = page.getByTestId("issues-table-scroll");
    await page.getByRole("button", { name: "Columns", exact: true }).click();
    await page
      .getByRole("dialog", { name: "Columns" })
      .getByRole("checkbox", { name: "Updated", exact: true })
      .check();
    await page.keyboard.press("Escape");
    await expect(page.getByRole("searchbox", { name: "Search issues" })).toBeVisible();
    const search = await page.getByRole("searchbox", { name: "Search issues" }).boundingBox();
    expect(search?.width).toBeGreaterThan(300);
    const keyHeader = page
      .getByRole("columnheader")
      .filter({ has: page.getByRole("button", { name: "Key", exact: true }) });
    const titleHeader = page
      .getByRole("columnheader")
      .filter({ has: page.getByRole("button", { name: "Title", exact: true }) });
    const keyBefore = await keyHeader.boundingBox();
    const titleBefore = await titleHeader.boundingBox();
    const tableBox = await box.boundingBox();
    if (!keyBefore || !titleBefore || !tableBox)
      throw new Error("The identifying columns are missing");
    expect(titleBefore.x + titleBefore.width - tableBox.x).toBeLessThanOrEqual(
      tableBox.width * 0.6 + 1
    );
    await box.evaluate((element) => {
      element.scrollLeft = element.scrollWidth;
    });
    const keyAfter = await keyHeader.boundingBox();
    const titleAfter = await titleHeader.boundingBox();
    if (!keyAfter || !titleAfter)
      throw new Error("The identifying columns disappeared after scrolling");
    expect(keyAfter.x).toBeCloseTo(keyBefore.x, 0);
    expect(titleAfter.x).toBeCloseTo(titleBefore.x, 0);
    await expect(
      page.locator("[data-issue-row]").first().locator("[data-issue-link]")
    ).toBeVisible();
    const remainingControl = page.getByRole("button", { name: "Filter Updated", exact: true });
    const controlAtEnd = await remainingControl.boundingBox();
    if (!controlAtEnd) throw new Error("The Updated filter is missing");
    await box.evaluate(
      (element, reveal) => {
        element.scrollLeft -= reveal;
      },
      Math.max(0, titleAfter.x + titleAfter.width - controlAtEnd.x)
    );
    const controlBox = await remainingControl.boundingBox();
    if (!controlBox) throw new Error("No control is visible beside the identifying columns");
    expect(controlBox.x).toBeGreaterThanOrEqual(titleAfter.x + titleAfter.width - 1);
    expect(controlBox.x + controlBox.width).toBeLessThanOrEqual(tableBox.x + tableBox.width);
    await remainingControl.click();
    await page.getByRole("combobox", { name: "Search Updated", exact: true }).fill("day");
    await page.keyboard.press("Escape");
    for (const name of ["Priority", "Key", "Title"]) {
      await box.evaluate((element) => {
        element.scrollLeft = 0;
      });
      await page.getByRole("button", { name: `Filter ${name}`, exact: true }).click();
      await page.keyboard.press("Escape");
    }
    await page.getByRole("button", { name: "Columns", exact: true }).click();
    const panel = page.getByRole("dialog", { name: "Columns" });
    await page.keyboard.press("Tab");
    await expect(panel).toBeVisible();
    expect(await panel.evaluate((element) => element.contains(document.activeElement))).toBe(true);
    await page.keyboard.press("Escape");
    await expect(page.getByRole("button", { name: "Columns", exact: true })).toBeFocused();
    await page.getByRole("combobox", { name: "Group by" }).selectOption("epic");
    await expect(page.locator(`[data-issue-row="${epic.key}"]`)).toHaveCount(0);
    await expect(
      page.getByRole("link", { name: `${epic.key} · Phone epic`, exact: true }).first()
    ).toHaveAttribute("href", `/issues/${epic.key}`);
    await expect(page.getByText("1 of 1 issue in 1 group", { exact: true })).toBeVisible();
    await page.screenshot({ path: testInfo.outputPath("issues-table-phone.png"), fullPage: true });
    await page.setViewportSize({ width: 900, height: 400 });
    await page.getByTestId("issues-table-pager").scrollIntoViewIfNeeded();
    const pager = await page.getByTestId("issues-table-pager").boundingBox();
    expect((pager?.y ?? 400) + (pager?.height ?? 1)).toBeLessThanOrEqual(400);
    await page.screenshot({
      path: testInfo.outputPath("issues-table-short-window.png"),
      fullPage: true,
    });
  } finally {
    await context.close();
  }
});

test("compact tables keep three full issues readable after scrolling the page @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  for (const title of [
    "Investigate authorization headers",
    "Review deployment pipeline",
    "Measure database connection pressure",
    "Correct dashboard rendering",
    "Document release verification",
    "Restore unavailable session",
  ])
    await createIssue({ project: "CORE", title });
  const context = await asUser(browser, "alice");
  try {
    for (const viewport of [
      { width: 390, height: 844 },
      { width: 900, height: 400 },
    ]) {
      const page = await context.newPage();
      await page.setViewportSize(viewport);
      await page.goto("/projects/CORE/issues");
      const box = page.getByTestId("issues-table-scroll");
      await expect(box.locator("[data-issue-row]")).toHaveCount(6);
      const capacity = await box.evaluate((element) => {
        const header = element.querySelector("thead")?.getBoundingClientRect().height ?? 0;
        const rows = [...element.querySelectorAll("[data-issue-row]")].slice(0, 3);
        return {
          available: element.clientHeight - header,
          required: rows.reduce((height, row) => height + row.getBoundingClientRect().height, 0),
        };
      });
      expect(capacity.available).toBeGreaterThanOrEqual(capacity.required);
      await box.evaluate((element) => element.scrollIntoView({ block: "start" }));
      await expect
        .poll(() =>
          box.evaluate((element) => {
            const bounds = element.getBoundingClientRect();
            const header =
              element.querySelector("thead")?.getBoundingClientRect().bottom ?? bounds.top;
            return [...element.querySelectorAll("[data-issue-row]")].filter((row) => {
              const rect = row.getBoundingClientRect();
              return (
                rect.top >= header && rect.bottom <= Math.min(bounds.bottom, window.innerHeight)
              );
            }).length;
          })
        )
        .toBeGreaterThanOrEqual(3);
      expect(await box.evaluate((element) => element.scrollTop)).toBe(0);
      await page.close();
    }
  } finally {
    await context.close();
  }
});

test("List and Board share case-insensitive every-label matching", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const both = await createIssue({ project: "CORE", title: "Carries both labels" });
  await patchIssue(both.key, { labels: ["FrontEnd", "BACKEND"] });
  const one = await createIssue({ project: "CORE", title: "Carries one label" });
  await patchIssue(one.key, { labels: ["frontend"] });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/projects/CORE/issues?label=frontend&label=backend");
    await expect(page.locator(`[data-issue-row="${both.key}"]`)).toBeVisible();
    await expect(page.locator(`[data-issue-row="${one.key}"]`)).toHaveCount(0);
    await page.getByRole("button", { name: "Board", exact: true }).click();
    await expect(page.getByRole("link", { name: /Carries both labels/ }).first()).toBeVisible();
    await expect(page.getByText("Carries one label", { exact: true })).toHaveCount(0);
  } finally {
    await context.close();
  }
});

test("compact windows give the table three full rows and let the page scroll @phone", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  for (let index = 0; index < 8; index++)
    await createIssue({ project: "CORE", title: `Compact row ${index}` });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    for (const viewport of [
      { width: 390, height: 844 },
      { width: 900, height: 400 },
    ]) {
      await page.setViewportSize(viewport);
      await page.goto("/projects/CORE/issues");
      await expect(page.locator("[data-issue-row]").nth(2)).toBeAttached();
      const visibleRows = await page.getByTestId("issues-table-scroll").evaluate((box) => {
        const bounds = box.getBoundingClientRect();
        const header = box.querySelector("th")?.getBoundingClientRect().bottom ?? bounds.top;
        return [...box.querySelectorAll("[data-issue-row]")].filter((row) => {
          const rect = row.getBoundingClientRect();
          return rect.top >= header - 1 && rect.bottom <= bounds.bottom + 1;
        }).length;
      });
      expect(visibleRows).toBeGreaterThanOrEqual(3);
      expect(await page.evaluate(() => document.documentElement.scrollHeight > innerHeight)).toBe(
        true
      );
      await page.getByTestId("issues-table-scroll").scrollIntoViewIfNeeded();
      await page.screenshot({
        path: testInfo.outputPath(`three-rows-${viewport.width}.png`),
        fullPage: true,
      });
      await page.getByTestId("issues-table-pager").scrollIntoViewIfNeeded();
      await expect(page.getByRole("combobox", { name: "Rows per page" })).toBeVisible();
    }
  } finally {
    await context.close();
  }
});

test("phone status labels and Done load are readable and reachable @phone", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  await createIssue({ project: "CORE", title: "Triage member" });
  const todo = await createIssue({ project: "CORE", title: "Todo member" });
  await patchIssue(todo.key, { status: "todo" });
  const testing = await createIssue({ project: "CORE", title: "Testing member" });
  await patchIssue(testing.key, { status: "testing" });
  const closed = await createIssue({ project: "CORE", title: "Completed member" });
  await createMessage(closed.key, { body: "Finished this member." });
  await patchIssue(closed.key, { status: "done" });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto("/projects/CORE/issues");
    const strip = page.getByRole("group", { name: "Issues by status" });
    await expect(strip.getByRole("button", { name: "Done: load", exact: true })).toBeAttached();
    for (const button of await strip.getByRole("button").all()) {
      await button.scrollIntoViewIfNeeded();
      const readable = await button.evaluate((element) => {
        const text = document.createRange();
        text.selectNodeContents(element);
        const bounds = element.getBoundingClientRect();
        const glyphs = text.getBoundingClientRect();
        return {
          height: bounds.height,
          readable: glyphs.width <= bounds.width && glyphs.height <= bounds.height,
        };
      });
      expect(readable.height).toBeGreaterThanOrEqual(44);
      expect(readable.readable).toBe(true);
      await expect(button).toBeInViewport();
    }
    await strip.getByRole("button", { name: "Done: load", exact: true }).click();
    await expect(page.locator(`[data-issue-row="${closed.key}"]`)).toBeAttached();
  } finally {
    await context.close();
  }
});

test("a session creator renders its live name, state and conversation link @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const id = "issues-table-creator";
  await setLiveSessions([{ session_id: id, title: "Live table creator" }]);
  const issue = await createIssue(
    { project: "CORE", title: "Created by a session" },
    {
      as: "agent",
      actor: { kind: "session", id, origin: { session_title: "Stored creator name" } },
    }
  );
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto("/projects/CORE/issues");
    const row = page.locator(`[data-issue-row="${issue.key}"]`);
    const creatorCell = row.getByRole("cell").filter({
      has: page.getByRole("link", { name: /Live table creator/ }),
    });
    const author = creatorCell.getByRole("link", { name: /Live table creator/ });
    await author.scrollIntoViewIfNeeded();
    await expect(author).toHaveAttribute("href", new RegExp(id));
    await expect(creatorCell.getByRole("img", { name: "Running", exact: true })).toBeVisible();
    await expect(row.getByText("Stored creator name", { exact: true })).toHaveCount(0);
  } finally {
    await context.close();
    await setLiveSessions([]);
  }
});

test("an unavailable ancestor uses one retry policy per attempt @phone", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const parent = await createIssue({ project: "CORE", title: "Unavailable ancestor" });
  await createIssue({ project: "CORE", parent: parent.key, title: "Open child" });
  await createMessage(parent.key, { body: "Closed ancestor." });
  await patchIssue(parent.key, { status: "done" });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ width: 390, height: 844 });
    let attempts = 0;
    await page.route(`**/api/v1/issues/${parent.key}`, async (route) => {
      attempts++;
      await route.fulfill({ status: 503, json: { error: "Ancestor unavailable" } });
    });
    await page.goto("/projects/CORE/issues");
    await expect(page.getByRole("alert")).toContainText("Could not load some ancestors");
    expect(attempts).toBe(3);
    await page.getByRole("button", { name: "Retry loading ancestors" }).click();
    await expect.poll(() => attempts).toBe(6);
    await expect(page.getByRole("alert")).toContainText("Unknown epic");
    await expect(page.getByRole("table", { name: "Project issues table" })).toBeAttached();
  } finally {
    await context.close();
  }
});

test("a refused merge move leaves the source open without a close reason", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const source = await createIssue({ project: "CORE", title: "Merge source" });
  const target = await createIssue({ project: "CORE", title: "Merge destination" });
  await patchIssue(source.key, { labels: ["epic"] });
  await patchIssue(target.key, { labels: ["epic"] });
  const bad = await createIssue({ project: "CORE", parent: source.key, title: "Refused move" });
  const good = await createIssue({
    project: "CORE",
    parent: source.key,
    title: "Independent move",
  });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.route(`**/api/v1/issues/${bad.key}`, async (route) => {
      if (route.request().method() === "PATCH")
        await route.fulfill({ status: 403, json: { error: "Move refused" } });
      else await route.continue();
    });
    await page.goto("/projects/CORE/issues?group=epic");
    await page.getByRole("button", { name: "Epic actions ▾" }).click();
    await page.getByRole("combobox", { name: "Merge into another epic" }).selectOption(target.key);
    await expect(page.getByRole("alert")).toContainText(bad.key);
    await expect.poll(() => getIssue(good.key).then((issue) => issue.parent)).toBe(target.key);
    expect((await getIssue(source.key)).status).not.toBe("done");
    expect(await listIssueMessages(source.key)).toHaveLength(0);
  } finally {
    await context.close();
  }
});

test("a stopped committed epic retries one key and reload finishes only its remaining adoption @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const first = await createIssue({ project: "CORE", title: "Preserve the caller identity" });
  const closed = await createIssue({ project: "CORE", title: "Document release verification" });
  const context = await asUser(browser, "alice");
  const held = Promise.withResolvers<void>();
  try {
    const page = await context.newPage();
    const posts = watchWrites(page, "POST", "/api/v1/issues");
    const committed = dropFirstAnswer(page, "**/api/v1/issues", "POST", held.promise);
    await page.clock.install();
    await page.goto("/projects/CORE/issues");
    await page.getByRole("checkbox", { name: `Select ${first.key}`, exact: true }).check();
    await page.getByRole("checkbox", { name: `Select ${closed.key}`, exact: true }).check();
    page.once("dialog", (dialog) => void dialog.accept("Recover one created epic"));
    await page.getByRole("button", { name: "New epic from these", exact: true }).click();
    expect(await committed).toBe(201);
    await page.clock.fastForward(SEND_DEADLINE_MS + 1);
    const pending = page.getByRole("region", { name: "Pending changes" });
    await pending.getByRole("button", { name: "Stop trying", exact: true }).click();
    await expect(pending).toContainText("Not saved");
    held.resolve();
    await page.reload();
    await expect(pending).toContainText("Not saved");
    await pending.getByRole("button", { name: "Retry", exact: true }).click();
    const interrupted = page.getByRole("region", { name: "Interrupted table action" });
    await expect(interrupted).toContainText("not yet adopted");
    await expect.poll(() => posts.length).toBe(2);
    expect(posts[0]?.key).toEqual(expect.any(String));
    expect(posts[1]?.key).toBe(posts[0]?.key);
    const epics = (await listIssues("CORE")).filter((issue) => issue.labels?.includes("epic"));
    expect(epics).toHaveLength(1);
    const epic = epics[0];
    if (!epic) throw new Error("The committed epic is missing");
    await expect(interrupted).toContainText(epic.key);
    expect((await getIssue(first.key)).parent).toBeNull();
    await createMessage(closed.key, { body: "Completed while the action was interrupted." });
    await expect(
      page.getByRole("region", { name: "Project issues" }).getByRole("alert")
    ).toHaveCount(0);
    await patchIssue(closed.key, { status: "done" });
    await page.reload();
    await expect(interrupted).toContainText(epic.key);
    expect((await getIssue(first.key)).parent).toBeNull();
    await interrupted.getByRole("button", { name: "Finish", exact: true }).click();
    await expect.poll(() => getIssue(first.key).then((issue) => issue.parent)).toBe(epic.key);
    expect((await getIssue(closed.key)).parent).toBeNull();
    await expect(interrupted).toHaveCount(0);
    expect(
      (await listIssues("CORE")).filter((issue) => issue.labels?.includes("epic"))
    ).toHaveLength(1);
    expect(posts).toHaveLength(2);
  } finally {
    held.resolve();
    await context.close();
  }
});

test("reload mid Start next finishes the recorded order without repeating its completed rank @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const rows = [];
  for (const title of [
    "Queue anchor",
    "Review access rules",
    "Correct query ordering",
    "Publish operator documentation",
  ]) {
    const issue = await createIssue({ project: "CORE", title });
    await patchIssue(issue.key, { status: "todo", labels: ["legion"] });
    rows.push(issue);
  }
  const [anchor, first, middle, last] = rows;
  if (!anchor || !first || !middle || !last) throw new Error("Queue fixtures are missing");
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    const completed = watchWrites(page, "PATCH", `/api/v1/issues/${last.key}`);
    let mode: "hold" | "refuse" | "through" = "hold";
    let held = false;
    await page.route(`**/api/v1/issues/${middle.key}`, async (route) => {
      if (route.request().method() !== "PATCH" || mode === "through") return route.fallback();
      if (mode === "hold") {
        held = true;
        return;
      }
      await route.fulfill({ status: 403, json: { error: "Rank refused during reload" } });
    });
    await page.goto("/projects/CORE/issues");
    for (const issue of [first, middle, last])
      await page.getByRole("checkbox", { name: `Select ${issue.key}`, exact: true }).check();
    await page
      .getByRole("toolbar")
      .getByRole("button", { name: "Start next", exact: true })
      .click();
    await expect.poll(() => held).toBe(true);
    expect(completed).toHaveLength(1);
    mode = "refuse";
    await page.reload();
    const interrupted = page.getByRole("region", { name: "Interrupted table action" });
    await expect(interrupted).toContainText("Start next");
    await expect(page.getByRole("region", { name: "Pending changes" })).toContainText("Not saved");
    mode = "through";
    await interrupted.getByRole("button", { name: "Finish", exact: true }).click();
    await expect(interrupted).toHaveCount(0);
    for (const [index, issue] of [first, middle, last, anchor].entries())
      await expect(
        page
          .getByRole("row", { name: `${issue.key} ${issue.title}`, exact: true })
          .getByRole("cell", { name: `#${index + 1}`, exact: true })
      ).toBeVisible();
    expect(completed).toHaveLength(1);
    await page.reload();
    await expect(interrupted).toHaveCount(0);
  } finally {
    await context.close();
  }
});

test("reload mid merge moves current open descendants then records one reason and closes @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const source = await createIssue({ project: "CORE", title: "Source workflow" });
  const target = await createIssue({ project: "CORE", title: "Combined destination" });
  await patchIssue(source.key, { labels: ["epic"] });
  await patchIssue(target.key, { labels: ["epic"] });
  const bridge = await createIssue({
    project: "CORE",
    parent: source.key,
    title: "Archived intermediate",
  });
  const pendingChild = await createIssue({
    project: "CORE",
    parent: bridge.key,
    title: "Retain the retry key",
  });
  const completedChild = await createIssue({
    project: "CORE",
    parent: source.key,
    title: "Independent completed move",
  });
  await createMessage(bridge.key, { body: "Intermediate completed." });
  await patchIssue(bridge.key, { status: "done" });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    const completed = watchWrites(page, "PATCH", `/api/v1/issues/${completedChild.key}`);
    let mode: "hold" | "refuse" | "through" = "hold";
    await page.route(`**/api/v1/issues/${pendingChild.key}`, async (route) => {
      if (route.request().method() !== "PATCH" || mode === "through") return route.fallback();
      if (mode === "hold") return;
      await route.fulfill({ status: 403, json: { error: "Move refused during reload" } });
    });
    await page.goto("/projects/CORE/issues?group=epic");
    await page.getByRole("button", { name: "Epic actions ▾" }).click();
    await page.getByRole("combobox", { name: "Merge into another epic" }).selectOption(target.key);
    await expect
      .poll(() => getIssue(completedChild.key).then((issue) => issue.parent))
      .toBe(target.key);
    mode = "refuse";
    await page.reload();
    const interrupted = page.getByRole("region", { name: "Interrupted table action" });
    await expect(interrupted).toContainText(`Merge ${source.key} into ${target.key}`);
    expect((await getIssue(source.key)).status).not.toBe("done");
    expect(await listIssueMessages(source.key)).toHaveLength(0);
    const added = await createIssue({
      project: "CORE",
      parent: bridge.key,
      title: "New descendant created during interruption",
    });
    mode = "through";
    await interrupted.getByRole("button", { name: "Finish", exact: true }).click();
    await expect.poll(() => getIssue(source.key).then((issue) => issue.status)).toBe("done");
    for (const child of [pendingChild, completedChild, added])
      expect((await getIssue(child.key)).parent).toBe(target.key);
    expect((await getIssue(bridge.key)).parent).toBe(source.key);
    expect((await listIssueMessages(source.key)).map(({ message }) => message.body)).toEqual([
      expect.stringContaining(`Merged into ${target.key}`),
    ]);
    expect(completed).toHaveLength(1);
    await expect(interrupted).toHaveCount(0);
    await page.reload();
    await expect(interrupted).toHaveCount(0);
    expect(await listIssueMessages(source.key)).toHaveLength(1);
  } finally {
    await context.close();
  }
});
