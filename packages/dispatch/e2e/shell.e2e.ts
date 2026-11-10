import { setTimeout as sleep } from "node:timers/promises";

import { expect, test } from "@playwright/test";

import { EVENT_HEAD_RETRY_CEILING_MS } from "../web/src/api/query-client";
import { baseUrl, createIssue, createProject, sessionCookieName } from "./api";
import { holdFirstRequest } from "./editor";
import { resetDatabase } from "./seed";
import { failBuildScript, missEntryChunk, recordTabReloads, tabReloads } from "./shell";
import { asUser } from "./users";

/**
 * The application shell's rows that need the real stack, each with the reason it is not a
 * stubbed row above it. Everything else this file held - most of the chunk recovery, the
 * sidebar's own scroll and the gutter a route with no margin takes - is in
 * `shell.stub.e2e.ts`.
 *
 * Three of the reasons recur. A row whose failing asset must be answered 404 stays: the stubbed
 * layer's file server answers an unknown path with `index.html` (e2e/stub/static-server.ts,
 * the SPA fallback), so a script of a build the server never had loads as HTML there instead of
 * failing, and the recovery the row is about never starts. A row that fulfils `whoami` itself
 * and then wants the real answer stays: the recording holds the refusals the row made as well
 * as the answer, and `routeFromHAR` serves the first entry that matches a URL, so the refusal is
 * what every later ask is given. And a row whose claim is the server's own stays, as it always
 * did.
 */

interface NavigationEventTrace {
  download?: unknown;
  sameDocument?: unknown;
  type: string;
  url?: unknown;
}

test.beforeEach(async () => {
  await resetDatabase();
});

// It is no stubbed row: the row's own 503s are recorded beside the answer that follows them, and
// a replay is given the first of them for every ask, so the page never opens.
// @pr: Sign-in and tokens: a whoami that fails must leave the person in the app behind a Retry, never on the sign-in page.
test("a transient whoami failure shows a retry banner and keeps the app, not the sign-in page @pr", async ({
  browser,
}) => {
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  await page.route("**/auth/whoami", (route) =>
    route.fulfill({
      body: JSON.stringify({ error: "Dispatch is unavailable" }),
      contentType: "application/json",
      status: 503,
    })
  );

  await page.goto("/");

  await expect(page.getByText("Couldn't reach Dispatch.")).toBeVisible();
  await expect(page.getByRole("button", { name: "Retry" })).toBeVisible();
  await expect(page.getByRole("link", { name: "Sign in with Google" })).toHaveCount(0);

  await page.unroute("**/auth/whoami");
  await page.getByRole("button", { name: "Retry" }).click();

  await expect(page.getByText("Signed in as alice")).toBeVisible();

  await context.close();
});

// After a restart, whoami answers 503 EVENT_HEAD_UNAVAILABLE ("ask again") for as long as an event
// write of Dispatch's database open since before the server's boot holds the head back: two
// seconds a request, then the refusal. Under load such a write can stay open past the three tries
// a read gets, and a page that stops on "Couldn't reach Dispatch." then stays there until the
// person clicks Retry, though Dispatch answers again a moment later (driven against a real server:
// a write held 15 s across a restart left the page on that message 10 s after it committed). The
// page asks again while whoami answers that code, and opens once it answers.
//
// It is no stubbed row: the refusals end in `route.continue()`, which is a request to a server.
test("a page asks whoami again while it answers EVENT_HEAD_UNAVAILABLE, and opens once it answers", async ({
  browser,
}) => {
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  // The page asks again on a timer of its own - 500 ms doubling to the ceiling
  // (`web/src/api/query-client.ts`) - so the refusals are counted rather than timed out, and
  // each wait between two of them is spent on the page's clock. Five of them is past the two
  // retries any other failed read gets.
  const refusals = 5;
  let refused = 0;
  await page.route("**/auth/whoami", (route) => {
    if (refused >= refusals) {
      return route.continue();
    }
    refused++;
    return route.fulfill({
      body: JSON.stringify({
        code: "EVENT_HEAD_UNAVAILABLE",
        error: "the event log's head could not be read; ask again",
      }),
      contentType: "application/json",
      status: 503,
    });
  });

  await page.clock.install();
  await page.goto("/");
  for (let ask = 1; ask <= refusals; ask += 1) {
    await expect.poll(() => refused).toBeGreaterThanOrEqual(ask);
    await page.clock.fastForward(EVENT_HEAD_RETRY_CEILING_MS);
  }

  await expect(page.getByText("Signed in as alice")).toBeVisible();
  expect(refused).toBe(refusals);

  await context.close();
});

