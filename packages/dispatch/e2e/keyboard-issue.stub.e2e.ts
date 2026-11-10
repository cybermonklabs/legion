/** The issue and project keyboard, in the rows that read only what the page draws: the roving
 *  the List does over its bands, what a collapsed band does to it, what `?` says is enabled
 *  where, and which scope the Board registers.
 *
 *  Each runs against a recording rather than against a harness
 *  (packages/dispatch/AGENTS.md, "The stubbed layer"). The rows that write a priority or a pin,
 *  and the ones that read the issue back to prove no write was made, stayed in
 *  `keyboard-issue.e2e.ts`.
 */
import { expect } from "@playwright/test";

import { CONTROL_LABELS } from "../web/src/features/issue/page-words";
import { createIssue, createProject, patchIssue } from "./api";
import { leaveFocus, onBody, openIssue } from "./keyboard-issue";
import { stubbed } from "./stub/stub-test";

/** Three Todo issues, in the order the list roves over them. */
const listTest = stubbed(async () => {
  await createProject({ key: "CORE", name: "Core" });
  const seed = async (title: string) => {
    const issue = await createIssue({ project: "CORE", title });
    await patchIssue(issue.key, { status: "todo" });
    return issue;
  };
  const alpha = await seed("Alpha");
  const bravo = await seed("Bravo");
  await seed("Charlie");
  return { alpha: alpha.key, bravo: bravo.key, project: "CORE" };
});

/** One issue in each of four bands, so a collapsed one sits between two open ones. */
const bandsTest = stubbed(async () => {
  await createProject({ key: "CORE", name: "Core" });
  const seed = async (title: string, status: "backlog" | "todo" | "in_progress" | "done") => {
    const issue = await createIssue({ project: "CORE", title });
    await patchIssue(issue.key, { status });
    return issue;
  };
  const alpha = await seed("Alpha", "backlog");
  const bravo = await seed("Bravo", "todo");
  const mike = await seed("Mike", "in_progress");
  const zulu = await seed("Zulu", "done");
  return {
    alpha: alpha.key,
    bravo: bravo.key,
    mike: mike.key,
    project: "CORE",
    zulu: zulu.key,
  };
});

/** A closed issue, whose header disables most of what the shortcuts reach. */
const closedTest = stubbed(async () => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Closed issue" });
  await patchIssue(issue.key, { status: "done" });
  return { issue: issue.key };
});

/** Two issues in two bands, both of which the reader collapses. */
const collapsedTest = stubbed(async () => {
  await createProject({ key: "CORE", name: "Core" });
  const keys: Record<string, string> = {};
  for (const [title, status] of [
    ["Alpha", "backlog"],
    ["Bravo", "todo"],
  ] as const) {
    const issue = await createIssue({ project: "CORE", title });
    await patchIssue(issue.key, { status });
    keys[title.toLowerCase()] = issue.key;
  }
  return { bravo: keys.bravo ?? "", project: "CORE" };
});

/** One Todo issue, for the Board's own scope. */
const boardTest = stubbed(async () => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Alpha" });
  await patchIssue(issue.key, { status: "todo" });
  return { project: "CORE" };
});

listTest(
  "the project List view roves with j/k and opens the focused row with Enter and o @pr",
  async ({ page, scene }, testInfo) => {
    await page.setViewportSize({ width: 1280, height: 900 });
    await page.goto(`/projects/${scene.project}/issues`);
    const rows = page
      .getByRole("table", { name: "Project issues table" })
      .locator("[data-issue-row]");
    // The seeded order is the order the rows rove in, so assert it before roving over it.
    await expect(rows).toHaveText([/Alpha/, /Bravo/, /Charlie/]);
    await page.locator("body").focus();

    await page.keyboard.press("j");
    await expect(rows.nth(0)).toBeFocused();
    await page.keyboard.press("j");
    await expect(rows.nth(1)).toBeFocused();
    await page.keyboard.press("k");
    await expect(rows.nth(0)).toBeFocused();
    await page.screenshot({
      path: testInfo.outputPath(`project-list-roving-${testInfo.project.name}.png`),
    });
    await page.keyboard.press("Enter");
    await expect(page).toHaveURL(new RegExp(`/issues/${scene.alpha}$`));

    // o opens the row the reader is on, not the first one.
    await page.goBack();
    await expect(rows).toHaveText([/Alpha/, /Bravo/, /Charlie/]);
    await page.locator("body").focus();
    await page.keyboard.press("j");
    await page.keyboard.press("j");
    await expect(rows.nth(1)).toBeFocused();
    await page.keyboard.press("o");
    await expect(page).toHaveURL(new RegExp(`/issues/${scene.bravo}$`));

    await page.goBack();
    await expect(rows).toHaveText([/Alpha/, /Bravo/, /Charlie/]);
    await page.locator("body").focus();
    await page.keyboard.press("?");
    const projectSection = page
      .getByRole("dialog", { name: "Keyboard shortcuts" })
      .getByRole("region", { name: "Project", exact: true });
    await expect(projectSection).toContainText("Next issue");
    await expect(projectSection).toContainText("Open issue");
  }
);

