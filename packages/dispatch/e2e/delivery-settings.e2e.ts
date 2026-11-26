/** saves per-project delivery settings against the fake GitHub App's repository access check and
 *  re-reads the stored settings, resolved default repositories and eval targets across tabs and
 *  reloads; turns delivery off and polls the server's recomputed deployment-wide pipeline count;
 *  and saves under the stack's agent bearer with no sign-in and asserts the stored writer session
 *  (dispatch://INFRA-978, R1, R2, R4 and k5). */

// acme-shaped placeholders throughout (AGENTS.md: "This repository is public").
import type { Page } from "@playwright/test";

import {
  baseUrl,
  e2eAgentToken,
  getProjectDeliverySettings,
  putProjectDeliverySettings,
} from "./api";
import { PIPELINE_AT, seedPipelineFixture } from "./delivery-seed";
import { deploySource, projectWithDefault, saveDeliverySettings } from "./delivery-settings-steps";
import { seedFakeGithub } from "./fake-github-helpers";
import { expect, test } from "./test";
import { asUser } from "./users";

/** The fake GitHub App installed on each of `repos` with Contents: read, which the settings save
 *  proves for each source's repository. */
async function appReads(...repos: string[]): Promise<void> {
  await seedFakeGithub(
    Object.fromEntries(
      repos.map((repo, index) => [repo, { contents: "read", installation_id: 970 + index }])
    )
  );
}

/** Fills deploy source `n`'s rule: its workflow, its production jobs and its PR checks workflow. */
async function fillRule(
  page: Page,
  n: number,
  rule: { workflow: string; jobs: string; checks: string }
): Promise<void> {
  const source = deploySource(page, n);
  await source.getByLabel("Deploy workflow", { exact: true }).fill(rule.workflow);
  await source.getByLabel("Production jobs", { exact: true }).fill(rule.jobs);
  await source.getByLabel("PR checks workflow", { exact: true }).fill(rule.checks);
}

test("two projects keep the settings saved on their own tabs, one with two deploy sources", async ({
  browser,
}) => {
  await appReads("acme/widgets", "acme/deploys", "acme/gadgets");
  await projectWithDefault("ACME", "Acme", "acme/widgets");
  await projectWithDefault("GADG", "Gadgets", "acme/gadgets");
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    // ACME: one source reading its default repository, one naming acme/deploys.
    await page.goto("/projects/ACME/settings");
    await expect(page.getByRole("tab", { name: "Settings" })).toHaveAttribute(
      "aria-selected",
      "true"
    );
    await page.getByRole("switch", { name: "Delivery" }).check();
    await fillRule(page, 1, {
      checks: ".github/workflows/pr-checks.yml",
      jobs: "widgets-release / widgets-release",
      workflow: ".github/workflows/deploy.yml",
    });
    await page.getByRole("button", { name: "Add a GitHub workflow source" }).click();
    await deploySource(page, 2).getByLabel("Repository", { exact: true }).fill("acme/deploys");
    await fillRule(page, 2, {
      checks: ".github/workflows/release-checks.yml",
      jobs: "release / apply",
      workflow: ".github/workflows/release.yml",
    });
    await page.getByLabel("Population authors", { exact: true }).fill("octocat");
    await page.getByLabel("Deploy frequency (deploys a day)", { exact: true }).fill("8");
    await page.getByLabel("Failed deployment recovery time (minutes)", { exact: true }).fill("120");
    await saveDeliverySettings(page, "ACME");

    // GADG: one source reading its own default repository, a failure source, other targets.
    await page.goto("/projects/GADG/settings");
    await page.getByRole("switch", { name: "Delivery" }).check();
    await fillRule(page, 1, {
      checks: ".github/workflows/checks.yml",
      jobs: "ship",
      workflow: ".github/workflows/ship.yml",
    });
    await page.getByLabel("A merged revert", { exact: true }).check();
    await page.getByLabel("Failures counted from", { exact: true }).fill("2026-11-01");
    await page.getByLabel("Deploy frequency (deploys a day)", { exact: true }).fill("3");
    await page.getByLabel("Change failure rate (a share from 0 to 1)", { exact: true }).fill("off");
    await saveDeliverySettings(page, "GADG");

    // Each project reads back its own settings through the route, the targets from its evals.
    const acme = await getProjectDeliverySettings("ACME");
    expect(acme?.delivery).toBe(true);
    expect(
      acme?.sources?.map(({ deploy_workflow_path, repo, resolved_repo }) => ({
        deploy_workflow_path,
        repo,
        resolved_repo,
      }))
    ).toEqual([
      {
        deploy_workflow_path: ".github/workflows/deploy.yml",
        repo: null,
        resolved_repo: "acme/widgets",
      },
      {
        deploy_workflow_path: ".github/workflows/release.yml",
        repo: "acme/deploys",
        resolved_repo: "acme/deploys",
      },
    ]);
    expect(acme?.targets?.["deploy-frequency"]).toBe(8);
    expect(acme?.targets?.["failed-deploy-recovery"]).toBe(120);
    const gadgets = await getProjectDeliverySettings("GADG");
    expect(gadgets?.sources?.map(({ repo, resolved_repo }) => [repo, resolved_repo])).toEqual([
      [null, "acme/gadgets"],
    ]);
    expect(gadgets?.failure_sources).toEqual(["reverts"]);
    expect(gadgets?.failures_measured_since).toBe("2026-11-01");
    expect(gadgets?.targets?.["deploy-frequency"]).toBe(3);
    expect(gadgets?.targets?.["change-fail-rate"]).toBe("off");

    // And each tab, opened again, shows its own.
    await page.goto("/projects/ACME/settings");
    await expect(deploySource(page, 2).getByLabel("Repository", { exact: true })).toHaveValue(
      "acme/deploys"
    );
    await expect(deploySource(page, 1).getByLabel("Repository", { exact: true })).toHaveValue("");
    await expect(page.getByLabel("Deploy frequency (deploys a day)", { exact: true })).toHaveValue(
      "8"
    );
    await page.goto("/projects/GADG/settings");
    await expect(page.getByRole("group", { name: "Deploy source 2" })).toHaveCount(0);
    await expect(page.getByLabel("A merged revert", { exact: true })).toBeChecked();
    await expect(
      page.getByLabel("Change failure rate (a share from 0 to 1)", { exact: true })
    ).toHaveValue("off");
  } finally {
    await context.close();
  }
});

