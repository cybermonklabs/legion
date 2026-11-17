import { type Dispatch, type SetStateAction, useCallback, useSyncExternalStore } from "react";

/** One keyed store of unsent text, and the ways its callers reach it. */
export interface DraftStore {
  /** The key's draft, read without subscribing to it. */
  read(key: string): string;
  /** Forgets every draft, and whose they were. The unit-test preload calls it after each test,
   *  since the store outlives any one render. */
  reset(): void;
  /** Writes the key's draft - the text, or an updater, which reads the draft as it is now - and
   *  tells every field holding that key; `""` forgets the draft. */
  set(key: string, next: SetStateAction<string>): void;
  /** Who is signed in now: `login`, or no one (`undefined`: the session lapsed). A draft is the
   *  person's who typed it, and the store outlives the shell, which another person signed in
   *  from another tab arrives in: their sign-in forgets every draft, as it drops every write not
   *  theirs (`Writes.signedIn`). A lapse keeps the drafts for the sign-in that follows, and the
   *  same person's keeps them on. */
  signedIn(login: string | undefined): void;
  /** The key's draft and its setter; the component re-renders whenever any field for that key
   *  changes it. An updater reads the draft as it is now, so a text added when an upload lands
   *  never overwrites what another upload or a keystroke put there since the render that
   *  started it. */
  useDraft(key: string): [string, Dispatch<SetStateAction<string>>];
}

/**
 * A store of unsent text, keyed by what the text belongs to (an ask id): the draft belongs to
 * that thing rather than to the field showing it, so every field for one key reads and writes
 * the same draft, a field that remounts picks it back up, and the draft outlives every render.
 *
 * Each of the app's draft stores is one of these - the answer drafts (`answer-drafts.ts`), the
 * reply drafts (`reply-drafts.ts`), and the two the composers' unsent drafts keep for each query
 * client (`unsent-drafts.ts`) - so a fix to the subscription, the listener cleanup, the updater or
 * the sign-in reaches every one.
 */
export function createDraftStore(): DraftStore {
  const drafts = new Map<string, string>();
  const listeners = new Map<string, Set<() => void>>();
  /** The person signed in when the drafts were typed: the last sign-in that named someone. */
  let owner: string | undefined;

  const set = (key: string, next: SetStateAction<string>): void => {
    const current = drafts.get(key) ?? "";
    const text = typeof next === "function" ? next(current) : next;
    if (current === text) return;
    if (text === "") drafts.delete(key);
    else drafts.set(key, text);
    for (const listener of listeners.get(key) ?? []) listener();
  };

  function useDraft(key: string): [string, Dispatch<SetStateAction<string>>] {
    const subscribe = useCallback(
      (listener: () => void) => {
        const keyListeners = listeners.get(key) ?? new Set<() => void>();
        listeners.set(key, keyListeners);
        keyListeners.add(listener);
        return () => {
          keyListeners.delete(listener);
          if (keyListeners.size === 0) listeners.delete(key);
        };
      },
      [key]
    );
    const draft = useSyncExternalStore(subscribe, () => drafts.get(key) ?? "");
    const setDraft = useCallback((next: SetStateAction<string>) => set(key, next), [key]);
    return [draft, setDraft];
  }

  const reset = (): void => {
    for (const key of [...drafts.keys()]) set(key, "");
    owner = undefined;
  };

  return {
    read: (key) => drafts.get(key) ?? "",
    reset,
    set,
    signedIn: (login) => {
      if (login === undefined) return;
      if (owner !== undefined && owner !== login) reset();
      owner = login;
    },
    useDraft,
  };
}
