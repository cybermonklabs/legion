import { lazy, type ReactNode, Suspense } from "react";

import { renderChunk } from "./DeploymentResilience";
import type { KeyBindingDescription } from "./keymap";

/**
 * Where the shell places the keyboard-shortcut sheet, and where that sheet's code is fetched:
 * when a reader presses `?`, never before. The sheet and the key hints it draws were in the
 * entry chunk until dispatch://INFRA-1109, which every page parses before it draws anything.
 *
 * What a reader loses by its being late: the frames between `?` and the chunk's arrival show no
 * sheet. The registry it lists is taken when `?` fires, before this chunk is asked for
 * (`appKeymap.describe()` in `app.tsx`), so the sheet shows the bindings of the row the reader
 * was on however long the download takes.
 */
// Under `renderChunk`, like every chunk a render waits on: the reader has asked for the sheet,
// so a download that fails must spend one of the tab's recovery reloads rather than be read as
// the entry's prefetch (`features/shell/DeploymentResilience.tsx`).
const Sheet = lazy(() =>
  renderChunk(() => import("./ShortcutHelp").then((module) => ({ default: module.ShortcutHelp })))
);

export function ShortcutHelpHost({
  onClose,
  snapshot,
}: {
  onClose: () => void;
  snapshot: readonly KeyBindingDescription[] | null;
}): ReactNode {
  // `snapshot` is the registry as `?` described it and `null` while the sheet is closed, so it
  // is also the one thing that says whether the reader has asked for this chunk yet.
  if (snapshot === null) {
    return null;
  }
  return (
    <Suspense fallback={null}>
      <Sheet onClose={onClose} snapshot={snapshot} />
    </Suspense>
  );
}
