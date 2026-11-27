/**
 * The attributes this part marks the page's elements with that a module outside the part reads:
 * here, in a module that imports nothing, so the tree's keymap (`useTreeKeymap.ts`) reads them
 * with no import of the components that draw them. `NodeThreads.tsx` imports `useTreeKeymap.ts`
 * (`moveToRow`), so the keymap could not import the sheet's own module with no cycle.
 */

/** The attribute of the sheet that holds one node's threads below 1024 px (`NodeThreads.tsx`). */
export const TEXT_THREADS_ATTRIBUTE = "data-text-threads";
