import { afterEach, beforeEach, test as bunTest, expect, spyOn } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { deliverySettings } from "../../__tests__/delivery-settings";
import { installIntersectionObserver } from "../../__tests__/intersection-observer";
import {
  errorFrame,
  type FakeLiveSocket,
  type FakeLiveSockets,
  installFakeLiveSockets,
  snapshotFrame,
} from "../../__tests__/live-socket-harness";
import {
  limitOf,
  timelineAnswer,
  timelineGrowth,
  timelineRows,
  timelineSnapshot,
} from "../../__tests__/timeline-fixture";
import { api } from "../../api/client";
import type { EvalKind } from "../../api/queries";
import type {
  DeliveryLiveDelta,
  EvalDetailResponse,
  EvalItemRow,
  EvalItemsResponse,
  EvalListItem,
  EvalListResponse,
  LegionSetting,
  LiveServerFrame,
  LiveSubscribe,
  ProjectDeliverySettings,
} from "../../api/types";
import { applyRevs } from "../delivery/lib/applyDelta";
import * as timeline from "../delivery/Timeline";
import bandReadings from "./charts/__fixtures__/band-readings.json";
import xyReadings from "./charts/__fixtures__/xy-readings.json";
import * as sections from "./EvalsSection";
import { EvalsTab } from "./EvalsTab";

// The state table of a section (dispatch://INFRA-2918/artifact/plan-md, "The state table of a
// section"): each row a test driven by a fake live socket that sends the frames in the order the
// row names.

const DAEMON: LegionSetting = {
  created_by: { id: "alice", kind: "user" },
  project: "ACME",
  state_url: "http://127.0.0.1:9",
  updated_at: "2026-10-01T00:00:00Z",
};

const WINDOW = { days: 29, from: "2026-10-26T00:00:00Z", to: "2026-11-24T00:00:00Z" };
const COMPARE = { days: 29, from: "2026-09-27T00:00:00Z", to: "2026-10-26T00:00:00Z" };

function evalItem(slug: string, kind: EvalKind): EvalListItem {
  const written = { id: "alice", kind: "user" } as const;
  return {
    definition: {
      answer: "",
      created_at: "2026-10-01T00:00:00Z",
      decision: "",
      direction: "",
      doc_block: "",
      eval_id: `ACME/${slug}`,
      failure_modes: [],
      issue_key: "",
      kind,
      links: [],
      name: slug,
      number_rule: "",
      owner_role: "",
      person: "alice",
      project_key: "ACME",
      question: "",
      replaced_by: "",
      slug,
      source: kind === "experiment" ? "logged" : "computed",
      state: "active",
      target: null,
      target_inclusive: false,
      target_judges: [],
      unit: "count",
      version: 1,
      written_by: written,
      written_via: "register",
    },
    id: `ACME/${slug}`,
    owner: { person: "alice", role: "", written_by: written },
    series: [],
    verdict: null,
  };
}

/** An evals view's answer holding `items`, of `total` evals in all. */
function answer(items: EvalListItem[], total = items.length, offset = 0): EvalListResponse {
  return {
    compare: COMPARE,
    delivery_freshness: null,
    freshness: { refresher_age_seconds: null, rows_updated_at: null },
    items,
    limit: 200,
    offset,
    total,
    window: WINDOW,
  };
}

/** One eval of each kind, the answer each view gives a tab with every section drawn. */
const ANSWERS: Record<EvalKind, EvalListResponse> = {
  ci: answer([evalItem("pr-ci", "ci")]),
  delivery: answer([evalItem("deploy-frequency", "delivery")]),
  experiment: answer([evalItem("widget-size", "experiment")]),
  legion: answer([evalItem("legion-work-time", "legion")]),
};

/** A delta of the view `sub` that adds or changes `upsert`. */
function deltaFrame(sub: string, upsert: EvalListItem[], seq = "2"): DeliveryLiveDelta {
  return {
    changed_at: null,
    counts: {},
    items: { items: { remove: [], upsert } },
    maps: {},
    revs: {
      items: { items: Object.fromEntries(upsert.map((item) => [item.id, "00000000000000aa"])) },
    },
    seq,
    set: {},
    sub,
    type: "delta",
  };
}

let sockets: FakeLiveSockets;
const spies: { mockRestore: () => void }[] = [];
beforeEach(() => {
  sockets = installFakeLiveSockets();
});
afterEach(() => {
  sockets.restore();
  for (const spy of spies.splice(0)) spy.mockRestore();
});

/** A row of this file, its page unmounted inside its body rather than in `afterEach`: the tab's
 *  timers are real ones, and Bun gives the event loop a turn between a row's body and its
 *  `afterEach`, where a timer due then would set state with no act scope open. */
const test = (name: string, body: () => Promise<void>): void =>
  bunTest(name, async () => {
    try {
      await body();
    } finally {
      cleanup();
    }
  });

/** An eval's detail read as Dispatch answers it: its method, `docText`, and no declared chart. */
function detailOf(docText: string | null): EvalDetailResponse {
  return {
    compare: COMPARE,
    doc_text: docText,
    freshness: { refresher_age_seconds: null, rows_updated_at: null },
    items: [],
    limit: 1,
    offset: 0,
    total: 1,
    window: WINDOW,
  };
}

/** A read's answer as a row names it: the row, `null` for none, "pending" for a read that never
 *  answers, or a promise the row settles. */
type Answer<T> = T | null | "pending" | Promise<T | null>;

function answerOf<T>(named: Answer<T>): Promise<T | null> {
  if (named === "pending") return Promise.withResolvers<never>().promise;
  return named instanceof Promise ? named : Promise.resolve(named);
}

/** The tab at `search`, its project's daemon read answering `daemon` and its delivery settings
 *  read `settings` (delivery on, by default), and each eval's detail read answering no method and
 *  no declared chart. */
function renderTab(
  daemon: Answer<LegionSetting> = DAEMON,
  search = "?window=28d",
  settings: Answer<ProjectDeliverySettings> = deliverySettings("ACME")
) {
  const getLegionDaemon = spyOn(api, "getLegionDaemon").mockImplementation(() => answerOf(daemon));
  const getProjectDeliverySettings = spyOn(api, "getProjectDeliverySettings").mockImplementation(
    () => answerOf(settings)
  );
  const listIssues = spyOn(api, "listIssues");
  const getEval = spyOn(api, "getEval").mockResolvedValue(detailOf(null));
  spies.push(getLegionDaemon, getProjectDeliverySettings, listIssues, getEval);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <MemoryRouter initialEntries={[`/projects/ACME/evals${search}`]}>
      <QueryClientProvider client={queryClient}>
        <EvalsTab project="ACME" />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { getEval, getLegionDaemon, getProjectDeliverySettings, listIssues };
}

