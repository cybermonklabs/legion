import { expect, type Locator, test } from "@playwright/test";

import {
  createAsk,
  createComment,
  createIssue,
  createIssueArtifact,
  createProject,
  createProjectDocument,
  getIssue,
  patchIssue,
  putIssueState,
} from "./api";
import { filterPicker, pickFilterOption } from "./filters";
import { resetDatabase } from "./seed";
import { navigateInApp } from "./sends";
import { assertPageFits } from "./unclipped";
import { asUser } from "./users";

const session = { actor: { id: "e2e-project", kind: "session" as const }, as: "agent" as const };

async function expectTouchTarget(locator: Locator): Promise<void> {
  const box = await locator.boundingBox();
  expect(box?.height ?? 0).toBeGreaterThanOrEqual(44);
}

test.beforeEach(async () => {
  await resetDatabase();
});

// SLOW(>=2s): the project's table, status and label filters, and a created project document.
test("project table filters narrow issues; documents stay scoped to the project", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const triage = await createIssue({ project: "CORE", title: "Triage work" });
  const icebox = await createIssue({ project: "CORE", title: "Icebox work" });
  const backlog = await createIssue({ project: "CORE", title: "Backlog work" });
  const parent = await createIssue({ project: "CORE", title: "Parent work" });
  await createIssueArtifact(parent.key, {
    content: "# Issue-only document\n",
    name: "Issue-only document",
  });
  const needsYou = await createIssue({ project: "CORE", title: "Needs attention" });
  const unread = await createIssue({ project: "CORE", title: "Unread work" });
  const child = await createIssue({ parent: parent.key, project: "CORE", title: "Child work" });
  const done = await createIssue({ project: "CORE", title: "Done work" });
  await patchIssue(triage.key, { status: "triage" });
  await patchIssue(icebox.key, { status: "icebox" });
  await patchIssue(backlog.key, { status: "backlog" });
  await patchIssue(parent.key, { status: "todo" });
  await patchIssue(needsYou.key, { labels: ["frontend"], status: "todo" });
  await patchIssue(unread.key, { labels: ["backend"], status: "in_progress" });
  await patchIssue(child.key, { status: "testing" });
  await patchIssue(done.key, { status: "done" });
  await createAsk(needsYou.key, { question: "Does this need review?" }, session);
  // The negative control for the header's pill: another person's question in this same project.
  const bobs = await createIssue({ project: "CORE", title: "Bob's work" }, { login: "bob" });
  await patchIssue(bobs.key, { status: "in_progress" });
  await createAsk(bobs.key, { question: "Does Bob need to answer this?" }, session);
  // Seven read marks, one round of round trips: the rows differ, the writes do not wait on
  // each other.
  await Promise.all(
    [triage, icebox, backlog, parent, needsYou, child, done].map(async (issue) => {
      const latest = await getIssue(issue.key);
      await putIssueState(issue.key, { last_read_seq: latest.last_seq });
    })
  );

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await page.setViewportSize({ width: 1280, height: 800 });

  try {
    await page.goto("/projects/CORE");
    await expect(page).toHaveTitle("CORE · Core · Dispatch");
    await expect(page.getByRole("heading", { name: "Core" })).toBeVisible();
    // The pill counts this project's questions waiting on the signed-in reader.
    await expect(
      page
        .locator(`[data-issue-row="${child.key}"]`)
        .getByRole("link", { name: parent.key, exact: true })
    ).toBeVisible();
    const blockedOnYou = page.getByRole("link", { name: "Blocked on you · 1" });
    const issueTable = page.getByRole("table", { name: "Project issues table" });
    await expect(issueTable).toBeVisible();
    expect((await page.getByTestId("issues-table-scroll").boundingBox())?.y).toBeLessThan(420);
    const blockerBox = await blockedOnYou.boundingBox();
    const mainBox = await page.getByTestId("main-content").boundingBox();
    if (blockerBox === null || mainBox === null) {
      throw new Error("project header is not visible");
    }
    expect(blockerBox.x + blockerBox.width).toBeLessThanOrEqual(mainBox.x + mainBox.width);
    await expect(page.getByRole("tab", { name: "Issues" })).toHaveAttribute(
      "aria-selected",
      "true"
    );
    await page.getByRole("button", { name: "Done: load", exact: true }).click();
    await expect(page.getByText("Done work", { exact: true })).toBeVisible();
    await expect(page.getByText("Child work", { exact: true })).toHaveCount(0);
    await page.getByRole("button", { name: "Testing 1", exact: true }).click();
    await expect(page.getByText("Child work", { exact: true })).toBeVisible();
    await expect(page.getByText("Backlog work", { exact: true })).toHaveCount(0);
    await pickFilterOption(page, "Status", "Triage");
    await expect(page.getByText("Triage work", { exact: true })).toBeVisible();
    await expect(page.getByText("Child work", { exact: true })).toBeVisible();
    await expect(page).toHaveURL(/status=testing.*status=triage/);
    await filterPicker(page, "Status").click();
    await page.getByRole("button", { name: "Clear status", exact: true }).click();
    await page.keyboard.press("Escape");
    await expect(page.getByText("Backlog work", { exact: true })).toBeVisible();
    await pickFilterOption(page, "Labels", "frontend");
    await expect(page.getByText("Needs attention", { exact: true })).toBeVisible();
    await expect(page.getByText("Backlog work", { exact: true })).toHaveCount(0);
    await filterPicker(page, "Labels").click();
    await page.getByRole("button", { name: "Clear labels", exact: true }).click();
    await page.keyboard.press("Escape");
    const search = page.getByRole("searchbox", { name: "Search issues" });
    await search.fill("backlog");
    await expect(page.getByText("Backlog work", { exact: true })).toBeVisible();
    await expect(page.getByText("Child work", { exact: true })).toHaveCount(0);
    await search.clear();
    await page.getByRole("button", { name: "Needs you", exact: true }).click();
    await expect(page.getByText("Needs attention", { exact: true })).toBeVisible();
    await expect(page.getByText("Backlog work", { exact: true })).toHaveCount(0);

    await page.getByRole("tab", { name: "Documents" }).click();
    await expect(page).toHaveURL("/projects/CORE/documents");
    await page.getByRole("button", { name: "New document" }).click();
    await page.getByRole("textbox", { name: "Title" }).fill("Design notes");
    await page.getByRole("button", { name: "Create" }).click();
    await expect(page).toHaveURL("/projects/CORE/documents/design-notes");
    await expect(page.getByRole("textbox", { name: "Document editor" })).toContainText(
      "Design notes"
    );
    await navigateInApp(page, "/projects/CORE/documents");
    await expect(page.getByRole("link", { name: "Design notes", exact: true })).toBeVisible();
    await page.screenshot({ path: testInfo.outputPath("project-documents.png"), fullPage: true });
    await expect(page.getByText("Issue-only document", { exact: true })).toHaveCount(0);
  } finally {
    await context.close();
  }
});

