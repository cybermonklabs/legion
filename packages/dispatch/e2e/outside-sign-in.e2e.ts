import { baseUrl, e2eAgentToken, sessionCookieName } from "./api";
import { expectSignedIn, queueGoogleSignIn, signInFromTheSignInPage } from "./google-signin";
import { googleOutsideOrigin, googleSignIn } from "./harness-signin";
import { sql } from "./psql";
import { conversationTurn } from "./reference-surfaces";
import { sqlLiteral } from "./seed";
import { type BrowserContext, expect, type Page, test } from "./test";

/**
 * A person from outside the sign-in domain signs in to Dispatch at its second origin as a
 * restricted member while a project's access list names them, and only then. Under
 * `DISPATCH_E2E_SIGNIN=google` (e2e/harness-signin.ts) each server signs people in with Google
 * for the Workspace domain `example.com`, with the dev OIDC issuer standing in for Google, and
 * with outside sign-in on (`DISPATCH_SIGNIN_OUTSIDE=1`); it answers at its canonical origin,
 * `http://127.0.0.1:<port>`, and at a second one, `http://localhost:<port>`
 * (`DISPATCH_OUTSIDE_ORIGIN`). The issuer signs in nobody of its own: each sign-in below first
 * queues its person, with the claims Google gives that account, at the issuer's
 * `/_dev/sign-in-as`.
 *
 * What a restricted member cannot read is avenue A's check, not this one's.
 */

const canonical = baseUrl;
const outside = googleOutsideOrigin(baseUrl);

/** A Workspace account of the sign-in domain, the project owner. */
const alice = `alice@${googleSignIn.domain}`;
/** A personal Google account outside the domain, which PRIV's access list names. */
const outsider = "outsider@other.example";
/** A personal Google account outside the domain, which no access list names. */
const stranger = "stranger@other.example";

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
  test("a person an access list names signs in at the outside origin as a restricted member, and is signed out once no list names them", async ({
    browser,
    request,
  }) => {
    const aliceContext = await browser.newContext();
    const outsiderContext = await browser.newContext();
    const strangerContext = await browser.newContext();
    try {
      const alicePage = await aliceContext.newPage();
      const outsiderPage = await outsiderContext.newPage();

      const { priv, pub } =
        await test.step("alice signs in at the canonical origin, makes PRIV and PUB, and lists the outsider on PRIV", async () => {
          await queueGoogleSignIn(alice, googleSignIn.domain);
          await expectSignedIn(await signInFromTheSignInPage(alicePage, canonical), alice);
          await expect(alicePage).toHaveURL(`${canonical}/`);
          await expect(alicePage.getByText(`Signed in as ${alice}`)).toBeVisible();
          // An account of the domain stays a full member with outside sign-in on: no member_kind.
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

      await test.step("the outsider signs in at the outside origin as a restricted member, and PRIV's issue opens", async () => {
        // No hd: a personal Google account, outside every Workspace.
        await queueGoogleSignIn(outsider);
        await expectSignedIn(await signInFromTheSignInPage(outsiderPage, outside), outsider);
        await expect(outsiderPage).toHaveURL(`${outside}/`);
        await expect(outsiderPage.getByText(`Signed in as ${outsider}`)).toBeVisible();
        expect(await whoami(outsiderPage), "the outsider's GET /auth/whoami").toEqual({
          body: {
            event_head: expect.stringMatching(/^\d+$/),
            kind: "user",
            login: outsider,
            member_kind: "restricted",
            origins: [canonical, outside],
          },
          status: 200,
        });
        await outsiderPage.goto(`${outside}/issues/${priv}`);
        await expect(outsiderPage.getByRole("heading", { name: "The private plan" })).toBeVisible();
      });

      await test.step("a link alice wrote at the canonical origin opens at the outside origin for the outsider", async () => {
        const comment = (await aliceWrites(
          aliceContext,
          "POST",
          `/api/v1/issues/${priv}/comments`,
          { body: `The plan is at ${canonical}/issues/${priv} now.` }
        )) as { id: string };
        await outsiderPage.goto(`${outside}/issues/${priv}/conversation`);
        const links = conversationTurn(outsiderPage, "comment", comment.id).locator(
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

      await test.step("an account no access list names is refused at the outside origin", async () => {
        const strangerPage = await strangerContext.newPage();
        await queueGoogleSignIn(stranger);
        const answered = await signInFromTheSignInPage(strangerPage, outside);
        // The page first, so a sign-in that should have been refused fails quoting what it reached
        // instead: the dashboard, signed in, where a status alone would say only 302.
        await expect(
          strangerPage.locator("body"),
          `the page ${stranger} reached once /auth/callback answered ${answered.status()}`
        ).toContainText(`${stranger} is not on the access list of any Dispatch project`);
        expect(answered.status(), `/auth/callback for ${stranger}`).toBe(403);
        expect((await strangerContext.cookies()).map((cookie) => cookie.name)).not.toContain(
          sessionCookieName
        );
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

      await test.step("once alice takes the outsider off PRIV, the next hourly confirmation signs them out, and the sign-in page says why", async () => {
        await aliceWrites(
          aliceContext,
          "DELETE",
          `/api/v1/projects/PRIV/access/people/${outsider}`
        );
        await sql(
          `update people set confirmed_at = now() - interval '2 hours' where email = ${sqlLiteral(outsider)}`
        );
        await outsiderPage.goto(`${outside}/issues/${priv}`);
        await expect(outsiderPage.getByRole("link", { name: "Sign in with Google" })).toBeVisible();
        await expect(
          outsiderPage.getByText(
            `You were signed out because no Dispatch project's access list names ${outsider} any more.`,
            { exact: true }
          )
        ).toBeVisible();
        expect(await whoami(outsiderPage), "the outsider's GET /auth/whoami").toMatchObject({
          body: { code: "NO_IDENTITY", login: outsider, reason: "NOT_LISTED" },
          status: 401,
        });
      });
    } finally {
      await Promise.all([aliceContext.close(), outsiderContext.close(), strangerContext.close()]);
    }
  });
});
