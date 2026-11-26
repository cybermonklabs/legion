import { afterEach, test as bunTest, expect, spyOn } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";

import { installFakeLiveSockets, snapshotFrame } from "../../__tests__/live-socket-harness";
import { api } from "../../api/client";
import type { EvalListItem, EvalListResponse, LiveSubscribe } from "../../api/types";
import type { EvalSeriesValues } from "./chart-model";
import { EvalsSection } from "./EvalsSection";
import { TargetCards } from "./TargetCards";
import type { EvalsScope, EvalsViewFacets, EvalsViewState } from "./useEvalsViews";

// The target cards' rows (dispatch://INFRA-2918/artifact/plan-md, T8), and the P0 link's rows of
// "The state table of the drill-down's controls and the P0 link", each read answered by a fake
// client in the order the row names.

const WINDOW = { days: 29, from: "2026-10-26T00:00:00Z", to: "2026-11-24T00:00:00Z" };
const COMPARE = { days: 29, from: "2026-09-27T00:00:00Z", to: "2026-10-26T00:00:00Z" };

type Verdict = EvalListItem["verdict"];

/** A series as the list read answers it: its input began before the window, and no row of it is
 *  in the window unless the row says so. */
function series(
  name: string,
  unit: string,
  overrides: Partial<EvalSeriesValues>
): EvalSeriesValues {
  return {
    agg: "quantile",
    change: null,
    compare: null,
    compare_days: null,
    created_at: "2026-10-01T00:00:00Z",
    data_since: "2026-09-01",
    days: [],
    eval_id: "ACME/eval",
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
    window: null,
    ...overrides,
  };
}

/** The eval `slug` of `kind`, its primary series `primary`, judged by `verdict`. */
function evalItem(
  slug: string,
  kind: "delivery" | "legion",
  primary: EvalSeriesValues,
  verdict: Verdict = null
): EvalListItem {
  const written = { id: "alice", kind: "user" } as const;
  return {
    definition: {
      answer: "",
      created_at: "2026-10-01T00:00:00Z",
      decision: "",
      direction: verdict?.direction ?? "lower_better",
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
      source: "computed",
      state: "active",
      target: verdict?.target ?? null,
      target_inclusive: verdict?.inclusive ?? false,
      target_judges: (verdict?.judges ?? []).map((judge) => judge.judge),
      unit: primary.unit,
      version: 1,
      written_by: written,
      written_via: "seed",
    },
    id: `ACME/${slug}`,
    owner: { person: "alice", role: "", written_by: written },
    series: [primary],
    verdict,
  };
}

/** The answer of a view that holds `items`. */
function answerOf(items: EvalListItem[]): EvalListResponse {
  return {
    compare: COMPARE,
    delivery_freshness: null,
    freshness: { refresher_age_seconds: null, rows_updated_at: null },
    items,
    limit: 200,
    offset: 0,
    total: items.length,
    window: WINDOW,
  };
}

/** A view of `kind` that answered `items`, the facets standing as `facets` (none where absent). */
function view(
  kind: "delivery" | "legion",
  items: EvalListItem[],
  facets?: EvalsViewFacets
): EvalsViewState {
  return {
    answer: answerOf(items),
    facets,
    items,
    kind,
    retry: () => undefined,
    status: "painted",
  };
}

/** unowned-p0 counting `count` issues now. */
function unowned(count: number): EvalListItem {
  return evalItem(
    "unowned-p0",
    "delivery",
    series("issues", "issues", {
      agg: "last",
      days: [{ day: "2026-11-23", statistics: { value: count } }],
      window: { value: count },
    })
  );
}

/** Where the cards' links land: the issue table's address. */
function Landed() {
  const location = useLocation();
  return <p data-testid="landed">{`${location.pathname}${location.search}`}</p>;
}

function renderCards(views: EvalsViewState[], delivery = true): void {
  render(
    <MemoryRouter initialEntries={["/projects/ACME/evals"]}>
      <Routes>
        <Route
          element={<TargetCards delivery={delivery} project="ACME" views={views} />}
          path="/projects/:key/evals"
        />
        <Route element={<Landed />} path="/projects/:key/issues" />
      </Routes>
    </MemoryRouter>
  );
}

function card(slug: string): HTMLElement {
  const found = document.querySelector<HTMLElement>(`[data-card="${slug}"]`);
  if (found === null) throw new Error(`no ${slug} card`);
  return found;
}

