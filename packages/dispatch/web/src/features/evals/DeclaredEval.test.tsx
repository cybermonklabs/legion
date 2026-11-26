import { afterEach, beforeEach, test as bunTest, expect, spyOn } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { COMPARE, evalItem, evalsAnswer, WINDOW } from "../../__tests__/evals-fixture";
import {
  type FakeLiveSocket,
  type FakeLiveSockets,
  installFakeLiveSockets,
  snapshotFrame,
} from "../../__tests__/live-socket-harness";
import { api } from "../../api/client";
import type {
  DeliveryLiveDelta,
  EvalDetailResponse,
  EvalItemRow,
  EvalItemsResponse,
  EvalListItem,
  LiveSubscribe,
} from "../../api/types";
import bandFixture from "./charts/__fixtures__/band-readings.json";
import numberFixture from "./charts/__fixtures__/number-readings.json";
import xyFixture from "./charts/__fixtures__/xy-readings.json";
import type { DeclaredSpec } from "./charts/kind";
import { keepLogged, LoggedEvals } from "./EveryEval";
import { type EvalsScope, useEvalsViews } from "./useEvalsViews";

// A declared eval on the Evals tab (dispatch://INFRA-2918/artifact/plan-md, D10, D11a, T20): its
// detail read for its charts and its answer, and its items views, opened only once its evals view
// has painted; its answer above its charts, each drawn by its kind; and a register write that moves
// its version read again, its new charts and answer drawn with no reload.

const SCOPE: EvalsScope = {
  compare: { from: COMPARE.from, to: COMPARE.to },
  project: "ACME",
  repos: [],
  window: { preset: 28 },
};

const TOKENS: DeclaredSpec = {
  group: "col",
  id: "tokens-over-time",
  kind: "xy",
  title: "Tokens to converge",
  y: { label: "Tokens to converge", series: "tokens" },
};

const LATENCY: DeclaredSpec = {
  group: "col",
  id: "latency-band",
  kind: "band",
  title: "Latency",
  y: { label: "Latency", series: "latency" },
};

/** A chart of a kind the platform may add later, which the tab has no drawing of. */
const RADAR: DeclaredSpec = { id: "arms-radar", kind: "radar", title: "Arms at a glance" };

/** A number whose one section stands after the band chart, as the platform's page draws it. */
const NUMBER: DeclaredSpec = {
  id: "proof-numbers",
  kind: "number",
  sections: [
    {
      after: "latency-band",
      tiles: [{ label: "still unfixed", series: "incidents-open" }],
      title: "Grader incidents",
    },
  ],
  series: ["proved", "incidents-open"],
  tiles: [{ label: "statements proved", series: "proved" }],
  title: "Where the proofs stand",
};

/** Two prose charts, which read no readings. */
const METHOD: DeclaredSpec = {
  id: "method",
  kind: "prose",
  paragraphs: ["Each arm runs the same forty tasks."],
  title: "How it was measured",
};
const CAVEATS: DeclaredSpec = {
  id: "caveats",
  kind: "prose",
  paragraphs: ["Arm B ran on a busier day."],
  title: "What to keep in mind",
};

/** The logged eval `convergence` at `version`, whose list read answers `answer`. */
function convergence(version: number, answer: string): EvalListItem {
  const item = evalItem("convergence", "experiment", [], version);
  return {
    ...item,
    definition: { ...item.definition, answer, name: "Convergence", source: "logged" },
  };
}

/** The eval's detail read at `version`: its charts and its answer. */
function detailOf(version: number, charts: DeclaredSpec[], answer: string): EvalDetailResponse {
  const item = convergence(version, answer);
  return {
    compare: COMPARE,
    doc_text: null,
    freshness: { refresher_age_seconds: null, rows_updated_at: null },
    items: [{ charts, definition: item.definition, owner: item.owner, series: [], verdict: null }],
    limit: 1,
    offset: 0,
    total: 1,
    window: WINDOW,
  };
}

