import { expect, test } from "bun:test";

import type { DeliveryPR } from "../../../api/types";
import {
  buildColorScale,
  COLOR_PALETTE,
  colorKeyFor,
  OTHER_COLOR,
  PLACEHOLDER_COLOR,
} from "./colorScale";
import { NO_ISSUE, NO_SESSION } from "./facets";

// The value the server sends for a PR whose issue is not known yet. The tests write it out, so
// they hold the page to the server's value.
const ISSUE_NOT_KNOWN_VALUE = "__issue_not_known__";

const noIssue: DeliveryPR = {
  additions: 1,
  author: "octocat",
  components: [],
  created_at: "2024-06-01T00:00:00Z",
  deletions: 1,
  deploy_run: null,
  deployed_at: null,
  deployed_status: "waiting",
  first_commit_at: null,
  id: "acme/widgets#1",
  issue: null,
  issue_title: null,
  merged_at: "2024-06-01T01:00:00Z",
  number: 1,
  parent_agent: null,
  partial: false,
  priority: null,
  repo: "acme/widgets",
  rework: false,
  sessions: [],
  title: "feat: a widget",
  unfetchable_reason: null,
  url: "https://github.com/acme/widgets/pull/1",
};

/** `n` values, busiest first: v0 has `n` PRs, v1 `n - 1`, and so on. */
function counts(n: number): Record<string, number> {
  return Object.fromEntries(Array.from({ length: n }, (_, i) => [`v${i}`, n - i]));
}

test("with nothing chosen, up to ten values take ten different palette colours and none is Other", () => {
  for (let n = 1; n <= COLOR_PALETTE.length; n++) {
    const scale = buildColorScale(counts(n));
    const colours = [...scale.values()];
    expect(new Set(colours).size).toBe(n);
    expect(colours).not.toContain(OTHER_COLOR);
    expect(colours.every((colour) => COLOR_PALETTE.includes(colour))).toBe(true);
  }
});

test("with nothing chosen, an eleventh value repeats the palette as before", () => {
  const scale = buildColorScale(counts(12));
  expect(scale.get("v10")).toBe(COLOR_PALETTE[0]);
  expect(scale.get("v11")).toBe(COLOR_PALETTE[1]);
});

test("chosen values take the palette in the chosen order; the rest are Other and a placeholder stays grey", () => {
  const scale = buildColorScale({ a: 5, b: 3, c: 2, [NO_SESSION]: 4 }, ["b", "a"]);
  expect(scale.get("b")).toBe(COLOR_PALETTE[0]);
  expect(scale.get("a")).toBe(COLOR_PALETTE[1]);
  expect(scale.get("c")).toBe(OTHER_COLOR);
  expect(scale.get(NO_SESSION)).toBe(PLACEHOLDER_COLOR);
});

test("a repeated chosen value consumes one palette slot", () => {
  const scale = buildColorScale({ a: 5, b: 3, c: 1 }, ["a", "a", "b"]);
  expect(scale.get("a")).toBe(COLOR_PALETTE[0]);
  expect(scale.get("b")).toBe(COLOR_PALETTE[1]);
  expect(scale.get("c")).toBe(OTHER_COLOR);
});

test("a chosen value the window does not hold takes no colour from the ones it does", () => {
  const scale = buildColorScale({ a: 5, b: 3 }, ["gone", "a"]);
  expect(scale.get("a")).toBe(COLOR_PALETTE[1]);
  expect(scale.get("b")).toBe(OTHER_COLOR);
});

test("a pull request whose issue is not known colours by its own placeholder, by priority and by component", () => {
  // Control: a PR whose issue_not_known is false keeps the No issue placeholder.
  const none = { ...noIssue, issue_not_known: false };
  expect(colorKeyFor(none, "priority")).toBe(NO_ISSUE);
  expect(colorKeyFor(none, "component")).toBe(NO_ISSUE);
  const notKnown = { ...noIssue, issue_not_known: true };
  expect(colorKeyFor(notKnown, "priority")).toBe(ISSUE_NOT_KNOWN_VALUE);
  expect(colorKeyFor(notKnown, "component")).toBe(ISSUE_NOT_KNOWN_VALUE);
});

test("the placeholder of an issue that is not known is grey and takes no palette slot", () => {
  const scale = buildColorScale({ [ISSUE_NOT_KNOWN_VALUE]: 5, a: 3, [NO_ISSUE]: 1 });
  // Control: the No issue placeholder is grey.
  expect(scale.get(NO_ISSUE)).toBe(PLACEHOLDER_COLOR);
  expect(scale.get(ISSUE_NOT_KNOWN_VALUE)).toBe(PLACEHOLDER_COLOR);
  expect(scale.get("a")).toBe(COLOR_PALETTE[0]);
});
