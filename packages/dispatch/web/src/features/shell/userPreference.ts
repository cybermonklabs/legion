import { useQuery } from "@tanstack/react-query";
import { useCallback, useRef, useSyncExternalStore } from "react";

import { whoAmIQuery } from "../../api/queries";

/** A name a browser keeps something under for one signed-in person: a choice they made, and the
 *  bar's recent items (`features/search/recents.ts`), which are theirs the same way. The Inbox's
 *  three are the ones that are the person's rather than the list's: how a thread's replies are
 *  ordered, how wide the reading pane is, and which bands they folded shut
 *  (`inbox.folded-groups`, one set per grouping). Its filters, sort, grouping and the switch
 *  between the Inbox and what they have dismissed out of it are in the address instead, so a
 *  link carries them. */
export type UserPreference =
  | "inbox.folded-groups"
  | "inbox.pane-width"
  | "inbox.thread-order"
  /** Whether this reader keeps today's issue header's Details open. */
  | "issue.details"
  /** Which of the new issue page's Details sections this reader has closed. */
  | "issue.margin-sections"
  | "issue.page"
  | "project.board-edges"
  | "project.board-lanes"
  | "project.issue-filters"
  | "project.issue-view"
  | "search.recents"
  | "shell.margin"
  | "shell.margin-width"
  | "shell.sidebar";

/** Browser-only values are private to the signed-in Dispatch identity. */
export function userPreferenceStorageKey(login: string, preference: UserPreference): string {
  return `dispatch.${preference}:${login}`;
}

/**
 * Where this browser kept the Agents page's pins before they became the person's own, held by
 * Dispatch (INFRA-812). Nothing writes it any more, so it is no `UserPreference`: the shell
 * reads it once at the first load after the change, writes each id it holds as a pin and
 * removes it. The format is the one `userPreferenceStorageKey` wrote, frozen as it was written.
 */
export function legacyPinnedAgentsKey(login: string): string {
  return `dispatch.agents.pinned:${login}`;
}

type UserPreferenceIdentity = "failed" | "pending" | "resolved";

interface UserPreferenceOptions<T> {
  failed?: T;
  pending?: T;
}

function preferenceValue<T>(
  identity: UserPreferenceIdentity,
  login: string | undefined,
  preference: UserPreference,
  read: (stored: string | null) => T,
  options: UserPreferenceOptions<T> | undefined
): T {
  const fallback = read(
    identity === "resolved" && login !== undefined
      ? window.localStorage.getItem(userPreferenceStorageKey(login, preference))
      : null
  );
  if (identity === "resolved") return fallback;
  return identity === "pending" ? (options?.pending ?? fallback) : (options?.failed ?? fallback);
}

/** The last thing that moved the readers of one key. A write in this tab gives each reader the
 *  value written. Text another tab stored is run through each reader's own `read`. Each change is
 *  a new object, so a reader knows a change it has not taken by its identity. */
type PreferenceChange =
  | { readonly kind: "written"; readonly value: unknown }
  | { readonly kind: "stored"; readonly text: string | null };

/** The readers of one key that are mounted now, and the last change of that key. */
interface PreferenceStore {
  change: PreferenceChange | undefined;
  readonly listeners: Set<() => void>;
  /** Whether the browser stores the key, so that another tab's write can reach it. A reader with
   *  no signed-in login reads a key that nothing stores. */
  readonly stored: boolean;
}

/** One store for each key that a mounted reader reads or that a write was made to. */
const stores = new Map<string, PreferenceStore>();

/** How many readers are mounted now, of every key. The window's `storage` listener is on while
 *  there is one. */
let mountedReaders = 0;

function storeOf(key: string, stored: boolean): PreferenceStore {
  let store = stores.get(key);
  if (store === undefined) {
    store = { change: undefined, listeners: new Set(), stored };
    stores.set(key, store);
  }
  return store;
}

function publish(store: PreferenceStore, change: PreferenceChange): void {
  store.change = change;
  for (const listener of store.listeners) listener();
}

/** Another tab stored text under a key, or cleared every key (`key` is null then). Every reader
 *  of that key runs its own `read` over the text. */
