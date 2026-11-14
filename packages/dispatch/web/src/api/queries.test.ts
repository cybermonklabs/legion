import { expect, test } from "bun:test";

import { projectIssuesQueryKey } from "./queries";

test("the query key is the plain list without labels and the labelled list with them", () => {
  expect(projectIssuesQueryKey("CORE", [])).toEqual(["issues", "project", "CORE"]);
  expect(projectIssuesQueryKey("CORE", ["a"])).toEqual([
    "issues",
    "project",
    "CORE",
    "labels",
    ["a"],
  ]);
});
