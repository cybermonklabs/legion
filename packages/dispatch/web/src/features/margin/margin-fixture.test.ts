import { expect, test } from "bun:test";

import {
  COMPACT_VIEWPORT_QUERY,
  MD_VIEWPORT_QUERY,
  PHONE_VIEWPORT_QUERY,
  READING_PANE_QUERY,
} from "../shell/useDialog";
import { DESKTOP_WIDTH, PHONE_WIDTH, stubViewport } from "./margin-fixture";

// The phone rows of the draft matrix draw a phone only if the stub answers as a phone does: the
// phone and compact queries true, and the `md:` and reading pane queries, which are `rem` widths,
// false. A wide desktop answers each the other way.
for (const [name, width, phone] of [
  ["phone", PHONE_WIDTH, true],
  ["desktop", DESKTOP_WIDTH, false],
] as const) {
  test(`stubViewport answers the shell's width queries as a ${name} does`, () => {
    const restore = stubViewport(width);
    try {
      expect(window.matchMedia(PHONE_VIEWPORT_QUERY).matches).toBe(phone);
      expect(window.matchMedia(COMPACT_VIEWPORT_QUERY).matches).toBe(phone);
      expect(window.matchMedia(MD_VIEWPORT_QUERY).matches).toBe(!phone);
      expect(window.matchMedia(READING_PANE_QUERY).matches).toBe(!phone);
    } finally {
      restore();
    }
  });
}

// Each bound is inclusive, as a browser's is, and a query joining two clauses holds only where
// both do.
test("stubViewport holds a width bound at its own edge and joins clauses with and", () => {
  const atEdge = stubViewport(768);
  try {
    expect(window.matchMedia(MD_VIEWPORT_QUERY).matches).toBe(true);
    expect(window.matchMedia(PHONE_VIEWPORT_QUERY).matches).toBe(false);
    expect(window.matchMedia("(min-width: 48rem) and (max-width: 1279px)").matches).toBe(true);
    expect(window.matchMedia("(min-width: 80rem) and (max-width: 1279px)").matches).toBe(false);
  } finally {
    atEdge();
  }
});

// A query the stub cannot read fails the row that asked it, rather than getting a guessed answer.
test("stubViewport throws on a query it cannot read, naming the query", () => {
  const restore = stubViewport(PHONE_WIDTH);
  try {
    expect(() => window.matchMedia("(prefers-color-scheme: dark)")).toThrow(
      'stubViewport cannot read the media query "(prefers-color-scheme: dark)"'
    );
  } finally {
    restore();
  }
});
