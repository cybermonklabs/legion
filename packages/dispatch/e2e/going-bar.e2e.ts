import { createIssue, createProject, createProjectDocument } from "./api";
import { documentEditor, holdFirstRequest, placeCaret } from "./editor";
import { expect, type Locator, type Page, test } from "./test";
import { asUser } from "./users";

// One bar from anywhere, and it says what it will act on. `Control+K` opens it on every page, the
// document editor included; `/` and `g p` open the same bar; the chip inside the box is seeded
// from the page and `Tab`/`Backspace` move it; an empty input lists the person's recent items and
// then the page's actions, as main lists them, and the words typed narrow the actions to those
// whose name holds them (design v3, state 1).

const documentName = "Design notes";
const paragraph = "The sextant sights the horizon.";
const documentBody = `# ${documentName}\n\n${paragraph}\n`;

interface Fixture {
  readonly documentSlug: string;
  readonly firstKey: string;
  readonly secondKey: string;
}

async function seed(): Promise<Fixture> {
  await createProject({ key: "CORE", name: "Core" });
  const first = await createIssue({ project: "CORE", title: "Openers" });
  const second = await createIssue({ project: "CORE", title: "Instruments" });
  const document = await createProjectDocument("CORE", {
    content: documentBody,
    name: documentName,
  });
  return { documentSlug: document.artifact.slug, firstKey: first.key, secondKey: second.key };
}

function bar(page: Page): Locator {
  return page.getByRole("dialog", { name: "Search" });
}

/** What the bar says it will act on: the chip inside the box, on its left, in short words ("the
 *  project CORE", "the whole project", "a block"), whose tooltip says it in full ("Acting on the
 *  project CORE · …"). Not the project chip of the filters behind the Filters button, which
 *  narrows the search and carries its own Remove control. */
function chip(page: Page): Locator {
  return bar(page).getByTestId("going-box-chip");
}

/** Opens the bar with the key under test from wherever focus already is. */
async function openBar(page: Page, keys: readonly string[]): Promise<void> {
  for (const key of keys) {
    await page.keyboard.press(key);
  }
  await expect(bar(page)).toBeVisible();
}

async function closeBar(page: Page): Promise<void> {
  await page.keyboard.press("Escape");
  await expect(bar(page)).toHaveCount(0);
}

/** The issue list of the seeded project, with its first row focused so the list's own keys act. */
async function openIssueList(page: Page): Promise<void> {
  await page.goto("/projects/CORE/issues");
  await expect(page.getByRole("link", { name: /Openers/ })).toBeVisible();
  await page.locator("body").focus();
}

/** The document with the writing focus in its editor, the caret inside the paragraph. */
async function openDocumentWithCaret(page: Page, slug: string): Promise<void> {
  await page.goto(`/projects/CORE/documents/${slug}`);
  await expect(documentEditor(page)).toContainText(paragraph);
  await placeCaret(page, "after", "sextant");
  await expect(documentEditor(page)).toBeFocused();
}

// SLOW(>=2s): four pages, one of them a document whose editor must connect before the caret
// goes in.
test("Control+K opens the bar from the issue list, an issue, a document being written in, and the agents page @phone", async ({
  browser,
}) => {
  const fixture = await seed();
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();

    await openIssueList(page);
    await openBar(page, ["Control+k"]);
    await closeBar(page);

    await page.goto(`/issues/${fixture.firstKey}`);
    await expect(page.getByRole("heading", { name: /Openers/ })).toBeVisible();
    await page.locator("body").focus();
    await openBar(page, ["Control+k"]);
    await closeBar(page);

    // The writing focus is in the editor: the key belongs to the bar there too, and the
    // keystroke writes no `k` into the document.
    await openDocumentWithCaret(page, fixture.documentSlug);
    await openBar(page, ["Control+k"]);
    await expect(page.getByRole("combobox", { name: "Search" })).toBeFocused();
    await closeBar(page);
    await expect(documentEditor(page)).toContainText(paragraph);

    await page.goto("/agents");
    await expect(page.getByRole("heading", { name: "Agents" })).toBeVisible();
    await page.locator("body").focus();
    await openBar(page, ["Control+k"]);
    await closeBar(page);
  } finally {
    await context.close();
  }
});

