/** The Evals tab's check rows k1 to k24 (dispatch://INFRA-2918/artifact/plan-md, "Checks: surface
 *  and command", T28) over the projects `e2e/evals-seed.ts` writes: which sections the server's
 *  sources show a project, the first paint's views and the frames the live hub sends for each, the
 *  issues the items route names behind each group link, the method and links the eval detail and
 *  the register write hold, the deploy record the production-event routes write, the figures the
 *  hub computes by default repository, from the input's first day and narrowed by a facet, and the
 *  pages of the merge timeline the project-timeline view grows by. */
import { readdirSync, readFileSync } from "node:fs";
import { join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import type {
  DeliveryMeasuresResponse,
  EvalListItem,
  EvalListResponse,
  LiveClientFrame,
  LiveServerFrame,
  ProjectTimelineResponse,
} from "../web/src/api/types";
import { baseUrl, getArtifactText, getProjectArtifact, mintAgentToken, registerEval } from "./api";
import {
  chartFigure,
  DECLARED_EVAL,
  DEFAULT_REPO,
  DELIVERY_OFF,
  DEPLOY_RECORD,
  deltasOf,
  dropDeliverySettings,
  EVERY_SOURCE,
  evalsViewItems,
  evalsViews,
  expectEveryRelation,
  expectLegionWork,
  expectNumberStands,
  GONE_LINK,
  getJson,
  groupLinks,
  headersCut,
  listEvals,
  liveFrame,
  logReadings,
  NO_DAEMON,
  NO_SOURCE,
  openGroupLink,
  openTab,
  PAGED,
  PAGED_MERGES,
  PAGED_WINDOW,
  pagedMergedAt,
  RELATIONS_EVAL,
  readTimeline,
  recordLive,
  seedDefaultRepository,
  seedDeployRecord,
  seedEverySource,
  seedFourProjects,
  seedNoDaemon,
  seedPagedTimeline,
  seedRelations,
  showChart,
  showGroupLinks,
  showP0Link,
  snapshotsOf,
  subscribedViews,
  TAB_QUERY,
  TAB_WINDOW,
  windowFigure,
  windowQuery,
} from "./evals-seed";
import {
  evalsSection,
  graphValues,
  openEvalsTimeline,
  openMergeTimeline,
  paintedSection,
} from "./evals-tab";
import { sql } from "./psql";
import { expect, type Locator, type Page, test, type WebSocketRoute } from "./test";

/** The section ids the tab draws, in the order it draws them (`data-section`). */
function sectionIds(page: Page): Promise<(string | null)[]> {
  return page
    .locator("main [data-section]")
    .evaluateAll((sections) => sections.map((section) => section.getAttribute("data-section")));
}

/** The bundle the harness server serves (`e2e/run-server.sh`'s `web_dist`, a path from
 *  packages/envoy where `DISPATCH_E2E_SERVER_WEB_DIST` names one), else this package's own. */
function webDist(): string {
  const named = process.env.DISPATCH_E2E_SERVER_WEB_DIST;
  return named === undefined || named === ""
    ? fileURLToPath(new URL("../web/dist/", import.meta.url))
    : resolve(fileURLToPath(new URL("../../envoy/", import.meta.url)), named);
}

/** Holds that the address `page` stands at is `path` with `search`, the query kept whole. */
async function expectAddress(page: Page, path: string, search: string): Promise<void> {
  await expect
    .poll(() => {
      const url = new URL(page.url());
      return `${url.pathname}${url.search}`;
    })
    .toBe(`${path}?${search}`);
}

// SLOW(>=2s): seeds four projects and opens each one's tab, then both old addresses.
test("every project's numbers are its Evals tab, which the old addresses open @evals k1", async ({
  page,
}) => {
  await seedFourProjects();
  for (const project of [EVERY_SOURCE, NO_DAEMON, DELIVERY_OFF, NO_SOURCE]) {
    await openTab(page, project);
    await expect(page.getByRole("tab", { name: /Legion/ }), project).toHaveCount(0);
  }

  // The old pages' addresses open the default project's tab, the first by key with a source
  // (decision 4), with the query they named.
  await page.goto("/delivery?window=7d");
  await expectAddress(page, `/projects/${EVERY_SOURCE}/evals`, "window=7d");
  await page.goto(`/pipeline?${TAB_QUERY}`);
  await expectAddress(page, `/projects/${EVERY_SOURCE}/evals`, TAB_QUERY);

  // The built bundle's route table reaches no Delivery page, no Pipeline page and no Legion tab:
  // no chunk is named for one, and no chunk's text names one. The Evals tab's own route is the
  // control that the search reads the route table.
  const assets = join(webDist(), "assets");
  const names = readdirSync(assets);
  expect(
    names.filter((name) => /^(DeliveryPage|PipelinePage|Legion(Page|Tab))\b/.test(name))
  ).toEqual([]);
  const texts = names
    .filter((name) => name.endsWith(".js"))
    .map((name) => ({ name, text: readFileSync(join(assets, name), "utf8") }));
  expect(texts.some(({ text }) => text.includes("/projects/:key/evals"))).toBe(true);
  expect(
    texts.flatMap(({ name, text }) =>
      ["DeliveryPage", "PipelinePage", "/projects/:key/legion"]
        .filter((word) => text.includes(word))
        .map((word) => `${name}: ${word}`)
    )
  ).toEqual([]);
});

// SLOW(>=2s): seeds every source, a Legion daemon and a logged eval with a declared chart.
test("the tab draws its sections in order under one header of four controls @evals k2", async ({
  page,
}) => {
  await seedEverySource();
  await openTab(page, EVERY_SOURCE, TAB_QUERY);
  await expect
    .poll(() => sectionIds(page))
    .toEqual(["cards", "legion", "ci", "deploys", "timeline", "logged"]);

  // Last, the logged eval's own section, which draws the chart it declares.
  const logged = await paintedSection(page, "logged");
  const declared = logged.locator(`[data-eval="${DECLARED_EVAL.slug}"] [data-declared]`);
  await expect(chartFigure(declared, `${DECLARED_EVAL.slug}/${DECLARED_EVAL.chart}`)).toBeVisible();

  // The header: the window, the compare range, the repositories and the freshness line, with the
  // days the first answer counts.
  const header = page.getByRole("group", { name: "Window and filters" });
  await expect(header.getByLabel("Window", { exact: true })).toBeVisible();
  await expect(header.getByLabel("Compare with")).toBeVisible();
  await expect(header.getByLabel("Repository", { exact: true })).toBeVisible();
  await expect(header.getByRole("region", { name: "Source freshness" })).toBeVisible();
  await expect(header.locator("[data-daemon-read]")).toBeVisible();
  await expect(header.getByTestId("evals-days")).toHaveText(
    "Window 2026-08-30 to 2026-10-02 (UTC), 34 days; compared with 2026-07-27 to 2026-08-29 (UTC), 34 days"
  );
});

// SLOW(>=2s): seeds every source, holds one view's frames back and writes a reading.
test("the first paint opens every view at once and each section paints from its own @evals k3", async ({
  page,
}) => {
  await seedEverySource();
  const sent: { at: number; frame: LiveClientFrame }[] = [];
  const received: { at: number; frame: LiveServerFrame }[] = [];
  // The CI evals view's frames are held back until the other sections have painted: the page's
  // socket and the frames Dispatch sent for that view's subscription while held.
  const hold: {
    active: boolean;
    messages: (string | Buffer)[];
    socket: WebSocketRoute | null;
    sub: string | null;
  } = { active: true, messages: [], socket: null, sub: null };
  await page.routeWebSocket(/\/ws\/live$/u, (socket) => {
    const server = socket.connectToServer();
    hold.socket = socket;
    socket.onMessage((message) => {
      const frame = liveFrame<LiveClientFrame>(message);
      sent.push({ at: Date.now(), frame });
      if (
        frame.type === "subscribe" &&
        frame.view.kind === "evals" &&
        new URLSearchParams(frame.view.query).get("kind") === "ci"
      ) {
        hold.sub = frame.sub;
      }
      server.send(message);
    });
    server.onMessage((message) => {
      const frame = liveFrame<LiveServerFrame>(message);
      received.push({ at: Date.now(), frame });
      if (hold.active && "sub" in frame && frame.sub !== undefined && frame.sub === hold.sub) {
        hold.messages.push(message);
        return;
      }
      socket.send(message);
    });
  });
  await openTab(page, EVERY_SOURCE, TAB_QUERY);

  // Every other section paints while the CI view has not answered.
  // The Logged section reads every kind's view, the CI one too, so it waits for it.
  for (const id of ["cards", "legion", "deploys"]) await paintedSection(page, id);
  const ci = evalsSection(page, "ci");
  await expect(ci).toHaveAttribute("data-state", "subscribed");
  await expect(ci.getByRole("status")).toHaveText("CI health: loading\u2026");
  hold.active = false;
  for (const message of hold.messages.splice(0)) hold.socket?.send(message);
  await paintedSection(page, "ci");

  // The four evals views, one of each kind the tab shows, and the daemon's read, opened at once,
  // each answered by one snapshot.
  const views = subscribedViews(sent).filter(
    (view) => view.kind === "evals" && view.project === EVERY_SOURCE
  );
  expect(views.map((view) => view.query.get("kind")).sort()).toEqual([
    "ci",
    "delivery",
    "experiment",
    "legion",
  ]);
  for (const view of views) expect(snapshotsOf(received, view.sub), view.sub).toHaveLength(1);
  const daemon = subscribedViews(sent).filter((view) => view.kind === "legion-read");
  expect(daemon.map((view) => view.project)).toEqual([EVERY_SOURCE]);

  // A reading's write reaches the experiment view as a delta.
  const experiment = views.find((view) => view.query.get("kind") === "experiment");
  if (experiment === undefined) throw new Error("the tab opened no experiment view");
  await logReadings(EVERY_SOURCE, DECLARED_EVAL.slug, [
    {
      measured_at: "2026-09-20T12:00:00Z",
      row_kind: "point",
      series: DECLARED_EVAL.series,
      value: 150,
    },
  ]);
  await expect.poll(() => deltasOf(received, experiment.sub).length).toBeGreaterThan(0);

  // The REST routes the views mirror answer the same shapes: the list route each evals view's
  // query names, and the daemon's read.
  for (const view of views) {
    const kind = view.query.get("kind") ?? "";
    const snapshot = snapshotsOf(received, view.sub)[0]?.frame.body as EvalListResponse;
    const listed = await getJson<EvalListResponse>(
      `/api/v1/projects/${EVERY_SOURCE}/evals?${windowQuery(TAB_WINDOW)}&${view.query.toString()}`
    );
    expect(Object.keys(listed).sort(), kind).toEqual(Object.keys(snapshot).sort());
    expect((listed.items ?? []).map((item) => item.id).sort(), kind).toEqual(
      (snapshot.items ?? []).map((item) => item.id).sort()
    );
  }
  const readSub = daemon[0]?.sub ?? "";
  const readBody = snapshotsOf(received, readSub)[0]?.frame.body ?? {};
  const read = await getJson<object>(`/api/v1/projects/${EVERY_SOURCE}/legion/read`);
  expect(Object.keys(read).sort()).toEqual(Object.keys(readBody).sort());
});

/** The delivery cards' and the Legion cards' slugs, as `TargetCards.tsx` names them. */
const DELIVERY_CARD = "deploy-frequency";
const LEGION_CARD = "legion-work-time";

// SLOW(>=2s): seeds four projects and opens each one's tab, one twice.
test("each project's sources decide what its tab shows, and no number reads 0 for none @evals k5", async ({
  page,
}) => {
  await seedFourProjects();

  // Every source: every section, no line on a missing source.
  await openTab(page, EVERY_SOURCE, TAB_QUERY);
  await expect
    .poll(() => sectionIds(page))
    .toEqual(["cards", "legion", "ci", "deploys", "timeline", "logged"]);
  await expect(page.locator("[data-missing-sources]")).toHaveCount(0);

  // No daemon: no Legion section and no Legion card; a delivery card of no row reads "no data".
  await openTab(page, NO_DAEMON);
  await expect
    .poll(() => sectionIds(page))
    .toEqual(["cards", "ci", "deploys", "timeline", "logged"]);
  let cards = await paintedSection(page, "cards");
  await expect(cards.locator(`[data-card="${LEGION_CARD}"]`)).toHaveCount(0);
  await expect(cards.locator(`[data-card="${DELIVERY_CARD}"]`)).toContainText("no data");
  const statistics = cards.locator(`[data-card="${DELIVERY_CARD}"] [data-window-statistic]`);
  await expect(statistics.first()).toHaveText("no data");
  expect((await statistics.allTextContents()).filter((text) => text !== "no data")).toEqual([]);
  await expect(page.locator("[data-missing-sources]")).toHaveCount(0);
  await expect(page.locator("[data-daemon-read]")).toHaveCount(0);

  // Delivery off: the Legion sections and cards alone, and the daemon's read in the header.
  await openTab(page, DELIVERY_OFF);
  await expect.poll(() => sectionIds(page)).toEqual(["cards", "legion", "timeline", "logged"]);
  cards = await paintedSection(page, "cards");
  await expect(cards.locator(`[data-card="${DELIVERY_CARD}"]`)).toHaveCount(0);
  await expect(cards.locator(`[data-card="${LEGION_CARD}"]`)).toContainText("no data");
  await expect(page.locator("[data-missing-sources]")).toHaveCount(0);
  await expect(page.locator("[data-daemon-read]")).toBeVisible();

  // The same daemon with no delivery settings row at all: the header says delivery is not set up,
  // linking to the project's settings.
  await dropDeliverySettings(DELIVERY_OFF);
  await openTab(page, DELIVERY_OFF);
  const notSetUp = page.locator("[data-missing-sources]").getByRole("link", {
    name: "Delivery: not set up",
  });
  await expect(notSetUp).toHaveAttribute("href", `/projects/${DELIVERY_OFF}/settings`);

  // No source: one line naming both settings, each linked, and no section.
  await openTab(page, NO_SOURCE);
  const missing = page.locator("[data-missing-sources]");
  await expect(missing).toContainText("Nothing to show yet");
  await expect(missing.getByRole("link", { name: "Legion daemon" })).toHaveAttribute(
    "href",
    "/settings"
  );
  await expect(missing.getByRole("link", { name: "delivery settings" })).toHaveAttribute(
    "href",
    `/projects/${NO_SOURCE}/settings`
  );
  await expect(page.locator("main [data-section]")).toHaveCount(0);
});

// SLOW(>=2s): seeds every source and opens the tab, its job lists, its timeline and a drill-down.
test("what the Delivery and Pipeline pages drew stands on the tab @evals k6", async ({ page }) => {
  await seedEverySource();
  await openTab(page, EVERY_SOURCE, TAB_QUERY);

  // The Pipeline page's four timing cards and its two job lists.
  const ci = await paintedSection(page, "ci");
  for (const slug of ["pr-ci", "staging-e2e", "staging-deploy", "production-deploy"]) {
    await expect(chartFigure(ci.locator(`[data-eval="${slug}"]`), slug), slug).toBeVisible();
  }
  await expect(
    ci.getByRole("table", { name: "PR CI: slowest jobs" }).locator('[data-row="build / unit"]')
  ).toBeVisible();
  await expect(
    ci
      .getByRole("table", { name: "PR CI: jobs that failed in the last 24 hours" })
      .locator("[data-row]")
      .first()
  ).toBeVisible();

  // The Delivery page's measures, each with its definition, and its freshness line.
  const deploys = await paintedSection(page, "deploys");
  for (const slug of ["deploy-frequency", "merge-to-production", "rework-share"]) {
    await expect(chartFigure(deploys.locator(`[data-eval="${slug}"]`), slug), slug).toBeVisible();
    await expect(deploys.locator(`[data-method="${slug}"]`), slug).toBeVisible();
  }
  await expect(page.getByRole("region", { name: "Source freshness" })).toBeVisible();

  // Its timeline, its list and a deploy's drill-down with the run's jobs.
  const timeline = await openMergeTimeline(page);
  await expect(timeline.getByTestId("delivery-chart")).toBeVisible();
  await timeline.getByRole("button", { exact: true, name: "List" }).click();
  await timeline.getByRole("button", { name: "Open deploy run #501" }).first().click();
  const details = page.getByRole("dialog", { name: "Details" });
  await expect(details.getByRole("heading", { name: "Deploy: run #501" })).toBeVisible();
  await expect(details.getByText("widgets-release / widgets-release").first()).toBeVisible();
});

// SLOW(>=2s): seeds every source and turns every target card to its table, sorting each column.
test("every chart takes Graph or Table and Compare, and its table holds what it draws @evals k7", async ({
  page,
}) => {
  await seedEverySource();
  await openTab(page, EVERY_SOURCE, TAB_QUERY);
  const cards = await paintedSection(page, "cards");
  const figures = cards.locator("[data-card] figure[data-chart]");
  await expect.poll(() => figures.count()).toBeGreaterThan(0);

  // Read after the poll above has seen the cards' charts drawn, so every card's chart is walked.
  for (const figure of await figures.all()) {
    await expect(figure.getByRole("button", { exact: true, name: "Graph" })).toBeVisible();
    await expect(figure.getByRole("checkbox", { name: "Compare" })).toBeChecked();
    const drawn = await graphValues(figure);
    await showChart(figure, "Table");
    const table = figure.getByRole("table");
    const tabled = await table
      .locator("[data-value]")
      .evaluateAll((values) => values.map((value) => value.getAttribute("data-value")));
    expect(drawn.filter((value) => !tabled.includes(value))).toEqual([]);
    expect(await headersCut(table), "unsorted").toEqual([]);
    // Read after the table's headers were measured above, so its sortable columns are drawn.
    for (const sort of await table.locator("th button[title^='Sort']").all()) {
      await sort.click();
      expect(await headersCut(table), "sorted").toEqual([]);
    }
  }

  // The merge timeline takes Graph or Table and no compare range (spec R7).
  const timeline = await openMergeTimeline(page);
  const merges = chartFigure(timeline, "timeline");
  await expect(merges.getByRole("button", { exact: true, name: "Table" })).toBeVisible();
  await expect(merges.getByRole("checkbox", { name: "Compare" })).toHaveCount(0);
  await showChart(merges, "Table");
  await expect(merges.getByRole("table", { name: "Merge timeline rows" })).toBeVisible();
});

// SLOW(>=2s): seeds every source and opens one group link of each kind, each on a tab of its own.
test("the tab lists no issue, and each group number opens the issues it counts @evals k8", async ({
  page,
}) => {
  await seedEverySource();
  await expectLegionWork();
  await showGroupLinks(page);
  await expect(page.locator("[data-issue-row]")).toHaveCount(0);
  await expect(page.getByRole("table", { name: /^Issues/ })).toHaveCount(0);
  // Read after groupLinks' own poll has seen at least ten links drawn, so each kind is found.
  const links = await groupLinks(page, 10);
  const kinds = [
    links.find(
      (link) => link.selector === '[data-eval="legion-work-time"] > div > button[data-group-count]'
    ),
    links.find((link) => link.selector.startsWith('[data-eval="legion-rework"] tr')),
    links.find((link) => link.selector.startsWith('[data-eval="legion-phase-time"] tr')),
  ];
  for (const link of kinds) {
    if (link === undefined)
      throw new Error(`a kind of group link is missing among ${links.length}`);
    await showGroupLinks(page);
    await openGroupLink(page, link);
  }
  // The P0 card's link, over the days from its input's first day (`showP0Link`).
  await openGroupLink(page, await showP0Link(page));
});

// SLOW(>=2s): seeds every source and a logged eval, and opens its rows and its panel.
test("links out draw by their relation in all three places and fetch nothing @evals k10", async ({
  page,
}) => {
  await seedEverySource();
  await seedRelations(EVERY_SOURCE);
  const record = recordLive(page);
  await openTab(page, EVERY_SOURCE, TAB_QUERY);
  const logged = await paintedSection(page, "logged");
  const box = logged.locator(`[data-eval="${RELATIONS_EVAL.slug}"]`);

  // The reading as logged, beside its own row, with the link whose target is gone drawn as any.
  await box.getByRole("button", { exact: true, name: "Rows" }).click();
  const reading = box.locator("[data-reading]");
  await expectEveryRelation(reading, "the reading as logged");
  await expect(reading.locator(`a[href="${GONE_LINK.href}"]`)).toHaveAttribute(
    "data-link-kind",
    GONE_LINK.rel
  );

  // The panel: the eval's links and its primary series' links, and the rows behind the number.
  await box.getByRole("button", { exact: true, name: RELATIONS_EVAL.name }).click();
  const panel = page.getByRole("dialog", { name: RELATIONS_EVAL.name });
  await expectEveryRelation(panel.getByRole("list", { name: "Links" }), "the panel");
  await expectEveryRelation(panel.locator("[data-eval-row]"), "the rows");

  // Nothing the page drew fetched a link's target: every request it made is Dispatch's own.
  const own = new URL(baseUrl).origin;
  expect(
    record.requests
      .filter(({ url }) => url.protocol.startsWith("http") && url.origin !== own)
      .map(({ url }) => url.href)
  ).toEqual([]);
});

// SLOW(>=2s): seeds two projects and opens the cross-project page.
test("every project's numbers are one read of the cross-project page @evals k11", async ({
  page,
}) => {
  await seedEverySource();
  await seedNoDaemon();
  const record = recordLive(page);
  await page.goto(`/evals?${TAB_QUERY}`);
  for (const project of [EVERY_SOURCE, NO_DAEMON]) {
    const section = page.locator(`section[data-project="${project}"]`);
    await expect(section.getByRole("link", { exact: true, name: project })).toHaveAttribute(
      "href",
      `/projects/${project}/evals?${TAB_QUERY}`
    );
    await expect(section.locator("[data-eval]").first()).toBeVisible();
  }
  // Each number with its eval's answer beside it.
  await expect(
    page.locator(
      `section[data-project="${EVERY_SOURCE}"] [data-eval="${DECLARED_EVAL.slug}"] [data-answer]`
    )
  ).toHaveText(DECLARED_EVAL.answer);
  expect(record.requests.filter(({ url }) => url.pathname === "/api/v1/evals")).toHaveLength(1);

  // The cross-project read's items are the project read's, field for field.
  const across = await getJson<EvalListResponse>(`/api/v1/evals?${TAB_QUERY}&limit=200`);
  expect(new Set((across.items ?? []).map((item) => item.definition.project_key))).toEqual(
    new Set([EVERY_SOURCE, NO_DAEMON])
  );
  // Read again until both answers stand on the same day rows: the refresher writes a day's
  // explicit zero row on its own pass, which can land between two reads.
  for (const slug of [DECLARED_EVAL.slug, "deploy-frequency", "merge-to-production"]) {
    const pick = (answer: EvalListResponse): EvalListItem | undefined =>
      (answer.items ?? []).find(
        (item) => item.definition.project_key === EVERY_SOURCE && item.definition.slug === slug
      );
    await expect
      .poll(async () => {
        const [crossed, own] = await Promise.all([
          getJson<EvalListResponse>(`/api/v1/evals?${TAB_QUERY}&limit=200`),
          listEvals(EVERY_SOURCE, TAB_WINDOW),
        ]);
        return (
          pick(own) !== undefined && JSON.stringify(pick(crossed)) === JSON.stringify(pick(own))
        );
      }, slug)
      .toBe(true);
  }
});

// SLOW(>=2s): seeds every source, opens the tab, its timeline, a panel and a drill-down.
test("the tab reads no old delivery route or view, which still answer @evals k13", async ({
  page,
}) => {
  await seedEverySource();
  const record = recordLive(page);
  await openTab(page, EVERY_SOURCE, TAB_QUERY);
  const cards = await paintedSection(page, "cards");
  for (const id of ["legion", "ci", "deploys", "logged"]) await paintedSection(page, id);
  await cards
    .locator('[data-card="deploy-frequency"]')
    .getByRole("button", { exact: true, name: "Deploys a day" })
    .click();
  await expect(page.locator('[data-eval-panel="deploy-frequency"]')).toBeVisible();
  await page.getByRole("button", { name: "Close the panel" }).click();
  const timeline = await openMergeTimeline(page);
  await timeline.getByRole("button", { exact: true, name: "List" }).click();
  await timeline.getByRole("button", { name: "Open deploy run #501" }).first().click();
  await expect(page.getByRole("dialog", { name: "Details" })).toBeVisible();

  // No request of the old routes and no view of the old kinds (decision 1).
  expect(
    record.requests
      .filter(({ url }) =>
        /^\/api\/v1\/delivery\/(measures|pipeline|timeline|runs\/)/.test(url.pathname)
      )
      .map(({ url }) => url.pathname)
  ).toEqual([]);
  expect(
    subscribedViews(record.sent)
      .filter((view) => ["measures", "pipeline", "timeline"].includes(view.kind))
      .map((view) => view.kind)
  ).toEqual([]);

  // Every card the tab drew is an eval one of its evals views holds.
  const held = new Set(
    evalsViews(record, EVERY_SOURCE).flatMap((view) =>
      [...evalsViewItems(record, view.sub).values()].map((item) => item.definition.slug)
    )
  );
  const drawn = await cards
    .locator("[data-card]")
    .evaluateAll((boxes) => boxes.map((box) => box.getAttribute("data-card") ?? ""));
  expect(drawn.length).toBeGreaterThan(0);
  expect(drawn.filter((slug) => !held.has(slug))).toEqual([]);

  // The old routes still answer, for the open tabs of the build before (decision 1).
  await getJson<object>(`/api/v1/delivery/measures?${TAB_QUERY}`);
  await getJson<object>("/api/v1/delivery/pipeline");
  await getJson<object>(`/api/v1/delivery/timeline?${TAB_QUERY}`);
  await getJson<object>("/api/v1/delivery/runs/501");
});

/** A logged eval the live rows write to (k14, k16): its readings' window is TAB_WINDOW's. */
const LIVE_EVAL = {
  name: "Widget throughput",
  series: "throughput",
  slug: "e2e-widget-throughput",
};

/** LIVE_EVAL's readings, one a day. */
function liveReadings(values: readonly number[], firstDay = 10) {
  return values.map((value, index) => ({
    measured_at: `2026-09-${String(firstDay + index).padStart(2, "0")}T12:00:00Z`,
    row_kind: "point",
    series: LIVE_EVAL.series,
    value,
  }));
}

/** Marks the page's document, so a later read of the mark says the page has not been loaded
 *  again since. */
async function markDocument(page: Page): Promise<void> {
  await page.evaluate(() => {
    document.documentElement.dataset.evalsRowMark = "kept";
  });
}

/** The mark `markDocument` left, or nothing on a document loaded since. */
function documentMark(page: Page): Promise<string | undefined> {
  return page.evaluate(() => document.documentElement.dataset.evalsRowMark);
}

// SLOW(>=2s): seeds every source and a logged eval, then writes a reading and a version of it.
test("a reading and a register write reach the open tab within 2 s on one subscription @evals k14", async ({
  page,
}) => {
  await seedEverySource();
  await logReadings(EVERY_SOURCE, LIVE_EVAL.slug, liveReadings([12, 14, 13]));
  const record = recordLive(page);
  await openTab(page, EVERY_SOURCE, TAB_QUERY);
  const box = (await paintedSection(page, "logged")).locator(`[data-eval="${LIVE_EVAL.slug}"]`);
  // A day's value as the chart's table holds it: a series of agg `last` that no step chart draws
  // answers no value over a window (plan D20 (b)), while each day holds its newest reading.
  const figure = chartFigure(box, LIVE_EVAL.slug);
  await showChart(figure, "Table");
  const day = (x: string) =>
    figure.locator(`[data-x="${x}"][data-statistic="value"][data-range="window"]`);
  await expect(day("2026-09-12")).toHaveAttribute("data-value", "13");
  await expect(box.locator("[data-eval-facts]")).toHaveText("no target");
  await markDocument(page);
  const before = evalsViews(record, EVERY_SOURCE).map((view) => view.sub);

  await logReadings(EVERY_SOURCE, LIVE_EVAL.slug, liveReadings([21], 25));
  await expect(day("2026-09-25")).toHaveAttribute("data-value", "21", { timeout: 2_000 });
  await registerEval(EVERY_SOURCE, {
    direction: "higher_better",
    slug: LIVE_EVAL.slug,
    target: 20,
  });
  await expect(box.locator("[data-eval-facts]")).toContainText("target", { timeout: 2_000 });
  await expect(box.locator("[data-eval-facts]")).not.toHaveText("no target");

  // One subscription each, the same before and after, and no load of the page.
  expect(evalsViews(record, EVERY_SOURCE).map((view) => view.sub)).toEqual(before);
  expect(await documentMark(page)).toBe("kept");
});

/** The method LIVE_EVAL's register writes give it, each a section of the project's evals-md. */
const METHOD = "Count the widgets each hour and take the day's median of the hourly counts.";
const NEW_METHOD = "Count the widgets each minute and take the day's mean of the minute counts.";

// SLOW(>=2s): seeds every source and a logged eval with a method, opens its panel and writes it.
test("the panel's method is the eval's evals-md section, and a write of it reaches the panel @evals k15", async ({
  page,
}) => {
  await seedEverySource();
  await logReadings(EVERY_SOURCE, LIVE_EVAL.slug, liveReadings([12, 14, 13]));
  await registerEval(EVERY_SOURCE, {
    doc_text: METHOD,
    name: LIVE_EVAL.name,
    slug: LIVE_EVAL.slug,
  });
  const document = await getProjectArtifact(EVERY_SOURCE, "evals-md");
  expect((await getArtifactText(document.id)).markdown).toContain(METHOD);

  await openTab(page, EVERY_SOURCE, TAB_QUERY);
  const box = (await paintedSection(page, "logged")).locator(`[data-eval="${LIVE_EVAL.slug}"]`);
  await box.getByRole("button", { exact: true, name: LIVE_EVAL.name }).click();
  const panel = page.getByRole("dialog", { name: LIVE_EVAL.name });
  await expect(panel.locator("[data-method-text]")).toHaveText(METHOD);
  await markDocument(page);

  await registerEval(EVERY_SOURCE, { doc_text: NEW_METHOD, slug: LIVE_EVAL.slug });
  await expect(panel.locator("[data-method-text]")).toHaveText(NEW_METHOD);
  expect((await getArtifactText(document.id)).markdown).toContain(NEW_METHOD);
  expect(await documentMark(page)).toBe("kept");
});

// SLOW(>=2s): seeds every source, logs a new eval's readings, opens its rows and registers it.
test("a logged eval shows with no code of its own, before and after its register write @evals k16", async ({
  page,
}) => {
  await seedEverySource();
  await logReadings(EVERY_SOURCE, LIVE_EVAL.slug, liveReadings([12, 14, 13]));
  await openTab(page, EVERY_SOURCE, TAB_QUERY);
  const logged = await paintedSection(page, "logged");
  const box = logged.locator(`[data-eval="${LIVE_EVAL.slug}"]`);

  /** The eval's chart, its table's values and its opened rows, under `name`. */
  const expectDrawn = async (name: string) => {
    const figure = chartFigure(box, LIVE_EVAL.slug);
    await expect(figure.getByRole("button", { exact: true, name })).toBeVisible();
    await showChart(figure, "Table");
    await expect(
      figure.getByRole("table").locator('[data-range="window"][data-statistic="value"][data-value]')
    ).toHaveText(["12", "14", "13"]);
    await showChart(figure, "Graph");
    await expect(
      box.getByRole("list", { name: `Rows of ${name}` }).locator("[data-eval-row]")
    ).toHaveCount(3);
  };

  // Before the register write: named by its slug, no unit, no target; its rows opened (D25 step 4).
  await expect(box.locator("[data-eval-facts]")).toHaveText("no target");
  await box.getByRole("button", { exact: true, name: "Rows" }).click();
  await expectDrawn(LIVE_EVAL.slug);

  // After it: its name, its unit and its target, on the same open tab.
  await registerEval(EVERY_SOURCE, {
    direction: "higher_better",
    name: LIVE_EVAL.name,
    slug: LIVE_EVAL.slug,
    target: 10,
    unit: "widgets",
  });
  await expect(box).toHaveAttribute("aria-label", LIVE_EVAL.name);
  await expect(box.locator("[data-eval-facts]")).toContainText("in widgets \u00b7 target");
  await expectDrawn(LIVE_EVAL.name);

  // Every eval the list route answers is on the tab: a card, a section's chart or a logged box.
  const listed = await listEvals(EVERY_SOURCE, TAB_WINDOW);
  const slugs = (listed.items ?? []).map((item) => item.definition.slug);
  expect(slugs).toContain(LIVE_EVAL.slug);
  await expect
    .poll(() =>
      page
        .locator("main [data-eval], main [data-card]")
        .evaluateAll(
          (boxes, wanted) =>
            wanted.filter(
              (slug) =>
                !boxes.some(
                  (drawn) =>
                    drawn.getAttribute("data-eval") === slug ||
                    drawn.getAttribute("data-card") === slug
                )
            ),
          slugs
        )
    )
    .toEqual([]);
});

/** What k17 writes into the deploy record. */
const FLAG_NOTE = "Checkout broke for every widget after this deploy.";
const REJECT_REASON = "The check failed on a flaky probe, not on this deploy.";

/** The drill-down of deploy run `id`, opened from the merge list. */
async function openDeployRun(timeline: Locator, page: Page, id: number): Promise<Locator> {
  await timeline
    .getByRole("button", { name: `Open deploy run #${id}` })
    .first()
    .click();
  const details = page.getByRole("dialog", { name: "Details" });
  await expect(details.getByRole("heading", { name: `Deploy: run #${id}` })).toBeVisible();
  return details;
}

// SLOW(>=2s): seeds a deploy record and writes a flag, a restore and a reject through its controls.
test("the drill-down's controls flag, restore and reject, and the failure measures count them @evals k17", async ({
  page,
}) => {
  const record = await seedDeployRecord(true);
  // Seven days: the window starts after the failure measures' first day, so they read a value.
  const timeline = await openEvalsTimeline(page, "window=7d&mode=list", DEPLOY_RECORD);
  const cards = await paintedSection(page, "cards");
  // The deploys with a counted failure, as the change failure rate's card writes them.
  const failed = cards.locator(
    '[data-card="change-fail-rate"] [data-window-series="per_deploy"] [data-window-statistic="failed"]'
  );
  await expect(failed).toHaveText("1");

  // Flag: the pull request it shipped ticked, and a note; a person's failure, which counts.
  let details = await openDeployRun(timeline, page, record.flagRun);
  await details.getByRole("button", { exact: true, name: "Flag" }).click();
  const flag = details.getByRole("form", { name: "Flag this deploy" });
  await flag.getByRole("checkbox").first().check();
  await flag.getByLabel("Note").fill(FLAG_NOTE);
  await flag.getByRole("button", { name: "Flag this deploy" }).click();
  const failures = details.getByRole("list", { name: "Failures of this deploy" });
  const person = failures.locator('li[data-signal="person"]');
  await expect(person).toHaveAttribute("data-state", "counted");
  await expect(person.locator("[data-note]")).toHaveText(`Note: ${FLAG_NOTE}`);
  await expect(failed).toHaveText("2");

  // Mark restored: the failure's restore, which the recovery time reads.
  await person.getByRole("button", { name: "Mark restored" }).click();
  await expect(person.locator("[data-restored]")).toContainText("by alice");
  await expect(
    cards.locator(
      '[data-card="failed-deploy-recovery"] [data-window-series="minutes"] [data-window-statistic="median"]'
    )
  ).toHaveText(/\d/);
  await details.getByRole("button", { name: "Close details" }).click();

  // Reject: the check failure stays listed with its reason and writer, and counts no more.
  details = await openDeployRun(timeline, page, record.failedRun);
  const check = details
    .getByRole("list", { name: "Failures of this deploy" })
    .locator('li[data-signal="check_fail"]');
  await check.getByRole("button", { exact: true, name: "Reject" }).click();
  const reject = check.getByRole("form", { name: "Reject this failure" });
  await reject.getByLabel("Reason").fill(REJECT_REASON);
  await reject.getByRole("button", { name: "Reject this failure" }).click();
  await expect(check).toHaveAttribute("data-state", "rejected");
  await expect(check.locator("[data-rejected]")).toHaveText(`Rejected by alice: ${REJECT_REASON}`);
  await expect(failed).toHaveText("1");
});

/** The cards and the section charts that count DEFAULT_REPO's two pull requests of no issue. */
const DEFAULT_REPO_CARDS = ["merge-to-production", "pr-open-to-merge"] as const;
const DEFAULT_REPO_CHARTS = [
  "merge-to-production",
  "first-commit-to-production",
  "open-to-production",
  "pr-open-to-merge",
  "rework-share",
] as const;
/** Of DEFAULT_REPO_CHARTS, the lead times, whose day rows count the pull requests they time. */
const LEAD_TIMES = DEFAULT_REPO_CHARTS.filter((slug) => slug !== "rework-share");

// SLOW(>=2s): seeds a default repository's project and opens its tab on two windows.
test("a number that counts rows by the default repository says how many @evals k18", async ({
  page,
}) => {
  await seedDefaultRepository();
  const [first, second] = ["2026-10-20", "2026-10-21"];
  const note = "2 by default repository";

  // Both days: the two pull requests of no issue, on every number that counts them.
  await openTab(
    page,
    DEFAULT_REPO,
    `${windowQuery({ from: `${first}T00:00:00Z`, to: "2026-10-22T00:00:00Z" })}&compare=off`
  );
  const cards = await paintedSection(page, "cards");
  for (const slug of DEFAULT_REPO_CARDS) {
    await expect(cards.locator(`[data-card="${slug}"] [data-note="repository"]`), slug).toHaveText(
      note
    );
  }
  const deploys = await paintedSection(page, "deploys");
  for (const slug of DEFAULT_REPO_CHARTS) {
    await expect(
      deploys.locator(`[data-eval="${slug}"] [data-note="repository"]`),
      slug
    ).toHaveText(note);
  }
  // Each lead time's day rows: the issue's pull request on the first day, the two on the second.
  for (const slug of LEAD_TIMES) {
    const figure = chartFigure(deploys.locator(`[data-eval="${slug}"]`), slug);
    await showChart(figure, "Table");
    const count = (day: string) =>
      figure.locator(`[data-x="${day}"][data-statistic="n"][data-range="window"]`);
    await expect(count(first), slug).toHaveAttribute("data-value", "1");
    await expect(count(second), slug).toHaveAttribute("data-value", "2");
  }

  // The first day alone counts none of them: no number says so.
  await openTab(
    page,
    DEFAULT_REPO,
    `${windowQuery({ from: `${first}T00:00:00Z`, to: `${second}T00:00:00Z` })}&compare=off`
  );
  const oneDay = await paintedSection(page, "cards");
  await expect(
    oneDay.locator('[data-card="merge-to-production"] [data-window-statistic="median"]')
  ).toHaveText(/\d/);
  await paintedSection(page, "deploys");
  await expect(page.locator('main [data-note="repository"]')).toHaveCount(0);
});

// SLOW(>=2s): seeds ten days of deploys and opens the tab on the 28-day and the 7-day window.
test("a window that starts before the input names its first day, and one after reads a value @evals k20", async ({
  page,
}) => {
  const record = await seedDeployRecord(false);
  const deploys = (cards: Locator) =>
    cards.locator(`[data-card="${DELIVERY_CARD}"] [data-window-series="deploys"]`);

  // 28 days: the input began ten days before the window's end.
  await openTab(page, DEPLOY_RECORD);
  let cards = await paintedSection(page, "cards");
  await expect(deploys(cards).locator('[data-note="data"]')).toHaveText(
    `no data before ${record.since}`
  );

  // 7 days: no note, a number of deploys, and a failure rate of none, which reads 0%, not "no data".
  await openTab(page, DEPLOY_RECORD, "window=7d");
  cards = await paintedSection(page, "cards");
  await expect(deploys(cards).locator('[data-window-statistic="deploys"]')).toHaveText(/^\d+$/);
  await expect(deploys(cards).locator('[data-note="data"]')).toHaveCount(0);
  await expect(
    cards.locator('[data-card="change-fail-rate"] [data-window-statistic="rate"]')
  ).toHaveText(/^0(\.0+)?%$/);
});

// SLOW(>=2s): seeds more merges than two pages and scrolls the timeline to its last row.
test("the first paint holds no timeline row, which loads newest first a page at a time @evals k22", async ({
  page,
}) => {
  await seedPagedTimeline();
  const record = recordLive(page);
  await openTab(page, PAGED, windowQuery(PAGED_WINDOW));
  await paintedSection(page, "cards");
  await paintedSection(page, "deploys");

  // The first paint subscribes no project-timeline view and requests no timeline.
  const timelineViews = () =>
    subscribedViews(record.sent).filter((view) => view.kind === "project-timeline");
  expect(timelineViews()).toEqual([]);
  expect(record.requests.filter(({ url }) => url.pathname.endsWith("/timeline"))).toEqual([]);
  await expect(evalsSection(page, "timeline")).toHaveAttribute("data-state", "closed");
  await expect(page.locator("[data-timeline-end]")).toHaveCount(0);

  // Opened, it holds the newest page first; scrolled to its end, it grows a page at a time.
  const timeline = await openMergeTimeline(page);
  const end = timeline.locator("[data-timeline-end]");
  await expect
    .poll(
      async () => {
        await end.scrollIntoViewIfNeeded();
        return end.textContent();
      },
      { timeout: 30_000 }
    )
    .toBe(`All ${PAGED_MERGES} rows of the window loaded.`);
  const views = timelineViews();
  expect(new Set(views.map((view) => view.sub)).size).toBe(1);
  expect(views.map((view) => Number(view.query.get("limit")))).toEqual([200, 400, PAGED_MERGES]);
  const first = snapshotsOf(record.received, views[0]?.sub ?? "")[0]?.frame
    .body as ProjectTimelineResponse;
  const merged = (first.items ?? []).map((row) => (row.kind === "merge" ? row.merged_at : null));
  expect(first.total).toBe(PAGED_MERGES);
  expect(merged).toHaveLength(200);
  const newestPage = Date.parse(pagedMergedAt(PAGED_MERGES - 199));
  expect(merged.filter((at) => at === null || Date.parse(at) < newestPage)).toEqual([]);
  expect((await readTimeline(PAGED, PAGED_WINDOW, 1)).total).toBe(PAGED_MERGES);
});

/** The merge timeline's facets as the tab's address names each (`FACET_KEYS`) and as the routes
 *  take it (`FACET_PARAMS`, web/src/features/delivery/lib/facets.ts), with the name a narrowed
 *  chart gives it (`narrowing.tsx`); and the search, `q`. */
const FACETS = [
  { name: "Parent agent", param: "parent_agent", url: "parentAgent" },
  { name: "Session", param: "session", url: "session" },
  { name: "Dispatch issue", param: "issue", url: "issue" },
  { name: "Priority", param: "priority", url: "priority" },
  { name: "Component", param: "component", url: "component" },
  { name: "Author", param: "author", url: "author" },
  { name: "Rework", param: "rework", url: "rework" },
  { name: "Deployed", param: "deployed", url: "deployed" },
  { name: "Search", param: "q", url: "q" },
  // Last: the Repository facet narrows every number through the header's repository.
  { name: "", param: "repo", url: "repo" },
] as const;

/** The three numbers k24 reads, as the tab's evals view and the base's measures name them. */
interface Narrowed {
  readonly mergeToProduction: number | null;
  readonly openToMerge: number | null;
  readonly reworkShare: number | null;
}

/** Where k24 reads the base's measures: the base server a Job names (`EVALS_K24_BASE_URL`, with
 *  `EVALS_K24_BASE_TOKEN` naming the file of its bearer), else this server's own old route, which
 *  answers as the base's did (decision 1). The base server reads the database this row seeds. */
const BASE_URL = process.env.EVALS_K24_BASE_URL ?? "";

/** The window k24 compares the numbers over: from the first day of EVERY_SOURCE's pull-request
 *  input, 2026-09-05 (the measures fixture's first merge), to TAB_WINDOW's end. A number reads "no
 *  data before <day>" over a window that starts before its input began (spec R18), which the base's
 *  measures route does not, so over TAB_WINDOW, five days earlier, every narrowed number of the
 *  tab reads no value where the base's reads one. */
const K24_WINDOW = { from: "2026-09-05T00:00:00Z", to: TAB_WINDOW.to } as const;
const K24_QUERY = windowQuery(K24_WINDOW);

/** The base's measures over K24_WINDOW with `query` (a facet's value, or nothing). */
async function baseMeasures(token: string, query: string): Promise<Narrowed> {
  const answered = await fetch(
    new URL(`/api/v1/delivery/measures?${K24_QUERY}${query}`, BASE_URL === "" ? baseUrl : BASE_URL),
    { headers: { Authorization: `Bearer ${token}` } }
  );
  const text = await answered.text();
  expect(answered.status, `the base's measures${query}: ${text}`).toBe(200);
  // The measures route's answer, as the contract names it.
  const { measures } = JSON.parse(text) as DeliveryMeasuresResponse;
  return {
    mergeToProduction: measures.lead_time.merge_to_production.median_minutes,
    openToMerge: measures.lead_time.opened_to_merge.median_minutes,
    reworkShare: measures.rework_share,
  };
}

/** The three numbers EVERY_SOURCE's tab draws with `facet` set to `value`, read on a page of its
 *  own from the delivery view that names the facet (or, for the Repository facet, the
 *  repository), once it has landed: the view the cards and the Deploys and lead time section
 *  draw. With any facet but the repository, the pull-request cards are marked narrowed by it and
 *  every other card reads "window and repository only" (plan D3). */
async function narrowedOnTheTab(
  page: Page,
  facet: (typeof FACETS)[number],
  value: string
): Promise<Narrowed> {
  const tab = await page.context().newPage();
  try {
    const record = recordLive(tab);
    await openTab(tab, EVERY_SOURCE, `${K24_QUERY}&${facet.url}=${encodeURIComponent(value)}`);
    const cards = await paintedSection(tab, "cards");
    const what = `${facet.param}=${value}`;
    if (facet.param !== "repo") {
      await expect(
        cards.locator('[data-card="merge-to-production"] [data-narrowing="narrowed"]'),
        what
      ).toHaveText(`narrowed by ${facet.name}`);
      await expect(
        cards.locator(`[data-card="${DELIVERY_CARD}"] [data-narrowing="window-only"]`),
        what
      ).toHaveText("window and repository only");
    }
    const landed = () =>
      evalsViews(record, EVERY_SOURCE).find(
        (view) =>
          view.query.get("kind") === "delivery" &&
          view.query.getAll(facet.param).includes(value) &&
          snapshotsOf(record.received, view.sub).length > 0
      );
    await expect.poll(() => landed() !== undefined, what).toBe(true);
    const items = evalsViewItems(record, landed()?.sub ?? "");
    return {
      mergeToProduction: windowFigure(items, "merge-to-production", "minutes", "median"),
      openToMerge: windowFigure(items, "pr-open-to-merge", "minutes", "median"),
      reworkShare: windowFigure(items, "rework-share", "prs", "rate"),
    };
  } finally {
    await tab.close();
  }
}

// SLOW(>=2s): seeds every source and opens the tab once for each facet and the search.
test("a facet narrows the pull-request numbers as the base's measures do, and a write moves them @evals k24", async ({
  page,
}, testInfo) => {
  // One tab is opened for each of the nine facets and the search.
  test.setTimeout(180_000);
  await seedEverySource();
  // The window's numbers stand built before any is compared (a window over days the walk has not
  // built reads no value).
  await expectNumberStands(
    EVERY_SOURCE,
    K24_WINDOW,
    "delivery",
    "pr-open-to-merge",
    "minutes",
    "median"
  );
  const tokenFile = process.env.EVALS_K24_BASE_TOKEN ?? "";
  const token =
    tokenFile === ""
      ? await mintAgentToken("evals-k24", "alice")
      : readFileSync(tokenFile, "utf8").trim();
  const unfiltered = await baseMeasures(token, "");
  const timeline = await readTimeline(EVERY_SOURCE, K24_WINDOW, 200);
  const titles = (timeline.items ?? []).flatMap((row) => (row.kind === "merge" ? [row.title] : []));

  const checked: string[] = [];
  const unmatched: string[] = [];
  for (const facet of FACETS) {
    const candidates =
      facet.param === "q"
        ? [...new Set(titles.map((title) => (title.split(/[\s:]/)[0] ?? "").toLowerCase()))]
        : Object.keys(timeline.facet_counts?.[facet.param] ?? {});
    let chosen: { value: string; base: Narrowed } | null = null;
    for (const value of candidates) {
      const base = await baseMeasures(token, `&${facet.param}=${encodeURIComponent(value)}`);
      if (
        base.mergeToProduction !== unfiltered.mergeToProduction ||
        base.openToMerge !== unfiltered.openToMerge ||
        base.reworkShare !== unfiltered.reworkShare
      ) {
        chosen = { base, value };
        break;
      }
    }
    if (chosen === null) {
      unmatched.push(facet.param);
      continue;
    }

    const drawn = await narrowedOnTheTab(page, facet, chosen.value);
    // The list route the evals view mirrors, narrowed the same way: which of the two a difference
    // from the base stands in.
    const listed = await listEvals(
      EVERY_SOURCE,
      K24_WINDOW,
      `&kind=delivery&${facet.param}=${encodeURIComponent(chosen.value)}`
    );
    const route = new Map((listed.items ?? []).map((item) => [item.id, item]));
    const routed: Narrowed = {
      mergeToProduction: windowFigure(route, "merge-to-production", "minutes", "median"),
      openToMerge: windowFigure(route, "pr-open-to-merge", "minutes", "median"),
      reworkShare: windowFigure(route, "rework-share", "prs", "rate"),
    };
    for (const key of ["mergeToProduction", "openToMerge", "reworkShare"] as const) {
      const what = `${facet.param}=${chosen.value} ${key} (the list route answers ${routed[key]}, the base ${chosen.base[key]})`;
      // Soft, so every facet's numbers are read and every one that differs is named.
      expect.soft(drawn[key] === null, `the tab's view: ${what}`).toBe(chosen.base[key] === null);
      expect.soft(drawn[key] ?? 0, `the tab's view: ${what}`).toBeCloseTo(chosen.base[key] ?? 0, 3);
    }
    checked.push(facet.param);
  }
  testInfo.annotations.push({
    description: `checked ${checked.join(", ")}; no value differs on the base for ${unmatched.join(", ") || "none"}`,
    type: "k24 facets",
  });
  // T27's one-project fixture, which a Job reads through the base server, holds a differing value
  // of every facet and of the search; this row's own seed holds one of each of these.
  if (BASE_URL !== "") expect(unmatched).toEqual([]);
  expect(checked).toEqual(expect.arrayContaining(["repo", "issue", "priority", "rework", "q"]));

  // A write moves the narrowed numbers within 2 s: one more rework pull request of the window.
  await openTab(page, EVERY_SOURCE, `${K24_QUERY}&rework=rework`);
  const deploys = await paintedSection(page, "deploys");
  const timed = deploys.locator(
    '[data-eval="pr-open-to-merge"] [data-window-series="minutes"] [data-window-statistic="n"]'
  );
  await expect(
    deploys.locator('[data-eval="pr-open-to-merge"] [data-narrowing="narrowed"]')
  ).toHaveText("narrowed by Rework");
  await expect(timed).toHaveText(/^\d+$/);
  // Read once the narrowing mark above says the narrowed view has landed, so it is its count.
  const before = Number(await timed.textContent());
  await sql(
    `INSERT INTO delivery_pull_requests (
       repo, number, title, url, author, created_at, merged_at, first_commit_at,
       additions, deletions, rework, issue_key, sessions, partial
     ) VALUES ('acme/widgets', 114, 'fix: widget rework after review',
       'https://github.com/acme/widgets/pull/114', 'octocat', '2026-09-20T08:00:00Z',
       '2026-09-20T10:00:00Z', '2026-09-20T07:00:00Z', 6, 2, true, 'ACME-100', ARRAY[]::text[], false)`
  );
  await expect(timed).toHaveText(String(before + 1), { timeout: 2_000 });
});
