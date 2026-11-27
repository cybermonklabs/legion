// The Pipeline page's rows, moved to the CI health section of the Evals tab, which replaced the
// page (dispatch://INFRA-2918, R1, R15): the four CI evals' timing charts and pr-ci's two job
// lists (`web/src/features/evals/CiHealth.tsx`), over the tab's window, here the seven UTC days
// ending on the fixture's pinned day (`PIPELINE_AT`), and the two lead-time cards the page showed
// beside them, now target cards (`TargetCards.tsx`).
import { createProject, mintAgentToken, registerEval } from "./api";
import { PIPELINE_AT, seedPipelineFixture } from "./delivery-seed";
import {
  DELIVERY_PROJECT,
  evalsSection,
  evalsTabPath,
  expectEvalsTab,
  paintedSection,
} from "./evals-tab";
import { sql } from "./psql";
import { expect, type Locator, type Page, test } from "./test";
import { asUser } from "./users";

/** The tab's custom window of the seven UTC days ending on `PIPELINE_AT`'s day: 2026-09-26 to
 *  2026-10-02, the days the Pipeline page showed at `?at=PIPELINE_AT`. */
function pipelineWindow(): string {
  const day = new Date(PIPELINE_AT);
  const to = new Date(Date.UTC(day.getUTCFullYear(), day.getUTCMonth(), day.getUTCDate() + 1));
  const from = new Date(to.getTime() - 7 * 24 * 60 * 60 * 1000);
  return `from=${encodeURIComponent(from.toISOString())}&to=${encodeURIComponent(to.toISOString())}`;
}

/** Opens `project`'s Evals tab on the Pipeline fixture's seven days, once its CI health section
 *  has painted. */
async function openCiHealth(page: Page, project = DELIVERY_PROJECT): Promise<Locator> {
  await page.goto(evalsTabPath(pipelineWindow(), project));
  await expectEvalsTab(page);
  return paintedSection(page, "ci");
}

/** One CI eval's figure over the window (`EvalChart.tsx`'s `data-window-statistic`) on its
 *  timing chart (`data-eval`, the eval's slug). */
function ciFigure(ci: Locator, slug: string, statistic: string): Locator {
  return ci.locator(
    `[data-eval="${slug}"] [data-window-series="minutes"] [data-window-statistic="${statistic}"]`
  );
}

/** One of pr-ci's two job lists (`data-job-list`: `slowest-jobs` or `failing-jobs`), once its
 *  items view has answered. */
async function jobList(ci: Locator, id: string): Promise<Locator> {
  const list = ci.locator(`[data-job-list="${id}"]`);
  await expect(list).toBeVisible();
  await expect(list.getByRole("status")).toHaveCount(0);
  return list;
}

/** The row of a job list's table that names `job`. */
function jobRow(list: Locator, job: string): Locator {
  return list.locator("tbody tr").filter({ hasText: job });
}

test("the Pipeline address opens the default project's Evals tab", async ({ browser }) => {
  await seedPipelineFixture();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  // The old page's address keeps working: it opens the tab of the one project with delivery on.
  await page.goto("/pipeline");
  await expect(page).toHaveURL(new RegExp(`/projects/${DELIVERY_PROJECT}/evals$`));
  await expectEvalsTab(page);
  await expect(evalsSection(page, "ci")).toBeVisible();

  await context.close();
});

// @pr: Live delivery: the Pipeline page's stage timings and failing jobs, which no other row on the lane reads.
test("the Pipeline page shows each stage's timing and the failing jobs @pr", async ({
  browser,
}, testInfo) => {
  await seedPipelineFixture();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const ci = await openCiHealth(page);

  // The window's two timed staging end-to-end results, 40 and 70 minutes, one of them a failure;
  // the cancelled one and the workflow_dispatch run's are not timed.
  await expect(ciFigure(ci, "staging-e2e", "n")).toHaveText(/^2(?!\d)/);
  await expect(ciFigure(ci, "staging-e2e", "median")).toContainText("55");
  await expect(ciFigure(ci, "staging-e2e", "success_rate")).toContainText("50");
  // The three staging deploys that finished; the one still running is not timed.
  await expect(ciFigure(ci, "staging-deploy", "n")).toHaveText(/^3(?!\d)/);
  // The feature/x run's successful production job is not counted, so the week has one result.
  await expect(ciFigure(ci, "production-deploy", "n")).toHaveText(/^1(?!\d)/);
  await expect(ciFigure(ci, "production-deploy", "success_rate")).toContainText("100");

  // The two lead-time cards: merge to production misses its 45 minutes, open to merge meets 60.
  const cards = await paintedSection(page, "cards");
  await expect(
    cards.locator('[data-card="merge-to-production"] [data-target-met]')
  ).toHaveAttribute("data-target-met", "false");
  await expect(cards.locator('[data-card="pr-open-to-merge"] [data-target-met]')).toHaveAttribute(
    "data-target-met",
    "true"
  );

  const slowest = await jobList(ci, "slowest-jobs");
  await expect(jobRow(slowest, "build / unit")).toHaveCount(1);
  // The 24 hours before the window ends are 2026-10-02 (UTC): build / unit failed in its two
  // runs there, and the cancelled run's job is not counted.
  const failing = await jobList(ci, "failing-jobs");
  const unit = jobRow(failing, "build / unit");
  await expect(unit.getByRole("cell", { exact: true, name: "2" })).toHaveCount(2);
  await expect(unit.getByRole("link")).toHaveAttribute(
    "href",
    "https://github.com/acme/widgets/actions/runs/2"
  );
  // The roll-up job is left out; a job that only failed before the 24 hours is not listed.
  await expect(jobRow(failing, "checks-complete")).toHaveCount(0);
  await expect(jobRow(failing, "typecheck")).toHaveCount(0);

  await page.screenshot({ path: testInfo.outputPath("pipeline.png"), fullPage: true });
  await context.close();
});

