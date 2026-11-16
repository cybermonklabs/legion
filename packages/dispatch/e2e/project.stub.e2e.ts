/** What the project page draws over one fixed answer: the fit and the touch targets a phone
 *  gives it, a filter chip too long for its row, the strip a cleared search leaves open, the
 *  reference closure an issue's Artifacts tab lists, the status groups and the header's key,
 *  and the width a table row gives a long title.
 *
 *  Each reads what the browser does, so each runs against a recording rather than against a
 *  harness (packages/dispatch/AGENTS.md, "The stubbed layer"). The walk that seeds nine issues,
 *  presses eight filters and creates a document in its editor stayed in `project.e2e.ts`.
 */
import { expect, type Locator } from "@playwright/test";

import {
  createComment,
  createIssue,
  createIssueArtifact,
  createProject,
  createProjectDocument,
  patchIssue,
} from "./api";
import { filterPicker, pickFilterOption } from "./filters";
import { stubbed } from "./stub/stub-test";
import { assertPageFits } from "./unclipped";

const session = { actor: { id: "e2e-project", kind: "session" as const }, as: "agent" as const };

async function expectTouchTarget(locator: Locator): Promise<void> {
  const box = await locator.boundingBox();
  expect(box?.height ?? 0).toBeGreaterThanOrEqual(44);
}

/** One labelled issue and one document, for the phone's own fit. */
const phoneTest = stubbed(async () => {
  await createProject({ key: "CORE", name: "Core" });
  const todo = await createIssue({ project: "CORE", title: "Needs attention" });
  await patchIssue(todo.key, { labels: ["frontend"], status: "todo" });
  await createProjectDocument("CORE", { content: "# Design notes\n", name: "Design notes" });
  return { project: "CORE" };
});

/** One issue, for the rows that read the filter strip. */
const filterTest = stubbed(async () => {
  await createProject({ key: "CORE", name: "Core" });
  await createIssue({ project: "CORE", title: "Core work" });
  return { project: "CORE", query: "x".repeat(1_000) };
});

/** A document citing an artifact of another issue, and a comment citing the document. */
const referencesTest = stubbed(async () => {
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
  return { issue: issue.key };
});

/** One open issue and one closed, so the Done band has something to open. */
const bandsTest = stubbed(async () => {
  await createProject({ key: "CORE", name: "Core" });
  const open = await createIssue({ project: "CORE", title: "Still open" });
  await patchIssue(open.key, { status: "in_progress" });
  const shipped = await createIssue({ project: "CORE", title: "Already shipped" });
  await patchIssue(shipped.key, { status: "done" });
  return { project: "CORE", shipped: shipped.key };
});

/** One issue whose title runs well past a row. */
const longTitleTest = stubbed(async () => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    title:
      "Smoke rig checkpoints 5 and 7 assert the retired commit-identity and handoff-cleanup design: implementer+ses_identity, Legion-Session trailer, and the reviewer App authoring the .legion/ deletion",
  });
  await patchIssue(issue.key, { labels: ["smoke"], priority: 2, status: "todo" });
  return { issue: issue.key, project: "CORE", title: issue.title };
});

// The walk in `project.e2e.ts` is the same work in both projects; this is the one claim the
// phone viewport can break: the project page fits its screen, and the tabs, filters and document
// controls a finger presses are touch-sized.
phoneTest(
  "the project page fits a phone screen with touch-sized tabs, filters and document controls @phone @phone-only @pr",
  async ({ page, scene }) => {
    await page.goto(`/projects/${scene.project}/documents`);
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
  }
);

