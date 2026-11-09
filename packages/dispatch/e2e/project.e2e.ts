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
import { pickFilterOption } from "./filters";
import { resetDatabase } from "./seed";
import { navigateInApp } from "./sends";
import { asUser } from "./users";

/**
 * The project page's one walk: nine issues across eight statuses, the grouped board over them,
 * eight filter presses, and the Documents tab down to a document created in its editor. What
 * the page draws over one fixed answer - its fit on a phone, its filter chips, its reference
 * closure, its status bands and its rows - is in `project.stub.e2e.ts`.
 */

const session = { actor: { id: "e2e-project", kind: "session" as const }, as: "agent" as const };

test.beforeEach(async () => {
  await resetDatabase();
});

// SLOW(>=2s): one row for the whole project page - nine issues seeded across eight statuses,
// the grouped board over them, eight filter presses, and the Documents tab down to a created
// document with its editor. Each clause of the title is a press the row has to make.
test("project page groups issues by status in board order; filters narrow issues; documents stay scoped to the project", async ({
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
    // The pill counts this project's questions waiting on the reader themselves: one, Bob's
    // not among them.
    const blockedOnYou = page.getByRole("link", { name: "Blocked on you · 1" });
    await expect(blockedOnYou).toHaveAttribute("href", "/");
    // The filter strip is the first content row under the dense header; the issue list
    // follows it. Density contract: that first row starts within 120px of the top.
    const filterStrip = page.getByRole("button", { name: /Filters · \d+ active/ });
    const stripBox = await filterStrip.boundingBox();
    const issueList = page.getByRole("region", { name: "Project issues" });
    const issueListBox = await issueList.boundingBox();
    if (stripBox === null || issueListBox === null) {
      throw new Error("project issue list is not visible");
    }
    expect(stripBox.y).toBeLessThanOrEqual(120);
    expect(issueListBox.y).toBeLessThanOrEqual(stripBox.y + stripBox.height + 24);
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
    await expect(page.locator("details > summary")).toHaveText([
      "Triage (1)",
      "Icebox (1)",
      "Backlog (1)",
      "Todo (2)",
      "In progress (2)",
      "Testing (1)",
      "Done (1)",
    ]);
    await expect(page.getByRole("group", { name: "Done (1)" })).not.toHaveAttribute("open", "");
    await expect(
      page
        .getByRole("listitem", { name: /CORE-.*Child work/ })
        .getByRole("link", { name: parent.key })
    ).toHaveAttribute("href", `/issues/${parent.key}`);

    // Status is a multi-select: one status narrows, a second widens (OR), each is a chip. Rows
    // and chips read the display label; the URL keeps the key.
    await page.getByRole("button", { name: "Filters · 0 active" }).click();
    await pickFilterOption(page, "Status", "Testing");
    await expect(page).toHaveURL(/\?status=testing$/);
    await expect(page.getByText("Child work")).toBeVisible();
    await expect(page.getByText("Triage work")).toHaveCount(0);
    await pickFilterOption(page, "Status", "Triage");
    await expect(page).toHaveURL(/\?status=testing&status=triage$/);
    await expect(page.getByText("Triage work")).toBeVisible();
    await expect(page.getByText("Backlog work")).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Filters · 2 active" })).toBeVisible();
    await page.getByRole("button", { name: "Remove Status: Testing filter" }).click();
    await expect(page.getByText("Child work")).toHaveCount(0);
    await page.getByRole("button", { name: "Remove Status: Triage filter" }).click();
    await expect(page.getByText("Backlog work")).toBeVisible();

    // Removing the last filter leaves the strip open under the user; only they close it.
    await expect(page.getByRole("button", { name: "Filters · 0 active" })).toHaveAttribute(
      "aria-expanded",
      "true"
    );
    await page.getByRole("button", { exact: true, name: "Needs you" }).click();
    await expect(page.getByText("Needs attention")).toBeVisible();
    await expect(page.getByText("Triage work")).toHaveCount(0);
    await page.getByRole("button", { exact: true, name: "Needs you" }).click();

    await page.getByRole("button", { exact: true, name: "Unread" }).click();
    await expect(page.getByText("Unread work")).toBeVisible();
    await expect(page.getByText("Needs attention")).toHaveCount(0);
    await page.getByRole("button", { exact: true, name: "Unread" }).click();

    await pickFilterOption(page, "Labels", "frontend");
    await expect(page.getByText("Needs attention")).toBeVisible();
    await expect(page.getByText("Unread work")).toHaveCount(0);
    await page.getByRole("searchbox", { name: "Search issues" }).fill("attention");
    await expect(page.getByText("Needs attention")).toBeVisible();
    await page.screenshot({ path: testInfo.outputPath("project-issues.png"), fullPage: true });

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
