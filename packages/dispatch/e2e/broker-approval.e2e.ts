import { createServer, type Server } from "node:http";

import { setPendingCredentialRequests } from "./fake-broker-helpers";
import { usesFakeBroker } from "./harness-broker";
import {
  aliceBrokerLogin,
  aliceSecret,
  bobBrokerLogin,
  devIssuerOrigin,
  queueSignIn,
  realBrokerOrigin,
  type SeededRequest,
  seededRequestStates,
  seedPendingRequests,
  withdrawSeededRequests,
} from "./real-broker";
import { type BrowserContext, expect, type Page, type Response, test } from "./test";
import { asUser } from "./users";

// A secret request is approved on the secrets broker's own page, by a person signed in to the
// broker, and Dispatch's Inbox only links there. These rows cross that boundary for real: each
// request is made on the REAL broker (e2e/run-broker.sh) by pods the seeding step enrolls through
// the dev issuer's tokens, and the fake broker Dispatch's server relays lists it for alice with
// the real record's `approve_url`, so a click on the Inbox row opens the real broker's page in a
// new tab, whose sign-in goes to the dev issuer, while the Inbox stays where it was. The broker's
// cookies are `__Host-` and Secure on plain http://127.0.0.1, which Chromium keeps because it
// treats loopback as a secure context; Dispatch and the broker share that host, so the two keep
// their cookies under different names in one jar.

/** The broker's session cookie, and both its cookies, which a lapsed session has lost. */
const sessionCookie = "__Host-broker-session";
const brokerCookies = /^__Host-broker-/;
/** What the broker answers a POST from any origin but its own, having written nothing. */
const originRefusal =
  "This request did not come from the secrets broker's own page, so nothing was changed.";

/** Skips a row this run has no real broker for: the harness starts it beside the fake broker
 *  only (e2e/harness-listeners.ts). Every row of this file runs in `chromium` and in no other
 *  project, since none of the tagged projects selects it. */
function requireRealBroker(): void {
  test.skip(!usesFakeBroker, "the harness starts the real broker beside the fake broker only");
}

/** Lists `requests` in alice's Inbox as the broker lists them, each linking to its real page. */
async function listForAlice(requests: readonly SeededRequest[]): Promise<void> {
  await setPendingCredentialRequests(
    requests.map((request) => ({
      approve_url: request.approve_url,
      approver: "alice",
      identifiers: [aliceSecret],
      kind: "agent_secret",
      record_id: request.record_id,
      requested_at: new Date().toISOString(),
      session: { enrollment: null, request: null },
    }))
  );
}

/** The Inbox row of the request whose page is `approveUrl`. */
function inboxRow(page: Page, approveUrl: string) {
  return page
    .getByRole("region", { name: "Credential requests" })
    .locator(`a[href="${approveUrl}"]`);
}

/** Clicks the Inbox row of the request whose page is `approveUrl` and returns the tab it opens.
 *  The Inbox tab stays at the address it had. */
async function openFromInbox(page: Page, approveUrl: string): Promise<Page> {
  const inbox = page.url();
  const [broker] = await Promise.all([
    page.context().waitForEvent("page"),
    inboxRow(page, approveUrl).click(),
  ]);
  await expect(broker).toHaveURL(approveUrl);
  expect(page.url()).toBe(inbox);
  return broker;
}

/** The broker page's block for one record, and its Approve button. */
function recordOn(page: Page, request: SeededRequest) {
  const record = page.locator(`#record-${request.record_id}`);
  return { approve: record.getByRole("button", { exact: true, name: "Approve" }), record };
}

/** Every URL a page of `context` requests from now on, and every page it navigates to: none of
 *  these pages draws a frame, and a new tab's first navigations have no frame to compare. */
function recordTraffic(context: BrowserContext): { requests: string[]; documents: string[] } {
  const requests: string[] = [];
  const documents: string[] = [];
  context.on("request", (request) => {
    requests.push(request.url());
    if (request.isNavigationRequest()) {
      documents.push(request.url());
    }
  });
  return { documents, requests };
}

/** The broker page's header line naming who is signed in. */
function signedInAs(page: Page, login: string) {
  return page.getByRole("banner").getByText(`Signed in as ${login}.`);
}

/** The broker's session cookie in `context`, read from every cookie the context holds, since
 *  Playwright's `cookies(url)` drops a Secure cookie for any http URL but localhost's. */
async function brokerSession(context: BrowserContext) {
  const host = new URL(realBrokerOrigin()).hostname;
  return (await context.cookies()).find((c) => c.domain === host && c.name === sessionCookie);
}

