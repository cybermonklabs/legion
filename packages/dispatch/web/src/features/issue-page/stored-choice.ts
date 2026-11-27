import { useSyncExternalStore } from "react";

import { userPreferenceStorageKey } from "../shell/userPreference";

/** Which page an issue's address draws for the reader: `outline` is the new issue page, `tabs`
 *  is today's. */
export type IssuePageChoice = "outline" | "tabs";

/** Every caller mounted now, each told when the stored choice may have changed. */
const listeners = new Set<() => void>();

/** Tells every caller mounted now that the stored choice may have changed: the window's
 *  `storage` event does it for another tab's write, and this tab's own write does it once it has
 *  stored (`useIssuePageChoice` in `choice.ts`). */
export function issuePageChoiceChanged(): void {
  for (const listener of listeners) listener();
}

/** Adds a caller. While any caller is mounted the window's `storage` event tells them all,
 *  which is how a choice made in another tab of this browser reaches this one. */
function subscribe(listener: () => void): () => void {
  if (listeners.size === 0) window.addEventListener("storage", issuePageChoiceChanged);
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
    if (listeners.size === 0) window.removeEventListener("storage", issuePageChoiceChanged);
  };
}

/** Where this browser keeps `login`'s choice. */
export function issuePageChoiceKey(login: string): string {
  return userPreferenceStorageKey(login, "issue.page");
}

/** The stored choice of `login`. Nobody signed in, nothing stored, and a stored word that is
 *  neither page's all read as today's page. */
function storedChoice(login: string | undefined): IssuePageChoice {
  if (login === undefined) return "tabs";
  return window.localStorage.getItem(issuePageChoiceKey(login)) === "outline" ? "outline" : "tabs";
}

/**
 * The issue page `login` chose, kept in this browser for each login
 * (`dispatch.issue.page:<login>`); Dispatch holds nothing of it.
 *
 * This is not `useUserPreference`: that hook keeps a state of its own in each caller, read when
 * the caller mounts or the login changes and at no other time, so two callers would disagree
 * until one of them mounted again, and a change made in another tab would reach neither. Here
 * every caller reads one store for the tab on each render, a write tells every caller at once,
 * and the window's `storage` event brings another tab's write. Every reader of the choice calls
 * this hook, or `useIssuePageChoice` (`choice.ts`) over it, and the one other read of the stored
 * value is `outlineChosenInThisBrowser`, for the entry's preload.
 *
 * The read is a module of its own, apart from the write, because the shell reads the choice on
 * every page (`app.tsx`) and only Settings changes it: the entry chunk, which every page parses
 * before it draws, carries the read alone.
 */
export function useStoredIssuePageChoice(login: string | undefined): IssuePageChoice {
  return useSyncExternalStore(subscribe, () => storedChoice(login));
}

/**
 * Whether any login's stored choice in this browser is the new issue page. The entry's preload
 * asks it (`app.tsx`, `preloadRouteChunk`), and it runs before the sign-in check has answered, so
 * it cannot know whose choice the page will read. Storage the browser refuses to read holds no
 * choice.
 */
export function outlineChosenInThisBrowser(): boolean {
  // The key of no login is what every login's key starts with.
  const prefix = issuePageChoiceKey("");
  try {
    const storage = window.localStorage;
    for (let index = 0; index < storage.length; index += 1) {
      const key = storage.key(index);
      if (key?.startsWith(prefix) && storage.getItem(key) === "outline") return true;
    }
  } catch {
    // The browser keeps no storage this page may read, so nobody's choice is stored.
  }
  return false;
}