// SLOW(>=2s): the seed, then three keys, each opening the bar and reading its chip, its actions
// group and its box.
test("slash and g p open the same bar as Control+K @phone", async ({ browser }) => {
  // Every check here carries the suite's own 15 s deadline, and a run where two of the three
  // keys open something else reports each of them, which takes longer than one row's budget.
  test.setTimeout(120_000);
  await seed();
  const context = await asUser(browser, "alice");

  // Each key is its own reported check (`expect.soft`), so a run where one of them opens
  // something else - the hits alone, the projects alone - says which key did.
  async function expectTheOneBar(page: Page, keys: readonly string[]): Promise<void> {
    const opened = keys.join(" ");
    await expect
      .soft(chip(page), `${opened} opens the bar with its chip`)
      .toHaveText("the project CORE");
    await expect
      .soft(chip(page), `${opened} opens the bar with its chip's words in full`)
      .toHaveAttribute("title", /^Acting on the project CORE/);
    // The empty box lists the page's actions, as main lists them (design v3, state 1).
    await expect
      .soft(
        bar(page).getByRole("group", { name: "Actions" }),
        `${opened} opens the bar with this page's actions`
      )
      .toBeVisible();
    await expect
      .soft(
        page.getByRole("combobox", { name: "Search" }),
        `${opened} opens one box for everything, not a list of its own`
      )
      .toHaveAttribute("placeholder", "Search or ask…");
  }

  try {
    const page = await context.newPage();
    await openIssueList(page);

    for (const keys of [["Control+k"], ["/"], ["g", "p"]]) {
      await page.locator("body").focus();
      await openBar(page, keys);
      await expectTheOneBar(page, keys);
      await closeBar(page);
    }
  } finally {
    await context.close();
  }
});

// SLOW(>=2s): the chip is read on four pages, one of them a document.
test("the chip names what the bar will act on, and nothing where the page names nothing @phone", async ({
  browser,
}) => {
  // One check per page, each its own reported failure against the suite's 15 s deadline.
  test.setTimeout(120_000);
  const fixture = await seed();
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();

    await page.goto(`/issues/${fixture.firstKey}`);
    await expect(page.getByRole("heading", { name: /Openers/ })).toBeVisible();
    await page.locator("body").focus();
    await openBar(page, ["Control+k"]);
    await expect.soft(chip(page), "on an issue, that issue").toHaveText("CORE-1");
    await closeBar(page);

    await openDocumentWithCaret(page, fixture.documentSlug);
    await openBar(page, ["Control+k"]);
    await expect.soft(chip(page), "on a document, that document").toHaveText("Design notes");
    await closeBar(page);

    await openIssueList(page);
    await openBar(page, ["Control+k"]);
    await expect
      .soft(chip(page), "on the issue list with none selected, the project")
      .toHaveText("the project CORE");
    await closeBar(page);

    // Two issues selected: `x` marks the row the list's own `j` left the focus on.
    await page.keyboard.press("j");
    await page.keyboard.press("x");
    await page.keyboard.press("j");
    await page.keyboard.press("x");
    await expect(page.getByRole("checkbox", { name: `Select ${fixture.firstKey}` })).toBeChecked();
    await expect(page.getByRole("checkbox", { name: `Select ${fixture.secondKey}` })).toBeChecked();
    await openBar(page, ["Control+k"]);
    await expect
      .soft(chip(page), "with two issues selected, those two")
      .toHaveText(`${fixture.firstKey} and ${fixture.secondKey}`);
    await closeBar(page);

    await page.goto("/agents");
    await expect(page.getByRole("heading", { name: "Agents" })).toBeVisible();
    await page.locator("body").focus();
    await openBar(page, ["Control+k"]);
    await expect.soft(chip(page), "elsewhere, nothing").toHaveCount(0);
    await closeBar(page);
  } finally {
    await context.close();
  }
});

test("the bar acts on the issues the table has selected, and leaving the table clears them @phone", async ({
  browser,
}) => {
  const fixture = await seed();
  const third = await createIssue({ project: "CORE", title: "Sextants" });
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await openIssueList(page);

    // Two rows by their own checkboxes, the third by the key that takes the row in hand: both
    // ways in reach the one selection the bar reads.
    await page.getByRole("checkbox", { name: `Select ${fixture.firstKey}` }).check();
    await page.getByRole("checkbox", { name: `Select ${fixture.secondKey}` }).check();
    await page.keyboard.press("j");
    await page.keyboard.press("x");
    await expect(page.getByRole("checkbox", { name: `Select ${third.key}` })).toBeChecked();

    await openBar(page, ["Control+k"]);
    await expect(chip(page)).toHaveText(
      `${fixture.firstKey}, ${fixture.secondKey} and ${third.key}`
    );
    await closeBar(page);

    // The Board takes the table off the page, and the selection goes with it.
    await page.getByRole("button", { name: "Board", exact: true }).click();
    await expect(page.getByRole("region", { name: "Project board" })).toBeVisible();
    await page.locator("body").focus();
    await openBar(page, ["Control+k"]);
    await expect(chip(page)).toHaveText("the project CORE");
    await closeBar(page);

    // Back on the table, the rows come back unselected rather than carrying a hidden mark.
    await page.getByRole("button", { name: "List", exact: true }).click();
    await expect(
      page.getByRole("checkbox", { name: `Select ${fixture.firstKey}` })
    ).not.toBeChecked();
  } finally {
    await context.close();
  }
});

