import { expect, test } from "@playwright/test";

import {
  createAsk,
  createIssue,
  createIssueArtifact,
  createProject,
  getIssue,
  patchIssue,
  putIssueState,
} from "./api";
import { filterPicker, pickFilterOption } from "./filters";
import { resetDatabase } from "./seed";
import { navigateInApp } from "./sends";
import { asUser } from "./users";

/**
 * The project page's one walk: nine issues across eight statuses, the table over them, eight
 * filter presses, and the Documents tab down to a document created in its editor. What the
 * page draws over one fixed answer - its fit on a phone, its long search, its reference
 * closure, its status groups and its rows - is in `project.stub.e2e.ts`.
 */

const session = { actor: { id: "e2e-project", kind: "session" as const }, as: "agent" as const };

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
    // Every row below is named inside the issue list. An issue's title is on the page twice
    // whenever a pointer rests on its row: the row's own link, and the reference preview card
    // that link opens (`features/refs/RefPreview.tsx`, whose heading is the key and the title
    // and whose link's accessible name is `CORE-1Triage work Triage`, the status pill with
    // them). A press that leaves the pointer over a row - every filter press here, at the width
    // where the controls sit above the table - opens that card, so an unscoped `getByText` names
    // two elements and fails as a strict-mode violation rather than as a product fault.
    const issues = page.getByRole("region", { name: "Project issues" });

    // Closed issues load when the Status filter lets Done through; the bar's own Done cell,
    // which only exists once they are loaded, then narrows to them.
    await pickFilterOption(page, "Status", "Done");
    await page.getByRole("button", { name: "Done 1", exact: true }).click();
    await expect(issues.getByText("Done work", { exact: true })).toBeVisible();
    await expect(issues.getByText("Child work", { exact: true })).toHaveCount(0);
    await page.getByRole("button", { name: "Testing 1", exact: true }).click();
    await expect(issues.getByText("Child work", { exact: true })).toBeVisible();
    await expect(issues.getByText("Backlog work", { exact: true })).toHaveCount(0);
    await pickFilterOption(page, "Status", "Triage");
    await expect(issues.getByText("Triage work", { exact: true })).toBeVisible();
    await expect(issues.getByText("Child work", { exact: true })).toBeVisible();
    await expect(page).toHaveURL(/status=testing.*status=triage/);
    await filterPicker(page, "Status").click();
    await page.getByRole("button", { name: "Clear status", exact: true }).click();
    await page.keyboard.press("Escape");
    await expect(issues.getByText("Backlog work", { exact: true })).toBeVisible();
    await pickFilterOption(page, "Labels", "frontend");
    await expect(issues.getByText("Needs attention", { exact: true })).toBeVisible();
    await expect(issues.getByText("Backlog work", { exact: true })).toHaveCount(0);
    await filterPicker(page, "Labels").click();
    await page.getByRole("button", { name: "Clear labels", exact: true }).click();
    await page.keyboard.press("Escape");
    const search = page.getByRole("searchbox", { name: "Search issues" });
    await search.fill("backlog");
    await expect(issues.getByText("Backlog work", { exact: true })).toBeVisible();
    await expect(issues.getByText("Child work", { exact: true })).toHaveCount(0);
    await search.clear();
    await page.getByRole("button", { name: "Needs you", exact: true }).click();
    await expect(issues.getByText("Needs attention", { exact: true })).toBeVisible();
    await expect(issues.getByText("Backlog work", { exact: true })).toHaveCount(0);

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
