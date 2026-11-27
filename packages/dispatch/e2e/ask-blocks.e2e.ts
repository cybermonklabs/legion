import {
  answerAsk,
  createComment,
  createIssue,
  createIssueArtifact,
  createProject,
  editArtifact,
  getAsk,
  getIssue,
  patchIssue,
  resolveAsk,
} from "./api";
import { expectHosted, hostedCard } from "./ask-blocks-rows";
import { documentEditor, indexedBlockAsk } from "./editor";
import { navigateInApp } from "./sends";
import { expect, type Locator, type Page, test } from "./test";
import { asUser } from "./users";

/** Picks one of the hosted card's options (a single-choice decision's radio row). */
async function pickOption(card: Locator, name: string): Promise<void> {
  await card.getByRole("radio", { name }).check();
}

async function alertsIn(page: Page): Promise<Locator> {
  return page.getByRole("article", { name: "Document" }).getByRole("alert");
}

test("agentWrittenAskBlockAppearsAndAnswersInPlace", async ({ browser }, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "## Specification\n\nContext\n",
    title: "Ask block",
  });
  await patchIssue(issue.key, { labels: ["dispatch", "decision", "priority"] });
  await editArtifact(
    issue.primary_artifact_id,
    {
      ops: [
        {
          after: "end",
          markdown:
            ':::ask{#ship-decision urgency="high" multiple="false" state="open"}\nShould we ship?\n\n- Ship: Release it\n- Hold: Wait for review\n:::\n',
          op: "insert",
        },
      ],
    },
    { as: "agent" }
  );
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.setViewportSize({ width: 1770, height: 900 });
    await page.goto(`/issues/${issue.key}`);
    const ask = documentEditor(page).locator('[data-dispatch-ask-block="ship-decision"]');
    await expect(ask).toContainText("Should we ship?");
    const decisions = page.getByRole("navigation", { name: "Open decisions" });
    await expect(decisions.getByText("1 open decision", { exact: true })).toBeVisible();
    const decisionLink = decisions.getByRole("link", { name: "Should we ship?" });
    await expect(decisionLink).toHaveAttribute("title", "Should we ship?");
    await expect(decisions).toHaveCSS("height", "28px");
    const header = page.getByTestId("issue-header");
    const specification = documentEditor(page).getByRole("heading", { name: "Specification" });
    const desktopMetrics = await Promise.all([
      header.evaluate((element) => element.getBoundingClientRect().height),
      specification.evaluate((element) => element.getBoundingClientRect().y),
    ]);
    await testInfo.attach("specchrome-1770-metrics", {
      body: JSON.stringify({ headerHeight: desktopMetrics[0], specificationY: desktopMetrics[1] }),
      contentType: "application/json",
    });
    await page.screenshot({ path: testInfo.outputPath("specchrome-1770.png") });
    expect(desktopMetrics[0]).toBeLessThanOrEqual(96);

    await page.setViewportSize({ width: 1280, height: 800 });
    const tabletMetrics = await Promise.all([
      header.evaluate((element) => element.getBoundingClientRect().height),
      specification.evaluate((element) => element.getBoundingClientRect().y),
    ]);
    expect(tabletMetrics[0]).toBeLessThanOrEqual(140);
    await testInfo.attach("specchrome-1280-metrics", {
      body: JSON.stringify({ headerHeight: tabletMetrics[0], specificationY: tabletMetrics[1] }),
      contentType: "application/json",
    });

    await page.setViewportSize({ width: 390, height: 844 });
    await page.screenshot({ path: testInfo.outputPath("specchrome-390.png") });

    const hosted = await expectHosted(ask);
    await expect(ask.locator("[data-dispatch-ask-pill]")).toHaveText("High");
    await expect(ask).toHaveAttribute("data-dispatch-ask-urgency", "high");
    // The block hosts the Inbox's ask card for its indexed ask; the card, not the block header,
    // says when it was asked (and by whom, once the server records the editing actor).
    await expect(hosted.locator("time")).toBeVisible();
    await expect(ask.locator("[data-dispatch-ask-asked]")).toHaveCount(0);
    // The question is the editor's text; the card does not repeat it.
    await expect(ask.getByText("Should we ship?")).toHaveCount(1);
    await expect(hosted.getByRole("radio")).toHaveCount(3);
    // Nothing chosen yet: nothing to submit, under the answer box that stands in place.
    const answer = hosted.getByLabel("Your answer");
    const submit = hosted.getByRole("button", { exact: true, name: "Answer" });
    await expect(submit).toBeDisabled();
    await answer.fill("Yes.");
    await answer.press("Enter");
    await expect(answer).toHaveValue("Yes.\n");
    // A note alone is not an answer while the decision has options.
    await expect(submit).toBeDisabled();
    await answer.press("Control+Enter");
    await expect(ask).not.toContainText("Answered by");
    await pickOption(hosted, "Ship Release it");
    // The note field stays open over its text (the compact card's rule) and the note goes with
    // the answer.
    await expect(answer).toHaveValue("Yes.\n");
    await expect(submit).toBeEnabled();
    await answer.press("Control+Enter");
    await expect(ask).toContainText("Answered by alice");
    await expect(ask.locator('ul[aria-label="Options"] li[data-selected="true"]')).toContainText(
      "Ship"
    );
    await expect(hostedCard(ask)).toContainText("Yes.");
    await expect(ask.locator("form")).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

