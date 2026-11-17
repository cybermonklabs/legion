import { createIssue, createProject, putStep } from "./api";
import { expect, test } from "./test";

// An issue's Steps tab reads the step records Legion's daemon wrote of it (dispatch://INFRA-3570,
// R18): each run of each step of each task, with the attempt the server counts, and a run that has
// not ended read as running. The records go through the route the daemon writes with, as the
// session `legion-daemon:LEG`, and a review's end arrives before its start, as an outbox row
// that retried can deliver it.

const at = (time: string) => `2026-11-16T${time}:00Z`;

test("the Steps tab lists each run of an issue's steps, a running run as running", async ({
  page,
}) => {
  await createProject({ key: "LEG", name: "Legion" });
  const issue = await createIssue({ project: "LEG", title: "Records its steps" });
  const never = await createIssue({ project: "LEG", title: "Never ran in Legion" });
  const identity = (role: string, step: number, type: string) => ({
    agent: `legion-leg-${issue.key.toLowerCase()}-${role}`,
    generation: 1,
    kind: "step" as const,
    role,
    step,
    task: "default",
    type,
  });
  const implementer = identity("implementer", 2, "implementation");
  const reviewer = identity("reviewer", 4, "review");

  await putStep(issue.key, crypto.randomUUID(), {
    ...implementer,
    ended_at: at("09:40"),
    model: "anthropic/claude-opus-5",
    result: "passed",
    started_at: at("09:00"),
  });
  const review = crypto.randomUUID();
  await putStep(issue.key, review, {
    ...reviewer,
    ended_at: at("10:30"),
    model: "openai/gpt-6.1-sol",
    result: "failed",
    returned_to: 2,
    started_at: at("10:00"),
  });
  // The start of the same run again, as the daemon writes it: the record keeps what it holds.
  await putStep(issue.key, review, { ...reviewer, started_at: at("10:01") });
  await putStep(issue.key, crypto.randomUUID(), { ...implementer, started_at: at("10:31") });

  await page.goto(`/issues/${issue.key}/steps`);
  await expect(page.getByRole("tab", { name: "Steps" })).toHaveAttribute("aria-selected", "true");
  const table = page.getByRole("table", { name: "Steps of task default" });
  const rows = table.locator("tbody tr");
  await expect(rows).toHaveCount(3);
  await expect(rows.nth(0)).toContainText("implementer");
  await expect(rows.nth(0)).toContainText("anthropic/claude-opus-5");
  await expect(rows.nth(1).getByRole("cell").nth(8)).toHaveText("failed, returned to step 2");
  await expect(rows.nth(1).locator("time").first()).toHaveAttribute("datetime", at("10:00"));
  // The second run of the implementation step: its attempt is the server's count, and it runs.
  await expect(rows.nth(2).getByRole("cell").nth(5)).toHaveText("2");
  await expect(rows.nth(2).getByRole("cell").nth(8)).toHaveText("running");

  await page.goto(`/issues/${never.key}/steps`);
  await expect(page.getByRole("region", { name: "Steps empty state" })).toHaveText(
    "No step of this issue has run in Legion."
  );
});
