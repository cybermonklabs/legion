import { afterEach, expect, test } from "bun:test";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createElement } from "react";

import type { EvalItemRow } from "../../../api/types";
import type { EvalRange } from "../chart-model";
import { EvalChart } from "../EvalChart";
import architectureFixture from "./__fixtures__/architecture-readings.json";
import fixture from "./__fixtures__/network-readings.json";
import { architectureKind } from "./architecture";
import { isArchitectureModel } from "./architecture-model";
import type { DeclaredSpec } from "./kind";
import { networkKind } from "./network";
import { isNetworkModel, LINKS, type NetworkModel } from "./network-model";
import { readingsOf } from "./readings";

// The network kind (dispatch://INFRA-2918/artifact/plan-md, D10, T20): its model's nodes, values,
// words, states, notes, layout and order from the node and edge readings of
// `__fixtures__/network-readings.json`, each node's size and colour in the compare range, and its
// table holding every number its graph draws.

const NETWORK: DeclaredSpec = {
  color: "status",
  empty_label: "unknown",
  // Accepted and drawn from nothing: Dispatch reads the network from readings (plan D10).
  graph: { source_prefix: "graphs/deps/", team: "demo" },
  id: "deps",
  kind: "network",
  label: "name",
  labels: { statements: "Statements", status: "Proof status" },
  not_reported: ["statements"],
  outline: "reviewed",
  ratio: { noun: "statements", of: "done", out_of: "total" },
  selector: ["team", "reviewed"],
  size: "statements",
  title: "Dependencies",
  value_labels: { no: "not reviewed", partial: "partly reviewed", yes: "reviewed" },
};

function model(
  compare: EvalRange | null = fixture.compare,
  items: readonly unknown[] = fixture.items
): NetworkModel {
  const drawn = networkKind.model(NETWORK, {
    compare,
    readings: readingsOf(items as EvalItemRow[]),
    slug: "supply-chain",
    window: fixture.window,
  });
  if (!isNetworkModel(drawn)) throw new Error("the network model holds no network");
  return drawn;
}

/** Each node's row of the table: its place, its name and each value's words. */
function rows(drawn: NetworkModel, part: "values" | "compare" | "change") {
  const statistics = drawn.series[0]?.statistics ?? [];
  return (drawn.series[0]?.points ?? []).map((point) => [
    point.x,
    point.label,
    ...statistics.map(({ id }) => point[part]?.[id]?.label ?? null),
  ]);
}

afterEach(() => {
  cleanup();
  localStorage.clear();
});

test("a network reads every node and edge reading of its eval: its spec names no series", () => {
  expect(networkKind.reads(NETWORK)).toEqual([{ rowKind: "node" }, { rowKind: "edge" }]);
});

