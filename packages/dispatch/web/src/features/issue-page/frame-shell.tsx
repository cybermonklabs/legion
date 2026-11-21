import { type ComponentProps, type ReactNode, useMemo, useState } from "react";
import { useLocation } from "react-router-dom";

import { borderDefault, pagePaperBg, skeletonBg } from "../../theme/classes";
import { MarginProvider } from "../margin/margin-context";
import { NO_HOVER_PREVIEW_ATTRIBUTE } from "../refs/ref-preview-card";
import { parseIssuePath } from "../refs/routes";
import { MD_VIEWPORT_QUERY, useMediaQuery } from "../shell/useDialog";
import { IconRail } from "./IconRail";
import { type MarginDrawer, type MarginDrawerPage, MarginSlotProvider } from "./margin-slot";
import { ReferenceNamesProvider } from "./reference-names";
import { type ShellNavigation, ShellNavigationContext } from "./shell-navigation";
import { ShortcutHelpOpener } from "./shortcut-help-opener";

/**
 * The shell of a reader who turned the new issue page on: the frame that page draws in (R1, R12,
 * R14), around the shell `app.tsx` draws for every reader (`AppShell`).
 *
 * WHY A CHUNK. Every page of the dashboard parses the entry chunk before it draws anything, and
 * a reader who has not turned the new page on draws none of this: not the icon rail, not the
 * Details drawer's state, not the providers the new page reads, not the frame's skeleton. So
 * this module is a chunk of its own (`frame-chunk.ts`), which the shell's switch draws only for
 * that reader, and its imports - the rail, the margin's slot, the published reference names,
 * the shell's navigation for the page's header and the opener of the list of keys - are
 * imported by no module of the entry.
 *
 * The frame changes the shell on an issue's address only (`ShellFrame`), and the providers wrap
 * everything the shell draws on every page (`FrameProviders`), as they did when the shell
 * mounted them itself: a move between an issue and another page then keeps every component the
 * shell draws, the live views among them. `AppShell` draws both with its own state, which stays
 * its own, and draws today's shell wherever the frame is undefined.
 */

/** What the frame's providers take from the shell's own state (`AppShell`). */
export interface ShellState {
  /** Whether the navigation's sheet is open. */
  readonly navigationOpen: boolean;
  /** What a page's own Keys control does: what `?` does. */
  readonly openHelp: () => void;
  readonly setNavigationOpen: (open: boolean) => void;
}

/** Draws `body`, everything the shell draws, inside the providers the new issue page reads. */
export type FrameProviders = (body: ReactNode, shell: ShellState) => ReactNode;

/** What the new issue page's frame changes in the shell, on an issue's address. */
export interface ShellFrame {
  /** What `<main>` holds while the page's code loads. */
  readonly fallback: ReactNode;
  /** `<main>`'s attributes in the frame: its references open no card on hover (R6). */
  readonly mainAttributes: Readonly<Record<string, string>>;
  /** `<main>`'s class in the frame. */
  readonly mainClass: string;
  /** The icon rail where the sidebar stands, or nothing below `md`, where the page's header
   *  draws the control that opens the navigation. */
  readonly rail: (props: ComponentProps<typeof IconRail>) => ReactNode;
  /** What Shift+M does in the frame: the margin is Details, which it opens and closes. */
  readonly toggleDetails: () => void;
}

export interface FrameShellProps {
  /** Draws the shell with the frame of this address, `undefined` off an issue's address, and
   *  the frame's providers. The shell is handed in rather than imported: `app.tsx` is the entry,
   *  and this chunk is what the entry loads. */
  readonly shell: (frame: ShellFrame | undefined, providers: FrameProviders) => ReactNode;
}

/**
 * In the frame `<main>` has no padding of its own: the page's header, its view row, its Contents
 * pane and its column each keep the inset the prototype gives them, the pane against the rail,
 * which `md:pl-14` makes room for.
 */
const FRAME_MAIN_CLASS = `min-h-dvh min-w-0 flex-1 outline-none md:pl-14 ${pagePaperBg}`;

const FRAME_MAIN_ATTRIBUTES = { [NO_HOVER_PREVIEW_ATTRIBUTE]: "" };

/**
 * What the new issue page's frame holds while the page's code loads (R10):
 * the header's two lines, the view row's five controls and the first rows, as boxes with no word
 * in them, so no name of a control flashes on screen and none of today's nine tab names does.
 * It takes the inset the page's own column takes, since the frame gives `<main>` no padding.
 * `min-w-0` and `overflow-hidden`: the row of boxes neither wraps nor scrolls, so a chunk's load
 * never makes the page wider than a phone's window. Every other reader keeps the shell's
 * `IssuePageFallback` (`app.tsx`).
 */