/** Signs `page`'s browser in to the broker as alice through `request`'s page: no session sends it
 *  to the issuer and back there. The broker's sign-in for a browser with no session names nobody
 *  (it reads no record first), so the issuer is told to sign alice in. */
async function signInToBroker(page: Page, request: SeededRequest): Promise<void> {
  await queueSignIn(aliceBrokerLogin);
  await page.goto(request.approve_url);
  await expect(page).toHaveURL(request.approve_url);
  await expect(signedInAs(page, aliceBrokerLogin)).toBeVisible();
}

/** Each seeded request's state, read through the broker's API by the pod that made it, saying so
 *  when the request holds a grant. */
async function states(state: string): Promise<string[]> {
  return (await seededRequestStates(state)).map((request) =>
    request.grant_id === null ? request.state : `${request.state} with a grant`
  );
}

// @pr: Sign-in and tokens: a live broker session reaches the record's page with no sign-in, and one Approve grants the credential.
test("from the Inbox, a live broker session lands on the record's page with no sign-in, and one Approve grants it @pr", async ({
  browser,
}, testInfo) => {
  requireRealBroker();
  const state = testInfo.outputPath("broker-requests.json");
  const [request] = await seedPendingRequests(state, aliceSecret);
  if (request === undefined) throw new Error("the seeding step made no request");
  let alice: BrowserContext | undefined;
  try {
    await listForAlice([request]);
    alice = await asUser(browser, "alice");
    const page = await alice.newPage();
    await signInToBroker(page, request);
    // Chromium kept the session cookie the broker set over plain http on loopback.
    expect(await brokerSession(alice)).toMatchObject({
      httpOnly: true,
      path: "/",
      sameSite: "Lax",
      secure: true,
    });

    await page.goto("/");
    await expect(inboxRow(page, request.approve_url)).toContainText(aliceSecret);
    const traffic = recordTraffic(alice);
    const broker = await openFromInbox(page, request.approve_url);
    const { approve, record } = recordOn(broker, request);
    await expect(approve).toBeVisible();
    await expect(record).toContainText("a pod enrollment");
    expect(traffic.documents).toEqual([request.approve_url]);
    expect(traffic.requests.filter((url) => url.startsWith(devIssuerOrigin()))).toEqual([]);

    await approve.click();
    await expect(broker.locator(".outcomes")).toContainText(
      `${aliceSecret}: granted (approved by ${aliceBrokerLogin} at`
    );
    expect(await states(state)).toEqual(["granted with a grant"]);
  } finally {
    await alice?.close();
    await withdrawSeededRequests(state);
  }
});

// @pr: Sign-in and tokens: a lapsed session signs in once and comes back to the same record's page.
test("from the Inbox, a lapsed broker session signs in once and returns to the same record's page @pr", async ({
  browser,
}, testInfo) => {
  requireRealBroker();
  const state = testInfo.outputPath("broker-requests.json");
  const [request] = await seedPendingRequests(state, aliceSecret);
  if (request === undefined) throw new Error("the seeding step made no request");
  let alice: BrowserContext | undefined;
  try {
    await listForAlice([request]);
    alice = await asUser(browser, "alice");
    const page = await alice.newPage();
    await signInToBroker(page, request);
    expect(await brokerSession(alice)).toBeDefined();
    await alice.clearCookies({ name: brokerCookies });
    expect(await brokerSession(alice)).toBeUndefined();

    await page.goto("/");
    await queueSignIn(aliceBrokerLogin);
    const traffic = recordTraffic(alice);
    const broker = await openFromInbox(page, request.approve_url);
    const { approve } = recordOn(broker, request);
    await expect(approve).toBeVisible();
    await expect(signedInAs(broker, aliceBrokerLogin)).toBeVisible();
    // One pass: the record's page, the issuer's sign-in, the broker's callback, the page again.
    const signIns = traffic.documents.filter((url) =>
      url.startsWith(`${devIssuerOrigin()}/authorize?`)
    );
    expect(signIns).toHaveLength(1);
    expect(traffic.documents.map((url) => new URL(url).origin + new URL(url).pathname)).toEqual([
      request.approve_url,
      `${devIssuerOrigin()}/authorize`,
      `${realBrokerOrigin()}/signin/callback`,
      request.approve_url,
    ]);
    // A browser with no session is sent to sign in naming nobody: the page reads no record first.
    expect(new URL(signIns[0] ?? "").searchParams.get("login_hint")).toBeNull();
    expect(await states(state)).toEqual(["pending"]);
  } finally {
    await alice?.close();
    await withdrawSeededRequests(state);
  }
});

