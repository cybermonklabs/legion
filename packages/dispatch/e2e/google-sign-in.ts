/** Google sign-in under `DISPATCH_E2E_SIGNIN=google` (e2e/harness-signin.ts), with the dev OIDC
 *  issuer standing in for Google, and the navigation repeat that every page load of
 *  `outside-passkey.e2e.ts` goes through.
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

import { harnessPorts } from "./harness-ports";
import type { Page, Request, Response } from "./test";

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

/** The dev OIDC issuer's origin. */
export function issuerOrigin(): string {
  return `http://127.0.0.1:${harnessPorts().signinIssuer.port}`;
}

/** Queues `email` as the person the issuer signs in at its next authorization, with the ID token
 *  claims Google gives that account: the address as its subject, a verified email, and `hd` for a
 *  Workspace account. */
export async function queueGoogleSignIn(email: string, hostedDomain: string): Promise<void> {
  const queued = await fetch(`${issuerOrigin()}/_dev/sign-in-as`, {
    body: JSON.stringify({ email, email_verified: true, hd: hostedDomain, sub: email }),
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
  hostedDomain: string,
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
      void page.goto(request.url()).catch(() => undefined);
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
