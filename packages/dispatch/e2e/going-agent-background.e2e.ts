import { getIssue } from "./api";
import {
  callTool,
  endlessStream,
  modelsLane,
  proposePlan,
  runCalls,
  say,
  scriptChat,
} from "./fake-models-helpers";
import {
  askTheAgent,
  bar,
  newestRun,
  openBarOver,
  prose,
  RUN_STEPS_MS,
  searchBox,
  seedCoreIssue,
} from "./going-panel";
import { expect, type Page, test } from "./test";
import { withAlice } from "./users";

// Check c16 of dispatch://INFRA-1588/artifact/verification-md (spec R10, R11, R1): Stop in the
// header stops a run whose model never ends: the server goes away from the model call in flight,
// the fake records no call after it, and the steps stay; Esc in the empty box while a run works
// stops it too; a run whose bar is closed goes on, and a chip on the page reads working, then done
// with Open; and a run that makes a plan, closed at once, ends with the chip that says a plan
// waits, and the plan is in the Inbox, where Approve runs it. Negative control: a Stop that only
// closes the client's stream, after which the server's model call goes on.

/** Holds that the stop ended the run's model call in flight: the fake records that its caller
 *  went away, and no call of the run came after it. */
async function expectCallLeft(words: string): Promise<void> {
  await expect
    .poll(async () => (await runCalls(words)).at(-1)?.left ?? null, { timeout: 5_000 })
    .not.toBeNull();
  const calls = await runCalls(words);
  const left = calls.at(-1)?.left ?? 0;
  expect(calls.filter((call) => call.at > left)).toHaveLength(0);
}

/** A run that reads the issue, then streams without end. */
function endless(key: string) {
  return {
    afterwards: endlessStream(),
    turns: [callTool("dispatch_read", { issue: key }), endlessStream()],
  };
}

async function waitForAStep(page: Page): Promise<void> {
  await expect(newestRun(page).getByTestId("going-step").first()).toHaveAttribute(
    "data-running",
    "false",
    { timeout: RUN_STEPS_MS }
  );
}

// SLOW(>=2s): a run that never ends, stopped by the header's Stop.
test("Stop in the header stops a run at once, and its steps stay", async ({ browser }) => {
  modelsLane();
  test.setTimeout(90_000);
  const key = await seedCoreIssue("Track the comet");
  const words = "watch the comet forever";
  await scriptChat({ default: prose("A short answer."), runs: [{ ...endless(key), words }] });
  await withAlice(browser, async (page) => {
    await openBarOver(page, "/projects/CORE/issues");
    await askTheAgent(page, words);
    await waitForAStep(page);
    await bar(page).getByTestId("going-stop").click();
    await expect(newestRun(page)).toHaveAttribute("data-state", "stopped_person", {
      timeout: RUN_STEPS_MS,
    });
    await expect(newestRun(page).getByTestId("going-step").first()).toBeVisible();
    await expectCallLeft(words);
  });
});

// SLOW(>=2s): a run that never ends, stopped by Esc in the empty box.
test("Esc in the empty box while a run works stops it, and the bar stays open", async ({
  browser,
}) => {
  modelsLane();
  test.setTimeout(90_000);
  const key = await seedCoreIssue("Track the comet");
  const words = "watch the comet until I say";
  await scriptChat({ default: prose("A short answer."), runs: [{ ...endless(key), words }] });
  await withAlice(browser, async (page) => {
    await openBarOver(page, "/projects/CORE/issues");
    await askTheAgent(page, words);
    await waitForAStep(page);
    await expect(searchBox(page)).toHaveValue("");
    await searchBox(page).press("Escape");
    await expect(newestRun(page)).toHaveAttribute("data-state", "stopped_person", {
      timeout: RUN_STEPS_MS,
    });
    await expect(bar(page)).toBeVisible();
    await expectCallLeft(words);
  });
});

// SLOW(>=2s): a run closed in the bar, which goes on and ends behind the chip.
test("a run whose bar is closed goes on, and the chip reads working, then done with Open", async ({
  browser,
}) => {
  modelsLane();
  test.setTimeout(90_000);
  const key = await seedCoreIssue("Track the comet");
  const words = "read the comet's issue when you can";
  await scriptChat({
    default: prose("A short answer."),
    runs: [
      {
        turns: [
          { ...callTool("dispatch_read", { issue: key }), delayMs: 300 },
          say(`The comet is tracked in ${key}.`),
        ],
        words,
      },
    ],
  });
  await withAlice(browser, async (page) => {
    await openBarOver(page, "/projects/CORE/issues");
    await askTheAgent(page, words);
    await bar(page).getByTestId("going-close").click();
    await expect(bar(page)).toHaveCount(0);
    const chip = page.getByTestId("going-chip");
    await expect(chip).toHaveAttribute("data-state", "working");
    await expect(chip).toHaveAttribute("data-state", "done", { timeout: RUN_STEPS_MS });
    await expect(chip).toContainText("Done");
    await chip.getByTestId("going-chip-open").click();
    await expect(newestRun(page).getByTestId("going-run-answer")).toContainText(
      `The comet is tracked in ${key}.`
    );
  });
});

// SLOW(>=2s): a run that plans, closed at once, and its plan approved in the Inbox.
test("a run that makes a plan, closed at once, ends with the chip saying a plan waits, and Approve in the Inbox runs it", async ({
  browser,
}) => {
  modelsLane();
  test.setTimeout(120_000);
  const key = await seedCoreIssue("Track the comet");
  const words = `start ${key} for me`;
  await scriptChat({
    default: prose("A short answer."),
    runs: [
      {
        afterwards: say("Started."),
        turns: [
          proposePlan("Start the comet's tracking.", [
            { arguments: { issue: key, status: "in_progress" }, tool: "dispatch_issue_update" },
          ]),
        ],
        words,
      },
    ],
  });
  await withAlice(browser, async (page) => {
    await openBarOver(page, "/");
    await askTheAgent(page, words);
    // Close, not Esc: Esc in the empty box while the run works would stop it.
    await bar(page).getByTestId("going-close").click();
    await expect(bar(page)).toHaveCount(0);
    const chip = page.getByTestId("going-chip");
    await expect(chip).toContainText("A plan waits", { timeout: RUN_STEPS_MS });
    const waiting = page.getByTestId("inbox-going-plan");
    await expect(waiting).toContainText(words, { timeout: RUN_STEPS_MS });
    await waiting.getByTestId("inbox-going-plan-approve").click();
    await expect
      .poll(async () => (await getIssue(key)).status, { timeout: RUN_STEPS_MS })
      .toBe("in_progress");
  });
});
