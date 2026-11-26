import { useLocation } from "react-router-dom";

/**
 * The following of the address: one value for each time an address is followed, which the stage,
 * each view's slot and the outline decide what an address lands on by, once each. It is the
 * location's key together with its address. The router gives each entry it makes a key of its
 * own, but every entry the browser makes - a load, a fragment typed in the address bar - the one
 * key "default", so the key alone does not tell two such followings apart.
 *
 * The words the filter by words types are no part of a following (part N's R4, decision 375):
 * the box writes `?q=` in place, by a replace, to which the router gives a key of its own, so the
 * box marks each entry it writes with the following it was typed in (`words` in the location's
 * state, `view-row-tools.tsx`), and that entry is the same following. A link or Back to an
 * address whose `?q=` differs carries no such mark, and is a following of its own.
 *
 * It is a module of its own so the outline's slot can read it too: the stage imports that slot
 * (`./body`), which could not import the stage back.
 */
export function useFollowing(): string {
  const { hash, key, pathname, search, state } = useLocation();
  return stateString(state, "words") ?? `${key} ${pathname}${search}${hash}`;
}

/** The string the location's `state` holds under `field`, or undefined where it holds none: the
 *  following a write of the filter by words marked its entry with (`words`, none for an entry the
 *  box did not write), and the address Documents' list or a document was opened from (`outline`,
 *  `document-address.ts`). */
export function stateString(state: unknown, field: "outline" | "words"): string | undefined {
  if (typeof state !== "object" || state === null || !(field in state)) return undefined;
  const value: unknown = Reflect.get(state, field);
  return typeof value === "string" ? value : undefined;
}
