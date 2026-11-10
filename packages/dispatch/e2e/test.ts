/** The `test` every row of the real-stack browser suite takes: Playwright's own, with the two
 *  things every one of those rows needs put on it once rather than written out 146 times.
 *
 *  **The reset runs as an automatic fixture, not as a hook each file copies.** A row of this
 *  suite seeds what it reads and must read nothing the row before it left, so it starts from an
 *  emptied database (`resetDatabase`, e2e/seed.ts). That used to be four lines at the top of
 *  every spec file -
 *
 *      test.beforeEach(async () => {
 *        await resetDatabase();
 *      });
 *
 *  - 146 copies of one hook: 146 places to read it, and one chance to forget it in every spec
 *  written since. As an automatic fixture it runs for every row of every file that imports this
 *  `test`, so a new spec resets by importing from here and a row cannot be written without one.
 *  A file whose setup does more than the reset still writes a hook, for that extra work alone.
 *
 *  **The sign-in is the default context's, not the row's.** Most rows of this suite open contexts
 *  of their own - `asUser(browser, "alice")`, `withAlice(browser, ...)` (e2e/users.ts) - and those
 *  are untouched: they take `{ browser }`, never instantiate the `context` fixture, and so never
 *  reach the override below or pay for it. What the override replaces is the other shape, the
 *  rows that take Playwright's own `page` or `context`, whose files each wrote the reset and the
 *  sign-in into one hook:
 *
 *      test.beforeEach(async ({ context }) => {
 *        await resetDatabase();
 *        await signIn(context, "alice");
 *      });
 *
 *  The order inside that hook is load-bearing, which is why the override names `freshDatabase`
 *  among its own fixtures: the sign-in carries the cookie e2e/api.ts holds for alice, and that
 *  cookie authenticates only because the reset put her `user_sessions` row back at the generation
 *  it names (the head comment of e2e/users.ts). Naming the reset fixture is what makes Playwright
 *  set it up first, so the order is kept here once instead of in each file that happened to
 *  write it.
 *
 *  **How a row opts out.** The `signedIn` option is the login the default context is signed in
 *  as, `"alice"` unless a file says otherwise, and `false` leaves that context signed out:
 *
 *      test.use({ signedIn: false });   // the reader nobody signed in
 *      test.use({ signedIn: "bob" });   // the default context is bob's
 *
 *  It is read where Playwright reads any option - at file scope, or inside a `test.describe` -
 *  and it governs the default context alone. A row that opens its own context signs that one in
 *  itself, as it always has.
 *
 *  The stubbed layer takes no part in any of this: it reaches no server, no database and no
 *  sign-in, and declares a `test` of its own (e2e/stub/stub-test.ts). So does the containerised
 *  visual review (e2e/visual.e2e.ts), whose rows seed outside Playwright.
 */

import { test as base } from "@playwright/test";

import { resetDatabase } from "./seed";
import { signIn } from "./users";

/** What a spec may set with `test.use`. */
type DispatchOptions = {
  /** The login Playwright's own `context` is signed in as, or `false` for a context nobody
   *  signed in. */
  signedIn: string | false;
};

/** What this `test` adds beyond the options. */
type DispatchFixtures = {
  /** The emptied database every row starts from. It is automatic, so no row asks for it, and it
   *  carries no value: what a row takes from it is that it ran. */
  freshDatabase: undefined;
};

export const test = base.extend<DispatchOptions & DispatchFixtures>({
  signedIn: ["alice", { option: true }],
  freshDatabase: [
    // Playwright refuses a first argument that is not a destructuring pattern.
    // biome-ignore lint/correctness/noEmptyPattern: the reset takes no other fixture.
    async ({}, use) => {
      await resetDatabase();
      await use(undefined);
    },
    { auto: true },
  ],
  // `freshDatabase` is in this list for its order rather than for its value, hence the unused
  // binding: naming it is what makes Playwright empty the database before this fixture signs
  // anyone in, which is the order the hooks this replaces were written in.
  context: async ({ context, signedIn, freshDatabase: _resetFirst }, use) => {
    if (signedIn !== false) {
      await signIn(context, signedIn);
    }
    await use(context);
  },
});

/** What a spec takes from Playwright beside this `test`, re-exported so a converted file has one
 *  import line rather than two: `devices` and `expect`, and the types the suite's specs name. A
 *  type Playwright exports and no spec names yet is not here; add it when a spec needs it. */
export type {
  APIResponse,
  Browser,
  BrowserContext,
  Locator,
  Page,
  Request,
  Response,
  Route,
  TestInfo,
  WebSocket,
  WebSocketRoute,
} from "@playwright/test";
export { devices, expect } from "@playwright/test";
