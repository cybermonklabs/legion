import { useQuery } from "@tanstack/react-query";
import { useCallback, useSyncExternalStore } from "react";

import { whoAmIQuery } from "../../api/queries";
import { userPreferenceStorageKey } from "../shell/userPreference";

/** Which page an issue's address draws for the reader: `outline` is the new issue page, `tabs`
 *  is today's. */
export type IssuePageChoice = "outline" | "tabs";

/** Every caller mounted now, each told when the stored choice may have changed. */
const listeners = new Set<() => void>();

function notify(): void {
  for (const listener of listeners) listener();
}

/** Adds a caller. While any caller is mounted the window's `storage` event tells them all,
 *  which is how a choice made in another tab of this browser reaches this one. */
function subscribe(listener: () => void): () => void {
  if (listeners.size === 0) window.addEventListener("storage", notify);
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
    if (listeners.size === 0) window.removeEventListener("storage", notify);
  };
}

/** Where this browser keeps `login`'s choice. */
function storageKey(login: string): string {
  return userPreferenceStorageKey(login, "issue.page");
}

/** The stored choice of `login`. Nobody signed in, nothing stored, and a stored word that is
 *  neither page's all read as today's page. */
function storedChoice(login: string | undefined): IssuePageChoice {
  if (login === undefined) return "tabs";
  return window.localStorage.getItem(storageKey(login)) === "outline" ? "outline" : "tabs";
}

/**
 * The reader's issue page and the one way to change it, kept in this browser for the signed-in
 * login (`dispatch.issue.page:<login>`); Dispatch holds nothing of it.
 *
 * This is not `useUserPreference`: that hook keeps a state of its own in each caller, read when
 * the caller mounts or the login changes and at no other time, so two callers would disagree
 * until one of them mounted again, and a change made in another tab would reach neither. Here
 * every caller reads one store for the tab on each render, a write tells every caller at once,
 * and the window's `storage` event brings another tab's write. Nothing else reads the stored
 * value: whatever needs the choice calls this hook.
 *
 * With nobody signed in the choice is `tabs` and a write stores nothing. A write the browser
 * refuses (private mode, a full quota) changes nothing, as `recordRecent` in
 * `features/search/recents.ts` does.
 */
export function useIssuePageChoice(): [IssuePageChoice, (next: IssuePageChoice) => void] {
  const login = useQuery(whoAmIQuery()).data?.login;
  const choice = useSyncExternalStore(subscribe, () => storedChoice(login));
  const choose = useCallback(
    (next: IssuePageChoice) => {
      if (login === undefined) return;
      try {
        window.localStorage.setItem(storageKey(login), next);
      } catch {
        // The browser kept nothing, so every caller keeps the page it has.
        return;
      }
      notify();
    },
    [login]
  );
  return [choice, choose];
}
