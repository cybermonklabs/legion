import { expect, test } from "bun:test";
import type { IssueSummary } from "../../api/types";
import { COLUMN_LABELS } from "./issues-table-columns";
import { buildItems, readUrl, viewIsChanged, writeUrl } from "./issues-table-model";
import { BUILT_IN_VIEWS, issuesViewHref, viewCount, viewOf } from "./issues-table-views";

const filterValue = (params: string, id: string, hasLegion = true) =>
  readUrl(new URLSearchParams(params), hasLegion).filters.find((filter) => filter.id === id)?.value;

function issue(key: string, overrides: Partial<IssueSummary> = {}): IssueSummary {
  return {
    key,
    title: key,
    status: "todo",
    priority: null,
    rank: key,
    parent: null,
    blocked_by: [],
    assignee: null,
    claim: null,
    components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
    route: null,
    repo: null,
    resolved_repo: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
    labels: [],
    open_asks: 0,
    progress: { tasks: null, children: null },
    last_seq: 1,
    primary_artifact_id: "spec",
    created_by: null,
    closed_at: null,
    created_at: "2026-09-10T00:00:00Z",
    updated_at: "2026-09-10T00:00:00Z",
    ...overrides,
  };
}

test("built-in views and column labels fit their controls", () => {
  expect(BUILT_IN_VIEWS.map((view) => view.name.length)).toSatisfy((lengths) =>
    lengths.every((length) => length <= 14)
  );
  expect(Object.values(COLUMN_LABELS).map((label) => label.length)).toSatisfy((lengths) =>
    lengths.every((length) => length <= 16)
  );
});

test("in-flight expands to every lifecycle stage and its sort", () => {
  const view = readUrl(new URLSearchParams("view=in-flight"), true);
  expect(filterValue("view=in-flight", "stage")).toEqual([
    "status:in_progress",
    "phase:design",
    "phase:planning",
    "phase:implementing",
    "phase:testing",
    "status:needs_review",
    "phase:reviewing",
    "phase:accepting",
    "phase:merging",
    "phase:awaiting_person",
    "phase:held",
    "phase:waiting_on_children",
    "phase:other",
    "status:retro",
  ]);
  expect(view.sorting).toEqual([
    { id: "stage", desc: false },
    { id: "time_in_phase", desc: true },
  ]);
});

test("Finished opens with Work time descending", () => {
  expect(readUrl(new URLSearchParams("view=finished"), true).sorting).toEqual([
    { id: "work_hours", desc: true },
  ]);
});

test("explicit view parameters replace a view's matching filter", () => {
  expect(filterValue("view=in-flight&f.stage=phase:implementing", "stage")).toEqual([
    "phase:implementing",
  ]);
  expect(filterValue("view=finished&f.closed=7d", "closed")).toBe("7d");
  expect(filterValue("view=finished&f.closed=7d", "legion")).toEqual(["worked"]);
});

test("an empty view parameter removes that filter", () => {
  expect(filterValue("view=slots&f.slot_use=", "slot_use")).toBeUndefined();
});

test("writing keeps only state that differs from a view", () => {
  const finished = readUrl(new URLSearchParams("view=finished"), true);
  expect(
    writeUrl(
      new URLSearchParams("view=finished"),
      {
        ...finished,
        sorting: [{ id: "work_hours", desc: true }],
      },
      true
    ).toString()
  ).toBe("view=finished");

  const slots = readUrl(new URLSearchParams("view=slots"), true);
  expect(
    writeUrl(
      new URLSearchParams("view=slots"),
      {
        ...slots,
        filters: slots.filters.filter((filter) => filter.id !== "slot_use"),
      },
      true
    ).toString()
  ).toBe("view=slots&f.slot_use=");
});

test("a view change ignores page-owned layout, columns, paging and size", () => {
  const unchanged = new URLSearchParams(
    "view=in-flight&layout=board&cols=key,title&page=2&size=100"
  );
  expect(viewIsChanged(unchanged, readUrl(unchanged, true), true)).toBe(false);

  const overridden = new URLSearchParams("view=in-flight&f.stage=phase:implementing");
  expect(viewIsChanged(overridden, readUrl(overridden, true), true)).toBe(true);
});

test("a pre-view link remains the open view with its overrides", () => {
  const state = readUrl(new URLSearchParams("status=done"));
  expect(filterValue("status=done", "status", false)).toEqual(["done"]);
  expect(state.sorting).toEqual([{ id: "queue", desc: false }]);
  expect(writeUrl(new URLSearchParams(), readUrl(new URLSearchParams())).toString()).toBe("");
});

test("projects without Legion retain only their applicable view rules", () => {
  expect(BUILT_IN_VIEWS.filter((view) => !view.legionOnly)).toHaveLength(5);
  expect(viewOf(new URLSearchParams("view=in-flight")).params(false).getAll("status")).toEqual([
    "in_progress",
    "testing",
    "needs_review",
    "retro",
  ]);
  expect(viewOf(new URLSearchParams("view=finished")).params(false).get("f.legion")).toBeNull();
  expect(viewOf(new URLSearchParams("view=slots")).legionOnly).toBe(true);
});

test("view counts resolve rolling closed windows at the supplied time", () => {
  const rows = buildItems(
    [issue("ACME-1", { closed_at: "2026-10-20T00:00:00Z", status: "done" })],
    (actor) => actor.id
  );
  expect(
    viewCount(
      viewOf(new URLSearchParams("view=finished")),
      rows,
      false,
      Date.parse("2026-12-01T00:00:00Z")
    )
  ).toBe(0);
});

test("issuesViewHref retains Evals filter overrides", () => {
  expect(
    issuesViewHref(
      "INFRA",
      "finished",
      new URLSearchParams("f.closed=28d&f.phase_hours=reviewing&sort=-phase_hours")
    )
  ).toBe(
    "/projects/INFRA/issues?view=finished&f.closed=28d&f.phase_hours=reviewing&sort=-phase_hours"
  );
});