/** The page's live socket, once it has opened one, as Dispatch accepts it. */
async function openSocket(): Promise<FakeLiveSocket> {
  await waitFor(() => expect(sockets.sockets.length).toBeGreaterThan(0));
  const socket = sockets.latest();
  act(() => socket.open());
  return socket;
}

/** The query of an evals view's subscribe frame. */
function queryOf(frame: LiveSubscribe): URLSearchParams {
  return new URLSearchParams(frame.view.kind === "evals" ? frame.view.query : "");
}

/** The evals views `socket` holds now: subscribed and not unsubscribed since. */
function heldViews(socket: FakeLiveSocket): LiveSubscribe[] {
  const dropped = new Set(socket.unsubscribes().map((frame) => frame.sub));
  return socket
    .subscribes()
    .filter((frame) => frame.view.kind === "evals" && !dropped.has(frame.sub));
}

/** The newest subscribe frame of the `kind` view at `offset`, once the page has sent one. */
function subscription(socket: FakeLiveSocket, kind: EvalKind, offset = 0): Promise<LiveSubscribe> {
  return waitFor(() => {
    const frame = socket
      .subscribes()
      .filter(
        (sent) =>
          sent.view.kind === "evals" &&
          queryOf(sent).get("kind") === kind &&
          Number(queryOf(sent).get("offset") ?? 0) === offset
      )
      .at(-1);
    if (frame === undefined) throw new Error(`no ${kind} view at offset ${offset} was subscribed`);
    return frame;
  });
}

/** Dispatch sends `frame` to the page. */
function serve(socket: FakeLiveSocket, frame: LiveServerFrame): void {
  act(() => socket.receive(frame));
}

/** Dispatch answers the `kind` view's first page with `body`. */
async function answerView(
  socket: FakeLiveSocket,
  kind: EvalKind,
  body = ANSWERS[kind],
  seq = "1"
): Promise<string> {
  const { sub } = await subscription(socket, kind);
  serve(socket, snapshotFrame(sub, body, { seq }));
  return sub;
}

function region(name: string): HTMLElement {
  return screen.getByRole("region", { name });
}

/** The evals a section lists, by slug. */
function listed(name: string): string[] {
  return [...region(name).querySelectorAll("[data-eval]")].map(
    (row) => row.getAttribute("data-eval") ?? ""
  );
}

/** The sections the tab draws, by id, in order. */
function sectionsDrawn(): string[] {
  return [...document.querySelectorAll("[data-section]")].map(
    (section) => section.getAttribute("data-section") ?? ""
  );
}

/** The target cards drawn, by eval slug, in order. */
function cardsDrawn(): string[] {
  return [...region("Target cards").querySelectorAll("[data-card]")].map(
    (card) => card.getAttribute("data-card") ?? ""
  );
}

/** The kinds of the evals views `socket` holds now, sorted. */
function heldKinds(socket: FakeLiveSocket): string[] {
  return heldViews(socket)
    .map((frame) => queryOf(frame).get("kind") ?? "")
    .sort();
}

/** A brush dragged over part of three UTC days, and the whole days it touches. */
const BRUSH = { end: "2026-11-04T12:00:00.000Z", start: "2026-11-02T06:00:00.000Z" };
const BRUSHED_DAYS = { from: "2026-11-02T00:00:00.000Z", to: "2026-11-05T00:00:00.000Z" };

/** Draws the merge timeline's chart as a stand-in, since the chart draws on a canvas the unit DOM
 *  cannot: it counts the rows it is handed and sets the brush on a press. */
function standInChart(): { drawn: () => number } {
  let drawn = 0;
  const chart = spyOn(timeline, "Timeline").mockImplementation((props) => {
    drawn = props.prs.length + props.runs.length;
    return (
      <button onClick={() => props.onBrush(BRUSH)} type="button">
        Brush
      </button>
    );
  });
  spies.push(chart);
  return { drawn: () => drawn };
}

/** Opens the merge timeline, and the project-timeline view it subscribes once it has. */
async function openTimeline(socket: FakeLiveSocket): Promise<LiveSubscribe> {
  await userEvent.setup().click(screen.getByRole("button", { name: "Show the merge timeline" }));
  return waitFor(() => socket.subscribed("project-timeline"));
}

/** The newest project-timeline subscribe frame, once it asks for `limit` rows. */
function grownTo(socket: FakeLiveSocket, limit: number): Promise<LiveSubscribe> {
  return waitFor(() => {
    const frame = socket.subscribed("project-timeline");
    expect(limitOf(frame)).toBe(limit);
    return frame;
  });
}

/** The end of the rows the timeline holds: the line under them. */
function rowsEnd(): Element {
  const end = document.querySelector("[data-timeline-end]");
  if (end === null) throw new Error("the timeline draws no end of its rows");
  return end;
}

/** The evals the facets narrow (plan D7a), each with the series its card and chart draw, that
 *  series' first headline statistic, and its unit. */
const PR_EVALS = [
  { series: "minutes", slug: "merge-to-production", statistic: "median", unit: "minutes" },
  { series: "minutes", slug: "first-commit-to-production", statistic: "median", unit: "minutes" },
  { series: "minutes", slug: "open-to-production", statistic: "median", unit: "minutes" },
  { series: "minutes", slug: "pr-open-to-merge", statistic: "median", unit: "minutes" },
  { series: "prs", slug: "rework-share", statistic: "rate", unit: "ratio" },
] as const;

/** A delivery view's answer: the deploys and the deploy runs as stored, and each pull-request
 *  eval at `value` (minutes, or a share of `value` in a hundred), its series narrowed by
 *  `narrowed`, as Dispatch names the facets it applied. */
function deliveryAnswer(value: number, narrowed: string[] | null = null): EvalListResponse {
  return answer([
    charted(evalItem("deploy-frequency", "delivery"), "deploys", "deploys", {
      deploys: 3,
      per_day: 3,
    }),
    charted(evalItem("deploy-run-success", "delivery"), "runs", "runs", {
      concluded: 4,
      rate: 0.75,
      reached_production: 3,
    }),
    ...PR_EVALS.map(({ series, slug, statistic, unit }) => {
      const item = charted(evalItem(slug, "delivery"), series, unit, {
        [statistic]: unit === "ratio" ? value / 100 : value,
      });
      return { ...item, series: (item.series ?? []).map((each) => ({ ...each, narrowed })) };
    }),
  ]);
}

