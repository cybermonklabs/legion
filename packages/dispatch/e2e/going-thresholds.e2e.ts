import { createIssue, createProject, getIssue, listComments, patchIssue } from "./api";
import type { ChatReply } from "./fake-chat-model";
import { decisionRecord, modelsLane, scriptChat, scriptDecision } from "./fake-models-helpers";
import {
  answerPanel,
  answerText,
  askAndWait,
  countGoingWrites,
  decided,
  offerRow,
  openBarOver,
  operationRow,
  STATUS_CHART_SPEC,
  submitOperation,
  writeLines,
} from "./going-panel";
import { expect, type Page, test } from "./test";

// Check c9 of dispatch://INFRA-1588/artifact/verification-md, serving R5: the bar acts only where
// the deciding model is calibrated to be right, and otherwise asks. Each row scripts the fake
// decision model's confidence against the thresholds the committed record holds for the model
// the harness names (`decisionRecord()`), never a number of its own, and reads what the bar then
// does: at or above the action threshold the operation runs on submit; below it the two best
// actions are offered and neither runs until the person picks one. The two thresholds are then
// read apart on one answer - the chart drawn while the action is only offered, and the action
// run while no chart is drawn. Every row runs in the models lane alone.

/** How far below a threshold a choice the bar must not act on sits. */
const BELOW = 0.01;

/** The change the rows ask for, and the runner-up the decision ranks second. */
const UPDATE = "dispatch_issue_update";
const COMMENT = "dispatch_comment";

/** The words of every row: words that ask for no block, so a chart is drawn only where the
 *  display choice says so. */
const words = "how are the CORE bearing issues spread across statuses";

interface Fixture {
  readonly keys: readonly string[];
}

/** Three issues at two statuses, so the project's status read has the two points a chart needs. */
async function seed(): Promise<Fixture> {
  await createProject({ key: "CORE", name: "Core" });
  const keys: string[] = [];
  for (const [title, status] of [
    ["Bearing grease", "todo"],
    ["Bearing seals", "todo"],
    ["Bearing shims", "backlog"],
  ] as const) {
    const issue = await createIssue({ project: "CORE", title });
    await patchIssue(issue.key, { status });
    keys.push(issue.key);
  }
  return { keys };
}

/** The writing model's reply: arguments for both actions, and, where `chart`, a chart of the
 *  project's issues by status the display choice may or may not draw. */
function reply(fixture: Fixture, chart: boolean): ChatReply {
  return {
    final: {
      answer: "Most of them are still to do.",
      arguments: {
        [COMMENT]: [{ body: "Most are still to do.", issue: fixture.keys[0] }],
        [UPDATE]: [{ issue: fixture.keys[0], status: "in_progress" }],
      },
      ...(chart
        ? { block: { kind: "chart", spec: STATUS_CHART_SPEC, title: "CORE by status" } }
        : {}),
    },
  };
}

async function ask(page: Page): Promise<void> {
  await openBarOver(page, "/projects/CORE/issues");
  await askAndWait(page, words);
  await expect(answerText(page)).toContainText("Most of them are still to do.");
}

/** The first issue's status, which the update changes. */
async function firstStatus(fixture: Fixture): Promise<string> {
  return (await getIssue(fixture.keys[0] ?? "")).status;
}

// SLOW(>=2s): a decision, an answer and the write the submit runs.
test("a choice at the action threshold is acted on: the submit runs it with no pick", async ({
  page,
}) => {
  modelsLane();
  test.setTimeout(90_000);
  const fixture = await seed();
  const threshold = decisionRecord().action_threshold;
  await scriptDecision({ default: decided(UPDATE, threshold, COMMENT) });
  await scriptChat({ default: reply(fixture, false) });
  const writes = countGoingWrites(page);

  await ask(page);
  await expect(operationRow(page)).toBeVisible();
  await expect(offerRow(page, UPDATE)).toHaveCount(0);
  await expect(offerRow(page, COMMENT)).toHaveCount(0);
  await submitOperation(page);

  await expect(writeLines(page)).toHaveCount(1);
  await expect(writeLines(page).first()).toHaveAttribute("data-state", "done");
  expect(writes.bodies).toEqual([{ approved: "auto", invocation: 0, offer: UPDATE }]);
  expect(await firstStatus(fixture)).toBe("in_progress");
});

