import { baseUrl, e2eAgentToken, sessionCookieName } from "./api";
import {
  canonicalWrite,
  expectSignedIn,
  noteRepeat,
  open,
  signInWithGoogle,
  whoami,
} from "./google-sign-in";
import { googleOutsideOrigin, googleSignIn } from "./harness-signin";
import { conversationTurn } from "./reference-surfaces";
import { type BrowserContext, expect, test } from "./test";

/**
 * Google signs in only the people of the sign-in domain, and only at Dispatch's canonical origin.
 * Under `DISPATCH_E2E_SIGNIN=google` (e2e/harness-signin.ts) each server signs people in with
 * Google for the Workspace domain `example.com`, with the dev OIDC issuer standing in for Google;
 * it answers at its canonical origin, `http://127.0.0.1:<port>`, and at a second one,
 * `http://localhost:<port>` (`DISPATCH_OUTSIDE_ORIGIN`), with passkey sign-in on there. A personal
 * Google account outside the domain is refused at the canonical origin even while a project's
 * access list names it, and the outside origin serves no Google sign-in at all: its `/auth/start`
 * and `/auth/callback` answer 404. The issuer signs in nobody of its own: each sign-in below first
 * queues its person, with the claims Google gives that account, at the issuer's `/_dev/sign-in-as`
 * (`signInWithGoogle`, e2e/google-sign-in.ts, which also makes a hop again that the host's network
 * change abandoned).
 */

const canonical = baseUrl;
const outside = googleOutsideOrigin(baseUrl);

/** A Workspace account of the sign-in domain, the project owner. */
const alice = `alice@${googleSignIn.domain}`;
/** A personal Google account outside the domain, which PRIV's access list names. */
const outsider = "outsider@other.example";

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
          await expectSignedIn(
            await signInWithGoogle(alicePage, canonical, alice, googleSignIn.domain, noteRepeat),
            alice
          );
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
          await canonicalWrite(aliceContext, "POST", "/api/v1/projects", {
            key: "PRIV",
            name: "Private work",
          });
          await canonicalWrite(aliceContext, "POST", "/api/v1/projects", {
            key: "PUB",
            name: "Public work",
          });
          // Only the owner may name a person here, so this answering at all says alice owns PRIV.
          await canonicalWrite(
            aliceContext,
            "PUT",
            `/api/v1/projects/PRIV/access/people/${outsider}`
          );
          const privIssue = (await canonicalWrite(aliceContext, "POST", "/api/v1/issues", {
            project: "PRIV",
            title: "The private plan",
          })) as { key: string };
          const pubIssue = (await canonicalWrite(aliceContext, "POST", "/api/v1/issues", {
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
        const answered = await signInWithGoogle(page, canonical, outsider, undefined, noteRepeat);
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
        await open(outsidePage, `${outside}/`);
        await expect(outsidePage.getByText(`Signed in as ${alice}`)).toBeVisible();
        const comment = (await canonicalWrite(
          aliceContext,
          "POST",
          `/api/v1/issues/${priv}/comments`,
          { body: `The plan is at ${canonical}/issues/${priv} now.` }
        )) as { id: string };
        await open(outsidePage, `${outside}/issues/${priv}/conversation`);
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
