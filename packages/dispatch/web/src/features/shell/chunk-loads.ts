import { type ComponentType, type LazyExoticComponent, lazy } from "react";

// Chunk downloads nobody is waiting for, and chunk downloads a reader is waiting for: what the
// chunk-failure recovery (`DeploymentResilience.tsx`, `reloadForChunkFailure`) reads to tell the
// failure of a prefetch, which reloads nothing, from the failure of a chunk a reader is in front
// of, which reloads from the tab's budget.
//
// WHAT THAT RULE RESTS ON. The recovery asks "is a reader waiting on any chunk?", and it can only
// answer from the chunks started through the functions of this module. A chunk a reader waits
// for and that was started some other way is invisible to it: it fails, `renderedChunksOut` is
// still zero, a prefetch of some other chunk happens to be in flight, and the page reloads
// nothing although the reader is in front of a page that cannot draw. Before
// dispatch://INFRA-1109 the `lazy` routes of `app.tsx`, the machine sign-in route, the live view,
// the margin, the editor library and Yjs transport a mounting document awaits, the Yjs a past
// version awaits, the pending-edit store a removed document's edits are deleted from, the parts
// of the hold a row's editor awaits (`features/issue-page/node-caret.ts`), the headless Markdown
// engine a drawn body awaits and the delivery timeline's charting library were all started bare,
// and that cost nothing while the shell prefetched no chunk, since the recovery then reloaded,
// within its budget, for every chunk that failed online. The entry now starts the landed page's
// chunks beside the landing read, before React has drawn anything (`app.tsx`,
// `preloadRouteChunk`), and the shell starts prefetches at a page's first paint - the search
// bar's and the shortcut sheet's, the Markdown engine's warm and, on an address the landing read
// takes for the Inbox, the Inbox peek's - and once the page's content has drawn
// (`content-drawn.ts`) - the Create issue dialog's and the pending-writes host's two (the
// pending-writes indicator and the kinds of write, `write-hosts.tsx`); the sign-in page starts the
// kinds of write as it draws and again as the reader comes back to it (`KeptWritesHost`); Settings
// starts the new issue page's frame as it draws its switch
// (`features/issue-page/IssuePagePreference.tsx`); and an open of a key's surface that the reader
// called off leaves its download out as a prefetch (`asked-surface.tsx`). A bare chunk that
// failed in any of those windows would reload nothing.
//
// So: a chunk a render waits on goes through `renderChunk`, a chunk nothing waits on through
// `prefetchChunk`, and nothing imports a chunk through neither. A component a `Suspense` waits
// on goes through `lazyChunk`, which is `renderChunk` and `importWhenOnline` under React's
// `lazy`, and that one is held by construction: `lazy` itself is refused everywhere in `web/src`
// but this module, by the `noRestrictedImports` rule of the repository's `biome.json`. Where one
// import is both - the entry prefetches a route's chunk that the router then renders, the shell
// warms the Markdown engine that a body then awaits - the two are told apart at each call, not
// in the import, because the download is shared.
//
// The functions live here, apart from the recovery, so a module the recovery's own component
// imports - the Markdown engine, which it warms - can take `renderChunk` without importing that
// component back, which would be an import cycle. `importWhenOnline` is here for the same
// reason, although it is the other half of the recovery's offline rule: `lazyChunk` loads
// through it, and the recovery's component imports this module.
//
// `vite:preloadError` is dispatched from the rejection itself, before any `then` of the promise
// these hold runs, so a render's failure always finds its own count still up.
let prefetchedChunksOut = 0;
let renderedChunksOut = 0;

/** Starts a chunk nothing is waiting for yet, and swallows its failure: the same import runs
 *  again under `renderChunk` when a reader comes to wait on it - the router rendering the route,
 *  a host drawing the component the chunk carries, a document or the write store's take-up
 *  awaiting the modules, the shell awaiting the new issue page's frame (`app.tsx`,
 *  `ChosenShell`), or a body drawn after a failed warm of the Markdown engine, which keeps no
 *  failed load (`keptUntilRejected`) - and that is where a reader meets a chunk that will not
 *  download. */
export function prefetchChunk(load: () => Promise<unknown>): void {
  prefetchedChunksOut += 1;
  void load()
    .catch(() => undefined)
    .finally(() => {
      prefetchedChunksOut -= 1;
    });
}

/** Starts a chunk a reader is waiting on: a route the router is rendering, or a library a mounted
 *  component awaits before it can draw. While one of these is out, a chunk failure reloads from
 *  the tab's budget however many prefetches are in flight beside it. */
export function renderChunk<T>(load: () => Promise<T>): Promise<T> {
  renderedChunksOut += 1;
  const chunk = load();
  void chunk
    .catch(() => undefined)
    .finally(() => {
      renderedChunksOut -= 1;
    });
  return chunk;
}

/**
 * A function that runs `load` once and answers that promise to every later call while it is out
 * or resolved; once it rejects it is dropped, and the next call runs `load` again. A promise that
 * loads a chunk or a module is kept only while it is out or resolved, because a kept rejection
 * answers every later caller with the same failure, runs no import, fires no `vite:preloadError`
 * and reloads nothing. The drop is registered before the promise is returned, so it runs before
 * any caller's own handler: a caller the failure reaches already finds nothing kept.
 */
