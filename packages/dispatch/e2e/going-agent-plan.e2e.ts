import { createProject, createProjectDocument, getIssue, listIssues, patchIssue } from "./api";
import type { ChatTurn } from "./fake-chat-model";
import { modelsLane, proposePlan, prose, say, scriptChat } from "./fake-models-helpers";
import {
  askTheAgent,
  bar,
  boxHint,
  conversationBox,
  newestRun,
  openBar,
  openBarOver,
  planCard,
  planOnPage,
  planRows,
  RUN_STEPS_MS,
  sidePanel,
} from "./going-locators";
import { type Split, seedSplit } from "./going-panel";
import { expect, type Page, test } from "./test";
import { withAlice } from "./users";

// Check c4 of dispatch://INFRA-1588/artifact/verification-md (spec R4, plan.md settlement 7): a
// split request, asked with Enter on the line Ask and so kept in the panel at the side, draws its
// plan on the page that shows its items - the issue list, the board, the parent's Children tab, a
// document's page - and otherwise in the plan card of the panel; the person unticks a row, which
// unticks the row that needs it, edits a title, and Enter in the panel's box runs the rest, so the
// store holds exactly the approved changes; a row whose item another person changes waits again
// with the new value; words revise the plan; and a plan whose conversation is not on the screen
// draws on no page. Negative control: a runner that runs the unticked row, which the store then
// holds.

/** The split's plan: a new issue under the parent, a retitle of the second child, and a start of
 *  the first that needs the new issue. */
function splitPlan(split: Split, summary = "Split the observatory in two."): ChatTurn {
  return proposePlan(summary, [
    {
      arguments: {
        parent: split.parent,
        project: "CORE",
        spec: "The second half of the observatory's work, split out of it.\n",
        title: "Raise the dome, second half",
      },
      tool: "dispatch_issue",
    },
    {
      arguments: { issue: split.second, title: "Raise the dome, first half" },
      tool: "dispatch_issue_update",
    },
    {
      arguments: { issue: split.first, status: "in_progress" },
      needs: [0],
      tool: "dispatch_issue_update",
    },
  ]);
}

async function scriptSplit(words: string, split: Split, afterwards?: ChatTurn): Promise<void> {
  await scriptChat({
    default: prose("A short answer while the person pauses."),
    runs: [
      {
        turns: [splitPlan(split), ...(afterwards === undefined ? [] : [afterwards])],
        words,
        ...(afterwards === undefined ? { afterwards: say("Done.") } : {}),
      },
    ],
  });
}

async function planWaits(page: Page): Promise<void> {
  await expect(newestRun(page)).toHaveAttribute("data-state", "waiting", { timeout: RUN_STEPS_MS });
}

// SLOW(>=2s): a seeded split and a run that plans it.
test("the issue list draws the split's plan on itself, and its new issue waits with the plan", async ({
  browser,
}) => {
  modelsLane();
  test.setTimeout(90_000);
  const split = await seedSplit();
  const words = `split ${split.parent} in two on the list`;
  await scriptSplit(words, split);
  const before = (await listIssues("CORE")).length;
  await withAlice(browser, async (page) => {
    await openBarOver(page, "/projects/CORE/issues");
    await askTheAgent(page, words);
    await planWaits(page);
    await expect(planRows(planOnPage(page))).toHaveCount(3);
    const created = planRows(planOnPage(page)).filter({ hasText: "Raise the dome, second half" });
    await expect(created).toHaveAttribute("data-state", "waiting");
    expect((await listIssues("CORE")).length).toBe(before);
  });
});

// SLOW(>=2s): a seeded split and a run that plans it on the board.
test("the board", async ({ browser }) => {
  modelsLane();
  test.setTimeout(90_000);
  const split = await seedSplit();
  const words = `split ${split.parent} in two on the board`;
  await scriptSplit(words, split);
  await withAlice(browser, async (page) => {
    await page.goto("/projects/CORE/issues");
    await page.getByRole("button", { name: "Board" }).click();
    await expect(page.getByRole("region", { name: "Project board" })).toBeVisible();
    await openBar(page);
    await askTheAgent(page, words);
    await planWaits(page);
    await expect(planOnPage(page)).toHaveAttribute("data-surface", "issues");
    await expect(planRows(planOnPage(page))).toHaveCount(3);
  });
});

