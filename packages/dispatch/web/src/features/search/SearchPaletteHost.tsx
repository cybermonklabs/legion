import { type ReactNode, useEffect } from "react";

import { askedSurface } from "../shell/asked-surface";
import { chunk } from "../shell/chunk-component";
import { prefetchChunk } from "../shell/chunk-loads";

/**
 * Where the shell places the search and actions bar, and where the bar's code is fetched: after
 * the page it landed on has drawn, never before it.
 *
 * The bar is mounted on every page and closed on all of them until `$mod+k`, `/`, `g p` or the
 * rail's Search control opens it, and it carried about 84,000 bytes of the entry chunk before
 * this split - its own surfaces, the tiered search model, the operators, the offers and the hit
 * list's match ranking. That is bytes every page of the dashboard parsed before it drew
 * anything, for a bar most page loads never open.
 *
 * The bar is drawn through `askedSurface` (`features/shell/asked-surface.tsx`) rather than behind
 * `lazy()` and a `Suspense`. A `lazy()` bar mounts in a commit of its own, one task after the
 * keystroke that opened it, and the bar measures the window as it mounts to scroll its
 * highlighted row into view: in that later commit the measurement is of a window the browser has
 * already scrolled, and the row opens half out of view, which
 * `e2e/keyboard-palette.stub.e2e.ts`'s "on a short window the palette fits the screen and each
 * highlighted row shows whole" catches at 1440x300. With its chunk in, the bar mounts in the same
 * commit the keystroke makes, exactly as it did when the entry carried it, and the keys typed
 * straight after it reach its box. Until its chunk is in, a small box takes those keys, and the
 * bar opens with them in its box when the chunk lands (`askedSurface` says how an open is asked
 * for, called off, and asked for again after a failure).
 *
 * For the same reason the bar is not held until a document draws, as the margin and the
 * reference-preview layer are (`features/shell/content-drawn.ts`): a reader who presses
 * `$mod+k` on an issue page before its document has drawn would meet a bar that is not yet in
 * the module cache. The same row catches a hold that takes the bar too. So the bar's 28,952 bytes
 * (its chunk and the shortcut sheet's and the ask hit status's it imports) still come down before
 * an issue page's first block, after the shell's first paint; that is the price of a bar that
 * opens whole on the first key.
 */
const searchPaletteChunk = chunk(() =>
  import("./SearchPalette").then((module) => module.SearchPalette)
);

const Palette = askedSurface(searchPaletteChunk, "Search");

export function SearchPaletteHost({
  onClose,
  open,
}: {
  onClose: () => void;
  open: boolean;
}): ReactNode {
  // The download, started on the commit that drew the page: an effect runs after that commit,
  // so a landing asks for no byte of the bar before it has drawn. Nobody is waiting for it
  // here, which is what `prefetchChunk` tells the chunk-failure recovery: a page whose bar has
  // not arrived is a page nobody is looking at a bar on
  // (`features/shell/DeploymentResilience.tsx`). The reader's own open asks again, under
  // `renderChunk`, if this one has not landed (`askedSurface`).
  useEffect(() => {
    prefetchChunk(searchPaletteChunk.load);
  }, []);
  // Drawn closed as well as open once its chunk is in: the bar runs a chosen row once it has
  // closed (`SearchPalette.tsx`, `pendingAction`), so it stays mounted through the close.
  return <Palette onClose={onClose} open={open} />;
}
