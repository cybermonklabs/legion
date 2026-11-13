import {
  answerAsk,
  createAsk,
  createComment,
  createIssue,
  createMessage,
  createProject,
  createProjectDocument,
  resolveAsk,
} from "./api";
import { documentEditor } from "./editor";
import { openSearchFromRail, searchSession, searchTerm, seedSearchFixture } from "./search";
import { expect, test } from "./test";
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
    // Three rows, in the server's order: the issue whose spec holds the term (an issue's
    // keyword vector carries its primary document's text), that document, and the comment.
    await expect(options).toHaveCount(3);
    await expect(options.first()).toHaveAttribute(
      "id",
      `search-option-issue-${fixture.firstIssueKey}`
    );
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
    // Down to the comment, the last of the three, and back up to the document.
    await search.press("ArrowDown");
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
    // Five rows: the four the term is marked in, and CORE-1, which the term reached through
    // its spec (an issue's keyword vector carries its primary document's text) and whose own
    // title never held it, so its row marks nothing.
    await expect(options).toHaveCount(5);
    for (const name of [/^issue Astrolabe calibration/, /^doc /, /^comment /, /^ask /]) {
      const option = dialog.getByRole("option", { name });
      await expect(option).toHaveCount(1);
      await expect(option.locator("mark").first()).toHaveText(/astrolabe/i);
    }
    const foundByItsSpec = dialog.getByRole("option", { name: /^issue Navigation instruments/ });
    await expect(foundByItsSpec).toHaveCount(1);
    await expect(foundByItsSpec.locator("mark")).toHaveCount(0);
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

// A search for an issue's key, the commonest search there is, over the marks Postgres itself
// writes: ts_headline marks `CORE-1` as `<b>CORE</b><b>-1</b>`, inside an address and as a bare key
// alike, so no fixture of this suite decides the shape the palette draws. Each hit that cites the
// issue reads as its name, marked whole, with no address left.
test("a search for an issue key names the issue in every hit that cites it, marked whole", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const cited = await createIssue({ project: "CORE", title: "Gyroscope alignment" });
  const citing = await createIssue({ project: "CORE", title: "Bench work" });
  await createComment(citing.key, {
    body: `Tracked as dispatch://${cited.key} since Monday.`,
  });
  await createMessage(citing.key, { body: `Tracked as ${cited.key} since Monday.` }, searchSession);
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await openSearchFromRail(page, false);
    await page.getByRole("combobox", { name: "Search" }).fill(cited.key);
    const dialog = page.getByRole("dialog", { name: "Search" });
    for (const name of [/^comment /, /^message /]) {
      const hit = dialog.getByRole("option", { name });
      await expect(hit).toHaveCount(1);
      await expect(hit.locator("mark")).toHaveText([`${cited.key} Gyroscope alignment`]);
      await expect(hit.locator("mark [data-dispatch-ref]")).toHaveCount(1);
      await expect(hit).not.toContainText("dispatch://");
      // ts_headline ends its fragment on the last word, so the sentence's stop is not in it.
      await expect(hit).toContainText(`Tracked as ${cited.key} Gyroscope alignment since Monday`);
    }
  } finally {
    await context.close();
  }
});

// One renderer: a hit reads its text the way the comment it came from reads it. A key the
// writer put in a code span is code in the comment, so it names nothing in the hit either; and a
// link whose words the writer wrote shows those words, never the link's markdown with the
// target's name drawn inside it. Postgres cuts a hit's fragment where its own parser finds a
// word, so a fragment can start inside the backticks or the brackets the source wrote.
test("a search hit draws no reference the comment it quotes holds in code, and no worded link's markdown", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const cited = await createIssue({ project: "CORE", title: "Gyroscope alignment" });
  const citing = await createIssue({ project: "CORE", title: "Bench work" });
  await createComment(citing.key, { body: `Run \`${cited.key}\` locally tagcodespan.` });
  await createComment(citing.key, {
    body: `See [the gyroscope](dispatch://${cited.key}) now tagworded.`,
  });
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await openSearchFromRail(page, false);
    const box = page.getByRole("combobox", { name: "Search" });
    const dialog = page.getByRole("dialog", { name: "Search" });

    await box.fill("CORE tagcodespan");
    const code = dialog.getByRole("option", { name: /tagcodespan/ });
    await expect(code).toHaveCount(1);
    await expect.soft(code.locator("[data-dispatch-ref]")).toHaveCount(0);
    await expect.soft(code).not.toContainText(cited.title);

    await box.fill("CORE tagworded");
    const worded = dialog.getByRole("option", { name: /tagworded/ });
    await expect(worded).toHaveCount(1);
    await expect.soft(worded).not.toContainText("](");
    await expect.soft(worded).not.toContainText(cited.title);
  } finally {
    await context.close();
  }
});

