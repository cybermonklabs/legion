import { createContext, useContext } from "react";

/**
 * What a chosen entry of the Contents list does to the box the list is drawn in: the Outline
 * sheet closes, and the pane, which stays, does nothing (`contents-frame.tsx` provides it). It
 * is a file of its own, importing no slot, because the list that calls it is a slot
 * (`slots/contents.tsx`) and the frame that provides it draws that slot.
 */
export const ContentsChosen = createContext<() => void>(() => {});

/** The function the Contents list calls when the reader chooses an entry (`slots/contents.tsx`):
 *  it closes the Outline sheet the list is drawn in, and does nothing in the pane. */
export function useContentsChosen(): () => void {
  return useContext(ContentsChosen);
}
