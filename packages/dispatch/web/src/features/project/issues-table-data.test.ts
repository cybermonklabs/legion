import { expect, test } from "bun:test";

import { queryIssuesParams, queryIssuesQuery } from "../../api/queries";
import { answeredWhole } from "./issues-table-data";

// A table reads one flag, and the rule is this one: a listing's open read is every row it holds
// only when the listing names its statuses and every one of them is open.
test("only a listing of named open statuses is answered whole by its open read", () => {
  // The two fault lists of the priorities page.
  expect(
    answeredWhole({
      advances: "none",
      statuses: ["in_progress", "testing", "needs_review", "retro"],
    })
  ).toBe(true);
  expect(answeredWhole({ advances: "any", statuses: ["backlog", "icebox"], moved: true })).toBe(
    true
  );
  // A listing that names `done` holds closed issues the open read leaves out.
  expect(answeredWhole({ advances: "any", statuses: ["done"] })).toBe(false);
  expect(answeredWhole({ advances: "any", statuses: ["backlog", "done"] })).toBe(false);
  // A listing that names no status, and a project's, hold every closed issue they have.
  expect(answeredWhole({ advances: "ship-it" })).toBe(false);
  expect(answeredWhole({ project: "CORE" })).toBe(false);
});

// A list a document holds reads its query's projects and labels and its filters' statuses and
// priorities, `P0` as `0`, with no `open` and no limit, in one read whatever its Status picks
// (dispatch://INFRA-4935/spec#b-r-exact-query).
test("a list's read sends its query's fields and no open, under one key per query", () => {
  const listing = {
    query: {
      labels: ["overnight"],
      priorities: ["P0", "none"],
      projects: ["INFRA", "OSSV"],
      statuses: ["done", "todo"],
    },
  };
  expect(queryIssuesParams(listing)).toEqual({
    label: ["overnight"],
    priority: ["0", "none"],
    project: ["INFRA", "OSSV"],
    status: ["done", "todo"],
  });
  const labelOnly = {
    query: { labels: ["overnight"], priorities: [], projects: [], statuses: [] },
  };
  expect(queryIssuesParams(labelOnly)).toEqual({ label: ["overnight"] });
  expect(answeredWhole(listing)).toBe(true);
  expect(answeredWhole(labelOnly)).toBe(true);
  expect(queryIssuesQuery(listing).queryKey.slice(0, 2)).toEqual(["issues", "query"]);
  expect(queryIssuesQuery(listing).queryKey).not.toEqual(queryIssuesQuery(labelOnly).queryKey);
});
