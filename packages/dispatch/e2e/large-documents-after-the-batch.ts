// What the rows of `large-documents-first-screen.e2e.ts` and its two siblings
// (`large-documents-first-screen.the-chrome.e2e.ts`,
// `large-documents-first-screen.the-keyboard.e2e.ts`) read the page's order by: which reads it
// made and when, when the editor's first batch landed, and when each part of the chrome the page
// defers was first drawn (dispatch://INFRA-341, R11, "the issue page's non-document panels are
// drawn after the editor's first batch"; `web/src/features/doc/AfterTheFirstBatch.tsx`), and
// how long they leave a held page to settle. No other file reads them. The measurement those
// rows sit beside, and the selector the first batch is read by, are `large-documents-page.ts`.
import type { Page } from "@playwright/test";

import { LONGEST_HOLD_MS } from "../web/src/features/doc/first-batch";
import { liveEditorSelector } from "./large-documents-page";

declare global {
  interface Window {
    /** `performance.now()` when the editor's first batch landed, which is the task its root
     *  was given `data-write-path="live"` in. Undefined until one has (`watchTheReads`). */
    __largeDocumentFirstBatchAt?: number;
    /** When each part of the chrome the page defers was first drawn, by the name
     *  `watchTheChrome` was given for it; `performance.now()` at that drawing's own task. */
    __chromeDrawn?: Record<string, number>;
    /** Every `/api/v1` request the page has made, by when it went out and the path it went
     *  out on, which is how a row reads the order of the page's reads (`watchTheReads`). */
    __largeDocumentReads?: { at: number; path: string }[];
  }
}

/** How long a row waits, with the socket held and the page settled, before it decides a read
 *  the page was going to make has not been made. The page's own draw of the document is on the
 *  screen well inside it, and every panel of the header has rendered.
 *
 *  Both this window and the editor's build after it have to finish inside the hold's own
 *  deadline, since a page held past that deadline releases the hold by design and the reads
 *  then go out with no batch behind them (`web/src/features/doc/first-batch.ts`,
 *  `LONGEST_HOLD_MS`, which the row of `large-documents-first-screen.the-chrome.e2e.ts` whose
 *  socket never answers covers). The assertion below is what keeps the two in step when either
 *  number moves. */
export const settledWithTheSocketHeldMs = 6_000;
if (settledWithTheSocketHeldMs * 2 >= LONGEST_HOLD_MS) {
  throw new Error(
    `the settled window (${settledWithTheSocketHeldMs} ms) leaves no room for the build inside the hold's deadline (${LONGEST_HOLD_MS} ms)`
  );
}

/**
 * What the page asked Dispatch for and when, and when the editor's first batch landed: the two
 * facts the rows that read the order of the page's reads are built on (dispatch://INFRA-341,
 * R11, "the issue page's non-document panels are drawn after the editor's first batch").
 *
 * Both come from the page itself. Each read is a `resource` entry of the browser's own
 * timing, taken through a `PerformanceObserver` rather than off the buffer, which holds 250
 * entries by default and which a document page fills; `startTime` is when the request went
 * out, which is the number the order is read on. The first batch is the task the editor's root
 * is given `data-write-path="live"` in (`@legion/proof-editor`'s `progressive-build.ts`), read
 * through a `MutationObserver`, whose callback runs at that task's own microtask checkpoint.
 * It runs before the commit that draws the chrome and starts the reads the page held, because
 * the batch's own blocks go into the editor before that attribute is set and queue this
 * observer there, while the page lets those reads go only in the callback the batch answers
 * with, which the build calls after both (`web/src/features/doc/ProofDocument.tsx`'s
 * `onBatch`): React's own microtask is therefore queued behind this one, and a read made at
 * the batch is never timed as one made before it.
 */
export interface PageRead {
  /** `performance.now()` when the request went out, counted from the start of navigation. */
  readonly at: number;
  /** The path it went out on, under `/api/v1`, with its query. */
  readonly path: string;
}

export interface ReadsOfThePage {
  /** `performance.now()` when the editor's first batch landed; undefined where none has. */
  readonly firstBatchMs: number | undefined;
  /** Every `/api/v1` request the page has made, in the order the browser timed them. */
  readonly reads: readonly PageRead[];
}

/** Installs those two recorders before the page's own scripts. Call it before the navigation
 *  whose reads the row is about; `readTheReads` reads what they recorded. */
export async function watchTheReads(page: Page): Promise<void> {
  await page.addInitScript(
    ({ live }: { live: string }) => {
      window.__largeDocumentReads = [];
      new PerformanceObserver((entries) => {
        for (const entry of entries.getEntries()) {
          const url = new URL(entry.name, window.location.origin);
          if (!url.pathname.startsWith("/api/v1/")) {
            continue;
          }
          window.__largeDocumentReads?.push({
            at: entry.startTime,
            path: `${url.pathname.slice("/api/v1".length)}${url.search}`,
          });
        }
      }).observe({ buffered: true, type: "resource" });
      const noteTheFirstBatch = () => {
        if (
          window.__largeDocumentFirstBatchAt === undefined &&
          document.querySelector(live) !== null
        ) {
          window.__largeDocumentFirstBatchAt = performance.now();
        }
      };
      new MutationObserver(noteTheFirstBatch).observe(document, {
        attributeFilter: ["data-write-path"],
        attributes: true,
        childList: true,
        subtree: true,
      });
    },
    { live: liveEditorSelector }
  );
}

/** What the two recorders above have so far. */
export async function readTheReads(page: Page): Promise<ReadsOfThePage> {
  return await page.evaluate(() => ({
    firstBatchMs: window.__largeDocumentFirstBatchAt,
    reads: window.__largeDocumentReads ?? [],
  }));
}

/**
 * When each part of the chrome the page defers first stood in the page, on the page's own
 * clock: the shell's own parts and the issue page's header, tab strip and artifact chrome,
 * which are drawn after the editor's first batch (dispatch://INFRA-341 R11,
 * `web/src/features/doc/AfterTheFirstBatch.tsx`).
 *
 * Each part is named by what the reader sees - its own element in the page - never by a class
 * the build renames, and each is recorded in the `MutationObserver` callback the browser runs
 * at the drawing task's own microtask checkpoint, so a part drawn in the batch's own task is
 * never timed later than it.
 */
export async function watchTheChrome(
  page: Page,
  parts: Readonly<Record<string, string>>
): Promise<void> {
  await page.addInitScript((watched: Readonly<Record<string, string>>) => {
    const drawn: Record<string, number> = {};
    window.__chromeDrawn = drawn;
    const look = () => {
      for (const [name, selector] of Object.entries(watched)) {
        if (drawn[name] === undefined && document.querySelector(selector) !== null) {
          drawn[name] = performance.now();
        }
      }
    };
    new MutationObserver(look).observe(document, {
      attributes: true,
      childList: true,
      subtree: true,
    });
    look();
  }, parts);
}

/** What `watchTheChrome` has recorded so far: the parts that have been drawn, by name. */
export async function readTheChrome(page: Page): Promise<Record<string, number>> {
  return await page.evaluate(() => window.__chromeDrawn ?? {});
}
