import { type ReactNode, useEffect, useRef, useState } from "react";

import { REQUEST_DEADLINE_MS } from "../../api/client";
import { useFootInset } from "../../hooks/useFootInset";
import {
  borderStrong,
  card,
  dismissButtonText,
  focusVisibleRing,
  primaryButtonBg,
  primaryButtonHoverBg,
  textSecondaryOnSurface,
} from "../../theme/shell-classes";
import { onlyPrefetchesOut, prefetchChunk } from "./chunk-loads";

// The tab's recovery reloads, as a JSON array of the epoch-millisecond times they started: a chunk
// that failed to download reloads the page while fewer than CHUNK_RELOAD_BUDGET of them fall in the
// last CHUNK_RELOAD_WINDOW_MS. `web/index.html`'s inline script keeps the same rule on the same key
// for a script or stylesheet that failed before any of this bundle ran, so the two share one
// budget whichever failed first. The budget renews, so a tab open across many deployments recovers
// from each, and a page whose assets fail on every load still stops after three reloads rather than
// reloading for ever. The earlier once-only key, `dispatch.reloaded-for-chunk`, is neither read nor
// written: a page of an older build in the same tab keeps spending it on its own.
const CHUNK_RELOADS_STORAGE_KEY = "dispatch.chunk-reloads";
const CHUNK_RELOAD_BUDGET = 3;
const CHUNK_RELOAD_WINDOW_MS = 5 * 60_000;
const VERSION_CHECK_INTERVAL_MS = 60_000;
const INDEX_ASSET_PATH = /^\/assets\/index-[^/]+\.js$/;
// GET /api/v1/dashboard/builds answers `{serving, live}`: the entry chunk of the build the
// answering process serves, and the entry chunks of the builds every live server process serves.
const DASHBOARD_BUILDS_PATH = "/api/v1/dashboard/builds";

declare const __DISPATCH_BUILD__: string;

// The Navigation API, as much of it as this module uses: TypeScript's DOM library does not declare
// it yet, and a browser without it leaves `window.navigation` undefined.
interface NavigateEvent extends Event {
  readonly destination: { readonly sameDocument: boolean; readonly url: string };
  readonly downloadRequest: string | null;
}

interface Navigation {
  addEventListener(type: "navigate", listener: (event: NavigateEvent) => void): void;
  removeEventListener(type: "navigate", listener: (event: NavigateEvent) => void): void;
}

declare global {
  interface Window {
    readonly navigation?: Navigation;
    /** Set while this bundle's chunk-failure recovery is installed, and read by `web/index.html`'s
     *  inline script: a `<link rel="modulepreload">` that fails while the bundle runs is one the
     *  bundle itself asked for, and `installChunkFailureRecovery` below judges it. */
    __dispatchChunkRecovery?: boolean;
  }
}

