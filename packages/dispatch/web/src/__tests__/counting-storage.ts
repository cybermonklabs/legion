/**
 * A stand-in for the page's `localStorage` that keeps every write made to it, in place of the
 * window's own until `restore` puts that back.
 *
 * happy-dom's `Storage` cannot be watched where it is. It is a proxy that hands out bound methods
 * of its own, so a `spyOn` of its `setItem` sees none of the page's calls; a method assigned over
 * `setItem` is stored as an item; and one defined over it stays on the one storage every later
 * test of the process reads. So the writes are kept by a storage of the test's own: the items in
 * a `Map`, behind the three methods the page calls. `window` is the global object here
 * (`register-dom.ts`), so a bare `localStorage` reads the stand-in too.
 */

export interface CountingStorage {
  /** Every `setItem` the page made, in order, as its key and its value. */
  readonly writes: readonly (readonly [string, string])[];
  /** Puts the window's own storage back. */
  readonly restore: () => void;
}

/** Swaps the window's storage for a counting one that starts out holding `items`. */
export function installCountingStorage(
  items: Readonly<Record<string, string>> = {}
): CountingStorage {
  const stored = new Map(Object.entries(items));
  const writes: [string, string][] = [];
  const storage = {
    getItem: (key: string) => stored.get(key) ?? null,
    removeItem: (key: string) => {
      stored.delete(key);
    },
    setItem: (key: string, value: string) => {
      writes.push([key, value]);
      stored.set(key, value);
    },
  };
  const own = Object.getOwnPropertyDescriptor(window, "localStorage");
  Object.defineProperty(window, "localStorage", { configurable: true, value: storage });
  return {
    restore: () => {
      if (own !== undefined) Object.defineProperty(window, "localStorage", own);
      else Reflect.deleteProperty(window, "localStorage");
    },
    writes,
  };
}
