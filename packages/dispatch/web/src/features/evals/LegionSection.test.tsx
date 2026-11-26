import { afterEach, test as bunTest, expect, spyOn } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";

import { deliverySettings } from "../../__tests__/delivery-settings";
import {
  evalItem,
  evalSeries,
  evalsView,
  itemRow,
  itemsPage,
  WINDOW,
} from "../../__tests__/evals-fixture";
import { installFakeLiveSockets } from "../../__tests__/live-socket-harness";
import { api } from "../../api/client";
import { EvalsTab } from "./EvalsTab";
import { LegionSection } from "./LegionSection";

// The Legion section's rows (dispatch://INFRA-2918/artifact/plan-md, T9). Main's issue table takes
// no done view and no closed-day filter, so each group link opens the issues it counts by their
// keys (`only=`), read from the eval's rows over the days the number names (plan D13).

/** Work time over the window's three days: two issues on the first day, one on the second. */
const WORK_TIME = evalItem("legion-work-time", "legion", [
  evalSeries("hours", "hours", {
    days: [
      { day: "2026-11-01", statistics: { median: 4, n: 2, p90: 6 } },
      { day: "2026-11-02", statistics: { median: 3, n: 1, p90: 3 } },
    ],
    is_primary: true,
    window: { median: 3.5, n: 3, p90: 6 },
  }),
  evalSeries("hours:roots", "hours", {
    days: [{ day: "2026-11-02", statistics: { median: 3, n: 1, p90: 3 } }],
    window: { median: 3, n: 1, p90: 3 },
  }),
]);

/** A phase-time bar of `hours`, `issues` of them holding hours on it. */
function bar(role: string, hours: number, issues: number) {
  return evalSeries(role, "hours", {
    window: {
      ci_wait: 0,
      hours,
      idle: hours / 2,
      idle_share: 0.25,
      in_a_turn: hours / 2,
      issues,
      median_per_issue: hours / issues,
      n: 3,
      waiting_on_children: 0,
      waiting_on_person: 0,
    },
  });
}

const LEGION = [
  WORK_TIME,
  evalItem("legion-phase-time", "legion", [bar("implementer", 10, 2), bar("reviewer", 2, 1)]),
  evalItem("legion-idle-time", "legion", [
    evalSeries("implementer", "hours", { window: { hours: 1, median: 0.5, n: 3, p90: 0.8 } }),
  ]),
  evalItem("legion-slot-wait", "legion", [
    evalSeries("hours", "hours", { window: { hours: 3, median: 1, n: 3, p90: 2 } }),
  ]),
  evalItem("legion-rework", "legion", [
    evalSeries("review_rounds", "rounds", {
      days: [{ day: "2026-11-01", statistics: { median: 1, n: 2, p90: 2 } }],
      is_primary: true,
      window: { median: 1, n: 3, p90: 2 },
    }),
    evalSeries("sent_back", "ratio", { window: { n: 3, sent_back: 1, share: 1 / 3 } }),
  ]),
  evalItem("legion-splitting", "legion", [
    evalSeries("finished", "issues", { window: { issues: 3 } }),
  ]),
];

/** Where the section's links land: the issue table's address. */
function Landed() {
  const location = useLocation();
  return <p data-testid="landed">{`${location.pathname}${location.search}`}</p>;
}

function renderSection(): void {
  render(
    <MemoryRouter initialEntries={["/projects/ACME/evals"]}>
      <Routes>
        <Route
          element={
            <LegionSection
              project="ACME"
              repos={["acme/widgets"]}
              views={[evalsView("legion", LEGION)]}
            />
          }
          path="/projects/:key/evals"
        />
        <Route element={<Landed />} path="/projects/:key/issues" />
      </Routes>
    </MemoryRouter>
  );
}

function chart(title: string): HTMLElement {
  return screen.getByRole("article", { name: title });
}

/** The address the issue table opened at, once a link has opened it. */
async function landed(): Promise<URL> {
  return new URL((await screen.findByTestId("landed")).textContent ?? "", "http://dispatch");
}

