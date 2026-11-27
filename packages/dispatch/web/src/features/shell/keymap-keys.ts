import { useContext, useMemo, useSyncExternalStore } from "react";

import { KeymapPersonContext } from "./keymap";
import {
  catalogueEntry,
  isKeymapAction,
  type KeymapActionId,
  keysOfAction,
  NO_PERSON_KEYS,
  type PersonKeys,
} from "./keymap-catalogue";
import { refusalOf } from "./keymap-refusal";
import { preferenceStore, userPreferenceStorageKey } from "./userPreference";

/**
 * The keys a person gave the actions of the keymap (R14), kept in this browser for the signed-in
 * login (`dispatch.shell.keymap:<login>`) as one map of the actions they changed and only those:
 * `{"inbox.snooze": ["Shift+B", "h"], "inbox.copy-ref": []}`. Dispatch holds nothing of it.
 *
 * This is not `useUserPreference`, for the reason `issue-page/choice.ts` gives: every reader reads
 * one store for the tab, a write tells every reader at once, and the window's `storage` event
 * brings another tab's write, so a change takes effect at once in each open tab of the browser.
 * `KeymapProvider` reads it and hands it to the registry through `KeymapPersonContext`.
 */

/** The readers of the stored keys, each told when they may have changed: by a write here, or by
 *  another tab of this browser. */
const { notify, subscribe } = preferenceStore();

/** Where this browser keeps `login`'s keys. */
function storageKey(login: string): string {
  return userPreferenceStorageKey(login, "shell.keymap");
}

/**
 * The keys a stored map gives, less every key the refusal check refuses now: an action a later
 * build renamed or removed, a key that is no key, and a key that another action has taken since
 * (a default this build added, or a map another tab wrote under an older build). Without this a
 * stale map would register a prefix, which throws in development (`appKeymap`'s `strict`). Each
 * key is checked against the whole stored map, so dropping one only ever takes keys away.
 */
function keptKeys(stored: string): PersonKeys {
  let read: unknown;
  try {
    read = JSON.parse(stored);
  } catch {
    return NO_PERSON_KEYS;
  }
  if (typeof read !== "object" || read === null || Array.isArray(read)) return NO_PERSON_KEYS;
  const named: Partial<Record<KeymapActionId, readonly string[]>> = {};
  for (const [action, keys] of Object.entries(read)) {
    if (
      isKeymapAction(action) &&
      Array.isArray(keys) &&
      keys.every((key) => typeof key === "string")
    ) {
      named[action] = keys;
    }
  }
  const kept: Partial<Record<KeymapActionId, readonly string[]>> = {};
  for (const [action, keys] of Object.entries(named) as [KeymapActionId, readonly string[]][]) {
    const free: string[] = [];
    for (const key of keys) {
      if (refusalOf(action, [...free, key], named).kind === "accepted") free.push(key);
    }
    kept[action] = free;
  }
  return kept;
}

/** The last stored text read and the keys it gives: one stored string is parsed and checked
 *  once, and every read of the same string answers the same object, which is what
 *  `useSyncExternalStore` needs of a snapshot. */
let parsed: { readonly keys: PersonKeys; readonly stored: string } | undefined;

/** The keys `login` changed, as this browser keeps them; none for nobody signed in, and none
 *  where the browser refuses the read. */
export function storedPersonKeys(login: string | undefined): PersonKeys {
  if (login === undefined) return NO_PERSON_KEYS;
  let stored: string | null;
  try {
    stored = window.localStorage.getItem(storageKey(login));
  } catch {
    return NO_PERSON_KEYS;
  }
  if (stored === null) return NO_PERSON_KEYS;
  if (parsed?.stored !== stored) parsed = { keys: keptKeys(stored), stored };
  return parsed.keys;
}

/** `login`'s keys, read again whenever this tab or another one of the browser writes them. */
export function usePersonKeys(login: string | undefined): PersonKeys {
  return useSyncExternalStore(subscribe, () => storedPersonKeys(login));
}

/** The keys that run `action` for the signed-in person: theirs, else the defaults, then the keys
 *  nobody changes. A control draws its action's keys from it, so it shows the person's own. With
 *  no `KeymapProvider`, the defaults. */
export function useActionKeys(action: KeymapActionId): readonly string[] {
  const { personKeys } = useContext(KeymapPersonContext);
  return useMemo(() => keysOfAction(action, personKeys), [action, personKeys]);
}

/** Stores `next` for `login`, or forgets the map where it changes nothing, and tells every
 *  reader. A write the browser refuses (private mode, a full quota) changes nothing, as
 *  `choice.ts`'s does. */
function store(login: string, next: PersonKeys): void {
  try {
    if (Object.keys(next).length === 0) window.localStorage.removeItem(storageKey(login));
    else window.localStorage.setItem(storageKey(login), JSON.stringify(next));
  } catch {
    return;
  }
  notify();
}

/**
 * Gives `action` the keys `keys` for `login`, or its defaults again where `keys` is undefined.
 * Keys equal to the defaults are stored as none changed. The caller asks `refusalOf` first: a
 * refused key is dropped on the next read.
 */
export function setActionKeys(
  login: string,
  action: KeymapActionId,
  keys: readonly string[] | undefined
): void {
  const { [action]: _replaced, ...others } = storedPersonKeys(login);
  const defaults = catalogueEntry(action).keys;
  const isDefault =
    keys === undefined ||
    (keys.length === defaults.length && keys.every((key, index) => key === defaults[index]));
  store(login, isDefault ? others : { ...others, [action]: [...keys] });
}

/** Gives every action its defaults again for `login`. */
export function resetAllKeys(login: string): void {
  store(login, NO_PERSON_KEYS);
}
