import { expect, test } from "@playwright/test";

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
import { openSearchFromRail, searchSession, searchTerm, seedSearchFixture } from "./search";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/**
 * The search palette, in the rows whose claim is what the index found: the grouped hits and
 * their marks, an ask hit's own answer, and the two routes a hit opens. The keyboard-hints row
 * is here too: it is read in the desktop and the phone configurations at once, and a stubbed
 * row runs in one of them. What the palette draws over one fixed answer is in
 * `search.stub.e2e.ts`, and the fixture both files read is in `search.ts`.
 */

test.beforeEach(async () => {
  await resetDatabase();
});

test("Ctrl+K and Cmd+K open the palette, grouped results navigate a document to its highlighted term", async ({
  browser,
}) => {
  const fixture = await seedSearchFixture();
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
    await search.fill(searchTerm);

    const options = dialog.getByRole("option");
    await expect(options).toHaveCount(2);
    const documentResult = dialog.getByRole("option", { name: /^doc spec\.md / });
    await expect(documentResult).toBeVisible();
    await expect(documentResult.locator("mark")).toHaveText([searchTerm, searchTerm]);
    await expect(dialog.locator('[role="presentation"]')).toHaveCount(2);
    await expect(
      dialog.locator('[role="presentation"][aria-label="CORE-1: Navigation instruments"]')
    ).toBeVisible();
    await expect(
      dialog.locator('[role="presentation"][aria-label="CORE-2: Other work"]')
    ).toBeVisible();

    const documentID = await documentResult.getAttribute("id");
    expect(documentID).not.toBeNull();
    await search.press("ArrowDown");
    await expect(search).toHaveAttribute("aria-activedescendant", /search-option-comment-/);
    await search.press("ArrowUp");
    await expect(search).toHaveAttribute("aria-activedescendant", documentID as string);
    await search.press("Enter");

    await expect(page).toHaveURL(
      new RegExp(`/issues/${fixture.firstIssueKey}/spec\\?q=${searchTerm}$`)
    );
    const editor = documentEditor(page);
    await expect(editor).toContainText(searchTerm);
    await expect(editor).toBeInViewport();
    await expect
      .poll(() =>
        page.evaluate(() => {
          // Playwright's TypeScript context lacks CSS Highlight API declarations.
          const css = CSS as unknown as { highlights?: Map<string, Highlight> };
          const highlights = css.highlights?.get("dispatch-search");
          return (
            highlights !== undefined &&
            [...highlights].some((range) => range.toString() === "astrolabe")
          );
        })
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
  await seedSearchFixture();
  const titleIssue = await createIssue({ project: "CORE", title: "Astrolabe calibration" });
  await createAsk(titleIssue.key, { question: "Who calibrates the astrolabe?" }, searchSession);
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await openSearchFromRail(page, false);
    const search = page.getByRole("combobox", { name: "Search" });
    await search.fill(searchTerm);

    // Scoped to the palette: inbox rows carry a priority <select> whose options share the role.
    const dialog = page.getByRole("dialog", { name: "Search" });
    const options = dialog.getByRole("option");
    await expect(options).toHaveCount(4);
    for (const name of [/^issue /, /^doc /, /^comment /, /^ask /]) {
      const option = dialog.getByRole("option", { name });
      await expect(option).toHaveCount(1);
      await expect(option.locator("mark").first()).toHaveText(/astrolabe/i);
    }
    await expect(dialog.locator('[role="presentation"]')).toHaveCount(3);
    await expect(
      dialog.locator('[role="presentation"][aria-label="CORE-4: Astrolabe calibration"]')
    ).toBeVisible();
  } finally {
    await context.close();
  }
});

test("an ask hit says whether it is answered, and an answered one shows its latest answer, who gave it and the options chosen", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Planetarium" });
  const answered = await createAsk(
    issue.key,
    {
      options: [{ label: "Brass" }, { label: "Steel" }],
      question: "Which orrery frame do we keep?",
    },
    searchSession
  );
  const resolved = await createAsk(
    issue.key,
    { question: "Should the orrery gears be oiled?" },
    searchSession
  );
  await createAsk(issue.key, { question: "Who winds the orrery?" }, searchSession);
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
    await page.getByRole("combobox", { name: "Search" }).fill("orrery");

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
  await createProject({ key: "CORE", name: "Core" });
  const document = await createProjectDocument("CORE", {
    content: "# Design notes\n\nThe sextant sights the horizon.\n",
    name: "Design notes",
  });
  const issue = await createIssue({ project: "CORE", title: "Sextant work" });
  const message = await createMessage(
    issue.key,
    { body: "Sextant calibration done." },
    searchSession
  );
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await openSearchFromRail(page, false);
    const search = page.getByRole("combobox", { name: "Search" });
    await search.fill("sextant");

    const dialog = page.getByRole("dialog", { name: "Search" });
    await expect(dialog.getByRole("option")).toHaveCount(3);
    await expect(
      dialog.locator('[role="presentation"][aria-label="CORE: Design notes"]')
    ).toBeVisible();
    await expect(
      dialog.locator('[role="presentation"][aria-label="CORE: Design notes"]')
    ).not.toHaveAttribute("data-status");
    await expect(
      page.locator(`[role="presentation"][aria-label="${issue.key}: Sextant work"]`)
    ).toHaveAttribute("data-status", "triage");

    await dialog.getByRole("option", { name: /^message / }).click();
    await expect(page).toHaveURL(new RegExp(`/issues/${issue.key}/messages/${message.id}$`));
    await expect(page.getByRole("tab", { name: "Conversation" })).toHaveAttribute(
      "aria-selected",
      "true"
    );
    await expect(
      page.getByRole("list", { name: "Conversation turns" }).locator('li[aria-current="true"]')
    ).toContainText("Sextant calibration done.");

    // The palette opens on the page the reader is already on: Ctrl+K, not another load.
    await page.keyboard.press("Control+k");
    await page.getByRole("combobox", { name: "Search" }).fill("sextant");
    await dialog.getByRole("option", { name: /^doc / }).click();
    await expect(page).toHaveURL(
      new RegExp(`/projects/CORE/documents/${document.artifact.slug}\\?q=sextant$`)
    );
    await expect(documentEditor(page)).toContainText("sextant");
  } finally {
    await context.close();
  }
});

// The hint names keys a touch reader does not have, and 33px of palette is 33px of hits, so it
// shows only where a pointer that can hover exists.
test("the palette's keyboard hints show for a pointer and not for a touch reader @phone", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  await createIssue({ project: "CORE", title: "Conversation view" });

  const alice = await asUser(browser, "alice");
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
