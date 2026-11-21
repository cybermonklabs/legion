import { chunk, chunkComponent } from "../shell/chunk-component";

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
 *
 * The card draws as soon as its chunk is in, and not 300 ms later, as a `lazy` card under a
 * `Suspense` did (`features/shell/chunk-component.tsx`). A card that draws above the block a
 * reader went to moves that block, and the reader can already have gone there: the Inbox's
 * Skip to the answer did, and the card above the answer then pushed it out of place
 * (`e2e/inbox-pane-scroll.e2e.ts`). Each card portals into the slots of its own block, so a card
 * whose chunk is not in holds back that card alone and never the document.
 */
const askBlockCardChunk = chunk(() =>
  import("./AskBlockCard").then((module) => module.AskBlockCard)
);

export const AskBlockCardHost = chunkComponent(askBlockCardChunk, null);
