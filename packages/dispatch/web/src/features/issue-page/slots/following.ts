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

/**
 * What the outline leaves in the history entry of the address it opens in place of one that
 * named an id no read holds (`body.tsx`'s `addressView`): an outline's own address, Conversation's
 * or Graph's, which the page opens there with the focus on no row. The row that address opens on
 * - the open question that waits on the reader, or Graph's first row - is not the node the address
 * named, and a focus there would land the reader on the wrong row (decision 553 (1), root R20).
 */
export interface DroppedAddress {
  readonly droppedAddress: true;
}

/** The mark `body.tsx` leaves on the address it opens in place of one it drops. */
export const DROPPED_ADDRESS: DroppedAddress = { droppedAddress: true };

/** Whether the address's history entry is one the outline opened in place of one it dropped. */
export function useDroppedAddress(): boolean {
  const { state } = useLocation();
  return (
    typeof state === "object" &&
    state !== null &&
    "droppedAddress" in state &&
    state.droppedAddress === true
  );
}