// SLOW(>=2s): a seeded split and a run that plans it on the parent's Children tab.
test("the issue page", async ({ browser }) => {
  modelsLane();
  test.setTimeout(90_000);
  const split = await seedSplit();
  const words = `split ${split.parent} in two on its page`;
  await scriptSplit(words, split);
  await withAlice(browser, async (page) => {
    await openBarOver(page, `/issues/${split.parent}/children`);
    await askTheAgent(page, words);
    await planWaits(page);
    const children = planOnPage(page).and(page.locator('[data-surface="children"]'));
    await expect(planRows(children)).toHaveCount(3);
  });
});

// SLOW(>=2s): a seeded document and a run that plans a suggestion in it.
test("the document page", async ({ browser }) => {
  modelsLane();
  test.setTimeout(90_000);
  await createProject({ key: "CORE", name: "Core" });
  await createProjectDocument("CORE", {
    content: "# Observatory\n\nThe dome turns by hand.\n",
    name: "Observatory",
  });
  const words = "say the dome turns by motor";
  await scriptChat({
    default: prose("A short answer while the person pauses."),
    runs: [
      {
        afterwards: say("Suggested."),
        turns: [
          proposePlan("Suggest that the dome turns by motor.", [
            {
              arguments: {
                artifact: "Observatory",
                body: "The dome turns by motor now, so the text says so. Nothing else changes.",
                project: "CORE",
                quote: "The dome turns by hand.",
                replace_with: "The dome turns by motor.",
              },
              tool: "dispatch_suggest",
            },
          ]),
        ],
        words,
      },
    ],
  });
  await withAlice(browser, async (page) => {
    await page.goto("/projects/CORE/documents");
    await page.getByRole("link", { name: "Observatory" }).first().click();
    await expect(page).toHaveURL(/\/projects\/CORE\/documents\//);
    await openBar(page);
    await askTheAgent(page, words);
    await planWaits(page);
    const drawn = planOnPage(page).and(page.locator('[data-surface="document"]'));
    await expect(drawn.getByTestId("going-plan-sentences")).toContainText("turns by motor now");
    await expect(drawn.getByTestId("going-plan-diff")).not.toHaveAttribute("open", "");
  });
});

// SLOW(>=2s): a run that plans from the Inbox, an untick, an edit and the approval.
test("from the Inbox the plan is in the card: untick unticks what needs it, an edit holds, and Enter runs exactly the rest", async ({
  browser,
}) => {
  modelsLane();
  test.setTimeout(120_000);
  const split = await seedSplit();
  const words = `split ${split.parent} in two from the inbox`;
  await scriptSplit(words, split);
  const before = (await listIssues("CORE")).length;
  // The status the first child holds, which the unticked row would set to in_progress.
  const firstStatus = (await getIssue(split.first)).status;
  expect(firstStatus).not.toBe("in_progress");
  await withAlice(browser, async (page) => {
    await openBarOver(page, "/");
    await askTheAgent(page, words);
    await planWaits(page);
    const card = planCard(page);
    await expect(planRows(card)).toHaveCount(3);
    await expect(planOnPage(page)).toHaveCount(0);
    // A new issue inside the split waits with the plan: nothing is filed before approval.
    await expect(planRows(card).nth(0)).toHaveAttribute("data-state", "waiting");
    expect((await listIssues("CORE")).length).toBe(before);

    // Each row's tick is named by what it does and its item, the new issue's by its title.
    await card.getByRole("checkbox", { name: "File Raise the dome, second half" }).uncheck();
    await expect(planRows(card).nth(2)).toHaveAttribute("data-ticked", "false");
    await planRows(card).nth(1).getByRole("button", { name: "Edit title" }).click();
    const title = card.getByRole("textbox", { name: "New title" });
    await title.fill("Raise the dome, the half that turns");
    await title.press("Enter");
    await conversationBox(page).focus();
    await conversationBox(page).press("Enter");

    await expect(newestRun(page)).toHaveAttribute("data-state", "done", { timeout: RUN_STEPS_MS });
    expect((await getIssue(split.second)).title).toBe("Raise the dome, the half that turns");
    expect((await getIssue(split.first)).status).toBe(firstStatus);
    expect((await listIssues("CORE")).length).toBe(before);
  });
});

// SLOW(>=2s): a waiting run whose plan the bar reads late, and the approval once it has.
test("before the bar has read the waiting plan, the hint says so, Enter opens no row and starts no run, and it approves once it has", async ({
  browser,
}) => {
  modelsLane();
  test.setTimeout(120_000);
  const split = await seedSplit();
  const words = `split ${split.parent} before the plan is read`;
  await scriptSplit(words, split);
  await withAlice(browser, async (page) => {
    // The bar's read of the plan (`GET /api/v1/going/runs/{id}/plan`) is held until released,
    // so the run's frames say it waits while the bar has no plan to approve.
    const { promise: held, resolve: release } = Promise.withResolvers<void>();
    await page.route(/\/api\/v1\/going\/runs\/[^/]+\/plan(\?|$)/u, async (route) => {
      if (route.request().method() === "GET") await held;
      await route.continue();
    });
    await openBarOver(page, "/");
    const opened = page.url();
    await askTheAgent(page, words);
    await planWaits(page);
    await expect(planCard(page)).toHaveCount(0);
    // Words in the box: the hint under it names the read, and Enter keeps the words and starts
    // no run.
    await conversationBox(page).fill("keep the dome whole");
    await expect(boxHint(page)).toContainText("Reading the plan");
    await conversationBox(page).press("Enter");
    await expect(conversationBox(page)).toHaveValue("keep the dome whole");
    await conversationBox(page).fill("");
    await conversationBox(page).press("Enter");

    release();
    await expect(planCard(page)).toHaveAttribute("data-state", "waiting", {
      timeout: RUN_STEPS_MS,
    });
    // The early Enter opened nothing: the panel is open on the page the bar opened over, and the
    // run still waits.
    await expect(sidePanel(page)).toBeVisible();
    expect(page.url()).toBe(opened);
    await expect(newestRun(page)).toHaveAttribute("data-state", "waiting");
    await conversationBox(page).press("Enter");
    await expect(newestRun(page)).toHaveAttribute("data-state", "done", { timeout: RUN_STEPS_MS });
  });
});

// SLOW(>=2s): a waiting plan, another person's change of one of its items, and the approval.
test("a row whose item another person changes after the plan showed shows the new value and waits again", async ({
  browser,
}) => {
  modelsLane();
  test.setTimeout(120_000);
  const split = await seedSplit();
  const words = `split ${split.parent} while bob renames`;
  await scriptSplit(words, split);
  await withAlice(browser, async (page) => {
    await openBarOver(page, "/");
    await askTheAgent(page, words);
    await planWaits(page);
    // The row is of a page that has not read the plan again since bob's write: such a page shows
    // the old title, and its Enter approves the plan as it showed it. A page that has read it again
    // shows bob's title, and its Enter approves that, so the row runs; which of the two a page is
    // when Enter lands is the live update's race (7 of 8 and 5 of 8 runs at 5676cdad and b8db472b
    // were the first). So the page's reads of the plan are held from before the write until the
    // approval is answered, and then go on; the route stays, and lets every later read through.
    const plan = "**/api/v1/going/runs/*/plan";
    const { promise: approvalAnswered, resolve: answered } = Promise.withResolvers<void>();
    await page.route(plan, async (route) => {
      if (route.request().method() === "GET") {
        await approvalAnswered;
      }
      await route.continue();
    });
    await patchIssue(split.second, { title: "Bob's dome" }, { login: "bob" });
    const approval = page.waitForResponse(
      (response) =>
        response.request().method() === "POST" && response.url().endsWith("/plan/approve")
    );
    await conversationBox(page).focus();
    await conversationBox(page).press("Enter");
    expect((await approval).ok()).toBe(true);
    answered();
    const changed = planRows(planCard(page)).filter({
      has: page.getByTestId("going-plan-changed"),
    });
    await expect(changed).toBeVisible({ timeout: RUN_STEPS_MS });
    await expect(changed).toContainText("Bob's dome");
    await expect(changed).toHaveAttribute("data-state", "waiting");
    await expect(newestRun(page)).toHaveAttribute("data-state", "waiting");
  });
});

// SLOW(>=2s): a waiting plan and a revision in words.
test("a revision in words replaces the plan", async ({ browser }) => {
  modelsLane();
  test.setTimeout(120_000);
  const split = await seedSplit();
  const words = `split ${split.parent} and then revise`;
  await scriptSplit(
    words,
    split,
    splitPlan(split, "Split the observatory, keeping the dome whole.")
  );
  await withAlice(browser, async (page) => {
    await openBarOver(page, "/");
    await askTheAgent(page, words);
    await planWaits(page);
    await expect(planCard(page)).toHaveAttribute("data-plan", "1");
    await conversationBox(page).fill("keep the dome whole");
    await expect(boxHint(page)).toContainText("sends your words");
    await conversationBox(page).press("Enter");
    await expect(planCard(page)).toHaveAttribute("data-plan", "2", { timeout: RUN_STEPS_MS });
    await expect(planCard(page)).toContainText("keeping the dome whole");
  });
});

// SLOW(>=2s): a plan with a row the list draws and a row only the card can.
test("a plan with rows on the page and in the card shows each in its place", async ({
  browser,
}) => {
  modelsLane();
  test.setTimeout(90_000);
  const split = await seedSplit();
  const words = `retitle ${split.second} and tell its agent`;
  await scriptChat({
    default: prose("A short answer while the person pauses."),
    runs: [
      {
        afterwards: say("Done."),
        turns: [
          proposePlan("Retitle the dome and say so.", [
            {
              arguments: { issue: split.second, title: "Raise the great dome" },
              tool: "dispatch_issue_update",
            },
            {
              arguments: { body: "The dome is now the great dome.", issue: split.second },
              tool: "dispatch_message",
            },
          ]),
        ],
        words,
      },
    ],
  });
  await withAlice(browser, async (page) => {
    await openBarOver(page, "/projects/CORE/issues");
    await askTheAgent(page, words);
    await planWaits(page);
    await expect(planRows(planOnPage(page))).toHaveCount(1);
    await expect(planOnPage(page)).toContainText("Raise the great dome");
    await expect(planRows(planCard(page))).toHaveCount(1);
    // Nothing routes, claims or follows the dome here, so the row says the message reaches no
    // session, in the form every message row has.
    await expect(planRows(planCard(page))).toContainText(
      `Message to no session on ${split.second}`
    );
  });
});

// SLOW(>=2s): a waiting plan, the bar closed, and its conversation opened again.
test("a plan waiting in the Inbox draws on no page until its conversation is opened", async ({
  browser,
}) => {
  modelsLane();
  test.setTimeout(90_000);
  const split = await seedSplit();
  const words = `split ${split.parent} and close the bar`;
  await scriptSplit(words, split);
  await withAlice(browser, async (page) => {
    await openBarOver(page, "/projects/CORE/issues");
    await askTheAgent(page, words);
    await planWaits(page);
    await expect(planOnPage(page)).toBeVisible();
    await sidePanel(page).getByTestId("going-close").click();
    await expect(sidePanel(page)).toHaveCount(0);
    await expect(planOnPage(page)).toHaveCount(0);
    await page.getByTestId("going-chip-open").click();
    await expect(planOnPage(page)).toBeVisible({ timeout: RUN_STEPS_MS });
    await expect(bar(page)).toBeVisible();
  });
});