/** The Deploys and lead time section's chart of `slug`. */
function chartOf(slug: string): Element {
  const found = region("Deploys and lead time").querySelector(`[data-eval="${slug}"]`);
  if (found === null) throw new Error(`no ${slug} chart`);
  return found;
}

/** What a card or a chart says of the facets: its mark's kind and words; null for no mark. */
function markOf(element: Element): [string | null, string | null] | null {
  const mark = element.querySelector("[data-narrowing]");
  return mark === null ? null : [mark.getAttribute("data-narrowing"), mark.textContent];
}

const LOADING: [string, string] = ["loading", "narrowing to the facets set\u2026"];
const WINDOW_ONLY: [string, string] = ["window-only", "window and repository only"];

/** Each pull-request eval's chart, and its card where it has one, draws `value` (as
 *  `deliveryAnswer` holds it) under `mark`, and, while loading, marked busy. */
function expectPullRequestEvals(value: number, mark: [string, string] | null): void {
  for (const { slug, statistic, unit } of PR_EVALS) {
    const drawn = region("Target cards").querySelector(`[data-card="${slug}"]`);
    for (const element of drawn === null ? [chartOf(slug)] : [chartOf(slug), drawn]) {
      expect(element.querySelector(`[data-window-statistic="${statistic}"]`)?.textContent).toBe(
        unit === "ratio" ? `${value.toFixed(1)}%` : `${value}m`
      );
      expect(markOf(element)).toEqual(mark);
      expect(element.querySelector('[aria-busy="true"]') !== null).toBe(mark === LOADING);
    }
  }
}

/** Every card and chart of an eval no facet narrows, and the CI and Legion sections, say "window
 *  and repository only". */
function expectWindowOnly(): void {
  const narrowed = new Set<string>(PR_EVALS.map(({ slug }) => slug));
  const unnarrowed = (name: string, attribute: string) =>
    [...region(name).querySelectorAll(`[${attribute}]`)].filter(
      (element) => !narrowed.has(element.getAttribute(attribute) ?? "")
    );
  const cards = unnarrowed("Target cards", "data-card");
  const charts = unnarrowed("Deploys and lead time", "data-eval");
  expect(cards.map((card) => card.getAttribute("data-card"))).toEqual([
    "deploy-frequency",
    "change-fail-rate",
    "deploy-run-success",
    "unowned-p0",
    "failed-deploy-recovery",
    "deployment-rework-rate",
    "legion-work-time",
    "legion-rework",
  ]);
  expect(charts.map((chart) => chart.getAttribute("data-eval"))).toEqual([
    "deploy-frequency",
    "deploy-run-success",
  ]);
  for (const element of [...cards, ...charts]) expect(markOf(element)).toEqual(WINDOW_ONLY);
  for (const name of ["CI health", "Legion measures"]) {
    expect(markOf(region(name))).toEqual(WINDOW_ONLY);
  }
}

/** The delivery views the page holds now: the unnarrowed one, and the one whose query sets
 *  `key`. */
function deliveryViews(socket: FakeLiveSocket, key: string) {
  const frames = heldViews(socket).filter((frame) => queryOf(frame).get("kind") === "delivery");
  return {
    narrowed: frames.filter((frame) => queryOf(frame).has(key)),
    plain: frames.filter((frame) => !queryOf(frame).has(key)),
  };
}

/** A tab opened from an address that sets the author facet, with its unnarrowed views painted,
 *  and the narrowed delivery view it reads beside them, which has not answered. */
async function paintedWithAuthor(): Promise<{
  narrowed: LiveSubscribe;
  plain: LiveSubscribe;
  socket: FakeLiveSocket;
}> {
  renderTab(DAEMON, "?window=28d&author=alice");
  const socket = await openSocket();
  const { narrowed, plain } = await waitFor(() => {
    const views = deliveryViews(socket, "author");
    expect([views.plain.length, views.narrowed.length]).toEqual([1, 1]);
    return { narrowed: views.narrowed[0], plain: views.plain[0] };
  });
  expect(queryOf(narrowed).getAll("author")).toEqual(["alice"]);
  serve(socket, snapshotFrame(plain.sub, deliveryAnswer(12)));
  for (const kind of ["legion", "ci", "experiment"] as const) {
    const { sub, view } = await subscription(socket, kind);
    // No facet narrows a view of another kind: it is the one view the tab opens without one.
    expect(view.kind === "evals" && new URLSearchParams(view.query).has("author")).toBe(false);
    serve(socket, snapshotFrame(sub, ANSWERS[kind]));
  }
  // Every section paints from the unnarrowed views, and the Logged evals' detail reads settle.
  await waitFor(() =>
    expect(
      [...document.querySelectorAll("[data-section]")].map((section) =>
        section.getAttribute("data-state")
      )
    ).toEqual(["painted", "painted", "painted", "painted", "closed", "painted"])
  );
  return { narrowed, plain, socket };
}

