import { type ReactNode, useMemo, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { useLocation } from "react-router-dom";

import { primarySpec } from "../../api/issue-cache";
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
import { briefCounts, IssueThreeThings } from "./IssueThreeThings";
import { issueAddress, presetOf } from "./issue-address";
import { useIssueNames } from "./one-line";
import { useOutlineReads } from "./outline-reads";
import { pageChrome } from "./page-structure";
import { CONTROL_LABELS, type Preset } from "./page-words";
import type { SlotProps } from "./slot-props";
import { ContentsSentences, ContentsSlot } from "./slots/contents";
import { specSummary } from "./spec-summary";
import { outlineOf, useStartOutlineRead } from "./tree-read/tree-read";

/** The width the Contents pane stands beside the outline from (R8, R14): 1024 px, in `rem` so
 *  it switches with Tailwind's `lg:` at any default font size. */
export const CONTENTS_PANE_QUERY = "(min-width: 64rem)";

/** The page's inset from the edges of its column, the prototype's: 28 px from the phone's width
 *  up, 14 px below it. The Contents pane takes none, and stands against the rail. */
export const PAGE_INSET = "px-3.5 md:px-7";

/** Whether the view `preset` draws the Contents list beside it (R13): the pane from 1024 px up,
 *  and the Outline control below. So in Conversation, in each filter and in Graph, the views
 *  whose page holds an outline read for the list to read (`outlineOf`), and in no other view. */
export function drawsContents(preset: Preset): boolean {
  return outlineOf(preset) !== undefined;
}

/**
 * The Contents pane at the left of the issue page (R14, R8, R13, the prototype's `#left`), which
 * draws `slots/contents.tsx`. It is drawn from 1024 px up beside Conversation, each filter and
 * Graph (`drawsContents`), and in no other view. Below 1024 px the same slot is in the sheet
 * the Outline control opens (`OutlineSheet`).
 *
 * It stands under the band that stays at the top of the screen (`identity-block.ts`'s
 * `--issue-identity-height`), as tall as the window less that band, and scrolls what it holds
 * inside itself.
 */
export function IssueContentsPane(props: SlotProps): ReactNode {
  const paned = useMediaQuery(CONTENTS_PANE_QUERY);
  const preset = presetOf(props.route);
  if (!paned || !drawsContents(preset)) return null;
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
 * sheet it opens over the page, from 56 px under the top to the foot. The view row draws it in
 * Conversation, in each filter and in Graph (`drawsContents`). The sheet holds, from its head
 * down, `tools` - the controls the view row moves here on a phone, which a later part hands it -
 * and `slots/contents.tsx`, whose list closes the sheet once an entry is chosen
 * (`contents-chosen.ts`'s `useContentsChosen`). Escape and its Close close it.
 *
 * Below 1024 px each of those views draws the issue's three sentences above its first row
 * (`ThreeThingsAbove`, and the Spec view's own first group), so the sheet draws the list and not
 * the sentences (`ContentsSentences`), and they are drawn once (R13).
 *
 * The list reads the outline read the page holds: Graph's in Graph, and Conversation's in every
 * other view that draws the sheet. On Spec loaded cold nothing has sent Conversation's read, since
 * Spec's own view read holds what the view draws, so the press that opens the sheet sends it;
 * where the page holds it, the press sends nothing (`useStartOutlineRead`).
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
  const startOutline = useStartOutlineRead(slot.issue.key);
  const preset = presetOf(slot.route);
  const readsConversation = outlineOf(preset) === "conversation";
  return (
    <>
      {/* The prototype's `#viewbar .ob`: 34 px, beside the view control, inside the view row,
          which takes the header's 24 px target size rather than the 44 px floor. */}
      <button
        aria-expanded={open}
        aria-haspopup="dialog"
        className={`shrink-0 rounded-lg border px-3 py-1.5 text-sm leading-5 font-semibold ${pageHairBorder} ${pagePaperBg} ${pageInkText} ${focusVisibleRing}`}
        onClick={() => {
          if (readsConversation) startOutline();
          setOpen(true);
        }}
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
                  <ContentsSentences.Provider value={false}>
                    <ContentsSlot {...slot} />
                  </ContentsSentences.Provider>
                </ContentsChosen.Provider>
              </div>
            </div>,
            document.body
          )
        : null}
    </>
  );
}

/**
 * The issue's three sentences and the two rows under them above the view's first row below
 * 1024 px, where no Contents pane stands to hold them (R13, root R8): `IssueThreeThings`, in the
 * page's column as the Spec view draws its first group (`look="spec"`), in Conversation,
 * Verification, Plan, Decisions and Graph. The stage draws it above the slot it chooses
 * (`slots/stage.tsx`). Spec draws the sentences as its own first group, so this draws none while
 * the stage draws the Spec view (`view`); where it draws the outline in that view's place, the
 * sentences stand here. From 1024 px up the pane holds them, and in a view that draws no Contents
 * list (`drawsContents`) nothing does.
 */
export function ThreeThingsAbove({
  view,
  ...props
}: SlotProps & {
  /** Whether the stage draws the view's own slot, and not the outline in its place. */
  readonly view: boolean;
}): ReactNode {
  const paned = useMediaQuery(CONTENTS_PANE_QUERY);
  const preset = presetOf(props.route);
  if (paned || !drawsContents(preset) || (preset === "spec" && view)) return null;
  return <ThreeThingsOf {...props} />;
}

/** The three sentences and the two rows under them as the Contents slot reads them
 *  (`slots/contents.tsx`): the spec's summary node of the tree the page holds, each reference it
 *  cites by the page's name for it, and the rows' counts over that tree and the words filter the
 *  view row counts its signals over (`briefCounts`). Its reads are the page's own, so it sends
 *  none. */
function ThreeThingsOf({ issue, route }: SlotProps): ReactNode {
  const { hash, search } = useLocation();
  const specSlug = primarySpec(issue)?.slug ?? "";
  const { filter, preset } = issueAddress(route, search, hash, specSlug);
  const { namesHeld, readsHeld, root } = useOutlineReads(issue);
  const summary = useMemo(() => specSummary(root, specSlug), [root, specSlug]);
  const counts = useMemo(
    () => briefCounts({ namesHeld, readsHeld, root }, { filter, preset }),
    [filter, namesHeld, preset, readsHeld, root]
  );
  const lineNames = useIssueNames(issue.key);
  return (
    <IssueThreeThings
      counts={counts}
      issueKey={issue.key}
      lineNames={lineNames}
      look="spec"
      summary={summary}
    />
  );
}
