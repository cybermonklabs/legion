import { type ReactNode, useEffect, useReducer, useState } from "react";

import { ChunkBoundary } from "../shell/ChunkBoundary";
import { chunk, chunkComponent } from "../shell/chunk-component";
import { useContentDrawn } from "../shell/content-drawn";
import { caretRefPreviewTrigger, focusAsksForCard, pointerAsksForCard } from "./ref-preview-card";

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
 * asks for it. One page does: the project board imports `closeRefPreview` from `RefPreview.tsx`
 * to close a card when a drag starts (`features/project/IssueBoard.tsx`), so the board's chunk
 * carries the card's module with it.
 */
// Through `chunkComponent` (`features/shell/chunk-component.tsx`): this layer is what draws the
// card a reader is pointing at, so a download that fails must spend one of the tab's recovery
// reloads rather than be read as a prefetch's (`features/shell/DeploymentResilience.tsx`). The
// shell draws the layer outside the page's error boundary (`app.tsx`), and it mounts with no
// reader action at all, so its own `ChunkBoundary` is what stops a failure of its chunk, or of a
// chunk a card draws from (a decision card in a previewed body,
// `features/doc/TextAskCardHost.tsx`), from reaching the app's top boundary (`main.tsx`), which
// would take the page and its sign-in with it. The recovery's reload is asked for all the same.
//
// The layer's first ask for the chunk comes when the page has drawn, before any card is asked
// for. It counts as a reader's wait all the same: it is where a page of a replaced build finds
// that the chunk is gone, and the reload it spends brings the build that has the chunk. After a
// failure, the layer draws nothing, and it asks again only when the reader asks for a card in
// one of the three ways that open one (`attachTriggers` in `RefPreview.tsx`): a pointer over a
// reference, a keyboard focus on a reference, or the caret entering a link of an editable
// document on a surface that opens no card on hover. `CardAsked` hears each of them and changes
// the boundary's `reset`. A reader waits on that download, so its failure spends a reload too.
// The host that arrives opens the card for what asked (`attachTriggers` again), because no event
// asks a second time. A move to another address asks for nothing: no one waits on that
// download, and a plain navigation must not spend a reload on it. Not `lazy`: React's `lazy`
// keeps the rejection of its one import for the life of the page, so a layer drawn again behind
// it would only throw the same failure again, where `chunk` keeps no failed download.
const RefPreviewHost = chunkComponent(
  chunk<object>(() => import("./RefPreview").then((module) => module.RefPreviewHost)),
  null
);

/** What the boundary draws in place of a layer that has failed: nothing, and listeners that call
 *  `ask` when the reader asks for a card. Each listener reads its gesture by the rule that the
 *  card's own listener (`attachTriggers` in `RefPreview.tsx`) reads it by, from
 *  `ref-preview-card.ts`:
 *  - a pointer that comes over a reference, but not a touch, not with a button down, and not on
 *    a surface that opens no card on hover (`pointerAsksForCard`);
 *  - a focus on a reference that `:focus-visible` says the keyboard made (`focusAsksForCard`);
 *  - the caret entering a link of an editable document on a surface that opens no card on hover
 *    (`caretRefPreviewTrigger`). A caret that moves inside the link asks for nothing, so the link
 *    the caret is in when this mounts asks only once the caret leaves it and comes back.
 *  A marked reference whose address the card cannot parse (`routeOf` in `RefPreview.tsx`) still
 *  asks and can spend a recovery reload without opening a card: telling those apart needs the
 *  reference parser in `features/refs/routes.ts`, which stays out of the entry, so only the
 *  card's chunk can tell, and the reader did point at a reference. */
export function CardAsked({ ask }: { ask: () => void }): null {
  useEffect(() => {
    const listening = new AbortController();
    const options = { signal: listening.signal };
    let caretIn = caretRefPreviewTrigger();
    document.addEventListener(
      "pointerover",
      (event) => {
        if (pointerAsksForCard(event)) ask();
      },
      options
    );
    document.addEventListener(
      "focusin",
      (event) => {
        if (focusAsksForCard(event.target)) ask();
      },
      options
    );
    document.addEventListener(
      "selectionchange",
      () => {
        const was = caretIn;
        caretIn = caretRefPreviewTrigger();
        if (caretIn !== null && caretIn !== was) ask();
      },
      options
    );
    return () => listening.abort();
  }, [ask]);
  return null;
}

export function RefPreviewLayer(): ReactNode {
  // The state, and not a bare render, is what holds the import until the page is on the screen:
  // an effect runs after the commit it belongs to, so the first paint of a landing carries no
  // request for this chunk and the second render of the shell asks for it. On a landing whose
  // page draws a live document, the page is on the screen only once the document's first block
  // is, which is long after the shell's first paint (`features/shell/content-drawn.ts`).
  const drawn = useContentDrawn();
  // The count of cards the reader asked for while the host had failed: `CardAsked` counts them,
  // and the boundary draws it only then. Each new count draws the host again.
  const [asks, ask] = useReducer((count: number) => count + 1, 0);
  const [wanted, setWanted] = useState(false);
  useEffect(() => {
    if (drawn) setWanted(true);
  }, [drawn]);
  if (!wanted) {
    return null;
  }
  // A render that threw is drawn again for the next card too, as a chunk that did not load is.
  const listener = <CardAsked ask={ask} />;
  return (
    <ChunkBoundary fallback={listener} reset={asks}>
      <RefPreviewHost />
    </ChunkBoundary>
  );
}
