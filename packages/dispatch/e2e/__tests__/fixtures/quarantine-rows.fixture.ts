/** The rows of `e2e/__tests__/fixtures/quarantine.config.ts`, which lists them to show that the
 *  quarantine's pattern takes out the rows its entries name and no row that only looks like one.
 *  Beside each quarantined row stands a row it must not take: one whose title ends with that
 *  title, one whose title begins with it, and one of the same title inside a `describe`. The
 *  second quarantined row sits inside that `describe`, so its full title carries the block's, and
 *  its own title holds the characters a pattern reads as syntax.
 *
 * Its name ends `.fixture.ts` and not `.e2e.ts` for the reason `browser-row.fixture.ts` beside it
 * gives: e2e/playwright.config.ts walks this directory too, and a `.e2e.ts` file here would join
 * the real suite. Nothing runs these rows; the bodies are never called by a listing.
 */

import { expect, test } from "@playwright/test";

test("saves the draft @pr", () => {
  expect("quarantined in alpha").toContain("alpha");
});

test("autosave saves the draft @pr", () => {
  expect("ends with a quarantined title").toContain("title");
});

test("saves the draft @pr twice", () => {
  expect("begins with a quarantined title").toContain("title");
});

test.describe("offline", () => {
  test("saves the draft @pr", () => {
    expect("the quarantined title inside a describe").toContain("describe");
  });

  test("costs $5 (or more)? @phone", () => {
    expect("quarantined in alpha-phone alone").toContain("alpha-phone");
  });
});
