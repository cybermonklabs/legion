import { createIssue, createProject, getGoingCapability, setNewIssueSettings } from "./api";
import { answerText, bar, lineOnTop, openBarOver, searchBox } from "./going-locators";
import { countAskedRuns } from "./going-panel";
import { usesFakeModels } from "./harness-models";
import { expect, test } from "./test";
import { asUser } from "./users";

// Check c14 of dispatch://INFRA-1588/artifact/verification-md@v16, serving what spec v61's Design
// keeps from #3924 (the capability route, GET /api/v1/going/capability): a Dispatch with
// nothing configured opens the bar and says in one sentence that it cannot answer (design v3,
// state 13). The default stack starts the server with no answering model, no deciding model, no
// session and no label; the bar opens, the hits appear, the one sentence shows where the line on
// top would be, nothing else about the deployment's settings shows, it asks no answer, and the
// capability route reports each of the four unset. The models lane configures both models, so
// this row skips there, unless the run is the check's negative control, which turns the skip off
// to run the same row in the models lane (`DISPATCH_E2E_MODELS=fake
// DISPATCH_E2E_CONTROL=unconfigured-in-models-lane bunx playwright test --config
// e2e/playwright.config.ts --project=chromium going-unconfigured`): there the bar answers, and the
// row fails at its first assertion of what the bar cannot do, `going-cannot-answer`.

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
test("with nothing configured the bar opens, lists the hits, says in one sentence it cannot answer and asks none", async ({
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
    // Said once, in the line on top's place, as no answer comes to carry it: one sentence, and
    // no note of the deciding model, the session or the label (design v3, rule 8).
    await expect(bar(page).getByTestId("going-cannot-answer")).toHaveText(
      "Answers are off on this Dispatch: it has no answering model."
    );
    await expect(bar(page).getByTestId("going-cannot-answer")).toHaveCount(1);
    await expect(lineOnTop(page)).toHaveCount(0);
    for (const note of [
      "going-capability",
      "going-cannot-decide",
      "going-no-session",
      "going-no-label",
    ]) {
      await expect(bar(page).getByTestId(note)).toHaveCount(0);
    }

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