function onStorage(event: StorageEvent): void {
  for (const [key, store] of stores) {
    if (!store.stored) continue;
    if (event.key === null) publish(store, { kind: "stored", text: null });
    else if (event.key === key) publish(store, { kind: "stored", text: event.newValue });
  }
}

function subscribe(key: string, stored: boolean, listener: () => void): () => void {
  const store = storeOf(key, stored);
  store.listeners.add(listener);
  mountedReaders += 1;
  if (mountedReaders === 1) window.addEventListener("storage", onStorage);
  return () => {
    store.listeners.delete(listener);
    if (store.listeners.size === 0 && stores.get(key) === store) stores.delete(key);
    mountedReaders -= 1;
    if (mountedReaders === 0) window.removeEventListener("storage", onStorage);
  };
}

/** What one reader holds: its value, the key it holds it for, and the last change of that key
 *  it has taken. */
interface Held<T> {
  readonly change: PreferenceChange | undefined;
  readonly key: string;
  readonly value: T;
}

/**
 * Reads a browser preference for a resolved signed-in identity. Pending and failed identity use
 * the caller's explicit fallback when one differs from `read(null)`; neither state persists.
 *
 * Every reader of one key reads one store for this tab. A write gives every mounted reader of
 * the key the value written, at once, with no reader mounted again. No `read` runs then, so a
 * reader whose `read` is not the inverse of `write` holds the value written. A write reaches no
 * reader of another key. When another tab stores the key, the window's `storage` event runs each
 * reader's own `read` over the text that tab stored.
 *
 * A reader runs its own `read` when it mounts, when the signed-in identity or `preference`
 * changes, and when another tab stores its key, and at no other time. So its own write never
 * reads again: the Board's filter strip, whose `read` shuts it while no filter is on, stays open
 * when the reader opens it. A `read` that consults other state sees that state as it was then.
 *
 * A write whose value is the one already stored is not made, so a caller whose `read` folds
 * several stored values into one never writes a value over itself. With no signed-in login a
 * write stores nothing and reaches the readers of this tab alone. A write the browser refuses
 * throws from `setItem` and changes nothing.
 */
export function useUserPreference<T>(
  preference: UserPreference,
  read: (stored: string | null) => T,
  write: (value: T) => string,
  options?: UserPreferenceOptions<T>
): [T, (next: T) => void] {
  const whoAmI = useQuery(whoAmIQuery());
  const login = whoAmI.data?.login;
  const identity: UserPreferenceIdentity =
    login !== undefined ? "resolved" : whoAmI.isPending ? "pending" : "failed";
  const readRef = useRef(read);
  readRef.current = read;
  const optionsRef = useRef(options);
  optionsRef.current = options;
  // The key every reader of this preference shares: the one the browser stores it under, or,
  // with no login, one for the identity's state that nothing stores.
  const stored = login !== undefined;
  const key = stored ? userPreferenceStorageKey(login, preference) : `${identity}:${preference}`;
  const held = useRef<Held<T>>(undefined);
  const subscribeKey = useCallback(
    (listener: () => void) => subscribe(key, stored, listener),
    [key, stored]
  );
  const value = useSyncExternalStore(subscribeKey, () => {
    const change = stores.get(key)?.change;
    let current = held.current;
    if (current === undefined || current.key !== key) {
      const first = preferenceValue(
        identity,
        login,
        preference,
        readRef.current,
        optionsRef.current
      );
      current = { change, key, value: first };
    } else if (change !== undefined && change !== current.change) {
      current = {
        change,
        key,
        value: change.kind === "written" ? (change.value as T) : readRef.current(change.text),
      };
    }
    held.current = current;
    return current.value;
  });
  const setAndPersist = useCallback(
    (next: T) => {
      if (login !== undefined) {
        const text = write(next);
        if (window.localStorage.getItem(key) !== text) window.localStorage.setItem(key, text);
      }
      publish(storeOf(key, stored), { kind: "written", value: next });
    },
    [key, login, stored, write]
  );
  return [value, setAndPersist];
}