// @pr: The live document: a reader's question threads under a decision block, the decision stays open, and the Inbox row carries the reply.
test("a reader asks back from a decision block: the question threads under the block, the decision stays open, and the Inbox row shows the reply @pr", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "## Specification\n\nContext before the decision.\n",
    title: "Ask back in a spec",
  });
  await editArtifact(
    issue.primary_artifact_id,
    {
      ops: [
        {
          after: "end",
          markdown:
            ':::ask{#ship-decision urgency="high" multiple="false" state="open"}\nShould we ship?\n\n- Ship: Release it\n- Hold: Wait for review\n:::\n',
          op: "insert",
        },
      ],
    },
    { as: "agent" }
  );
  const blockAsk = await indexedBlockAsk(issue.key, "ship-decision");
  // Written as a reader writes: a question, a list, and a fenced command.
  const question =
    "Ship to staging first, or straight to production?\n\n- staging has the new schema\n- prod does not\n\n```sh\nlegion status CORE\n```";
  const questionLead = "Ship to staging first, or straight to production?";
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.setViewportSize({ width: 1280, height: 900 });
    await page.goto(`/issues/${issue.key}`);
    const block = documentEditor(page).locator('[data-dispatch-ask-block="ship-decision"]');
    const hosted = await expectHosted(block);
    // No exchange yet: nothing to disclose.
    await expect(block.getByRole("button", { name: /repl/ })).toHaveCount(0);
    // The same two-row composer as the Inbox card: the words can answer or ask back.
    const askBack = hosted.getByRole("button", { name: "Ask back" });
    await expect(askBack).toBeDisabled();
    await hosted.getByLabel("Your answer").fill(question);
    await expect(askBack).toBeEnabled();
    await block.scrollIntoViewIfNeeded();
    await block.screenshot({
      path: testInfo.outputPath(`askback-composer-${testInfo.project.name}.png`),
    });
    await askBack.click();

    // The clarification remains directly visible under the block while the decision stays open.
    await expect(hosted.getByLabel("Your answer")).toHaveValue("");
    const thread = block.getByTestId(`thread-${blockAsk.id}`);
    await expect(thread).toContainText(questionLead);
    // The reply is typeset as the Inbox typesets it: a real list and a code box, not bare text.
    await expect(thread.locator(".dispatch-markdown ul")).toHaveCSS("list-style-type", "disc");
    await expect(thread.locator(".dispatch-markdown pre")).toHaveCSS("overflow-x", "auto");
    await expect(hosted.getByRole("button", { exact: true, name: "Answer" })).toBeVisible();
    await expect(block).not.toContainText("Answered by");
    // The turn passed to the asker, and the block says so from the refetched issue asks.
    await expect(block.getByTestId(`turn-${blockAsk.id}`)).toHaveText(
      "Waiting on session:e2e-seed…"
    );
    await expect
      .poll(() => getAsk(blockAsk.id))
      .toMatchObject({
        ask: { answer: null, state: "open", waiting_on: "agent" },
        replies: [{ body: question }],
      });
    await block.scrollIntoViewIfNeeded();
    await block.screenshot({
      path: testInfo.outputPath(`askback-reply-${testInfo.project.name}.png`),
    });

    // The Inbox row for the same ask carries the reply. Reached in the app: the row is drawn
    // from the same loaded client, and a second document load would prove nothing more.
    await navigateInApp(page, "/");
    const row = page.getByTestId(`ask-${blockAsk.id}`);
    await expect(row).toBeVisible();
    await expect(row.getByTestId(`thread-${blockAsk.id}`)).toContainText(questionLead);
  } finally {
    await alice.close();
  }
});

