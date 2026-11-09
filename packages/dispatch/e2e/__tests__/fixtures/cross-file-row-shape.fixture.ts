/** The planted shape of e2e/__tests__/no-browser-rows.test.ts whose halves are BOTH names and
 *  whose callback is declared in ANOTHER MODULE: the title is computed and the second argument
 *  is an identifier imported from `cross-file-row-callback.ts`. Playwright runs it like any
 *  other row and it binds no browser fixture, so `rowsOfFiles` must report it and
 *  `unexplainedRows` must name it.
 *
 * `unresolved-row-shape.fixture.ts` beside it declares its callback in its own file. Here the
 * declaration is a module away, which a reading of the file under scan alone could not follow:
 * it reported the row with no fixtures and could not have said whether that was the truth. The
 * scan builds one program over the files it reads, so the import is a symbol like any other and
 * `getAliasedSymbol` leads to the function it names; that function takes no parameter, and the
 * empty fixture list is read rather than assumed.
 *
 * What says the call is a row at all is its callee: `test` takes a title first and nothing else,
 * while `test.skip` also takes a condition (@playwright/test 1.64.1, `types/test.d.ts`), which
 * is what keeps the four `test.skip(<condition>, <reason>)` calls of this suite out.
 *
 * Its name ends `.fixture.ts` and not `.e2e.ts` because e2e/playwright.config.ts says
 * `testDir: "."` and `testMatch: /.*\.e2e\.ts/`: Playwright walks the `e2e` directory
 * recursively, so a `.e2e.ts` file here, under `__tests__/fixtures/`, would join the real suite
 * and run. `bun test` collects only `*.test.ts` and its kin, so this file runs nowhere; the scan
 * parses it and the test reads the rows.
 */

import { test } from "@playwright/test";

import { offTheBrowserInAnotherFile } from "./cross-file-row-callback";

/** The row's title, computed so that the source alone cannot say what it reads. */
const computedTitle = [
  "a planted row whose callback is imported",
  "from a module beside this one",
].join(" ");

test(computedTitle, offTheBrowserInAnotherFile);
