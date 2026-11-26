import type { ComponentProps, ReactNode } from "react";

import { pagePaperBg } from "../../theme/classes";
import { borderDefault, skeletonBg } from "../../theme/shell-classes";
import { parseIssuePath } from "../refs/page-routes";
import { NO_HOVER_PREVIEW_ATTRIBUTE } from "../refs/ref-preview-card";
import { IconRail } from "./IconRail";
import type { ShellNavigation } from "./shell-navigation";

/**
 * The frame the new issue page draws in (R1, R12, R14), for a reader who turned that page on:
 * what it changes in the shell `app.tsx` draws for every reader (`AppShell`), on an issue's
 * address and nowhere else.
 *
 * WHY A CHUNK. Every page of the dashboard parses the entry chunk before it draws anything, and
 * a reader who has not turned the new page on draws none of this: not the icon rail, not the
 * frame's skeleton. So this module is a chunk of its own (`frame-chunk.ts`), which the shell
 * loads only for that reader, and its imports - the rail and the attribute that keeps reference
 * cards off the page - are imported by no module of the entry. Details' drawer is not here: the
 * shell makes it from its own state, wherever this answers a frame (`AppShell`, `app.tsx`).
 *
 * WHY A FUNCTION THE SHELL CALLS, AND NOT A COMPONENT AROUND THE SHELL. A reader can turn the
 * page on or off at any moment, in this tab or another (`stored-choice.ts`), and the shell must
 * then stay mounted: a component of this chunk drawn around the shell for one choice alone would
 * make React mount the whole shell again at the change, taking the focus, open dialogs, live views
 * and every unsent field with it. So the shell draws itself the same way for both choices and
 * calls `frameOf` in its own render, over state it keeps for the frame (`FrameInput`): what this
 * answers is plain values the shell draws with, and the providers the page reads take them from
 * the shell's one frame context (`frame-context.ts`).
 */

/** What the frame is computed from: the address, and the navigation sheet's state, which the
 *  shell keeps for the frame so that a change of the choice keeps it. */
export interface FrameInput {
  /** The address the shell is on. */
  readonly location: { readonly pathname: string; readonly search: string };
  /** Whether the navigation's sheet is open. */
  readonly navigationOpen: boolean;
  /** Whether the icon rail fits: the query that is Tailwind's `md` itself, in `rem`
   *  (`MD_VIEWPORT_QUERY`), because the room the page makes for the rail is `md:pl-14`; a pixel
   *  query would draw the rail over the page wherever the two disagree, which is every width from
   *  768 to 959 px at a 20 px default font. */
  readonly railFits: boolean;
  readonly setNavigationOpen: (open: boolean) => void;
}

/** What the new issue page's frame changes in the shell, on an issue's address. */
export interface ShellFrame {
  /** What `<main>` holds while the page's code loads. */
  readonly fallback: ReactNode;
  /** `<main>`'s attributes in the frame: its references open no card on hover (R6). */
  readonly mainAttributes: Readonly<Record<string, string>>;
  /** `<main>`'s class in the frame. */
  readonly mainClass: string;
  /** What the page's header draws its `☰` from below `md`, where the frame draws neither the
   *  shell's bar nor the rail; undefined from `md` up, where the rail draws that control. */
  readonly navigation: ShellNavigation | undefined;
  /** The icon rail where the sidebar stands, or nothing below `md`. */
  readonly rail: (props: ComponentProps<typeof IconRail>) => ReactNode;
}

/** The frame of the address `input` names, `undefined` off an issue's address. */
export type FrameOf = (input: FrameInput) => ShellFrame | undefined;

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
 * The frame (R1, R14): on an issue's address, since the shell asks only for a reader who chose
 * the new page. In it an icon rail of 56 px stands where the sidebar stands, from Tailwind's
 * `md` up, and below that the page's header draws the control that opens the navigation
 * (`ShellNavigation`); the margin is Details, a drawer over the page, which the shell makes
 * (`AppShell`). Every other page keeps the shell exactly as it is for every other reader.
 */
export const frameOf: FrameOf = ({ location, navigationOpen, railFits, setNavigationOpen }) => {
  if (parseIssuePath(location.pathname, location.search) === undefined) return undefined;
  return {
    fallback: <IssueFrameFallback />,
    mainAttributes: FRAME_MAIN_ATTRIBUTES,
    mainClass: FRAME_MAIN_CLASS,
    navigation: railFits
      ? undefined
      : { onOpen: () => setNavigationOpen(true), open: navigationOpen },
    rail: (props) => (railFits ? <IconRail {...props} /> : null),
  };
};
