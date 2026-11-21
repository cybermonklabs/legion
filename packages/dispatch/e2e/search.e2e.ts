import {
  answerAsk,
  createAsk,
  createIssue,
  createMessage,
  createProject,
  createProjectDocument,
  resolveAsk,
} from "./api";
import { documentEditor } from "./editor";
import {
  keyedSearchWord,
  openSearchFromRail,
  searchSession,
  searchTerm,
  seedSearchFixture,
} from "./search";
import { expect, rowKey, rowPerson, test } from "./test";
import { asUser } from "./users";

/**
 * The search palette, in the rows whose claim is what the index found: the grouped hits and
 * their marks, an ask hit's own answer, and the two routes a hit opens. The keyboard-hints row
 * is here too: it is read in the desktop and the phone configurations at once, and a stubbed
 * row runs in one of them. What the palette draws over one fixed answer is in
 * `search.stub.e2e.ts`, and the fixture both files read is in `search.ts`.
 */

test("Ctrl+K and Cmd+K open the palette, grouped results navigate a document to its highlighted term", async ({
  browser,
}) => {
  const core = rowKey("CORE");
  // The word searched for holds the row's own key (`keyedSearchWord`, e2e/search.ts): search
  // reads every project of the stack, and only this row's items hold it.
  const term = keyedSearchWord(searchTerm, core);
  const fixture = await seedSearchFixture(core, term);
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await page.goto("/");
    await expect(page.getByRole("button", { name: /^search/i })).toBeVisible();
    await page.locator("body").focus();
    await page.keyboard.press("Control+k");

    const dialog = page.getByRole("dialog", { name: "Search" });
    const search = page.getByRole("combobox", { name: "Search" });
    await expect(dialog).toBeVisible();
    await expect(search).toBeFocused();
    await search.fill(term);

    const options = dialog.getByRole("option");
    // Three rows, in the server's order: the issue whose spec holds the term (an issue's
    // keyword vector carries its primary document's text), that document, and the comment.
    await expect(options).toHaveCount(3);
    await expect(options.first()).toHaveAttribute(
      "id",
      `search-option-issue-${fixture.firstIssueKey}`
    );
    const documentResult = dialog.getByRole("option", { name: /^doc spec\.md / });
    await expect(documentResult).toBeVisible();
    await expect(documentResult.locator("mark")).toHaveText([term, term]);
    await expect(dialog.locator('[role="presentation"]')).toHaveCount(2);
    await expect(
      dialog.locator(`[role="presentation"][aria-label="${core}-1: Navigation instruments"]`)
    ).toBeVisible();
    await expect(
      dialog.locator(`[role="presentation"][aria-label="${core}-2: Other work"]`)
    ).toBeVisible();

    const documentID = await documentResult.getAttribute("id");
    expect(documentID).not.toBeNull();
    // Down to the comment, the last of the three, and back up to the document.
    await search.press("ArrowDown");
    await search.press("ArrowDown");
    await expect(search).toHaveAttribute("aria-activedescendant", /search-option-comment-/);
    await search.press("ArrowUp");
    await expect(search).toHaveAttribute("aria-activedescendant", documentID as string);
    await search.press("Enter");

    await expect(page).toHaveURL(new RegExp(`/issues/${fixture.firstIssueKey}/spec\\?q=${term}$`));
    const editor = documentEditor(page);
    await expect(editor).toContainText(term);
    await expect(editor).toBeInViewport();
    await expect
      .poll(() =>
        page.evaluate((word) => {
          // Playwright's TypeScript context lacks CSS Highlight API declarations.
          const css = CSS as unknown as { highlights?: Map<string, Highlight> };
          const highlights = css.highlights?.get("dispatch-search");
          return (
            highlights !== undefined && [...highlights].some((range) => range.toString() === word)
          );
        }, term)
      )
      .toBe(true);
    await expect(dialog).toHaveCount(0);

    await page.keyboard.press("Meta+k");
    await expect(dialog).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(dialog).toHaveCount(0);
  } finally {
    await context.close();
  }
});

