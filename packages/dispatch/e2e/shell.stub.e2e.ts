/** What the application shell draws when its own assets fail to download, and what it reserves
 *  for the rails beside a route.
 *
 *  Every row here is about the page: a chunk the browser could not download, the viewer read the
 *  row itself refuses, a sidebar taller than the viewport, the gutter a route with no margin
 *  takes. None of it is a claim about what the server computes, so each runs against a recording
 *  rather than against a harness (packages/dispatch/AGENTS.md, "The stubbed layer"). Three of
 *  them run with no recording at all, and take no `scene`: their page never reaches the API,
 *  because the row answers every read the page makes for its viewer, or the entry chunk never
 *  loads.
 *
 *  What stayed in `shell.e2e.ts`, and why, is written there.
 */
import { setTimeout as sleep } from "node:timers/promises";

import { expect, type Route } from "@playwright/test";

import { QUERY_RETRY_DELAY_MS } from "../web/src/api/query-client";
import { createIssue, createProject } from "./api";
import {
  failAssetWhen,
  holdAsset,
  missEntryChunk,
  pageAssetPath,
  shellLoads,
  tabReloads,
} from "./shell";
import { stubbed } from "./stub/stub-test";

const test = stubbed(async () => {
  await createProject({ key: "CORE", name: "Core" });
  const unreachable = await createIssue({ project: "CORE", title: "Unreachable page" });
  const withMargin = await createIssue({ project: "CORE", title: "Has a margin" });
  // Enough projects that the navigation is taller than any viewport under test.
  for (let index = 0; index < 17; index += 1) {
    const key = `P${String(index).padStart(2, "0")}`;
    await createProject({ key, name: `Project ${key}` });
  }
  return {
    lastProject: "P16",
    person: "alice",
    unreachable: unreachable.key,
    withMargin: withMargin.key,
  };
});

// whoami answers 503 EVENT_HEAD_READ_FAILED when it cannot read the event log's head at all: the
// database or its pool does not answer. Unlike EVENT_HEAD_UNAVAILABLE that need not end on its own,
// so the page asks again only as any read does, and then shows that it could not reach Dispatch,
// with Retry, rather than loading for ever.
//
// The read the page makes for its viewer is the landing (`web/src/api/queries.ts`,
// `readLanding`), which answers that same 503 under that same code; `/auth/whoami` is what it
// falls back to for every other failure. So both are refused here, and both are counted. A row
// that refused the fallback alone would leave the landing to whatever stands behind it: in a
// recording run that is the real harness, which answers a page that has not signed in with 401
// and draws the sign-in page, so the row would say nothing about the page a reader meets.
//
// It takes no `scene`: every read of the viewer is this row's own refusal, the app never opens,
// and so no read of it reaches the API. There is nothing for a recording to answer.
test("a page stops on Couldn't reach Dispatch. while whoami answers EVENT_HEAD_READ_FAILED @pr", async ({
  page,
}) => {
  let refused = 0;
  const refuse = (route: Route) => {
    refused++;
    return route.fulfill({
      body: JSON.stringify({
        code: "EVENT_HEAD_READ_FAILED",
        error: "the event log's head could not be read",
      }),
      contentType: "application/json",
      status: 503,
    });
  };
  await page.route("**/auth/whoami", refuse);
  await page.route(/\/api\/v1\/landing(\?|$)/u, refuse);

  await page.clock.install();
  await page.goto("/");
  // The two retries this code gets (`queryRetry`, `web/src/api/query-client.ts`) wait
  // `QUERY_RETRY_DELAY_MS` each: page timers, fired here rather than waited out.
  for (let ask = 1; ask <= 3; ask += 1) {
    await expect.poll(() => refused).toBeGreaterThanOrEqual(ask);
    await page.clock.fastForward(QUERY_RETRY_DELAY_MS);
  }
  await expect(page.getByText("Couldn't reach Dispatch.")).toBeVisible();
  await expect(page.getByRole("button", { name: "Retry" })).toBeVisible();
  const shown = refused;
  expect(shown).toBeLessThanOrEqual(3);
  // A minute of the page's own time, in which a retry tier the policy does not have would fire;
  // the margin is for such a request reaching the route above.
  await page.clock.fastForward(60_000);
  // The margin that paragraph names: a fourth ask would leave the page inside the jump above,
  // and this window in Node is for it to reach the route that counts it; an ask the policy never
  // makes marks nothing else to wait on, so the window is the check.
  await sleep(100);
  expect(refused).toBe(shown);
});