// The walk above is the same work in both projects; this is the one claim the phone viewport
// can break: the project page fits its screen, and the tabs, filters and document controls a
// finger presses are touch-sized.
test("the project page fits a phone screen with touch-sized tabs, filters and document controls @phone", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name !== "iphone",
    "the 44 px touch rule and the fit are the phone layout's"
  );
  await createProject({ key: "CORE", name: "Core" });
  const todo = await createIssue({ project: "CORE", title: "Needs attention" });
  await patchIssue(todo.key, { labels: ["frontend"], status: "todo" });
  await createProjectDocument("CORE", { content: "# Design notes\n", name: "Design notes" });

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/projects/CORE/documents");
    // The page draws a loading skeleton first, and one of its placeholder rows is 472 px wide,
    // which zooms a 390 px screen out to 473. The claim is about the page, so wait for the
    // page's own content before measuring it.
    await expect(page.getByRole("button", { name: "New document" })).toBeVisible();
    await assertPageFits(page);
    await expectTouchTarget(page.getByRole("tab", { name: "Issues" }));
    await expectTouchTarget(page.getByRole("button", { name: "New document" }));
    await expectTouchTarget(page.getByRole("button", { name: /Drop a file here/ }));
    await page.getByRole("tab", { name: "Issues" }).click();
    await expect(page.getByRole("table", { name: "Project issues table" })).toBeVisible();
    await expectTouchTarget(filterPicker(page, "Status"));
    await expectTouchTarget(filterPicker(page, "Labels"));
    await expectTouchTarget(page.getByRole("button", { exact: true, name: "Needs you" }));
    await expectTouchTarget(page.getByRole("button", { name: "Unread" }));
    await filterPicker(page, "Status").click();
    await expectTouchTarget(page.getByRole("option", { name: /^Todo,/ }));
    await page.keyboard.press("Escape");
    await assertPageFits(page);
  } finally {
    await context.close();
  }
});

