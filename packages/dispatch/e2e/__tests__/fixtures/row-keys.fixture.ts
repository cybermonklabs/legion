/** The rows of `row-keys.config.ts`: one at the top of its file, one in a titled `describe`, one
 *  in a `describe` with no title, one in a titled `describe` inside that, and one whose title
 *  holds a newline, which Playwright allows. The rows do nothing: the config is only listed. */
import { test } from "@playwright/test";

test("a top-level row", () => {});

test("a row of\ntwo lines", () => {});

test.describe("a titled describe", () => {
  test("a row in a titled describe", () => {});
});

test.describe(() => {
  test("a row in an untitled describe", () => {});

  test.describe("a titled describe in an untitled one", () => {
    test("a row two describes down", () => {});
  });
});