test("a page chunk that fails on every load reloads three times and then shows the failed download @pr", async ({
  page,
  scene,
}) => {
  // Every download of the issue page's code fails, the way a request does when the network
  // changes under it (a VPN reconnect, a Wi-Fi switch).
  await page.route(/\/assets\/IssuePage-[^/]+\.js$/u, (route) => route.abort());
  let loads = 0;
  page.on("load", () => {
    loads += 1;
  });

  await page.goto(`/issues/${scene.unreachable}`);
  // Each of the first three failures reloads the page; the fourth, with the tab's three reloads
  // of the last five minutes spent, stays.
  await expect.poll(() => loads).toBe(4);
  const box = page.getByTestId("error-boundary");
  await expect(box).toContainText("Failed to fetch dynamically imported module");
  await expect(box).not.toContainText("Cannot read properties of undefined");
  await expect(box.getByRole("button", { name: "Reload Dispatch" })).toBeVisible();
});

// During a rolling deploy the load balancer can send a page's HTML to one server and its entry
// chunk to another that never built it. Nothing of the app has run, so only the page itself can
// recover: it reloads, and the next load asks for both again.
test("a page whose entry chunk is missing once reloads and renders the app @pr", async ({
  baseURL,
  page,
  scene,
}) => {
  const counts = await missEntryChunk(page, (request) => request === 1, baseURL ?? "");

  await page.goto("/", { waitUntil: "commit" });
  await expect(page.getByText(`Signed in as ${scene.person}`)).toBeVisible();
  expect(counts).toEqual({ documents: 2, entryRequests: 2 });
});

// The entry's reloads come from the tab's budget, so a page whose entry chunk never loads reloads
// three times and then stays blank rather than reloading for ever.
//
// It takes no `scene`: nothing of the app ever runs, so no read of it reaches the API.
test("a page whose entry chunk is missing on every load reloads three times and stops @pr", async ({
  baseURL,
  page,
}) => {
  const counts = await missEntryChunk(page, () => true, baseURL ?? "");

  await page.goto("/", { waitUntil: "commit" });
  await expect.poll(() => counts.entryRequests).toBe(4);
  // The recovery's reload starts from the failed-asset handler of the load that just counted
  // that request, so a fifth document request would already be out; this margin is four frames
  // for it.
  await sleep(250);
  expect(counts).toEqual({ documents: 4, entryRequests: 4 });
  await expect(page.locator("#root")).toBeEmpty();
});

// One budget for the whole tab: a page chunk's failures that already reloaded the page three times
// leave none for the entry chunk, so the two recoveries cannot hand the page back and forth.
test("a missing entry chunk after a page chunk spent the tab's reloads stays @pr", async ({
  baseURL,
  page,
  scene,
}) => {
  await page.route(/\/assets\/IssuePage-[^/]+\.js$/u, (route) => route.abort());
  let entryMissing = false;
  const counts = await missEntryChunk(page, () => entryMissing, baseURL ?? "");

  await page.goto(`/issues/${scene.unreachable}`, { waitUntil: "commit" });
  // The issue page's code failed, reloaded the page three times, and failed again.
  await expect.poll(() => ({ ...counts })).toEqual({ documents: 4, entryRequests: 4 });
  await expect(page.getByTestId("error-boundary")).toBeVisible();

  entryMissing = true;
  await page.reload({ waitUntil: "commit" });
  await expect.poll(() => counts.entryRequests).toBe(5);
  // The recovery's reload starts from the failed-asset handler of the load that just happened,
  // so a sixth document request would already be out; this window is four frames of margin for
  // it, and the wall clock is the measurement because there is no reload to await.
  await page.waitForTimeout(250);
  expect(counts).toEqual({ documents: 5, entryRequests: 5 });
  await expect(page.locator("#root")).toBeEmpty();
});

