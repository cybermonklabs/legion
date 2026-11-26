import { afterEach, beforeEach, test as bunTest, expect, spyOn } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { deliverySettings } from "../../__tests__/delivery-settings";
import {
  evalItem,
  evalSeries,
  evalsAnswer,
  evalsView,
  itemRow,
  itemsPage,
  WINDOW,
} from "../../__tests__/evals-fixture";
import {
  type FakeLiveSocket,
  type FakeLiveSockets,
  installFakeLiveSockets,
  snapshotFrame,
} from "../../__tests__/live-socket-harness";
import { api } from "../../api/client";
import type { EvalKind } from "../../api/queries";
import type { EvalListItem, EvalListResponse, LegionSetting } from "../../api/types";
import { successText } from "../../theme/classes";
import { EvalsTab } from "./EvalsTab";
import { LoggedEvals, UnnamedEvals } from "./EveryEval";
import type { EvalsScope } from "./useEvalsViews";

// Every registered eval on the tab (dispatch://INFRA-2918/artifact/plan-md, D7, T17; spec R19,
// R2): each logged eval, whatever its kind, in the Logged section after the merge timeline, with
// its chart, its table and its rows; a computed eval no section draws by name at the end of its
// kind's section; and every eval of the list read drawn somewhere on the tab.

const SCOPE: EvalsScope = { compare: null, project: "ACME", repos: [], window: { preset: 28 } };

const DAEMON: LegionSetting = {
  created_by: { id: "alice", kind: "user" },
  project: "ACME",
  state_url: "http://127.0.0.1:9",
  updated_at: "2026-10-01T00:00:00Z",
};

/** The eval `slug` of `kind` as an agent or a job logs it: its source `logged`, registered by its
 *  first reading (no name, no unit, no target) unless `definition` says otherwise. */
function logged(
  slug: string,
  kind: EvalKind,
  series: EvalListItem["series"] = [],
  definition: Partial<EvalListItem["definition"]> = {}
): EvalListItem {
  const item = evalItem(slug, kind, series ?? []);
  return {
    ...item,
    definition: {
      ...item.definition,
      name: "",
      source: "logged",
      unit: "",
      written_via: "reading",
      ...definition,
    },
  };
}

/** A logged series of widgets, folded by its last value, holding 3 on the window's first day and
 *  5 on its second. */
const WIDGETS = evalSeries("widgets", "", {
  agg: "last",
  days: [
    { day: "2026-11-01", statistics: { n: 1, value: 3 } },
    { day: "2026-11-02", statistics: { n: 2, value: 5 } },
  ],
  is_primary: true,
  window: { n: 3, value: 5 },
});

let sockets: FakeLiveSockets;
const spies: { mockRestore: () => void }[] = [];
beforeEach(() => {
  sockets = installFakeLiveSockets();
  const getEval = spyOn(api, "getEval").mockResolvedValue({
    compare: null,
    doc_text: null,
    freshness: { refresher_age_seconds: null, rows_updated_at: null },
    items: [],
    limit: 1,
    offset: 0,
    total: 1,
    window: WINDOW,
  });
  spies.push(getEval);
});
afterEach(() => {
  sockets.restore();
  for (const spy of spies.splice(0)) spy.mockRestore();
  localStorage.clear();
});

/** A row of this file, its page unmounted inside its body. */
const test = (name: string, body: () => Promise<void>): void =>
  bunTest(name, async () => {
    try {
      await body();
    } finally {
      cleanup();
    }
  });

/** The page's live socket, once it has opened one. */
async function openSocket(): Promise<FakeLiveSocket> {
  await waitFor(() => expect(sockets.sockets.length).toBeGreaterThan(0));
  const socket = sockets.latest();
  act(() => socket.open());
  return socket;
}

