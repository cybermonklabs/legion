/** The default wait for asynchronous server and rendering readiness, in milliseconds. A spec only
 *  sets its own timeout when that deadline is its observable contract (the keyboard chord-expiry
 *  test).
 *
 *  `e2e/playwright.config.ts` sets it as every project's `expect.timeout`, and a row that waits
 *  outside `expect` passes it as well: the seed's waits (`seedWorkspace`) then give up at the
 *  bound the row's own assertions use, well inside the row's 60 s, so the wait's message reaches
 *  the reader before the row's clock ends the row. It is a module of its own because a row cannot
 *  read it from Playwright, whose `testInfo.project` carries no `expect` at run time or in its
 *  types (1.64.1), and a row must not import the config. */
export const expectTimeout = 15_000;