// It is no stubbed row: the claim is the server's, that logout advanced alice's session
// generation there.
// @pr: Sign-in and tokens: the sign-out revokes the session on the server, not only in the page.
test("signing out revokes the session on the server and returns to the sign-in page @pr", async ({
  browser,
}) => {
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await page.goto("/");
  await expect(page.getByText("Signed in as alice")).toBeVisible();
  const session = (await context.cookies()).find((cookie) => cookie.name === sessionCookieName);
  if (session === undefined) {
    throw new Error(`no ${sessionCookieName} cookie after sign-in`);
  }
  const whoami = () =>
    fetch(new URL("/auth/whoami", baseUrl), {
      headers: { Cookie: `${sessionCookieName}=${session.value}` },
    });
  // The copy authenticates on its own before Sign out, so its refusal afterwards is the logout's.
  expect((await whoami()).status).toBe(200);

  await page.getByRole("button", { name: "Sign out" }).click();
  await expect(page.getByRole("link", { name: "Sign in with Google" })).toBeVisible();

  // /auth/logout clears the browser's cookie, which alone would make the next whoami 401. The
  // check that cannot pass for that reason: a copy of the cookie the browser held, replayed after
  // Sign out, is refused because logout advanced alice's session generation on the server.
  expect((await whoami()).status).toBe(401);
  await context.close();
});

// A Download link (`<a download>`) starts a navigation the browser answers with a download, so the
// page stays the reader's. The Navigation API's `navigate` event fires for it with `downloadRequest`
// set and `destination.sameDocument` false, and a chunk that fails after it must still reload the
// page, as on any page nobody is leaving.
//
// It is no stubbed row: the `firefox-download-navigation` project selects it by this file's name
// (e2e/playwright.config.ts), and the engine it covers is the point of the row.
// @pr: Upload and download, and the nearest the lane has to the rolling-release walk: a chunk that fails after a Download link was followed reloads the page.
test("a chunk that fails after a Download link was followed reloads the page @pr", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name !== "chromium" && testInfo.project.name !== "firefox-download-navigation",
    "the guard is checked in Chromium and Firefox"
  );
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", spec: "Downloadable spec.", title: "Files" });
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  // The spec's document transport is still downloading when the reader opens the Artifacts tab
  // and follows a Download link; the chunk fails only after that.
  const transportCode = await holdFirstRequest(page, /\/assets\/yjs-[^/]+\.js$/u);
  await page.addInitScript(() => {
    const seen: Array<{
      download?: unknown;
      sameDocument?: unknown;
      type: string;
      url?: unknown;
    }> = [];
    Object.assign(window, { seenNavigations: seen });
    const navigation = "navigation" in window ? window.navigation : undefined;
    if (navigation instanceof EventTarget) {
      navigation.addEventListener("navigate", (event) => {
        const destination = "destination" in event ? event.destination : undefined;
        seen.push({
          download: "downloadRequest" in event ? event.downloadRequest : undefined,
          sameDocument:
            typeof destination === "object" && destination !== null && "sameDocument" in destination
              ? destination.sameDocument
              : undefined,
          type: "navigate",
          url:
            typeof destination === "object" && destination !== null && "url" in destination
              ? destination.url
              : undefined,
        });
      });
    }
    window.addEventListener("pageshow", () => seen.push({ type: "pageshow" }));
    document.addEventListener("visibilitychange", () =>
      seen.push({ type: `visibilitychange:${document.visibilityState}` })
    );
  });
  let loads = 0;
  page.on("load", () => {
    loads += 1;
  });

  try {
    await page.goto(`/issues/${issue.key}/spec`);
    const held = await transportCode.held;
    await page.getByRole("tab", { name: /^Artifacts/ }).click();
    const download = page.waitForEvent("download");
    await page.getByRole("link", { name: "Download version 1" }).click();
    await download;
    // The browser announced the download as a navigation to another document: the case the
    // guard has to tell apart from a page being left.
    const events = await page.evaluate<NavigationEventTrace[] | undefined>(() => {
      const eventWindow = window as Window & { seenNavigations?: NavigationEventTrace[] };
      return eventWindow.seenNavigations;
    });
    expect(events).toBeDefined();
    const downloadNavigation = events?.findIndex(
      (event) =>
        event.type === "navigate" && event.download !== null && event.download !== undefined
    );
    expect(downloadNavigation).toBeGreaterThanOrEqual(0);
    expect(events?.[downloadNavigation ?? -1]).toMatchObject({
      download: expect.any(String),
      sameDocument: false,
      type: "navigate",
    });
    if (testInfo.project.name === "firefox-download-navigation") {
      console.log(`firefox event order: ${JSON.stringify(events)}`);
      const downloadEvent = events?.[downloadNavigation ?? -1];
      const followOnNavigation = events?.findIndex(
        (event, index) =>
          index > (downloadNavigation ?? -1) &&
          event.type === "navigate" &&
          event.download === null &&
          event.sameDocument === false &&
          event.url === downloadEvent?.url
      );
      expect(followOnNavigation).toBeGreaterThan(downloadNavigation ?? -1);
      expect(
        events
          ?.slice((downloadNavigation ?? -1) + 1, followOnNavigation)
          .some((event) => event.type === "pageshow" || event.type === "visibilitychange:visible")
      ).toBe(false);
    }
    expect(loads).toBe(1);

    await held.abort();
    await expect.poll(() => loads).toBe(2);
  } finally {
    await context.close();
  }
});

