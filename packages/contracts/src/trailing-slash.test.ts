import { expect, test } from "bun:test";
import { withoutTrailingSlashes } from "./trailing-slash";

const addresses = [
  ["https://example.test", "https://example.test"],
  ["https://example.test/", "https://example.test"],
  ["https://example.test///", "https://example.test"],
  ["/", ""],
  ["////", ""],
  ["", ""],
  ["https://example.test/v1/tokens", "https://example.test/v1/tokens"],
  ["https://example.test/v1//tokens//", "https://example.test/v1//tokens"],
] as const;

test.each(addresses)("%j without the slashes it ends in is %j", (given, want) => {
  expect(withoutTrailingSlashes(given)).toBe(want);
});
