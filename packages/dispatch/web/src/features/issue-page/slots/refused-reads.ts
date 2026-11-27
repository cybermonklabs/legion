import { useLayoutEffect } from "react";

import type { FailedRead, ReadKey } from "../outline-reads";
import type { ViewSlotProps } from "./stage";

/**
 * How a view drawn in the stage tells it whether it draws the node the address names
 * (`ViewSlotProps.onAddressDrawn`) where a read its tree is built from stands refused, by the
 * rule `StageSlot` gives. Only a read rows come from counts (`FailedRead.placesRows`). One that
 * the view draws no line of its own for is no row of the view: where one of those stands refused
 * once the reads have settled (`settled`, `useOutlineReads`'s `readsSettled`), the view says it
 * draws no node, before the browser paints, and the stage draws the outline in its place with
 * that read's line and Retry. Where only reads the view draws its own line for stand refused
 * (`own`, by the key of each one's line), the view says nothing and keeps its line. Gives whether
 * the view may say what it draws itself: the reads have settled and none rows come from stands
 * refused.
 */
export function useTellsAddressDrawn(
  failed: readonly FailedRead[],
  settled: boolean,
  own: readonly ReadKey[],
  onAddressDrawn: ViewSlotProps["onAddressDrawn"] | undefined
): boolean {
  const refused = failed.filter(({ placesRows }) => placesRows);
  const foreign = refused.some(({ key }) => !own.some((line) => line === key));
  useLayoutEffect(() => {
    if (settled && foreign) onAddressDrawn?.(false);
  }, [foreign, onAddressDrawn, settled]);
  return settled && refused.length === 0;
}
