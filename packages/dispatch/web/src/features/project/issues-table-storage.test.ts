import { expect, test } from "bun:test";

import { layoutKeyOf, tableStorageKey } from "./issues-table-storage";

test("each built-in view keeps its own column layout except All open", () => {
  expect(layoutKeyOf("ACME", "open")).toBe("ACME");
  expect(layoutKeyOf("ACME", "finished")).toBe("ACME.finished");
  expect(tableStorageKey(layoutKeyOf("ACME", "finished"))).toBe(
    "dispatch.project-table.ACME.finished"
  );
});
