import { baseUrl } from "./api";
import { goAgain, repeatOnNetworkChange, signInWithGoogle } from "./google-sign-in";
import { googleOutsideOrigin, googleSignIn } from "./harness-signin";
import { sql } from "./psql";
import { resetDatabase, sqlLiteral } from "./seed";
import {
  type BrowserContext,
  expect,
  type Locator,
  type Page,
  type Response,
  type Route,
  test,
} from "./test";
import { addVirtualPasskey, type VirtualPasskey } from "./virtual-passkey";

/**
 * A person from outside the sign-in domain enrolls a passkey through the link the owner of a
 * project makes on its Access tab, and signs in with it at Dispatch's outside origin as a
 * restricted member. Under `DISPATCH_E2E_SIGNIN=google` (e2e/harness-signin.ts) each server signs
 * the people of the Workspace domain `example.com` in with Google at its canonical origin,
 * `http://127.0.0.1:<port>`, with the dev OIDC issuer standing in for Google, and answers at a
 * second origin, `http://localhost:<port>` (`DISPATCH_OUTSIDE_ORIGIN`), where passkey sign-in is
 * on (`DISPATCH_SIGNIN_PASSKEY=1`). The person's device is Chromium's virtual authenticator, which
 * Playwright reaches through the page's CDP session (the WebAuthn domain): it makes a
 * discoverable credential and verifies the user with no prompt.
 *
 * What a restricted member cannot read is avenue A's check, not this one's; the ceremony's own
 * rules (a sign count, the hourly check) are the Go tests' of the same check, and this row reads
 * each of them only as far as the page shows it.
 */

const canonical = baseUrl;
const outside = googleOutsideOrigin(baseUrl);

/** A Workspace account of the sign-in domain, the owner of PRIV. */
const alice = `alice@${googleSignIn.domain}`;
/** A Workspace colleague PRIV's list names directly, whom no link may be made for. */
const colleague = `carol@${googleSignIn.domain}`;
/** The person from outside whom PRIV's list names, and who enrolls. */
const outsider = "outsider@other.example";
/** A second person from outside PRIV's list names, whose links are the expired and delisted
 *  controls. */
const later = "later@other.example";

/** Records a navigation the host's network change made the driver repeat (e2e/google-sign-in.ts)
 *  on the row's report, so a repeat is seen rather than silent. */
function noteRepeat(note: string): void {
  test.info().annotations.push({ description: note, type: "network-change" });
}

/** Opens `url` in `page`, again if Chromium abandoned it because the host's network changed. */
function open(page: Page, url: string): Promise<unknown> {
  return repeatOnNetworkChange(`GET ${url}`, () => page.goto(url), noteRepeat);
}

/** Reloads `page`, again if Chromium abandoned it because the host's network changed. */
function reload(page: Page): Promise<unknown> {
  return repeatOnNetworkChange(`reload ${page.url()}`, () => page.reload(), noteRepeat);
}

