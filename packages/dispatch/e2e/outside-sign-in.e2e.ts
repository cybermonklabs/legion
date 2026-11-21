import { baseUrl, e2eAgentToken, sessionCookieName } from "./api";
import { harnessPorts } from "./harness-ports";
import { googleOutsideOrigin, googleSignIn } from "./harness-signin";
import { conversationTurn } from "./reference-surfaces";
import { type BrowserContext, expect, type Page, type Response, test } from "./test";

/**
 * Google signs in only the people of the sign-in domain, and only at Dispatch's canonical origin.
 * Under `DISPATCH_E2E_SIGNIN=google` (e2e/harness-signin.ts) each server signs people in with
 * Google for the Workspace domain `example.com`, with the dev OIDC issuer standing in for Google;
 * it answers at its canonical origin, `http://127.0.0.1:<port>`, and at a second one,
 * `http://localhost:<port>` (`DISPATCH_OUTSIDE_ORIGIN`), with passkey sign-in on there. A personal
 * Google account outside the domain is refused at the canonical origin even while a project's
 * access list names it, and the outside origin serves no Google sign-in at all: its `/auth/start`
 * and `/auth/callback` answer 404. The issuer signs in nobody of its own: each sign-in below first
 * queues its person, with the claims Google gives that account, at the issuer's `/_dev/sign-in-as`.
 */

const canonical = baseUrl;
const outside = googleOutsideOrigin(baseUrl);

/** A Workspace account of the sign-in domain, the project owner. */
const alice = `alice@${googleSignIn.domain}`;
/** A personal Google account outside the domain, which PRIV's access list names. */
const outsider = "outsider@other.example";

/** Queues `email` as the person the issuer signs in at its next authorization, with the ID token
 *  claims Google gives that account: the address as its subject, as the issuer's own default
 *  person has, a verified email, and `hd` for a Workspace account alone. */
