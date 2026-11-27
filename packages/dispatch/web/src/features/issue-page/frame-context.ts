import { createContext } from "react";

import type { MarginDrawer, MarginFill } from "./margin-slot";
import type { ShellNavigation } from "./shell-navigation";

/**
 * What the shell (`app.tsx`, `AppShell`) hands the new issue page and the margin beside it: the
 * one context the shell draws, at one place in its tree, for every reader and on every page.
 *
 * WHY ONE CONTEXT, AND WHY HERE. The reader can turn the new page on or off at any moment, with
 * Settings' switch in this tab or another tab's (the window's `storage` event,
 * `stored-choice.ts`), and that change must mount nothing above the issue page it changes: the
 * checkbox the reader pressed keeps the focus, and every tab of the login keeps its open dialogs,
 * its live views and what the reader typed and has not sent. A provider drawn for one choice and
 * not for the other would mount everything under it again, since React mounts a component anew
 * wherever the components above it change. So the provider is drawn for both choices, which puts
 * its code in the entry chunk every page parses before it draws (`asset-sizes-in-build.ts`
 * holds that chunk to its budget). One context costs a few bytes there; the margin slot's four,
 * the shell navigation's, the opener's and the reference names' store, each a provider of its
 * own, would each put a provider's code there too.
 *
 * So this carries only what crosses between the shell, the page and the margin. What the frame
 * computes from the shell's state - the navigation for the page's header - is the frame chunk's
 * code (`frame-shell.tsx`), Details' drawer is the shell's own (`AppShell`), and the providers
 * only the page reads are the page's own, fed from here (`IssueOutlinePage.tsx`). The margin
 * slot's hooks read it directly (`margin-slot.tsx`), since the margin reads them outside the page
 * as well as the page inside it. Outside the shell - a test that draws a part on its own - it is
 * undefined, and every hook over it answers as no frame and no slot.
 */
export interface ShellFrameValue {
  /** The margin as the new page's Details drawer, on an issue's address for a reader who chose
   *  that page and whose frame is in; undefined wherever the margin is its column. */
  readonly drawer: MarginDrawer | undefined;
  /** Whether the margin keeps a column for the page's panel right now. The shell leaves it unset,
   *  and the margin then draws the panel exactly where it is the drawer; a test's
   *  `MarginSlotProvider` sets it. */
  readonly drawsSlot?: boolean;
  /** What the page put in the margin's slot, read by the margin (`useMarginSlotFill`). */
  readonly fill: MarginFill | undefined;
  /** Where the page puts it (`useFillMarginSlot`): a stable function. */
  readonly fillSlot: (fill: MarginFill | undefined) => void;
  /** What the page's header draws its `☰` from, in the frame below `md`; undefined wherever the
   *  shell draws that control itself (`ShellNavigation`). */
  readonly navigation?: ShellNavigation;
  /** What a page's own Keys control does: what `?` does. */
  readonly openHelp?: () => void;
}

export const ShellFrameContext = createContext<ShellFrameValue | undefined>(undefined);