// The inline script and the bundle's own handler spend one budget between them: a page whose entry
// chunk is missing twice reloads twice, and its route's chunk failing after that reloads it once
// more and then stays. The broadcasts page's chunk imports nothing the entry does not already, so
// Vite inserts no preload link for it and only the bundle's handler sees it fail.
//
// The recording is what answers the reads of the load that does open; the row names nothing of
// the seeding, so it binds the fixture under a name that says why it is here.
test("an entry chunk's reloads and a page chunk's share the tab's three @pr", async ({
  baseURL,
  page,
  scene: _recording,
}) => {
  await page.route(/\/assets\/BroadcastsPage-[^/]+\.js$/u, (route) => route.abort());
  const counts = await missEntryChunk(page, (request) => request <= 2, baseURL ?? "");

  await page.goto("/agents/broadcasts", { waitUntil: "commit" });
  await expect.poll(() => ({ ...counts })).toEqual({ documents: 4, entryRequests: 4 });
  await expect(page.getByTestId("error-boundary")).toContainText(
    "Failed to fetch dynamically imported module"
  );
  // The recovery's reload starts from the failed-asset handler of the load whose boundary is
  // drawn above, so a fifth document request would already be out; four frames of margin.
  await sleep(250);
  expect(counts).toEqual({ documents: 4, entryRequests: 4 });
});

// A chunk the shell starts once the page has drawn and nobody is waiting on: the search bar's,
// which comes down before any key opens the bar (`web/src/features/search/SearchPaletteHost.tsx`,
// `prefetchChunk`). Its failure says only that the download has not happened yet, so it reloads
// nothing and spends nothing of the tab's budget (`web/src/features/shell/DeploymentResilience.tsx`).
// The download is failed once the margin's and the preview layer's chunks are in: a failure while
// a render waits on a chunk of its own is that reader's, and reloads whatever chunk it was.
test("a chunk the shell only prefetched reloads nothing when it fails @pr", async ({
  baseURL,
  page,
  scene: _recording,
}) => {
  const counts = await missEntryChunk(page, () => false, baseURL ?? "");
  const load = shellLoads(page);
  const failed = await failAssetWhen(
    page,
    /\/assets\/SearchPalette-[^/]+\.js$/u,
    () => load().settled
  );

  await page.goto("/agents");
  await expect(page.getByRole("heading", { level: 1, name: "Agents" })).toBeVisible();
  await expect.poll(failed).toBeGreaterThan(0);
  // A reload the failure started would already be out; four frames of margin for it.
  await sleep(250);
  expect(counts).toEqual({ documents: 1, entryRequests: 1 });
  expect(await tabReloads(page)).toBeNull();
});

// The same prefetch, failing in one of the module preload links Vite inserts for it: the bar's
// chunk imports the shortcut sheet's, which no page has asked for yet. `web/index.html`'s inline
// script leaves such a link to the bundle while the bundle's recovery runs
// (`__dispatchChunkRecovery`), and the bundle knows it for a prefetch; the inline script alone
// would reload for it as for an asset the document itself names.
test("a module preload that fails for a chunk the shell only prefetched is left to the bundle and reloads nothing @pr", async ({
  baseURL,
  page,
  scene: _recording,
}) => {
  const counts = await missEntryChunk(page, () => false, baseURL ?? "");
  const load = shellLoads(page);
  const failed = await failAssetWhen(
    page,
    /\/assets\/ShortcutHelp-[^/]+\.js$/u,
    () => load().settled
  );

  await page.goto("/agents");
  await expect(page.getByRole("heading", { level: 1, name: "Agents" })).toBeVisible();
  await expect.poll(failed).toBeGreaterThan(0);
  // A reload the failure started would already be out; four frames of margin for it.
  await sleep(250);
  expect(counts).toEqual({ documents: 1, entryRequests: 1 });
  expect(await tabReloads(page)).toBeNull();
});

