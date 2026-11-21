import { hashKey, type MutationKey } from "@tanstack/react-query";

// The name the margin reads a composer's drafts under from outside the composer, apart from the
// composer's hook (`useKeptDrafts.ts`): the margin's provider is in the entry chunk
// (`features/margin/margin-context.tsx`), and Rollup never splits a module across chunks, so an
// import of it from the hook's module put the whole hook, and the composer's draft model with it,
// in what every page parses before it draws anything (dispatch://INFRA-1109, R13).

/** The name of the composer that sends under `mutationKey` (`KeptUnder.composerName`): its send's
 *  own name, which is what it keeps the reader's drafts under (`keptNames`). The margin keeps and
 *  reads a compose's drafts under it from outside the composer (`giveWay`, `useMarginSheet`). */
export function composerNameOf(mutationKey: MutationKey): string {
  return hashKey(mutationKey);
}
