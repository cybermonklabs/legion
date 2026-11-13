/** What the shell's chunk-recovery rows need of the page: the assets the document names, a
 *  deployment that never built one of them, and the tab's record of the reloads it has spent.
 *
 *  Two spec files share this. `shell.stub.e2e.ts` holds the rows that need nothing but the
 *  page and its own routes; `shell.e2e.ts` holds the rows whose failed asset must be the
 *  server's own 404, which the stubbed layer's file server does not give (it answers an
 *  unknown path with `index.html`, as the SPA fallback).
 */
import { setTimeout as sleep } from "node:timers/promises";

import type { Page } from "@playwright/test";

/** The path of the asset the page at `origin` names in `pattern`'s first group. */
export async function pageAssetPath(
  pattern: RegExp,
  what: string,
  origin: string
): Promise<string> {
  const html = await (await fetch(new URL("/", origin))).text();
  const asset = pattern.exec(html)?.[1];
  if (asset === undefined) {
    throw new Error(`the page names no ${what}: ${html}`);
  }
  return new URL(asset, origin).pathname;
}

/**
 * Answers the page's entry chunk 404, as a server whose deployment never built it does, whenever
 * `missing` says so for that request (numbered from 1; it may hold the request until it answers),
 * and counts the page's document loads and entry requests.
 */
export async function missEntryChunk(
  page: Page,
  missing: (request: number) => boolean | Promise<boolean>,
  origin: string
) {
  // The entry chunk: the module script Vite writes into index.html.
  const entry = await pageAssetPath(
    /<script\b[^>]*\btype="module"[^>]*\bsrc="([^"]+)"/u,
    "module script",
    origin
  );
  const counts = { documents: 0, entryRequests: 0 };
  page.on("request", (request) => {
    if (request.isNavigationRequest() && request.frame() === page.mainFrame()) {
      counts.documents += 1;
    }
  });
  await page.route(
    (url) => url.pathname === entry,
    async (route) => {
      counts.entryRequests += 1;
      if (!(await missing(counts.entryRequests))) {
        await route.continue();
        return;
      }
      await route.fulfill({
        body: JSON.stringify({ error: "not found" }),
        contentType: "application/json",
        status: 404,
      });
    }
  );
  return counts;
}

/** Records reloads `minutesAgo` by the page's clock as the tab's recovery reloads, and returns
 *  their times. */
export function recordTabReloads(page: Page, minutesAgo: number[]): Promise<number[]> {
  return page.evaluate((ages) => {
    const now = Date.now();
    const times = ages.map((age) => now - age * 60_000);
    window.sessionStorage.setItem("dispatch.chunk-reloads", JSON.stringify(times));
    return times;
  }, minutesAgo);
}

/** The tab's recorded recovery reloads. */
export function tabReloads(page: Page): Promise<number[] | null> {
  return page.evaluate(() =>
    JSON.parse(window.sessionStorage.getItem("dispatch.chunk-reloads") ?? "null")
  );
}

/** Adds a script of this build that the server never had, as a replaced deployment's would fail. */
export async function failBuildScript(page: Page): Promise<void> {
  await page.evaluate(() => {
    const script = document.createElement("script");
    script.src = "/assets/index-missing.js";
    document.head.append(script);
  });
}

/** One document load of a page, as the rows about a prefetched chunk read it. */
export interface ShellLoad {
  /** Settles once the two chunks the shell renders on every page and waits on - the margin's and
   *  the reference-preview layer's - have come down, and a moment more for their imports to
   *  settle. A chunk failure while one of them is out is a failure a reader is waiting behind,
   *  whatever chunk it is (`web/src/features/shell/DeploymentResilience.tsx`), so a row about a
   *  chunk nobody waits on fails its chunk only after this. */
  readonly settled: Promise<void>;
  /** Settles once the Inbox peek's prefetch has gone out (`web/src/features/inbox/InboxHost.tsx`,
   *  `usePeekChunkAfterFirstPaint`), which a page the landing reads as the Inbox starts after its
   *  first paint. */
  readonly peekAsked: Promise<void>;
}

function shellLoad() {
  const margin = Promise.withResolvers<void>();
  const preview = Promise.withResolvers<void>();
  const peek = Promise.withResolvers<void>();
  const settled = (async () => {
    await Promise.all([margin.promise, preview.promise]);
    // A chunk's response is in before the import it answers has settled and before
    // `renderChunk`'s count of it has dropped; twelve frames is that settling with room to spare.
    await sleep(200);
  })();
  return { load: { peekAsked: peek.promise, settled }, margin, peek, preview };
}

/** The current document load of `page`, a new one from each navigation of its main frame. */
export function shellLoads(page: Page): () => ShellLoad {
  let current = shellLoad();
  page.on("request", (request) => {
    if (request.isNavigationRequest() && request.frame() === page.mainFrame()) {
      current = shellLoad();
    } else if (/\/assets\/inbox-chunk-[^/]+\.js$/u.test(new URL(request.url()).pathname)) {
      current.peek.resolve();
    }
  });
  page.on("requestfinished", (request) => {
    const path = new URL(request.url()).pathname;
    if (/\/assets\/Margin-[^/]+\.js$/u.test(path)) current.margin.resolve();
    if (/\/assets\/RefPreview-[^/]+\.js$/u.test(path)) current.preview.resolve();
  });
  return () => current.load;
}

/** Holds every request for the asset at `pattern` on the wire for the rest of the row: a
 *  download that is out and never answers. */
export async function holdAsset(page: Page, pattern: RegExp): Promise<void> {
  await page.route(pattern, () => undefined);
}

/** Fails every request for the asset at `pattern`, each once `ready` has settled for the load it
 *  belongs to, and returns how many it failed. */
export async function failAssetWhen(
  page: Page,
  pattern: RegExp,
  ready: () => Promise<unknown>
): Promise<() => number> {
  let failed = 0;
  await page.route(pattern, async (route) => {
    await ready();
    await route.abort();
    failed += 1;
  });
  return () => failed;
}