const stateRows: [string, () => Promise<void>][] = [
  [
    "subscribed | own snapshot | painted: the section's numbers",
    async () => {
      renderTab();
      const socket = await openSocket();
      expect(region("CI health")).toHaveAttribute("data-state", "subscribed");
      await answerView(socket, "ci");
      expect(region("CI health")).toHaveAttribute("data-state", "painted");
      expect(listed("CI health")).toEqual(["pr-ci"]);
    },
  ],
  [
    "subscribed | another section's snapshot | subscribed: this section's loading line; the other section paints",
    async () => {
      renderTab();
      const socket = await openSocket();
      await answerView(socket, "ci");
      expect(listed("CI health")).toEqual(["pr-ci"]);
      expect(region("Legion measures")).toHaveAttribute("data-state", "subscribed");
      expect(within(region("Legion measures")).getByRole("status").textContent).toBe(
        "Legion measures: loading…"
      );
      expect(listed("Legion measures")).toEqual([]);
    },
  ],
  [
    "subscribed | own error frame | failed: the section's error line with Retry; others unaffected",
    async () => {
      const user = userEvent.setup();
      renderTab();
      const socket = await openSocket();
      const { sub } = await subscription(socket, "ci");
      serve(socket, errorFrame(sub, "INTERNAL", "the read failed"));
      const failed = region("CI health");
      await waitFor(() => expect(failed).toHaveAttribute("data-state", "failed"));
      expect(within(failed).getByRole("alert").textContent).toContain("CI health: could not load.");
      await answerView(socket, "delivery");
      expect(region("Deploys and lead time")).toHaveAttribute("data-state", "painted");
      expect(listed("Deploys and lead time")).toEqual(["deploy-frequency"]);

      // Retry subscribes the refused view again, and its answer paints the section.
      await user.click(within(failed).getByRole("button", { name: "Retry" }));
      const again = await waitFor(() => {
        const frame = socket.subscribed("evals");
        expect(frame.sub).not.toBe(sub);
        expect(queryOf(frame).get("kind")).toBe("ci");
        return frame;
      });
      serve(socket, snapshotFrame(again.sub, ANSWERS.ci));
      await waitFor(() => expect(region("CI health")).toHaveAttribute("data-state", "painted"));
    },
  ],
  [
    "painted | own delta | painted: only this section re-renders (render count of the others unchanged)",
    async () => {
      const rendered = spyOn(sections, "EvalsSection");
      spies.push(rendered);
      renderTab();
      const socket = await openSocket();
      const subs = {
        ci: await answerView(socket, "ci"),
        delivery: await answerView(socket, "delivery"),
        experiment: await answerView(socket, "experiment"),
        legion: await answerView(socket, "legion"),
      };
      await waitFor(() =>
        expect(
          [...document.querySelectorAll("[data-section]")].map((section) =>
            section.getAttribute("data-state")
          )
        ).toEqual(["painted", "painted", "painted", "painted", "closed", "painted"])
      );
      const counts = () => {
        const byId: Record<string, number> = {};
        for (const [props] of rendered.mock.calls) byId[props.id] = (byId[props.id] ?? 0) + 1;
        return byId;
      };
      const before = counts();
      serve(socket, deltaFrame(subs.ci, [evalItem("staging-e2e", "ci")]));
      await waitFor(() => expect(listed("CI health")).toEqual(["pr-ci", "staging-e2e"]));
      const after = counts();
      expect(after.ci).toBeGreaterThan(before.ci ?? 0);
      expect({ ...after, ci: 0 }).toEqual({ ...before, ci: 0 });
    },
  ],
  [
    "painted | header window change | subscribed: a new view; the old one is unsubscribed",
    async () => {
      const user = userEvent.setup();
      renderTab();
      const socket = await openSocket();
      const old = await answerView(socket, "ci");
      expect(region("CI health")).toHaveAttribute("data-state", "painted");

      await user.selectOptions(screen.getByLabelText("Window"), "7d");
      const next = await waitFor(() => {
        const frame = socket.subscribed("evals");
        expect(frame.view).toMatchObject({ window: { preset: 7 } });
        return frame;
      });
      await waitFor(() => expect(socket.unsubscribes().map((frame) => frame.sub)).toContain(old));
      expect(
        heldViews(socket).every(
          (frame) =>
            frame.view.kind === "evals" &&
            "preset" in frame.view.window &&
            frame.view.window.preset === 7
        )
      ).toBe(true);
      expect(next.sub).not.toBe(old);
      expect(region("CI health")).toHaveAttribute("data-state", "subscribed");
    },
  ],
  [
    "painted | socket reconnect | painted: resume with the view's revisions, no blank",
    async () => {
      renderTab();
      const socket = await openSocket();
      const sub = await answerView(socket, "ci", ANSWERS.ci, "5");
      act(() => socket.restart());
      // Lost, and not yet back: the section keeps what it drew.
      expect(region("CI health")).toHaveAttribute("data-state", "painted");
      expect(listed("CI health")).toEqual(["pr-ci"]);

      await waitFor(() => expect(sockets.sockets.length).toBe(2));
      const next = sockets.latest();
      act(() => next.open());
      const resumed = next.subscribes().find((frame) => frame.sub === sub);
      expect(resumed?.resume).toEqual({
        revs: { counts: {}, fields: {}, items: {}, maps: {} },
        seq: "5",
      });
      expect(listed("CI health")).toEqual(["pr-ci"]);
    },
  ],
  [
    'subscribed | sources answer "no daemon" | hidden (Legion): no Legion section, no Legion card',
    async () => {
      renderTab(null);
      const socket = await openSocket();
      await waitFor(() =>
        expect(screen.queryByRole("region", { name: "Legion measures" })).toBeNull()
      );
      await answerView(socket, "delivery");
      // The delivery cards, and neither Legion card.
      expect(
        [...region("Target cards").querySelectorAll("[data-card]")].map((card) =>
          card.getAttribute("data-card")
        )
      ).toEqual([
        "deploy-frequency",
        "change-fail-rate",
        "merge-to-production",
        "pr-open-to-merge",
        "deploy-run-success",
        "unowned-p0",
        "failed-deploy-recovery",
        "deployment-rework-rate",
      ]);
      expect(
        heldViews(socket)
          .map((frame) => queryOf(frame).get("kind"))
          .sort()
      ).toEqual(["ci", "delivery", "experiment"]);
    },
  ],
  [
    "painted | the repository filter set | subscribed: the stored views with `repo` (no narrowed view)",
    async () => {
      const user = userEvent.setup();
      renderTab();
      const socket = await openSocket();
      const old = await answerView(socket, "delivery");

      await user.type(screen.getByLabelText("Repository"), "acme/widgets{Enter}");
      await waitFor(() => expect(socket.unsubscribes().map((frame) => frame.sub)).toContain(old));
      const views = heldViews(socket);
      expect(views.map((frame) => queryOf(frame).get("kind")).sort()).toEqual([
        "ci",
        "delivery",
        "experiment",
        "legion",
      ]);
      for (const frame of views) {
        expect(queryOf(frame).getAll("repo")).toEqual(["acme/widgets"]);
        expect([...new Set(queryOf(frame).keys())].sort()).toEqual([
          "compare_from",
          "compare_to",
          "kind",
          "limit",
          "repo",
        ]);
      }
      expect(region("Deploys and lead time")).toHaveAttribute("data-state", "subscribed");
    },
  ],
  [
    "closed (timeline) | open | subscribed with limit=200",
    async () => {
      standInChart();
      renderTab();
      const socket = await openSocket();
      const section = region("Merge timeline");
      expect(section).toHaveAttribute("data-state", "closed");
      expect(socket.subscribes().filter((frame) => frame.view.kind === "project-timeline")).toEqual(
        []
      );

      const opened = await openTimeline(socket);
      expect(opened.view).toEqual({
        kind: "project-timeline",
        project: "ACME",
        query: "limit=200",
        window: { preset: 28 },
      });
      expect(opened.resume).toBeUndefined();
      expect(section).toHaveAttribute("data-state", "subscribed");
      serve(socket, timelineSnapshot(opened.sub, timelineAnswer(timelineRows(0, 200), 1300)));
      expect(section).toHaveAttribute("data-state", "painted");
    },
  ],
  [
    "painted (timeline) | scrolled to the end | growing: same sub, limit + 200, a resume with the held revisions; rows kept on screen; only the lacking rows arrive, past 500 as below it",
    async () => {
      const chart = standInChart();
      const observers = installIntersectionObserver();
      spies.push({ mockRestore: observers.restore });
      renderTab();
      const socket = await openSocket();
      const opened = await openTimeline(socket);
      const snapshot = timelineSnapshot(opened.sub, timelineAnswer(timelineRows(0, 200), 1300));
      serve(socket, snapshot);
      const section = region("Merge timeline");
      expect(section).toHaveAttribute("data-state", "painted");

      act(() => observers.show(rowsEnd()));
      let held = snapshot.revs;
      let seq = snapshot.seq;
      // A window of 1,300 rows grows past 500 on the one view as it grew below it.
      for (const limit of [400, 600]) {
        const grown = await grownTo(socket, limit);
        expect(grown.sub).toBe(opened.sub);
        expect(grown.resume).toEqual({ revs: held, seq });
        expect(section).toHaveAttribute("data-state", "growing");
        expect(chart.drawn()).toBe(limit - 200);
        // Dispatch answers the rows the page lacked, and those alone.
        const growth = timelineGrowth(
          opened.sub,
          timelineRows(limit - 200, limit),
          limit,
          String(Number(seq) + 1)
        );
        serve(socket, growth);
        expect(chart.drawn()).toBe(limit);
        held = applyRevs(held, growth.revs);
        seq = growth.seq;
      }
      expect(socket.unsubscribes()).toEqual([]);
    },
  ],
  [
    "painted (timeline) | limit reaches total | painted: no further growth",
    async () => {
      const chart = standInChart();
      const observers = installIntersectionObserver();
      spies.push({ mockRestore: observers.restore });
      renderTab();
      const socket = await openSocket();
      const opened = await openTimeline(socket);
      serve(socket, timelineSnapshot(opened.sub, timelineAnswer(timelineRows(0, 200), 450)));
      act(() => observers.show(rowsEnd()));
      await grownTo(socket, 400);
      serve(socket, timelineGrowth(opened.sub, timelineRows(200, 400), 400, "2"));
      await grownTo(socket, 450);
      serve(socket, timelineGrowth(opened.sub, timelineRows(400, 450), 450, "3"));
      const section = region("Merge timeline");
      expect(section).toHaveAttribute("data-state", "painted");
      expect(chart.drawn()).toBe(450);

      act(() => observers.hide(rowsEnd()));
      act(() => observers.show(rowsEnd()));
      expect(section).toHaveAttribute("data-state", "painted");
      expect(
        socket
          .subscribes()
          .filter((frame) => frame.view.kind === "project-timeline")
          .map(limitOf)
      ).toEqual([200, 400, 450]);
    },
  ],
  [
    "painted | the brush set on the timeline | subscribed: the cards' views over the brushed days; the cards name the days",
    async () => {
      const user = userEvent.setup();
      standInChart();
      renderTab();
      const socket = await openSocket();
      for (const kind of ["delivery", "legion", "ci", "experiment"] as const) {
        await answerView(socket, kind);
      }
      expect(region("Target cards")).toHaveAttribute("data-state", "painted");
      const opened = await openTimeline(socket);
      serve(socket, timelineSnapshot(opened.sub, timelineAnswer(timelineRows(0, 20), 20)));

      await user.click(within(region("Merge timeline")).getByRole("button", { name: "Brush" }));
      const brushed = await waitFor(() => {
        const frames = heldViews(socket).filter(
          (frame) => frame.view.kind === "evals" && "from" in frame.view.window
        );
        expect(frames.map((frame) => queryOf(frame).get("kind")).sort()).toEqual([
          "delivery",
          "legion",
        ]);
        return frames;
      });
      for (const frame of brushed) expect(frame.view).toMatchObject({ window: BRUSHED_DAYS });
      expect(region("Target cards")).toHaveAttribute("data-state", "subscribed");
      // The other sections keep the header's window and what they drew.
      expect(region("Deploys and lead time")).toHaveAttribute("data-state", "painted");
      for (const card of region("Target cards").querySelectorAll("[data-card]")) {
        expect(card.querySelector("[data-card-days]")?.textContent).toBe(
          "brushed days 2026-11-02 to 2026-11-04"
        );
      }

      for (const frame of brushed) {
        const kind = queryOf(frame).get("kind") as EvalKind;
        serve(socket, snapshotFrame(frame.sub, ANSWERS[kind]));
      }
      expect(region("Target cards")).toHaveAttribute("data-state", "painted");
    },
  ],
  [
    "painted, an open panel | the evals view's delta carries a new definition.version | re-reading: the detail read again; then the new method (D11a)",
    async () => {
      const user = userEvent.setup();
      const deploys = (version: number): EvalListItem => {
        const item = charted(evalItem("deploy-frequency", "delivery"), "deploys", "deploys", {
          deploys: 3,
          per_day: 3,
        });
        return { ...item, definition: { ...item.definition, version } };
      };
      const { getEval } = renderTab();
      // Each detail read, held until the row answers it.
      const reads: ((detail: EvalDetailResponse) => void)[] = [];
      getEval.mockImplementation(() => {
        const read = Promise.withResolvers<EvalDetailResponse>();
        reads.push(read.resolve);
        return read.promise;
      });
      const socket = await openSocket();
      const sub = await answerView(socket, "delivery", answer([deploys(1)]));
      await user.click(
        within(region("Deploys and lead time")).getByRole("button", { name: "Deploy frequency" })
      );
      const panel = screen.getByRole("dialog", { name: "deploy-frequency" });
      await waitFor(() => expect(reads).toHaveLength(1));
      act(() => reads[0]?.(detailOf("Deploys a day, as version 1 counts them.")));
      await waitFor(() =>
        expect(panel.querySelector("[data-method-text]")?.textContent).toBe(
          "Deploys a day, as version 1 counts them."
        )
      );

      // A register write rewrote the method: the delivery view's delta carries version 2.
      serve(socket, deltaFrame(sub, [deploys(2)]));
      await waitFor(() => expect(reads).toHaveLength(2));
      expect(
        within(panel)
          .getAllByRole("status")
          .map((line) => line.textContent)
      ).toContain("Reading the method…");
      act(() => reads[1]?.(detailOf("Deploys a day, as version 2 counts them.")));
      await waitFor(() =>
        expect(panel.querySelector("[data-method-text]")?.textContent).toBe(
          "Deploys a day, as version 2 counts them."
        )
      );
      // The open panel read it on the one view it had: no view was subscribed again.
      expect(screen.getByRole("dialog", { name: "deploy-frequency" })).toBe(panel);
      expect(heldViews(socket).filter((frame) => frame.sub === sub)).toHaveLength(1);
    },
  ],
  [
    'painted | a facet or the search set (header or timeline) | subscribed: the delivery view again with the facets; the cards and the Deploys and lead time section\'s five pull-request charts draw its narrowed values; the other delivery charts and the window-only cards show the "window and repository only" mark',
    async () => {
      const user = userEvent.setup();
      standInChart();
      renderTab();
      const socket = await openSocket();
      const plain = await answerView(socket, "delivery", deliveryAnswer(12));
      const others: string[] = [];
      for (const kind of ["legion", "ci", "experiment"] as const) {
        others.push(await answerView(socket, kind));
      }
      expectPullRequestEvals(12, null);
      expect(document.querySelector("[data-narrowing]")).toBeNull();

      // The timeline's search, once it holds still, narrows the delivery view.
      const opened = await openTimeline(socket);
      serve(socket, timelineSnapshot(opened.sub, timelineAnswer(timelineRows(0, 20), 20)));
      await user.type(within(region("Merge timeline")).getByLabelText("Search"), "fix");
      const narrowed = await waitFor(() => {
        const frame = socket.subscribed("evals");
        expect(queryOf(frame).get("q")).toBe("fix");
        return frame;
      });
      expect(queryOf(narrowed).get("kind")).toBe("delivery");
      // Until it answers, the delivery view drawn stays, under the loading mark.
      expect(deliveryViews(socket, "q").plain.map((frame) => frame.sub)).toEqual([plain]);
      expectPullRequestEvals(12, LOADING);
      expectWindowOnly();

      serve(socket, snapshotFrame(narrowed.sub, deliveryAnswer(7, ["q"])));
      expectPullRequestEvals(7, ["narrowed", "narrowed by Search"]);
      expectWindowOnly();
      await waitFor(() => expect(socket.unsubscribes().map((frame) => frame.sub)).toContain(plain));
      // No view of another kind was subscribed again: no facet narrows it.
      expect(
        heldViews(socket)
          .map((frame) => frame.sub)
          .sort()
      ).toEqual([narrowed.sub, ...others].sort());
    },
  ],
  [
    "opened from an address with a facet | the unnarrowed snapshots land | painted: every section; the narrowed cards and the Deploys and lead time section's five pull-request charts show a loading mark over their unnarrowed values",
    async () => {
      const { narrowed, plain, socket } = await paintedWithAuthor();
      // The narrowed delivery view has not answered: both are held.
      expect(deliveryViews(socket, "author")).toEqual({ narrowed: [narrowed], plain: [plain] });
      expectPullRequestEvals(12, LOADING);
      expectWindowOnly();
    },
  ],
  [
    "painted, the narrowed cards and charts loading | the narrowed delivery view's snapshot | painted: those cards and charts draw the narrowed values and name the facets applied; the unnarrowed delivery view is unsubscribed",
    async () => {
      const { narrowed, plain, socket } = await paintedWithAuthor();
      serve(socket, snapshotFrame(narrowed.sub, deliveryAnswer(7, ["author"])));
      expectPullRequestEvals(7, ["narrowed", "narrowed by Author"]);
      expectWindowOnly();
      await waitFor(() =>
        expect(socket.unsubscribes().map((frame) => frame.sub)).toContain(plain.sub)
      );
      expect(deliveryViews(socket, "author")).toEqual({ narrowed: [narrowed], plain: [] });
      expect(
        [...document.querySelectorAll("[data-section]")].map((section) =>
          section.getAttribute("data-state")
        )
      ).toEqual(["painted", "painted", "painted", "painted", "closed", "painted"]);
    },
  ],
  [
    "painted, a declared eval | the evals view's delta carries a new definition.version | re-reading: the detail read again; then the new charts and answer (D11a)",
    async () => {
      const declared = (version: number, said: string): EvalListItem => {
        const item = evalItem("convergence", "experiment");
        return { ...item, definition: { ...item.definition, answer: said, version } };
      };
      const detail = (version: number, chart: object, said: string): EvalDetailResponse => {
        const { definition, owner } = declared(version, said);
        return {
          ...detailOf(null),
          items: [{ charts: [chart], definition, owner, series: [], verdict: null }],
        };
      };
      /** Dispatch answers the eval's items view of `series` with its rows in `fixture`. */
      const serveItems = async (series: string, fixture: { items: unknown[] }) => {
        const frame = await waitFor(() => {
          const sent = socket
            .subscribes()
            .filter(
              (one) =>
                one.view.kind === "items" &&
                new URLSearchParams(one.view.query).get("series") === series
            )
            .at(-1);
          if (sent === undefined) throw new Error(`no items view of ${series} was subscribed`);
          return sent;
        });
        const rows = (fixture.items as EvalItemRow[]).filter((row) => row.cols?.series === series);
        const body: EvalItemsResponse = {
          compare: COMPARE,
          items: rows,
          limit: 200,
          offset: 0,
          total: rows.length,
          window: WINDOW,
        };
        serve(socket, snapshotFrame(frame.sub, body));
      };
      const { getEval } = renderTab();
      // Each detail read, held until the row answers it.
      const reads: ((detail: EvalDetailResponse) => void)[] = [];
      getEval.mockImplementation(() => {
        const read = Promise.withResolvers<EvalDetailResponse>();
        reads.push(read.resolve);
        return read.promise;
      });
      const socket = await openSocket();
      for (const kind of ["delivery", "legion", "ci"] as const) await answerView(socket, kind);
      const first = "Arm A converges in fewer tokens.";
      const sub = await answerView(socket, "experiment", answer([declared(1, first)]));
      await waitFor(() => expect(reads).toHaveLength(1));
      act(() =>
        reads[0]?.(
          detail(
            1,
            {
              group: "col",
              id: "tokens-over-time",
              kind: "xy",
              title: "Tokens to converge",
              y: { series: "tokens" },
            },
            first
          )
        )
      );
      await serveItems("tokens", xyReadings);
      const logged = region("Logged");
      await waitFor(() => within(logged).getByRole("figure", { name: "Tokens to converge" }));
      const eval_ = () => {
        const found = logged.querySelector('[data-declared="convergence"]');
        if (!(found instanceof HTMLElement)) throw new Error("no declared eval is drawn");
        return found;
      };
      expect(eval_().querySelector("[data-answer]")?.textContent).toBe(first);

      // A register write changed the charts and the answer: the view's delta carries version 2.
      const next = "Arm A converges sooner, and its latency spread narrows.";
      serve(socket, deltaFrame(sub, [declared(2, next)]));
      await waitFor(() => expect(reads).toHaveLength(2));
      expect(within(eval_()).getByRole("status").textContent).toBe("Reading the charts…");
      act(() =>
        reads[1]?.(
          detail(
            2,
            {
              group: "col",
              id: "latency-band",
              kind: "band",
              title: "Latency",
              y: { series: "latency" },
            },
            next
          )
        )
      );
      await serveItems("latency", bandReadings);
      const band = await waitFor(() => within(eval_()).getByRole("figure", { name: "Latency" }));
      expect(band).toHaveAttribute("data-chart-kind", "band");
      expect(within(eval_()).queryByRole("figure", { name: "Tokens to converge" })).toBeNull();
      expect(eval_().querySelector("[data-answer]")?.textContent).toBe(next);
      expect(within(eval_()).queryByRole("status")).toBeNull();
      // The one view it had: no view was subscribed again.
      expect(heldViews(socket).filter((frame) => frame.sub === sub)).toHaveLength(1);
    },
  ],
  [
    'subscribed | sources answer "delivery off" | hidden (delivery, CI)',
    async () => {
      const settings = Promise.withResolvers<ProjectDeliverySettings | null>();
      renderTab(DAEMON, "?window=28d", settings.promise);
      const socket = await openSocket();
      // Until the settings answer, the delivery and CI views open with the others and paint.
      const delivery = await answerView(socket, "delivery");
      const ci = await answerView(socket, "ci");
      await answerView(socket, "legion");
      expect(cardsDrawn()).toContain("deploy-frequency");

      act(() => settings.resolve(deliverySettings("ACME", { delivery: false })));
      await waitFor(() =>
        expect(sectionsDrawn()).toEqual(["cards", "legion", "timeline", "logged"])
      );
      // The delivery view held the delivery evals: the tab hides them from the settings alone.
      expect(cardsDrawn()).toEqual(["legion-work-time", "legion-rework"]);
      await waitFor(() =>
        expect(socket.unsubscribes().map((frame) => frame.sub)).toEqual(
          expect.arrayContaining([delivery, ci])
        )
      );
      expect(heldKinds(socket)).toEqual(["experiment", "legion"]);
      // The project has its settings, with delivery off: no "not set up" line.
      expect(document.querySelector("[data-missing-sources]")).toBeNull();
    },
  ],
  [
    'subscribed | sources answer "no settings row" | hidden (delivery, CI); the header\'s "Delivery: not set up" line with its link',
    async () => {
      renderTab(DAEMON, "?window=28d", null);
      const socket = await openSocket();
      const link = await screen.findByRole("link", { name: "Delivery: not set up" });
      expect(link).toHaveAttribute("href", "/projects/ACME/settings");
      expect(document.querySelectorAll("[data-missing-sources]")).toHaveLength(1);
      await waitFor(() =>
        expect(sectionsDrawn()).toEqual(["cards", "legion", "timeline", "logged"])
      );
      await waitFor(() => expect(heldKinds(socket)).toEqual(["experiment", "legion"]));
      await answerView(socket, "legion");
      expect(cardsDrawn()).toEqual(["legion-work-time", "legion-rework"]);
    },
  ],
];

