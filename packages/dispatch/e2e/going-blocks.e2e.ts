import { plannerSession, resetFakeEnvoy, reviewerSession, setLiveSessions } from "./agents";
import {
  createIssue,
  createProject,
  listIssueArtifacts,
  listProjectArtifacts,
  patchIssue,
  setGoingDisplayFit,
} from "./api";
import { seedDeliveryFixture } from "./delivery-seed";
import { decisionRecord, modelsLane, scriptChat, scriptDecision } from "./fake-models-helpers";
import {
  ANSWER_ENDS_MS,
  answerPanel,
  answerText,
  askAndWait,
  bar,
  countAskedRuns,
  openBarOver,
  plainWords,
  prose,
  searchBox,
  shownWords,
  withBlock,
} from "./going-panel";
import { expect, type Locator, type Page, test } from "./test";
import { asUser } from "./users";

// Check c4 of dispatch://INFRA-1588/artifact/verification-md, serving R2, and the browser legs
// of c6, serving R9: a block is drawn when it is asked for, and its numbers are the server's
// read. Each row seeds a fixture whose counts it set, scripts the fake writing model to name the
// read that fills a block - a spec that names fields and never a number - and reads what the
// panel draws against the fixture. A plain question draws no block, even where the model writes
// one; a chart appears unasked at or above the chart-display threshold the committed record holds
// and not below it, and with the display fit turned off only an asked chart is drawn. A chart is
// not stored: the issue and the project hold no new artifact after one, and reopening the bar
// draws it again from the same run. Every row runs in the models lane alone.

/** The issues the fixture seeds, by the status each is set to: the counts every block is read
 *  against. */
const FIXTURE = { done: 2, todo: 3 } as const;

/** The read every progress-shaped block names: the project's issues, counted by status. */
const projectRead = { params: { project: "CORE" }, source: "read_progress" } as const;
const chartSpec = { ...projectRead, mark: "bar", table: "statuses", x: "status", y: "count" };

interface Fixture {
  readonly open: readonly string[];
  readonly issue: string;
}

async function seedIssues(): Promise<Fixture> {
  await createProject({ key: "CORE", name: "Core" });
  const open: string[] = [];
  for (const [status, count] of Object.entries(FIXTURE)) {
    for (let index = 0; index < count; index += 1) {
      const issue = await createIssue({ project: "CORE", title: `Bearing ${status} ${index}` });
      await patchIssue(issue.key, { status });
      if (status !== "done") open.push(issue.key);
    }
  }
  return { issue: open[0] ?? "", open };
}

function block(page: Page): Locator {
  return answerPanel(page).getByTestId("going-block");
}

/** Each row of a table the panel drew, as its cells' text. */
async function tableRows(table: Locator): Promise<string[][]> {
  return table
    .locator("tbody tr")
    .evaluateAll((rows) =>
      rows.map((row) => [...row.querySelectorAll("td")].map((cell) => cell.textContent ?? ""))
    );
}

/** The chart's points as the panel drew them, `[x, y]`, in the read's order. */
function chartPoints(page: Page): Promise<string[][]> {
  return tableRows(page.getByTestId("going-block-chart-data"));
}

/** `[status, count]` points sorted by status, so two lists compare whatever order each is in. */
function fixturePoints(points: string[][]): string[][] {
  return [...points].sort(([left = ""], [right = ""]) => left.localeCompare(right));
}

const fixtureCountPoints = fixturePoints(
  Object.entries(FIXTURE).map(([status, count]) => [status, String(count)])
);

async function expectChart(page: Page): Promise<void> {
  await expect(block(page)).toHaveAttribute("data-kind", "chart");
  await expect(page.getByTestId("going-block-chart")).toHaveAttribute("data-chart-drawn", "true");
  expect(fixturePoints(await chartPoints(page))).toEqual(fixtureCountPoints);
}

/** One kind asked for by name: the words, the spec the model writes, and what the panel must
 *  draw from the fixture. */
interface KindCase {
  readonly kind: string;
  readonly words: string;
  readonly spec: unknown;
  readonly seed?: () => Promise<void>;
  readonly check: (page: Page, fixture: Fixture) => Promise<void>;
}

const DELIVERY_WINDOW = { from: "2024-06-01T00:00:00Z", to: "2024-06-02T00:00:00Z" };