test("a search term lists marked document, comment, ask, and issue-title results in issue groups", async ({
  browser,
}) => {
  const core = rowKey("CORE");
  // The word searched for holds the row's own key (`keyedSearchWord`, e2e/search.ts), and so
  // does the title that starts with it: only this row's items hold it.
  const term = keyedSearchWord(searchTerm, core);
  const titleWord = keyedSearchWord("Astrolabe", core);
  await seedSearchFixture(core, term);
  const titleIssue = await createIssue({ project: core, title: `${titleWord} calibration` });
  await createAsk(titleIssue.key, { question: `Who calibrates the ${term}?` }, searchSession);
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await openSearchFromRail(page, false);
    const search = page.getByRole("combobox", { name: "Search" });
    await search.fill(term);

    // Scoped to the palette: inbox rows carry a priority <select> whose options share the role.
    const dialog = page.getByRole("dialog", { name: "Search" });
    const options = dialog.getByRole("option");
    // Five rows: the four the term is marked in, and CORE-1, which the term reached through
    // its spec (an issue's keyword vector carries its primary document's text) and whose own
    // title never held it, so its row marks nothing.
    await expect(options).toHaveCount(5);
    for (const name of [
      new RegExp(`^issue ${titleWord} calibration`),
      /^doc /,
      /^comment /,
      /^ask /,
    ]) {
      const option = dialog.getByRole("option", { name });
      await expect(option).toHaveCount(1);
      await expect(option.locator("mark").first()).toHaveText(new RegExp(term, "i"));
    }
    const foundByItsSpec = dialog.getByRole("option", { name: /^issue Navigation instruments/ });
    await expect(foundByItsSpec).toHaveCount(1);
    await expect(foundByItsSpec.locator("mark")).toHaveCount(0);
    await expect(dialog.locator('[role="presentation"]')).toHaveCount(3);
    await expect(
      dialog.locator(`[role="presentation"][aria-label="${core}-4: ${titleWord} calibration"]`)
    ).toBeVisible();
  } finally {
    await context.close();
  }
});

test("an ask hit says whether it is answered, and an answered one shows its latest answer, who gave it and the options chosen", async ({
  browser,
}) => {
  const core = rowKey("CORE");
  await createProject({ key: core, name: "Core" });
  const issue = await createIssue({ project: core, title: "Planetarium" });
  // Each question names the row's own key (`rowKey`, e2e/test.ts) beside the word it is found
  // by: search reads every project of the stack, and only this row's three asks hold both.
  const answered = await createAsk(
    issue.key,
    {
      options: [{ label: "Brass" }, { label: "Steel" }],
      question: `Which orrery frame do we keep in ${core}?`,
    },
    searchSession
  );
  const resolved = await createAsk(
    issue.key,
    { question: `Should the orrery gears be oiled in ${core}?` },
    searchSession
  );
  await createAsk(issue.key, { question: `Who winds the orrery of ${core}?` }, searchSession);
  const first = await answerAsk(answered.id, {
    expected_edited_at: null,
    selected: ["Brass"],
    text: "Keep the brass one.",
  });
  expect(first.answer).not.toBeNull();
  // A long last answer with a word only its end holds: the hit carries the answer's start
  // alone, so that word never reaches the option or its accessible name.
  const longAnswer = `Keep the steel one after all. ${"The steel frame holds its true under a heavy load and wants no polish between voyages. ".repeat(30)}zenithward`;
  expect(longAnswer.length).toBeGreaterThan(2000);
  await answerAsk(answered.id, {
    expected_answer_at: first.answer?.at,
    expected_edited_at: null,
    selected: [],
    text: longAnswer,
  });
  await resolveAsk(
    resolved.id,
    { kind: "resolved", reason: "Settled in the spec." },
    searchSession
  );
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await openSearchFromRail(page, false);
    await page.getByRole("combobox", { name: "Search" }).fill(`orrery ${core}`);

    const dialog = page.getByRole("dialog", { name: "Search" });
    await expect(dialog.getByRole("option")).toHaveCount(3);
    await expect(dialog.getByRole("option", { name: /^ask / })).toHaveCount(3);

    const answeredHit = dialog.getByRole("option").filter({ hasText: "Which orrery frame" });
    await expect(answeredHit).toContainText("Answered");
    await expect(answeredHit).toContainText("alice");
    await expect(answeredHit).toContainText("No option chosen");
    await expect(answeredHit).toContainText("Keep the steel one after all.");
    await expect(answeredHit).not.toContainText("zenithward");
    // The option's name is all its text: the excerpt, the status line and the answer's start
    // (`ANSWER_PREVIEW_CHARS` beside `AskHitStatus`), well under the 2,000-character answer.
    await expect(answeredHit).toHaveAccessibleName(/^ask /u);
    await expect(answeredHit).not.toHaveAccessibleName(/zenithward/u);
    await expect(answeredHit).toHaveAccessibleName(/^[\s\S]{1,600}$/u);
    await expect(answeredHit).not.toContainText("Keep the brass one.");
    await expect(answeredHit).not.toContainText("Chose Brass");

    const resolvedHit = dialog.getByRole("option").filter({ hasText: "orrery gears" });
    await expect(resolvedHit).toContainText("Closed, no answer");
    await expect(resolvedHit).toContainText("Resolved by");
    await expect(resolvedHit).not.toContainText("Answered");

    // The server's snippet drops the question's leading stop word.
    const openHit = dialog.getByRole("option").filter({ hasText: "winds the orrery" });
    await expect(openHit).toContainText("Open");
    await expect(openHit).not.toContainText("Answered");
  } finally {
    await context.close();
  }
});

