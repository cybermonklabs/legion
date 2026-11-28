/**
 * A value held outside React's state, which each component that reads it subscribes to on its own
 * (`useSyncExternalStore`): setting it draws again only the components whose own reading of it
 * changed, and nothing above them.
 */
export interface Held<T> {
  readonly get: () => T;
  /** Holds `next` and tells every reader, unless it is the value held already. */
  readonly set: (next: T) => void;
  readonly subscribe: (listener: () => void) => () => void;
}

/** A `Held` value that starts as `initial`. */
export function createHeld<T>(initial: T): Held<T> {
  let value = initial;
  const listeners = new Set<() => void>();
  return {
    get: () => value,
    set: (next) => {
      if (Object.is(next, value)) return;
      value = next;
      for (const listener of listeners) listener();
    },
    subscribe: (listener) => {
      listeners.add(listener);
      return () => {
        listeners.delete(listener);
      };
    },
  };
}
