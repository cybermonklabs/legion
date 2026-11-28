import { createIssue, createProject, putStep } from "./api";
import { expect, test } from "./test";

// An issue's Steps tab reads the step records Legion's daemon wrote of it: each run of each step
// of each task, with the attempt the server counts, and a run that has not ended read as running.
// The records go through the route the daemon writes with, as the session `legion-daemon:LEG`.
// The review's run is written whole, its start and its end in one write, with the model its role
// was set to and the two models that answered it. It is then written again with only a start at
// another moment, which the record does not take: the run keeps its first start, its failed end
// and its models. The implementer's runs name no answers, so the tab does not know their counts.

const at = (time: string) => `2026-11-16T${time}:00Z`;

// SLOW(>=2s): four writes of three runs through the daemon's route, two issues' tabs read, and one
// screenshot of the table scrolled to its last columns, which the review of the tab reads.
test("the Steps tab lists each run of an issue's steps, a running run as running", async ({
  page,
}, testInfo) => {
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
    answers: [
      { calls: 41, model: "openai/gpt-6.1-sol" },
      { calls: 3, model: "anthropic/claude-opus-5" },
    ],
    ended_at: at("10:30"),
    model: "openai/gpt-6.1-sol",
    result: "failed",
    returned_to: 2,
    set_model: "openai/gpt-6.1-sol",
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
  // Set model and Answered by follow Result. The review names both, the implementer's ended run
  // names neither, and the run that has not ended has no answers yet.
  await expect(rows.nth(1).getByRole("cell").nth(9)).toHaveText("openai/gpt-6.1-sol");
  await expect(rows.nth(1).getByRole("cell").nth(10)).toHaveText(
    "openai/gpt-6.1-sol=41(93%),anthropic/claude-opus-5=3(7%)"
  );
  await expect(rows.nth(0).getByRole("cell").nth(9)).toHaveText("—");
  await expect(rows.nth(0).getByRole("cell").nth(10)).toHaveText("unknown");
  await expect(rows.nth(2).getByRole("cell").nth(10)).toHaveText("running");
  // Beside the margin the table is wider than its card, which scrolls it. The shot is of the card
  // scrolled to its end, so it shows Result, Set model and Answered by whole.
  await table.evaluate((element) => {
    const box = element.parentElement;
    if (box !== null) box.scrollLeft = box.scrollWidth;
  });
  await expect(rows.nth(1).getByRole("cell").nth(10)).toBeInViewport({ ratio: 1 });
  await page.screenshot({
    fullPage: true,
    path: testInfo.outputPath("steps-tab-answered-by.png"),
  });

  await page.goto(`/issues/${never.key}/steps`);
  await expect(page.getByRole("region", { name: "Steps empty state" })).toHaveText(
    "No step of this issue has run in Legion."
  );
});