test("three pending requests are approved by one click on Approve all 3 shown", async ({
  browser,
}, testInfo) => {
  requireRealBroker();
  const state = testInfo.outputPath("broker-requests.json");
  const requests = await seedPendingRequests(state, aliceSecret, 3);
  const [first] = requests;
  if (first === undefined || requests.length !== 3) {
    throw new Error(`the seeding step made ${requests.length} requests, not 3`);
  }
  let alice: BrowserContext | undefined;
  try {
    await listForAlice(requests);
    alice = await asUser(browser, "alice");
    const page = await alice.newPage();
    await page.goto("/");
    // A browser with no session is sent to sign in naming nobody, so the issuer is told who.
    await queueSignIn(aliceBrokerLogin);
    const broker = await openFromInbox(page, first.approve_url);
    for (const request of requests) {
      await expect(recordOn(broker, request).approve).toBeVisible();
    }

    await broker.getByRole("button", { exact: true, name: "Approve all 3 shown" }).click();
    const outcomes = broker.locator(".outcomes li");
    await expect(outcomes).toHaveCount(3);
    // The count of three above waited for every outcome line, so one read lists them all.
    for (const outcome of await outcomes.all()) {
      await expect(outcome).toContainText(
        `${aliceSecret}: granted (approved by ${aliceBrokerLogin} at`
      );
    }
    expect(await states(state)).toEqual([
      "granted with a grant",
      "granted with a grant",
      "granted with a grant",
    ]);
  } finally {
    await alice?.close();
    await withdrawSeededRequests(state);
  }
});

/** The form a sibling page would post to decide `request`'s record: what the broker's own page
 *  posts, with a token it cannot compute (the session secret is in an HttpOnly cookie of another
 *  origin). */
function forgedDecision(request: SeededRequest): Record<string, string> {
  return {
    decision: "approve",
    expires: String(Math.floor(Date.now() / 1000) + 3600),
    printed: request.record_id,
    record: request.record_id,
    token: "0".repeat(64),
  };
}

/** A page on another 127.0.0.1 port than the broker's: the same site, another origin, as Dispatch
 *  is to the broker in production. Each path serves one attempt on `request`'s record. */
