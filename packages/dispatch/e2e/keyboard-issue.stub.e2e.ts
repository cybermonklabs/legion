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
  await seed("Alpha", "backlog");
  await seed("Bravo", "todo");
  await seed("Mike", "in_progress");
  await seed("Zulu", "done");
  return { project: "CORE" };
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
  for (const [title, status] of [
    ["Alpha", "backlog"],
    ["Bravo", "todo"],
  ] as const) {
    const issue = await createIssue({ project: "CORE", title });
    await patchIssue(issue.key, { status });
  }
  return { project: "CORE" };
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
    const rows = page.getByRole("list", { name: "Todo issues" }).getByRole("listitem");
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
    await page.goto(`/projects/${scene.project}/issues`);
    // A closed `details` keeps its content out of the accessibility tree, so these are DOM
    // locators: that a collapsed band's rows ARE in the DOM is the whole point here - rows the
    // page never rendered would satisfy the assertions below for another reason entirely.
    const rowsIn = (band: string) =>
      page.locator(`ul[aria-label="${band} issues"] [data-issue-row]`);
    const alpha = rowsIn("Backlog");
    const bravo = rowsIn("Todo");
    const mike = rowsIn("In progress");
    const zulu = rowsIn("Done");
    for (const row of [alpha, bravo, mike, zulu]) {
      await expect(row).toHaveCount(1);
    }
    // Done is the one band the page renders closed; Todo is closed here by the reader, which
    // puts a collapsed band between two open ones.
    await expect(zulu).toBeHidden();
    await page.getByRole("group", { name: "Todo (1)" }).locator("summary").click();
    await expect(bravo).toBeHidden();
    await leaveFocus(page);

    await page.keyboard.press("j");
    await expect(alpha).toBeFocused();
    await page.keyboard.press("j");
    await expect(mike).toBeFocused();
    await expect(bravo).not.toBeFocused();
    // Nothing visible past Mike: the collapsed Done band is not a destination, so j holds, as
    // it does at the end of the list.
    await page.keyboard.press("j");
    await expect(mike).toBeFocused();
    await page.keyboard.press("k");
    await expect(alpha).toBeFocused();

    // Opening a band puts its rows back in the order, between the two they sit between.
    await page.getByRole("group", { name: "Todo (1)" }).locator("summary").click();
    await expect(bravo).toBeVisible();
    await alpha.focus();
    await page.keyboard.press("j");
    await expect(bravo).toBeFocused();
    await page.getByRole("group", { name: "Done (1)" }).locator("summary").click();
    await expect(zulu).toBeVisible();
    await mike.focus();
    await page.keyboard.press("j");
    await expect(zulu).toBeFocused();
  }
);

closedTest(
  "a closed issue greys the shortcuts its header disables, and keeps the ones it does not @pr",
  async ({ page, scene }) => {
    await page.setViewportSize({ width: 1280, height: 900 });
    await openIssue(page, scene.issue, "Closed issue");
    await expect(page.getByRole("button", { name: "Reopen issue" })).toBeVisible();

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
    await expect(row("Conversation tab")).toHaveAttribute("data-enabled", "true");
  }
);

collapsedTest(
  "with every band collapsed j and k do nothing and ? greys them, and opening one restores both @pr",
  async ({ page, scene }) => {
    await page.setViewportSize({ width: 1280, height: 900 });
    await page.goto(`/projects/${scene.project}/issues`);
    const rows = page.locator("[data-issue-row]");
    await expect(rows).toHaveCount(2);
    for (const band of ["Backlog (1)", "Todo (1)"]) {
      await page.getByRole("group", { name: band }).locator("summary").click();
    }
    await expect(rows.first()).toBeHidden();
    await expect(rows.last()).toBeHidden();
    await leaveFocus(page);

    // Nothing the reader can see is a step, so the keys move nothing …
    await page.keyboard.press("j");
    await page.keyboard.press("k");
    expect(await onBody(page)).toBe(true);

    // … and `?` says so, as it does for the issue scope's disabled controls.
    await page.keyboard.press("?");
    const dialog = page.getByRole("dialog", { name: "Keyboard shortcuts" });
    const project = dialog.getByRole("region", { name: "Project", exact: true });
    const roving = ["Next issue", "Previous issue"].map((label) =>
      project.getByRole("listitem").filter({ hasText: label }).first()
    );
    for (const row of roving) {
      await expect(row).toHaveAttribute("data-enabled", "false");
    }
    await page.keyboard.press("Escape");
    await expect(dialog).toHaveCount(0);

    // Opening one band gives them somewhere to go again.
    await page.getByRole("group", { name: "Todo (1)" }).locator("summary").click();
    await expect(rows.last()).toBeVisible();
    await leaveFocus(page);
    await page.keyboard.press("j");
    await expect(rows.last()).toBeFocused();
    await page.keyboard.press("?");
    for (const row of roving) {
      await expect(row).toHaveAttribute("data-enabled", "true");
    }
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