async function queueSignIn(email: string, hostedDomain?: string): Promise<void> {
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
async function signInFromTheSignInPage(page: Page, origin: string): Promise<Response> {
  await page.goto(`${origin}/`);
  const callback = page.waitForResponse(
    (response) => new URL(response.url()).pathname === "/auth/callback"
  );
  await page.getByRole("link", { name: "Sign in with Google" }).click();
  return callback;
}

/** Requires `answered` to be the callback's redirect into Dispatch, which sets the session. */
async function expectSignedIn(answered: Response, email: string): Promise<void> {
  // A redirect's body cannot be read; any other answer's body says why the sign-in stopped.
  const why = answered.status() === 302 ? "" : ` ${await answered.text()}`;
  expect(answered.status(), `/auth/callback for ${email} answered ${answered.status()}${why}`).toBe(
    302
  );
}

/** `GET /auth/whoami` as `page`'s own script asks it: from the page's origin, with its cookie. */
function whoami(page: Page): Promise<{ body: unknown; status: number }> {
  return page.evaluate(async () => {
    const response = await fetch("/auth/whoami");
    return { body: await response.json(), status: response.status };
  });
}

/** A write alice makes with her session at the canonical origin, where Dispatch takes a
 *  cookie-authenticated write only with that origin as its `Origin` (enforceCookieOrigin). */
async function aliceWrites(
  context: BrowserContext,
  method: "DELETE" | "POST" | "PUT",
  path: string,
  data?: object
): Promise<unknown> {
  const response = await context.request.fetch(`${canonical}${path}`, {
    data,
    headers: { Origin: canonical },
    method,
  });
  expect(response.ok(), `${method} ${path}: ${response.status()} ${await response.text()}`).toBe(
    true
  );
  return response.json();
}

// The servers mount no dev sign-in route under google (e2e/harness-signin.ts), so the default
// context's sign-in has nothing to mint at; every context below signs in through the issuer.
test.use({ signedIn: false });

// One row whose steps run in order, rather than a row per step: e2e/test.ts empties the database
// before every row, and each step reads what the steps before it wrote.
test.describe("outside sign-in", () => {
  test("a Google sign-in of an address outside the domain is refused though an access list names it, and the outside origin serves no Google sign-in", async ({
    browser,
    request,
  }) => {
    const aliceContext = await browser.newContext();
    const outsideAliceContext = await browser.newContext();
    const outsiderContexts: BrowserContext[] = [];
    try {
      const alicePage = await aliceContext.newPage();

      const { priv, pub } =
        await test.step("alice signs in at the canonical origin, makes PRIV and PUB, and lists the outsider on PRIV", async () => {
          await queueSignIn(alice, googleSignIn.domain);
          await expectSignedIn(await signInFromTheSignInPage(alicePage, canonical), alice);
          await expect(alicePage).toHaveURL(`${canonical}/`);
          await expect(alicePage.getByText(`Signed in as ${alice}`)).toBeVisible();
          expect(await whoami(alicePage), "alice's GET /auth/whoami").toEqual({
            body: {
              event_head: expect.stringMatching(/^\d+$/),
              kind: "user",
              login: alice,
              origins: [canonical, outside],
            },
            status: 200,
          });
          await aliceWrites(aliceContext, "POST", "/api/v1/projects", {
            key: "PRIV",
            name: "Private work",
          });
          await aliceWrites(aliceContext, "POST", "/api/v1/projects", {
            key: "PUB",
            name: "Public work",
          });
          // Only the owner may name a person here, so this answering at all says alice owns PRIV.
          await aliceWrites(aliceContext, "PUT", `/api/v1/projects/PRIV/access/people/${outsider}`);
          const privIssue = (await aliceWrites(aliceContext, "POST", "/api/v1/issues", {
            project: "PRIV",
            title: "The private plan",
          })) as { key: string };
          const pubIssue = (await aliceWrites(aliceContext, "POST", "/api/v1/issues", {
            project: "PUB",
            title: "The public plan",
          })) as { key: string };
          return { priv: privIssue.key, pub: pubIssue.key };
        });

      await test.step(`the outsider's Google sign-in at ${canonical} is refused as not in the domain`, async () => {
        const context = await browser.newContext();
        outsiderContexts.push(context);
        const page = await context.newPage();
        // No hd: a personal Google account, outside every Workspace.
        await queueSignIn(outsider);
        const answered = await signInFromTheSignInPage(page, canonical);
        // The page first, so a sign-in that should have been refused fails quoting what it
        // reached instead: the dashboard, signed in, where a status alone would say only 302.
        await expect(
          page.locator("body"),
          `the page ${outsider} reached at ${canonical} once /auth/callback answered ${answered.status()}`
        ).toContainText(`${outsider} is not in the domain that may use Dispatch`);
        expect(answered.status(), `/auth/callback for ${outsider} at ${canonical}`).toBe(403);
        expect((await context.cookies()).map((cookie) => cookie.name)).not.toContain(
          sessionCookieName
        );
      });

      await test.step(`${outside} serves no Google sign-in: its /auth/start and /auth/callback answer 404`, async () => {
        for (const path of ["/auth/start", "/auth/callback?code=c&state=s"]) {
          const answered = await request.get(`${outside}${path}`, { maxRedirects: 0 });
          expect(answered.status(), `GET ${outside}${path}: ${await answered.text()}`).toBe(404);
          expect(answered.headers()["set-cookie"], `GET ${outside}${path} sets no cookie`).toBe(
            undefined
          );
        }
      });

      await test.step("alice's session at the outside origin opens a link she wrote at the canonical origin there", async () => {
        // Nobody signs in with Google at the outside origin, so alice's session is carried there
        // as a browser holding it would send it: the claim here is where a link leads on that
        // origin's page, not how a person signs in to it.
        const session = (await aliceContext.cookies(canonical)).find(
          (cookie) => cookie.name === sessionCookieName
        );
        expect(session, "alice's session cookie at the canonical origin").toBeDefined();
        await outsideAliceContext.addCookies([
          { name: sessionCookieName, url: outside, value: session?.value ?? "" },
        ]);
        const outsidePage = await outsideAliceContext.newPage();
        await outsidePage.goto(`${outside}/`);
        await expect(outsidePage.getByText(`Signed in as ${alice}`)).toBeVisible();
        const comment = (await aliceWrites(
          aliceContext,
          "POST",
          `/api/v1/issues/${priv}/comments`,
          { body: `The plan is at ${canonical}/issues/${priv} now.` }
        )) as { id: string };
        await outsidePage.goto(`${outside}/issues/${priv}/conversation`);
        const links = conversationTurn(outsidePage, "comment", comment.id).locator(
          `a[href$="/issues/${priv}"]`
        );
        // The href as the page resolves it, which is where a press goes: an attribute written as
        // a path resolves on the page's own origin, and one left as the canonical URL does not.
        await expect
          .poll(
            () =>
              links.evaluateAll((anchors) => [
                ...new Set(anchors.map((anchor) => (anchor as HTMLAnchorElement).href)),
              ]),
            { message: `where the comment's link to ${priv} leads on the outside origin` }
          )
          .toEqual([`${outside}/issues/${priv}`]);
      });

      await test.step("the outside origin answers Dispatch's API, not only its sign-in", async () => {
        const read = await request.get(`${outside}/api/v1/issues/${pub}`, {
          headers: { Authorization: `Bearer ${e2eAgentToken}` },
        });
        expect(read.status(), `GET ${outside}/api/v1/issues/${pub}: ${await read.text()}`).toBe(
          200
        );
        expect(await read.json()).toMatchObject({ key: pub });
      });
    } finally {
      await Promise.all(
        [aliceContext, outsideAliceContext, ...outsiderContexts].map((context) => context.close())
      );
    }
  });
});