const kindCases: readonly KindCase[] = [
  {
    check: expectChart,
    kind: "chart",
    spec: chartSpec,
    words: "chart the CORE issues by status",
  },
  {
    check: async (page) => {
      const rows = await tableRows(page.getByTestId("going-block-table"));
      expect(fixturePoints(rows)).toEqual(fixtureCountPoints);
    },
    kind: "table",
    spec: { ...projectRead, columns: ["status", "count"], table: "statuses" },
    words: "show the CORE issues by status as a table",
  },
  {
    check: async (page) => {
      await expect(page.getByTestId("going-block-metric-value")).toHaveText(
        String(FIXTURE.done + FIXTURE.todo)
      );
      await expect(page.getByTestId("going-block-metric-compare")).toHaveText(String(FIXTURE.done));
    },
    kind: "metric",
    spec: {
      ...projectRead,
      compare: "done",
      compare_label: "done",
      field: "total",
      label: "issues",
      table: "progress",
    },
    words: "how many issues does CORE have",
  },
  {
    check: async (page) => {
      await expect(page.getByTestId("going-block-progress-count")).toHaveText(
        `${FIXTURE.done} of ${FIXTURE.done + FIXTURE.todo}`
      );
    },
    kind: "progress",
    spec: projectRead,
    words: "what is the progress of CORE",
  },
  {
    check: async (page, fixture) => {
      const keys = await page
        .getByTestId("going-block-issue")
        .evaluateAll((rows) => rows.map((row) => row.querySelector("span")?.textContent ?? ""));
      expect([...keys].sort()).toEqual([...fixture.open].sort());
    },
    kind: "issue_list",
    spec: projectRead,
    words: "list the open issues of CORE",
  },
  {
    check: async (page) => {
      const agents = page.getByTestId("going-block-agent");
      await expect(agents).toHaveCount(2);
      const sessions = await agents.evaluateAll((rows) =>
        rows.map((row) => row.getAttribute("data-session") ?? "")
      );
      expect(sessions.sort()).toEqual(
        [plannerSession.session_id, reviewerSession.session_id].sort()
      );
      // Neither seeded session has an open ask, and the block says so in each row.
      await expect(page.getByTestId("going-block-agent-asks")).toHaveText(["0", "0"]);
      await expect(page.getByTestId("going-answer-roster")).toHaveText(/^roster \d+ s old$/);
    },
    kind: "agent_status",
    seed: async () => {
      await resetFakeEnvoy();
      await setLiveSessions([plannerSession, reviewerSession]);
    },
    spec: { params: {}, source: "read_roster" },
    words: "what is the agent status",
  },
  {
    check: async (page) => {
      // The delivery fixture's day: two merged pull requests and two deploy runs.
      const events = page.getByTestId("going-block-timeline-event");
      await expect(events).toHaveCount(4);
      await expect(events.filter({ hasText: "acme/widgets#1 feat: a shipped widget" })).toHaveCount(
        1
      );
      await expect(events.filter({ hasText: "run 501: failure" })).toHaveCount(1);
    },
    kind: "timeline",
    seed: seedDeliveryFixture,
    spec: { params: { view: "timeline", ...DELIVERY_WINDOW }, source: "read_delivery" },
    words: "show the delivery timeline",
  },
];

for (const kindCase of kindCases) {
  // SLOW(>=2s): a seeded fixture, a decision, an answer and the read its block names.
  test(`a ${kindCase.kind} block is drawn when the words ask for it, its numbers the fixture's own`, async ({
    browser,
  }) => {
    modelsLane();
    test.setTimeout(90_000);
    const fixture = await seedIssues();
    await kindCase.seed?.();
    await scriptChat({ default: withBlock(kindCase.kind, kindCase.spec) });
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openBarOver(page, "/projects/CORE/issues");
      await askAndWait(page, kindCase.words);

      await expect(block(page)).toHaveAttribute("data-kind", kindCase.kind);
      await kindCase.check(page, fixture);
    } finally {
      await context.close();
    }
  });
}

/** Words that name no block of any kind. */
const plainQuestion = "where are the CORE bearing notes kept";

// SLOW(>=2s): two decisions and two answers.
test("a plain question draws no block, even where the model writes a table for it", async ({
  browser,
}) => {
  modelsLane();
  test.setTimeout(90_000);
  await seedIssues();
  await scriptChat({
    replies: [
      prose("They are kept with the bearings."),
      withBlock("table", { ...projectRead, columns: ["status", "count"], table: "statuses" }),
    ],
  });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await openBarOver(page, "/projects/CORE/issues");
    await askAndWait(page, plainQuestion);
    await expect(answerText(page)).toContainText("kept with the bearings");
    await expect(block(page)).toHaveCount(0);

    await askAndWait(page, `${plainQuestion} now`);
    await expect(answerText(page)).toContainText("Here it is.");
    await expect(block(page)).toHaveCount(0);
  } finally {
    await context.close();
  }
});

// SLOW(>=2s): two decisions and two answers.
test("a chart appears unasked at the chart-display threshold and not below it", async ({
  browser,
}) => {
  modelsLane();
  test.setTimeout(90_000);
  const threshold = decisionRecord().chart_threshold;
  test.skip(threshold === null, "the committed record holds no chart-display threshold");
  if (threshold === null) return;
  await seedIssues();
  await scriptDecision({
    replies: [
      { display: { choice: "chart", confidence: threshold } },
      { display: { choice: "chart", confidence: threshold - 0.01 } },
    ],
  });
  await scriptChat({ default: withBlock("chart", chartSpec) });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await openBarOver(page, "/projects/CORE/issues");
    await askAndWait(page, "how are the CORE issues spread across statuses");
    await expectChart(page);

    await askAndWait(page, "how are the CORE issues spread out now");
    await expect(answerText(page)).toContainText("Here it is.");
    await expect(block(page)).toHaveCount(0);
  } finally {
    await context.close();
  }
});

