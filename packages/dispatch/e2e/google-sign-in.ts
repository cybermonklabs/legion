/** Google sign-in under `DISPATCH_E2E_SIGNIN=google` (e2e/harness-signin.ts), with the dev OIDC
 *  issuer standing in for Google, and what the two drivers of that mode share
 *  (`outside-passkey.e2e.ts`, `outside-sign-in.e2e.ts`): the navigation repeat that every page
 *  load of theirs goes through, the sign-in's check, `whoami`, and the owner's writes.
 *
 *  Chromium fails a navigation with `net::ERR_NETWORK_CHANGED` when the host's network interfaces
 *  change while the request is still connecting: it drops its socket pools on an address change,
 *  and the request reaches no server. On a machine whose containers come and go that happens many
 *  times a minute: 332 interface events in 30 s were counted on the shared machine this was
 *  measured on (2026-11-21), and 1 in 300 sign-ins there lost a navigation to it. A redirect hop
 *  of the sign-in that it hits leaves the page on Chromium's error page, so a wait for Dispatch's
 *  `/auth/callback` sees nothing until its deadline, 45 s on, and fails naming no hop. So a
 *  navigation abandoned that way is made again from the URL it was abandoned at, at most
 *  `networkChangeRepeats` times, and the caller hears of each repeat. A navigation that fails any
 *  other way fails as it did. */

import { baseUrl } from "./api";
import { harnessPorts } from "./harness-ports";
import { type BrowserContext, expect, type Page, type Request, type Response, test } from "./test";

export const NETWORK_CHANGED = "net::ERR_NETWORK_CHANGED";

const networkChangeRepeats = 3;

/** How long the sign-in's redirect chain may take: the config's action deadline. On a loaded
 *  machine (2026-11-21) the issuer answered each of 19 timed authorizations within 20 ms, and 299
 *  of 300 sign-ins reached Dispatch's `/auth/callback`, the other losing its navigation to a
 *  network change. So a hop that has not answered by then has stopped, and the sign-in fails
 *  naming that hop. */
const signInBoundMs = 45_000;

/** Runs `navigation` - a goto or a reload - and makes it again when Chromium abandoned it because
 *  the host's network changed. `onRepeat` hears of each repeat. */
export async function repeatOnNetworkChange<T>(
  what: string,
  navigation: () => Promise<T>,
  onRepeat: (note: string) => void
): Promise<T> {
  for (let attempt = 0; ; attempt++) {
    try {
      return await navigation();
    } catch (error) {
      if (attempt >= networkChangeRepeats || !String(error).includes(NETWORK_CHANGED)) throw error;
      onRepeat(`${what}: ${NETWORK_CHANGED}, made again`);
    }
  }
}

/** Records a navigation the host's network change made a driver repeat, or a wait of `goAgain`'s
 *  that ran out, on the row's report, so it is seen rather than silent. */
export function noteRepeat(note: string): void {
  test.info().annotations.push({ description: note, type: "network-change" });
}

/** Opens `url` in `page`, again if Chromium abandoned it because the host's network changed. */
export function open(page: Page, url: string): Promise<unknown> {
  return repeatOnNetworkChange(`GET ${url}`, () => page.goto(url), noteRepeat);
}

/** Reloads `page`, again if Chromium abandoned it because the host's network changed. */
export function reload(page: Page): Promise<unknown> {
  return repeatOnNetworkChange(`reload ${page.url()}`, () => page.reload(), noteRepeat);
}

/** How long `goAgain` waits for Chromium's error page of the navigation it abandoned. */
const errorPageWaitMs = 5_000;

/** Opens `url` in `page` after Chromium abandoned a navigation there. The abandoned navigation
 *  commits Chromium's error page (`chrome-error://chromewebdata/`) shortly after its request
 *  fails, and that commit would interrupt a navigation begun before it, so this waits for the
 *  error page first (`errorPageWaitMs` at most), then navigates once. A wait that runs out is
 *  told to `onNote` before the navigation, so a navigation the late error page then interrupts
 *  fails beside a note that says why. Navigating again after an interrupted attempt would not
 *  do: the attempt Playwright reports interrupted can still run its redirect chain, and a second
 *  one then finds the issuer's queued person spent. */
export async function goAgain(
  page: Page,
  url: string,
  onNote: (note: string) => void
): Promise<void> {
  if (!page.url().startsWith("chrome-error:")) {
    const committed = await page
      .waitForURL((at) => at.protocol === "chrome-error:", {
        timeout: errorPageWaitMs,
        waitUntil: "commit",
      })
      .then(
        () => true,
        () => false
      );
    if (!committed) {
      onNote(
        `GET ${url}: Chromium's error page did not commit within ${errorPageWaitMs / 1000} s; navigated from ${page.url()} anyway`
      );
    }
  }
  await page.goto(url);
}

/** The dev OIDC issuer's origin. */
export function issuerOrigin(): string {
  return `http://127.0.0.1:${harnessPorts().signinIssuer.port}`;
}

/** Queues `email` as the person the issuer signs in at its next authorization, with the ID token
 *  claims Google gives that account: the address as its subject, a verified email, and `hd` for a
 *  Workspace account alone (a `hostedDomain` of `undefined` is a personal Google account). */
export async function queueGoogleSignIn(
  email: string,
  hostedDomain: string | undefined
): Promise<void> {
  const claims = {
    email,
    email_verified: true,
    sub: email,
    ...(hostedDomain === undefined ? {} : { hd: hostedDomain }),
  };
  const queued = await fetch(`${issuerOrigin()}/_dev/sign-in-as`, {
    body: JSON.stringify(claims),
    method: "POST",
  });
  if (queued.status !== 204) {
    throw new Error(`the issuer did not queue ${email}: ${queued.status} ${await queued.text()}`);
  }
}

