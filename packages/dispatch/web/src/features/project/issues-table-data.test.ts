import { expect, test } from "bun:test";

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
