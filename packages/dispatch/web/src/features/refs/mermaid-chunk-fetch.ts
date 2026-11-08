/**
 * Telling a chunk the browser could not fetch from a source Mermaid refuses, for the two modules
 * that have to: `mermaid-upstream.ts`, which awaits Mermaid's own dynamic imports, and
 * `mermaid-render.worker.ts`, which turns a throw into the reply the page reads. Neither may
 * import a value of the other -- the worker reaches the renderer module with `await import(...)`,
 * so a static edge would put Mermaid, d3 and cytoscape in the worker's own chunk -- so the one
 * reading of a throw lives here, in a module that imports nothing.
 *
 * A dynamic import whose fetch fails rejects with a `TypeError` in every engine, but the message
 * is the engine's own. These three were measured on 2026-11-07 in this repository's own
 * Playwright browsers (Chromium 1248, Firefox 1555, WebKit 2370), inside a module Web Worker
 * whose `import(...)` was answered with an aborted request, with a 404 and with a 500 in turn;
 * each engine gave one message for all three failures:
 *
 *   Chromium  TypeError: Failed to fetch dynamically imported module: <url>
 *   Firefox   TypeError: error loading dynamically imported module: <url>
 *   WebKit    TypeError: Importing a module script failed.
 *
 * Only a `TypeError` whose message is one of those is read as a failed fetch. Everything else is
 * read as a refusal of the source, which is the safer of the two answers to be wrong about: a
 * refusal is drawn as the block's status line and remembered, so it ends, where a source wrongly
 * called unavailable is left as code with no reason beside it and asked for again on every paint
 * of every body that holds it, for as long as a reader has one open. So a wording an engine
 * changes costs that engine's readers the wrong message, which is what they get today, rather
 * than a loop; and a `TypeError` Mermaid itself throws over a source -- a renderer's own bug --
 * is never taken for a missing chunk.
 *
 * Bun, which the unit suite and an ad-hoc harness run these modules under, answers a failed
 * import with a `ResolveMessage` ("Cannot find module ...") rather than a `TypeError`, and is
 * deliberately not recognised: the page runs in a browser, and under Bun a chunk that will not
 * load is a build that did not emit it rather than a deploy that moved one.
 */

/** What each engine's `TypeError` says, lower-cased; the URL some of them end in is left out, so
 *  the test is on the wording alone. */
const FAILED_FETCH_WORDINGS = [
  "failed to fetch dynamically imported module",
  "error loading dynamically imported module",
  "importing a module script failed",
];

/** Whether `error` is one of Mermaid's chunks that the browser could not fetch, rather than
 *  anything Mermaid refused a source for. */
export function failedChunkFetch(error: unknown): boolean {
  if (!(error instanceof TypeError)) return false;
  const message = error.message.toLowerCase();
  return FAILED_FETCH_WORDINGS.some((wording) => message.includes(wording));
}