// The same pass, held to what the link pass links, over Postgres's own marks. A mark grows over a
// reference only where that reference names something and is drawn as a link: `ts_headline`
// marks `<b>SHA</b>-256`, `<b>UTF</b>-8`, `#<b>general</b>`, `#<b>1234</b>`, `<b>COVID</b>-19`
// and, for an issue that does not exist, `<b>CORE</b>-99`, and none of those is a reference that
// links, so each keeps the mark the server sent. The `COVID-19` body also cites an issue that
// does exist, so the link pass runs over it and its filter is what holds the mark. The query
// `CORE` marks `<b>CORE</b>-1` of that issue, and that mark grows to the whole key, which reads
// as the issue's name. (`ts_headline` starts its fragment a word in, so no row reads the
// sentence's first word.)
// SLOW(>=2s): seven seeded comments and seven searches, each answered by Postgres's headline.
test("a search mark grows over a reference only where it links, and stays as the server sent it elsewhere", async ({
  browser,
}) => {
  // Each soft read that fails waits out its own 15 s, and a run against a wrong pass should name
  // every body whose marks moved, so the row has room for all twelve.
  test.setTimeout(240_000);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Bench work" });
  const missing = "CORE-99";
  const bodies = [
    {
      query: "sha",
      text: "We hash every upload with SHA-256 before it reaches the bucket on the storage side.",
      marks: ["SHA"],
      refs: 0,
      shows: "SHA-256",
    },
    {
      query: "utf",
      text: "Store every name as UTF-8 throughout the pipeline so nothing is lost on the way.",
      marks: ["UTF"],
      refs: 0,
      shows: "UTF-8",
    },
    {
      query: "general",
      text: "The question was posted in #general this morning by the person who owns the board.",
      marks: ["general"],
      refs: 0,
      shows: "#general",
    },
    {
      query: "1234",
      text: "We merged the fix in PR #1234 for the patch release that shipped last week.",
      marks: ["1234"],
      refs: 0,
      shows: "#1234",
    },
    {
      query: "CORE",
      text: `The release is blocked on ${missing} until the vendor answers our question on Friday.`,
      marks: ["CORE"],
      refs: 0,
      shows: missing,
    },
    {
      query: "covid",
      text: `The COVID-19 notice that ${issue.key} tracks goes out to every office on Monday morning.`,
      marks: ["COVID"],
      refs: 1,
      shows: "COVID-19",
    },
  ];
  for (const body of bodies) {
    await createComment(issue.key, { body: body.text });
  }
  await createComment(issue.key, {
    body: `The launch is waiting on ${issue.key} until the vendor answers our question on Friday.`,
  });
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await openSearchFromRail(page, false);
    const search = page.getByRole("combobox", { name: "Search" });
    const dialog = page.getByRole("dialog", { name: "Search" });
    for (const body of bodies) {
      await search.fill(body.query);
      const hit = dialog.getByRole("option", { name: /^comment / }).filter({ hasText: body.shows });
      await expect(hit, body.query).toHaveCount(1);
      // Soft, so a run names every body whose marks moved rather than the first alone.
      await expect.soft(hit.locator("mark"), body.query).toHaveText(body.marks);
      await expect.soft(hit.locator("[data-dispatch-ref]"), body.query).toHaveCount(body.refs);
    }
    // The query that marks a part of a real key, which the link pass links.
    await search.fill("CORE");
    const linked = dialog
      .getByRole("option", { name: /^comment / })
      .filter({ hasText: "is waiting on" });
    await expect(linked).toHaveCount(1);
    await expect(linked.locator("mark")).toHaveText([`${issue.key} Bench work`]);
    await expect(linked.locator("mark [data-dispatch-ref]")).toHaveCount(1);
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
