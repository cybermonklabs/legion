import { chunk, chunkComponent } from "../shell/chunk-component";

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
 *
 * `useRenderedMarkdown` mounts one of these per `ask` shell, through a portal into the shell's
 * slots, so a card whose chunk is not in draws nothing in its own slots and never replaces the
 * body. It draws through `chunkComponent`, as the document's card does (`AskBlockCardHost.tsx`),
 * and not through `lazy` and `Suspense`: a `lazy` card shows its fallback first and draws no
 * sooner than 300 ms after it, also when another card brought its chunk in long before
 * (`features/shell/chunk-component.tsx`).
 */
const textAskBlockCardChunk = chunk(() =>
  import("./AskBlockCard").then((module) => module.TextAskBlockCard)
);

export const TextAskCardHost = chunkComponent(textAskBlockCardChunk, null);
