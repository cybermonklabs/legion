import { useQuery } from "@tanstack/react-query";
import { useCallback, useEffect, useMemo, useRef, useState, useSyncExternalStore } from "react";

import { whoAmIQuery } from "../../api/queries";

/** A name a browser keeps something under for one signed-in person: a choice they made, and the
 *  bar's recent items (`features/search/recents.ts`), which are theirs the same way. The Inbox's
 *  three are the ones that are the person's rather than the list's: how a thread's replies are
 *  ordered, how wide the reading pane is, and which bands they folded shut
 *  (`inbox.folded-groups`, one set per grouping). Its filters, sort, grouping and the switch
 *  between the Inbox and what they have dismissed out of it are in the address instead, so a
 *  link carries them. The Issues tab keeps the views the person saved, each project's under its
 *  key (`project.saved-views`, read by `readSavedViews` in `features/project/issues-views.ts`),
 *  and whether Time per phase draws each phase's share or its hours (`project.phase-hours`:
 *  `share` or `hours`). */
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
  | "project.phase-hours"
  | "project.saved-views"
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

/**
 * Reads a browser preference for a resolved signed-in identity. Pending and failed identity use
 * the caller's explicit fallback when one differs from `read(null)`; neither state persists.
 * `read` runs when the hook mounts and when the signed-in identity or `preference` changes, and
 * at no other time: a `read` that consults other state sees that state as it was then. A write
 * whose value is the one already stored is not made, so a caller whose `read` folds several
 * stored values into one (the Board's panel, shut while no filter is on whatever is stored)
 * never writes a value over itself. A value that two tabs of the browser each change a part of
 * is `useStoredUserPreference`'s instead.
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
  const [value, setValue] = useState(() =>
    preferenceValue(identity, login, preference, readRef.current, optionsRef.current)
  );
  useEffect(() => {
    setValue(preferenceValue(identity, login, preference, readRef.current, optionsRef.current));
  }, [identity, login, preference]);
  const setAndPersist = useCallback(
    (next: T) => {
      setValue(next);
      if (login === undefined) return;
      const key = userPreferenceStorageKey(login, preference);
      const stored = write(next);
      if (window.localStorage.getItem(key) !== stored) window.localStorage.setItem(key, stored);
    },
    [login, preference, write]
  );
  return [value, setAndPersist];
}

/** Every caller of `useStoredUserPreference` mounted now, each told when what is stored may have
 *  changed. */
const storeListeners = new Set<() => void>();

function notifyStore(): void {
  for (const listener of storeListeners) listener();
}

/** Adds a caller. While any caller is mounted the window's `storage` event tells them all, which
 *  is how a write in another tab of this browser reaches this one: the browser tells every tab
 *  but the one that wrote. */
function subscribeStore(listener: () => void): () => void {
  if (storeListeners.size === 0) window.addEventListener("storage", notifyStore);
  storeListeners.add(listener);
  return () => {
    storeListeners.delete(listener);
    if (storeListeners.size === 0) window.removeEventListener("storage", notifyStore);
  };
}

/**
 * A browser preference for the signed-in login that every caller reads as it is stored now: a
 * value that two tabs of the browser each change a part of, such as `project.saved-views`, which
 * holds the views the reader saves in whichever tab they are on. `useUserPreference` keeps a copy
 * of its own in each caller, read once, so a write there puts back that copy whole and drops what
 * another tab wrote since. Here every caller reads the store on each render, a write tells every
 * caller in this tab at once, and the window's `storage` event brings another tab's write. A
 * change is handed the value as it is stored at the moment it writes, so it changes only its own
 * part. A write whose value is the one already stored is not made, and a write the browser refuses
 * (private mode, a full quota) changes nothing, as `recordRecent` in `features/search/recents.ts`
 * does. With no signed-in login the value is `read(null)` and a change writes nothing. Pass a
 * module-level `read`: the value is read again each time `read` is a new function.
 */
export function useStoredUserPreference<T>(
  preference: UserPreference,
  read: (stored: string | null) => T,
  write: (value: T) => string
): [T, (change: (stored: T) => T) => void] {
  const login = useQuery(whoAmIQuery()).data?.login;
  const key = login === undefined ? undefined : userPreferenceStorageKey(login, preference);
  const stored = useSyncExternalStore(subscribeStore, () =>
    key === undefined ? null : window.localStorage.getItem(key)
  );
  const value = useMemo(() => read(stored), [read, stored]);
  const change = useCallback(
    (next: (stored: T) => T) => {
      if (key === undefined) return;
      const now = window.localStorage.getItem(key);
      const written = write(next(read(now)));
      if (written === now) return;
      try {
        window.localStorage.setItem(key, written);
      } catch {
        // The browser kept nothing, so every caller keeps the value it has.
        return;
      }
      notifyStore();
    },
    [key, read, write]
  );
  return [value, change];
}
