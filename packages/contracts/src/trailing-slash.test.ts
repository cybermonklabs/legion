import { expect, test } from "bun:test";
import { withoutTrailingSlashes } from "./trailing-slash";

test("drops every slash a string ends in and nothing else", () => {
  for (const [given, want] of [
    ["https://example.test", "https://example.test"],
    ["https://example.test/", "https://example.test"],
    ["https://example.test///", "https://example.test"],
    ["/", ""],
    ["////", ""],
    ["", ""],
    ["https://example.test/v1/tokens", "https://example.test/v1/tokens"],
    ["https://example.test/v1//tokens//", "https://example.test/v1//tokens"],
  ] as const) {
    expect(withoutTrailingSlashes(given)).toBe(want);
  }
});
