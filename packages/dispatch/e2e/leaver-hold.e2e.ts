import { baseUrl } from "./api";
import { expectSignedIn, queueGoogleSignIn, signInFromTheSignInPage } from "./google-signin";
import { harnessPorts } from "./harness-ports";
import { googleSignIn } from "./harness-signin";
import { sql } from "./psql";
import { sqlLiteral } from "./seed";
import { expect, type Page, test } from "./test";

/**
 * Dispatch holds the personal tokens of a person Google no longer confirms, and revokes nothing:
 * the same token works again once its owner signs in. Under `DISPATCH_E2E_SIGNIN=google`
 * (e2e/harness-signin.ts) each server signs people in with Google for the Workspace domain
 * `example.com`, with the dev OIDC issuer standing in for Google. The issuer's loopback-only
 * `POST /_dev/refuse-refresh` makes it answer one person's refresh `400 invalid_grant`, as Google
 * answers the refresh of an account it no longer confirms. Real Google's refusal is the one part
 * not driven here; it reaches Dispatch as the same error.
 */

/** A Workspace account of the sign-in domain. */
const alice = `alice@${googleSignIn.domain}`;

/** Tells the issuer to refuse alice's refresh, or to serve it again. */
async function refuseAlicesRefresh(refuse: boolean): Promise<void> {
  const issuer = `http://127.0.0.1:${harnessPorts().signinIssuer.port}`;
  const answered = await fetch(`${issuer}/_dev/refuse-refresh`, {
    body: JSON.stringify({ email: alice, refuse }),
    method: "POST",
  });
  expect(
    answered.status,
    `the issuer took refuse=${refuse} for ${alice}: ${await answered.text()}`
  ).toBe(204);
}

/** Queues alice at the issuer, signs her in from Dispatch's sign-in page, and requires that she
 *  reached Dispatch signed in. */
async function aliceSignsIn(page: Page): Promise<void> {
  await queueGoogleSignIn(alice, googleSignIn.domain);
  await expectSignedIn(await signInFromTheSignInPage(page, baseUrl), alice);
  await expect(page.getByText(`Signed in as ${alice}`)).toBeVisible();
}

// The servers mount no dev sign-in route under google (e2e/harness-signin.ts), so the default
// context's sign-in has nothing to mint at; alice signs in through the issuer.
test.use({ signedIn: false });

// One row whose steps run in order: e2e/test.ts empties the database before every row, and each
// step reads what the steps before it wrote.
test("a held personal token works again after its owner signs in", async ({ page, request }) => {
  /** `GET /api/v1/whoami` with `token` alone: the `request` fixture carries no cookie. */
  const whoami = async (token: string) => {
    const answered = await request.get(`${baseUrl}/api/v1/whoami`, {
      headers: { Authorization: `Bearer ${token}` },
    });
    return { body: await answered.text(), status: answered.status() };
  };

  try {
    await test.step("alice signs in from the sign-in page", async () => {
      await aliceSignsIn(page);
    });

    const token =
      await test.step("alice makes a personal token on the agent tokens page, and it answers 200", async () => {
        await page.goto(`${baseUrl}/settings`);
        await page.getByLabel("Token label").fill("Leaver hold");
        await page.getByRole("button", { exact: true, name: "New token" }).click();
        const shown = page.getByRole("status").locator("code").first();
        await expect(shown).toHaveText(/^dsp_[A-Za-z0-9_-]{43}$/);
        const made = (await shown.textContent()) ?? "";
        const answered = await whoami(made);
        expect(answered.status, `GET /api/v1/whoami with the new token: ${answered.body}`).toBe(
          200
        );
        expect(JSON.parse(answered.body)).toMatchObject({ kind: "agent", owner: alice });
        return made;
      });

    await test.step("alice presses Sign out, and her token still answers 200", async () => {
      await page.getByRole("button", { name: "Sign out" }).click();
      await expect(page.getByRole("link", { name: "Sign in with Google" })).toBeVisible();
      const answered = await whoami(token);
      expect(answered.status, `GET /api/v1/whoami after the sign-out: ${answered.body}`).toBe(200);
    });

    await test.step("the issuer refuses alice's refresh, and her confirmation is two hours old", async () => {
      await refuseAlicesRefresh(true);
      // Only a confirmation the sign-out kept is aged: a row holds one exactly while it holds a
      // refresh token (the people table's check), so a sign-out that forgot the token leaves
      // nothing to age, and the next step reads the token as Dispatch then answers it.
      await sql(
        `update people set confirmed_at = now() - interval '2 hours' where email = ${sqlLiteral(alice)} and confirmed_at is not null`
      );
    });

    await test.step("after Google refuses alice, her token answers 401 MEMBERSHIP_HELD and tells her to sign in to Dispatch again", async () => {
      const answered = await whoami(token);
      expect(
        answered.status,
        `GET /api/v1/whoami after Google refused alice: ${answered.body}`
      ).toBe(401);
      expect(JSON.parse(answered.body)).toMatchObject({
        code: "MEMBERSHIP_HELD",
        error: expect.stringContaining("sign in to Dispatch again"),
      });
    });

    await test.step("Google confirms alice again, she signs in from the sign-in page, and the same token answers 200", async () => {
      await refuseAlicesRefresh(false);
      await aliceSignsIn(page);
      const answered = await whoami(token);
      expect(answered.status, `GET /api/v1/whoami after alice signed in: ${answered.body}`).toBe(
        200
      );
      expect(JSON.parse(answered.body)).toMatchObject({ kind: "agent", owner: alice });
    });
  } finally {
    // The issuer outlives the row, where the database does not: a row that stopped while alice
    // was refused would leave the next row's alice refused.
    await refuseAlicesRefresh(false);
  }
});
