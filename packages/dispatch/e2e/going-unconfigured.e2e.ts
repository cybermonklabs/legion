import { createIssue, createProject, getGoingCapability, setNewIssueSettings } from "./api";
import {
  answerPanel,
  answerText,
  bar,
  countAskedRuns,
  openBarOver,
  searchBox,
} from "./going-panel";
import { usesFakeModels } from "./harness-models";
import { expect, test } from "./test";
import { asUser } from "./users";

// Check c14 of dispatch://INFRA-1588/artifact/verification-md, serving R8: a Dispatch with
// nothing configured opens the bar and says what it cannot do. The default stack starts the
// server with no answering model, no deciding model, no session and no label; the bar opens, the
// hits appear, it says what it cannot do and asks no answer, and the capability route reports each
// of the four unset. The models lane configures both models, so this row skips there, unless the
// run is the check's negative control, which turns the skip off to run the same row in the models
// lane (`DISPATCH_E2E_MODELS=fake DISPATCH_E2E_CONTROL=unconfigured-in-models-lane bunx playwright
// test --config e2e/playwright.config.ts --project=chromium going-unconfigured`): there the bar
// answers, and the row fails at its first assertion of what the bar cannot do,
// `going-cannot-answer`.

/** Twice the bar's pause (`MEANING_PAUSE_MS`, 500 ms, `web/src/features/search/useTieredSearch.ts`,
 *  which imports the api client and so is no module a spec loads): a bar that answers has asked
 *  by then. */
const ABSENCE_WINDOW_MS = 1_000;

/** Whether this run is the negative control, which runs the row where the models are set. */
const control = process.env.DISPATCH_E2E_CONTROL === "unconfigured-in-models-lane";

test.skip(
  usesFakeModels && !control,
  "the models lane configures the models this row reads as unset"
);

// SLOW(>=2s): a seeded issue, the bar's two searches and its pause waited out.
test("with nothing configured the bar opens, lists the hits, says what it cannot do and asks no answer", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Quasar sightings log" });
  // The session and the label are the server's own while it runs: put them back as it started,
  // whatever a row before this one set them to through the test hook.
  await setNewIssueSettings({ label: "", session: "" });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await openBarOver(page, "/projects/CORE/issues");
    const asked = countAskedRuns(page);
    await searchBox(page).fill("quasar sightings");

    await expect(bar(page).locator(`[id^="search-option-issue-${issue.key}"]`)).toBeVisible();
    const panel = answerPanel(page);
    await expect(panel.getByTestId("going-cannot-answer")).toBeVisible();
    await expect(panel.getByTestId("going-cannot-decide")).toBeVisible();
    await expect(panel.getByTestId("going-no-session")).toBeVisible();
    await expect(panel.getByTestId("going-no-label")).toBeVisible();

    // An absence window: twice the bar's pause, by which a bar that answers has asked already.
    await page.waitForTimeout(ABSENCE_WINDOW_MS);
    expect(asked.words).toEqual([]);
    await expect(answerText(page)).toHaveCount(0);
  } finally {
    await context.close();
  }

  const capability = await getGoingCapability();
  expect(capability.answer.configured).toBe(false);
  expect(capability.decision.configured).toBe(false);
  expect(capability.session.configured).toBe(false);
  expect(capability.label.configured).toBe(false);
});
