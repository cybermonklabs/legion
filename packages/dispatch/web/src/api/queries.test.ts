import { expect, test } from "bun:test";

import {
  EVALS_VIEW_LIMIT,
  type EvalKind,
  type EvalsViewParams,
  evalsViewQuery,
  itemsViewQuery,
  PROJECT_TIMELINE_PAGE,
  projectTimelineViewQuery,
} from "./queries";

const KINDS: EvalKind[] = ["delivery", "legion", "ci", "experiment"];

const COMPARE = { from: "2026-09-27T00:00:00.000Z", to: "2026-10-26T00:00:00.000Z" };

/** The live view `params` subscribe: the key's last part, under the project's evals. */
function viewOf(params: EvalsViewParams) {
  const [scope, project, name, view] = evalsViewQuery(params).queryKey;
  if (typeof view === "string") throw new Error("the key names no view");
  expect([scope, project, name]).toEqual(["evals", params.project, "view"]);
  return view;
}

// The query is the one GET /api/v1/projects/{key}/evals takes without its window, which the view
// names apart (packages/contracts DeliveryLiveView), so the hub reads it as the route reads it.
test.each(
  KINDS
)("the %s view carries the project, the window, the compare range, the repositories and its kind", (kind: EvalKind) => {
  const view = viewOf({
    compare: COMPARE,
    kind,
    offset: 0,
    project: "ACME",
    repos: ["acme/widgets", "acme/gadgets"],
    window: { preset: 28 },
  });
  expect(view).toEqual({
    kind: "evals",
    project: "ACME",
    query: expect.any(String),
    window: { preset: 28 },
  });
  const query = new URLSearchParams(view.query);
  expect([...query.keys()].sort()).toEqual([
    "compare_from",
    "compare_to",
    "kind",
    "limit",
    "repo",
    "repo",
  ]);
  expect(query.get("compare_from")).toBe(COMPARE.from);
  expect(query.get("compare_to")).toBe(COMPARE.to);
  expect(query.getAll("repo")).toEqual(["acme/widgets", "acme/gadgets"]);
  expect(query.get("kind")).toBe(kind);
  expect(query.get("limit")).toBe(String(EVALS_VIEW_LIMIT));
});

test("a view with no compare range names none, one of the project's own rows no repository, and a later page its offset", () => {
  const first = viewOf({
    compare: null,
    kind: "ci",
    offset: 0,
    project: "ACME",
    repos: [],
    window: { from: "2026-01-01T00:00:00Z", to: "2026-01-29T00:00:00Z" },
  });
  expect(first.window).toEqual({ from: "2026-01-01T00:00:00Z", to: "2026-01-29T00:00:00Z" });
  expect(first.query).toBe("kind=ci&limit=200");

  const second = viewOf({
    compare: null,
    kind: "ci",
    offset: EVALS_VIEW_LIMIT,
    project: "ACME",
    repos: [],
    window: { preset: 7 },
  });
  expect(second.query).toBe("kind=ci&limit=200&offset=200");
});

// The timeline's facets but the repository, and its search, narrow the delivery evals alone
// (dispatch://INFRA-2918/artifact/plan-md, D7a): a delivery view sends each under the key the list
// read takes it by, and a view of another kind is the same view with or without them.
test("a delivery view carries the search and each facet value under the list read's keys, and a view of another kind none", () => {
  const narrowing =
    "q=fix&parent_agent=ACME-1&session=s1&issue=ACME-2&priority=P1&component=ACME%2Fweb&author=alice&author=bob&rework=rework&deployed=waiting";
  const scope: Omit<EvalsViewParams, "kind"> = {
    compare: null,
    narrowing,
    offset: 0,
    project: "ACME",
    repos: ["acme/widgets"],
    window: { preset: 28 },
  };
  const query = new URLSearchParams(viewOf({ ...scope, kind: "delivery" }).query);
  expect([...query.keys()]).toEqual([
    "repo",
    "q",
    "parent_agent",
    "session",
    "issue",
    "priority",
    "component",
    "author",
    "author",
    "rework",
    "deployed",
    "kind",
    "limit",
  ]);
  expect(query.get("q")).toBe("fix");
  expect(query.getAll("author")).toEqual(["alice", "bob"]);
  expect(query.get("component")).toBe("ACME/web");
  expect(query.getAll("repo")).toEqual(["acme/widgets"]);
  // Narrowed and unnarrowed, each is its own view.
  expect(evalsViewQuery({ ...scope, kind: "delivery" }).queryKey).not.toEqual(
    evalsViewQuery({ ...scope, kind: "delivery", narrowing: "" }).queryKey
  );

  for (const kind of ["legion", "ci", "experiment"] as const) {
    expect(evalsViewQuery({ ...scope, kind }).queryKey).toEqual(
      evalsViewQuery({ ...scope, kind, narrowing: undefined }).queryKey
    );
    expect(viewOf({ ...scope, kind }).query).toBe(`repo=acme%2Fwidgets&kind=${kind}&limit=200`);
  }
});

// An items view names the eval whose rows it shows apart, and the rest of what the items route
// takes as its query (packages/contracts DeliveryLiveView), so the hub reads it as the route does.
test("an items view carries the project, the eval, the window, and the items route's query", () => {
  const [scope, project, name, view] = itemsViewQuery({
    compare: COMPARE,
    limit: 5,
    project: "ACME",
    repos: ["acme/widgets"],
    slug: "pr-ci",
    sort: "median",
    window: { preset: 28 },
  }).queryKey;
  expect([scope, project, name]).toEqual(["evals", "ACME", "items"]);
  expect(view).toEqual({
    kind: "items",
    project: "ACME",
    query: `compare_from=${encodeURIComponent(COMPARE.from)}&compare_to=${encodeURIComponent(COMPARE.to)}&repo=acme%2Fwidgets&sort=median&limit=5`,
    slug: "pr-ci",
    window: { preset: 28 },
  });

  const [, , , filtered] = itemsViewQuery({
    compare: null,
    limit: 200,
    offset: 200,
    project: "ACME",
    repos: [],
    row: "ACME-7",
    rowKind: "issue",
    series: "hours:roots",
    slug: "legion-work-time",
    window: { from: "2026-01-01T00:00:00Z", to: "2026-01-29T00:00:00Z" },
  }).queryKey;
  expect(typeof filtered === "string" ? null : filtered?.query).toBe(
    "series=hours%3Aroots&row_kind=issue&row=ACME-7&limit=200&offset=200"
  );
});

// A project-timeline view grows by its limit on the one subscription (plan D8), so its key names
// the project, the window, the facets and the search, and never the limit a growth raises.
test("a project-timeline view is keyed by its window, facets and search, never its limit", () => {
  const params = {
    project: "ACME",
    query: "q=widget&author=octocat",
    window: { preset: 28 },
  } as const;
  const key = [...projectTimelineViewQuery(params).queryKey];
  expect(key).toEqual(["evals", "ACME", "timeline", params]);
  expect(JSON.stringify(key)).not.toContain("limit");
  expect(PROJECT_TIMELINE_PAGE).toBe(200);
});
