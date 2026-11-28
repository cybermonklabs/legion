/**
 * The Web Worker that cuts the room's first sync into parts for `splitSyncInWorker`
 * (`sync-parts.ts`), off the page's thread. The page creates it with
 * `new Worker(new URL(…), { type: "module" })` for one first sync, posts it one `SplitRequest`,
 * and ends it once it has answered with the `SplitAnswer`, whose parts' buffers it hands over
 * rather than copies. A cut that throws is thrown rather than answered, so the worker's `error`
 * reaches the page, which then cuts the update itself and hears the same failure.
 */
import { type SplitAnswer, type SplitRequest, splitSync } from "./sync-parts";

/** The dedicated worker's own scope: `self`, typed as far as this module uses it (the SPA's
 *  TypeScript program carries the DOM library, not the worker one). */
interface WorkerScope {
  addEventListener(type: "message", listener: (event: MessageEvent<SplitRequest>) => void): void;
  postMessage(message: SplitAnswer, transfer: ArrayBufferLike[]): void;
}

// A dedicated worker's global is a `WorkerGlobalScope`, which a page does not expose, so nothing
// listens where this module is imported for its types alone.
if ("WorkerGlobalScope" in globalThis) {
  const scope = globalThis as unknown as WorkerScope;
  scope.addEventListener("message", (event) => {
    const parts = splitSync(event.data.update, event.data.maxStructs);
    scope.postMessage(
      { parts },
      parts.map((part) => part.buffer)
    );
  });
}