/** Queues `email` at the issuer, opens `origin` signed out, presses its "Sign in with Google"
 *  link, and answers Dispatch's `/auth/callback` once the issuer sends the browser back. Each hop
 *  of the redirect chain - Dispatch's `/auth/start`, the issuer's `/authorize`, Dispatch's
 *  `/auth/callback` - is watched:
 *  - a hop Chromium abandoned because the host's network changed is made again from its own URL
 *    (it reached no server, so the issuer's queued person and the callback's code are unspent);
 *  - a hop that failed any other way, or that answered an error status, fails the sign-in at once,
 *    naming the hop and what it got;
 *  - a hop still unanswered after `signInBoundMs` fails it naming that hop.
 *
 *  `repeatable` is the browser errors a hop is made again for, `NETWORK_CHANGED` alone unless a
 *  caller names others: Playwright cannot fail a request with that error itself (its
 *  `route.abort` codes hold no network change), so the row that proves the repeat
 *  (`outside-passkey.e2e.ts`) drops a hop with another error and names it here. */
export async function signInWithGoogle(
  page: Page,
  origin: string,
  email: string,
  hostedDomain: string | undefined,
  onRepeat: (note: string) => void,
  repeatable: readonly string[] = [NETWORK_CHANGED]
): Promise<Response> {
  await queueGoogleSignIn(email, hostedDomain);
  await repeatOnNetworkChange(`GET ${origin}/`, () => page.goto(`${origin}/`), onRepeat);
  const issuer = issuerOrigin();
  const hopOf = (url: string): string | undefined => {
    const { origin: at, pathname } = new URL(url);
    if (at === origin && (pathname === "/auth/start" || pathname === "/auth/callback")) {
      return `Dispatch's ${pathname}`;
    }
    if (at === issuer && pathname === "/authorize") return "the issuer's /authorize";
    return undefined;
  };
  const unanswered = new Set<string>();
  let repeats = 0;
  const { promise: answered, resolve, reject } = Promise.withResolvers<Response>();
  // A hop can fail while the press below is still pending, before anything awaits the answer;
  // the runner fails a row on a rejection nothing handles yet, so the answer is marked handled
  // here, and `await answered` below still throws it.
  answered.catch(() => undefined);
  const sent = (request: Request) => {
    const hop = hopOf(request.url());
    if (hop !== undefined && request.isNavigationRequest()) unanswered.add(hop);
  };
  const received = async (response: Response) => {
    const hop = hopOf(response.url());
    if (hop === undefined) return;
    unanswered.delete(hop);
    if (new URL(response.url()).pathname === "/auth/callback") {
      resolve(response);
    } else if (response.status() >= 400) {
      const body = await response.text().catch(() => "");
      reject(new Error(`${hop} answered the sign-in of ${email} ${response.status()}: ${body}`));
    }
  };
  const failed = (request: Request) => {
    const hop = hopOf(request.url());
    if (hop === undefined || !request.isNavigationRequest()) return;
    unanswered.delete(hop);
    const error = request.failure()?.errorText ?? "an error the browser did not name";
    if (repeatable.includes(error) && repeats < networkChangeRepeats) {
      repeats++;
      onRepeat(`${hop}: ${error}, made again`);
      // The repeat's own events settle the wait, and a failure of it reaches `failed` again.
      void goAgain(page, request.url(), onRepeat).catch(() => undefined);
      return;
    }
    reject(new Error(`the browser's request to ${hop} for ${email} failed: ${error}`));
  };
  page.on("request", sent);
  page.on("response", received);
  page.on("requestfailed", failed);
  const bound = setTimeout(
    () =>
      reject(
        new Error(
          `the sign-in of ${email} at ${origin} did not reach Dispatch's /auth/callback within ${signInBoundMs / 1000} s; unanswered: ${[...unanswered].join(", ") || "no hop of the chain was sent"}`
        )
      ),
    signInBoundMs
  );
  try {
    // The press does not wait for the navigation it starts: Playwright's wait for it would end
    // at the action deadline as a bare click timeout that names no hop, before the bound above.
    await page.getByRole("link", { name: "Sign in with Google" }).click({ noWaitAfter: true });
    return await answered;
  } finally {
    clearTimeout(bound);
    page.off("request", sent);
    page.off("response", received);
    page.off("requestfailed", failed);
  }
}

/** Requires `answered`, Dispatch's `/auth/callback`, to be its redirect into Dispatch, which sets
 *  the session. */
export async function expectSignedIn(answered: Response, email: string): Promise<void> {
  // A redirect's body cannot be read; any other answer's body says why the sign-in stopped.
  const why = answered.status() === 302 ? "" : ` ${await answered.text()}`;
  expect(answered.status(), `/auth/callback for ${email} answered ${answered.status()}${why}`).toBe(
    302
  );
}

/** `GET /auth/whoami` as `page`'s own script asks it: from the page's origin, with its cookie. */
export function whoami(page: Page): Promise<{ body: unknown; status: number }> {
  return page.evaluate(async () => {
    const response = await fetch("/auth/whoami");
    return { body: await response.json(), status: response.status };
  });
}

/** A write made with `context`'s session at the canonical origin, where Dispatch takes a
 *  cookie-authenticated write only with that origin as its `Origin` (enforceCookieOrigin). */
export async function canonicalWrite(
  context: BrowserContext,
  method: "DELETE" | "POST" | "PUT",
  path: string,
  data?: object
): Promise<unknown> {
  const response = await context.request.fetch(`${baseUrl}${path}`, {
    data,
    headers: { Origin: baseUrl },
    method,
  });
  expect(response.ok(), `${method} ${path}: ${response.status()} ${await response.text()}`).toBe(
    true
  );
  return response.json();
}
