import type { Browser, BrowserContext, BrowserContextOptions, Page } from "@playwright/test";

import { devSignInError, devSignInPath, type ResetLogin, reusableSessionCookie } from "./api";
import { measured } from "./measure";

/** A login a sign-in takes that no reset re-creates: the running row's own person,
 *  `rowPerson(...)`, or a login fixed on purpose, `fixedPerson(...)` (both e2e/test.ts), whose
 *  `people` and `user_sessions` rows the row's end deletes (`ownPeople`, e2e/test.ts); or a
 *  fixed login that leaves no person a later row reads, `fixedLogin(...)`, below. A literal
 *  login other than alice or bob would leave a person in the real-stack database until the next
 *  reset. The worker resets before its first row and before it seeds the review corpus
 *  (`e2e/test.ts`). A few rows also reset the database themselves. Until then, a later row that
 *  reads the whole people list would see that person in its assignee controls. The brand turns
 *  that sign-in into a type error, as `RowKey` does for a key (e2e/api.ts). */
export type RowLogin = string & { readonly rowLogin: unique symbol };

/** What `signIn`, `asUser`, `asPlainHttpUser`, the `signedIn` option (e2e/test.ts) and the API
 *  client (`login`, e2e/api.ts) sign in as, and so what each helper that signs in as a login its
 *  caller passes takes: alice or bob (`resetLogins`, e2e/api.ts), whom every reset re-creates, or
 *  a `RowLogin`. */
export type SignInLogin = ResetLogin | RowLogin;

/** `login` as a login the sign-ins take, for a caller whose login is fixed on purpose and leaves
 *  no person that a later row reads: the docs' pictures, outside the real-stack suite's rows,
 *  which show it (docs/site/media/broker/flow.ts); a stubbed row's seed, whose recording holds it
 *  and which runs in record mode alone, after `resetDatabase` (e2e/stub/stub-test.ts); and alice
 *  or bob in another case, which the server records as that person (issue.e2e.ts reads that it
 *  does). `why` names that reason where the login is written, and a call without one throws, as
 *  `fixedKey`'s does (e2e/api.ts). A real-stack row that needs a person of a fixed login writes
 *  `fixedPerson(login, why)` (e2e/test.ts) instead, whose person the row's end deletes. */
export function fixedLogin(login: string, why: string): RowLogin {
  if (why.trim() === "") {
    throw new Error(
      `fixedLogin: the fixed login ${JSON.stringify(login)} must name why it is fixed`
    );
  }
  return login as RowLogin;
}

/** Signs `context` in as `login`, with the cookie an issuer sign-in issues.
 *
 *  The first sign-in of alice or bob since the last reset carries the cookie e2e/api.ts already
 *  holds for that login into the context (`reusableSessionCookie`), rather than asking the
 *  harness server for one: the reset re-created the `user_sessions` row that cookie names
 *  (e2e/seed.ts), so one cache serves the row's API seeds and browser contexts as the same
 *  session. The worker resets before its first row and before it seeds the review corpus
 *  (`e2e/test.ts`). Some rows also call `resetDatabase` (e2e/seed.ts) themselves. The cookie
 *  carries no state of an earlier row: it names a login and a generation that those rows do not
 *  change. A row signs out only a person of its own (`rowPerson`).
 *
 *  Every other sign-in mints at the harness server's dev sign-in route, as every sign-in did: the
 *  context's request client shares the context's cookie jar, and a relative URL resolves against
 *  the config's baseURL. The route answers a sign-in with a 302, which is not followed: the
 *  redirect is the dashboard, which this does not need. */
export async function signIn(context: BrowserContext, login: SignInLogin): Promise<void> {
  await measured("sign-in", async () => {
    const reusable = reusableSessionCookie(login);
    if (reusable !== undefined) {
      await context.addCookies([await reusable]);
      return;
    }
    const response = await context.request.get(devSignInPath(login), { maxRedirects: 0 });
    if (response.status() !== 302) {
      throw devSignInError(login, response.status(), await response.text());
    }
  });
}

export async function asUser(
  browser: Browser,
  login: SignInLogin,
  options: BrowserContextOptions = {}
): Promise<BrowserContext> {
  const context = await browser.newContext(options);
  await signIn(context, login);
  return context;
}

/** Opens a page signed in as alice for `check`, and closes her context once it returns. */
export async function withAlice(
  browser: Browser,
  check: (page: Page) => Promise<void>
): Promise<void> {
  const alice = await asUser(browser, "alice");
  try {
    await check(await alice.newPage());
  } finally {
    await alice.close();
  }
}

/** `asUser` for the `chromium-plain-http` project: a context signed in as `login` by opening the
 *  dev sign-in route in a page rather than through `context.request`. The project's host name
 *  resolves only inside Chromium (its `--host-resolver-rules`), while the request client resolves
 *  names in Node, which answers `ENOTFOUND`. A sign-in the route refuses is not redirected, so the
 *  page's own answer, with its body, is the refusal. */
export async function asPlainHttpUser(
  browser: Browser,
  login: SignInLogin
): Promise<BrowserContext> {
  const context = await browser.newContext();
  const page = await context.newPage();
  const landed = await measured("sign-in", () => page.goto(devSignInPath(login)));
  const signInResponse = (await landed?.request().redirectedFrom()?.response()) ?? landed;
  if (signInResponse?.status() !== 302) {
    throw devSignInError(
      login,
      signInResponse?.status() ?? 0,
      signInResponse ? await signInResponse.text() : ""
    );
  }
  await page.close();
  return context;
}
