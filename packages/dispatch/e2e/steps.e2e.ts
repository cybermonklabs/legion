import { createIssue, createProject, putStep } from "./api";
import { expect, rowKey, test } from "./test";

// An issue's Steps tab reads the step records Legion's daemon wrote of it: each run of each step
// of each task, with the attempt the server counts, and a run that has not ended read as running.
// The records go through the route the daemon writes with, as the session `legion-daemon:LEG`.
// The review's run is written whole, its start and its end in one write, and then again with
// only a start at another moment, which the record does not take: the run keeps its first start
// and its failed end.

const at = (time: string) => `2026-11-16T${time}:00Z`;

test("the Steps tab lists each run of an issue's steps, a running run as running", async ({
  page,
}) => {
  const leg = rowKey("LEG");
  await createProject({ key: leg, name: "Legion" });
  const issue = await createIssue({ project: leg, title: "Records its steps" });
  const never = await createIssue({ project: leg, title: "Never ran in Legion" });
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
  // A second start of the same run at another moment, which no daemon write carries (its start
  // and its end write one moment, and its only second start is a retry of the same body): the
  // record keeps the start it holds.
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