/** The card's window figure of `statistic`. */
function figure(slug: string, statistic: string): string | null | undefined {
  return card(slug).querySelector(`[data-window-statistic="${statistic}"]`)?.textContent;
}

/** One page of unowned-p0's rows: the issues `keys`, of `total` in all. */
function itemsPage(keys: string[], total = keys.length, offset = 0) {
  return {
    compare: null,
    items: keys.map((key) => ({
      cols: { title: `Issue ${key}` },
      href: `/issues/${key}`,
      links: [],
      measured_at: "2026-11-23T10:00:00Z",
      range: "window" as const,
      row_key: key,
      row_kind: "issue",
      source: "delivery.ListUnownedP0",
    })),
    limit: 200,
    offset,
    total,
    window: WINDOW,
  };
}

const spies: { mockRestore: () => void }[] = [];
afterEach(() => {
  for (const spy of spies.splice(0)) spy.mockRestore();
  localStorage.clear();
});

/** A row of this file, its cards unmounted inside its body. */
const test = (name: string, body: () => Promise<void>): void =>
  bunTest(name, async () => {
    try {
      await body();
    } finally {
      cleanup();
    }
  });

test('a card reads "no data" for absent day rows and never 0', async () => {
  renderCards([
    view("delivery", [
      evalItem(
        "deploy-frequency",
        "delivery",
        series("deploys", "deploys", {
          agg: "sum",
          window: { deploys: null, per_day: null, with_prs: null, with_prs_per_day: null },
        })
      ),
    ]),
  ]);
  expect(figure("deploy-frequency", "per_day")).toBe("no data");
  expect(figure("deploy-frequency", "deploys")).toBe("no data");
  expect(within(card("deploy-frequency")).queryByText(/^0/)).toBeNull();
  // No day of the window holds a row: the graph draws no value at all.
  expect(
    card("deploy-frequency").querySelectorAll('[data-range="window"][data-value]')
  ).toHaveLength(0);
  // An eval the view does not hold reads the same.
  expect(within(card("deployment-rework-rate")).getByText("no data")).toBeInTheDocument();
});

test("a window before data_since names that day", async () => {
  renderCards([
    view("delivery", [
      evalItem(
        "pr-open-to-merge",
        "delivery",
        series("minutes", "minutes", {
          data_since: "2026-11-10",
          days: [{ day: "2026-11-12", statistics: { max: 50, median: 45, n: 1, p90: 50 } }],
          window: { max: 50, median: 45, n: 1, p90: 50 },
        })
      ),
    ]),
  ]);
  expect(
    within(card("pr-open-to-merge")).getByText("no data before 2026-11-10")
  ).toBeInTheDocument();
  expect(figure("pr-open-to-merge", "median")).toBe("45m");
});

test("a value of 0 shows 0", async () => {
  renderCards([view("delivery", [unowned(0)])]);
  expect(figure("unowned-p0", "value")).toBe("0");
  // Nothing to open: no link to an empty list.
  expect(card("unowned-p0").querySelector("[data-group-count]")).toBeNull();
});

test("a via-repository count of 2 shows the note with 2 and a count of 0 none", async () => {
  const leadTime = (slug: string, via: number) =>
    evalItem(
      slug,
      "delivery",
      series("minutes", "minutes", {
        via_repository: via,
        window: { max: 300, median: 120, n: 4, p90: 240 },
      })
    );
  renderCards([
    view("delivery", [leadTime("merge-to-production", 2), leadTime("pr-open-to-merge", 0)]),
  ]);
  const figures = card("merge-to-production").querySelector('[data-window-series="minutes"]');
  expect(figures?.querySelector('[data-window-statistic="median"]')?.textContent).toBe("2.0h");
  expect(figures?.querySelector('[data-note="repository"]')?.textContent).toBe(
    "2 by default repository"
  );
  expect(card("pr-open-to-merge").querySelector('[data-note="repository"]')).toBeNull();
  expect(within(card("pr-open-to-merge")).queryByText(/by default repository/)).toBeNull();
});