test("each node of the window at its newest reading, its attributes in the spec's words", () => {
  const drawn = model();
  expect([drawn.kind, drawn.name, drawn.title, drawn.comparable]).toEqual([
    "network",
    "supply-chain/deps",
    "Dependencies",
    true,
  ]);
  expect(drawn.compareRange).toEqual(fixture.compare);
  expect(drawn.axis).toEqual({ label: "Node", many: "nodes", one: "node" });
  expect(drawn.series).toHaveLength(1);
  expect(drawn.series[0]?.statistics).toEqual([
    { id: "statements", label: "Statements" },
    { id: "status", label: "Proof status" },
    { id: "team", label: "Team" },
    { id: "reviewed", label: "Reviewed" },
    { id: LINKS, label: "Links to" },
  ]);
  expect(rows(drawn, "values")).toEqual([
    // curl's reading of 3 Nov, not its first; its colour attribute as `ratio` writes it.
    ["curl", "curl", "28", "19 of 28 statements", "net", "reviewed", "OpenSSL, zlib, nghttp2"],
    ["openssl", "OpenSSL", "40", "40 of 40 statements", "crypto", "reviewed", "zlib"],
    // No done and total: the status as written; "true" is no `value_labels.yes`, only true is.
    ["zlib", "zlib", "0", "proved", "unknown", "true", "none"],
    // No attribute: `not_reported` for statements, `empty_label` for the rest, never 0; named by
    // its id; its edge to a node the window does not hold is still its link.
    ["nghttp2", "nghttp2", "not reported", "unknown", "unknown", "unknown", "gone"],
    ["libssh", "libssh", "12", "partial", "net", "partly reviewed (0.5)", "none"],
  ]);
  const points = drawn.series[0]?.points ?? [];
  // A size of 0 is a value; a node of none has no number.
  expect(points.map((point) => point.values.statements?.value)).toEqual([28, 40, 0, null, 12]);
  expect(points.map((point) => point.values.statements?.state)).toEqual([
    "plain",
    "plain",
    "plain",
    "absent",
    "plain",
  ]);
  expect(points.map((point) => [point.values.status?.state, point.values[LINKS]?.state])).toEqual([
    ["plain", "plain"],
    ["plain", "plain"],
    ["plain", "absent"],
    ["absent", "plain"],
    ["plain", "absent"],
  ]);
  expect(points.map((point) => point.values.reviewed?.value)).toEqual([
    null,
    null,
    null,
    null,
    0.5,
  ]);
  expect(drawn.network.notes).toEqual([
    "1 edge not drawn: an end names no node of the window.",
    "1 edge reading not drawn: no from and to in its attributes.",
  ]);
  expect(drawn.network.size).toEqual({ attribute: "statements", label: "Statements" });
  expect(drawn.network.outline).toEqual({ attribute: "reviewed", label: "Reviewed" });
});

test("each node one column right of the furthest node whose edge reaches it, sized and ringed", () => {
  const { network } = model();
  expect(network.edges.map((edge) => `${edge.from}\u2192${edge.to}`)).toEqual([
    "curl\u2192openssl",
    "curl\u2192zlib",
    "openssl\u2192zlib",
    "curl\u2192nghttp2",
  ]);
  expect(
    network.nodes.map((node) => [
      node.id,
      node.label,
      node.column,
      node.row,
      node.radius,
      node.sized,
      node.outlined,
    ])
  ).toEqual([
    ["curl", "curl", 0, 0, 17.7, true, true],
    // 40, the largest size of either range, draws at the most radius.
    ["openssl", "OpenSSL", 1, 0, 20, true, true],
    // zlib's edges come from curl and from openssl: it stands right of openssl. Its outline
    // attribute is the words "true", which outline it.
    ["zlib", "zlib", 2, 0, 6, true, true],
    ["nghttp2", "nghttp2", 1, 1, 6, false, false],
    // No edge touches libssh: it stands under the rest.
    ["libssh", "libssh", 0, 2, 13.7, true, false],
  ]);
  expect([network.columns, network.rows]).toEqual([3, 3]);
});

test("the colour attribute and each of the selector's: its values in order, each its colour", () => {
  const { network } = model();
  expect(
    network.colourings.map((colouring) => [
      colouring.attribute,
      colouring.label,
      colouring.categories.map((category) => [category.label, category.tone]),
    ])
  ).toEqual([
    [
      "status",
      "Proof status",
      [
        ["partial", 0],
        ["proved", 1],
        ["unknown", "other"],
      ],
    ],
    [
      "team",
      "Team",
      [
        ["crypto", 0],
        ["net", 1],
        ["unknown", "other"],
      ],
    ],
    [
      "reviewed",
      "Reviewed",
      [
        // true and "yes" read as one value_labels.yes, so they are one value of one colour.
        ["not reviewed", 0],
        ["partly reviewed (0.5)", 1],
        ["reviewed", 2],
        ["true", 3],
        ["unknown", "other"],
      ],
    ],
  ]);
  const curl = network.nodes[0];
  expect(curl?.categories).toEqual({ reviewed: "reviewed", status: "proved", team: "net" });
  expect(curl?.compareCategories).toEqual({
    reviewed: "not reviewed",
    status: "partial",
    team: "net",
  });
});