// SLOW(>=2s): a decision, an answer, a wait that proves the submit runs nothing, and a pick.
test("the same choice below the action threshold offers the two best actions and runs neither until the person picks one", async ({
  page,
}) => {
  modelsLane();
  test.setTimeout(90_000);
  const fixture = await seed();
  const threshold = decisionRecord().action_threshold;
  await scriptDecision({ default: decided(UPDATE, threshold - BELOW, COMMENT) });
  await scriptChat({ default: reply(fixture, false) });
  const writes = countGoingWrites(page);

  await ask(page);
  await expect(offerRow(page, UPDATE)).toBeVisible();
  await expect(offerRow(page, COMMENT)).toBeVisible();
  await expect(operationRow(page)).toHaveCount(0);
  await submitOperation(page);
  // An absence window: below the threshold a submit must run neither offer during this second.
  await page.waitForTimeout(1_000);
  expect(writes.bodies).toEqual([]);
  expect(await firstStatus(fixture)).toBe("todo");

  await offerRow(page, UPDATE).click();

  await expect(writeLines(page)).toHaveCount(1);
  await expect(writeLines(page).first()).toHaveAttribute("data-state", "done");
  expect(writes.bodies).toEqual([{ approved: "person", invocation: 0, offer: UPDATE }]);
  expect(await firstStatus(fixture)).toBe("in_progress");
  expect(await listComments(fixture.keys[0] ?? "")).toEqual([]);
});

// SLOW(>=2s): a decision, an answer with its chart, and a wait that proves nothing runs.
test("display confidence above the chart threshold with action confidence below the action threshold: the chart is drawn and the two best are offered and neither runs", async ({
  page,
}) => {
  modelsLane();
  test.setTimeout(90_000);
  const record = decisionRecord();
  test.skip(record.chart_threshold === null, "the committed record holds no chart threshold");
  const chartThreshold = record.chart_threshold ?? 0;
  const fixture = await seed();
  await scriptDecision({
    default: decided(UPDATE, record.action_threshold - BELOW, COMMENT, {
      choice: "chart",
      confidence: chartThreshold,
    }),
  });
  await scriptChat({ default: reply(fixture, true) });
  const writes = countGoingWrites(page);

  await ask(page);
  const block = answerPanel(page).getByTestId("going-block");
  await expect(block).toHaveAttribute("data-kind", "chart");
  await expect(page.getByTestId("going-block-chart")).toHaveAttribute("data-chart-drawn", "true");
  await expect(offerRow(page, UPDATE)).toBeVisible();
  await expect(offerRow(page, COMMENT)).toBeVisible();
  await expect(operationRow(page)).toHaveCount(0);
  await submitOperation(page);
  // An absence window: a chart drawn says nothing of the action, which must still not run.
  await page.waitForTimeout(1_000);
  expect(writes.bodies).toEqual([]);
  expect(await firstStatus(fixture)).toBe("todo");
  expect(await listComments(fixture.keys[0] ?? "")).toEqual([]);
});

// SLOW(>=2s): a decision, an answer with an undrawn chart, and the write the submit runs.
test("display confidence below the chart threshold with action confidence above the action threshold: no chart is drawn and the action runs at once", async ({
  page,
}) => {
  modelsLane();
  test.setTimeout(90_000);
  const record = decisionRecord();
  test.skip(record.chart_threshold === null, "the committed record holds no chart threshold");
  const chartThreshold = record.chart_threshold ?? 0;
  const fixture = await seed();
  await scriptDecision({
    default: decided(UPDATE, record.action_threshold, COMMENT, {
      choice: "chart",
      confidence: chartThreshold - BELOW,
    }),
  });
  await scriptChat({ default: reply(fixture, true) });
  const writes = countGoingWrites(page);

  await ask(page);
  await expect(answerPanel(page).getByTestId("going-block")).toHaveCount(0);
  await expect(operationRow(page)).toBeVisible();
  await submitOperation(page);

  await expect(writeLines(page)).toHaveCount(1);
  await expect(writeLines(page).first()).toHaveAttribute("data-state", "done");
  expect(writes.bodies).toEqual([{ approved: "auto", invocation: 0, offer: UPDATE }]);
  expect(await firstStatus(fixture)).toBe("in_progress");
});
