import { useSyncExternalStore } from "react";

/**
 * Whether the page a cold landing opened has drawn its own content yet, for the shell's code
 * that draws no part of that content: the margin, the reference-preview layer and the map from a
 * live event to the reads it refreshes. One more reader waits for the same moment for another
 * reason: the ring on a landed artifact's header (`features/artifacts/ArtifactHeader.tsx`)
 * counts its two seconds from it, because the margin comes in after it and the page is still
 * changing until then.
 *
 * WHY "AFTER THE FIRST PAINT" WAS NOT ENOUGH. Each of those starts its download after the
 * shell's first paint (an effect, or a render that follows one). The shell paints as soon as the
 * landing read answers, but a document draws only when its text has been read again and its
 * live connection has synced (`features/doc/ProofDocument.tsx`, `admit`). On a cold
 * `/issues/CORE-1` the shell's effects asked for the margin at 900 ms and for the event map at
 * 996 ms; the first block of the Spec drew at 1,767 ms. So "after the first paint" was still
 * before the content: those two chunks, 53,544 bytes, came down and were parsed while the reader
 * waited for the document. Measured with the R13 reading of dispatch://INFRA-1109 (Resource
 * Timing of `/assets/*.js` against the first block's insertion) at that issue's head 90ee14dd.
 *
 * The search bar is not held, and `features/search/SearchPaletteHost.tsx` says why: a bar that
 * arrives after the key that opened it opens with its row half out of view.
 *
 * THE RULE. A landing whose page draws a live document holds these until the document's first
 * block is in the page (`holdUntilContentDraws`, started from the address alone by
 * `preloadRouteChunk` in `app.tsx`, before React renders). Every other landing holds nothing,
 * and its shell starts them after its first paint, as before. The hold ends at the first of:
 *
 * - the live document drew, or it cannot draw: a failed admission, a failed load, or the
 *   document left the page (`contentDrawn`, from `ProofDocument.tsx`); the issue page and the
 *   document page end it too when their reads say they draw no document at all;
 * - the reader pressed a key or a pointer anywhere. That is the reader asking the page for
 *   something, and what they ask for may be the margin or a reference's card;
 * - `HOLD_LIMIT_MS` passed since the hold began. That is for a document that neither draws nor
 *   fails - a socket that stays "connecting" - so that a stuck document never also stops the
 *   page's live refresh and its margin.
 *
 * So no page waits forever. What a page loses while the hold is on is the live refresh: an event
 * that arrives is kept, not dropped (`api/event-feed.ts`), and its refresh runs when the hold
 * ends.
 *
 * The hold ends one frame after `contentDrawn`, not at the call: the document's first block is
 * in the DOM then but not yet painted, and the downloads would otherwise go out in the same task
 * as that paint. This is the same "a timeout set inside a frame callback" rule as
 * `features/inbox/InboxHost.tsx`, `usePeekChunkAfterFirstPaint`.
 */

/**
 * The longest a hold lasts. Over twice the slowest first block measured on a cold
 * `/issues/CORE-1`: 4,451 ms, with a run of 1,475 unit tests beside the browser on the same
 * machine (the R13 reading of dispatch://INFRA-1109; the same reading on an idle machine drew it
 * at 1,767 ms). A document slower than this still draws; the shell's extras only stop waiting
 * for it.
 */
export const HOLD_LIMIT_MS = 10_000;

let held = false;
let releasing = false;
let limit: number | undefined;
const listeners = new Set<() => void>();

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

function release(): void {
  if (!held) {
    return;
  }
  held = false;
  releasing = false;
  window.clearTimeout(limit);
  limit = undefined;
  window.removeEventListener("pointerdown", contentDrawn, true);
  window.removeEventListener("keydown", contentDrawn, true);
  for (const listener of [...listeners]) listener();
}

/** Holds the shell's extras until the landed page's live document has drawn. Called once, from
 *  the landed address, before React renders (`app.tsx`, `preloadRouteChunk`). */
export function holdUntilContentDraws(): void {
  if (held) {
    return;
  }
  held = true;
  // Captured on the window (the `true`), so a handler that stops a press from propagating cannot
  // hide it. The short form of `{ capture: true }` is the one used, because this module is in the
  // entry and so in what every landing loads before its first block.
  window.addEventListener("pointerdown", contentDrawn, true);
  window.addEventListener("keydown", contentDrawn, true);
  limit = window.setTimeout(release, HOLD_LIMIT_MS);
}

/** The landed page's content is in the page, or it will not be: ends the hold one frame later.
 *  Does nothing where nothing is held, which is every page after the first, and every landing
 *  whose page draws no live document. */
export function contentDrawn(): void {
  if (!held || releasing) {
    return;
  }
  releasing = true;
  // A runtime with no frames - a test's document-less one - ends the hold at once, since there
  // is no paint there to wait for.
  if (typeof requestAnimationFrame !== "function") {
    release();
    return;
  }
  requestAnimationFrame(() => {
    window.setTimeout(release, 0);
  });
}

/** Whether the shell may start its extras now: true unless a hold is still on. */
export function useContentDrawn(): boolean {
  return useSyncExternalStore(
    subscribe,
    () => !held,
    () => true
  );
}

/** Resolves once no hold is on: at once where none is. */
export function whenContentDrawn(): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  if (!held) {
    resolve();
    return promise;
  }
  const listener = () => {
    listeners.delete(listener);
    resolve();
  };
  listeners.add(listener);
  return promise;
}