/** Signs alice in at the canonical origin through the sign-in page's Google link. */
async function aliceSignsIn(page: Page): Promise<void> {
  const answered: Response = await signInWithGoogle(
    page,
    canonical,
    alice,
    googleSignIn.domain,
    noteRepeat
  );
  const why = answered.status() === 302 ? "" : ` ${await answered.text()}`;
  expect(answered.status(), `/auth/callback for ${alice} answered ${answered.status()}${why}`).toBe(
    302
  );
  await expect(page.getByText(`Signed in as ${alice}`)).toBeVisible();
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

/** `GET /auth/whoami` as `page`'s own script asks it: from the page's origin, with its cookie. */
function whoami(page: Page): Promise<{ body: unknown; status: number }> {
  return page.evaluate(async () => {
    const response = await fetch("/auth/whoami");
    return { body: await response.json(), status: response.status };
  });
}

/** The project keys the page's sidebar lists, in its order. */
function sidebarProjects(page: Page): Promise<string[]> {
  return page
    .getByRole("navigation", { name: "Navigation" })
    .locator('a[href^="/projects/"]')
    .evaluateAll((links) =>
      links.map((link) => new URL((link as HTMLAnchorElement).href).pathname.split("/")[2] ?? "")
    );
}

/** The one credential `device` holds, as the authenticator keeps it. */
async function onlyCredential(device: VirtualPasskey) {
  const { credentials } = await device.cdp.send("WebAuthn.getCredentials", {
    authenticatorId: device.authenticatorId,
  });
  expect(credentials, "the credentials the virtual authenticator holds").toHaveLength(1);
  const [credential] = credentials;
  if (credential === undefined) throw new Error("the virtual authenticator holds no credential");
  return credential;
}

/** Puts `device`'s one credential back with `signCount`, as a copy of the key taken when its
 *  counter stood there would sign. */
async function setSignCount(device: VirtualPasskey, signCount: number): Promise<void> {
  const credential = await onlyCredential(device);
  await device.cdp.send("WebAuthn.removeCredential", {
    authenticatorId: device.authenticatorId,
    credentialId: credential.credentialId,
  });
  await device.cdp.send("WebAuthn.addCredential", {
    authenticatorId: device.authenticatorId,
    credential: { ...credential, signCount },
  });
}

/** PRIV's Access tab's row for `email`: the one whose Remove names them. */
function personRow(page: Page, email: string): Locator {
  return page
    .getByRole("list", { name: "People" })
    .getByRole("listitem")
    .filter({ has: page.getByRole("button", { name: `Remove ${email}` }) });
}

/** Presses Invite on `email`'s row of the Access tab alice has open and returns the link the row
 *  then shows. */
async function invite(page: Page, email: string): Promise<string> {
  const row = personRow(page, email);
  await row.getByRole("button", { name: `Invite ${email}` }).click();
  const link = row.getByRole("textbox", { name: `Enrollment link for ${email}` });
  await expect(link).toHaveValue(new RegExp(`^${outside}/enroll#[A-Za-z0-9_-]{43}$`));
  await expect(row.getByTestId("access-invitation-expiry")).toContainText("Works once, until");
  return link.inputValue();
}

/** Opens `link` in a new signed-out context and returns the alert its enrollment page shows. */
async function refusedLink(context: BrowserContext, link: string): Promise<Locator> {
  const page = await context.newPage();
  await open(page, link);
  await expect(page.getByRole("heading", { name: "Enroll a passkey" })).toBeVisible();
  await expect(page.getByRole("button", { name: "Create a passkey" })).toHaveCount(0);
  return page.getByRole("alert");
}

/** Signs the page's person out from the shell, accepting the question a sign-out asks. */
async function signOut(page: Page): Promise<void> {
  page.once("dialog", (dialog) => void dialog.accept());
  await page.getByRole("button", { name: "Sign out" }).click();
  await expect(page.getByRole("button", { name: "Sign in with a passkey" })).toBeVisible();
}

// The servers mount no dev sign-in route under google (e2e/harness-signin.ts), so the default
// context's sign-in has nothing to mint at; every context below signs in by itself.
test.use({ signedIn: false });

// One row whose steps run in order, rather than a row per step: e2e/test.ts empties the database
// before every row, and each step reads what the steps before it wrote.
test.describe("outside passkey sign-in", () => {
  test("a person an access list names enrolls a passkey through the owner's link and signs in with it at the outside origin", async ({
    browser,
    request,
  }) => {
    const aliceContext = await browser.newContext();
    const outsiderContext = await browser.newContext();
    const strangerContext = await browser.newContext();
    try {
      const alicePage = await aliceContext.newPage();
      const outsiderPage = await outsiderContext.newPage();
      const outsiderKey = await addVirtualPasskey(outsiderPage);

      await test.step("alice signs in at the canonical origin, makes PRIV and PUB, and lists the outsider, a second outsider and a colleague on PRIV", async () => {
        await aliceSignsIn(alicePage);
        await aliceWrites(aliceContext, "POST", "/api/v1/projects", {
          key: "PRIV",
          name: "Private work",
        });
        await aliceWrites(aliceContext, "POST", "/api/v1/projects", {
          key: "PUB",
          name: "Public work",
        });
        for (const email of [outsider, later, colleague]) {
          await aliceWrites(aliceContext, "PUT", `/api/v1/projects/PRIV/access/people/${email}`);
        }
      });

      const link =
        await test.step("alice presses Invite for the outsider on PRIV's Access tab, and the row shows the link and when it stops", async () => {
          await open(alicePage, `${canonical}/projects/PRIV/access`);
          return invite(alicePage, outsider);
        });

      await test.step("no link can be made for an address of the Workspace, or for one the list does not name", async () => {
        // The Access tab offers none: Dispatch names the people it would make a link for. The
        // row itself must be there, or a missing button would prove nothing.
        const row = personRow(alicePage, colleague);
        await expect(row).toHaveCount(1);
        await expect(row.getByRole("button", { name: `Invite ${colleague}` })).toHaveCount(0);
        // And the owner's own route refuses both.
        for (const [path, status, code] of [
          [
            `/api/v1/projects/PRIV/access/people/${colleague}/invitations`,
            400,
            "WORKSPACE_ADDRESS",
          ],
          [`/api/v1/projects/PUB/access/people/${outsider}/invitations`, 409, "PERSON_NOT_LISTED"],
        ] as const) {
          const answer = await aliceContext.request.post(`${canonical}${path}`, {
            headers: { Origin: canonical },
          });
          const body = await answer.text();
          expect(answer.status(), `POST ${path}: ${body}`).toBe(status);
          expect(JSON.parse(body), `POST ${path}`).toMatchObject({ code });
        }
      });

      await test.step("the outsider opens the link at the outside origin, enrolls a passkey, and is signed in, seeing PRIV alone", async () => {
        await open(outsiderPage, link);
        await expect(outsiderPage.getByRole("heading", { name: "Enroll a passkey" })).toBeVisible();
        await expect(
          outsiderPage.getByText(`The passkey signs you in to Dispatch as ${outsider}.`)
        ).toBeVisible();
        await outsiderPage.getByRole("button", { name: "Create a passkey" }).click();
        await expect(outsiderPage).toHaveURL(`${outside}/`);
        await expect(outsiderPage.getByText(`Signed in as ${outsider}`)).toBeVisible();
        expect(await whoami(outsiderPage), "the outsider's GET /auth/whoami").toMatchObject({
          body: { kind: "user", login: outsider, member_kind: "restricted" },
          status: 200,
        });
        await expect.poll(() => sidebarProjects(outsiderPage)).toEqual(["PRIV"]);
      });

      await test.step("the same link a second time is refused on the page", async () => {
        const alert = await refusedLink(strangerContext, link);
        await expect(alert).toHaveText(
          "This link has been used already. Ask the project's owner for a new one."
        );
      });

      await test.step("alice's Access tab shows when the outsider's passkey was enrolled", async () => {
        await reload(alicePage);
        await expect(personRow(alicePage, outsider).getByTestId("access-passkey")).toHaveCount(1);
        await expect(personRow(alicePage, outsider).getByTestId("access-passkey")).toContainText(
          "Passkey enrolled"
        );
        await expect(personRow(alicePage, later)).toHaveCount(1);
        await expect(personRow(alicePage, later).getByTestId("access-passkey")).toHaveCount(0);
        await expect(personRow(alicePage, later).getByText("No passkey yet")).toBeVisible();
      });

      await test.step("the outsider signs out, and signs in again with the passkey alone", async () => {
        await signOut(outsiderPage);
        await expect(outsiderPage.getByRole("link", { name: "Sign in with Google" })).toHaveCount(
          0
        );
        await outsiderPage.getByRole("button", { name: "Sign in with a passkey" }).click();
        await expect(outsiderPage.getByText(`Signed in as ${outsider}`)).toBeVisible();
        await expect.poll(() => sidebarProjects(outsiderPage)).toEqual(["PRIV"]);
      });

      await test.step("a passkey signing with a lower count than Dispatch stored is refused on the page", async () => {
        const stored = (await onlyCredential(outsiderKey)).signCount;
        expect(stored, "the sign count after an enrollment and a sign-in").toBeGreaterThan(1);
        await signOut(outsiderPage);
        await setSignCount(outsiderKey, 0);
        await outsiderPage.getByRole("button", { name: "Sign in with a passkey" }).click();
        await expect(outsiderPage.getByRole("alert")).toHaveText(
          "Dispatch refused this passkey: its sign-in count did not go up since its last use, which happens when a passkey has been copied. Ask a project's owner for a new link."
        );
        await expect(outsiderPage.getByText(`Signed in as ${outsider}`)).toHaveCount(0);
        // The real device again, its counter past the one Dispatch stored.
        await setSignCount(outsiderKey, stored + 10);
        await outsiderPage.getByRole("button", { name: "Sign in with a passkey" }).click();
        await expect(outsiderPage.getByText(`Signed in as ${outsider}`)).toBeVisible();
      });

      await test.step("a link past its 24 hours is refused on the page", async () => {
        const expiring = await invite(alicePage, later);
        // The link as if alice had made it 25 hours ago: both of its times move, since the table
        // holds every link to expire after it was made.
        await sql(
          `update outside_invitations set made_at = made_at - interval '25 hours', expires_at = expires_at - interval '25 hours' where email = ${sqlLiteral(later)} and used_at is null`
        );
        const alert = await refusedLink(strangerContext, expiring);
        await expect(alert).toHaveText(
          "This link has expired. Ask the project's owner for a new one."
        );
      });

      await test.step("a link for an address the list no longer names is refused on the page", async () => {
        const delisted = await invite(alicePage, later);
        // Taking the person off the list ends their unused links in the same transaction, so the
        // link is one Dispatch no longer holds.
        await aliceWrites(aliceContext, "DELETE", `/api/v1/projects/PRIV/access/people/${later}`);
        const alert = await refusedLink(strangerContext, delisted);
        await expect(alert).toHaveText(
          "Dispatch does not hold this link: the project's owner made a newer one, reset the address, or took it off the access list. Ask the project's owner for a new one."
        );
      });

      await test.step("the owner's Reset ends the outsider's sign-in and deletes the passkey, so it signs in no more", async () => {
        await reload(alicePage);
        const row = personRow(alicePage, outsider);
        alicePage.once("dialog", (dialog) => void dialog.accept());
        await row.getByRole("button", { name: `Reset ${outsider}` }).click();
        await expect(row.getByTestId("access-passkey")).toHaveCount(0);
        await expect(row.getByRole("button", { name: `Reset ${outsider}` })).toHaveCount(0);

        await reload(outsiderPage);
        await outsiderPage.getByRole("button", { name: "Sign in with a passkey" }).click();
        await expect(outsiderPage.getByRole("alert")).toHaveText(
          "Dispatch does not know this passkey: it was removed, so it signs in no more. Ask a project's owner for a new link."
        );
        expect((await whoami(outsiderPage)).status, "the outsider's GET /auth/whoami").toBe(401);
      });

      await test.step("each origin serves only its own sign-in", async () => {
        for (const path of ["/auth/start", "/auth/callback"]) {
          const answer = await request.get(`${outside}${path}`, { maxRedirects: 0 });
          expect(answer.status(), `GET ${outside}${path}`).toBe(404);
        }
        for (const path of [
          "/auth/passkey/register/begin",
          "/auth/passkey/register/finish",
          "/auth/passkey/login/begin",
          "/auth/passkey/login/finish",
        ]) {
          const answer = await request.post(`${canonical}${path}`, {
            data: {},
            headers: { Origin: canonical },
          });
          expect(answer.status(), `POST ${canonical}${path}`).toBe(404);
        }
        // At the outside origin, a ceremony step a page of another origin sends is refused before
        // it runs; the same step from the outside origin's own page is answered (the control).
        const crossOrigin = await request.post(`${outside}/auth/passkey/login/begin`, {
          data: {},
          headers: { Origin: canonical },
        });
        expect(crossOrigin.status(), "POST login/begin from the canonical origin's page").toBe(403);
        expect(await crossOrigin.json()).toEqual({ error: "invalid request origin" });
        const sameOrigin = await request.post(`${outside}/auth/passkey/login/begin`, {
          data: {},
          headers: { Origin: outside },
        });
        expect(sameOrigin.status(), "POST login/begin from the outside origin's page").toBe(200);
      });
    } finally {
      await Promise.all([aliceContext.close(), outsiderContext.close(), strangerContext.close()]);
    }
  });

  // The reset every row begins with (e2e/test.ts) empties the passkeys and the open ceremonies,
  // which no foreign key ties to a table the reset names: a passkey one row enrolled must not
  // sign in in the next. The counts before the reset are this row's control: a row that enrolled
  // nothing would pass the counts after it for no reason.
  test("the reset between rows leaves no passkey and no open ceremony, so a passkey enrolled before it signs in no more", async ({
    browser,
  }) => {
    const aliceContext = await browser.newContext();
    const outsiderContext = await browser.newContext();
    const count = async (table: "passkeys" | "passkey_ceremonies") =>
      Number((await sql(`select count(*) from ${table}`)).trim());
    try {
      const outsiderPage = await outsiderContext.newPage();
      await addVirtualPasskey(outsiderPage);
      await test.step("alice lists the outsider on PRIV and makes a link; the outsider enrolls with it", async () => {
        await aliceSignsIn(await aliceContext.newPage());
        await aliceWrites(aliceContext, "POST", "/api/v1/projects", {
          key: "PRIV",
          name: "Private work",
        });
        await aliceWrites(aliceContext, "PUT", `/api/v1/projects/PRIV/access/people/${outsider}`);
        const { url } = (await aliceWrites(
          aliceContext,
          "POST",
          `/api/v1/projects/PRIV/access/people/${outsider}/invitations`
        )) as { url: string };
        await open(outsiderPage, url);
        await outsiderPage.getByRole("button", { name: "Create a passkey" }).click();
        await expect(outsiderPage.getByText(`Signed in as ${outsider}`)).toBeVisible();
      });

      await test.step("a sign-in begun and never finished leaves an open ceremony; then the reset runs", async () => {
        const begun = await outsiderPage.evaluate(async () => {
          const response = await fetch("/auth/passkey/login/begin", {
            body: "{}",
            headers: { "Content-Type": "application/json" },
            method: "POST",
          });
          return response.status;
        });
        expect(begun, "POST login/begin from the outside origin's page").toBe(200);
        expect(await count("passkeys"), "passkeys before the reset").toBe(1);
        expect(
          await count("passkey_ceremonies"),
          "open ceremonies before the reset"
        ).toBeGreaterThan(0);
        await aliceContext.close();
        await resetDatabase();
        expect(await count("passkeys"), "passkeys after the reset").toBe(0);
        expect(await count("passkey_ceremonies"), "open ceremonies after the reset").toBe(0);
      });

      await test.step("the passkey enrolled before the reset signs in no more", async () => {
        await reload(outsiderPage);
        await outsiderPage.getByRole("button", { name: "Sign in with a passkey" }).click();
        await expect(outsiderPage.getByRole("alert")).toHaveText(
          "Dispatch does not know this passkey: it was removed, so it signs in no more. Ask a project's owner for a new link."
        );
      });
    } finally {
      await Promise.all([aliceContext.close(), outsiderContext.close()]);
    }
  });

  // Every row above starts with a Google sign-in through three redirect hops, which a loaded
  // machine can slow or drop (e2e/google-sign-in.ts). This row forces both on the chain's first
  // hop, Dispatch's /auth/start: Playwright routes the first request of a navigation and none of
  // the redirects that follow it, and it cannot fail a request with the network-change error
  // itself, so the row drops the hop with net::ERR_FAILED and names that error as one to repeat.
  // The issuer's own hop held up is the control run outside the suite, by stopping its process.
  test("a slow or dropped hop of the Google sign-in still signs alice in, or fails at once naming the hop", async ({
    browser,
  }) => {
    const dropped = "net::ERR_FAILED";
    /** A new signed-out page whose first /auth/start `first` answers, and every later one
     *  reaches Dispatch. */
    const pageWith = async (first: (route: Route) => Promise<void>): Promise<Page> => {
      const context = await browser.newContext();
      const page = await context.newPage();
      let routed = false;
      await page.route(`${canonical}/auth/start**`, async (route) => {
        if (routed) return route.continue();
        routed = true;
        await first(route);
      });
      return page;
    };
    const signedIn = async (page: Page, answered: Response) => {
      expect(answered.status(), `/auth/callback for ${alice}`).toBe(302);
      await expect(page.getByText(`Signed in as ${alice}`)).toBeVisible();
      await page.context().close();
    };

    await test.step("a hop held for 5 s signs alice in", async () => {
      const page = await pageWith(async (route) => {
        await new Promise((resolve) => setTimeout(resolve, 5_000));
        await route.continue();
      });
      await signedIn(
        page,
        await signInWithGoogle(page, canonical, alice, googleSignIn.domain, noteRepeat)
      );
    });

    await test.step("a hop dropped with an error the sign-in repeats is made again, says so, and signs alice in", async () => {
      const page = await pageWith((route) => route.abort("failed"));
      const repeats: string[] = [];
      const answered = await signInWithGoogle(
        page,
        canonical,
        alice,
        googleSignIn.domain,
        (note) => repeats.push(note),
        [dropped]
      );
      expect(repeats).toEqual([`Dispatch's /auth/start: ${dropped}, made again`]);
      await signedIn(page, answered);
    });

    await test.step("a hop dropped with any other error fails the sign-in at once, naming the hop; the unspent sign-in then completes by hand", async () => {
      const page = await pageWith((route) => route.abort("failed"));
      const began = Date.now();
      await expect(
        signInWithGoogle(page, canonical, alice, googleSignIn.domain, noteRepeat)
      ).rejects.toThrow(
        `the browser's request to Dispatch's /auth/start for ${alice} failed: ${dropped}`
      );
      // Named, not timed out: far inside the 45 s bound.
      expect(Date.now() - began, "ms from the press to the failure").toBeLessThan(15_000);
      // The dropped hop reached no server, so the person the issuer holds for alice is unspent;
      // the hop made by hand spends it and signs her in.
      const callback = page.waitForResponse(
        (response) => new URL(response.url()).pathname === "/auth/callback"
      );
      await goAgain(page, `${canonical}/auth/start`);
      await signedIn(page, await callback);
    });
  });
});
