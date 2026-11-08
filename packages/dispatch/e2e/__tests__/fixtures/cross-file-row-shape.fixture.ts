/** The fifth planted shape of e2e/__tests__/no-browser-rows.test.ts, and the half of the hole
 *  that same-file resolution cannot close: a row whose title is computed AND whose callback is
 *  an identifier imported from another module (`cross-file-row-callback.ts`). Playwright runs it
 *  like any other row and it binds no browser fixture, so `rowsOfSource` must report it and
 *  `unexplainedRows` must name it.
 *
 * `unresolved-row-shape.fixture.ts` beside it declares its callback in its own file, so reading
 * the declarations of the file under scan settles that one. Here there is nothing to read:
 * `rowsOfSource` parses this file alone, with no type checker and no second parse, and
 * `offTheBrowserInAnotherFile` names nothing it holds. The check must still report the row,
 * with no fixtures, because fixtures it did not read are not fixtures it saw — what tells this
 * call from the four `test.skip(<condition>, <reason>)` calls of the suite written with no
 * string literal is its callee: `test` takes a title first and nothing else, while `test.skip`
 * also takes a condition (@playwright/test 1.64.1, `types/test.d.ts`).
 *
 * Its name ends `.fixture.ts` and not `.e2e.ts` because e2e/playwright.config.ts says
 * `testDir: "."` and `testMatch: /.*\.e2e\.ts/`: Playwright walks the `e2e` directory
 * recursively, so a `.e2e.ts` file here, under `__tests__/fixtures/`, would join the real suite
 * and run. `bun test` collects only `*.test.ts` and its kin, so this file runs nowhere; the test
 * reads its text and parses it.
 */

import { test } from "@playwright/test";

import { offTheBrowserInAnotherFile } from "./cross-file-row-callback";

/** The row's title, computed so that the source alone cannot say what it reads. */
const computedTitle = [
  "a planted row whose callback is imported",
  "from a module beside this one",
].join(" ");

test(computedTitle, offTheBrowserInAnotherFile);