test("a 1,000-character table search stays inside the page @phone", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  await createIssue({ project: "CORE", title: "Core work" });
  const query = "x".repeat(1_000);

  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  if (testInfo.project.name === "chromium") {
    await page.setViewportSize({ height: 800, width: 1280 });
  }

  try {
    await page.goto(`/projects/CORE/issues?q=${query}`);
    const search = page.getByRole("searchbox", { name: "Search issues" });
    await expect(search).toHaveValue(query);
    await expect(page.getByText("No issues match these filters.")).toBeVisible();
    await assertPageFits(page);
    const box = await search.boundingBox();
    const viewport = page.viewportSize();
    if (!box || !viewport) throw new Error("The search box or viewport could not be measured");
    expect(box.x + box.width).toBeLessThanOrEqual(viewport.width);
  } finally {
    await alice.close();
  }
});

test("clearing the table search keeps the next keystroke in the search box", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  await createIssue({ project: "CORE", title: "Core work" });

  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  try {
    await page.goto("/projects/CORE/issues");
    const search = page.getByRole("searchbox", { name: "Search issues" });
    await search.click();
    await page.keyboard.type("core");
    await expect(page).toHaveURL(/[?&]q=core(?:&|$)/);
    await page.keyboard.press("ControlOrMeta+A");
    await page.keyboard.press("Backspace");
    await expect(page).not.toHaveURL(/[?&]q=/);
    await expect(search).toHaveValue("");
    await expect(search).toBeFocused();
    await page.keyboard.type("w");
    await expect(search).toHaveValue("w");
    await expect(page).toHaveURL(/[?&]q=w(?:&|$)/);
  } finally {
    await alice.close();
  }
});

test("an issue's Artifacts tab lists its reference closure and a document page lists its referrers", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Reference source" });
  const related = await createIssue({ project: "CORE", title: "Referenced artifact" });
  await createIssueArtifact(issue.key, {
    content: "See dispatch://CORE/artifact/design-notes",
    name: "Source doc",
  });
  await createIssueArtifact(related.key, { content: "diagram", name: "Diagram" });
  await createProjectDocument("CORE", {
    content: `See dispatch://${related.key}/artifact/diagram`,
    name: "Design notes",
  });
  await createComment(issue.key, { body: "See dispatch://CORE/artifact/design-notes" }, session);
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await page.goto(`/issues/${issue.key}/artifacts`);
    await page.getByText(/References \(\d+\)/).click();
    const references = page.getByRole("region", { name: "References" });
    await expect(references.getByRole("link", { name: "Design notes" })).toHaveAttribute(
      "href",
      "/projects/CORE/documents/design-notes"
    );
    await expect(references).toContainText("via artifact");
    await expect(references).toContainText("depth 1");
    await expect(references).toContainText("depth 2");
    const tabs = page.getByRole("main").getByRole("tab");
    await expect(tabs).toHaveCount(8);
    for (const [index, name] of [
      "Spec",
      "Verification",
      "Plan",
      "Conversation",
      "Children",
      "Artifacts (2)",
      "Activity",
      "Decisions",
    ].entries()) {
      await expect(tabs.nth(index)).toHaveAccessibleName(name);
    }
    await expect(page.getByRole("button", { name: /make primary/i })).toHaveCount(0);
    await page.getByRole("link", { name: "Design notes" }).click();
    await expect(page).toHaveURL("/projects/CORE/documents/design-notes");
    await expect(page.getByRole("region", { name: "Referenced by" })).toContainText(
      `Comment · ${issue.key}`
    );
    // A document mention deep-links to the block that carries it.
    await expect(page.getByRole("link", { name: `Artifact · ${issue.key}` })).toHaveAttribute(
      "href",
      new RegExp(`^/issues/${issue.key}/artifacts/source-doc#b-`)
    );
    await page.screenshot({
      path: testInfo.outputPath("project-document-references.png"),
      fullPage: true,
    });
  } finally {
    await context.close();
  }
});

