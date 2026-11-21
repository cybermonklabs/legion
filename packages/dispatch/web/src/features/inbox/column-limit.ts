// This module imports nothing, and must keep it that way: the browser rows read the bound
// (`e2e/inbox-pane-scroll.ts`), and Playwright loads them under Node, where a module that reaches
// the app's keymap fails at load on `import.meta.env`, which only a Vite build defines.

/** How long the Inbox's reading column may go on putting the document it opened back at its
 *  section. The bound is the column's own, and not the page's correction bound (`LIMIT_MS`,
 *  `features/doc/refocus-when-settled.ts`), because a bound says how long a surface may re-place
 *  itself and the two surfaces draw different things: the page's correction must outlive the
 *  build of the largest document this branch measures, tens of seconds of it, while the column's
 *  need only outlive the longest draw the pane itself serves.
 *
 *  That draw was measured rather than taken on faith: the heaviest document a reading pane
 *  hosts - the AHXV6-1-shaped one, about 81,000 characters and 28 diagrams
 *  (`e2e/large-documents.ts`) - drew for 7.15 s, 6.26 s and 6.05 s from the row change to its
 *  last block, on a two-processor pod under the 4x CPU throttle the phone rows run with. Ten
 *  seconds is the worst of those with about two fifths over it. The five-times-larger document
 *  drew for 40 s, and once for 136 s, on the same pod: no placement's bound covers that, since
 *  a column still being written ten seconds after the reader opened it is one they have long
 *  since taken over - which ends the placement itself, as their first press, scroll, touch or
 *  key in the column always does, a modifier pressed alone aside (`pane-placement.ts`). */
export const COLUMN_LIMIT_MS = 10_000;