test("each card's verdict", async () => {
  const judged = (met: boolean | null, value: number | null): Verdict => ({
    direction: "higher_better",
    inclusive: true,
    judges: [{ judge: "deploys.per_day", met, value }],
    met,
    target: 1,
  });
  const deploys = (perDay: number | null) =>
    series("deploys", "deploys", { agg: "sum", window: { deploys: perDay, per_day: perDay } });
  const rate = (value: number) =>
    series("per_deploy", "ratio", { agg: "ratio", window: { failed: 1, rate: value, total: 20 } });
  renderCards([
    view("delivery", [
      evalItem("deploy-frequency", "delivery", deploys(2), judged(true, 2)),
      evalItem("change-fail-rate", "delivery", rate(0.05), {
        direction: "lower_better",
        inclusive: false,
        judges: [{ judge: "per_deploy.rate", met: false, value: 0.05 }],
        met: false,
        target: 0.05,
      }),
      evalItem("deploy-run-success", "delivery", series("runs", "ratio", {}), {
        direction: "higher_better",
        inclusive: true,
        judges: [{ judge: "runs.rate", met: null, value: null }],
        met: null,
        target: 0.9,
      }),
      evalItem("failed-deploy-recovery", "delivery", series("minutes", "minutes", {})),
    ]),
  ]);
  const badge = (slug: string) => card(slug).querySelector("[data-target-met]");
  // Met: the badge, and the judged figure in its tone, written on its side of the target.
  expect(badge("deploy-frequency")?.getAttribute("data-target-met")).toBe("true");
  expect(badge("deploy-frequency")?.textContent).toBe("\u2713 met");
  expect(within(card("deploy-frequency")).getByText("target \u2265 1/day")).toBeInTheDocument();
  // Missed on a strict target it sits on.
  expect(badge("change-fail-rate")?.getAttribute("data-target-met")).toBe("false");
  expect(badge("change-fail-rate")?.textContent).toBe("\u2717 missed");
  expect(figure("change-fail-rate", "rate")).toBe("5.0%");
  // A target with nothing to judge: no verdict.
  expect(badge("deploy-run-success")?.getAttribute("data-target-met")).toBe("none");
  expect(badge("deploy-run-success")?.textContent).toBe("no data");
  // No target: no badge.
  expect(badge("failed-deploy-recovery")).toBeNull();
  expect(within(card("failed-deploy-recovery")).getByText("no target")).toBeInTheDocument();
});

test("a delivery card is not drawn when the tab passes it delivery off, though its eval is in the view", async () => {
  const delivery = view("delivery", [
    unowned(3),
    evalItem("deploy-frequency", "delivery", series("deploys", "deploys", { agg: "sum" })),
  ]);
  const legion = view("legion", [
    evalItem("legion-work-time", "legion", series("hours", "hours", { window: { median: 5 } })),
  ]);
  renderCards([delivery, legion], false);
  expect(
    [...document.querySelectorAll("[data-card]")].map((drawn) => drawn.getAttribute("data-card"))
  ).toEqual(["legion-work-time", "legion-rework"]);
  expect(figure("legion-work-time", "median")).toBe("5.0 h");
});

// The narrowed cards' rows (dispatch://INFRA-2918/artifact/plan-md, T30, D3): the cards of a
// section's views, subscribed on a fake live socket, and of views handed in.

const SCOPE: EvalsScope = { compare: null, project: "ACME", repos: [], window: { preset: 28 } };

/** The two lead-time cards' evals at `median` minutes, their series narrowed by `narrowed`. */
function leadTimes(median: number, narrowed: string[] | null = null): EvalListItem[] {
  return ["merge-to-production", "pr-open-to-merge"].map((slug) =>
    evalItem(
      slug,
      "delivery",
      series("minutes", "minutes", {
        narrowed,
        window: { max: median * 2, median, n: 4, p90: median * 2 },
      })
    )
  );
}

/** The cards of the section reading the delivery and the Legion views over `scope`, as the tab
 *  draws them; the function returned draws them over another scope, as the tab does when a facet
 *  is set. */
function renderSubscribedCards(scope: EvalsScope): (next: EvalsScope) => void {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const tree = (current: EvalsScope) => (
    <MemoryRouter initialEntries={["/projects/ACME/evals"]}>
      <QueryClientProvider client={client}>
        <EvalsSection
          id="cards"
          kinds={["delivery", "legion"]}
          scope={current}
          title="Target cards"
        >
          {(views) => <TargetCards delivery project="ACME" views={views} />}
        </EvalsSection>
      </QueryClientProvider>
    </MemoryRouter>
  );
  const { rerender } = render(tree(scope));
  return (next) => rerender(tree(next));
}