// dispatch://INFRA-969 on the page it was reported on. On a decision block that takes several
// answers, a tick of a real option once Other was ticked hid the box for the person's own words
// with Other still ticked, and Answer refuses Other without its words, so the decision could not
// be answered until the reader found the box again. The box stands in place on every card now
// (dispatch://INFRA-5702 R15): it stays while Other is ticked, and the card draws no control that
// hides it. Each absence below is read beside the option it ticked and the box it found.
test("a decision block that takes several answers keeps the box for the reader's own words while Other is ticked, with no control that hides it, and answers with the option and the words", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: 'Intro.\n\n:::ask{#checks urgency="med" multiple="true"}\nWhich checks run before the merge?\n\n- Docs\n- Tests\n:::\n\nAfter.\n',
    title: "Several answers",
  });
  const blockAsk = await indexedBlockAsk(issue.key, "checks");
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.setViewportSize({ width: 1280, height: 900 });
    await page.goto(`/issues/${issue.key}`);
    const block = documentEditor(page).locator('[data-dispatch-ask-block="checks"]');
    const hosted = await expectHosted(block);
    const other = hosted.getByRole("checkbox", { exact: true, name: "Other" });
    const docs = hosted.getByRole("checkbox", { exact: true, name: "Docs" });
    const box = hosted.getByLabel("Your answer");
    const disclosure = hosted.locator("[data-ask-note-disclosure]");
    // Nothing ticked: the box stands in place, and no disclosure is drawn.
    await expect(box).toBeVisible();
    await expect(disclosure).toHaveCount(0);

    await other.check();
    await expect(box).toBeVisible();
    await docs.check();
    await expect(docs).toBeChecked();
    await expect(other).toBeChecked();
    await expect(box).toBeVisible();
    await expect(disclosure).toHaveCount(0);

    await box.fill("And the release notes.");
    await docs.uncheck();
    await expect(docs).not.toBeChecked();
    await docs.check();
    await expect(docs).toBeChecked();
    await expect(other).toBeChecked();
    await expect(box).toHaveValue("And the release notes.");
    await expect(disclosure).toHaveCount(0);

    // Unticking Other drops its words on an ask that takes several answers (`toggleOther`), and
    // the box stands in place, empty, with no disclosure drawn.
    await other.uncheck();
    await expect(other).not.toBeChecked();
    await expect(box).toHaveValue("");
    await expect(disclosure).toHaveCount(0);

    await other.check();
    await box.fill("And the release notes.");
    const answer = hosted.getByRole("button", { exact: true, name: "Answer" });
    await expect(answer).toHaveCount(1);
    await answer.click();
    await expect
      .poll(() => getAsk(blockAsk.id))
      .toMatchObject({
        ask: {
          answer: { selected: ["Docs"], text: "And the release notes." },
          state: "answered",
        },
      });
  } finally {
    await alice.close();
  }
});

