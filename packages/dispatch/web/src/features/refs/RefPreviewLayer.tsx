import { lazy, type ReactNode, Suspense, useEffect, useState } from "react";

import { renderChunk } from "../shell/DeploymentResilience";

/**
 * Where the shell mounts the reference-preview layer, and where that layer's code is fetched:
 * after the page has drawn, never before it.
 *
 * The layer draws the card that appears over a `dispatch://` reference a reader hovers or
 * focuses. The shell imported it directly until dispatch://INFRA-1109, so the card, its read,
 * its Markdown surface and the preview timing were all in the entry chunk - about 46,000 bytes
 * every page of the dashboard parsed before it drew anything, for a card a page load may never
 * show.
 *
 * What a reader loses by its being late: nothing they can point at. The layer is asked for in
 * the shell's first effect, which React runs on the commit that draws the page, so the download
 * is started beside the page's own and is in long before a hand reaches a reference. A page
 * that shows references pulls the same chunk through its own code anyway - every reference
 * trigger is `referenceTriggerProps` of this module (`RefPreview.tsx`) - so for those pages this
 * is the same download moved off the first parse rather than a download added.
 */
// Under `renderChunk`, like every chunk a render waits on: this layer is what draws the card a
// reader is pointing at, so a download that fails must spend one of the tab's recovery reloads
// rather than be read as the entry's prefetch (`features/shell/DeploymentResilience.tsx`).
const RefPreviewHost = lazy(() =>
  renderChunk(() => import("./RefPreview").then((module) => ({ default: module.RefPreviewHost })))
);

export function RefPreviewLayer(): ReactNode {
  // The state, and not a bare render, is what holds the import until the page is on the screen:
  // an effect runs after the commit it belongs to, so the first paint of a landing carries no
  // request for this chunk and the second render of the shell asks for it.
  const [wanted, setWanted] = useState(false);
  useEffect(() => {
    setWanted(true);
  }, []);
  if (!wanted) {
    return null;
  }
  return (
    <Suspense fallback={null}>
      <RefPreviewHost />
    </Suspense>
  );
}