/** The tab of a project with a Legion daemon and delivery on, and its live socket, open. */
async function renderTab(): Promise<FakeLiveSocket> {
  const daemon = spyOn(api, "getLegionDaemon").mockResolvedValue(DAEMON);
  const settings = spyOn(api, "getProjectDeliverySettings").mockResolvedValue(
    deliverySettings("ACME")
  );
  spies.push(daemon, settings);
  render(
    <MemoryRouter initialEntries={["/projects/ACME/evals?window=28d"]}>
      <QueryClientProvider
        client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
      >
        <EvalsTab project="ACME" />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return openSocket();
}

/** Dispatch answers the tab's evals view of `kind` with `items`. */
async function answerView(socket: FakeLiveSocket, kind: EvalKind, items: EvalListItem[]) {
  const frame = await waitFor(() => {
    const sent = socket
      .subscribes()
      .find(
        (subscribe) =>
          subscribe.view.kind === "evals" &&
          new URLSearchParams(subscribe.view.query).get("kind") === kind
      );
    if (sent === undefined) throw new Error(`no ${kind} view was subscribed`);
    return sent;
  });
  act(() => socket.receive(snapshotFrame(frame.sub, evalsAnswer(items))));
}

function region(name: string): HTMLElement {
  return screen.getByRole("region", { name });
}

/** The evals a section draws, by slug, in order. */
function listed(name: string): string[] {
  return [...region(name).querySelectorAll("[data-eval]")].map(
    (box) => box.getAttribute("data-eval") ?? ""
  );
}

/** The Logged section over `views`, outside the tab, and its live socket, open. */
async function renderLogged(
  items: EvalListItem[]
): Promise<{ socket: FakeLiveSocket; redraw: (next: EvalListItem[]) => void }> {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const page = (next: EvalListItem[]) => (
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <LoggedEvals scope={SCOPE} views={[evalsView("experiment", next)]} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  const { rerender } = render(page(items));
  return { redraw: (next) => rerender(page(next)), socket: await openSocket() };
}

test("a logged eval of kind ci draws in the Logged section after the merge timeline, not in CI health", async () => {
  const socket = await renderTab();
  await answerView(socket, "ci", [
    evalItem("pr-ci", "ci", []),
    logged("flaky-tests", "ci", [WIDGETS]),
  ]);
  await waitFor(() => expect(listed("Logged")).toEqual(["flaky-tests"]));
  expect(listed("CI health")).toEqual(["pr-ci"]);
  const sections = [...document.querySelectorAll("[data-section]")];
  expect(sections.at(-1)).toBe(region("Logged"));
  expect(sections.indexOf(region("Logged"))).toBe(sections.indexOf(region("Merge timeline")) + 1);
});

test("a logged eval with readings and no registry row draws its chart, its table and its rows", async () => {
  const user = userEvent.setup();
  const { socket } = await renderLogged([logged("widget-size", "experiment", [WIDGETS])]);
  const box = screen.getByRole("article", { name: "widget-size" });

  // Its chart: the per-day line of its series, each day's value named in the DOM.
  const chart = within(box).getByRole("figure", { name: "widget-size" });
  expect(chart).toHaveAttribute("data-chart", "widget-size");
  expect(chart).toHaveAttribute("data-chart-kind", "line");
  const drawn = [...chart.querySelectorAll('[data-series="widgets"][data-value]')].map((value) => [
    value.getAttribute("data-x"),
    value.getAttribute("data-value"),
  ]);
  expect(drawn).toEqual(
    expect.arrayContaining([
      ["2026-11-01", "3"],
      ["2026-11-02", "5"],
    ])
  );

  // Its table: one row a day, holding each value the line drew.
  await user.click(within(chart).getByRole("button", { name: "Table" }));
  const table = within(chart).getByRole("table");
  expect(
    [...table.querySelectorAll('[data-range="window"][data-statistic="value"][data-value]')].map(
      (value) => [value.getAttribute("data-x"), value.getAttribute("data-value")]
    )
  ).toEqual([
    ["2026-11-01", "3"],
    ["2026-11-02", "5"],
  ]);

  // Its rows: the readings behind it, through its items view, each as logged.
  const items = await waitFor(() => socket.subscribed("items"));
  expect(items.view).toMatchObject({ kind: "items", project: "ACME", slug: "widget-size" });
  const reading = {
    ...itemRow("widget-1", "row", { id: "reading-1", series: "widgets", value: 5 }),
    href: "https://results.example.com/widget-1",
    links: [
      {
        href: "https://results.example.com/widget-1",
        label: "Widget 1 run",
        recorded_at: "2026-11-02T10:00:00Z",
        rel: "via",
      },
    ],
  };
  act(() => socket.receive(snapshotFrame(items.sub, itemsPage([reading]))));
  const row = await waitFor(() => {
    const found = box.querySelector('[data-reading="row"]');
    if (!(found instanceof HTMLElement)) throw new Error("the box lists no reading yet");
    return found;
  });
  expect(row.textContent).toContain("widgets: 5");
  expect(within(row).getByRole("link", { name: "Widget 1 run" })).toHaveAttribute(
    "data-link-kind",
    "via"
  );
});

test("after registration its target, its unit and its label draw", async () => {
  const before = logged("widget-size", "experiment", [WIDGETS]);
  const { redraw } = await renderLogged([before]);
  const facts = () => screen.getByRole("article").querySelector("[data-eval-facts]")?.textContent;
  expect(screen.getByRole("article", { name: "widget-size" })).toBeInTheDocument();
  expect(facts()).toBe("no target");

  // The register write names it, gives its unit and sets its target: a version of its own.
  redraw([
    {
      ...logged("widget-size", "experiment", [WIDGETS], {
        direction: "higher_better",
        name: "Widget size",
        target: 4,
        unit: "widgets",
        version: 2,
        written_via: "register",
      }),
      verdict: {
        direction: "higher_better",
        inclusive: true,
        judges: [{ judge: "widgets.value", met: true, value: 5 }],
        met: true,
        target: 4,
      },
    },
  ]);
  const box = screen.getByRole("article", { name: "Widget size" });
  expect(within(box).getByRole("figure", { name: "Widget size" })).toBeInTheDocument();
  expect(facts()).toBe("in widgets \u00b7 target widgets.value \u2265 4");
  // The window's value meets the target, and is drawn so.
  const value = box.querySelector('[data-window-statistic="value"]');
  expect(value?.textContent).toBe("5");
  expect(value).toHaveClass(successText);
});

test("every eval of the list read appears on the tab, counted against the read's total", async () => {
  const socket = await renderTab();
  const answers: Record<EvalKind, EvalListItem[]> = {
    ci: [evalItem("pr-ci", "ci", []), logged("flaky-tests", "ci")],
    delivery: [
      evalItem("deploy-frequency", "delivery", []),
      // A later catalogue's eval, which no section draws by name.
      evalItem("hotfix-share", "delivery", [WIDGETS]),
      logged("release-notes", "delivery"),
      // Drawn by the target cards alone.
      evalItem("unowned-p0", "delivery", []),
    ],
    experiment: [logged("widget-size", "experiment"), evalItem("ab-uplift", "experiment", [])],
    legion: [
      evalItem("legion-new-measure", "legion", []),
      evalItem("legion-work-time", "legion", []),
    ],
  };
  const read: EvalListResponse[] = [];
  for (const kind of ["delivery", "legion", "ci", "experiment"] as const) {
    await answerView(socket, kind, answers[kind]);
    read.push(evalsAnswer(answers[kind]));
  }
  await waitFor(() =>
    expect(
      [...document.querySelectorAll("[data-section]")].map((section) =>
        section.getAttribute("data-state")
      )
    ).toEqual(["painted", "painted", "painted", "painted", "closed", "painted"])
  );

  const drawn = new Set(
    [...document.querySelectorAll("[data-eval], [data-card]")].map(
      (box) => box.getAttribute("data-eval") ?? box.getAttribute("data-card") ?? ""
    )
  );
  const slugs = read.flatMap((answer) => (answer.items ?? []).map((item) => item.definition.slug));
  expect(slugs.filter((slug) => !drawn.has(slug))).toEqual([]);
  expect(slugs.filter((slug) => drawn.has(slug))).toHaveLength(
    read.reduce((total, answer) => total + answer.total, 0)
  );

  // A computed eval no section draws by name, at the end of its kind's section.
  expect(listed("Deploys and lead time").at(-1)).toBe("hotfix-share");
  expect(listed("Legion measures").at(-1)).toBe("legion-new-measure");
  // The Logged section: every logged eval of every view, and every experiment, by slug.
  expect(listed("Logged")).toEqual(["ab-uplift", "flaky-tests", "release-notes", "widget-size"]);
});

// dispatch://INFRA-3206: the two code evals draw as the cards of Deploys and lead time, so the
// section's evals no section draws by name hold neither (NAMED.delivery).
test("UnnamedEvals draws neither code eval", async () => {
  render(
    <MemoryRouter>
      <QueryClientProvider
        client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
      >
        <UnnamedEvals
          kind="delivery"
          scope={SCOPE}
          views={[
            evalsView("delivery", [
              evalItem("code-survival-30d", "delivery", [WIDGETS]),
              evalItem("hotfix-share", "delivery", [WIDGETS]),
              evalItem("post-merge-rework-30d", "delivery", [WIDGETS]),
            ]),
          ]}
        />
      </QueryClientProvider>
    </MemoryRouter>
  );
  expect(
    [...document.querySelectorAll("[data-eval]")].map((box) => box.getAttribute("data-eval"))
  ).toEqual(["hotfix-share"]);
});