// The narrow-screen half of the row above: the timing charts stack in one column on a phone,
// where the section is narrower than the 42rem the second column needs.
test("the Pipeline page stacks its cards in one column on a phone @phone @phone-only", async ({
  browser,
}, testInfo) => {
  await seedPipelineFixture();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const ci = await openCiHealth(page);

  const first = ci.locator('[data-eval="pr-ci"]');
  await expect(first).toBeVisible();
  const second = ci.locator('[data-eval="staging-e2e"]');
  await expect(second).toBeVisible();
  // The waits above drew both charts, and a drawn element has a box, so each has one.
  const firstBox = await first.boundingBox();
  // The second chart was waited for above too, so its box is there as the first one's is.
  const secondBox = await second.boundingBox();
  if (firstBox === null || secondBox === null) throw new Error("the timing charts are hidden");
  expect(secondBox.y).toBeGreaterThanOrEqual(firstBox.y + firstBox.height);
  expect(Math.abs(secondBox.x - firstBox.x)).toBeLessThan(2);

  await page.screenshot({ path: testInfo.outputPath("pipeline-phone.png"), fullPage: true });
  await context.close();
});

// The slowest-jobs list ranks a job once it holds the min_n a person set on the project's pr-ci
// eval, and its sentence names that number (INFRA-2916, spec R5; dispatch://INFRA-978, D9).
// With no min_n set the threshold is 50 and build / unit, the fixture's one job with 50 timed
// results or more, is ranked alone; at min_n 1, lint, with three timed results, is ranked too.
test("the Pipeline's slowest jobs follow the min_n a person set on pr-ci", async ({ browser }) => {
  // Made through the API, so its evals are seeded, before the fixture's settings name it.
  await createProject({ key: "PIPE", name: "Pipeline" });
  await seedPipelineFixture("PIPE");
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  let slowest = await jobList(await openCiHealth(page, "PIPE"), "slowest-jobs");
  await expect(slowest).toContainText("a job needs 50 timed results in the window to be ranked");
  await expect(jobRow(slowest, "build / unit")).toHaveCount(1);
  await expect(jobRow(slowest, "lint")).toHaveCount(0);

  await registerEval("PIPE", { series: [{ min_n: 1, series: "job:*" }], slug: "pr-ci" });
  slowest = await jobList(await openCiHealth(page, "PIPE"), "slowest-jobs");
  await expect(slowest).toContainText("a job needs 1 timed result in the window to be ranked");
  await expect(jobRow(slowest, "build / unit")).toHaveCount(1);
  await expect(jobRow(slowest, "lint")).toHaveCount(1);
  await context.close();
});

test("a pipeline with no staging end-to-end gate draws no timing for it", async ({ browser }) => {
  await seedPipelineFixture();
  await sql("UPDATE project_deploy_sources SET staging_e2e_job_name = null");
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const ci = await openCiHealth(page);

  await expect(ciFigure(ci, "pr-ci", "n")).toBeVisible();
  await expect(ci.locator('[data-eval="staging-e2e"] [data-window-statistic]')).toHaveCount(0);
  await expect(ciFigure(ci, "staging-deploy", "n")).toBeVisible();
  await context.close();
});

test("the pipeline API gives an agent the same numbers", async ({ browser }) => {
  await seedPipelineFixture();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const token = await mintAgentToken("pipeline-e2e", "alice");
  const response = await page.request.get(`/api/v1/delivery/pipeline?at=${PIPELINE_AT}`, {
    headers: { Authorization: `Bearer ${token}` },
  });
  expect(response.status()).toBe(200);
  const body = (await response.json()) as {
    days: string[];
    staging_e2e: { days: { n: number; median_minutes: number | null }[] } | null;
    pr_checks: { failing_jobs: { name: string; failures: number; results: number }[] };
    production_apply: { total: { n: number } };
  };
  expect(body.days.at(-1)).toBe("2026-10-02");
  expect(body.staging_e2e?.days[6]?.n).toBe(2);
  expect(body.staging_e2e?.days[6]?.median_minutes).toBe(55);
  expect(body.pr_checks.failing_jobs[0]).toEqual(
    expect.objectContaining({ name: "build / unit", failures: 2, results: 3 })
  );
  expect(body.production_apply.total.n).toBe(1);
  await context.close();
});