// The budget renews: a reload counts against the tab for five minutes, so a tab that spent all
// three long ago recovers from the next deployment. This tab also holds the once-only key an
// earlier build's page spends in the same tab, which this build neither reads nor writes.
//
// It is no stubbed row: the new failure is a script of this build the server never had, which
// the stubbed layer's file server answers with `index.html` rather than a 404.
test("a tab whose three reloads are older than five minutes reloads for a new failure", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "the recovery does not depend on the layout");
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const counts = await missEntryChunk(page, () => false, baseUrl);

  try {
    await page.goto("/");
    await expect(page.getByText("Signed in as alice")).toBeVisible();
    const spent = await recordTabReloads(page, [8, 7, 6]);
    await page.evaluate(() => window.sessionStorage.setItem("dispatch.reloaded-for-chunk", "true"));

    await failBuildScript(page);
    await expect.poll(() => counts.documents).toBe(2);
    await expect(page.getByText("Signed in as alice")).toBeVisible();
    // The three old reloads have left the window; the new one is all the tab now holds.
    const reloads = await tabReloads(page);
    expect(reloads).toHaveLength(1);
    expect(reloads?.[0]).toBeGreaterThan(Math.max(...spent));
  } finally {
    await context.close();
  }
});

// Three reloads in the last five minutes spend the tab's budget, whatever the failure: a page
// whose assets keep failing stops reloading rather than reloading for ever.
//
// It is no stubbed row, for the reason above: the failure the budget is held against must be a
// real 404.
test("a tab that reloaded three times in the last five minutes does not reload for a new failure", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "the recovery does not depend on the layout");
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const counts = await missEntryChunk(page, () => false, baseUrl);

  try {
    await page.goto("/");
    await expect(page.getByText("Signed in as alice")).toBeVisible();
    const spent = await recordTabReloads(page, [3, 2, 1]);

    const failed = page.waitForResponse((response) =>
      response.url().endsWith("/assets/index-missing.js")
    );
    await failBuildScript(page);
    // The handler runs on the failure itself, so a reload it decided to make would already have
    // asked for the page; four frames of margin for the request to reach the counter.
    await failed;
    await sleep(250);
    expect(counts).toEqual({ documents: 1, entryRequests: 1 });
    expect(await tabReloads(page)).toEqual(spent);
  } finally {
    await context.close();
  }
});