async function hostileSibling(request: SeededRequest): Promise<{ origin: string; server: Server }> {
  const inputs = Object.entries(forgedDecision(request))
    .map(([name, value]) => `<input type="hidden" name="${name}" value="${value}">`)
    .join("");
  const pages: Record<string, string> = {
    "/form": `<form method="post" action="${realBrokerOrigin()}/approve">${inputs}<button>Win a prize</button></form>`,
    "/frame": `<iframe title="the broker" src="${request.approve_url}" width="600" height="400"></iframe>`,
    "/script": "<p>A page that sends a credentialed fetch.</p>",
  };
  const server = createServer((incoming, answer) => {
    const body = pages[incoming.url ?? ""];
    answer.writeHead(body === undefined ? 404 : 200, { "Content-Type": "text/html" });
    answer.end(`<!doctype html><title>sibling</title>${body ?? ""}`);
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const address = server.address();
  if (address === null || typeof address === "string") {
    throw new Error(`the sibling page listens on no TCP port: ${String(address)}`);
  }
  return { origin: `http://127.0.0.1:${address.port}`, server };
}

// @pr: Sign-in and tokens: the approval route must refuse a sibling origin's form, credentialed fetch and frame.
test("a page on a sibling origin cannot decide the record with a form, a credentialed fetch, or a frame @pr", async ({
  browser,
}, testInfo) => {
  requireRealBroker();
  const state = testInfo.outputPath("broker-requests.json");
  const [request] = await seedPendingRequests(state, aliceSecret);
  if (request === undefined) throw new Error("the seeding step made no request");
  const { origin, server } = await hostileSibling(request);
  let alice: BrowserContext | undefined;
  try {
    alice = await asUser(browser, "alice");
    const page = await alice.newPage();
    await signInToBroker(page, request);
    const decide = (response: Response) =>
      response.url() === `${realBrokerOrigin()}/approve` && response.request().method() === "POST";
    expect(await brokerSession(alice)).toBeDefined();

    // (a) A form posted to the broker carries alice's session (same site, so SameSite=Lax sends
    // it) and is refused for its origin.
    await page.goto(`${origin}/form`);
    const [formAnswer] = await Promise.all([
      page.waitForResponse(decide),
      page.getByRole("button", { name: "Win a prize" }).click(),
    ]);
    expect(formAnswer.status()).toBe(403);
    expect(await formAnswer.request().headerValue("origin")).toBe(origin);
    expect(await formAnswer.request().headerValue("cookie")).toContain(`${sessionCookie}=`);
    await expect(page.getByRole("heading", { name: "Refused" })).toBeVisible();
    await expect(page.getByText(originRefusal)).toBeVisible();
    expect(await states(state)).toEqual(["pending"]);

    // (b) A credentialed fetch is refused. Read as CORS, it fails outright: the broker sends no
    // CORS header, so Chromium fails the fetch, and reports the request failed rather than
    // answered. Sent no-cors, as a page that needs no answer would, it is answered opaque to the
    // script, and the broker refused it (403, the origin check running before any other).
    await page.goto(`${origin}/script`);
    const sendFetch = (mode: "cors" | "no-cors") =>
      page.evaluate(
        async ({ body, mode, url }) => {
          try {
            const answer = await fetch(url, {
              body: new URLSearchParams(body),
              credentials: "include",
              method: "POST",
              mode,
            });
            return `answered ${answer.type} ${answer.status}`;
          } catch (error) {
            return `failed: ${String(error)}`;
          }
        },
        { body: forgedDecision(request), mode, url: `${realBrokerOrigin()}/approve` }
      );
    expect(await sendFetch("cors")).toBe("failed: TypeError: Failed to fetch");
    expect(await states(state)).toEqual(["pending"]);
    const [fetchAnswer, outcome] = await Promise.all([
      page.waitForResponse(decide),
      sendFetch("no-cors"),
    ]);
    expect(outcome).toBe("answered opaque 0");
    expect(fetchAnswer.status()).toBe(403);
    expect(await fetchAnswer.headerValue("access-control-allow-origin")).toBeNull();
    expect(await states(state)).toEqual(["pending"]);

    // (c) A frame of the record's page is refused by its frame-ancestors 'none': the broker answers
    // the page, and the browser renders none of it.
    const [frameAnswer] = await Promise.all([
      page.waitForResponse((response) => response.url() === request.approve_url),
      page.goto(`${origin}/frame`),
    ]);
    expect(frameAnswer.status()).toBe(200);
    expect(await frameAnswer.headerValue("content-security-policy")).toContain(
      "frame-ancestors 'none'"
    );
    await page.waitForLoadState("load");
    const frame = page.frameLocator("iframe");
    await expect(frame.getByText("Signed in as")).toHaveCount(0);
    await expect(frame.getByRole("button", { name: "Approve" })).toHaveCount(0);
    expect(page.frames().map((f) => f.url())).not.toContain(request.approve_url);
    expect(await states(state)).toEqual(["pending"]);
  } finally {
    await alice?.close();
    server.close();
    await withdrawSeededRequests(state);
  }
});

// @pr: Sign-in and tokens: signed in as the wrong person, the record's page offers only the switch, which signs in its approver.
test("signed in as another person, the record's page offers only the switch, which signs in its approver @pr", async ({
  browser,
}, testInfo) => {
  requireRealBroker();
  const state = testInfo.outputPath("broker-requests.json");
  const [request] = await seedPendingRequests(state, aliceSecret);
  if (request === undefined) throw new Error("the seeding step made no request");
  let alice: BrowserContext | undefined;
  try {
    await listForAlice([request]);
    alice = await asUser(browser, "alice");
    const page = await alice.newPage();
    // The issuer signs Bob in at the first sign-in; only the later account switch names Alice.
    await queueSignIn(bobBrokerLogin);
    await page.goto("/");
    const broker = await openFromInbox(page, request.approve_url);
    await expect(signedInAs(broker, bobBrokerLogin)).toBeVisible();
    const { approve, record } = recordOn(broker, request);
    await expect(record).toContainText(`Only ${aliceBrokerLogin} may decide this request`);
    await expect(broker.getByRole("button", { name: /^Approve/ })).toHaveCount(0);
    await expect(broker.getByRole("button", { name: "Deny" })).toHaveCount(0);

    const traffic = recordTraffic(alice);
    await record.getByRole("link", { exact: true, name: "Sign in as someone else" }).click();
    await expect(broker).toHaveURL(request.approve_url);
    await expect(signedInAs(broker, aliceBrokerLogin)).toBeVisible();
    await expect(approve).toBeVisible();
    const signIn = traffic.documents.find((url) =>
      url.startsWith(`${devIssuerOrigin()}/authorize?`)
    );
    const signInQuery = new URL(signIn ?? devIssuerOrigin()).searchParams;
    expect(signInQuery.get("login_hint")).toBe(aliceBrokerLogin);
    expect(signInQuery.get("prompt")).toBe("select_account");
    expect(await states(state)).toEqual(["pending"]);
  } finally {
    await alice?.close();
    await withdrawSeededRequests(state);
  }
});