// A route chunk the entry never prefetches - the broadcasts page's - failing while a prefetch of
// another chunk is out: the Inbox peek's, which this address starts after its first paint and
// this row holds on the wire. The reader is waiting on the route, so its failure reloads from the
// tab's three and then shows the failed download, as the issue page's does above. A route chunk
// started outside `renderChunk` would be read as that prefetch's and reload nothing
// (`web/src/features/shell/DeploymentResilience.tsx`).
test("a route chunk nothing prefetched reloads for its failure while another chunk's prefetch is out @pr", async ({
  baseURL,
  page,
  scene: _recording,
}) => {
  const counts = await missEntryChunk(page, () => false, baseURL ?? "");
  const load = shellLoads(page);
  await holdAsset(page, /\/assets\/inbox-chunk-[^/]+\.js$/u);
  await failAssetWhen(page, /\/assets\/BroadcastsPage-[^/]+\.js$/u, async () => {
    const current = load();
    await Promise.all([current.settled, current.peekAsked]);
  });

  await page.goto("/agents/broadcasts", { waitUntil: "commit" });
  await expect.poll(() => counts.documents).toBe(4);
  await expect(page.getByTestId("error-boundary")).toContainText(
    "Failed to fetch dynamically imported module"
  );
  // The recovery's reload starts from the failed-asset handler of the load whose boundary is
  // drawn above, so a fifth document request would already be out; four frames of margin.
  await sleep(250);
  expect(counts).toEqual({ documents: 4, entryRequests: 4 });
});

// A host's chunk, opened by the reader: the search bar's prefetch failed with nobody waiting, as
// in the row above it, and `$mod+k` then asks for the same chunk under `renderChunk`
// (`web/src/features/search/SearchPaletteHost.tsx`). The reader is waiting on that ask, so its
// failure reloads, although the Inbox peek's prefetch is still out beside it.
test("a bar whose prefetch failed reloads for its chunk when the reader opens it, while another prefetch is out @pr", async ({
  baseURL,
  page,
  scene: _recording,
}) => {
  const counts = await missEntryChunk(page, () => false, baseURL ?? "");
  const load = shellLoads(page);
  await holdAsset(page, /\/assets\/inbox-chunk-[^/]+\.js$/u);
  const failed = await failAssetWhen(page, /\/assets\/SearchPalette-[^/]+\.js$/u, async () => {
    const current = load();
    await Promise.all([current.settled, current.peekAsked]);
  });

  await page.goto("/agents");
  await expect(page.getByRole("heading", { level: 1, name: "Agents" })).toBeVisible();
  await expect.poll(failed).toBeGreaterThan(0);
  // The prefetch's failure reloaded nothing; four frames of margin for a reload it started.
  await sleep(250);
  expect(counts.documents).toBe(1);

  await page.keyboard.press("ControlOrMeta+k");
  await expect.poll(() => counts.documents).toBe(2);
  // Read across the reload: a read made while the old document is being left has no page to
  // run in, and is asked again.
  await expect.poll(() => tabReloads(page).catch(() => null)).toHaveLength(1);
});

// The main stylesheet is linked after the inline recovery script, so one a server that never built
// it fails to serve reloads the page as a missing entry chunk does, rather than leaving the app
// running unstyled.
test("a page whose main stylesheet fails once reloads and renders styled @pr", async ({
  baseURL,
  page,
  scene,
}) => {
  const stylesheet = await pageAssetPath(
    /<link\b[^>]*\brel="stylesheet"[^>]*\bhref="([^"]+)"/u,
    "stylesheet",
    baseURL ?? ""
  );
  const counts = await missEntryChunk(page, () => false, baseURL ?? "");
  let stylesheetRequests = 0;
  await page.route(
    (url) => url.pathname === stylesheet,
    async (route) => {
      stylesheetRequests += 1;
      if (stylesheetRequests === 1) {
        await route.abort();
        return;
      }
      await route.continue();
    }
  );

  await page.goto("/", { waitUntil: "commit" });
  await expect(page.getByText(`Signed in as ${scene.person}`)).toBeVisible();
  // The stylesheet's preflight takes away the browser's own 8px margin around the body.
  await expect.poll(() => page.evaluate(() => getComputedStyle(document.body).margin)).toBe("0px");
  expect({ ...counts, stylesheetRequests }).toEqual({
    documents: 2,
    entryRequests: 2,
    stylesheetRequests: 2,
  });
});

// A failed download while the browser is offline is an outage, not a replaced deployment, and a
// reload would swap the blank page for the browser's offline page.
//
// It takes no `scene`: nothing of the app ever runs, so no read of it reaches the API.
test("a page whose entry chunk fails while the browser is offline does not reload @pr", async ({
  baseURL,
  page,
}) => {
  await page.addInitScript(() => {
    Object.defineProperty(Navigator.prototype, "onLine", { configurable: true, get: () => false });
  });
  const counts = await missEntryChunk(page, () => true, baseURL ?? "");

  await page.goto("/", { waitUntil: "commit" });
  await expect.poll(() => counts.entryRequests).toBeGreaterThan(0);
  // The miss is counted as the request is routed and answered 404 at once, so a reload it
  // started would already be out; four frames of margin for it.
  await sleep(250);
  expect(counts).toEqual({ documents: 1, entryRequests: 1 });
});

