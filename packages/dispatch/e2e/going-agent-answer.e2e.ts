import { createIssue, createProject, getIssueEvents, patchIssue } from "./api";
import { callTool, modelsLane, say, scriptChat } from "./fake-models-helpers";
import {
  askAndWait,
  askTheAgent,
  newestRun,
  openBarOver,
  prose,
  RUN_STEPS_MS,
} from "./going-panel";
import { expect, type Locator, test } from "./test";
import { withAlice } from "./users";

// Check c3 of dispatch://INFRA-1588/artifact/verification-md (spec R3): a pause streams a short
// answer and the store records no write; Enter starts a run, each tool call shows as a step while
// it runs, and the answer links each item the fake read; and each number of a chart is the
// server's, from the chain the fake asks for over its read, never the number the fake writes in its
// text. Negative control: a bar that draws the numbers the model wrote shows the wrong number.

/** The value the chart draws for the point of `status`, whether the chart writes it as stored
 *  (`in_progress`) or in words (`In progress`). */
function chartValue(chart: Locator, status: string): Locator {
  return chart
    .getByTestId("going-block-chart-point")
    .filter({ hasText: new RegExp(status.replaceAll("_", "[_ ]"), "i") })
    .locator("td")
    .nth(1);
}

// SLOW(>=2s): a seeded issue and the pause's short answer.
test("a pause streams a short answer, and the store records no write", async ({ browser }) => {
  modelsLane();
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Map the lunar craters" });
  const before = (await getIssueEvents(issue.key)).length;
  await scriptChat({ default: prose(`The craters are mapped in ${issue.key}.`) });
  await withAlice(browser, async (page) => {
    await openBarOver(page, "/projects/CORE/issues");
    await askAndWait(page, "where are the lunar craters mapped");
    expect((await getIssueEvents(issue.key)).length).toBe(before);
  });
});

// SLOW(>=2s): a run of two model calls and a read.
test("Enter starts a run: each tool call shows as a step while it runs, and the answer links each item it read", async ({
  browser,
}) => {
  modelsLane();
  test.setTimeout(90_000);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Map the lunar craters" });
  const words = "summarise the crater mapping";
  await scriptChat({
    default: prose("A short answer while the person pauses."),
    runs: [
      {
        turns: [
          // The read streams slowly, so its step shows working before its frame ends it.
          { ...callTool("dispatch_read", { issue: issue.key }), delayMs: 200 },
          say(`The mapping is in [${issue.key}](/issues/${issue.key}).`),
        ],
        words,
      },
    ],
  });
  await withAlice(browser, async (page) => {
    await openBarOver(page, "/projects/CORE/issues");
    // Every step the page draws as running is recorded from here on, however short its while.
    await page.evaluate(() => {
      const running: string[] = [];
      Reflect.set(window, "__goingRunningSteps", running);
      new MutationObserver(() => {
        for (const step of document.querySelectorAll(
          '[data-testid="going-step"][data-running="true"]'
        )) {
          running.push(step.getAttribute("data-tool") ?? "");
        }
      }).observe(document.body, { attributes: true, childList: true, subtree: true });
    });
    await askTheAgent(page, words);
    const run = newestRun(page);
    await expect(run.getByTestId("going-step").first()).toHaveAttribute("data-tool", "call_tool", {
      timeout: RUN_STEPS_MS,
    });
    await expect(run.getByTestId("going-step").first()).toHaveAttribute("data-running", "false", {
      timeout: RUN_STEPS_MS,
    });
    const running = await page.evaluate(() => {
      const seen: unknown = Reflect.get(window, "__goingRunningSteps");
      return Array.isArray(seen) ? seen.map(String) : [];
    });
    expect(running).toContain("call_tool");
    const answer = run.getByTestId("going-run-answer");
    await expect(answer.getByRole("link", { name: issue.key })).toHaveAttribute(
      "href",
      `/issues/${issue.key}`,
      { timeout: RUN_STEPS_MS }
    );
    await expect(run).toHaveAttribute("data-state", "done", { timeout: RUN_STEPS_MS });
  });
});

// SLOW(>=2s): three seeded issues, a run of a read, a chart and an answer.
test("each number of a chart is the server's count over the read, and the number the model wrote shows nowhere in it", async ({
  browser,
}) => {
  modelsLane();
  test.setTimeout(90_000);
  await createProject({ key: "CORE", name: "Core" });
  for (const title of ["Grind the lens", "Silver the mirror"]) {
    const made = await createIssue({ project: "CORE", title });
    await patchIssue(made.key, { status: "in_progress" });
  }
  // The third issue keeps the status a new issue is given.
  const unstarted = (await createIssue({ project: "CORE", title: "Align the finder" })).status;
  const words = "chart CORE's issues by status";
  await scriptChat({
    default: prose("A short answer while the person pauses."),
    runs: [
      {
        turns: [
          callTool("dispatch_issues", { project: "CORE" }),
          callTool("draw_block", {
            chain: [{ field: "status", op: "group" }, { op: "count" }],
            kind: "chart",
            steps: [{ call: 1, step: 2 }],
            title: "CORE's issues by status",
            x: "status",
            y: "count",
          }),
          say("CORE holds 47 issues in progress."),
        ],
        words,
      },
    ],
  });
  await withAlice(browser, async (page) => {
    await openBarOver(page, "/projects/CORE/issues");
    await askTheAgent(page, words);
    const run = newestRun(page);
    const chart = run.getByTestId("going-block");
    await expect(chart).toHaveAttribute("data-kind", "chart", { timeout: RUN_STEPS_MS });
    await expect(run).toHaveAttribute("data-state", "done", { timeout: RUN_STEPS_MS });
    // Two issues are in progress and one keeps a new issue's status, each count beside its own.
    await expect(chartValue(chart, "in_progress")).toHaveText("2");
    await expect(chartValue(chart, unstarted)).toHaveText("1");
    await expect(chart).not.toContainText("47");
  });
});
