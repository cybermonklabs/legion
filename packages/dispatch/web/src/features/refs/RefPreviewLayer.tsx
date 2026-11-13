import { lazy, type ReactNode, Suspense, useEffect, useState } from "react";

import { useContentDrawn } from "../shell/content-drawn";
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
 * the shell's first effect, which React runs on the commit that draws the page - or, on a
 * landing whose page draws a live document, on the commit after that document's first block
 * (`features/shell/content-drawn.ts`) - and the download is in long before a hand reaches a
 * reference. A link names its target with `referenceTriggerProps` (`reference-trigger.ts`), a
 * module of its own, so a page that draws references pulls no byte of the card until this layer
 * asks for it.
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
  // request for this chunk and the second render of the shell asks for it. On a landing whose
  // page draws a live document, the page is on the screen only once the document's first block
  // is, which is long after the shell's first paint (`features/shell/content-drawn.ts`).
  const drawn = useContentDrawn();
  const [wanted, setWanted] = useState(false);
  useEffect(() => {
    if (drawn) setWanted(true);
  }, [drawn]);
  if (!wanted) {
    return null;
  }
  return (
    <Suspense fallback={null}>
      <RefPreviewHost />
    </Suspense>
  );
}