// SLOW(>=2s): a seeded project document, issue and message, and the two routes the palette's
// hits open - a Conversation turn and the document, which mounts its editor.
test("a project-document hit is grouped under the document and opens it; a message hit opens its Conversation turn", async ({
  browser,
}) => {
  const core = rowKey("CORE");
  // The word searched for holds the row's own key (`keyedSearchWord`, e2e/search.ts), and so do
  // the title and the message that start with it: only this row's items hold it.
  const term = keyedSearchWord("sextant", core);
  const sentenceWord = keyedSearchWord("Sextant", core);
  await createProject({ key: core, name: "Core" });
  const document = await createProjectDocument(core, {
    content: `# Design notes\n\nThe ${term} sights the horizon.\n`,
    name: "Design notes",
  });
  const issue = await createIssue({ project: core, title: `${sentenceWord} work` });
  const message = await createMessage(
    issue.key,
    { body: `${sentenceWord} calibration done.` },
    searchSession
  );
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await openSearchFromRail(page, false);
    const search = page.getByRole("combobox", { name: "Search" });
    await search.fill(term);

    const dialog = page.getByRole("dialog", { name: "Search" });
    await expect(dialog.getByRole("option")).toHaveCount(3);
    await expect(
      dialog.locator(`[role="presentation"][aria-label="${core}: Design notes"]`)
    ).toBeVisible();
    await expect(
      dialog.locator(`[role="presentation"][aria-label="${core}: Design notes"]`)
    ).not.toHaveAttribute("data-status");
    await expect(
      page.locator(`[role="presentation"][aria-label="${issue.key}: ${sentenceWord} work"]`)
    ).toHaveAttribute("data-status", "triage");

    await dialog.getByRole("option", { name: /^message / }).click();
    await expect(page).toHaveURL(new RegExp(`/issues/${issue.key}/messages/${message.id}$`));
    await expect(page.getByRole("tab", { name: "Conversation" })).toHaveAttribute(
      "aria-selected",
      "true"
    );
    await expect(
      page.getByRole("list", { name: "Conversation turns" }).locator('li[aria-current="true"]')
    ).toContainText(`${sentenceWord} calibration done.`);

    // The palette opens on the page the reader is already on: Ctrl+K, not another load.
    await page.keyboard.press("Control+k");
    await page.getByRole("combobox", { name: "Search" }).fill(term);
    await dialog.getByRole("option", { name: /^doc / }).click();
    await expect(page).toHaveURL(
      new RegExp(`/projects/${core}/documents/${document.artifact.slug}\\?q=${term}$`)
    );
    await expect(documentEditor(page)).toContainText(term);
  } finally {
    await context.close();
  }
});

// The hint names keys a touch reader does not have, and 33px of palette is 33px of hits, so it
// shows only where a pointer that can hover exists.
test("the palette's keyboard hints show for a pointer and not for a touch reader @phone", async ({
  browser,
}, testInfo) => {
  const core = rowKey("CORE");
  await createProject({ key: core, name: "Core" });
  await createIssue({ project: core, title: "Conversation view" });

  // The reader is a person of the row's own (`rowPerson`, e2e/test.ts): the Inbox behind the
  // palette holds none of another row's questions, whose priority selects carry options of
  // their own, so the first option on the page is the palette's.
  const alice = await asUser(browser, rowPerson("alice"));
  try {
    const page = await alice.newPage();
    await page.goto("/");
    if (testInfo.project.name === "iphone") {
      // No keyboard and no rail: the drawer carries the palette's own control.
      await page.getByRole("button", { name: "Open navigation" }).click();
      await page.getByRole("button", { name: /^search/i }).click();
    } else {
      await expect(page.getByRole("button", { name: /^search/i })).toBeVisible();
      await page.locator("body").focus();
      await page.keyboard.press("Control+k");
    }
    await page.getByRole("combobox", { name: "Search" }).fill("conversation");
    await expect(page.getByRole("option").first()).toBeVisible();

    const hints = page.getByTestId("search-keyboard-hints");
    if (testInfo.project.name === "chromium") {
      await expect(hints).toBeVisible();
      await expect(hints).toContainText("to open");
    } else {
      await expect(hints).toBeHidden();
    }
  } finally {
    await alice.close();
  }
});