export function keptUntilRejected<T>(load: () => Promise<T>): () => Promise<T> {
  let kept: Promise<T> | undefined;
  return () => {
    if (kept !== undefined) {
      return kept;
    }
    const loading = load();
    kept = loading;
    void loading.catch(() => {
      if (kept === loading) {
        kept = undefined;
      }
    });
    return loading;
  };
}

/**
 * Runs `load`, and runs it again once the window fires `online` each time it rejects while the
 * browser is offline; a rejection while online reaches the caller. It is the other half of the
 * recovery's offline rule (`DeploymentResilience.tsx`, `reloadForChunkFailure`): a chunk that
 * fails while the browser is offline reloads nothing, since a reload then would swap the app for
 * the browser's offline page, so the chunk is asked for again when the network returns instead.
 * A browser that keeps a failed module fetch in its module map makes that retry reject again, as
 * WebKit did when the keyed-writes outage row was tried on it (Chromium and Firefox asked for the
 * chunk again); the retry then rejects online, propagates like any online failure and reaches
 * `reloadForChunkFailure`, which reloads while the tab's budget lasts.
 *
 * What loads through it: the chunks of a document - the editor's library and its node views,
 * Yjs, Hocuspocus and the pending-edit store, code-split so the issue route's chunk carries none
 * of them, which the entry starts beside the landing read of a page that draws a document and a
 * document awaits as it mounts and connects (`features/doc/document-chunks.ts`,
 * `loadEditorLibrary` and `loadTransportModules`), the Yjs a past version awaits
 * (`features/doc/editor.ts`, `createDoc`) and the pending-edit store a removed document's edits
 * are deleted from (`features/doc/ProofDocument.tsx`) - the kinds of write the write store's
 * take-up waits for (`features/writes/write-hosts.tsx`, `loadWriteKinds`), the new issue page's
 * frame the shell waits for (`app.tsx`, `ChosenShell`), the parts of the hold a row's editor
 * awaits (`features/issue-page/node-caret.ts`, `loadHoldParts`), every `lazyChunk`, every
 * `chunkComponent` (`chunk-component.tsx`) and every surface a reader opens with a key
 * (`asked-surface.tsx`).
 */
export async function importWhenOnline<T>(load: () => Promise<T>): Promise<T> {
  for (;;) {
    try {
      return await load();
    } catch (error) {
      if (navigator.onLine) {
        throw error;
      }
      await new Promise<void>((resolve) => {
        window.addEventListener("online", () => resolve(), { once: true });
      });
    }
  }
}

/**
 * React's `lazy`, with the download under `renderChunk` and `importWhenOnline`: how every
 * component of this app whose chunk a `Suspense` waits on is written - a route, the Inbox's own
 * page, a page behind a gate, the new issue page (`features/issue-page/IssuePage.tsx`), the
 * margin (`features/margin/MarginColumn.tsx`) and the live view
 * (`features/agent-view/MountedLiveViews.tsx`). React asks for the chunk the first
 * time it renders the component, so a reader is in front of the boundary that waits on it, and a
 * download that fails must spend one of the tab's recovery reloads however many prefetches are
 * in flight beside it. A surface a reader opens with a key is not written so: its keys would
 * reach the page while `lazy` holds it back (`asked-surface.tsx`).
 *
 * WHY `importWhenOnline`. React's `lazy` keeps the promise its loader returned for the life of
 * the page and never calls the loader again, so a rejection it holds answers every later render
 * of the component with the same failure. Online that failure is Vite's `vite:preloadError`
 * while `renderChunk` counts the render as waiting, so the recovery reloads the tab, and the page
 * that kept the rejection goes with it - unless no reload follows: the page still counts as being
 * left, the reader stays at the leave prompt a write not yet confirmed opens, the page has asked
 * for its one reload already, or the tab's budget of reloads is spent. The rejection then outlives
 * the page, and the error boundary around the component (`ErrorBoundary.tsx`) draws the failure
 * in its region's place, with a reload of its own, while the rest of the page stays drawn.
 * Offline the recovery reloads nothing (`DeploymentResilience.tsx`, `reloadForChunkFailure`),
 * so a rejection kept there would leave the component broken for the life of the tab. Under
 * `importWhenOnline` the promise `lazy` keeps rejects only while the browser is online, where a
 * failure reloads; offline, the boundary's `Suspense` keeps its fallback until the window fires
 * `online` and the import runs again.
 *
 * Typed on the component, as `lazy` is, so the component keeps its own props.
 */
// biome-ignore lint/suspicious/noExplicitAny: this is `lazy`'s own bound on the component it loads, passed through to it unchanged; `ComponentType<never>` is not assignable to it, through the class half of `ComponentType`.
export function lazyChunk<T extends ComponentType<any>>(
  load: () => Promise<{ default: T }>
): LazyExoticComponent<T> {
  return lazy(() => renderChunk(() => importWhenOnline(load)));
}

/** Whether the chunks out are prefetches alone: at least one is, and no render waits on any. A
 *  chunk failure then is a prefetch's, and the recovery reloads nothing for it. */
export function onlyPrefetchesOut(): boolean {
  return prefetchedChunksOut > 0 && renderedChunksOut === 0;
}

/** How many chunk downloads `prefetchChunk` and `renderChunk` have started and not seen end. The
 *  recovery reads the page's state through `onlyPrefetchesOut`; a test of that rule reads this to
 *  start from a page with no download out, which one an earlier test started and did not wait for
 *  would otherwise leave behind for it. */
export function chunkLoadsOut(): number {
  return prefetchedChunksOut + renderedChunksOut;
}