// The recovery is for this build's own assets. A script another origin serves (a browser
// extension's, or a third party's) is not this build's, whatever its path, so its failure to load
// neither reloads a page the reader is using nor spends the tab's budget.
test("a script from another origin that fails under an /assets/ path leaves the page alone @pr", async ({
  baseURL,
  page,
  scene,
}) => {
  await page.route("https://extension.invalid/**", (route) => route.abort());
  const counts = await missEntryChunk(page, () => false, baseURL ?? "");

  await page.goto("/");
  await expect(page.getByText(`Signed in as ${scene.person}`)).toBeVisible();
  await page
    .evaluate(
      () =>
        new Promise<void>((resolve) => {
          const script = document.createElement("script");
          script.src = "https://extension.invalid/assets/inject.js";
          script.addEventListener("error", () => resolve());
          document.head.append(script);
        })
    )
    .catch(() => undefined);
  // The script's own error event is awaited above, so a reload the failure started would
  // already be out; four frames of margin for it.
  await sleep(250);
  expect(counts).toEqual({ documents: 1, entryRequests: 1 });
  expect(await tabReloads(page)).toBeNull();
});

// The sidebar's footer holds Sign out, and the navigation above it grows with the project list.
// A column that is only as tall as the viewport at its minimum pushes the footer and the lower
// links off the bottom of the screen, where a reader reaches them only by scrolling to the end
// of the page.
test("the sidebar scrolls, so Sign out is reachable with a navigation taller than the viewport @pr", async ({
  page,
  scene,
}) => {
  await page.setViewportSize({ height: 720, width: 1280 });
  await page.goto("/");

  const signOut = page.getByRole("button", { name: "Sign out" });
  await expect(signOut).toBeAttached();
  // The rail is its own scroll container: the footer comes into view by scrolling the rail,
  // and the page itself never moves, so the footer is never left past the end of a long
  // issue, reachable only by scrolling the document to its end.
  await signOut.scrollIntoViewIfNeeded();
  await expect(signOut).toBeInViewport();
  expect(await page.evaluate(() => window.scrollY)).toBe(0);
  await signOut.focus();
  await expect(signOut).toBeInViewport();
  await expect(signOut).toBeFocused();
  // Every project link is reachable by scrolling that container, still not the page.
  const lastProject = page.getByRole("link", { name: new RegExp(scene.lastProject) });
  await lastProject.scrollIntoViewIfNeeded();
  await expect(lastProject).toBeInViewport();
  expect(await page.evaluate(() => window.scrollY)).toBe(0);
});

// The right gutter is reserved for the collapsed-margin rail, and that rail only exists where
// the route has a margin. Reading the raw preference instead left 80px of padding on the right
// of the Inbox, beside nothing.
test("a route with no margin reserves no gutter for a margin rail @pr", async ({ page, scene }) => {
  await page.setViewportSize({ height: 900, width: 1440 });
  await page.goto(`/issues/${scene.withMargin}`);
  await page.getByRole("button", { name: "Hide margin" }).click();
  await expect(page.getByTestId("margin-rail")).toBeVisible();
  const main = page.getByTestId("main-content");
  expect(await main.evaluate((node) => getComputedStyle(node).paddingRight)).toBe("80px");

  await page.goto("/");
  await expect(page.getByTestId("margin-rail")).toHaveCount(0);
  expect(await main.evaluate((node) => getComputedStyle(node).paddingRight)).toBe("24px");
  await expect(main).toHaveAttribute("data-shell-layout", "standard");

  await page.getByRole("button", { name: "Hide sidebar" }).click();
  await expect(main).toHaveAttribute("data-shell-layout", "full-width");
  expect(await main.evaluate((node) => getComputedStyle(node).paddingRight)).toBe("24px");
  expect(await main.evaluate((node) => getComputedStyle(node).paddingLeft)).toBe("80px");
});