const spies: { mockRestore: () => void }[] = [];
afterEach(() => {
  for (const spy of spies.splice(0)) spy.mockRestore();
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

test("the six Legion evals each draw a chart, in order, and work time's and rework's tables hold one row per UTC day of the window", async () => {
  const user = userEvent.setup();
  renderSection();
  expect(
    [...document.querySelectorAll("[data-eval]")].map((drawn) => drawn.getAttribute("data-eval"))
  ).toEqual([
    "legion-work-time",
    "legion-phase-time",
    "legion-idle-time",
    "legion-slot-wait",
    "legion-rework",
    "legion-splitting",
  ]);
  for (const drawn of document.querySelectorAll("[data-eval]")) {
    expect(drawn.querySelector("figure")?.getAttribute("data-chart")).toBe(
      drawn.getAttribute("data-eval")
    );
  }
  for (const title of ["Work time", "Rework"]) {
    await user.click(within(chart(title)).getByRole("button", { name: "Table" }));
    const rows = within(chart(title)).getByRole("table").querySelectorAll("[data-row]");
    expect([...rows].map((row) => row.getAttribute("data-row"))).toEqual([
      "2026-11-01",
      "2026-11-02",
      "2026-11-03",
    ]);
  }
  // The tab lists no issue: no row of the issue table's kind.
  expect(document.querySelector("[data-issue-row]")).toBeNull();
});

test("a day row's link opens the issue table on that day's issues, by key", async () => {
  const user = userEvent.setup();
  const listEvalItems = spyOn(api, "listEvalItems").mockResolvedValue(
    itemsPage([
      itemRow("ACME-1", "issue", { started_at: "2026-10-30T10:00:00Z" }),
      itemRow("ACME-2", "issue", { started_at: "2026-10-31T10:00:00Z" }),
      // No first worker event: no work time, so the day's count leaves it out and so does its link.
      itemRow("ACME-3", "issue", { started_at: null }),
    ])
  );
  spies.push(listEvalItems);
  renderSection();
  await user.click(within(chart("Work time")).getByRole("button", { name: "Table" }));
  const day = within(chart("Work time"))
    .getByRole("table")
    .querySelector('[data-row="2026-11-01"]');
  if (!(day instanceof HTMLElement)) throw new Error("no row of 2026-11-01");
  const link = within(day).getByRole("button", { name: "Open the 2 issues" });
  expect(link).toHaveAttribute("data-group-count", "2");
  await user.click(link);

  const address = await landed();
  expect(address.pathname).toBe("/projects/ACME/issues");
  expect(address.searchParams.get("only")).toBe("ACME-1,ACME-2");
  expect(address.searchParams.get("onlyLabel")).toBe("Work time, 2026-11-01");
  expect(address.searchParams.get("nofilter")).toBe("1");
  expect(listEvalItems.mock.calls).toEqual([
    [
      "ACME",
      "legion-work-time",
      {
        from: "2026-11-01T00:00:00Z",
        limit: 200,
        offset: 0,
        repo: ["acme/widgets"],
        row_kind: "issue",
        series: "hours",
        to: "2026-11-02T00:00:00Z",
      },
    ],
  ]);
});

test("a headline's link names the window's days and the cut", async () => {
  const user = userEvent.setup();
  const listEvalItems = spyOn(api, "listEvalItems").mockResolvedValue(
    itemsPage([itemRow("ACME-5", "issue", { started_at: "2026-11-01T09:00:00Z" })])
  );
  spies.push(listEvalItems);
  renderSection();
  await user.selectOptions(screen.getByLabelText("Legion issues"), "roots");
  await user.click(within(chart("Work time")).getByRole("button", { name: "Open the issue" }));

  const address = await landed();
  expect(address.searchParams.get("only")).toBe("ACME-5");
  expect(address.searchParams.get("onlyLabel")).toBe("Work time, 2026-11-01 to 2026-11-03, roots");
  expect(listEvalItems.mock.calls.map(([, , query]) => query)).toEqual([
    {
      from: WINDOW.from,
      limit: 200,
      offset: 0,
      repo: ["acme/widgets"],
      row_kind: "issue",
      series: "hours:roots",
      to: WINDOW.to,
    },
  ]);
});

test("a phase part opens the issues holding hours on its bar", async () => {
  const user = userEvent.setup();
  const listEvalItems = spyOn(api, "listEvalItems").mockResolvedValue(
    itemsPage([
      itemRow("ACME-1", "issue", { bar_implementer: 6 }),
      itemRow("ACME-2", "issue", { bar_implementer: 0 }),
      itemRow("ACME-3", "issue", { bar_implementer: 4 }),
    ])
  );
  spies.push(listEvalItems);
  renderSection();
  const phase = chart("Phase time by role");
  // One bar per role the eval holds, each split into its segments.
  expect(
    [...phase.querySelectorAll('[data-statistic="hours"][data-range="window"]')].map((value) =>
      value.getAttribute("data-x")
    )
  ).toEqual(["implementer", "reviewer"]);
  expect(phase.querySelector('[data-x="implementer"][data-statistic="in_a_turn"]')).toHaveAttribute(
    "data-value",
    "5"
  );

  await user.click(within(phase).getByRole("button", { name: "Table" }));
  const role = within(phase).getByRole("table").querySelector('[data-row="implementer"]');
  if (!(role instanceof HTMLElement)) throw new Error("no implementer row");
  await user.click(within(role).getByRole("button", { name: "Open the 2 issues" }));

  const address = await landed();
  expect(address.searchParams.get("only")).toBe("ACME-1,ACME-3");
  expect(address.searchParams.get("onlyLabel")).toBe(
    "Phase time by role: Implementer, 2026-11-01 to 2026-11-03"
  );
  expect(listEvalItems.mock.calls.map(([, slug, query]) => [slug, query?.series])).toEqual([
    ["legion-phase-time", "implementer"],
  ]);
});

test("the section is absent with no daemon", async () => {
  const sockets = installFakeLiveSockets();
  const getLegionDaemon = spyOn(api, "getLegionDaemon").mockResolvedValue(null);
  const settings = spyOn(api, "getProjectDeliverySettings").mockResolvedValue(
    deliverySettings("ACME")
  );
  spies.push(getLegionDaemon, settings, { mockRestore: sockets.restore });
  render(
    <MemoryRouter initialEntries={["/projects/ACME/evals?window=28d"]}>
      <QueryClientProvider
        client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
      >
        <EvalsTab project="ACME" />
      </QueryClientProvider>
    </MemoryRouter>
  );
  await waitFor(() => expect(sockets.sockets.length).toBeGreaterThan(0));
  const socket = sockets.latest();
  act(() => socket.open());
  await waitFor(() => expect(getLegionDaemon).toHaveBeenCalledWith("ACME"));
  await waitFor(() => expect(screen.queryByRole("region", { name: "Legion measures" })).toBeNull());
  expect(document.querySelector("[data-eval^='legion-']")).toBeNull();
  // Until the daemon read answers the section shows, so its view may be opened; then it goes.
  await waitFor(() => {
    const dropped = new Set(socket.unsubscribes().map((frame) => frame.sub));
    expect(
      socket
        .subscribes()
        .filter((frame) => !dropped.has(frame.sub))
        .map((frame) =>
          frame.view.kind === "evals" ? new URLSearchParams(frame.view.query).get("kind") : null
        )
    ).not.toContain("legion");
  });
});
