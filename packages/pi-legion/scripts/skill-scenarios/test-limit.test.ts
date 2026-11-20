import { expect, test } from "bun:test";
import { ROW_MARGIN_MS } from "@legion/test-fixtures/row-clock";
import { ROW_WORK_MS, rowLimit } from "./test-limit";

test("a row's limit is the bounds of what it waits for, its own work and the margin", () => {
  const parts = [
    { part: "one", boundMs: 1_000 },
    { part: "two", boundMs: 2_000 },
  ];
  expect(rowLimit(...parts)).toBe(3_000 + ROW_WORK_MS + ROW_MARGIN_MS);
  expect(rowLimit()).toBe(ROW_WORK_MS + ROW_MARGIN_MS);
});
