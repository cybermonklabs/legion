import { expect, test } from "bun:test";

import { segmentWidths } from "./issues-table-controls";
import { keyColumnWidth } from "./issues-table-model";

test("a segment is as wide as its share of the issues, and the segments fill the bar", () => {
  const widths = segmentWidths([50, 30, 20]);
  expect(widths).toEqual([50, 30, 20]);
  expect(widths.reduce((sum, width) => sum + width, 0)).toBeCloseTo(100, 6);
});

test("a count of one keeps a pressable width, and the bar is still exactly full", () => {
  const widths = segmentWidths([997, 1, 1, 1]);
  expect(widths.slice(1)).toEqual([3, 3, 3]);
  expect(widths.reduce((sum, width) => sum + width, 0)).toBeCloseTo(100, 6);
  // Fifteen phases, the most the Issues page draws: each is at least the floor of the bar, and
  // the one big segment keeps the rest.
  const many = segmentWidths([400, ...Array.from({ length: 14 }, () => 1)]);
  expect(many.length).toBe(15);
  expect(many.reduce((sum, width) => sum + width, 0)).toBeCloseTo(100, 6);
  expect(Math.min(...many)).toBe(3);
  expect(many[0]).toBeCloseTo(58, 6);
});

test("an empty bar divides itself evenly rather than dividing by zero", () => {
  expect(segmentWidths([0, 0])).toEqual([50, 50]);
  expect(segmentWidths([])).toEqual([]);
});

test("the Key column is wide enough for the project's longest key", () => {
  expect(keyColumnWidth("INFRA-1508".length)).toBeGreaterThan(100);
  expect(keyColumnWidth("OSSV-1".length)).toBeLessThan(keyColumnWidth("INFRA-1508".length));
  // A key no project writes never widens the column past the title.
  expect(keyColumnWidth(80)).toBe(176);
});
