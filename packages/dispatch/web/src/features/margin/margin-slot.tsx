import { createContext, type ReactNode, useCallback, useContext, useEffect, useState } from "react";

/**
 * The slot a page fills in the shell's margin (dispatch://INFRA-1196 R12).
 *
 * The margin is mounted once beside the router and every page shares it, so the margin must not
 * know which page is showing: it offers this slot and draws whatever the page put in it. A page
 * that fills nothing leaves the margin exactly as it was, which is every page but the issue
 * page's own (`features/issue/IssueMargin.tsx`).
 *
 * A fill takes the margin's own content and answers what the margin draws in its place, so the
 * page wraps the conversation rather than sitting beside it: the issue page's panel is the
 * properties, the facts of the focused node and that conversation, in one panel (R12).
 */
export type MarginFill = (content: ReactNode) => ReactNode;

/** What the page put in the slot, read by the margin. */
const MarginFillContext = createContext<MarginFill | undefined>(undefined);

/** Where a page registers its fill. A page outside the provider - a test's bare margin, a
 *  component rendered on its own - registers with nobody and draws no panel of its own. */
const MarginFillRegistry = createContext<(fill: MarginFill | undefined) => void>(() => {});

/** Whether the shell's margin has a column of its own at this width that draws what the page
 *  put in the slot. False outside the provider, and false while the margin is collapsed to its
 *  rail or is the phone sheet, so a page that draws a panel draws it itself rather than
 *  handing it to a margin that is not there (dispatch://INFRA-1196 R11, R12). */
const MarginDrawsSlotContext = createContext(false);

/**
 * The margin as a drawer: on the issue page the panel opens over the right of the page, the
 * Details drawer, instead of keeping a column of its own, so the page's own column is the window
 * less the navigation rail (dispatch://INFRA-1196 R14). The shell holds whether it is open, as
 * it holds every other preference of the margin's, and a page reads it to draw the control that
 * opens it. Undefined wherever the margin is a column, which is every other page.
 */
export interface MarginDrawer {
  readonly open: boolean;
  readonly setOpen: (open: boolean) => void;
}

const MarginDrawerContext = createContext<MarginDrawer | undefined>(undefined);

/** The attribute the Details drawer and the control that opens it carry, so the outline can tell
 *  the focus moving to Details - where the facts of the node it leaves are read - from the focus
 *  leaving for the rest of the page (`IssueTree.tsx`'s `releaseFocus`). */
export const DETAILS_ATTRIBUTE = "data-details";

export function MarginSlotProvider({
  children,
  drawer,
  drawsSlot = false,
}: {
  children: ReactNode;
  /** The drawer the margin is on this page, where it is one. */
  drawer?: MarginDrawer;
  /** Whether the margin is keeping a column for the page's panel right now. The shell answers
   *  it, because the shell holds the reader's collapse preference and the width. */
  drawsSlot?: boolean;
}): ReactNode {
  const [fill, setFill] = useState<MarginFill | undefined>(undefined);
  // A fill is a function, and React reads a function handed to a setter as an updater, so a
  // registration always wraps it in one.
  const register = useCallback((next: MarginFill | undefined) => setFill(() => next), []);
  return (
    <MarginFillRegistry.Provider value={register}>
      <MarginDrawerContext.Provider value={drawer}>
        <MarginDrawsSlotContext.Provider value={drawsSlot}>
          <MarginFillContext.Provider value={fill}>{children}</MarginFillContext.Provider>
        </MarginDrawsSlotContext.Provider>
      </MarginDrawerContext.Provider>
    </MarginFillRegistry.Provider>
  );
}

/** The Details drawer the margin is on this page, or `undefined` where it is a column. */
export function useMarginDrawer(): MarginDrawer | undefined {
  return useContext(MarginDrawerContext);
}

/**
 * Fills the margin's slot with `fill` for as long as the calling page is mounted, and empties it
 * again when the reader leaves. `fill` must be stable across renders (`useCallback`): the slot
 * holds it in state, so a fresh function every render would register on every commit.
 */
export function useFillMarginSlot(fill: MarginFill | undefined): void {
  const register = useContext(MarginFillRegistry);
  useEffect(() => {
    register(fill);
    return () => register(undefined);
  }, [fill, register]);
}

/** What the page filling the slot draws, or `undefined` while no page fills it. */
export function useMarginSlotFill(): MarginFill | undefined {
  return useContext(MarginFillContext);
}

/** Whether the margin draws the page's panel. A page draws it in its own column wherever this
 *  is false, so every property is on the screen at every width and with every preference
 *  (R11). */
export function useMarginDrawsSlot(): boolean {
  return useContext(MarginDrawsSlotContext);
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
