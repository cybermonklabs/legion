import { lazy, type ReactNode, Suspense } from "react";

import { renderChunk } from "../shell/DeploymentResilience";
import type { AskBlockCardProps } from "./AskBlockCard";

/**
 * The decision card a live or past document draws on each of its decision blocks, and where the
 * card's code is fetched: when the editor reports a decision block in the document, never before.
 *
 * WHY. `ProofDocument.tsx` and `VersionView.tsx` imported the card themselves until
 * dispatch://INFRA-1109, and the card draws the whole ask surface - the question, the options,
 * the recipients, the thread and the Markdown under them (`features/inbox/AskCard.tsx`, a chunk
 * of 52,107 bytes). So every page that drew a document fetched that chunk before its first block
 * drew, and most documents hold no decision block at all. Measured on a cold `/issues/CORE-1`
 * with the R13 reading of dispatch://INFRA-1109 (Resource Timing of `/assets/*.js` against the
 * first block's insertion) at that issue's head 90ee14dd.
 *
 * What a reader loses by its being late: for the frames between the block's paint and this
 * chunk's arrival, the editor's own node view draws the decision block (`ask-block.ts`), and its
 * header and footer slots are empty until the card fills them. That is the same frame
 * `TextAskCardHost.tsx` gives a decision block in a Markdown body.
 */
// Under `renderChunk`, like every chunk a render waits on: a reader is in front of the block
// this card is to draw, so a download that fails must spend one of the tab's recovery reloads
// rather than be read as the entry's prefetch (`features/shell/DeploymentResilience.tsx`).
const AskBlockCard = lazy(() =>
  renderChunk(() => import("./AskBlockCard").then((module) => ({ default: module.AskBlockCard })))
);

/** One `Suspense` per card, as `TextAskCardHost` has: each card portals into the slots of its
 *  own block, so a boundary here holds back that card alone and never the document. */
export function AskBlockCardHost(props: AskBlockCardProps): ReactNode {
  return (
    <Suspense fallback={null}>
      <AskBlockCard {...props} />
    </Suspense>
  );
}