test("a project turned off shows the switch alone, and the pipeline counts none of its runs", async ({
  browser,
}) => {
  // ACME reads acme/widgets, whose Pipeline fixture runs it holds; GADG stays on with a
  // repository that has no run, so the pipeline still answers once ACME is off.
  await appReads("acme/gadgets");
  await seedPipelineFixture();
  await projectWithDefault("GADG", "Gadgets", "acme/gadgets");
  await putProjectDeliverySettings("GADG", {
    delivery: true,
    sources: [
      {
        deploy_workflow_path: ".github/workflows/ship.yml",
        kind: "github_workflow",
        pr_checks_workflow_path: ".github/workflows/checks.yml",
        production_job_names: ["ship"],
        repo: null,
      },
    ],
  });
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const productionRuns = async (): Promise<number> => {
    const response = await page.request.get(`/api/v1/delivery/pipeline?at=${PIPELINE_AT}`, {
      headers: { Authorization: `Bearer ${e2eAgentToken}` },
    });
    expect(response.status()).toBe(200);
    const body = (await response.json()) as { production_apply: { total: { n: number } } };
    return body.production_apply.total.n;
  };

  try {
    await expect.poll(productionRuns).toBeGreaterThan(0);

    await page.goto("/projects/ACME/settings");
    const delivery = page.getByRole("switch", { name: "Delivery" });
    await expect(delivery).toBeChecked();
    await delivery.uncheck();
    await expect(page.getByRole("group", { name: "Deploy sources" })).toHaveCount(0);
    await saveDeliverySettings(page, "ACME");

    // The tab shows the switch alone, the stored source kept behind it.
    await page.reload();
    await expect(delivery).not.toBeChecked();
    await expect(page.getByLabel("Default repository", { exact: true })).toHaveValue(
      "acme/widgets"
    );
    await expect(page.getByRole("group", { name: "Deploy sources" })).toHaveCount(0);
    await expect(page.getByRole("group", { name: "Targets" })).toHaveCount(0);
    const acme = await getProjectDeliverySettings("ACME");
    expect(acme?.delivery).toBe(false);
    expect(acme?.sources).toHaveLength(1);

    // The deployment-wide pipeline reads GADG's repository alone now: none of ACME's runs.
    await expect.poll(productionRuns).toBe(0);
  } finally {
    await context.close();
  }
});

/** The session the agent's browser names as its writer: the one the harness's own agent writes
 *  name (`seedActor` in e2e/api.ts), since the stack's shared token proves no session of its own. */
const AGENT_SESSION = "e2e-seed";

// The spec's check k5 on the local stack: the editor driven by an agent, with the agent bearer on
// every request to the stack's origin and no sign-in. GET /auth/whoami answers the bearer as an
// agent (`authWhoami` in internal/dispatch/routes/router.go), and the page opens on `?session=`,
// the session its save names as its writer (`actor`), which a bearer whose credential proves none
// must name (`bearerSessionActor` in internal/dispatch/api/server.go).
test("the settings tab saves under the stack's agent bearer with no sign-in", async ({
  browser,
  baseURL,
}) => {
  await appReads("acme/widgets");
  await projectWithDefault("ACME", "Acme", "acme/widgets");
  // No sign-in: a context of its own, with no cookie. Every request to the stack's origin, and
  // to no other host, carries the agent bearer, as a headless browser driven by an agent does.
  const context = await browser.newContext();
  const page = await context.newPage();
  const origin = new URL(baseURL ?? baseUrl).origin;
  await page.route(`${origin}/**`, (route) =>
    route.continue({
      headers: { ...route.request().headers(), authorization: `Bearer ${e2eAgentToken}` },
    })
  );

  try {
    await page.goto(`/projects/ACME/settings?session=${AGENT_SESSION}`);
    await page.getByRole("switch", { name: "Delivery" }).check();
    await fillRule(page, 1, {
      checks: ".github/workflows/pr-checks.yml",
      jobs: "widgets-release / widgets-release",
      workflow: ".github/workflows/deploy.yml",
    });
    await page.getByLabel("Deploy frequency (deploys a day)", { exact: true }).fill("5");
    await saveDeliverySettings(page, "ACME");

    // Read back through the API: the settings as saved, written by the session the page named.
    const acme = await getProjectDeliverySettings("ACME");
    expect(acme?.delivery).toBe(true);
    expect(acme?.sources?.map(({ resolved_repo }) => resolved_repo)).toEqual(["acme/widgets"]);
    expect(acme?.targets?.["deploy-frequency"]).toBe(5);
    expect(acme?.updated_by).toMatchObject({ id: AGENT_SESSION, kind: "session" });

    await page.reload();
    await expect(page.getByRole("switch", { name: "Delivery" })).toBeChecked();
    await expect(deploySource(page, 1).getByLabel("Deploy workflow", { exact: true })).toHaveValue(
      ".github/workflows/deploy.yml"
    );
  } finally {
    await context.close();
  }
});