bunTest.each(stateRows)("%s", async (_row, run) => {
  try {
    await run();
  } finally {
    cleanup();
  }
});

// Spec R5: with no daemon, every source is missing whether the project has no settings row, has
// delivery off, or has saved no deploy source (Dispatch saves delivery on only with one).
const everySourceMissingRows: [string, ProjectDeliverySettings | null][] = [
  ["no settings row", null],
  ["delivery off", deliverySettings("ACME", { delivery: false })],
  ["no deploy source", deliverySettings("ACME", { delivery: false, sources: [] })],
];

bunTest.each(everySourceMissingRows)(
  "with no daemon and %s, one line names both settings with links, and no section shows",
  async (_row, settings) => {
    try {
      renderTab(null, "?window=28d", settings);
      const socket = await openSocket();
      const line = await waitFor(() => {
        const found = document.querySelector<HTMLElement>("[data-missing-sources]");
        expect(found?.textContent).toBe(
          "Nothing to show yet: set this project's Legion daemon or its delivery settings."
        );
        return found as HTMLElement;
      });
      expect(document.querySelectorAll("[data-missing-sources]")).toHaveLength(1);
      expect(within(line).getByRole("link", { name: "Legion daemon" })).toHaveAttribute(
        "href",
        "/settings"
      );
      expect(within(line).getByRole("link", { name: "delivery settings" })).toHaveAttribute(
        "href",
        "/projects/ACME/settings"
      );
      expect(screen.queryByRole("link", { name: "Delivery: not set up" })).toBeNull();
      expect(sectionsDrawn()).toEqual([]);
      await waitFor(() => expect(heldKinds(socket)).toEqual([]));
    } finally {
      cleanup();
    }
  }
);