// SLOW(>=2s): two decisions and two answers, with the display fit turned off and on again.
test("with the chart-display threshold unset, a full-confidence chart choice draws no chart and an asked chart is drawn", async ({
  browser,
}) => {
  modelsLane();
  test.setTimeout(90_000);
  await seedIssues();
  await scriptDecision({ default: { display: { choice: "chart", confidence: 1 } } });
  await scriptChat({ default: withBlock("chart", chartSpec) });
  await setGoingDisplayFit(true);
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await openBarOver(page, "/projects/CORE/issues");
    await askAndWait(page, "how are the CORE issues spread across statuses");
    await expect(answerText(page)).toContainText("Here it is.");
    await expect(block(page)).toHaveCount(0);

    await askAndWait(page, "chart the CORE issues by status");
    await expectChart(page);
  } finally {
    await context.close();
    await setGoingDisplayFit(false);
  }
});

// SLOW(>=2s): a decision and a 115-word answer with its table.
test("115 words with a table draws both, the prose uncut, and the block frame after the text frames", async ({
  browser,
}) => {
  modelsLane();
  test.setTimeout(90_000);
  await seedIssues();
  await scriptChat({
    default: withBlock(
      "table",
      { ...projectRead, columns: ["status", "count"], table: "statuses" },
      plainWords("gamma", 115)
    ),
  });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await openBarOver(page, "/projects/CORE/issues");
    const streamed = page.waitForResponse(
      (response) => /\/api\/v1\/going\/requests\/[^/]+\/stream$/.test(response.url()),
      { timeout: ANSWER_ENDS_MS }
    );
    await askAndWait(page, "show the CORE issues by status as a table");

    expect(await shownWords(page)).toHaveLength(115);
    await expect(page.getByTestId("going-answer-cut")).toHaveCount(0);
    expect(fixturePoints(await tableRows(page.getByTestId("going-block-table")))).toEqual(
      fixtureCountPoints
    );
    // The block is drawn after the text, as its frame came after every text frame.
    const after = await answerText(page).evaluate(
      (text, figure) => Boolean(text.compareDocumentPosition(figure) & 4),
      await block(page).elementHandle()
    );
    expect(after).toBe(true);
    const frames = [...(await (await streamed).text()).matchAll(/^event: (\w+)$/gm)].map(
      ([, name]) => name
    );
    expect(frames.lastIndexOf("text")).toBeGreaterThanOrEqual(0);
    expect(frames.indexOf("block")).toBeGreaterThan(frames.lastIndexOf("text"));
  } finally {
    await context.close();
  }
});

// SLOW(>=2s): a decision and an answer.
test("a block of a kind outside the seven is dropped and the answer stays prose and links", async ({
  browser,
}) => {
  modelsLane();
  test.setTimeout(90_000);
  await seedIssues();
  await scriptChat({
    default: withBlock("pie", { ...projectRead, table: "statuses" }, "The issues, by status."),
  });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await openBarOver(page, "/projects/CORE/issues");
    await askAndWait(page, "show the CORE issues as a pie");
    await expect(answerText(page)).toContainText("The issues, by status.");
    await expect(block(page)).toHaveCount(0);
  } finally {
    await context.close();
  }
});

// SLOW(>=2s): a decision, an answer with its chart, two artifact reads, and the bar reopened.
test("a chart is not stored: the issue and the project gain no artifact, and reopening the bar draws it again from the same run", async ({
  browser,
}) => {
  modelsLane();
  test.setTimeout(90_000);
  const fixture = await seedIssues();
  const issueBefore = await listIssueArtifacts(fixture.issue);
  const projectBefore = await listProjectArtifacts("CORE");
  await scriptChat({ default: withBlock("chart", chartSpec) });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    const asked = countAskedRuns(page);
    await openBarOver(page, `/issues/${fixture.issue}`);
    await askAndWait(page, "chart the CORE issues by status");
    await expectChart(page);
    const run = await answerPanel(page).getAttribute("data-run");
    expect(run).not.toBeNull();

    expect(await listIssueArtifacts(fixture.issue)).toEqual(issueBefore);
    expect(await listProjectArtifacts("CORE")).toEqual(projectBefore);

    // Closed and opened again, with an empty box: the same run's stream, read from its first
    // frame, draws the chart again, and nothing is asked anew.
    await page.keyboard.press("Escape");
    await expect(bar(page)).toHaveCount(0);
    const replayed = page.waitForRequest((request) =>
      request.url().endsWith(`/api/v1/going/requests/${run}/stream`)
    );
    await page.locator("body").focus();
    await page.keyboard.press("Control+k");
    await expect(searchBox(page)).toBeFocused();
    expect((await replayed).headers()["last-event-id"]).toBe("0");
    await expect(page.getByTestId("going-answer-replayed")).toContainText(
      "chart the CORE issues by status"
    );
    await expect(answerPanel(page)).toHaveAttribute("data-run", run ?? "");
    await expectChart(page);
    expect(asked.words).toEqual(["chart the CORE issues by status"]);
    expect(await listIssueArtifacts(fixture.issue)).toEqual(issueBefore);
  } finally {
    await context.close();
  }
});
