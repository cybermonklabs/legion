import { useLocation } from "react-router-dom";

/**
 * The following of the address: one value for each time an address is followed, which the stage,
 * each view's slot and the outline decide what an address lands on by, once each. It is the
 * location's key together with its address. The router gives each entry it makes a key of its
 * own, but every entry the browser makes - a load, a fragment typed in the address bar - the one
 * key "default", so the key alone does not tell two such followings apart.
 *
 * It is a module of its own so the outline's slot can read it too: the stage imports that slot
 * (`./body`), which could not import the stage back.
 */
export function useFollowing(): string {
  const { hash, key, pathname, search } = useLocation();
  return `${key} ${pathname}${search}${hash}`;
}