test('a live section with no data reads "no data", not 0', async () => {
  renderTab();
  const socket = await openSocket();
  // The delivery view holds deploy-frequency with no series, and no deployment-rework-rate.
  await answerView(socket, "delivery");
  for (const slug of ["deploy-frequency", "deployment-rework-rate"]) {
    const card = region("Target cards").querySelector<HTMLElement>(`[data-card="${slug}"]`);
    if (card === null) throw new Error(`no ${slug} card`);
    expect(within(card).getAllByText("no data").length).toBeGreaterThan(0);
    expect(within(card).queryByText(/^0/)).toBeNull();
  }
});

// Plan D2: the header's freshness line is SourceFreshness fed by the delivery evals view's
// `delivery_freshness`, which Dispatch answers null for a project with no deploy source.
test("the header's freshness line is the delivery view's delivery_freshness, and none while it is null", async () => {
  renderTab();
  const socket = await openSocket();
  await answerView(socket, "legion");
  await waitFor(() => expect(screen.getByTestId("evals-days")).toBeTruthy());
  const sub = await answerView(socket, "delivery");
  await waitFor(() => expect(cardsDrawn()).toContain("deploy-frequency"));
  expect(screen.queryByRole("region", { name: "Source freshness" })).toBeNull();

  serve(socket, {
    ...deltaFrame(sub, []),
    set: {
      delivery_freshness: {
        last_error: "acme/deploys: rate limited",
        last_event_at: "2026-11-23T10:00:00Z",
        last_reconcile_at: "2026-11-23T10:00:00Z",
        unfetchable_count: 2,
      },
    },
  });
  const line = await screen.findByRole("region", { name: "Source freshness" });
  const prs = line.querySelector<HTMLElement>('[data-source="prs"]');
  expect(prs).toHaveAttribute("data-red", "true");
  expect(prs?.textContent).toContain("acme/deploys: rate limited");
  expect(line.querySelector('[data-source="unfetchable"]')?.textContent).toBe(
    "2 pull requests can no longer be fetched from GitHub"
  );
});

