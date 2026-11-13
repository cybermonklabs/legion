/** A plan and its review as the issue page draws them: each plan task in the Plan filter's
 *  outline under the requirement it serves, with the task it needs, and the plan review's
 *  approval on the plan's own page. What the page renders from one fixed answer, so it runs
 *  against a recording (packages/dispatch/AGENTS.md, "The stubbed layer").
 */
import { expect } from "@playwright/test";

import { createIssue, createIssueArtifact, createPlanReview, createProject } from "./api";
import { nodeRow, parentOfRow, unfoldEveryBranch } from "./issue-page";
import { stubbed } from "./stub/stub-test";

/** The spec the plan's tasks serve. A `serves` pointer is refused unless it names a block that
 *  exists, so the requirements are written with the issue, before the plan points at one. */
const spec = `:::requirement{#r1}
Every issue page shows a Plan tab.
:::

:::requirement{#r2}
The Plan tab shows the plan review's approval.
:::
`;

const test = stubbed(async () => {
  await createProject({ key: "PLN", name: "Planning" });
  const issue = await createIssue({ project: "PLN", spec, title: "Plan tab" });
  const plan = await createIssueArtifact(issue.key, {
    // A `needs` pointer names a task of the plan's own document, so both tasks are written in
    // the one upload that makes the plan.
    content: `How ${issue.key} is built.

:::plan_task{#t0 serves="dispatch://${issue.key}/spec#b-r1"}
Add the routes.
:::

:::plan_task{#t1 serves="dispatch://${issue.key}/spec#b-r2" needs="dispatch://${issue.key}/artifact/plan-md#b-t0"}
Draw the panel.
:::
`,
    name: "plan.md",
  });
  expect(plan.artifact.slug).toBe("plan-md");
  await createPlanReview(plan.artifact.id, 1);
  return { issue: issue.key };
});

// The row reads what the Plan filter and the plan's own page draw from the plan the server
// already stored, so it runs against a recording rather than against a harness
// (packages/dispatch/AGENTS.md, "The stubbed layer"). The Plan tab's own list of tasks is gone
// (dispatch://INFRA-1196 R3, the Design's "Where each thing goes": "Plan tab - The Plan
// filter"): a task is a node of the outline, drawn under the requirement its `serves` names
// (R4), carrying a chip for each task it `needs`, and the approval is the plan's own.
// SLOW(>=2s): 4.0 s on replay - two loads, the Plan filter's outline with every branch opened and
// the plan's own page with its approval.
test("the Plan filter draws each plan task under the requirement it serves with the task it needs, and the plan's page its review's approval @pr", async ({
  page,
  scene,
}) => {
  await page.goto(`/issues/${scene.issue}/plan`);
  const routes = nodeRow(page, "plan-md#b-t0");
  const panel = nodeRow(page, "plan-md#b-t1");
  // The fold may leave a task under its requirement shut, so every branch is opened first.
  await expect(nodeRow(page, "spec#b-r1")).toBeVisible();
  await unfoldEveryBranch(page);
  // Each task by its own words, and never by its block's source (R21).
  await expect(routes).toContainText("Add the routes.");
  await expect(panel).toContainText("Draw the panel.");
  await expect(panel).not.toContainText(":::plan_task");

  // Each task sits under the requirement its `serves` names.
  expect(await parentOfRow(page, "plan-md#b-t0")).toBe("spec#b-r1");
  expect(await parentOfRow(page, "plan-md#b-t1")).toBe("spec#b-r2");
  await expect(nodeRow(page, "spec#b-r1")).toContainText("Every issue page shows a Plan tab.");
  await expect(nodeRow(page, "spec#b-r2")).toContainText(
    "The Plan tab shows the plan review's approval."
  );

  // The task it needs is a chip on its row, a link to that task named by its words; a task that
  // needs nothing carries none.
  const needs = panel.locator("[data-node-chip]").getByRole("link");
  await expect(needs).toHaveAttribute("href", `/issues/${scene.issue}/artifacts/plan-md#b-t0`);
  await expect(needs).toHaveText("Add the routes.");
  await expect(routes.locator("[data-node-chip]")).toHaveCount(0);

  // A plan is approved by its plan review, never by a person, and the plan's own page says so.
  await page.goto(`/issues/${scene.issue}/artifacts/plan-md`);
  await expect(page.getByTestId("artifact-header")).toContainText("Approved by plan review v1");
});