// The search filter holds up to 1,000 characters, and its chip stays inside its row: that text
// on one line makes the page 7,535 px wide at 1280, and a phone zooms out until it is 1,560 px.
filterTest(
  "a 1,000-character table search stays inside the page @phone @pr",
  async ({ page, scene }, testInfo) => {
    // The desktop case this row ran in `chromium` on the real stack, at the width that makes
    // the claim: that text on one line makes the page 7,535 px wide at 1280.
    if (testInfo.project.name === "stub") {
      await page.setViewportSize({ height: 800, width: 1280 });
    }
    await page.goto(`/projects/${scene.project}/issues?q=${scene.query}`);
    const search = page.getByRole("searchbox", { name: "Search issues" });
    await expect(search).toHaveValue(scene.query);
    await expect(page.getByText("No issues match these filters.")).toBeVisible();
    await assertPageFits(page);
    const box = await search.boundingBox();
    const viewport = page.viewportSize();
    if (!box || !viewport) throw new Error("The search box or viewport could not be measured");
    expect(box.x + box.width).toBeLessThanOrEqual(viewport.width);
  }
);

// Clearing the box leaves the reader in it: the input they were typing in stays on screen and
// focused, so the next key lands there rather than in the page.
filterTest(
  "clearing the table search keeps the next keystroke in the search box @pr",
  async ({ page, scene }) => {
    await page.goto(`/projects/${scene.project}/issues`);
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
  }
);

referencesTest(
  "an issue's Artifacts tab lists its reference closure and a document page lists its referrers @pr",
  async ({ page, scene }, testInfo) => {
    await page.goto(`/issues/${scene.issue}/artifacts`);
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
    await expect(tabs).toHaveCount(9);
    for (const [index, name] of [
      "Spec",
      "Verification",
      "Plan",
      "Conversation",
      "Children",
      "Artifacts 2",
      "Activity",
      "Decisions",
      "Collaborators",
    ].entries()) {
      await expect(tabs.nth(index)).toHaveAccessibleName(name);
    }
    await expect(page.getByRole("button", { name: /make primary/i })).toHaveCount(0);
    await page.getByRole("link", { name: "Design notes" }).click();
    await expect(page).toHaveURL("/projects/CORE/documents/design-notes");
    await expect(page.getByRole("region", { name: "Referenced by" })).toContainText(
      `Comment · ${scene.issue}`
    );
    // A document mention deep-links to the block that carries it.
    await expect(page.getByRole("link", { name: `Artifact · ${scene.issue}` })).toHaveAttribute(
      "href",
      new RegExp(`^/issues/${scene.issue}/artifacts/source-doc#b-`)
    );
    await page.screenshot({
      path: testInfo.outputPath("project-document-references.png"),
      fullPage: true,
    });
  }
);

// The status groups are collapsible and the header is one strip. Both regressed once while
// this surface was being polished: a group heading that does not toggle, and a title allowed to
// grow pushed the project key hundreds of pixels from a short name.
bandsTest(
  "status groups stay expandable and the project key sits beside its name @pr",
  async ({ page, scene }) => {
    await page.setViewportSize({ height: 900, width: 1280 });
    await page.goto(`/projects/${scene.project}/issues`);

    const shippedLink = page.getByRole("link", {
      name: `${scene.shipped} Already shipped`,
      exact: true,
    });
    await expect(shippedLink).toHaveCount(0);
    await pickFilterOption(page, "Status", "Done");
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
  }
);

// A row's key and title are one reference. Below 1280 px every link is an inline-flex box,
// which would make the key and the title two flex columns: the key would break mid-key (`CORE-`
// over `1`) and a long title would wrap in whatever the cell's other content left.
longTitleTest(
  "a table keeps its key whole and uses its title cell without overflowing the phone @phone @pr",
  async ({ page, scene }, testInfo) => {
    // The desktop case this row ran in `chromium` on the real stack: below 1280 px every link
    // is an inline-flex box, so the claim needs both widths.
    if (testInfo.project.name === "stub") {
      await page.setViewportSize({ height: 900, width: 1280 });
    }
    await page.goto(`/projects/${scene.project}/issues`);
    const row = page.getByRole("row", { name: `${scene.issue} ${scene.title}`, exact: true });
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
          cellBox.width -
          Number.parseFloat(style.paddingLeft) -
          Number.parseFloat(style.paddingRight),
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
  }
);