/** The query of an evals view's subscribe frame. */
function queryOf(frame: LiveSubscribe): URLSearchParams {
  return new URLSearchParams(frame.view.kind === "evals" ? frame.view.query : "");
}

/** What a card says of the facets: its mark's kind and words; null for no mark. */
function markOf(slug: string): [string | null, string | null] | null {
  const mark = card(slug).querySelector("[data-narrowing]");
  return mark === null ? null : [mark.getAttribute("data-narrowing"), mark.textContent];
}

test("a facet set re-subscribes the delivery view with the facets, and the narrowed cards draw its values", async () => {
  const sockets = installFakeLiveSockets();
  spies.push({ mockRestore: sockets.restore });
  const rescope = renderSubscribedCards(SCOPE);
  await waitFor(() => expect(sockets.sockets.length).toBeGreaterThan(0));
  const socket = sockets.latest();
  act(() => socket.open());
  const [plain, legion] = await waitFor(() =>
    ["delivery", "legion"].map((kind) => {
      const frame = socket.subscribes().find((sent) => queryOf(sent).get("kind") === kind);
      if (frame === undefined) throw new Error(`no ${kind} view was subscribed`);
      return frame;
    })
  );
  act(() => socket.receive(snapshotFrame(plain.sub, answerOf(leadTimes(45)))));
  act(() => socket.receive(snapshotFrame(legion.sub, answerOf([]))));
  expect(figure("merge-to-production", "median")).toBe("45m");
  expect(document.querySelector("[data-narrowing]")).toBeNull();

  // The author facet set: the tab reads the delivery view with it, holding the view it drew.
  const narrowing = "author=alice";
  act(() => rescope({ ...SCOPE, held: "", narrowing }));
  const narrowed = await waitFor(() => {
    const frame = socket.subscribed("evals");
    expect(queryOf(frame).getAll("author")).toEqual(["alice"]);
    return frame;
  });
  expect(queryOf(narrowed).get("kind")).toBe("delivery");
  // No facet narrows the Legion view: it is not subscribed again.
  expect(socket.subscribes().filter((frame) => queryOf(frame).get("kind") === "legion")).toEqual([
    legion,
  ]);
  expect(socket.unsubscribes()).toEqual([]);
  // Until the narrowed view answers, the narrowed cards keep the values drawn, marked loading.
  for (const slug of ["merge-to-production", "pr-open-to-merge"]) {
    expect(figure(slug, "median")).toBe("45m");
    expect(markOf(slug)).toEqual(["loading", "narrowing to the facets set\u2026"]);
    expect(card(slug).querySelector('[aria-busy="true"]')).not.toBeNull();
  }

  act(() => socket.receive(snapshotFrame(narrowed.sub, answerOf(leadTimes(20, ["author"])))));
  for (const slug of ["merge-to-production", "pr-open-to-merge"]) {
    expect(figure(slug, "median")).toBe("20m");
    expect(markOf(slug)).toEqual(["narrowed", "narrowed by Author"]);
    expect(card(slug).querySelector('[aria-busy="true"]')).toBeNull();
  }

  // Once the tab holds the narrowed view, the view it drew before is unsubscribed.
  act(() => rescope({ ...SCOPE, held: narrowing, narrowing }));
  await waitFor(() => expect(socket.unsubscribes().map((frame) => frame.sub)).toEqual([plain.sub]));
  expect(figure("merge-to-production", "median")).toBe("20m");
});

test('a window-only card says "window and repository only" while a facet is set, and nothing with none', async () => {
  const delivery = [
    evalItem(
      "deploy-frequency",
      "delivery",
      series("deploys", "deploys", { agg: "sum", window: { deploys: 6, per_day: 0.2 } })
    ),
    unowned(2),
    ...leadTimes(20, ["author", "q"]),
  ];
  const legion = [
    evalItem("legion-work-time", "legion", series("hours", "hours", { window: { median: 5 } })),
  ];
  const facets = { loading: false, set: true };
  renderCards([view("delivery", delivery, facets), view("legion", legion, facets)]);
  const cards = [...document.querySelectorAll("[data-card]")].map(
    (drawn) => drawn.getAttribute("data-card") ?? ""
  );
  for (const slug of cards) {
    expect(markOf(slug)).toEqual(
      slug === "merge-to-production" || slug === "pr-open-to-merge"
        ? ["narrowed", "narrowed by Author, Search"]
        : ["window-only", "window and repository only"]
    );
  }
  // Their stored values, as the view answers them.
  expect(figure("legion-work-time", "median")).toBe("5.0 h");
  expect(figure("unowned-p0", "value")).toBe("2");
  cleanup();

  renderCards([view("delivery", delivery), view("legion", legion)]);
  expect(document.querySelector("[data-narrowing]")).toBeNull();
});