test("a resolved decision block is read-only", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", spec: "Context\n", title: "Resolved ask" });
  await editArtifact(
    issue.primary_artifact_id,
    {
      ops: [
        {
          after: "end",
          markdown:
            ':::ask{#resolved-decision urgency="med" multiple="false" state="open"}\nShould we ship?\n\n- Ship\n- Hold\n:::\n',
          op: "insert",
        },
      ],
    },
    { as: "agent" }
  );
  const blockAsk = await indexedBlockAsk(issue.key, "resolved-decision");
  await resolveAsk(
    blockAsk.id,
    { kind: "resolved", reason: "The decision is no longer needed." },
    { as: "agent" }
  );
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}`);
    const ask = documentEditor(page).locator('[data-dispatch-ask-block="resolved-decision"]');
    // The hosted card's resolution record, with its reason; no composer.
    const hosted = await expectHosted(ask);
    await expect(hosted.getByTestId("ask-resolution-badge")).toHaveText("Resolved");
    await expect(hosted).toContainText("The decision is no longer needed.");
    await expect(ask.locator("form")).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

test("a failed in-document decision answer reports inside the failed block and can be retried", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", spec: "Context\n", title: "Failing ask" });
  await editArtifact(
    issue.primary_artifact_id,
    {
      ops: [
        {
          after: "end",
          markdown:
            ':::ask{#failing-decision urgency="med" multiple="false" state="open"}\nShould we ship?\n\n- Ship\n- Hold\n:::\n',
          op: "insert",
        },
      ],
    },
    { as: "agent" }
  );
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    // Dispatch's verdict: a 5xx would be tried again under the answer's key, not refused.
    await page.route("**/api/v1/asks/*/answer", async (route) => {
      await route.fulfill({
        body: JSON.stringify({ code: "ASK_INPUT", error: "Dispatch refused the answer" }),
        contentType: "application/json",
        status: 400,
      });
    });
    await page.goto(`/issues/${issue.key}`);
    await expect(page.getByRole("navigation", { name: "Open decisions" })).toBeVisible();
    const ask = documentEditor(page).locator('[data-dispatch-ask-block="failing-decision"]');
    const hosted = await expectHosted(ask);
    const errors = await alertsIn(page);
    await expect(errors).toHaveCount(0);
    await pickOption(hosted, "Other");
    await hosted.getByLabel("Your answer").fill("Ship after review.");
    await hosted.getByRole("button", { exact: true, name: "Answer" }).click();
    const error = hosted.getByRole("alert");
    await expect(error).toContainText("Could not save your answer.");
    await expect(errors).toHaveCount(1);
    await expect(error.getByRole("button", { name: "Retry" })).toBeEnabled();
  } finally {
    await alice.close();
  }
});

test("malformed decision with a blank option label disables the answer form", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", spec: "Context\n", title: "Malformed ask" });
  const malformed = await createIssueArtifact(
    issue.key,
    {
      content:
        ':::ask{#malformed-decision urgency="high" multiple="false" state="open"}\nWhich path?\n\n- Ship: Release it\n- : No label\n:::\n',
      name: "malformed.md",
    },
    { as: "agent" }
  );
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.setViewportSize({ width: 1280, height: 800 });
    await page.goto(`/issues/${issue.key}/artifacts/${malformed.artifact.slug}`);
    const ask = documentEditor(page).locator('[data-dispatch-ask-block="malformed-decision"]');
    await expect(ask.getByText("Malformed", { exact: true })).toBeVisible();
    await expect(
      ask.getByText(/ask block "malformed-decision" has an option without a label/)
    ).toBeVisible();
    await expect(
      ask.getByText("Fix the block text; the decision re-activates once it parses.")
    ).toBeVisible();
    await expect(ask.locator("form")).toHaveCount(0);
    await expect(ask.locator("li:empty")).toHaveCount(0);
    await expect(ask.getByRole("listitem")).toHaveCount(2);
    await page.screenshot({ path: testInfo.outputPath("specchrome-malformed-1280.png") });
  } finally {
    await alice.close();
  }
});

test("open decision navigation cycles through the available decisions", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", spec: "Context\n", title: "Two decisions" });
  await editArtifact(
    issue.primary_artifact_id,
    {
      ops: [
        {
          after: "end",
          markdown:
            ':::ask{#first-decision urgency="med" multiple="false" state="open"}\nUse Postgres?\n\n- Yes\n:::\n\n:::ask{#second-decision urgency="med" multiple="false" state="open"}\nUse Redis?\n\n- Yes\n:::\n',
          op: "insert",
        },
      ],
    },
    { as: "agent" }
  );
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}`);
    const decisions = page.getByRole("navigation", { name: "Open decisions" });
    await expect(decisions.getByText("2 open decisions", { exact: true })).toBeVisible();
    const firstDecision = await decisions.getByRole("link").textContent();
    expect(firstDecision).not.toBeNull();
    await decisions.getByRole("button", { name: "Next open decision" }).click();
    await expect(decisions.getByRole("link")).not.toHaveText(firstDecision ?? "");
    await decisions.getByRole("button", { name: "Next open decision" }).click();
    await expect(decisions.getByRole("link")).toHaveText(firstDecision ?? "");
  } finally {
    await alice.close();
  }
});