/** The rows of `series` an items view answers, from a kind's fixture. */
function itemsOf(items: unknown[], series: string): EvalItemsResponse {
  const rows = (items as EvalItemRow[]).filter((row) => row.cols?.series === series);
  return {
    compare: COMPARE,
    items: rows,
    limit: 200,
    offset: 0,
    total: rows.length,
    window: WINDOW,
  };
}

/** Each detail read, held until the row answers it. */
let reads: ((detail: EvalDetailResponse) => void)[];
let sockets: FakeLiveSockets;
const spies: { mockRestore: () => void }[] = [];
beforeEach(() => {
  reads = [];
  sockets = installFakeLiveSockets();
  spies.push(
    spyOn(api, "getEval").mockImplementation(() => {
      const read = Promise.withResolvers<EvalDetailResponse>();
      reads.push(read.resolve);
      return read.promise;
    })
  );
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

/** The Logged section's evals, read through their evals view as the tab reads them. */
function Logged({ scope }: { scope: EvalsScope }) {
  const views = useEvalsViews(scope, ["experiment"], keepLogged);
  return <LoggedEvals scope={scope} views={views} />;
}

/** The Logged section under `scope`, the header's, and its live socket, open. */
async function renderLogged(scope: EvalsScope = SCOPE): Promise<FakeLiveSocket> {
  render(
    <MemoryRouter>
      <QueryClientProvider
        client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
      >
        <Logged scope={scope} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  await waitFor(() => expect(sockets.sockets.length).toBeGreaterThan(0));
  const socket = sockets.latest();
  act(() => socket.open());
  return socket;
}

/** The items views of `series` the page has subscribed. */
function itemsViews(socket: FakeLiveSocket, series: string): LiveSubscribe[] {
  return socket
    .subscribes()
    .filter(
      (frame) =>
        frame.view.kind === "items" &&
        new URLSearchParams(frame.view.query).get("series") === series
    );
}

/** Dispatch answers the items view of `series` with `body`. */
async function serveItems(socket: FakeLiveSocket, series: string, body: EvalItemsResponse) {
  const frame = await waitFor(() => {
    const sent = itemsViews(socket, series).at(-1);
    if (sent === undefined) throw new Error(`no items view of ${series} was subscribed`);
    return sent;
  });
  act(() => socket.receive(snapshotFrame(frame.sub, body)));
  return frame;
}

/** A delta of the evals view `sub` that changes `item`. */
function deltaOf(sub: string, item: EvalListItem): DeliveryLiveDelta {
  return {
    changed_at: null,
    counts: {},
    items: { items: { remove: [], upsert: [item] } },
    maps: {},
    revs: { items: { items: { [item.id]: "00000000000000bb" } } },
    seq: "2",
    set: {},
    sub,
    type: "delta",
  };
}

function declared(): HTMLElement {
  const found = document.querySelector('[data-declared="convergence"]');
  if (!(found instanceof HTMLElement)) throw new Error("no declared eval is drawn");
  return found;
}

/** The charts `declared()` draws, by name, in the order they draw. */
function chartsDrawn(): string[] {
  return [...declared().querySelectorAll("figure[data-chart]")].map(
    (figure) => figure.getAttribute("data-chart") ?? ""
  );
}

/** The eval drawn with the detail `charts` and every series they read served from `fixtures`. */
async function drawDeclared(
  charts: DeclaredSpec[],
  fixtures: Record<string, { items: unknown[] }>
): Promise<FakeLiveSocket> {
  const socket = await renderLogged();
  const view = await waitFor(() => socket.subscribed("evals"));
  act(() => socket.receive(snapshotFrame(view.sub, evalsAnswer([convergence(1, "")]))));
  await waitFor(() => expect(reads).toHaveLength(1));
  act(() => reads[0]?.(detailOf(1, charts, "")));
  for (const [series, fixture] of Object.entries(fixtures)) {
    await serveItems(socket, series, itemsOf(fixture.items, series));
  }
  return socket;
}

test("no items view opens before the first paint lands; then one for each series the charts name", async () => {
  const socket = await renderLogged();
  const view = await waitFor(() => socket.subscribed("evals"));
  // The evals view is out, and nothing of the declared eval is read yet.
  expect(socket.subscribes().filter((frame) => frame.view.kind === "items")).toEqual([]);
  expect(reads).toHaveLength(0);

  act(() =>
    socket.receive(
      snapshotFrame(view.sub, evalsAnswer([convergence(1, "Arm A converges in fewer tokens.")]))
    )
  );
  await waitFor(() => expect(reads).toHaveLength(1));
  // The detail has not answered: no chart is known, so no series is read.
  expect(itemsViews(socket, "tokens")).toEqual([]);
  act(() => reads[0]?.(detailOf(1, [TOKENS, RADAR], "Arm A converges in fewer tokens.")));

  const frame = await serveItems(socket, "tokens", itemsOf(xyFixture.items, "tokens"));
  // The header's window and compare range.
  expect(frame.view).toMatchObject({ kind: "items", slug: "convergence", window: { preset: 28 } });
  const query = new URLSearchParams(frame.view.kind === "items" ? frame.view.query : "");
  expect([query.get("compare_from"), query.get("compare_to")]).toEqual([COMPARE.from, COMPARE.to]);
  expect(itemsViews(socket, "tokens")).toHaveLength(1);

  const chart = await waitFor(() =>
    within(declared()).getByRole("figure", { name: "Tokens to converge" })
  );
  expect(chart).toHaveAttribute("data-chart", "convergence/tokens-over-time");
  expect(chart).toHaveAttribute("data-chart-kind", "xy");
  expect(chart.querySelectorAll('[data-statistic="y"][data-range="window"]').length).toBe(4);
  // A kind the tab does not draw is named in a line of its own.
  expect(declared().querySelector('[data-declared-chart="arms-radar"]')?.textContent).toBe(
    "Arms at a glance: the tab draws no chart of kind radar."
  );
});

test("the answer stands above the charts", async () => {
  const socket = await renderLogged();
  const view = await waitFor(() => socket.subscribed("evals"));
  act(() =>
    socket.receive(
      snapshotFrame(view.sub, evalsAnswer([convergence(1, "Arm A, as the list reads it.")]))
    )
  );
  await waitFor(() => expect(reads).toHaveLength(1));
  act(() => reads[0]?.(detailOf(1, [TOKENS], "Arm A converges in fewer tokens.")));
  await serveItems(socket, "tokens", itemsOf(xyFixture.items, "tokens"));
  const chart = await waitFor(() =>
    within(declared()).getByRole("figure", { name: "Tokens to converge" })
  );
  const answer = declared().querySelector("[data-answer]");
  // The detail's answer, at the version it read.
  expect(answer?.textContent).toBe("Arm A converges in fewer tokens.");
  expect((answer as Node).compareDocumentPosition(chart) & Node.DOCUMENT_POSITION_FOLLOWING).toBe(
    Node.DOCUMENT_POSITION_FOLLOWING
  );
});

test("a new definition.version in the evals view's delta reads the detail again, and draws the new charts and answer", async () => {
  const socket = await renderLogged();
  const view = await waitFor(() => socket.subscribed("evals"));
  act(() =>
    socket.receive(
      snapshotFrame(view.sub, evalsAnswer([convergence(1, "Arm A converges in fewer tokens.")]))
    )
  );
  await waitFor(() => expect(reads).toHaveLength(1));
  act(() => reads[0]?.(detailOf(1, [TOKENS], "Arm A converges in fewer tokens.")));
  await serveItems(socket, "tokens", itemsOf(xyFixture.items, "tokens"));
  await waitFor(() => within(declared()).getByRole("figure", { name: "Tokens to converge" }));

  // A register write changed the charts and the answer: the view's delta carries version 2.
  const next = "Arm A converges sooner, and its latency spread narrows.";
  act(() => socket.receive(deltaOf(view.sub, convergence(2, next))));
  await waitFor(() => expect(reads).toHaveLength(2));
  // Re-reading: the charts drawn so far stay while the new detail is out.
  expect(within(declared()).getByRole("status").textContent).toBe("Reading the charts…");
  expect(
    within(declared()).getByRole("figure", { name: "Tokens to converge" })
  ).toBeInTheDocument();

  act(() => reads[1]?.(detailOf(2, [LATENCY], next)));
  await serveItems(socket, "latency", itemsOf(bandFixture.items, "latency"));
  const band = await waitFor(() => within(declared()).getByRole("figure", { name: "Latency" }));
  expect(band).toHaveAttribute("data-chart-kind", "band");
  expect(within(declared()).queryByRole("figure", { name: "Tokens to converge" })).toBeNull();
  expect(declared().querySelector("[data-answer]")?.textContent).toBe(next);
  expect(within(declared()).queryByRole("status")).toBeNull();
  // The one evals view it had: nothing was subscribed again but the new series.
  expect(socket.subscribes().filter((frame) => frame.view.kind === "evals")).toHaveLength(1);
  expect(itemsViews(socket, "latency")).toHaveLength(1);
});

test("the readings views name no repository, though the header names one", async () => {
  const socket = await renderLogged({ ...SCOPE, repos: ["acme/widgets"] });
  const view = await waitFor(() => socket.subscribed("evals"));
  // The header's repository narrows the evals view.
  expect(
    new URLSearchParams(view.view.kind === "evals" ? view.view.query : "").getAll("repo")
  ).toEqual(["acme/widgets"]);
  act(() =>
    socket.receive(
      snapshotFrame(view.sub, evalsAnswer([convergence(1, "Arm A converges in fewer tokens.")]))
    )
  );
  await waitFor(() => expect(reads).toHaveLength(1));
  act(() => reads[0]?.(detailOf(1, [TOKENS], "Arm A converges in fewer tokens.")));

  // A logged eval's readings belong to no repository, and the items route keeps only the
  // readings a `repo` names: the view names none, so its readings stay in the chart.
  const frame = await serveItems(socket, "tokens", itemsOf(xyFixture.items, "tokens"));
  expect(
    new URLSearchParams(frame.view.kind === "items" ? frame.view.query : "").getAll("repo")
  ).toEqual([]);
  const chart = await waitFor(() =>
    within(declared()).getByRole("figure", { name: "Tokens to converge" })
  );
  expect(chart.querySelectorAll('[data-statistic="y"][data-range="window"]').length).toBe(4);
});

// Spec R20: a number's section stands on the platform's page after the chart its `after` names.
test("a number's section draws after the chart its `after` names, not inside the number", async () => {
  await drawDeclared([NUMBER, TOKENS, LATENCY], {
    "incidents-open": numberFixture,
    latency: bandFixture,
    proved: numberFixture,
    tokens: xyFixture,
  });
  await waitFor(() => within(declared()).getByRole("figure", { name: "Grader incidents" }));
  expect(chartsDrawn()).toEqual([
    "convergence/proof-numbers",
    "convergence/tokens-over-time",
    "convergence/latency-band",
    "convergence/proof-numbers/0",
  ]);
  const number = within(declared()).getByRole("figure", { name: "Where the proofs stand" });
  expect(number.querySelector('[data-number-place="proved"]')).not.toBeNull();
  expect(number.querySelector("[data-number-section]")).toBeNull();
  expect(number.querySelector('[data-number-place="incidents-open"]')).toBeNull();
  const section = within(declared()).getByRole("figure", { name: "Grader incidents" });
  expect(section.querySelector('[data-after="latency-band"]')).not.toBeNull();
  expect(section.querySelector('[data-number-place="incidents-open"]')).not.toBeNull();
  // Its caption is the section's title, so it draws no heading of its own.
  expect(section.querySelector("h5")).toBeNull();
});

// Spec R20: the platform draws every prose chart after all the eval's other charts.
test("prose charts draw after all the other charts, in the order they are declared", async () => {
  await drawDeclared([METHOD, TOKENS, CAVEATS, LATENCY], {
    latency: bandFixture,
    tokens: xyFixture,
  });
  await waitFor(() => within(declared()).getByRole("figure", { name: "Latency" }));
  expect(chartsDrawn()).toEqual([
    "convergence/tokens-over-time",
    "convergence/latency-band",
    "convergence/method",
    "convergence/caveats",
  ]);
});