test("the P0 link opens only= with the items' keys", async () => {
  const user = userEvent.setup();
  const listEvalItems = spyOn(api, "listEvalItems").mockResolvedValue(
    itemsPage(["ACME-7", "ACME-9"])
  );
  spies.push(listEvalItems);
  renderCards([view("delivery", [unowned(2)])]);
  const link = within(card("unowned-p0")).getByRole("button", { name: "Open the 2 issues" });
  expect(link).toHaveAttribute("data-group-count", "2");
  await user.click(link);
  expect((await screen.findByTestId("landed")).textContent).toBe(
    "/projects/ACME/issues?only=ACME-7%2CACME-9&onlyLabel=P0+with+no+owner&nofilter=1"
  );
});

// The P0 link's rows of the state table.

test("P0 card | pressed | resolving: the card marked busy, the unowned-p0 items read", async () => {
  const user = userEvent.setup();
  const listEvalItems = spyOn(api, "listEvalItems").mockImplementation(
    () => Promise.withResolvers<never>().promise
  );
  spies.push(listEvalItems);
  renderCards([view("delivery", [unowned(3)])]);
  expect(card("unowned-p0")).not.toHaveAttribute("aria-busy");

  await user.click(within(card("unowned-p0")).getByRole("button", { name: "Open the 3 issues" }));
  expect(card("unowned-p0")).toHaveAttribute("aria-busy", "true");
  expect(
    within(card("unowned-p0")).getByRole("button", { name: "Reading the issues\u2026" })
  ).toBeDisabled();
  expect(listEvalItems.mock.calls).toEqual([
    ["ACME", "unowned-p0", { limit: 200, offset: 0, row_kind: "issue" }],
  ]);
  expect(screen.queryByTestId("landed")).toBeNull();
});

test("resolving | items answered | navigated to only=<keys>&onlyLabel=…&nofilter=1", async () => {
  const user = userEvent.setup();
  // Two pages: the link reads every row the count counts.
  const listEvalItems = spyOn(api, "listEvalItems").mockImplementation((_project, _slug, query) =>
    Promise.resolve(
      query?.offset === 0 ? itemsPage(["ACME-1", "ACME-2"], 3) : itemsPage(["ACME-3"], 3, 2)
    )
  );
  spies.push(listEvalItems);
  renderCards([view("delivery", [unowned(3)])]);
  await user.click(within(card("unowned-p0")).getByRole("button", { name: "Open the 3 issues" }));
  const landed = new URL((await screen.findByTestId("landed")).textContent ?? "", "http://x");
  expect(landed.pathname).toBe("/projects/ACME/issues");
  expect(landed.searchParams.get("only")).toBe("ACME-1,ACME-2,ACME-3");
  expect(landed.searchParams.get("onlyLabel")).toBe("P0 with no owner");
  expect(landed.searchParams.get("nofilter")).toBe("1");
  expect(listEvalItems.mock.calls.map(([, , query]) => query?.offset)).toEqual([0, 2]);
});

test("resolving | items read failed | the card shows the error and a retry", async () => {
  const user = userEvent.setup();
  const listEvalItems = spyOn(api, "listEvalItems")
    .mockImplementationOnce(() => Promise.reject(new Error("the read failed")))
    .mockImplementationOnce(() => Promise.resolve(itemsPage(["ACME-4"])));
  spies.push(listEvalItems);
  renderCards([view("delivery", [unowned(1)])]);
  await user.click(within(card("unowned-p0")).getByRole("button", { name: "Open the issue" }));

  const alert = await within(card("unowned-p0")).findByRole("alert");
  expect(alert.textContent).toContain("Could not read the P0 issues with no owner.");
  expect(card("unowned-p0")).not.toHaveAttribute("aria-busy");
  expect(screen.queryByTestId("landed")).toBeNull();

  await user.click(within(alert).getByRole("button", { name: "Retry" }));
  await waitFor(() =>
    expect(screen.getByTestId("landed").textContent).toBe(
      "/projects/ACME/issues?only=ACME-4&onlyLabel=P0+with+no+owner&nofilter=1"
    )
  );
});