test("the compare range: each node's values there and their change, no data where it holds none", () => {
  const drawn = model();
  expect(rows(drawn, "compare")).toEqual([
    ["curl", "curl", "20", "8 of 20 statements", "net", "not reviewed", "OpenSSL"],
    ["openssl", "OpenSSL", "40", "40 of 40 statements", "crypto", "reviewed", "none"],
    ["zlib", "zlib", "no data", "no data", "no data", "no data", "no data"],
    ["nghttp2", "nghttp2", "no data", "no data", "no data", "no data", "no data"],
    ["libssh", "libssh", "no data", "no data", "no data", "no data", "no data"],
  ]);
  expect(rows(drawn, "change")).toEqual([
    ["curl", "curl", "+8", "changed", "no change", "changed", "changed"],
    ["openssl", "OpenSSL", "no change", "no change", "no change", "no change", "changed"],
    ["zlib", "zlib", "no data", "no data", "no data", "no data", "no data"],
    ["nghttp2", "nghttp2", "no data", "no data", "no data", "no data", "no data"],
    ["libssh", "libssh", "no data", "no data", "no data", "no data", "no data"],
  ]);
  expect(drawn.series[0]?.points.map((point) => point.change?.statements?.value)).toEqual([
    8,
    0,
    null,
    null,
    null,
  ]);
  expect(drawn.network.nodes.map((node) => node.compareRadius)).toEqual([
    15.9,
    20,
    null,
    null,
    null,
  ]);
  expect(drawn.network.compareOnly).toEqual(["brotli"]);
});

test("with no compare range no node holds a compare value", () => {
  const drawn = model(null);
  expect(
    drawn.series[0]?.points.every((point) => point.compare === null && point.change === null)
  ).toBe(true);
  expect(drawn.network.nodes.every((node) => node.compareRadius === null)).toBe(true);
  expect(drawn.network.compareOnly).toEqual([]);
});

/** Each value drawn under `root`, by what it names: series, statistic, place, range and value. */
function drawnValues(root: Element): string[] {
  return [...root.querySelectorAll("[data-value]")].map((element) =>
    ["data-series", "data-statistic", "data-x", "data-range", "data-value"]
      .map((name) => element.getAttribute(name))
      .join(" ")
  );
}

test("the table holds every number the graph draws, the compare range's and the changes too", async () => {
  const user = userEvent.setup();
  render(createElement(EvalChart, { model: model(), renderer: networkKind.renderer }));
  const figure = screen.getByRole("figure", { name: "Dependencies" });
  const graph = drawnValues(figure);
  // Each size, each compare ring's size and each size's change.
  expect(graph).toEqual(
    expect.arrayContaining([
      "nodes statements curl window 28",
      "nodes statements zlib window 0",
      "nodes statements curl compare 20",
      "nodes statements curl change 8",
      "nodes statements openssl change 0",
    ])
  );

  await user.click(within(figure).getByRole("button", { name: "Table" }));
  const table = within(figure).getByRole("table");
  const tabled = drawnValues(table);
  expect(graph.filter((value) => !tabled.includes(value))).toEqual([]);
  // The edges are in the table too, as each node's links.
  expect(within(table).getByText("OpenSSL, zlib, nghttp2")).toBeTruthy();
});

test("the compare range draws a dashed ring of each node's size and colour there", async () => {
  const user = userEvent.setup();
  render(createElement(EvalChart, { model: model(), renderer: networkKind.renderer }));
  const figure = screen.getByRole("figure", { name: "Dependencies" });
  const rings = [...figure.querySelectorAll("[data-network-ring]")];
  expect(rings.map((ring) => [ring.getAttribute("data-x"), ring.getAttribute("r")])).toEqual([
    ["curl", "15.9"],
    ["openssl", "20"],
  ]);
  // curl was partial in the compare range: its ring takes partial's colour, the palette's first.
  expect(rings[0]?.getAttribute("class")).toContain("stroke-sky-700");
  expect(rings[0]?.getAttribute("stroke-dasharray")).toBe("4 3");
  expect(within(figure).getByText("In the compare range only: brotli.")).toBeTruthy();

  await user.click(within(figure).getByRole("checkbox", { name: "Compare" }));
  expect(figure.querySelector("[data-network-ring]")).toBeNull();
  expect(figure.querySelector('[data-range="change"]')).toBeNull();
  expect(within(figure).queryByText("In the compare range only: brotli.")).toBeNull();
});