const DECISION_SPEC = [
  ':::ask{#storage urgency="blocking" multiple="false" state="open"}\nWhich storage engine backs the event log?\n\n- Postgres: Reuse the existing cluster\n- SQLite: One file per team, no ops\n:::\n',
  "Some prose between the decisions so the margins around each block are visible.\n",
  ':::ask{#retention urgency="high" multiple="true" state="open"}\nWhich retention windows do we support at launch?\n\n- 7 days\n- 30 days\n- Forever\n:::\n',
  ':::ask{#naming urgency="low" multiple="false" state="open"}\nShould the CLI be called `legion` or `lg`?\n\n- legion\n- lg\n:::\n',
  "Closing paragraph after the last decision.\n",
].join("\n");

// SLOW(>=2s): four cold loads of the issue document - two widths at two colour schemes - each
// with its own screenshot, over a spec seeded with three decisions, an answer and a reply.
test("decision blocks read as urgency-accented cards in the document and its version view", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "## Specification\n\nContext paragraph before the first decision.\n",
    title: "Decision rendering",
  });
  await editArtifact(
    issue.primary_artifact_id,
    { ops: [{ after: "end", markdown: DECISION_SPEC, op: "insert" }] },
    { as: "agent" }
  );
  await expect.poll(async () => ((await getIssue(issue.key)).open_asks ?? []).length).toBe(3);
  const naming = ((await getIssue(issue.key)).open_asks ?? []).find(
    (ask) => ask.block_id === "naming"
  );
  if (naming === undefined) {
    throw new Error("the naming decision was not indexed");
  }
  await answerAsk(
    naming.id,
    { expected_edited_at: null, selected: ["legion"], text: "Short names are for shells." },
    { login: "bob" }
  );
  // An exchange under the answered decision, written in Markdown as agents write.
  await createComment(
    issue.key,
    { ask_id: naming.id, body: "Noted. Two follow-ups:\n\n- alias `lg`\n- update the docs" },
    { as: "agent" }
  );
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const editor = documentEditor(page);
    for (const [width, height] of [
      [1280, 900],
      [390, 844],
    ] as const) {
      for (const scheme of ["light", "dark"] as const) {
        await page.emulateMedia({ colorScheme: scheme });
        await page.setViewportSize({ width, height });
        await page.goto(`/issues/${issue.key}`);
        await expect(editor.locator('[data-dispatch-ask-block="naming"]')).toContainText(
          "Answered by bob"
        );
        await expectHosted(editor.locator('[data-dispatch-ask-block="storage"]'));
        await page.screenshot({
          fullPage: width > 400,
          path: testInfo.outputPath(`decisions-${width}-${scheme}.png`),
        });
      }
    }

    for (const [blockId, urgency, label] of [
      ["storage", "blocking", "Blocking"],
      ["retention", "high", "High"],
      ["naming", "low", "Low"],
    ] as const) {
      const block = editor.locator(`[data-dispatch-ask-block="${blockId}"]`);
      await expect(block.locator("[data-dispatch-ask-pill]")).toHaveText(label);
      await expect(block).toHaveAttribute("data-dispatch-ask-urgency", urgency);
      await expect(block.getByText("Decision", { exact: true })).toBeVisible();
    }
    const storage = editor.locator('[data-dispatch-ask-block="storage"]');
    const context = editor.getByText("Context paragraph before the first decision.");
    // The rows are the options; the source "- Option: description" list stays folded until the
    // caret is in the block, and folds again when the caret leaves.
    const source = storage.locator("[data-proof-block-content] ul");
    await expect(source).toBeHidden();
    await storage.getByText("Which storage engine backs the event log?").click();
    await expect(storage).toHaveAttribute("data-dispatch-ask-editing", "true");
    await expect(source).toBeVisible();
    await expect(source.getByText("Postgres: Reuse the existing cluster")).toBeVisible();
    await context.click();
    await expect(storage).not.toHaveAttribute("data-dispatch-ask-editing", "true");
    await expect(source).toBeHidden();
    // The hosted card offers the options as the shared choice rows plus Other; nothing to
    // submit until one is chosen.
    const storageCard = hostedCard(storage);
    await expect(storageCard.getByRole("radio")).toHaveCount(3);
    await expect(storageCard.getByText("Reuse the existing cluster")).toBeVisible();
    await expect(storageCard.getByRole("button", { exact: true, name: "Answer" })).toBeDisabled();
    await pickOption(storageCard, "Postgres Reuse the existing cluster");
    await expect(storageCard.getByRole("button", { exact: true, name: "Answer" })).toBeEnabled();

    // The card sits in the prose with the same gap above and below it.
    const gaps = await storage.evaluate((section) => {
      const previous = section.previousElementSibling?.getBoundingClientRect();
      const next = section.nextElementSibling?.getBoundingClientRect();
      const own = section.getBoundingClientRect();
      return previous === undefined || next === undefined
        ? undefined
        : { above: own.top - previous.bottom, below: next.top - own.bottom };
    });
    expect(gaps).toBeDefined();
    expect(Math.abs((gaps?.above ?? 0) - (gaps?.below ?? 1))).toBeLessThan(1);
    expect(gaps?.above).toBeGreaterThan(12);

    const answered = editor.locator('[data-dispatch-ask-block="naming"]');
    await expect(answered.locator('li[data-selected="true"]')).toContainText("legion");
    await expect(answered.locator('li[data-selected="false"]')).toContainText("lg");
    await expect(hostedCard(answered)).toContainText("Short names are for shells.");
    await expect(answered.locator("form")).toHaveCount(0);
    // The exchange under the record is the card's own list, not document prose: no marker, no
    // indent, the card's spacing — and the reply's Markdown list keeps its markers.
    const thread = answered.getByTestId(`thread-${naming.id}`);
    const replies = thread.locator("> ul");
    await expect(replies).toHaveCSS("list-style-type", "none");
    await expect(replies).toHaveCSS("padding-left", "0px");
    await expect(replies).toHaveCSS("margin-top", "0px");
    const replyList = thread.locator(".dispatch-markdown ul");
    await expect(replyList).toHaveCSS("list-style-type", "disc");
    await expect(replyList.locator("li")).toHaveCount(2);
    // Once answered, the rows are the record and the source list never shows, caret or not.
    const answeredSource = answered.locator("[data-proof-block-content] ul");
    await expect(answeredSource).toBeHidden();
    await answered.getByText("Should the CLI be called").click();
    await expect(answered).toHaveAttribute("data-dispatch-ask-editing", "true");
    await expect(answeredSource).toBeHidden();
    await context.click();

    // The historical version keeps the block's own record, read-only: options shown, nothing to
    // answer, no card.
    await page.emulateMedia({ colorScheme: "light" });
    await page.setViewportSize({ width: 1280, height: 900 });
    await page.getByRole("combobox", { name: "Version" }).selectOption("2");
    const versionView = page.getByTestId("version-view");
    const historical = versionView.locator('[data-dispatch-ask-block="storage"]');
    await expect(historical.locator("[data-dispatch-ask-pill]")).toHaveText("Blocking");
    await expect(historical.locator("form")).toHaveCount(0);
    const historicalSource = historical.locator("[data-proof-block-content] ul");
    await expect(historicalSource).toBeHidden();
    await historical.getByText("Which storage engine backs the event log?").click();
    await expect(historicalSource).toBeHidden();
    await expect(hostedCard(historical)).toHaveCount(0);
    await expect(historical.locator('ul[aria-label="Options"] li')).toHaveCount(2);
    await expect(
      historical.locator('ul[aria-label="Options"]').getByText("Reuse the existing cluster")
    ).toBeVisible();
    await expect(historical.getByRole("radio")).toHaveCount(0);
    await expect(historical.getByRole("button", { exact: true, name: "Answer" })).toHaveCount(0);
    await page.screenshot({ path: testInfo.outputPath("decisions-version-1280-light.png") });
  } finally {
    await alice.close();
  }
});
