/** What the shell's chunk-recovery rows need of the page: the assets the document names, a
 *  deployment that never built one of them, and the tab's record of the reloads it has spent.
 *
 *  Two spec files share this. `shell.stub.e2e.ts` holds the rows that need nothing but the
 *  page and its own routes; `shell.e2e.ts` holds the rows whose failed asset must be the
 *  server's own 404, which the stubbed layer's file server does not give (it answers an
 *  unknown path with `index.html`, as the SPA fallback).
 */
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
