import { expect, test } from "bun:test";

import { barTargets } from "./bar-scope";

// The three places the two keys leave the chip on an issue page whose writing cursor is in a
// block of it: the chip the page seeds, the chip `Backspace` widens to the project, and the
// chip `Tab` narrows to the block. Each row says which offers have something to act on - the
// handoff row an issue, the new issue a project - and what each of them acts on.

const block = "the block “The sextant sights the horizon.”";
const page = { block, issue: "CORE-1", project: "CORE" };

test("the chip the page leaves: the handoff acts on the issue, the new issue on its project", () => {
  expect(barTargets("page", page)).toEqual({ block: undefined, issue: "CORE-1", project: "CORE" });
});

test("the chip after Backspace: the handoff has no issue to act on, the new issue has the project", () => {
  expect(barTargets("project", page)).toEqual({
    block: undefined,
    issue: undefined,
    project: "CORE",
  });
});

test("the chip after Tab: the handoff acts on that block of its issue, the new issue on the project", () => {
  expect(barTargets("block", page)).toEqual({ block, issue: "CORE-1", project: "CORE" });
});

test("a page naming no block or issue leaves the block and the issue to no offer", () => {
  const bare = { block: undefined, issue: undefined, project: "CORE" };
  expect(barTargets("block", bare)).toEqual(bare);
  expect(barTargets("page", bare)).toEqual(bare);
});
