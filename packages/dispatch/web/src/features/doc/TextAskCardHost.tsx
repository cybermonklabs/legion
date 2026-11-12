import { lazy, type ReactNode, Suspense } from "react";

import { renderChunk } from "../shell/DeploymentResilience";
import type { AskBlockHost } from "./typed-block-drawing";

/**
 * The decision card a text body draws, and where that card's code is fetched: when a body the
 * reader is looking at holds a decision block, never before.
 *
 * The shell provides this component for every Markdown body it renders (`TextAskCard` in
 * `app.tsx`), and it imported the card itself until dispatch://INFRA-1109. The card draws the
 * whole ask surface - the question, the options, the recipients, the thread and the Markdown
 * under all of them - so the entry chunk every page parses before drawing anything carried
 * about 145,000 bytes of ask-card code for the bodies that hold no decision block at all, which
 * is most of them.
 *
 * What a reader loses by its being late: for the frames between a body's paint and this
 * chunk's arrival, a decision block in that body keeps its question and options as the source
 * list the body already drew - the same frame a reader is shown where the shell provides no
 * card at all (`features/refs/markdown-cards.ts`) - and the card then fills the block's header
 * and footer slots.
 */
// Under `renderChunk`, like every chunk a render waits on: a reader is in front of a block this
// card is to draw, so a download that fails must spend one of the tab's recovery reloads rather
// than be read as the entry's prefetch (`features/shell/DeploymentResilience.tsx`).
const TextAskBlockCard = lazy(() =>
  renderChunk(() =>
    import("./AskBlockCard").then((module) => ({ default: module.TextAskBlockCard }))
  )
);

/**
 * Its own `Suspense`, inside the card rather than around the body: `useRenderedMarkdown` mounts
 * one of these per `ask` shell, through a portal into the shell's slots, and the trees those
 * portals hang in are as many as there are bodies on a page. A boundary here suspends the card
 * alone, so a body whose paint has landed is never replaced by a fallback while this chunk
 * comes down.
 */
export function TextAskCardHost({ host }: { host: AskBlockHost }): ReactNode {
  return (
    <Suspense fallback={null}>
      <TextAskBlockCard host={host} />
    </Suspense>
  );
}