function IssueFrameFallback(): ReactNode {
  return (
    <div aria-busy="true" className="px-3.5 pt-2 md:px-7">
      <div className={`h-4 w-16 animate-pulse rounded ${skeletonBg}`} />
      <div className={`mt-2 h-7 w-2/3 animate-pulse rounded ${skeletonBg}`} />
      <div
        className={`mt-4 mb-4 flex min-w-0 gap-2 overflow-hidden border-b pb-2 ${borderDefault}`}
        role="toolbar"
      >
        {[0, 1, 2, 3, 4].map((box) => (
          <span className={`h-5 w-16 shrink-0 animate-pulse rounded ${skeletonBg}`} key={box} />
        ))}
      </div>
      <div className="space-y-3">
        {[0, 1, 2, 3].map((row) => (
          <div className={`h-4 w-full animate-pulse rounded ${skeletonBg}`} key={row} />
        ))}
      </div>
    </div>
  );
}

/**
 * The providers mounted around everything the shell draws: the margin's own, and the four the
 * new issue page's frame reads (R12, R14). `drawer` is Details, which only
 * the frame passes: with it undefined the margin is the column it has always been, and no page
 * fills its slot. `navigationShown` says whether the page's header draws its `☰` from the
 * navigation, and the Keys control opens `openHelp`. A page publishes the names it read for its
 * references to everything under the shell, the margin beside the router among it
 * (`ReferenceNamesProvider`).
 */
function ShellProviders({
  children,
  drawer,
  navigationOpen,
  navigationShown,
  openHelp,
  setNavigationOpen,
}: ShellState & {
  children: ReactNode;
  drawer: MarginDrawer | undefined;
  navigationShown: boolean;
}): ReactNode {
  // What the page's header draws its `☰` from below `md`, where the frame draws neither the
  // shell's bar nor the rail; undefined wherever the shell draws a control of its own.
  const navigation = useMemo<ShellNavigation | undefined>(
    () =>
      navigationShown ? { onOpen: () => setNavigationOpen(true), open: navigationOpen } : undefined,
    [navigationOpen, navigationShown, setNavigationOpen]
  );
  return (
    <MarginProvider>
      <ReferenceNamesProvider>
        <MarginSlotProvider drawer={drawer} drawsSlot={drawer !== undefined}>
          <ShellNavigationContext.Provider value={navigation}>
            <ShortcutHelpOpener.Provider value={openHelp}>{children}</ShortcutHelpOpener.Provider>
          </ShellNavigationContext.Provider>
        </MarginSlotProvider>
      </ReferenceNamesProvider>
    </MarginProvider>
  );
}

export function FrameShell({ shell }: FrameShellProps): ReactNode {
  const location = useLocation();
  // The frame (R1, R14), computed here once: an issue's address, since this shell is drawn only
  // for a reader who chose the new page. In it an icon rail of 56 px stands where the sidebar
  // stands, from Tailwind's `md` up, and below that the page's header draws the control that
  // opens the navigation (`ShellNavigationContext`); the margin is Details, a drawer over the
  // page. The rail is drawn by the query that is `md` itself, in `rem`, because the room the
  // page makes for it is `md:pl-14`: a pixel query draws the rail over the page wherever the two
  // disagree, which is every width from 768 to 959 px at a 20 px default font. Every other page
  // keeps the shell exactly as it is for every other reader.
  const issueFrame = parseIssuePath(location.pathname, location.search) !== undefined;
  const railFits = useMediaQuery(MD_VIEWPORT_QUERY);
  // Whether Details is open, held for the issue it was opened on: every load opens it closed, and
  // so does every move to another issue. Leaving the issue forgets it in the same render, so
  // coming back to it - by a link or by Back - finds it closed, as a load does.
  const detailsIssue = parseIssuePath(location.pathname)?.key;
  const [detailsFor, setDetailsFor] = useState<string | undefined>(undefined);
  if (detailsFor !== undefined && detailsFor !== detailsIssue) setDetailsFor(undefined);
  // What the new page's root hands Details: its words and its address rule
  // (`drawer-page.ts`), so that the shell imports neither.
  const [drawerPage, setDrawerPage] = useState<MarginDrawerPage | undefined>(undefined);
  const detailsDrawer = useMemo<MarginDrawer | undefined>(
    () =>
      issueFrame
        ? {
            open: detailsFor !== undefined && detailsFor === detailsIssue,
            page: drawerPage,
            setOpen: (open: boolean) => setDetailsFor(open ? detailsIssue : undefined),
            setPage: setDrawerPage,
          }
        : undefined,
    [detailsFor, detailsIssue, drawerPage, issueFrame]
  );
  const frame: ShellFrame | undefined =
    detailsDrawer === undefined
      ? undefined
      : {
          fallback: <IssueFrameFallback />,
          mainAttributes: FRAME_MAIN_ATTRIBUTES,
          mainClass: FRAME_MAIN_CLASS,
          rail: (props) => (railFits ? <IconRail {...props} /> : null),
          toggleDetails: () => detailsDrawer.setOpen(!detailsDrawer.open),
        };
  const providers: FrameProviders = (body, state) => (
    <ShellProviders drawer={detailsDrawer} navigationShown={issueFrame && !railFits} {...state}>
      {body}
    </ShellProviders>
  );
  return shell(frame, providers);
}