function indexAssetPath(source: string): string | undefined {
  const pathname = source.startsWith("/") ? source.split(/[?#]/, 1)[0] : new URL(source).pathname;
  return INDEX_ASSET_PATH.test(pathname) ? pathname : undefined;
}

function runningIndexAsset(): string {
  const scripts = document.querySelectorAll<HTMLScriptElement>("script[src]");
  for (const script of scripts) {
    const asset = indexAssetPath(script.src);
    if (asset !== undefined) {
      return asset;
    }
  }
  // The build ID keeps Vite's entry chunk distinct for every deployment, including a rebuild with
  // no source change: vite.config.ts defines it, and this is the reference that writes it into the
  // entry chunk, so it stays. It is not an entry path, and deploymentChanged compares nothing with
  // it: a page that names no entry chunk (Vite's dev server loads /src/main.tsx) offers no reload.
  return __DISPATCH_BUILD__;
}

// Whether to offer the reload notice: only when every live server process serves one build, and it
// is not this tab's. One request answers it, from whichever process the load balancer picks, since
// every process reads the same registry; asking `/` instead named the build of that one process,
// so during a rolling update or a rollback, while two builds are live, a tab was told to reload
// into a page the next request could replace with its own build again. Anything but a 200 offers
// nothing: a 404 from a process older than the route (one of the two during a roll), a 401, a 5xx,
// and a network failure, which rejects and which the caller swallows. A page that names no entry
// chunk offers nothing either: the build ID it falls back to is never a live entry path, so under
// `bun run dev` every focus would offer a reload.
//
// The request is held to the read deadline, its body included, so a check Dispatch never answers
// gives up rather than holding one of the browser's few connections to it for good (INFRA-258).
async function deploymentChanged(): Promise<boolean> {
  const response = await fetch(DASHBOARD_BUILDS_PATH, {
    cache: "no-store",
    signal: AbortSignal.timeout(REQUEST_DEADLINE_MS),
  });
  if (response.status !== 200) {
    return false;
  }
  const { live } = (await response.json()) as { live?: unknown };
  if (!Array.isArray(live) || live.length !== 1) {
    return false;
  }
  const [newest] = live;
  const running = runningIndexAsset();
  return typeof newest === "string" && INDEX_ASSET_PATH.test(running) && newest !== running;
}

// Set from the start of a navigation away from this page (`beforeunload`, or the Navigation API's
// `navigate` to another document, since iOS Safari never fires `beforeunload`) until the page is
// plainly still the reader's: shown again (`pageshow`, which a back/forward-cache restore fires,
// or the tab becoming visible again), or pressed (a pointer or a key on it). WebKit and Firefox
// cancel the chunk downloads still in flight when a navigation starts, WebKit also refuses the ones
// the page starts after it, and Vite reports each as a failed chunk. Those are not a replaced
// deployment, and a reload then would replace the reader's navigation with a reload of the page
// they are leaving. A navigation can also not happen after all - cancelled at another page's leave
// prompt, stopped, or answered by the server with a download - and no event says so; the press or
// the return that follows is what ends the state, never a timer, since a refusal can arrive any
// number of tasks after the navigation started.
let leavingPage = false;

function markPageLeaving(): void {
  leavingPage = true;
}

// A route change inside the app is a navigation within this document. A link with `download`
// (Dispatch's Download version links) fires `navigate` to another document too, with
// `downloadRequest` set, and then downloads the file and leaves the page where it is. Firefox
// (Playwright's build, at least) then fires a second `navigate` for the same click, to the same
// URL with no `downloadRequest`, so that immediate navigation is skipped. Once the page is
// plainly still in use, the next navigation to that URL leaves like any other. iOS Safari fires
// `navigate` from 26.2, for a link followed or a form submitted but not for an address the reader
// types.
let lastDownloadUrl: string | undefined;

function markPageLeavingForAnotherDocument(event: NavigateEvent): void {
  if (event.destination.sameDocument) {
    return;
  }
  const download = lastDownloadUrl;
  lastDownloadUrl = undefined;
  if (event.downloadRequest !== null) {
    lastDownloadUrl = event.destination.url;
    return;
  }
  if (event.destination.url !== download) {
    leavingPage = true;
  }
}

function markPageStaying(): void {
  leavingPage = false;
  lastDownloadUrl = undefined;
  // A reload the reader turned down at a leave prompt - one an unconfirmed keyed send raises -
  // leaves the tab's budget as it was: the page is plainly still theirs, so nothing is spent.
  chunkReloadAskedAt = undefined;
}

function markPageStayingWhenVisible(): void {
  if (document.visibilityState === "visible") {
    markPageStaying();
  }
}

// The recovery reloads of this tab that started within the window before `now`. A value that does
// not parse as an array of times reads as none, and the next reload overwrites it.
function recentChunkReloads(now: number): number[] {
  let stored: unknown;
  try {
    stored = JSON.parse(window.sessionStorage.getItem(CHUNK_RELOADS_STORAGE_KEY) ?? "[]");
  } catch {
    return [];
  }
  if (!Array.isArray(stored)) {
    return [];
  }
  return stored.filter(
    (time): time is number => typeof time === "number" && now - time < CHUNK_RELOAD_WINDOW_MS
  );
}

/**
 * Spends one of the tab's recovery reloads at `now` and says whether one was left: true records
 * `now` (dropping the times that have left the window) and the caller reloads; false records
 * nothing. The same rule as `web/index.html`'s inline script, which shares the key.
 */
export function spendChunkReload(now: number): boolean {
  const recent = recentChunkReloads(now);
  if (recent.length >= CHUNK_RELOAD_BUDGET) {
    return false;
  }
  recent.push(now);
  window.sessionStorage.setItem(CHUNK_RELOADS_STORAGE_KEY, JSON.stringify(recent));
  return true;
}

// When a chunk failure asked for one of the tab's recovery reloads and the page has not gone yet;
// undefined otherwise. The reload is spent as the page goes (`pagehide`), at that time, rather than
// when it is asked for: the browser may never leave - a leave prompt the reader cancels, a reload
// something else stops - and a reload asked for and never taken would leave the budget one short
// with the failed chunk still on the page.
let chunkReloadAskedAt: number | undefined;

function spendAskedChunkReload(): void {
  const askedAt = chunkReloadAskedAt;
  if (askedAt === undefined) {
    return;
  }
  chunkReloadAskedAt = undefined;
  try {
    spendChunkReload(askedAt);
  } catch {
    // A storage the browser refuses the page keeps no record; the reload still happens.
  }
}

// Chunk downloads nobody is waiting for, and chunk downloads a reader is waiting for: which of
// the two a failure is decides whether it reloads.
//
// The entry starts the landed route's chunks beside the landing read, before React has rendered
// anything (`app.tsx`, `preloadRouteChunk`), and the router asks for the same chunk again when it
// renders that route (`renderChunk`, which every `lazyChunk` route there goes through). A
// prefetch is nobody waiting: its failure says only that the download has not happened yet, and
// the router's own ask for the same chunk fails a moment later, in front of a reader whose page
// cannot draw. That second failure is the one this recovery was written for.
//
// Reloading for the prefetch instead costs the reader what the render's failure does not. It
// throws the document away while it is still loading - measured on the stubbed layer: with the
// prefetch judged, the three reloads of a chunk that never downloads all happen before the
// document fires `load`, so the reader is shown nothing at all until the budget is spent - and a
// download that only the prefetch lost, one the render would have got, would spend a reload for
// nothing. So a failure while a prefetch is still out and no render is waiting on a chunk is read
// as the prefetch's, and reloads nothing.
//
// WHAT THAT RULE RESTS ON is that every chunk a reader waits for is started under `renderChunk`,
// every chunk nobody waits for under `prefetchChunk`, and no chunk under neither: the rule can
// see only the chunks those two started. `chunk-loads.ts`, where they and `lazyChunk` live,
// says how that is held - for a component a `Suspense` waits on, by construction, since `lazy`
// itself is refused everywhere in `web/src` but that module (the `noRestrictedImports` rule of
// the repository's `biome.json`) - and which chunks were started bare until dispatch://INFRA-1109.

// Reloads the page for a chunk that failed to download while online, from the tab's budget.
function reloadForChunkFailure(): void {
  if (leavingPage) {
    return;
  }
  // A chunk a prefetch started and no render has asked for yet: the render asks again, and
  // that failure is the one a reader meets (above). That holds for the Markdown engine's warm too,
  // whose failed load is not kept (`features/refs/markdown-engine.ts`, `loadEngine`).
  if (onlyPrefetchesOut()) {
    return;
  }
  // A chunk that fails to download while the browser is offline is a network outage, not a
  // replaced deployment: reloading now would swap the app for the browser's offline page.
  if (!navigator.onLine) {
    return;
  }
  if (chunkReloadAskedAt !== undefined) {
    return;
  }
  if (recentChunkReloads(Date.now()).length >= CHUNK_RELOAD_BUDGET) {
    return;
  }
  chunkReloadAskedAt = Date.now();
  window.location.reload();
}

// Vite dispatches `vite:preloadError` for a chunk or one of its stylesheets that failed to
// download, and throws the failure to the importer unless the event is default-prevented. A
// prevented failure of the chunk itself resolves the import with `undefined`, which the importer
// reads as a module and fails on with a TypeError that says nothing about the download; a
// prevented stylesheet failure loads the module without its styles. So the handler never
// prevents it: the importer always sees the failure itself, and the page also reloads from the
// tab's budget, never while it is being left. A failed chunk or stylesheet Vite preloaded through
// a `<link rel="modulepreload">` is a download this bundle asked for, so `web/index.html`'s
// inline script leaves it to this handler while `__dispatchChunkRecovery` says the handler is
// installed: Vite reports the same failure here, with the prefetch rule above and the offline and
// leaving rules applied, while the inline script knows only the path. The inline script keeps
// every failure of the document's own assets - the entry chunk, its preload links and the main
// stylesheet - since those fail before this bundle can run, or while it never does.
export function installChunkFailureRecovery(): () => void {
  const navigation = window.navigation;
  window.addEventListener("beforeunload", markPageLeaving);
  navigation?.addEventListener("navigate", markPageLeavingForAnotherDocument);
  window.addEventListener("pagehide", spendAskedChunkReload);
  window.addEventListener("pageshow", markPageStaying);
  // Captured on the window, so a handler that stops a press from propagating cannot hide it.
  window.addEventListener("pointerdown", markPageStaying, { capture: true });
  window.addEventListener("keydown", markPageStaying, { capture: true });
  document.addEventListener("visibilitychange", markPageStayingWhenVisible);
  window.addEventListener("vite:preloadError", reloadForChunkFailure);
  window.__dispatchChunkRecovery = true;

  return () => {
    window.removeEventListener("beforeunload", markPageLeaving);
    navigation?.removeEventListener("navigate", markPageLeavingForAnotherDocument);
    window.removeEventListener("pagehide", spendAskedChunkReload);
    window.removeEventListener("pageshow", markPageStaying);
    window.removeEventListener("pointerdown", markPageStaying, { capture: true });
    window.removeEventListener("keydown", markPageStaying, { capture: true });
    document.removeEventListener("visibilitychange", markPageStayingWhenVisible);
    window.removeEventListener("vite:preloadError", reloadForChunkFailure);
    window.__dispatchChunkRecovery = undefined;
    markPageStaying();
  };
}

/**
 * Keeps long-lived Dispatch tabs able to render Markdown after a SPA deployment, and offers a
 * deliberate reload once every live server process serves one build other than the page's.
 */
export function DeploymentResilience(): ReactNode {
  const [updateAvailable, setUpdateAvailable] = useState(false);
  const dismissed = useRef(false);
  const lastVersionCheckAt = useRef(0);
  // The banner is the one float of the foot's banner layer (`hooks/useFootInset.ts` lists the
  // foot's layers and the elements in each). It stands 1rem above the page layer's property, which
  // the hook chooses so the banner lies over no page element and stays on the screen. It has no
  // floor. The hook holds its height, so the status corner keeps clear of it.
  const bannerRef = useRef<HTMLElement>(null);
  useFootInset(bannerRef, updateAvailable, "banner");

  // The headless Markdown engine, warmed once this component has painted. Its module
  // (`features/refs/markdown-engine.ts`) is fetched here rather than imported, and it is a chunk
  // of its own (`markdown-engine-*.js`, which carries the typed-block drawing as well, beside
  // the parse worker's queue and the stored-tree builder it shares with the editor's chunks): this
  // component is in the entry chunk, which every page parses before it draws anything, and this
  // warm was the engine's one importer there. The entry fell by 13,554 bytes when it stopped
  // importing it (measured 2026-11-15, dispatch://INFRA-1109). Every surface that draws a body
  // imports the engine itself, from a route or a host that is a chunk of its own already, so a
  // page that draws a body has the engine's chunk come down with that chunk, whatever this warm
  // does.
  //
  // What a reader loses by its being late: nothing they can point at. The warm now waits on one
  // more download, the engine module's own, which the chunk of a page that draws a body brings
  // down with it anyway - on a cold landing beside the landing read (`app.tsx`,
  // `preloadRouteChunk`) - and no body draws before that read has answered.
  //
  // Under `prefetchChunk`, the engine's own downloads - the headless editor and its serializer -
  // as well as its module: nobody is waiting on this warm. A body that draws awaits the same load
  // under `renderChunk` (`features/refs/markdown-engine.ts`, `withEngine`), and so does a past
  // version's change marks (`features/doc/VersionView.tsx`), and a failure while either waits is
  // the one that reloads. The warm ran outside the prefetch until dispatch://INFRA-1109, as it
  // had when this module imported the engine statically, which left those downloads under
  // neither function (`chunk-loads.ts`).
  //
  // A warm that fails reloads nothing and keeps nothing: the next body drawn asks again
  // (`features/refs/markdown-engine.ts`, `loadEngine`; `e2e/shell.e2e.ts`).
  useEffect(() => {
    const timer = window.setTimeout(() => {
      prefetchChunk(() =>
        import("../refs/markdown-engine").then((markdownEngine) =>
          markdownEngine.warmMarkdownRenderer()
        )
      );
    }, 0);
    return () => window.clearTimeout(timer);
  }, []);

  useEffect(() => {
    const checkForUpdate = () => {
      const now = Date.now();
      if (now - lastVersionCheckAt.current < VERSION_CHECK_INTERVAL_MS) {
        return;
      }
      lastVersionCheckAt.current = now;
      void deploymentChanged()
        .then((changed) => {
          if (changed && !dismissed.current) {
            setUpdateAvailable(true);
          }
        })
        .catch(() => undefined);
    };

    window.addEventListener("focus", checkForUpdate);
    return () => window.removeEventListener("focus", checkForUpdate);
  }, []);

  if (!updateAvailable) {
    return null;
  }

  return (
    <section
      aria-label="A new version is available"
      className={`fixed right-4 bottom-[calc(var(--foot-page-inset,0px)_+_1rem)] left-4 z-50 mx-auto flex max-w-md items-center justify-between gap-3 rounded-xl border p-3 shadow-lg ${card} ${borderStrong}`}
      ref={bannerRef}
      role="status"
    >
      <p className={`text-sm ${textSecondaryOnSurface}`}>A new version is available - Reload</p>
      <div className="flex shrink-0 items-center gap-2">
        <button
          className={`rounded-lg px-3 py-1.5 text-sm font-semibold ${primaryButtonBg} ${primaryButtonHoverBg}`}
          onClick={() => window.location.reload()}
          type="button"
        >
          Reload
        </button>
        <button
          aria-label="Dismiss update notice"
          className={`rounded-lg p-1.5 text-sm ${dismissButtonText} focus-visible:ring-2 ${focusVisibleRing}`}
          onClick={() => {
            dismissed.current = true;
            setUpdateAvailable(false);
          }}
          type="button"
        >
          Dismiss
        </button>
      </div>
    </section>
  );
}
