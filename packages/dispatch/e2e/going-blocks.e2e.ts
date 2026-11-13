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
  answerPanel,
  answerText,
  askAndWait,
  closeBar,
  countAskedRuns,
  openBar,
  openBarOver,
  plainWords,
  prose,
  STATUS_CHART_SPEC,
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

declare global {
  interface Window {
    __goingFrames?: string[];
  }
}

/** Records, from before the page loads, the name of every frame of the answer stream
 *  (`GET /api/v1/going/requests/{id}/stream`) in the order it arrives, read off a copy of the body
 *  of the `fetch` the API client calls beside the page's own reading of it. A connection the page
 *  opens again resumes after the last frame it read, so the record holds each frame once. Returns
 *  a reader of what was recorded. */
async function recordStreamedFrames(page: Page): Promise<() => Promise<string[]>> {
  await page.addInitScript(() => {
    const frames: string[] = [];
    window.__goingFrames = frames;
    const sent = window.fetch;
    // Every name of the page's own is read off `window`: this module's `Request` and `Response`
    // are Playwright's.
    const recording = async (
      input: Parameters<typeof window.fetch>[0],
      init?: Parameters<typeof window.fetch>[1]
    ) => {
      const response = await sent(input, init);
      const asked = input instanceof window.Request ? input.url : String(input);
      const path = new URL(asked, window.location.href).pathname;
      if (response.body === null || !/^\/api\/v1\/going\/requests\/[^/]+\/stream$/.test(path)) {
        return response;
      }
      const [read, copy] = response.body.tee();
      void (async () => {
        const pieces = copy.getReader();
        const text = new TextDecoder();
        let held = "";
        try {
          for (;;) {
            const piece = await pieces.read();
            if (piece.done) return;
            held += text.decode(piece.value, { stream: true });
            const lines = held.split("\n");
            held = lines.pop() ?? "";
            for (const line of lines) {
              const name = /^event: (\w+)\r?$/.exec(line)?.[1];
              if (name !== undefined) frames.push(name);
            }
          }
        } catch {
          // A connection cut under the page is the page's to open again; the record keeps what
          // arrived on it.
        }
      })();
      return new window.Response(read, {
        headers: response.headers,
        status: response.status,
        statusText: response.statusText,
      });
    };
    window.fetch = recording as typeof window.fetch;
  });
  return () => page.evaluate(() => [...(window.__goingFrames ?? [])]);
}

/** The issues the fixture seeds, by the status each is set to: the counts every block is read
 *  against. */
const FIXTURE = { done: 2, todo: 3 } as const;

/** The read every progress-shaped block names: the project's issues, counted by status. */
const projectRead = { params: { project: "CORE" }, source: "read_progress" } as const;

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
    spec: STATUS_CHART_SPEC,
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
  await scriptChat({ default: withBlock("chart", STATUS_CHART_SPEC) });
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
  await scriptChat({ default: withBlock("chart", STATUS_CHART_SPEC) });
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
    const streamedFrames = await recordStreamedFrames(page);
    await openBarOver(page, "/projects/CORE/issues");
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
    // The panel drew the whole answer, so its stream has ended; the record reads its last frame.
    await expect.poll(streamedFrames).toContain("end");
    const frames = await streamedFrames();
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
  await scriptChat({ default: withBlock("chart", STATUS_CHART_SPEC) });
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
    await closeBar(page);
    const replayed = page.waitForRequest((request) =>
      request.url().endsWith(`/api/v1/going/requests/${run}/stream`)
    );
    await openBar(page);
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
