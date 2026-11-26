import { afterEach, beforeEach, test as bunTest, expect } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";

import { evalItem, evalSeries, evalsView, itemRow, itemsPage } from "../../__tests__/evals-fixture";
import {
  type FakeLiveSocket,
  type FakeLiveSockets,
  installFakeLiveSockets,
  snapshotFrame,
} from "../../__tests__/live-socket-harness";
import type { EvalItemsResponse, EvalListItem, LiveSubscribe } from "../../api/types";
import { CiHealth } from "./CiHealth";
import type { EvalsScope } from "./useEvalsViews";

// CI health's rows (dispatch://INFRA-2918/artifact/plan-md, T11): the four timing charts from the
// CI view, and pr-ci's two job lists, each read through its items view on a fake live socket.

const SCOPE: EvalsScope = {
  compare: null,
  project: "ACME",
  repos: ["acme/widgets"],
  window: { preset: 28 },
};

/** A CI eval's timing over the window: every figure `measures.FoldTiming` states. */
function timed(slug: string, window: Record<string, number | null>, via = 0): EvalListItem {
  return evalItem(slug, "ci", [
    evalSeries("minutes", "minutes", {
      days: [{ day: "2026-11-02", statistics: window }],
      is_primary: true,
      via_repository: via,
      window,
    }),
  ]);
}

const PR_CI = {
  cancelled: 2,
  failure: 8,
  median: 12,
  n: 40,
  p90: 20,
  success: 30,
  success_rate: 0.75,
};

let sockets: FakeLiveSockets;
beforeEach(() => {
  sockets = installFakeLiveSockets();
});
afterEach(() => {
  sockets.restore();
  localStorage.clear();
});

/** A row of this file, its section unmounted inside its body. */
const test = (name: string, body: () => Promise<void>): void =>
  bunTest(name, async () => {
    try {
      await body();
    } finally {
      cleanup();
    }
  });

/** The section over a CI view holding `items`, and its live socket, open. */
async function renderSection(items: EvalListItem[]): Promise<FakeLiveSocket> {
  render(
    <QueryClientProvider
      client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
    >
      <CiHealth scope={SCOPE} views={[evalsView("ci", items)]} />
    </QueryClientProvider>
  );
  await waitFor(() => expect(sockets.sockets.length).toBeGreaterThan(0));
  const socket = sockets.latest();
  act(() => socket.open());
  return socket;
}

/** The items view of pr-ci in `sort`'s order, once the section has subscribed it. */
function jobsView(socket: FakeLiveSocket, sort: string): Promise<LiveSubscribe> {
  return waitFor(() => {
    const frame = socket
      .subscribes()
      .find(
        (sent) =>
          sent.view.kind === "items" && new URLSearchParams(sent.view.query).get("sort") === sort
      );
    if (frame === undefined) throw new Error(`no ${sort} view was subscribed`);
    return frame;
  });
}

/** Dispatch answers `frame`'s view with `body`. */
function serve(socket: FakeLiveSocket, frame: LiveSubscribe, body: EvalItemsResponse): void {
  act(() => socket.receive(snapshotFrame(frame.sub, body)));
}

function chart(title: string): HTMLElement {
  return screen.getByRole("article", { name: title });
}

/** The chart's window figure of `statistic`. */
function figure(title: string, statistic: string): string | null | undefined {
  return chart(title).querySelector(`[data-window-statistic="${statistic}"]`)?.textContent;
}

/** The cells of each row of the table named `name`, in order. */
function cells(name: string): string[][] {
  return [...screen.getByRole("table", { name }).querySelectorAll("tbody tr[data-row]")].map(
    (row) => [...row.querySelectorAll("td")].map((cell) => cell.textContent ?? "")
  );
}

test("each timing chart draws its runs, median, p90 and success counts", async () => {
  await renderSection([
    timed("production-deploy", { ...PR_CI, n: 5, success: 5, failure: 0, cancelled: 0 }),
    timed("pr-ci", PR_CI),
    timed("staging-e2e", PR_CI),
    timed("staging-deploy", PR_CI),
  ]);
  expect(
    [...document.querySelectorAll("[data-eval]")].map((drawn) => drawn.getAttribute("data-eval"))
  ).toEqual(["pr-ci", "staging-e2e", "staging-deploy", "production-deploy"]);
  expect(chart("PR CI").querySelector("figure")).toHaveAttribute("data-chart-kind", "timing");
  expect(figure("PR CI", "median")).toBe("12m");
  expect(figure("PR CI", "p90")).toBe("20m");
  expect(figure("PR CI", "n")).toBe("40");
  expect(figure("PR CI", "success")).toBe("30");
  expect(figure("PR CI", "failure")).toBe("8");
  expect(figure("PR CI", "cancelled")).toBe("2");
  expect(figure("PR CI", "success_rate")).toBe("75.0%");
  expect(figure("Deploy to production", "n")).toBe("5");
});