test("status groups stay expandable and the project key sits beside its name", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const open = await createIssue({ project: "CORE", title: "Still open" });
  await patchIssue(open.key, { status: "in_progress" });
  const shipped = await createIssue({ project: "CORE", title: "Already shipped" });
  await patchIssue(shipped.key, { status: "done" });

  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  await page.setViewportSize({ height: 900, width: 1280 });

  try {
    await page.goto("/projects/CORE/issues");

    const shippedLink = page.getByRole("link", {
      name: `${shipped.key} Already shipped`,
      exact: true,
    });
    await expect(shippedLink).toHaveCount(0);
    await page.getByRole("button", { name: "Done: load", exact: true }).click();
    await expect(shippedLink).toBeVisible();
    await page.getByRole("combobox", { name: "Group by" }).selectOption("status");
    const done = page.getByRole("button", { name: /Done.*1 issue/ });
    await done.click();
    await expect(shippedLink).toHaveCount(0);
    await done.click();
    await expect(shippedLink).toBeVisible();

    // The key pill sits beside the name, not across the row from it.
    const heading = page.getByRole("heading", { level: 1, name: "Core" });
    const pill = page.getByRole("link", { exact: true, name: "CORE" });
    // The rendered text's own right edge, not the heading's box: a heading allowed to grow
    // keeps its text at the left and stretches the box, so the box edge stays beside the pill
    // while the name sits hundreds of pixels away from it.
    const textRight = await heading.evaluate((node) => {
      const range = node.ownerDocument.createRange();
      range.selectNodeContents(node);
      return range.getBoundingClientRect().right;
    });
    const pillBox = await pill.boundingBox();
    if (pillBox === null) {
      throw new Error("project header is not visible");
    }
    const gap = pillBox.x - textRight;
    expect(gap).toBeGreaterThanOrEqual(0);
    // The key beside the name is the one-strip desktop layout: below `md` the name, key and
    // blocker pill share the first row by design, the name taking the space the other two leave.
    expect(gap).toBeLessThanOrEqual(24);
    // The full name is available even when the heading truncates.
    await expect(heading).toHaveAttribute("title", "Core");
  } finally {
    await alice.close();
  }
});

test("a table keeps its key whole and uses its title cell without overflowing the phone @phone", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    title:
      "Smoke rig checkpoints 5 and 7 assert the retired commit-identity and handoff-cleanup design: implementer+ses_identity, Legion-Session trailer, and the reviewer App authoring the .legion/ deletion",
  });
  await patchIssue(issue.key, { labels: ["smoke"], priority: 2, status: "todo" });

  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  if (testInfo.project.name === "chromium") {
    await page.setViewportSize({ height: 900, width: 1280 });
  }

  try {
    await page.goto("/projects/CORE/issues");
    const row = page.getByRole("row", { name: `${issue.key} ${issue.title}`, exact: true });
    await expect(row).toBeVisible();
    const layout = await row.evaluate((node) => {
      const key = node.querySelector("a:not([data-issue-link])");
      const title = node.querySelector("[data-issue-link]");
      const cell = title?.closest("td");
      if (!key || !title || !cell) throw new Error("the table row has no key or title cell");
      const rects = (element: Element) => {
        const range = node.ownerDocument.createRange();
        range.selectNodeContents(element);
        return [...range.getClientRects()];
      };
      const titleRects = rects(title);
      const cellBox = cell.getBoundingClientRect();
      const style = getComputedStyle(cell);
      return {
        keyLines: new Set(rects(key).map((rect) => Math.round(rect.top))).size,
        availableTitleWidth:
          cellBox.width - parseFloat(style.paddingLeft) - parseFloat(style.paddingRight),
        titleOverflows: titleRects.some(
          (rect) => rect.left < cellBox.left || rect.right > cellBox.right
        ),
        titleWidth:
          Math.max(...titleRects.map((rect) => rect.right)) -
          Math.min(...titleRects.map((rect) => rect.left)),
      };
    });
    expect(layout.keyLines).toBe(1);
    expect(layout.titleOverflows).toBe(false);
    expect(layout.titleWidth).toBeGreaterThanOrEqual(layout.availableTitleWidth * 0.8);
    const scroll = await page
      .getByTestId("issues-table-scroll")
      .evaluate((element) => ({ client: element.clientWidth, scroll: element.scrollWidth }));
    expect(scroll.scroll).toBeGreaterThan(scroll.client);
    await assertPageFits(page);
  } finally {
    await alice.close();
  }
});
