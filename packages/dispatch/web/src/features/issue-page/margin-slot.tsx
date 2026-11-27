import { type ReactNode, useCallback, useContext, useEffect, useMemo, useState } from "react";

import { ShellFrameContext } from "./frame-context";

/**
 * The slot a page fills in the shell's margin.
 *
 * The margin is mounted once beside the router and every page shares it, so the margin must not
 * know which page is showing: it offers this slot and draws whatever the page put in it. A page
 * that fills nothing leaves the margin exactly as it was, which is every page but the issue
 * page's own (`IssueMargin.tsx`).
 *
 * A fill takes the margin's own content and answers what the margin draws in its place, so the
 * page wraps the conversation rather than sitting beside it: the issue page's panel is the
 * properties, the facts of the focused node and that conversation, in one panel (R12).
 *
 * The slot, the drawer and whether the margin draws the slot all travel in the shell's one frame
 * context (`frame-context.ts`), which the shell draws for every reader so that a change of the
 * issue-page choice mounts nothing again; the hooks below are how every part reads them.
 */
export type MarginFill = (content: ReactNode) => ReactNode;

/**
 * The margin as a drawer: on the issue page the panel opens over the right of the page, the
 * Details drawer, instead of keeping a column of its own, so the page's own column is the window
 * less the navigation rail. The shell holds whether it is open, as it holds every other
 * preference of the margin's, and a page reads it to draw the control that opens it. Undefined
 * wherever the margin is a column, which is every other page.
 */
export interface MarginDrawer {
  readonly open: boolean;
  /** The new page's words and its address rule, where its root has set them (`setPage`):
   *  undefined until then, and on every page that is not the new one. */
  readonly page?: MarginDrawerPage;
  /** What held the focus at the press that opened the drawer (`setOpen(true)` while it was
   *  closed), for the drawer to read as it draws open (`features/margin/Margin.tsx`). One the
   *  reader opened before the margin's chunk was in draws only once that chunk lands, and by then
   *  the reader may have gone elsewhere: from the phone's width up the drawer then takes no focus
   *  from where they went, and below it, where the drawer is a sheet that takes the focus as it
   *  opens, the open is called off. Undefined while the drawer is closed, and where nothing
   *  records it: the drawer then takes the focus as it opens. */
  readonly openedFrom?: Element | null;
  /** Told by the drawer as it is drawn and as it goes (`features/margin/Margin.tsx`), into state
   *  the shell keeps (`AppShell`, `app.tsx`): until it is drawn, which is once the margin's chunk
   *  is in, the shell's Escape calls off an open of it, and from then on its Escape is the
   *  drawer's own. Undefined where nothing listens. */
  readonly setDrawn?: (drawn: boolean) => void;
  readonly setOpen: (open: boolean) => void;
  /** Sets `page`, and with `undefined` takes it away again when the new page goes. */
  readonly setPage: (page: MarginDrawerPage | undefined) => void;
}

/**
 * What the new issue page hands the shell through the drawer, so that the shell draws the page's
 * words and follows its address rule with no import of the page's word list or its model: the
 * page's lazy root sets it. While it is not set, Details draws no head words and the margin
 * names no item.
 */
export interface MarginDrawerPage {
  /** The attributes that mark the drawer's own words as the page's own. */
  readonly chrome: Readonly<Record<string, string>>;
  /** The item the address names, by its id, and whether it stands beside the document the
   *  address opens (`?mark=`, or an item at a pinned version) rather than as a node of the
   *  outline; `undefined` where the address names no item. */
  readonly routeItem: (location: {
    readonly hash: string;
    readonly pathname: string;
    readonly search: string;
  }) => { readonly besideDocument: boolean; readonly id: string } | undefined;
  /** The drawer's head words: its name, and the name of the control that closes it. */
  readonly words: { readonly close: string; readonly details: string };
}

/** The attribute the Details drawer and the control that opens it carry, so the outline can tell
 *  the focus moving to Details - where the facts of the node it leaves are read - from the focus
 *  leaving for the rest of the page (`IssueTree.tsx`'s `releaseFocus`). */
export const DETAILS_ATTRIBUTE = "data-details";

/**
 * The slot, its drawer and whether the margin draws it, for a part drawn without the shell: a
 * test's page or margin on its own. It holds the fill in state, as the shell does (`app.tsx`),
 * and keeps whatever else a shell's context above it carries.
 */
export function MarginSlotProvider({
  children,
  drawer,
  drawsSlot = false,
}: {
  children: ReactNode;
  /** The drawer the margin is on this page, where it is one. */
  drawer?: MarginDrawer;
  /** Whether the margin is keeping a column for the page's panel right now. The shell leaves it
   *  unset (`frame-context.ts`), and the margin then draws the panel exactly where it is the
   *  drawer; this provider, which no shell draws, sets it, false unless the test says otherwise. */
  drawsSlot?: boolean;
}): ReactNode {
  const shell = useContext(ShellFrameContext);
  const [fill, setFill] = useState<MarginFill | undefined>(undefined);
  // A fill is a function, and React reads a function handed to a setter as an updater, so a
  // registration always wraps it in one.
  const fillSlot = useCallback((next: MarginFill | undefined) => setFill(() => next), []);
  const value = useMemo(
    () => ({ ...shell, drawer, drawsSlot, fill, fillSlot }),
    [drawer, drawsSlot, fill, fillSlot, shell]
  );
  return <ShellFrameContext value={value}>{children}</ShellFrameContext>;
}

/** The Details drawer the margin is on this page, or `undefined` where it is a column. */
export function useMarginDrawer(): MarginDrawer | undefined {
  return useContext(ShellFrameContext)?.drawer;
}

/**
 * Fills the margin's slot with `fill` for as long as the calling page is mounted, and empties it
 * again when the reader leaves. `fill` must be stable across renders (`useCallback`): the slot
 * holds it in state, so a fresh function every render would register on every commit. A page
 * outside the shell - a test's bare margin, a component rendered on its own - registers with
 * nobody and draws no panel of its own.
 */
export function useFillMarginSlot(fill: MarginFill | undefined): void {
  const register = useContext(ShellFrameContext)?.fillSlot;
  useEffect(() => {
    register?.(fill);
    return () => register?.(undefined);
  }, [fill, register]);
}

/** What the page filling the slot draws, or `undefined` while no page fills it. */
export function useMarginSlotFill(): MarginFill | undefined {
  return useContext(ShellFrameContext)?.fill;
}

/** Whether the margin draws the page's panel. A page draws it in its own column wherever this
 *  is false, so every property is on the screen at every width and with every preference
 *  (R11). The shell's margin draws it exactly where it is the drawer, and nowhere else: not
 *  outside the shell, and not where the margin is its column, open or collapsed to its rail. */
export function useMarginDrawsSlot(): boolean {
  const shell = useContext(ShellFrameContext);
  return shell?.drawsSlot ?? shell?.drawer !== undefined;
}

/** The margin's own content, drawn inside whatever the page filling the slot wraps it in, and
 *  drawn plain while `fill` is undefined: the margin hands its content over rather than asking
 *  which page it belongs to. */
export function FilledMarginSlot({
  children,
  fill,
}: {
  children: ReactNode;
  fill: MarginFill | undefined;
}): ReactNode {
  return <>{fill === undefined ? children : fill(children)}</>;
}
