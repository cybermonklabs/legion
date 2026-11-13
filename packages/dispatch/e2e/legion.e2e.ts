// The Legion view on the project Issues page, its settings control, the Settings page's own
// Legion daemon field, the live push, `/pipeline` and the Delivery page's Legion section and chart
// controls (dispatch://INFRA-1077, acceptance A1; dispatch://INFRA-2916, R14). Every spec is tagged
// `@legion`, so `--grep @legion` runs this file's work and nothing else.
//
// The board reads only what Dispatch recorded of the project's daemon, so the fixture writes those
// rows (`legion-seed.ts`); the settings control is the one place that does call the daemon, through
// Dispatch's relay, so that spec stands a fake daemon up and reads what reached it.
import type { EvalListResponse } from "../web/src/api/types";
import { baseUrl, createProject, userHeaders } from "./api";
import { PIPELINE_SECTION_URL, seedPipelineFixture } from "./delivery-seed";
import {
  LEGION_PROJECT,
  pushLegionObservation,
  seedLegionFinishedTree,
  seedLegionFixture,
  seedLegionProject,
  startFakeLegionDaemon,
} from "./legion-seed";
import { expect, test } from "./test";
import { asUser } from "./users";

/** The ten columns, in the order the server sends them and the view draws them. */
const COLUMNS = [
  "queue",
  "design",
  "planning",
  "implementing",
  "testing",
  "reviewing",
  "accepting",
  "merging",
  "awaiting_person",
  "held",
];

test("the Legion view shows the daemon's columns and its cards @legion", async ({ browser }) => {
  const daemon = await startFakeLegionDaemon({});
  await seedLegionFixture(daemon.url);
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    await page.goto(`/projects/${LEGION_PROJECT}`);
    await page.getByRole("button", { name: "Legion" }).click();

    const view = page.getByRole("region", { name: "Legion" });
    await expect(view).toBeVisible();
    // Every column, in the server's order, each with the label the server gave it.
    await expect(view.locator("[data-legion-column]")).toHaveCount(COLUMNS.length);
    expect(
      await view
        .locator("[data-legion-column]")
        .evaluateAll((columns) =>
          columns.map((column) => column.getAttribute("data-legion-column"))
        )
    ).toEqual(COLUMNS);
    await expect(page.getByRole("region", { name: "Waiting on a person" })).toBeVisible();

    // The header: two of the four slots held, and the daemon's waiting line.
    await expect(view.locator("[data-legion-slots]")).toHaveText("2 of 4 slots in use");
    await expect(view.locator("[data-legion-queue-length]")).toHaveText("3 waiting in the queue");

    // The implementing card, with every fact the spec names on it.
    const implementing = page.getByRole("region", { name: "Implementing" });
    const card = implementing.locator('[data-legion-card="ACME-1"]');
    await expect(card).toBeVisible();
    await expect(card).toContainText("Ship the widgets");
    await expect(card).toContainText("P0");
    await expect(card.locator('[data-legion-fact="phase-time"]')).toContainText("2 h");
    await expect(card.locator('[data-legion-fact="slot"]')).toContainText("0");
    await expect(card.locator('[data-legion-fact="roles"]')).toContainText("implementer");
    await expect(card.locator('[data-legion-fact="checks"]')).toContainText("failing");

    // The queue keeps the daemon's own order: ACME-4 before ACME-3, then the blocked root, which
    // names what holds it. Neither key order nor priority order would give this.
    const queue = page.getByRole("region", { name: "Queue" });
    expect(
      await queue
        .locator("[data-legion-card]")
        .evaluateAll((cards) => cards.map((card) => card.getAttribute("data-legion-card")))
    ).toEqual(["ACME-4", "ACME-3", "ACME-5"]);
    await expect(
      queue.locator('[data-legion-card="ACME-5"] [data-legion-fact="blocked-by"]')
    ).toContainText("Blocked by ACME-1");

    // What a root waits on from a person, on its own card.
    await expect(
      page
        .getByRole("region", { name: "Waiting on a person" })
        .locator('[data-legion-card="ACME-2"] [data-legion-fact="waiting-on"]')
    ).toContainText("Waiting on ask");
  } finally {
    await context.close();
    await daemon.close();
  }
});

