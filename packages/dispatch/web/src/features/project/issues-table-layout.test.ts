import { expect, test } from "bun:test";

import { fitColumns } from "./issues-table-layout";
import { ISSUE_TABLE_COLUMNS } from "./issues-table-model";

// c-phone-key at the unit level (dispatch://INFRA-4935/spec#b-r-phone-key): in a box 400 px
// wide the Key column is as wide as the widest key the rows draw, plus its cells' 16 px of
// padding, and Title gives up the width, down to its phone minimum of 120 px.
test("on a phone the Key column is as wide as the widest key drawn, and Title gives up the width", () => {
  // INFRA-4935 and its unread dot measure 102 px.
  const fitted = fitColumns(ISSUE_TABLE_COLUMNS, {}, 400, 102);
  expect(fitted.pinned).toEqual(["select", "key", "title"]);
  expect(fitted.sizing.key).toBe(118);
  // 85% of 400 px, less Select's 44 px and Key's 118 px.
  expect(fitted.sizing.title).toBe(178);

  // A key no wider than the phone width keeps the phone width.
  expect(fitColumns(ISSUE_TABLE_COLUMNS, {}, 400, 50).sizing.key).toBe(88);
  // Nothing measured yet: the phone width.
  expect(fitColumns(ISSUE_TABLE_COLUMNS, {}, 400, 0).sizing.key).toBe(88);
  // A key so wide that Title would go under its minimum: Title stays at 120 px and the box
  // scrolls sideways.
  const long = fitColumns(ISSUE_TABLE_COLUMNS, {}, 400, 220);
  expect(long.sizing.key).toBe(236);
  expect(long.sizing.title).toBe(120);
  // A wide box draws Key at its own width, never below its minimum, whatever the keys measure.
  expect(fitColumns(ISSUE_TABLE_COLUMNS, {}, 1280, 102).sizing.key).toBe(108);
});