test("a reader colours the nodes by another attribute the selector names", async () => {
  const user = userEvent.setup();
  render(createElement(EvalChart, { model: model(null), renderer: networkKind.renderer }));
  const figure = screen.getByRole("figure", { name: "Dependencies" });
  const circle = (id: string) =>
    figure.querySelector(`[data-network-node="${id}"] circle`)?.getAttribute("class");
  // openssl is proved, the second value of the status: the palette's second colour.
  expect(circle("openssl")).toContain("fill-amber-700");
  expect(figure.querySelectorAll('[data-statistic="status"][data-range="window"]')).toHaveLength(5);

  await user.click(within(figure).getByRole("button", { name: "Team" }));
  // crypto, the first team.
  expect(circle("openssl")).toContain("fill-sky-700");
  expect(figure.querySelector('[data-statistic="status"]')).toBeNull();
  expect(figure.querySelectorAll('[data-statistic="team"][data-range="window"]')).toHaveLength(5);
  const key = within(figure).getByRole("list", { name: "Key" });
  expect(within(key).getByText("crypto")).toBeTruthy();

  await user.click(within(figure).getByRole("button", { name: "Reviewed" }));
  // With no compare range, no node was "not reviewed": zlib's "true" is the third value of
  // reviewed, the palette's third colour.
  expect(circle("zlib")).toContain("fill-green-700");
});

test("a network with no node reading in the window says so, and its table holds no row", async () => {
  const user = userEvent.setup();
  const empty = model(fixture.compare, []);
  expect(empty.series[0]?.points).toEqual([]);
  render(createElement(EvalChart, { model: empty, renderer: networkKind.renderer }));
  const figure = screen.getByRole("figure", { name: "Dependencies" });
  expect(
    within(figure).getByText("No node of this network has a reading in the window.")
  ).toBeTruthy();
  await user.click(within(figure).getByRole("button", { name: "Table" }));
  expect(within(figure).getByText("No nodes.")).toBeTruthy();
});

// Ledger 31: an eval that declares a network and an architecture reads one `edge` view, so each
// graph keeps only the edges of the series its nodes or parts are readings of.
test("a network and an architecture of one eval neither draw nor count each other's edges", () => {
  const network = (items: readonly unknown[]) => model(fixture.compare, items).network;
  const alone = network(fixture.items);
  const beside = network([...fixture.items, ...architectureFixture.items]);
  expect(beside.edges).toEqual(alone.edges);
  expect(beside.notes).toEqual(alone.notes);
  // The network's own edges are drawn: the scoping keeps them.
  expect(alone.edges.length).toBeGreaterThan(0);

  const diagram: DeclaredSpec = {
    id: "grader-risk",
    kind: "architecture",
    title: "Where the risk sits",
    value_series: "risk",
  };
  const architecture = (items: readonly unknown[]) => {
    const drawn = architectureKind.model(diagram, {
      compare: architectureFixture.compare,
      readings: readingsOf(items as EvalItemRow[]),
      slug: "supply-chain",
      window: architectureFixture.window,
    });
    if (!isArchitectureModel(drawn)) throw new Error("the architecture model holds no diagram");
    return drawn.architecture;
  };
  const own = architecture(architectureFixture.items);
  const both = architecture([...architectureFixture.items, ...fixture.items]);
  expect(both.edges).toEqual(own.edges);
  expect(both.notes).toEqual(own.notes);
  expect(own.edges.length).toBeGreaterThan(0);
});
