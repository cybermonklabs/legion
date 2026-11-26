import { afterEach, test as bunTest, expect, spyOn } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { COMPARE, evalItem, evalSeries, WINDOW } from "../../__tests__/evals-fixture";
import { api } from "../../api/client";
import { evalsViewQuery } from "../../api/queries";
import type { EvalListItem, EvalListResponse } from "../../api/types";
import { compareRange, liveWindow, readWindow, windowDays } from "../delivery/lib/window";
import { EvalsAcrossProjects } from "./EvalsAcrossProjects";

// The cross-project page (dispatch://INFRA-2918/artifact/plan-md, D11, T21; spec R11): one
// request to `GET /api/v1/evals` naming no project, its items grouped by project, each project
// named and linked to its tab, each number drawn with `EvalChart` and its eval's answer beside it.

const spies: { mockRestore: () => void }[] = [];
afterEach(() => {
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

/** The eval `slug` of `project`, its primary series holding `value` on the window's first day,
 *  with `answer` as its eval's answer. */
function projectEval(project: string, slug: string, value: number, answer: string): EvalListItem {
  const item = evalItem(slug, "experiment", [
    evalSeries("widgets", "", {
      agg: "last",
      days: [{ day: "2026-11-01", statistics: { n: 1, value } }],
      is_primary: true,
      window: { n: 1, value },
    }),
  ]);
  return {
    ...item,
    definition: {
      ...item.definition,
      answer,
      eval_id: `${project}/${slug}`,
      project_key: project,
    },
    id: `${project}/${slug}`,
  };
}

/** Two projects' evals as the cross-project read answers them, by project and then by slug. */
const ITEMS = [
  projectEval("ACME", "widget-size", 3, "Widgets grew after the cache change."),
  projectEval("ACME", "widget-speed", 7, ""),
  projectEval("BETA", "gadget-size", 11, "Gadgets hold steady."),
];

/** The cross-project read's answer: `items` of `total`, from `offset` on. */
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

/** The page at `/evals<search>`, the cross-project read answering `pages` in turn. */
function renderPage(search: string, ...pages: EvalListResponse[]) {
  const listEvals = spyOn(api, "listEvals").mockImplementation(async (query = {}) => {
    const page = pages.find((candidate) => candidate.offset === (query.offset ?? 0));
    if (page === undefined) throw new Error(`no page at offset ${query.offset}`);
    return page;
  });
  const getEval = spyOn(api, "getEval");
  spies.push(listEvals, getEval);
  render(
    <MemoryRouter initialEntries={[`/evals${search}`]}>
      <QueryClientProvider
        client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
      >
        <EvalsAcrossProjects />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { getEval, listEvals };
}

/** The number of the eval named `name`, once the page draws it. */
function number(name: string): Promise<HTMLElement> {
  return waitFor(() => screen.getByRole("article", { name }));
}

test("one request reads two projects' evals, naming no project", async () => {
  const { listEvals } = renderPage("?window=28d", answer(ITEMS));
  await number("gadget-size");
  expect(listEvals).toHaveBeenCalledTimes(1);
  const [query] = listEvals.mock.calls[0] ?? [];
  expect(query).not.toHaveProperty("project");
  expect(query?.kind).toBeUndefined();
  expect(screen.getByRole("region", { name: "ACME" })).toBeInTheDocument();
  expect(screen.getByRole("region", { name: "BETA" })).toBeInTheDocument();
});

test("each item is read as a project tab reads its items, and drawn from its own fields", async () => {
  const before = Date.now();
  const { listEvals } = renderPage("?window=28d&repo=acme%2Fwidgets", answer(ITEMS));
  await number("widget-size");
  const [query] = listEvals.mock.calls[0] ?? [];
  // The project tab's evals view for the same header: its query names the same compare range,
  // the same repositories and the same page, and its window is the same days.
  const choice = readWindow(new URLSearchParams("window=28d"));
  const compare = compareRange({ kind: "previous" }, choice, before);
  const [, , , view] = evalsViewQuery({
    compare,
    kind: "experiment",
    offset: 0,
    project: "ACME",
    repos: ["acme/widgets"],
    window: liveWindow(choice),
  }).queryKey;
  const tab = new URLSearchParams(typeof view === "string" ? "" : view.query);
  expect(query?.compare_from).toBe(tab.get("compare_from") ?? undefined);
  expect(query?.compare_to).toBe(tab.get("compare_to") ?? undefined);
  expect(query?.repo).toEqual(tab.getAll("repo"));
  expect(query?.limit).toBe(Number(tab.get("limit")));
  expect({ from: query?.from, to: query?.to }).toEqual(windowDays(choice, before));

  // Each number draws its own item: its chart named by its slug, its first day's value, and its
  // window's value beside it.
  for (const item of ITEMS) {
    const box = await number(item.definition.slug);
    const chart = within(box).getByRole("figure");
    expect(chart).toHaveAttribute("data-chart", item.definition.slug);
    const values = item.series?.[0];
    expect(
      chart
        .querySelector('[data-x="2026-11-01"][data-range="window"][data-value]')
        ?.getAttribute("data-value")
    ).toBe(String(values?.days?.[0]?.statistics?.value));
    expect(chart.querySelector('[data-window-statistic="value"]')?.textContent).toBe(
      String(values?.window?.value)
    );
  }
});

test("each number's project is named, and links to that project's tab on the same window", async () => {
  renderPage("?window=7d", answer(ITEMS));
  await number("widget-size");
  const acme = screen.getByRole("region", { name: "ACME" });
  expect(within(acme).getByRole("link", { name: "ACME" })).toHaveAttribute(
    "href",
    "/projects/ACME/evals?window=7d"
  );
  expect(
    within(acme)
      .getAllByRole("article")
      .map((box) => box.getAttribute("data-eval"))
  ).toEqual(["widget-size", "widget-speed"]);
  const beta = screen.getByRole("region", { name: "BETA" });
  expect(within(beta).getByRole("link", { name: "BETA" })).toHaveAttribute(
    "href",
    "/projects/BETA/evals?window=7d"
  );
  expect(
    within(beta)
      .getAllByRole("article")
      .map((box) => box.getAttribute("data-eval"))
  ).toEqual(["gadget-size"]);
});

test("each number's answer is drawn beside it, and no declared chart", async () => {
  const { getEval } = renderPage("?window=28d", answer(ITEMS));
  const size = await number("widget-size");
  expect(size.querySelector("[data-answer]")?.textContent).toBe(
    "Widgets grew after the cache change."
  );
  expect((await number("gadget-size")).querySelector("[data-answer]")?.textContent).toBe(
    "Gadgets hold steady."
  );
  // An eval with no answer says so, beside its chart all the same.
  const speed = await number("widget-speed");
  expect(speed.querySelector("[data-answer]")).toBeNull();
  expect(speed.textContent).toContain("No answer is written for this eval.");
  // The page reads no eval's detail, so it draws no declared chart.
  expect(getEval).not.toHaveBeenCalled();
  expect(document.querySelector("[data-declared]")).toBeNull();
});

test("a read of more evals than a page holds reads the next page, once", async () => {
  const more = projectEval("GAMMA", "gizmo-size", 2, "Gizmos shrank.");
  const { listEvals } = renderPage("?window=28d", answer(ITEMS, 201), answer([more], 201, 200));
  await number("gizmo-size");
  expect(listEvals.mock.calls.map(([query]) => query?.offset)).toEqual([undefined, 200]);
  expect(screen.getByRole("region", { name: "GAMMA" })).toBeInTheDocument();
});