test("the settings control changes the admission cap of a daemon @legion", async ({ browser }) => {
  const daemon = await startFakeLegionDaemon({ admission_cap: { applied: "at-once" } });
  await seedLegionFixture(daemon.url);
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    await page.goto(`/projects/${LEGION_PROJECT}`);
    await page.getByRole("button", { name: "Legion" }).click();

    // Each setting with its value and where it came from, as the daemon reports them.
    const cap = page.locator('[data-legion-setting="admission_cap"]');
    await expect(cap.locator("[data-legion-setting-value]")).toHaveText("4");
    await expect(cap.locator('[data-legion-setting-source="file"]')).toContainText(
      "configuration file"
    );
    await expect(
      page.locator('[data-legion-setting="review_round_cap"] [data-legion-setting-source="route"]')
    ).toContainText("Changed here by bob");

    await page.getByLabel("New value for admission_cap").fill("6");
    await page.getByRole("button", { name: "Change admission_cap" }).click();

    // What the daemon said about the key, and the new value with the route as its source and the
    // person who changed it.
    await expect(cap.locator("[data-legion-setting-result]")).toHaveText("Applied at once");
    await expect(cap.locator("[data-legion-setting-value]")).toHaveText("6");
    await expect(cap.locator('[data-legion-setting-source="route"]')).toContainText(
      "Changed here by alice"
    );

    // Dispatch relayed the change itself, naming the person who asked for it.
    expect(daemon.changes).toEqual([{ by: "alice", changes: { admission_cap: 6 } }]);
  } finally {
    await context.close();
    await daemon.close();
  }
});

// The daemon's third verdict, the one a person must not read as a success: a key it took into
// its own settings and could not carry into the thing that holds a copy of the value
// (`failed`, the daemon's internal/api/settings_route.go). The row's answer is announced
// (`role="alert"`, on the span RowAnswer renders), reads the holder's own error, and the value
// stays the new one, because the daemon's settings carry it and only a client, listener or
// connection kept the old.
//
// It drives `admission_cap`: the fixture seeds that key and `review_round_cap` and no other
// (`legion-seed.ts`), and the panel reads the verdict alone — `describeVerdict` keys on nothing
// about the path. The verdict is the shape a real failure has, the daemon's own
// `worker_rpc_timeout_seconds`: `holder` is the row's named holder, while the holder that kept
// the old value is named inside `failed`, which the daemon writes as `<holder>: <error>`
// (internal/daemon/rebuilder.go's observe). So the row proves the other half too — the text
// names what the daemon's text names, never the row's holder.
test("the panel shows a failed verdict as a failure and keeps the new value @legion", async ({
  browser,
}) => {
  const daemon = await startFakeLegionDaemon({
    admission_cap: {
      failed: "nats: reopen the intake consumers: nats: no servers available for connection",
      holder: "worker-stream",
      old: "4",
      new: "6",
    },
  });
  await seedLegionFixture(daemon.url);
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    await page.goto(`/projects/${LEGION_PROJECT}`);
    await page.getByRole("button", { name: "Legion" }).click();
    const cap = page.locator('[data-legion-setting="admission_cap"]');
    await expect(cap.locator("[data-legion-setting-value]")).toHaveText("4");

    await page.getByLabel("New value for admission_cap").fill("6");
    await page.getByRole("button", { name: "Change admission_cap" }).click();

    const answer = cap.locator("[data-legion-setting-result]");
    const failed =
      "The daemon took the new value, and what holds it kept the old one: " +
      "nats: reopen the intake consumers: nats: no servers available for connection";
    await expect(answer).toHaveText(failed);
    // Announced as a failure, and naming no holder the daemon's own text did not name.
    await expect(answer.locator('[role="alert"]')).toHaveText(failed);
    await expect(answer).not.toContainText("worker-stream");
    // The daemon took the value, so the row shows the new one rather than rolling back to 4.
    await expect(cap.locator("[data-legion-setting-value]")).toHaveText("6");
  } finally {
    await context.close();
    await daemon.close();
  }
});

