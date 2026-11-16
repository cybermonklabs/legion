/** The planted shape of e2e/__tests__/no-browser-rows.test.ts that pins WHAT A RESOLVED
 *  CALLBACK IS READ FOR: the two ways a file declares a callback a row then passes by name, each
 *  binding a fixture, so that reading the declaration's parameters is pinned and not merely the
 *  fail-closed `fixtures: []` every callback the scan cannot read reports.
 *
 * The first row's callback is a function declaration and its title is computed; the second's is
 * a function-valued `const` and its title is a literal, so neither the title's shape nor the
 * declaration's decides what is read. One binds `page` and drives a browser, the other binds
 * `request` — Playwright's HTTP client, which opens no page — and does not, so this file also
 * shows that a resolved callback can take a row out of `unexplainedRows` as well as leave it
 * in.
 *
 * Its name ends `.fixture.ts` and not `.e2e.ts` because e2e/playwright.config.ts says
 * `testDir: "."` and `testMatch: /.*\.e2e\.ts/`: Playwright walks the `e2e` directory
 * recursively, so a `.e2e.ts` file here, under `__tests__/fixtures/`, would join the real suite
 * and run. `bun test` collects only `*.test.ts` and its kin, so this file runs nowhere; the test
 * reads its text and parses it.
 */

import { type APIRequestContext, expect, type Page, test } from "@playwright/test";

/** The first row's title, computed so that the source alone cannot say what it reads. */
const computedTitle = ["a planted row whose callback, declared beside it,", "opens a page"].join(
  " "
);

/** The first row's callback: a function declaration binding a browser fixture. */
async function opensAPage({ page }: { page: Page }): Promise<void> {
  await page.goto("/");
}

test(computedTitle, opensAPage);

/** The second row's callback: a function-valued `const` binding Playwright's HTTP client, which
 *  is no browser. */
const asksOverHttp = async ({ request }: { request: APIRequestContext }): Promise<void> => {
  expect((await request.get("/healthz")).ok()).toBe(true);
};

test("a planted row whose callback is a const declared beside it", asksOverHttp);