bandsTest(
  "j and k cross a collapsed band instead of dead-ending in it, and reach its rows once it opens @pr",
  async ({ page, scene }) => {
    await page.setViewportSize({ width: 1280, height: 900 });
    // Grouped by status, sorted by key and with every status shown: the table's own shape for
    // the bands this row was written against, where a group is a heading the reader collapses.
    await page.goto(`/projects/${scene.project}/issues?group=status&sort=key&nofilter=1`);
    const row = (key: string) => page.locator(`[data-issue-row="${key}"]`);
    await expect(row(scene.zulu)).toBeVisible();
    // A collapsed group between two open ones: its rows leave the page, and the keys step over
    // the heading to the next group's rows rather than dead-ending in it.
    await page.getByRole("button", { name: /Todo.*1 issue/ }).click();
    await expect(row(scene.bravo)).toHaveCount(0);
    await row(scene.alpha).focus();
    await page.keyboard.press("j");
    await expect(row(scene.mike)).toBeFocused();
    await page.keyboard.press("j");
    await expect(row(scene.zulu)).toBeFocused();
    await page.keyboard.press("k");
    await expect(row(scene.mike)).toBeFocused();
    await page.keyboard.press("k");
    await expect(row(scene.alpha)).toBeFocused();
    // Opening it again puts its rows back between the two they sit between.
    await page.getByRole("button", { name: /Todo.*1 issue/ }).click();
    await row(scene.alpha).focus();
    await page.keyboard.press("j");
    await expect(row(scene.bravo)).toBeFocused();
  }
);

closedTest(
  "a closed issue greys the shortcuts its header disables, and keeps the ones it does not @pr",
  async ({ page, scene }) => {
    await page.setViewportSize({ width: 1280, height: 900 });
    await openIssue(page, scene.issue, "Closed issue");
    // Reopen is behind the identity block's actions control now (dispatch://INFRA-1196 R11).
    await page.getByRole("button", { name: "Issue actions" }).click();
    await expect(
      page.getByRole("button", { name: CONTROL_LABELS.reopen_into_backlog })
    ).toBeVisible();

    await page.keyboard.press("?");
    const issueSection = page
      .getByRole("dialog", { name: "Keyboard shortcuts" })
      .getByRole("region", { name: "Issue", exact: true });
    const row = (label: string) => issueSection.getByRole("listitem").filter({ hasText: label });
    // The title heading takes no focus while the issue is closed, so `e` is greyed with the
    // controls the header disables; pinning a closed issue still works, so Shift+P is not.
    await expect(row("Edit the title")).toHaveAttribute("data-enabled", "false");
    await expect(row("Focus the status")).toHaveAttribute("data-enabled", "false");
    await expect(row("Edit labels")).toHaveAttribute("data-enabled", "false");
    await expect(row("Set priority P0–P3")).toHaveAttribute("data-enabled", "false");
    await expect(row("Pin or unpin the issue")).toHaveAttribute("data-enabled", "true");
    await expect(row("Open the conversation in the panel")).toHaveAttribute("data-enabled", "true");
  }
);

collapsedTest(
  "with every band collapsed j and k do nothing and ? greys them, and opening one restores both @pr",
  async ({ page, scene }) => {
    await page.setViewportSize({ width: 1280, height: 900 });
    await page.goto(`/projects/${scene.project}/issues?group=status`);
    await expect(page.locator("[data-issue-row]")).toHaveCount(2);
    for (const status of ["Backlog", "Todo"])
      await page.getByRole("button", { name: new RegExp(`${status}.*1 issue`) }).click();
    await expect(page.locator("[data-issue-row]")).toHaveCount(0);
    await leaveFocus(page);
    await page.keyboard.press("j");
    await page.keyboard.press("k");
    expect(await onBody(page)).toBe(true);
    await page.keyboard.press("?");
    const section = page
      .getByRole("dialog", { name: "Keyboard shortcuts" })
      .getByRole("region", { name: "Project", exact: true });
    const roving = ["Next issue", "Previous issue"].map((label) =>
      section.getByRole("listitem").filter({ hasText: label }).first()
    );
    for (const listed of roving) await expect(listed).toHaveAttribute("data-enabled", "false");
    await page.keyboard.press("Escape");
    await page.getByRole("button", { name: /Todo.*1 issue/ }).click();
    await leaveFocus(page);
    await page.keyboard.press("j");
    await expect(page.locator(`[data-issue-row="${scene.bravo}"]`)).toBeFocused();
    await page.keyboard.press("?");
    for (const listed of roving) await expect(listed).toHaveAttribute("data-enabled", "true");
  }
);

boardTest(
  "the Board lists its own roving keys once: the List's are not registered while it shows @pr",
  async ({ page, scene }) => {
    await page.setViewportSize({ width: 1280, height: 900 });
    await page.goto(`/projects/${scene.project}/issues`);
    await expect(page.locator("[data-issue-row]")).toHaveCount(1);
    await page.getByRole("button", { name: "Board" }).click();
    await expect(page.getByRole("region", { name: "Todo" }).getByRole("article")).toHaveCount(1);
    await leaveFocus(page);

    // The List's rows are gone, so its keys are not the reader's here and `?` must not list
    // them: `o` and `Enter` would otherwise appear twice over, once per scope.
    await page.keyboard.press("?");
    const dialog = page.getByRole("dialog", { name: "Keyboard shortcuts" });
    await expect(dialog.getByRole("region", { name: "Board", exact: true })).toBeVisible();
    const project = dialog.getByRole("region", { name: "Project", exact: true });
    for (const label of ["Next issue", "Previous issue", "Open the focused issue"]) {
      await expect(project.getByRole("listitem").filter({ hasText: label })).toHaveCount(0);
    }
    await expect(dialog.getByRole("listitem").filter({ hasText: "Open issue" })).toHaveCount(1);
    await expect(
      project.getByRole("listitem").filter({ hasText: "Toggle List / Board" })
    ).toHaveCount(1);
  }
);