test("the four views open at once, with the Legion daemon and delivery settings reads in parallel", async () => {
  const { getLegionDaemon, getProjectDeliverySettings } = renderTab(
    "pending",
    "?window=28d",
    "pending"
  );
  const socket = await openSocket();
  await waitFor(() =>
    expect(
      heldViews(socket)
        .map((frame) => queryOf(frame).get("kind"))
        .sort()
    ).toEqual(["ci", "delivery", "experiment", "legion"])
  );
  expect(getLegionDaemon).toHaveBeenCalledWith("ACME");
  expect(getProjectDeliverySettings).toHaveBeenCalledWith("ACME");
  for (const frame of heldViews(socket)) {
    expect(frame.view).toMatchObject({ kind: "evals", project: "ACME", window: { preset: 28 } });
    expect(queryOf(frame).get("limit")).toBe("200");
    expect(queryOf(frame).has("offset")).toBe(false);
  }
  // Nothing has answered: every section that reads a view says so, and the timeline is closed.
  expect(
    [...document.querySelectorAll("[data-section]")].map((section) =>
      section.getAttribute("data-state")
    )
  ).toEqual(["subscribed", "subscribed", "subscribed", "subscribed", "closed", "subscribed"]);
});

test("a view whose total exceeds 200 subscribes the next offset as a second view of the same section", async () => {
  renderTab();
  const socket = await openSocket();
  await answerView(
    socket,
    "ci",
    answer([evalItem("pr-ci", "ci"), evalItem("staging-e2e", "ci")], 250)
  );
  const second = await subscription(socket, "ci", 200);
  expect(queryOf(second).get("limit")).toBe("200");
  serve(socket, snapshotFrame(second.sub, answer([evalItem("production-deploy", "ci")], 250, 200)));
  await waitFor(() =>
    expect(listed("CI health")).toEqual(["pr-ci", "staging-e2e", "production-deploy"])
  );
  // 250 evals are two pages: no third view.
  expect(socket.subscribes().filter((frame) => queryOf(frame).get("offset") === "400")).toEqual([]);
});

