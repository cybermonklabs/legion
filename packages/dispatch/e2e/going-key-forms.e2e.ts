import { createIssue, createProject, type RowKey } from "./api";
import { sql } from "./psql";
import { expect, type Page, rowKey, rowNumber, test } from "./test";
import { asUser } from "./users";

// Check k3 of dispatch://INFRA-1042/artifact/verification-md, serving R3: an issue key finds its
// issue as the first row in every form a person writes it - `INFRA-1042`, `infra-1042`,
// `INFRA 1042` and the bare `1042`, which takes the project the page names and, on a page naming
// none, heads the list with every project's issue of that number.
//
// The number is the row's own (`rowNumber`, e2e/test.ts) rather than the check's 1042: a bare
// number on a page naming no project answers every project's issue of it over the whole stack,
// and every earlier run of these rows on the worker - a repeat of the same row among them - left
// two projects holding issue 1042 and two decoys whose titles hold it.

const targetTitle = "Navigation instruments";
/** A second issue of the same project whose own title holds the number. It outranks the issue
 *  the person asked for on words alone - a title match is the strongest keyword hit there is -
 *  so a run where the key leads is a run where the key, not the text, decided. */
function decoyTitle(number: number): string {
  return `Astrolabe ${number} calibration`;
}

/** `<project>-<number>` and the decoy beside it. The project's next number is set rather than
 *  counted up to, as `search_bar_test.go` sets it: the check names this key, and the issues
 *  before it would be the whole cost of the row. */
async function seedProject(project: RowKey, number: number): Promise<string> {
  await createProject({ key: project, name: `${project} work` });
  await sql(`update projects set next_number = ${number} where key = '${project}'`);
  const target = await createIssue({ project, title: targetTitle });
  await createIssue({ project, title: decoyTitle(number) });
  return target.key;
}

/** Opens the bar with `/`. Its rows are this page's actions and the person's recent items as
 *  well as the hits, so the checks below count the hits by their own ids. */
async function openSearch(page: Page): Promise<void> {
  await page.locator("body").focus();
  await page.keyboard.press("/");
  await expect(page.getByRole("dialog", { name: "Search" })).toBeVisible();
}

// SLOW(>=2s): two seeded projects of two issues each, and four searches of them - one per form
// of the key, each its own request behind the bar's debounce.
test("every form of an issue key puts that issue first on a page of its project", async ({
  browser,
}) => {
  const infraKey = rowKey("INFRA");
  // Every check here carries the suite's own 15 s deadline, and a run where several of the
  // forms miss reports each of them, which takes longer than one row's default budget.
  test.setTimeout(120_000);
  const issueNumber = rowNumber("issue");
  const key = await seedProject(infraKey, issueNumber);
  // A second project holding the same decoy title: a search from INFRA's own page still
  // answers this project's rows, since the page narrows nothing.
  await seedProject(rowKey("LINUX"), issueNumber);
  const number = key.slice(key.indexOf("-") + 1);
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await page.goto(`/projects/${infraKey}`);
    await openSearch(page);
    const dialog = page.getByRole("dialog", { name: "Search" });
    const search = page.getByRole("combobox", { name: "Search" });

    // Each form is its own reported check (`expect.soft`), so a run where one form leads and
    // another does not says which.
    for (const form of [key, key.toLowerCase(), key.replace("-", " "), number]) {
      // Emptied between forms: the bar holds no hit for a query under two characters, so the
      // row this form finds is this form's own and never the last one's.
      await search.fill("");
      await expect(dialog.locator('[role="option"][id^="search-option-issue-"]')).toHaveCount(0);
      await search.fill(form);
      await expect
        .soft(dialog.getByRole("option").first(), `the form "${form}" heads the list`)
        .toHaveAttribute("id", `search-option-issue-${key}`);
    }
    // The page named a project and the bar narrowed by none: no chip claims one, and the last
    // form searched - the bare number - answered both projects' rows, with the issue the page
    // names leading them.
    await expect.soft(dialog.getByRole("button", { name: /^Remove Project/ })).toHaveCount(0);
    await expect
      .soft(
        dialog.getByRole("option").filter({ hasText: decoyTitle(issueNumber) }),
        "both projects' decoys"
      )
      .toHaveCount(2);
  } finally {
    await context.close();
  }
});

test("on a page naming no project the bare number heads the list with every project's issue of it", async ({
  browser,
}) => {
  // Both leading rows are checked by themselves, and a run where neither is the issue reports
  // both, each against the suite's own 15 s deadline.
  test.setTimeout(60_000);
  const number = rowNumber("issue");
  const infra = await seedProject(rowKey("INFRA"), number);
  const linux = await seedProject(rowKey("LINUX"), number);
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await page.goto("/");
    await openSearch(page);
    const dialog = page.getByRole("dialog", { name: "Search" });
    await expect(dialog.getByRole("button", { name: /^Remove Project/ })).toHaveCount(0);
    await page.getByRole("combobox", { name: "Search" }).fill(String(number));

    // Both issues of that number lead, in either order, and the two decoys whose titles hold
    // it are below them. Each leading row is checked by itself, so a run that heads the list
    // with a decoy says so of that row.
    const options = dialog.getByRole("option");
    const numbered = new RegExp(`^search-option-issue-(${infra}|${linux})$`);
    await expect.soft(options.nth(0)).toHaveAttribute("id", numbered);
    await expect.soft(options.nth(1)).toHaveAttribute("id", numbered);
    await expect(options).toHaveCount(4);
  } finally {
    await context.close();
  }
});
