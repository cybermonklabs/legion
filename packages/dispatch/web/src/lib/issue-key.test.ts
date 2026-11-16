import { expect, test } from "bun:test";

import { issueProject } from "./issue-key";

test("an issue key's project is the part before its first hyphen", () => {
  expect(issueProject("CORE-12")).toBe("CORE");
  expect(issueProject("INFRA2-7")).toBe("INFRA2");
  // A string with no hyphen names no number: the whole of it is what there is.
  expect(issueProject("CORE")).toBe("CORE");
});