test("the slowest-jobs table respects the floor", async () => {
  const socket = await renderSection([timed("pr-ci", PR_CI)]);
  const slowest = await jobsView(socket, "median");
  // pr-ci's job rows, the five slowest the route ranks over the window with its floor.
  expect(slowest.view).toMatchObject({
    kind: "items",
    project: "ACME",
    slug: "pr-ci",
    window: { preset: 28 },
  });
  expect(
    new URLSearchParams(slowest.view.kind === "items" ? slowest.view.query : "").toString()
  ).toBe("repo=acme%2Fwidgets&sort=median&limit=5");
  const list = screen.getByRole("region", { name: "PR CI: slowest jobs" });
  expect(list.textContent).toContain("a job needs 50 timed results in the window to be ranked");

  serve(socket, slowest, itemsPage([]));
  await waitFor(() =>
    expect(
      within(list).getByText("No job has 50 timed results in the window yet.")
    ).toBeInTheDocument()
  );

  serve(
    socket,
    slowest,
    itemsPage(
      [
        itemRow("build / e2e", "job", {
          median_minutes: 42,
          n: 60,
          p90_minutes: 55,
          success_rate: 0.9,
        }),
        itemRow("build / unit", "job", {
          median_minutes: 9.5,
          n: 50,
          p90_minutes: 12,
          success_rate: 1,
        }),
      ],
      7,
      0,
      5
    )
  );
  await waitFor(() =>
    expect(cells("PR CI: slowest jobs")).toEqual([
      ["build / e2e", "42m", "55m", "60", "90%"],
      ["build / unit", "9.5m", "12m", "50", "100%"],
    ])
  );
});

test("the failing table lists failures and results", async () => {
  const socket = await renderSection([timed("pr-ci", PR_CI)]);
  const failing = await jobsView(socket, "failures");
  expect(
    new URLSearchParams(failing.view.kind === "items" ? failing.view.query : "").get("limit")
  ).toBe("200");
  // Two and a half hours before the section was drawn, which its age is measured from.
  const lastFailed = new Date(Date.now() - 2.5 * 3600 * 1000).toISOString();
  serve(
    socket,
    failing,
    itemsPage([
      itemRow("acme/widgets/build / lint", "job", {
        failures: 3,
        last_failed_at: lastFailed,
        last_run_url: "https://github.com/acme/widgets/actions/runs/51",
        name: "build / lint",
        repo: "acme/widgets",
        results: 4,
      }),
    ])
  );
  await waitFor(() =>
    expect(cells("PR CI: jobs that failed in the last 24 hours")).toEqual([
      ["build / lint", "acme/widgets", "3", "4", "75%", "2 h ago"],
    ])
  );
  expect(
    within(
      screen.getByRole("table", { name: "PR CI: jobs that failed in the last 24 hours" })
    ).getByRole("link", { name: "2 h ago" })
  ).toHaveAttribute("href", "https://github.com/acme/widgets/actions/runs/51");
});

test("a job of one name that failed in two repositories draws a row of each, under its name and its repository", async () => {
  const socket = await renderSection([timed("pr-ci", PR_CI)]);
  const failing = await jobsView(socket, "failures");
  const lastFailed = new Date(Date.now() - 2.5 * 3600 * 1000).toISOString();
  // Each row keyed `<repo>/<name>`, as the items route keys a failing job.
  const job = (repo: string, failures: number, results: number) =>
    itemRow(`${repo}/build / lint`, "job", {
      failures,
      last_failed_at: lastFailed,
      last_run_url: `https://github.com/${repo}/actions/runs/51`,
      name: "build / lint",
      repo,
      results,
    });
  serve(socket, failing, itemsPage([job("acme/widgets", 3, 4), job("acme/gadgets", 1, 5)]));
  await waitFor(() =>
    expect(cells("PR CI: jobs that failed in the last 24 hours")).toEqual([
      ["build / lint", "acme/widgets", "3", "4", "75%", "2 h ago"],
      ["build / lint", "acme/gadgets", "1", "5", "20%", "2 h ago"],
    ])
  );
});

test('a staging chart whose eval has no data reads "no data", never 0, and an eval the view lacks draws none', async () => {
  const none = {
    cancelled: null,
    failure: null,
    median: null,
    n: null,
    p90: null,
    success: null,
    success_rate: null,
  };
  await renderSection([
    timed("pr-ci", PR_CI),
    evalItem("staging-e2e", "ci", [
      evalSeries("minutes", "minutes", { is_primary: true, window: none }),
    ]),
  ]);
  expect(
    [...document.querySelectorAll("[data-eval]")].map((drawn) => drawn.getAttribute("data-eval"))
  ).toEqual(["pr-ci", "staging-e2e"]);
  for (const statistic of ["median", "p90", "n", "success"]) {
    expect(figure("Staging end-to-end gate", statistic)).toBe("no data");
  }
  expect(within(chart("Staging end-to-end gate")).queryByText(/^0/)).toBeNull();
  expect(
    chart("Staging end-to-end gate").querySelectorAll('[data-range="window"][data-value]')
  ).toHaveLength(0);
});

test("pr-ci's timing with a via_repository count of 2 shows the note, and one of 0 none", async () => {
  await renderSection([timed("pr-ci", PR_CI, 2), timed("staging-e2e", PR_CI, 0)]);
  const figures = chart("PR CI").querySelector('[data-window-series="minutes"]');
  expect(figures?.querySelector('[data-note="repository"]')?.textContent).toBe(
    "2 by default repository"
  );
  expect(chart("Staging end-to-end gate").querySelector('[data-note="repository"]')).toBeNull();
});