test("the tab lists no issues", async () => {
  const { listIssues } = renderTab();
  const socket = await openSocket();
  for (const kind of ["delivery", "legion", "ci", "experiment"] as const) {
    await answerView(socket, kind);
  }
  await waitFor(() =>
    expect(
      [...document.querySelectorAll("[data-section]")].map((section) =>
        section.getAttribute("data-state")
      )
    ).toEqual(["painted", "painted", "painted", "painted", "closed", "painted"])
  );
  expect(document.querySelector("[data-issue-row]")).toBeNull();
  expect(screen.queryByRole("table", { name: /issue/i })).toBeNull();
  expect(listIssues).not.toHaveBeenCalled();
});

/** `item` with one listed primary series `name` in `unit`, whose first window day holds
 *  `statistics`, as do the window's figures: an eval each section draws a chart of. */
function charted(
  item: EvalListItem,
  name: string,
  unit: string,
  statistics: Record<string, number>
): EvalListItem {
  return {
    ...item,
    series: [
      {
        agg: "quantile",
        change: null,
        compare: null,
        compare_days: [],
        created_at: "2026-10-01T00:00:00Z",
        data_since: "2026-09-01",
        days: [{ day: WINDOW.from.slice(0, 10), statistics }],
        eval_id: item.id,
        is_primary: true,
        links: [],
        listed: true,
        min_n: null,
        narrowed: null,
        role: "headline",
        series: name,
        unit,
        version: 1,
        via_repository: 0,
        window: statistics,
      },
    ],
  };
}

test("in Table mode the tab holds no [data-issue-row]", async () => {
  const user = userEvent.setup();
  renderTab();
  const socket = await openSocket();
  const timing = { median: 12, n: 4, p90: 20 };
  await answerView(
    socket,
    "delivery",
    answer([
      charted(evalItem("deploy-frequency", "delivery"), "deploys", "deploys", {
        deploys: 3,
        per_day: 3,
      }),
      charted(evalItem("merge-to-production", "delivery"), "minutes", "minutes", timing),
    ])
  );
  await answerView(
    socket,
    "legion",
    answer([charted(evalItem("legion-work-time", "legion"), "hours", "hours", timing)])
  );
  await answerView(
    socket,
    "ci",
    answer([charted(evalItem("pr-ci", "ci"), "minutes", "minutes", timing)])
  );
  await answerView(socket, "experiment");
  const toggles = await waitFor(() => {
    const found = screen.getAllByRole("button", { name: "Table" });
    expect(found.length).toBeGreaterThan(0);
    return found;
  });
  for (const toggle of toggles) await user.click(toggle);
  expect(document.querySelectorAll("figure table")).toHaveLength(toggles.length);
  expect(document.querySelector("[data-issue-row]")).toBeNull();
  expect(document.querySelectorAll("table [data-row]").length).toBeGreaterThan(0);
});
