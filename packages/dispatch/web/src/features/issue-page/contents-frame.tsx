import { type ReactNode, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { useLocation } from "react-router-dom";

import {
  focusVisibleRing,
  pageHairBorder,
  pageInkText,
  pageLineBorder,
  pageMutedText,
  pagePaneBg,
  pagePaperBg,
} from "../../theme/classes";
import { NO_HOVER_PREVIEW_ATTRIBUTE } from "../refs/ref-preview-card";
import { useDialog, useMediaQuery } from "../shell/useDialog";
import { ContentsChosen } from "./contents-chosen";
import { issueAddress } from "./issue-address";
import { pageChrome } from "./page-structure";
import { CONTROL_LABELS } from "./page-words";
import type { SlotProps } from "./slot-props";
import { ContentsSlot } from "./slots/contents";

/** The width the Contents pane stands beside the outline from (R8, R14): 1024 px, in `rem` so
 *  it switches with Tailwind's `lg:` at any default font size. */
export const CONTENTS_PANE_QUERY = "(min-width: 64rem)";

/** The page's inset from the edges of its column, the prototype's: 28 px from the phone's width
 *  up, 14 px below it. The Contents pane takes none, and stands against the rail. */
export const PAGE_INSET = "px-3.5 md:px-7";

/**
 * The Contents pane at the left of the issue page (R14, R8, the
 * prototype's `#left`), which draws `slots/contents.tsx`. It is drawn from 1024 px up with no
 * view on, beside the outline; under a view it is not drawn, and the view's body has the whole
 * width (the architect's ruling 5), and neither is it on Dashboard, which draws no outline
 * (dispatch://INFRA-5702/spec#b-r-new-page). Below 1024 px the same slot is in the sheet the
 * Outline control opens (`OutlineSheet`).
 *
 * It stands under the band that stays at the top of the screen (`identity-block.ts`'s
 * `--issue-identity-height`), as tall as the window less that band, and scrolls what it holds
 * inside itself.
 */
export function IssueContentsPane(props: SlotProps): ReactNode {
  const paned = useMediaQuery(CONTENTS_PANE_QUERY);
  const { hash, search } = useLocation();
  // Only Conversation draws the outline beside it: every filter draws its own view, and the
  // issue's own address opens Dashboard unless it names a node or a words filter.
  const outline = issueAddress(props.route, search, hash, undefined).preset === "conversation";
  if (!paned || !outline) return null;
  return (
    <aside
      aria-label={CONTROL_LABELS.contents}
      className={`sticky top-[var(--issue-identity-height,0px)] flex h-[calc(100dvh-var(--issue-identity-height,0px))] w-[240px] shrink-0 flex-col overflow-y-auto border-r min-[1241px]:w-[260px] ${pageLineBorder} ${pagePaneBg}`}
      data-testid="issue-contents"
    >
      <ContentsSlot {...props} />
    </aside>
  );
}

/**
 * What the page keeps behind the Outline control below 1024 px, as the 390 px picture draws it
 * (the prototype's `#viewbar .ob` and `#sheet`): the control beside the one view control, and the
 * sheet it opens over the page, from 56 px under the top to the foot. The sheet holds, from its
 * head down, `tools` - the controls the view row moves here on a phone, which a later part
 * hands it - and `slots/contents.tsx`, whose list closes the sheet once an entry is chosen
 * (`contents-chosen.ts`'s `useContentsChosen`). Escape and its Close close it.
 */
export function OutlineSheet({
  slot,
  tools,
}: {
  /** The page's slot props, which the sheet hands `slots/contents.tsx`. */
  slot: SlotProps;
  tools?: ReactNode;
}): ReactNode {
  const [open, setOpen] = useState(false);
  const closeButton = useRef<HTMLButtonElement>(null);
  const dialog = useDialog<HTMLDivElement>({
    initialFocusRef: closeButton,
    onClose: () => setOpen(false),
    open,
  });
  return (
    <>
      {/* The prototype's `#viewbar .ob`: 34 px, beside the view control, inside the view row,
          which takes the header's 24 px target size rather than the 44 px floor. */}
      <button
        aria-expanded={open}
        aria-haspopup="dialog"
        className={`shrink-0 rounded-lg border px-3 py-1.5 text-sm leading-5 font-semibold ${pageHairBorder} ${pagePaperBg} ${pageInkText} ${focusVisibleRing}`}
        onClick={() => setOpen(true)}
        type="button"
        {...pageChrome}
      >
        {CONTROL_LABELS.outline}
      </button>
      {/* Drawn in the body, not in the view row, which is sticky on a phone and so a stacking
          context of its own: there the sheet would stand under the shell's status corner. Out
          of `<main>`, it carries the frame's mark itself, so its references open no card on
          hover. */}
      {open
        ? createPortal(
            <div
              aria-label={CONTROL_LABELS.contents}
              aria-modal="true"
              className={`fixed inset-x-0 top-14 bottom-0 z-50 flex flex-col rounded-t-[14px] shadow-[0_-10px_30px_rgba(0,0,0,0.2)] ${pagePaneBg} ${pageInkText}`}
              data-testid="issue-outline-sheet"
              ref={dialog.containerRef}
              role="dialog"
              {...{ [NO_HOVER_PREVIEW_ATTRIBUTE]: "" }}
            >
              <div className="flex shrink-0 items-start gap-2 px-3.5 pt-2.5">
                <div className="flex min-w-0 flex-1 flex-wrap items-center gap-2">{tools}</div>
                <button
                  className={`rounded-[7px] border px-3 py-[3px] text-sm font-medium ${pageHairBorder} ${pagePaperBg} ${pageMutedText} ${focusVisibleRing}`}
                  onClick={() => setOpen(false)}
                  ref={closeButton}
                  type="button"
                  {...pageChrome}
                >
                  {CONTROL_LABELS.close}
                </button>
              </div>
              <div className="flex min-h-0 flex-1 flex-col overflow-y-auto">
                <ContentsChosen.Provider value={() => setOpen(false)}>
                  <ContentsSlot {...slot} />
                </ContentsChosen.Provider>
              </div>
            </div>,
            document.body
          )
        : null}
    </>
  );
}
