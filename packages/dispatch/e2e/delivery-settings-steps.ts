// The steps every row that drives a project's delivery settings tab takes, in one place, so the
// Delivery page's rows and the settings tab's own rows press the same button and wait for the same
// answer.
import type { Page } from "@playwright/test";

import { createProject, putProjectDefaultRepository } from "./api";
import { expect } from "./test";

/** A project made through the API, so its evals are seeded, with `repo` as its default
 *  repository. */
export async function projectWithDefault(key: string, name: string, repo: string): Promise<void> {
  await createProject({ key, name });
  await putProjectDefaultRepository(key, repo);
}

/** Deploy source `n` (from 1) of the settings tab `page` shows. */
export function deploySource(page: Page, n: number) {
  return page.getByRole("group", { name: `Deploy source ${n}` });
}

/** Fills deploy source `n`'s rule on the settings tab `page` shows: its workflow, its production
 *  jobs and its PR checks workflow. */
export async function fillRule(
  page: Page,
  n: number,
  rule: { workflow: string; jobs: string; checks: string }
): Promise<void> {
  const source = deploySource(page, n);
  await source.getByLabel("Deploy workflow", { exact: true }).fill(rule.workflow);
  await source.getByLabel("Production jobs", { exact: true }).fill(rule.jobs);
  await source.getByLabel("PR checks workflow", { exact: true }).fill(rule.checks);
}

/** Presses Save on `project`'s settings tab and waits for the server's 200 and the line that
 *  says so. */
export async function saveDeliverySettings(page: Page, project: string): Promise<void> {
  const saved = page.waitForResponse(
    (response) =>
      response.request().method() === "PUT" &&
      response.url().endsWith(`/api/v1/projects/${project}/settings/delivery`) &&
      response.ok()
  );
  await page.getByRole("button", { name: "Save delivery settings" }).click();
  await saved;
  await expect(
    page.getByRole("status").filter({ hasText: "Delivery settings saved" })
  ).toBeVisible();
}