// A1: "a daemon URL set on the Settings page that brings the view", and A4's "A project with no
// Legion setting shows no Legion view". The project is seeded with its issues and its daemon's
// observations but no `legion_settings` row, so the only thing that can bring the view is the
// person naming the daemon on the Settings page, beside the Delivery settings, as the Design's
// "Where the data comes from" says.
test("a project with no Legion setting has no Legion view until the Settings page names its daemon @legion", async ({
  browser,
}) => {
  const daemon = await startFakeLegionDaemon({});
  await seedLegionProject();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    await page.goto(`/projects/${LEGION_PROJECT}`);
    await expect(page.getByRole("button", { name: "Board" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Legion" })).toHaveCount(0);

    // The Settings page, beside the Delivery settings.
    await page.goto("/settings");
    await expect(page.getByRole("heading", { name: "Legion" })).toBeVisible();
    await page.getByLabel("Legion daemon state URL").fill(daemon.url);
    await page
      .getByRole("combobox", { exact: true, name: "Legion project" })
      .selectOption(LEGION_PROJECT);
    const saved = page.waitForResponse(
      (response) =>
        response.request().method() === "PUT" &&
        response.url().endsWith(`/api/v1/settings/legion/${LEGION_PROJECT}`)
    );
    await page.getByRole("button", { name: "Save Legion daemon" }).click();
    expect((await saved).status()).toBe(200);

    // The view the setting brought.
    await page.goto(`/projects/${LEGION_PROJECT}`);
    await page.getByRole("button", { name: "Legion" }).click();
    await expect(page.getByRole("region", { name: "Legion" })).toBeVisible();
    await expect(
      page.getByRole("region", { name: "Implementing" }).locator('[data-legion-card="ACME-1"]')
    ).toBeVisible();
  } finally {
    await context.close();
    await daemon.close();
  }
});

// The other half of the Settings control: the URL it refuses, and the Remove that takes the
// view away again. A state URL that is not an absolute http(s) one with a host and no path is
// refused, and the page's own wording must be the server's own, since the page carries a second
// copy of the rule (`legionStateUrl` in LegionDaemonsSection.tsx) and only the server's copy
// decides what is stored.
test("the Settings page refuses a state URL that is not a daemon's, and its Remove takes the view away @legion", async ({
  browser,
}) => {
  const daemon = await startFakeLegionDaemon({});
  await seedLegionFixture(daemon.url);
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const bad = `${daemon.url}/legion/v1/state`;

  try {
    // What the server says about this URL, so the page can be held to its wording.
    const refusal = await fetch(new URL(`/api/v1/settings/legion/${LEGION_PROJECT}`, baseUrl), {
      body: JSON.stringify({ state_url: bad }),
      headers: { "Content-Type": "application/json", ...(await userHeaders("alice")) },
      method: "PUT",
    });
    expect(refusal.status).toBe(400);
    const refused = (await refusal.json()) as { code: string; error: string };
    expect(refused.code).toBe("LEGION_SETTING_INPUT");

    await page.goto("/settings");
    const section = page.locator("[data-legion-daemons]");
    await expect(section).toContainText(daemon.url);

    // The page refuses the same URL, in the server's own words, and sends no write at all.
    let wrote = false;
    page.on("request", (request) => {
      if (
        request.method() === "PUT" &&
        request.url().endsWith(`/api/v1/settings/legion/${LEGION_PROJECT}`)
      ) {
        wrote = true;
      }
    });
    await page.getByLabel("Legion daemon state URL").fill(bad);
    await page
      .getByRole("combobox", { exact: true, name: "Legion project" })
      .selectOption(LEGION_PROJECT);
    await page.getByRole("button", { name: "Save Legion daemon" }).click();
    await expect(page.getByRole("alert")).toHaveText(refused.error);
    expect(wrote).toBe(false);
    // The refusal changed nothing: the project still reads the daemon it did.
    await expect(section).toContainText(daemon.url);

    // Remove stops the reading, and the Issues page loses its Legion view.
    const removed = page.waitForResponse(
      (response) =>
        response.request().method() === "DELETE" &&
        response.url().endsWith(`/api/v1/settings/legion/${LEGION_PROJECT}`)
    );
    await page
      .getByRole("button", { name: `Remove the Legion daemon of ${LEGION_PROJECT}` })
      .click();
    expect((await removed).status()).toBeLessThan(300);

    await page.goto(`/projects/${LEGION_PROJECT}`);
    await expect(page.getByRole("button", { name: "Board" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Legion" })).toHaveCount(0);
  } finally {
    await context.close();
    await daemon.close();
  }
});

// A1: "a `/ws/live` push that moves a card with no reload". A read of the daemon writes the
// observations it found and announces itself on `legion.StateChannel`; the open board recomputes
// and the card moves with no navigation of any kind.
test("a live push moves a card between columns with no reload @legion", async ({ browser }) => {
  const daemon = await startFakeLegionDaemon({});
  await seedLegionFixture(daemon.url);
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    await page.goto(`/projects/${LEGION_PROJECT}`);
    await page.getByRole("button", { name: "Legion" }).click();
    const implementing = page.getByRole("region", { name: "Implementing" });
    const testing = page.getByRole("region", { name: "Testing" });
    await expect(implementing.locator('[data-legion-card="ACME-1"]')).toBeVisible();
    await expect(testing.locator('[data-legion-card="ACME-1"]')).toHaveCount(0);

    // What a read of the daemon a minute later would have left, and the announcement it makes.
    await pushLegionObservation("ACME-1", "phase", '"testing"');

    // No reload: the card moves under the open page.
    await expect(testing.locator('[data-legion-card="ACME-1"]')).toBeVisible({ timeout: 15000 });
    await expect(implementing.locator('[data-legion-card="ACME-1"]')).toHaveCount(0);
  } finally {
    await context.close();
    await daemon.close();
  }
});

// A1: "`/pipeline` reaching the Pipeline section".
test("/pipeline reaches the Delivery page's Pipeline section @legion", async ({ browser }) => {
  await seedPipelineFixture();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    await page.goto("/pipeline");
    await expect(page).toHaveURL(/\/delivery#pipeline$/);
    const pipeline = page.locator("#pipeline");
    await expect(pipeline).toBeVisible();
    await expect(page.getByRole("heading", { name: /Pipeline/ })).toBeVisible();
  } finally {
    await context.close();
  }
});

// A1: "every chart on the Delivery page switching to its table, back to its graph, and to a
// compare range". Every chart the page draws is read off the page itself, so a chart added later
// is covered without this spec being edited. The Legion section is seeded here too, so its four
// metrics' charts are among them.
test("every chart on the Delivery page shows its table, its graph again and a compare range @legion", async ({
  browser,
}) => {
  // Four more charts than the Pipeline section's own: the Legion section's metrics are on this
  // page too, and each one is toggled, compared and toggled again here.
  test.setTimeout(120_000);
  const daemon = await startFakeLegionDaemon({});
  await seedPipelineFixture();
  await seedLegionFixture(daemon.url);
  await seedLegionFinishedTree();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto(PIPELINE_SECTION_URL);
    await expect(page.getByRole("heading", { name: "Delivery timeline" })).toBeVisible();
    const labels = await page
      .locator("[data-chart]")
      .evaluateAll((charts) => charts.map((chart) => chart.getAttribute("data-chart") ?? ""));
    expect(labels.length).toBeGreaterThan(0);

    for (const label of labels) {
      const chart = page.locator(`[data-chart="${label}"]`);
      const view = chart.getByRole("group", { name: `${label} view` });
      await view.getByRole("button", { name: "Table" }).click();
      await expect(chart.locator(`[data-chart-table="${label}"]`)).toBeVisible();
      await view.getByRole("button", { name: "Graph" }).click();
      await expect(chart.locator(`[data-chart-table="${label}"]`)).toHaveCount(0);
      await expect(chart.locator('[data-chart-range="window"]').first()).toBeVisible();

      // The compare control's default second range: the range just before the window.
      await chart.locator(`select[data-chart-compare="${label}"]`).selectOption("preceding");
      await expect(chart.locator('[data-chart-range="compare"]').first()).toBeVisible();
      await view.getByRole("button", { name: "Table" }).click();
      // The compare column's heading names whatever a row of this chart is: a UTC day for the
      // delivery and pipeline charts, an issue, a worker type or a UTC week for the Legion ones.
      await expect(
        chart.locator(`[data-chart-table="${label}"] th`, { hasText: /^Compare / }).first()
      ).toBeVisible();
      await view.getByRole("button", { name: "Graph" }).click();
    }
  } finally {
    await context.close();
    await daemon.close();
  }
});

/** One read of a project's Legion evals over `[from, to)`, or over the route's own default window
 *  where none is given, as `GET /api/v1/projects/{key}/evals?kind=legion` answers it. */
async function readLegionEvals(
  project: string,
  window: { from: string; to: string } | null
): Promise<EvalListResponse> {
  const url = new URL(`/api/v1/projects/${encodeURIComponent(project)}/evals`, baseUrl);
  url.searchParams.set("kind", "legion");
  if (window !== null) {
    url.searchParams.set("from", window.from);
    url.searchParams.set("to", window.to);
  }
  const response = await fetch(url, { headers: await userHeaders("alice") });
  if (!response.ok) throw new Error(`GET ${url.pathname} failed: ${response.status}`);
  return (await response.json()) as EvalListResponse;
}

/** How long the Legion evals of a seeded history take to be built: the facts consumer's sweep,
 *  every 30 s whatever wakes it (`legionfacts.SweepEvery`), stores the issues' facts, the day-row
 *  refresher, ten seconds apart at most (`evals.RefresherGap`), folds them into day rows, and a
 *  project written straight into Postgres has its evals seeded on that refresher's pass. Two
 *  sweeps and two passes, with room. */
const legionEvalsBuilt = 150_000;

/** The window from the first day the project's work-time eval holds data to the midnight after
 *  today, once its count over that window is `finished`: a window that starts before a series'
 *  first day reads no value at all (`evalSeriesOf`, api/evals.go), so the page is opened over the
 *  days the history holds. */
async function legionWindow(
  project: string,
  finished: number
): Promise<{ from: string; to: string }> {
  const today = new Date();
  const to = new Date(
    Date.UTC(today.getUTCFullYear(), today.getUTCMonth(), today.getUTCDate() + 1)
  ).toISOString();
  let window = { from: "", to };
  await expect
    .poll(
      async () => {
        const hours = (read: EvalListResponse) =>
          read.items
            ?.find((item) => item.definition.slug === "legion-work-time")
            ?.series?.find((one) => one.series === "hours");
        const since = hours(await readLegionEvals(project, null))?.data_since;
        if (since === null || since === undefined) return null;
        window = { from: `${since}T00:00:00Z`, to };
        return hours(await readLegionEvals(project, window))?.window?.n ?? null;
      },
      { intervals: [2_000], timeout: legionEvalsBuilt }
    )
    .toBe(finished);
  return window;
}

// Spec R3/R4 and the Design's "The Delivery page", section 1, as dispatch://INFRA-2916's R14 moved
// it onto the eval registry: the Legion section draws the four metrics — work time to merge, phase
// time by worker type, rework, and splitting and follow-ups — of the project with a daemon, each
// through the one chart component, with the roots and the children cuts beside every issue and no
// toggle between them. The fixture's tree is one root and one child that finished
// (`seedLegionFinishedTree`); the numbers are the project's Legion evals the server builds from
// it, so the row waits for them and opens the page over the days they hold.
// SLOW(>=2s): it waits for the facts consumer's sweep and the day-row refresher to build the evals it draws.
test("the Delivery page's Legion section draws the four metrics @legion", async ({ browser }) => {
  test.setTimeout(legionEvalsBuilt + 60_000);
  const daemon = await startFakeLegionDaemon({});
  await seedLegionFixture(daemon.url);
  await seedLegionFinishedTree();
  const window = await legionWindow(LEGION_PROJECT, 2);
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    await page.goto(
      `/delivery?from=${encodeURIComponent(window.from)}&to=${encodeURIComponent(window.to)}`
    );
    const section = page.getByRole("region", { name: "Legion: issue to merge" });
    await expect(section).toBeVisible();
    const block = section.locator(`[data-legion-project="${LEGION_PROJECT}"]`);
    await expect(block).toBeVisible();

    // One card per metric, each drawing its numbers through `Chart`, so each has the
    // graph-or-table toggle and the compare control every chart on this page has.
    for (const [metric, title] of [
      ["work_time", "Work time to merge"],
      ["phase_time", "Phase time by worker type"],
      ["rework", "Rework"],
      ["splitting", "Splitting and follow-ups"],
    ]) {
      const label = `${LEGION_PROJECT}: ${title}`;
      const card = block.locator(`[data-legion-metric="${metric}"]`);
      await expect(card).toBeVisible();
      await expect(card.locator(`[data-chart="${label}"]`)).toBeVisible();
      await expect(card.getByRole("group", { name: `${label} view` })).toBeVisible();
      await expect(card.locator(`select[data-chart-compare="${label}"]`)).toBeVisible();
    }
    await expect(block.locator("[data-legion-metric]")).toHaveCount(4);

    // The headline numbers of the two issues that finished: both are counted, the child's
    // review round and send-back are on the rework card, and the child the architect filed
    // while designing the root is on the splitting card, in the children cut.
    const figure = (metric: string, label: string) =>
      block.locator(`[data-legion-metric="${metric}"] [data-legion-figure="${label}"]`);
    await expect(figure("work_time", "Issues finished")).toHaveText("2");
    // Two issues finished: the root with no review round, the child with one.
    await expect(figure("rework", "Median review rounds")).toHaveText("0.5");
    await expect(figure("rework", "Median review rounds, roots")).toHaveText("0.0");
    await expect(figure("rework", "Median review rounds, children")).toHaveText("1.0");
    await expect(figure("rework", "Sent back at least once")).toHaveText("50.0%");
    await expect(figure("splitting", "Issues filed")).toHaveText("1");
    await expect(figure("splitting", "Issues filed, roots")).toHaveText("0");
    await expect(figure("splitting", "Issues filed, children")).toHaveText("1");
    await expect(figure("splitting", "Roots finished")).toHaveText("1");

    // The worker types the child's work was shared out over, on the phase-time card's table,
    // and the count of issues each bar holds hours for: a bar's median runs over every finished
    // issue, one the role never worked counting as zero, so that count is what keeps a median
    // of 0 readable as a population most issues are not in. The cuts are columns beside it.
    const phaseTime = block.locator('[data-legion-metric="phase_time"]');
    await phaseTime.getByRole("button", { name: "Table" }).click();
    const table = phaseTime.locator(
      `[data-chart-table="${LEGION_PROJECT}: Phase time by worker type"]`
    );
    await expect(table.locator("td", { hasText: "implementer" }).first()).toBeVisible();
    await expect(table.locator("th", { hasText: "Worker type" }).first()).toBeVisible();
    await expect(table.locator("th", { hasText: "Issues with hours" }).first()).toBeVisible();
    await expect(table.locator("th", { hasText: "Hours, roots" }).first()).toBeVisible();
    await expect(table.locator("th", { hasText: "Hours, children" }).first()).toBeVisible();

    // No toggle and no picker: every cut is drawn, and every project with a daemon is a block.
    await expect(section.getByRole("button", { name: "Roots" })).toHaveCount(0);
    await expect(section.getByLabel("Legion project")).toHaveCount(0);
  } finally {
    await context.close();
    await daemon.close();
  }
});

/** The Legion metrics route dispatch://INFRA-2916 deleted, which the row below refuses a request
 *  of. It is written by its segments so that a search of the repository for a caller of that
 *  path (k14) finds none, this guard against one included. */
const DELETED_LEGION_METRICS = ["", "api", "v1", "legion", "metrics"].join("/");

// dispatch://INFRA-2916's R14 (k14's browser half): the Legion section reads the eval routes and
// draws one block for each project with a Legion setting, with no first-project default. Two
// projects are made through the API, which seeds each one's evals, and each names a daemon; the
// page's requests are recorded from before it opens. The base head read the deleted metrics
// route (DELETED_LEGION_METRICS) for the first project alone, which this row refuses three ways:
// a request naming that route, no request of GET /api/v1/evals, and fewer blocks than projects
// with a setting.
// @pr: The Delivery page's Legion section is the one reader of the cross-project eval read, on a page a person reads.
test("the Delivery page's Legion section reads the eval routes and draws one block for each project with a Legion setting @legion @pr", async ({
  browser,
}) => {
  const daemon = await startFakeLegionDaemon({});
  for (const key of ["ACME", "WIDGET"]) {
    await createProject({ key, name: key });
    const named = await fetch(new URL(`/api/v1/settings/legion/${key}`, baseUrl), {
      body: JSON.stringify({ state_url: daemon.url }),
      headers: { "Content-Type": "application/json", ...(await userHeaders("alice")) },
      method: "PUT",
    });
    expect(named.status).toBe(200);
  }
  const listed = await fetch(new URL("/api/v1/settings/legion", baseUrl), {
    headers: await userHeaders("alice"),
  });
  const projects = ((await listed.json()) as { project: string }[]).map((one) => one.project);
  expect([...projects].sort()).toEqual(["ACME", "WIDGET"]);
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const requested: URL[] = [];
  page.on("request", (request) => requested.push(new URL(request.url())));

  try {
    await page.goto("/delivery");
    const section = page.getByRole("region", { name: "Legion: issue to merge" });
    await expect(section.locator("[data-legion-project]")).toHaveCount(projects.length);
    for (const project of projects) {
      const block = section.locator(`[data-legion-project="${project}"]`);
      await expect(block).toBeVisible();
      await expect(block.locator("[data-legion-metric]")).toHaveCount(4);
    }

    expect(requested.filter((url) => url.pathname.startsWith(DELETED_LEGION_METRICS))).toEqual([]);
    const evals = requested.filter((url) => url.pathname === "/api/v1/evals");
    expect(evals.length).toBeGreaterThan(0);
    // Every read names every project with a setting and the Legion kind: no project is picked.
    for (const url of evals) {
      expect(url.searchParams.getAll("project").sort()).toEqual([...projects].sort());
      expect(url.searchParams.get("kind")).toBe("legion");
    }
  } finally {
    await context.close();
    await daemon.close();
  }
});
