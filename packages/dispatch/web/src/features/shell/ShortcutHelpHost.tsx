import { type ReactNode, useEffect } from "react";

import { askedSurface } from "./asked-surface";
import { chunk } from "./chunk-component";
import { prefetchChunk } from "./chunk-loads";
import type { KeyBindingDescription } from "./keymap";

/**
 * Where the shell places the keyboard-shortcut sheet, and where that sheet's code is fetched:
 * once the page has drawn, never before it. The sheet and the key hints it draws were in the
 * entry chunk until dispatch://INFRA-1109, which every page parses before it draws anything.
 *
 * The sheet is drawn through `askedSurface` (`asked-surface.tsx`), not behind `lazy` and
 * `Suspense`: with its chunk in, it opens in the commit of the `?` that asked for it, and the
 * keys typed straight after `?` reach its filter and not the page - on an issue page a digit
 * would write the issue's priority. So the chunk is started in this host's effect, which runs
 * after the commit that drew the page; the search bar's chunk imports the sheet's for its key
 * hints (`features/search/SearchPalette.tsx`) and is started at the same moment, so the two
 * share the one download, and this start is what tells the host the sheet is in. Nobody is
 * waiting on it yet, which is what `prefetchChunk` tells the chunk-failure recovery
 * (`DeploymentResilience.tsx`).
 *
 * What a reader loses by its being late: the sheet's place shows a small box that takes the keys
 * until the chunk lands, and then the sheet opens with what they typed in its filter. The
 * registry it lists is taken when `?` fires, before the chunk is asked for (`appKeymap.describe()`
 * in `app.tsx`), so the sheet shows the bindings of the row the reader was on however long the
 * download takes.
 */
const shortcutHelpChunk = chunk(() =>
  import("./ShortcutHelp").then((module) => module.ShortcutHelp)
);

const Sheet = askedSurface(shortcutHelpChunk, "Keyboard shortcuts");

export function ShortcutHelpHost({
  onClose,
  snapshot,
}: {
  onClose: () => void;
  snapshot: readonly KeyBindingDescription[] | null;
}): ReactNode {
  useEffect(() => {
    prefetchChunk(shortcutHelpChunk.load);
  }, []);
  // `snapshot` is the registry as `?` described it and `null` while the sheet is closed, so it
  // is also the one thing that says whether the reader has asked for the sheet.
  if (snapshot === null) {
    return null;
  }
  return <Sheet onClose={onClose} snapshot={snapshot} />;
}