// WebKit and Firefox cancel the downloads still in flight when the reader navigates away, and a
// cancelled entry chunk fails like a missing one; a reload then would replace the reader's
// navigation. The page counts as left from `beforeunload`, or from `pagehide` in a browser that
// fires no `beforeunload` (iOS Safari), until it is shown again.
//
// They are no stubbed rows: each ends on a script of this build the server never had, which the
// stubbed layer's file server answers with `index.html` rather than a 404.
for (const leaving of ["beforeunload", "pagehide"]) {
  test(`a page whose entry chunk fails after ${leaving} stays until it is shown again`, async ({
    browser,
  }, testInfo) => {
    test.skip(testInfo.project.name !== "chromium", "the recovery does not depend on the layout");
    const context = await asUser(browser, "alice");
    const page = await context.newPage();
    const requested = Promise.withResolvers<void>();
    const released = Promise.withResolvers<void>();
    const counts = await missEntryChunk(
      page,
      async (request) => {
        if (request > 1) {
          return false;
        }
        requested.resolve();
        await released.promise;
        return true;
      },
      baseUrl
    );

    try {
      await page.goto("/", { waitUntil: "commit" });
      await requested.promise;
      await page.evaluate((type) => window.dispatchEvent(new Event(type)), leaving);
      // The 404 the release lets through is what the page's handler sees; waiting for it puts
      // the margin after the failure rather than around it.
      const missed = page.waitForResponse((response) => response.status() === 404);
      released.resolve();
      await missed;
      // Four frames of margin for a reload the miss could have started.
      await sleep(250);
      expect(counts).toEqual({ documents: 1, entryRequests: 1 });

      // Shown again (a back/forward-cache restore fires `pageshow`), the page's next failed build
      // script reloads it.
      await page.evaluate(() => {
        window.dispatchEvent(new Event("pageshow"));
        const script = document.createElement("script");
        script.src = "/assets/index-missing.js";
        document.head.append(script);
      });
      await expect(page.getByText("Signed in as alice")).toBeVisible();
      expect(counts).toEqual({ documents: 2, entryRequests: 2 });
    } finally {
      await context.close();
    }
  });
}

// It is no stubbed row: the editor draws from the Yjs socket, which no recording answers.
test("the document editor renders headings and ordered lists with real typography", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "# Title\n\nSome body text.\n\n## Section\n\n### Detail\n\n1. First\n2. Second\n",
    title: "Spec typography",
  });

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await page.goto(`/issues/${issue.key}`);
  await page.getByRole("tab", { name: "Spec" }).click();

  await expect(page.getByRole("heading", { level: 1, name: "Title" })).toBeVisible();
  await expect(page.getByRole("heading", { level: 2, name: "Section" })).toBeVisible();

  const sizes = await page.evaluate(() => {
    const root = document.querySelector('[role="textbox"][aria-label="Document editor"]');
    const h1 = root?.querySelector("h1");
    const h2 = root?.querySelector("h2");
    const paragraph = root?.querySelector("p");
    const list = root?.querySelector("ol");
    return {
      h1: h1 ? Number.parseFloat(getComputedStyle(h1).fontSize) : 0,
      h2: h2 ? Number.parseFloat(getComputedStyle(h2).fontSize) : 0,
      listStyleType: list ? getComputedStyle(list).listStyleType : "",
      paragraph: paragraph ? Number.parseFloat(getComputedStyle(paragraph).fontSize) : 0,
    };
  });

  expect(sizes.h1).toBeGreaterThan(sizes.paragraph);
  expect(sizes.h2).toBeGreaterThan(sizes.paragraph);
  expect(sizes.h1).toBeGreaterThan(sizes.h2);
  expect(sizes.listStyleType).not.toBe("none");

  await context.close();
});
