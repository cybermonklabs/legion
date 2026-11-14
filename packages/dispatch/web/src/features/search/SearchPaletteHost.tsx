import { type ComponentType, type ReactNode, useEffect, useState } from "react";

import { prefetchChunk, renderChunk } from "../shell/DeploymentResilience";

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
 * The bar is held in state rather than behind `lazy()` and a `Suspense`, which is what this
 * host was first written as. A `lazy()` bar mounts in a commit of its own, one task after the
 * keystroke that opened it, and the bar measures the window as it mounts to scroll its
 * highlighted row into view: in that later commit the measurement is of a window the browser
 * has already scrolled, and the row opens half out of view, which
 * `e2e/keyboard-palette.stub.e2e.ts`'s "on a short window the palette fits the screen and each
 * highlighted row shows whole" catches at 1440x300. Held in state, the bar is in the module
 * cache before the first keystroke and mounts in the same commit the keystroke makes, exactly
 * as it did when the entry carried it.
 *
 * For the same reason the bar is not held until a document draws, as the margin and the
 * reference-preview layer are (`features/shell/content-drawn.ts`): a reader who presses
 * `$mod+k` on an issue page before its document has drawn would meet a bar that is not yet in
 * the module cache, and it would open with its row half out of view. The same row caught that
 * when the hold took the bar too. So the bar's 28,952 bytes (its chunk and the shortcut sheet's
 * and the ask hit status's it imports) still come down before an issue page's first block,
 * after the shell's first paint; that is the price of a bar that opens whole on the first key.
 */
type Palette = ComponentType<{ onClose: () => void; open: boolean }>;

const loadPalette = (): Promise<{ SearchPalette: Palette }> => import("./SearchPalette");

export function SearchPaletteHost({
  onClose,
  open,
}: {
  onClose: () => void;
  open: boolean;
}): ReactNode {
  const [Palette, setPalette] = useState<Palette | null>(null);

  // The download, started on the commit that drew the page: an effect runs after that commit,
  // so a landing asks for no byte of the bar before it has drawn. Nobody is waiting for it
  // here, which is what `prefetchChunk` tells the chunk-failure recovery: a page whose bar has
  // not arrived is a page nobody is looking at a bar on
  // (`features/shell/DeploymentResilience.tsx`).
  useEffect(() => {
    let live = true;
    prefetchChunk(() =>
      loadPalette().then((module) => {
        if (live) {
          setPalette(() => module.SearchPalette);
        }
      })
    );
    return () => {
      live = false;
    };
  }, []);

  // The reader opened the bar and the download has not landed - a slow network, or a download
  // that failed while nobody was waiting. Now somebody is, so this ask goes through
  // `renderChunk`: a failure here spends one of the tab's recovery reloads rather than leaving
  // a reader pressing a key that opens nothing.
  useEffect(() => {
    if (!open || Palette !== null) {
      return;
    }
    let live = true;
    void renderChunk(loadPalette).then(
      (module) => {
        if (live) {
          setPalette(() => module.SearchPalette);
        }
      },
      () => {
        // The recovery has the tab's reload; there is nothing else this host can draw.
      }
    );
    return () => {
      live = false;
    };
  }, [open, Palette]);

  if (Palette === null) {
    return null;
  }
  return <Palette onClose={onClose} open={open} />;
}
