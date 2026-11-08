/** The planted rows of e2e/__tests__/no-browser-rows.test.ts that turn on WHICH COMPILER
 *  OPTIONS the scan's program is built with. `scanOptions` (e2e/no-browser-rows.ts) takes the
 *  package's `tsconfig.json` whole, and the two calls below are what that is worth: without `jsx`,
 *  or under a `types` list without `bun`, one of them is read wrong, and nothing else says so.
 *
 * Two rows' worth, one per option:
 *
 *  - the first call's callback is imported from a `.tsx` module beside this one. With `jsx`
 *    among the options the import resolves and the row is read with `["page"]`, the fixtures
 *    the callback binds; without it the module is refused (TS6142), the name resolves to
 *    nothing, and the row is read with no fixtures at all — a real row stripped of what it
 *    takes, which the rule then refuses on correct source.
 *  - the second is a `test.skip(<condition>, <reason>)` that marks a row and declares none.
 *    Where `bun` reaches the program, `process.env.SKIP_REASON` is a `string | undefined`,
 *    which is no body, so the call is read as the marker it is; under a `types` list that leaves
 *    `bun` out, `process` is `any`, the reason reads as a body, and the marking call is reported
 *    as a row that drives no browser — noise on correct source.
 *
 * Its name ends `.fixture.ts` and not `.e2e.ts` because e2e/playwright.config.ts says
 * `testDir: "."` and `testMatch: /.*\.e2e\.ts/`: Playwright walks the `e2e` directory
 * recursively, so a `.e2e.ts` file here, under `__tests__/fixtures/`, would join the real suite
 * and run. `bun test` collects only `*.test.ts` and its kin, so this file runs nowhere; the
 * scan parses it and the test reads the rows.
 */

import { test } from "@playwright/test";

import { opensAPageFromATsxModule } from "./tsx-row-callback";

test("a planted row whose callback is imported from a .tsx module", opensAPageFromATsxModule);

test.skip(!process.env.CI, process.env.SKIP_REASON);