// SLOW(>=2s): a document opened and its editor connected before the caret goes in.
test("Tab narrows the chip to the block the cursor is in and Backspace widens it to the project @phone", async ({
  browser,
}) => {
  const fixture = await seed();
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await openDocumentWithCaret(page, fixture.documentSlug);
    await openBar(page, ["Control+k"]);
    await expect(chip(page)).toHaveText("Design notes");

    // Tab and Backspace move the chip as keys of the box (`useBarScope`'s `handleKey`). The box
    // takes the focus a frame after the bar opens, and a key pressed before that frame reaches
    // the bar itself, which holds the focus until then (`useDialog`), so the row waits for the box.
    await expect(
      bar(page).getByRole("combobox", { name: "Search" }),
      "the bar's box took no focus"
    ).toBeFocused();
    await page.keyboard.press("Tab");
    await expect(chip(page)).toHaveText("a block");
    await expect(chip(page)).toHaveAttribute(
      "title",
      new RegExp(`^Acting on the block “${paragraph}”`)
    );

    await page.keyboard.press("Backspace");
    await expect(chip(page)).toHaveText("the whole project");
    await expect(chip(page)).toHaveAttribute("title", /^Acting on the project CORE/);
  } finally {
    await context.close();
  }
});

// SLOW(>=2s): two items are opened before the bar is, so the browser has recent items to list.
test("an empty input lists the person's recent items and then the page's actions, and the words typed narrow the actions @phone", async ({
  browser,
}) => {
  // Several reported checks on one open, each against the suite's own 15 s deadline.
  test.setTimeout(120_000);
  const fixture = await seed();
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();

    // What the person opened, in this browser: the two items the bar lists back to them.
    await page.goto(`/issues/${fixture.firstKey}`);
    await expect(page.getByRole("heading", { name: /Openers/ })).toBeVisible();
    await page.goto(`/projects/CORE/documents/${fixture.documentSlug}`);
    await expect(documentEditor(page)).toContainText(paragraph);

    await openIssueList(page);
    // A row in hand, so the list's own action is among the page's.
    await page.keyboard.press("j");
    await openBar(page, ["Control+k"]);
    const box = page.getByRole("combobox", { name: "Search" });
    await expect(box).toHaveValue("");

    const recent = bar(page).getByRole("group", { name: "Recent" });
    await expect.soft(recent.getByRole("option", { name: /Design notes/ })).toHaveCount(1);
    await expect
      .soft(recent.getByRole("option", { name: new RegExp(fixture.firstKey) }))
      .toHaveCount(1);
    // The page's actions follow Recent, as main lists them (design v3, state 1).
    const actions = bar(page).getByRole("group", { name: "Actions" });
    await expect.soft(actions.getByRole("option", { name: "Open issue" })).toHaveCount(1);
    await expect.soft(actions.getByRole("option", { name: "Create issue" })).toHaveCount(1);
    const ids = await bar(page)
      .getByRole("option")
      .evaluateAll((options) => options.map((option) => option.id));
    expect
      .soft(
        ids.findIndex((id) => id.startsWith("search-option-recent-")),
        "the empty box lists Recent before the page's actions"
      )
      .toBeLessThan(ids.findIndex((id) => id.startsWith("search-option-action-")));

    // The words typed start an action's name, and Recent leaves the list.
    await box.fill("Open issue");
    await expect.soft(actions.getByRole("option", { name: "Open issue" })).toHaveCount(1);
    await expect.soft(recent).toHaveCount(0);
    await box.fill("Create");
    await expect.soft(actions.getByRole("option", { name: "Create issue" })).toHaveCount(1);

    // A recent item opens from the bar.
    await box.fill("");
    await recent.getByRole("option", { name: /Design notes/ }).click();
    await expect(page).toHaveURL(`/projects/CORE/documents/${fixture.documentSlug}`);
  } finally {
    await context.close();
  }
});

// The bare `/projects/CORE`, the path the sidebar links, is a route that settles itself: it
// draws nothing while its architecture-sources read is pending, then redirects with `replace`
// to Issues. The bar a person opened in that moment is theirs, and a redirect the page made on
// its own is no navigation of theirs, so the bar stays open and stays typable
// (dispatch://INFRA-2452).
// SLOW(>=2s): the seed of a project, two issues and a document, then a page held mid-redirect.
test("the bar opened on a project page that has not settled survives that page's own redirect", async ({
  browser,
}) => {
  await seed();
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    // The sources read is held, so the page is still deciding where it goes while the bar opens
    // on it; letting it through sends the redirect out from under the open bar. Awaiting the
    // held route is also what proves the page asked: a row whose pattern stopped matching fails
    // here rather than passing over a page that never redirected.
    const sources = await holdFirstRequest(
      page,
      /\/api\/v1\/projects\/[^/]+\/architecture-sources$/u
    );

    await page.goto("/projects/CORE");
    await page.locator("body").focus();
    await openBar(page, ["/"]);
    const box = page.getByRole("combobox", { name: "Search" });
    await expect(box).toBeVisible();

    await (await sources.held).continue();
    await expect(page).toHaveURL(/\/projects\/CORE\/issues$/);

    // The redirect has landed and the bar is still there, and still the same box to type in:
    // the page moved under it and took nothing from the person.
    await expect(bar(page)).toBeVisible();
    await box.fill("Instruments");
    await expect(box).toHaveValue("Instruments");
  } finally {
    await context.close();
  }
});
