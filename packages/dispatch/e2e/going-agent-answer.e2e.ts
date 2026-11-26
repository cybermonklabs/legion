import { createIssue, createProject, getIssueEvents, patchIssue } from "./api";
import { callTool, modelsLane, prose, say, scriptChat } from "./fake-models-helpers";
import { askAndWait, askTheAgent, newestRun, openBarOver, RUN_STEPS_MS } from "./going-locators";
import { runFrames } from "./going-panel";
import { expect, type Locator, test } from "./test";
import { withAlice } from "./users";

// Check c3 of dispatch://INFRA-1588/artifact/verification-md (spec R3): a pause streams a short
// answer and the store records no write; Enter starts a run, each tool call is a step the run
// sends as it starts (its `call` frame, in the present tense) before it ends (its `tool` frame),
// and the answer links each item the fake read; and each number of a chart is the server's, from
// the chain the fake asks for over its read, never the number the fake writes in its text.
// Negative control: a bar that draws the numbers the model wrote shows the wrong number.

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
test("Enter starts a run: each tool call is a step sent as it starts and again as it ends, and the answer links each item it read", async ({
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
          callTool("dispatch_read", { issue: issue.key }),
          say(`The mapping is in [${issue.key}](/issues/${issue.key}).`),
        ],
        words,
      },
    ],
  });
  await withAlice(browser, async (page) => {
    await openBarOver(page, "/projects/CORE/issues");
    await askTheAgent(page, words);
    const run = newestRun(page);
    await expect(run).toHaveAttribute("data-state", "done", { timeout: RUN_STEPS_MS });
    // Once the answer came, the steps fold into one line above it, which says what they did and
    // opens them on a click.
    const fold = run.getByTestId("going-steps-fold");
    await expect(fold).toHaveAccessibleName(/read 1 item$/i);
    await fold.click();
    await expect(run.getByTestId("going-step").first()).toHaveAttribute("data-tool", "call_tool");
    await expect(run.getByTestId("going-step").first()).toHaveAttribute("data-running", "false");
    // The ended step reads as words: the read's past form, never the tool's name or its JSON, and
    // its key is a plain link.
    await expect(run.getByTestId("going-step").first()).toHaveText(`Read ${issue.key}`);
    await expect(
      run.getByTestId("going-step").first().getByRole("link", { name: issue.key })
    ).toHaveAttribute("href", `/issues/${issue.key}`);
    // The answer's link to the item it read is the item's chip: its key, status and title.
    const answer = run.getByTestId("going-run-answer");
    await expect(answer.getByRole("link", { name: issue.key })).toHaveAttribute(
      "href",
      `/issues/${issue.key}`
    );
    await expect(answer.getByTestId("item-chip")).toContainText(issue.key);
    await expect(answer.getByTestId("item-chip")).toContainText("Map the lunar craters");
    await expect(run.getByTestId("going-run-state")).toHaveText("Done");
    // The run's stream, as the person reads it: the read's call frame, with its present words,
    // comes before the tool frame that ends the same step. Both can reach the page in one render,
    // so the running line itself is proven in `run-frames.test.ts` and `ConversationView.test.tsx`.
    const frames = await runFrames(page, (await run.getAttribute("data-run")) ?? "");
    const call = frames.findIndex((frame) => frame.name === "call");
    expect(frames[call]?.data).toMatchObject({
      kind: "read",
      label: `Reading ${issue.key}`,
      tool: "call_tool",
    });
    const step = frames[call]?.data.step;
    expect(typeof step).toBe("number");
    const ended = frames.findIndex((frame) => frame.name === "tool" && frame.data.step === step);
    expect(ended).toBeGreaterThan(call);
    expect(frames[ended]?.data).toMatchObject({ label: `Read ${issue.key}`, tool: "call_tool" });
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
          // 83 is above 59, so no time or date on the page can hold it: the chart's caption shows
          // the time of its read ("as of 11:47 AM"), where a number up to 59 shows whenever the
          // clock's minute is that number.
          say("CORE holds 83 issues in progress."),
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
    await expect(chart).not.toContainText("83");
  });
});
