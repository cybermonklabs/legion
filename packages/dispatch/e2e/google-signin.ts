// How a spec under DISPATCH_E2E_SIGNIN=google (e2e/harness-signin.ts) signs a person in, with the
// dev OIDC issuer standing in for Google: it queues the person at the issuer, which signs in
// nobody of its own, then presses Dispatch's sign-in link and reads what `/auth/callback`
// answered. It is not in e2e/harness-signin.ts: e2e/harness-ports.ts imports that module, and
// this one reads the issuer's port from e2e/harness-ports.ts, so the two would import each other.
import { expect, type Page, type Response } from "@playwright/test";

import { harnessPorts } from "./harness-ports";

/** Queues `email` as the person the issuer signs in at its next authorization, with the ID token
 *  claims Google gives that account: the address as its subject, as the issuer's own default
 *  person has, a verified email, and `hd` for a Workspace account alone. */
export async function queueGoogleSignIn(email: string, hostedDomain?: string): Promise<void> {
  const claims = {
    email,
    email_verified: true,
    sub: email,
    ...(hostedDomain === undefined ? {} : { hd: hostedDomain }),
  };
  const issuer = `http://127.0.0.1:${harnessPorts().signinIssuer.port}`;
  const queued = await fetch(`${issuer}/_dev/sign-in-as`, {
    body: JSON.stringify(claims),
    method: "POST",
  });
  expect(queued.status, `the issuer queued ${email}: ${await queued.text()}`).toBe(204);
}

/** Opens Dispatch at `origin` signed out, presses its sign-in link, and returns what
 *  `/auth/callback` answered once the issuer sent the browser back there. */
export async function signInFromTheSignInPage(page: Page, origin: string): Promise<Response> {
  await page.goto(`${origin}/`);
  const callback = page.waitForResponse(
    (response) => new URL(response.url()).pathname === "/auth/callback"
  );
  await page.getByRole("link", { name: "Sign in with Google" }).click();
  return callback;
}

/** Requires `answered` to be the callback's redirect into Dispatch, which sets the session. */
export async function expectSignedIn(answered: Response, email: string): Promise<void> {
  // A redirect's body cannot be read; any other answer's body says why the sign-in stopped.
  const why = answered.status() === 302 ? "" : ` ${await answered.text()}`;
  expect(answered.status(), `/auth/callback for ${email} answered ${answered.status()}${why}`).toBe(
    302
  );
}
