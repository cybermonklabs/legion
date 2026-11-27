import { type ReactNode, type RefObject, useEffect, useLayoutEffect, useRef } from "react";

import type { Artifact, Ask, DispatchEvent } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { FOOT_COMPOSER_FRAME_BOUND, useFootInset } from "../../hooks/useFootInset";
import {
  activeTabIndicatorBorder,
  activeTabIndicatorText,
  backdrop50,
  borderDefault,
  card,
  dragHandleBg,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import type { ComposerKind } from "../conversation/composer-model";
import type { SaveCommentEdit } from "../conversation/send-request";
import type { AwayDraft } from "../conversation/unsent-drafts";
import { useMarginDrawer, useMarginSlotFill } from "../issue-page/margin-slot";
import {
  COMPACT_VIEWPORT_QUERY,
  PHONE_VIEWPORT_QUERY,
  useDialog,
  useMediaQuery,
} from "../shell/useDialog";
import {
  CommentsTab,
  type MarginComposer,
  MarginComposerSlot,
  MarginKeptDrafts,
} from "./CommentsTab";
import { marginReplySendKey, useDetailsDrawsConversation, useMargin } from "./margin-context";
import { PinnedTab } from "./PinnedTab";
import { publishReviewSheetSlots } from "./review-sheet-slots";
import { ThreadCard } from "./ThreadCard";
import type { CommentActionFailure } from "./useCommentActionQueue";
import type {
  DecisionMarginItem,
  MarginItemAction,
  MarginOwner,
  MarginTab,
  MarkPlacement,
  PinnedConversationCard,
  Thread,
} from "./useMarginItems";

export interface MarginSheetModel {
  actions: {
    /** The composer's Close, Escape and Discard: the open compose ends unsaved, and a refusal the
     *  store holds for the document goes, its mark with it. */
    closeComposer: (composer: MarginComposer) => void;
    onAction: (id: string, action: MarginItemAction) => void;
    /** The kind switch of the composer on the document `artifact` (`retypeCompose`). */
    onComposerKindChange: (artifact: string, kind: ComposerKind) => string | undefined;
    onEdit: SaveCommentEdit;
    onRetryAction: () => void;
    /** Discard my draft on a draft the compose kept for words of the document (`keptDrafts`). */
    discardKeptDraft: (away: AwayDraft) => void;
    onUnpin: (eventId: number) => void;
    onUnpinConversation: (id: string) => void;
    onRetryAnsweredAsk: (() => void) | undefined;
    onRetryComments: () => void;
    onRetryIssue: () => void;
    onToggleThread: (key: string) => void;
    onEditingChange: (id: string | undefined) => void;
    onToggleResolved: () => void;
  };
  /** The composer on the open document, if the reader has one there. */
  composer: MarginComposer | undefined;
  items: {
    actionFailure: CommentActionFailure | undefined;
    answeredAsksPending: boolean;
    asksPending: boolean;
    commentsError: boolean;
    commentsPending: boolean;
    /** The decisions beside the spec's blocks, in the spec's order; none beside any other
     *  document. */
    decisions: readonly DecisionMarginItem[];
    historicalAsks: Ask[];
    marginRef: RefObject<HTMLElement | null>;
    isClosed: boolean;
    issueError: boolean;
    owner: MarginOwner | undefined;
    issuePending: boolean;
    /** What the margin's compose kept for words of the open document (`AwayDraft.anchor`), which
     *  the margin draws while no compose is open there. */
    keptDrafts: readonly AwayDraft[];
    needsYou: Ask[];
    onSelectCard: (id: string, blockID?: string) => void;
    openAskCount: number;
    pendingActionIds: ReadonlySet<string>;
    pinned: DispatchEvent[];
    pinnedConversations: PinnedConversationCard[];
    pinnedIds: string[];
    resolvedThreads: Thread[];
    /** Retracted and withdrawn asks hidden behind the same toggle as resolved threads. */
    foldedAskCount: number;
    threads: Thread[];
    viewerLogin: string;
    visibleArtifact: Artifact | undefined;
  };
  placement: {
    blockPlacements: ReadonlyMap<string, MarkPlacement>;
    markPlacements: ReadonlyMap<string, MarkPlacement>;
  };
  selection: {
    expandedThreadKey: string | undefined;
    editingCommentId: string | undefined;
    savingCommentEditId: string | undefined;
    hoveredItemId: string | undefined;
    hoveredMarkId: string | undefined;
    /** The address's landing on a card beside its document, keyed per navigation, once the
     *  margin holds that card (`useMarginSheet.ts`); undefined while the address names none. In
     *  the new issue page's Details, a closed drawer opens for it (`Margin.tsx`). */
    landedCard: string | undefined;
    /** The item the address names where it is a node of the new issue page's outline rather
     *  than a card beside an open document: marked, as the address's own landing, and never the
     *  reader's pick, so it opens no Details (`Margin.tsx`). Undefined on every other page. */
    routeMarkedItemId: string | undefined;
    selectedItemId: string | undefined;
    showResolved: boolean;
  };
  sheet: {
    /** Back and Escape: refused while the thread's reply is out, until the send's deadline. Past
     *  it they close the thread and the reply keeps its send; otherwise they drop the reply, its
     *  draft and a refusal with it, as a Conversation phone thread's do. */
    closeThread: () => void;
    expanded: boolean;
    /** That reply is out: Back holds. */
    replySending: boolean;
    thread: Thread | undefined;
    toggle: (expanded?: boolean) => void;
  };
  filter: {
    blockId: string | undefined;
    clear(): void;
  };
  tab: {
    set: (tab: MarginTab) => void;
    value: MarginTab;
  };
}

interface MarginSheetProps {
  /** The desktop margin is collapsed to its rail: the sheet stays mounted, hidden, holding only
   *  its composer. */
  collapsed?: boolean;
  desktopControl?: ReactNode;
  /** The sheet is the body of the new issue page's Details drawer (`Margin.tsx`'s
   *  `DetailsDrawer`): a box that scrolls inside the drawer at every width, with no sheet of its
   *  own below `xl`, which draws only what the page put in the margin's slot - the slot's fill,
   *  handed this sheet's content - and nothing while the page has put nothing there
   *  (R14). False everywhere else. */
  drawer?: boolean;
  model: MarginSheetModel;
}

/**
 * The document's review margin. Its groups, in this order (the layout rule of
 * `packages/dispatch/web/src/AGENTS.md`, dispatch://INFRA-5200):
 *
 * 1. Compact toggle: below `xl`, the sheet's handle and its Open / Close review panel button,
 *    one row at the foot of the screen while the sheet is collapsed.
 * 2. Tabs and desktop control: the margin's tabs, then the control that collapses the margin to
 *    its rail. The control is drawn from `xl` up only; below it the tabs have the row alone.
 * 3. Filter and Clear: while a reader has filtered the margin to one block from the document,
 *    "References for this block" and Clear filter, on a row of their own under the tabs; the
 *    words wrap inside their item, and Clear keeps its place at the row's end.
 * 4. Composer and content: the margin's compose, then the cards of the open tab, one per line.
 * 5. Phone thread: on a phone an opened thread is a full-screen view in place of the body, with
 *    its own composer at the foot; Back returns to the list.
 *
 * Below `xl` the margin is a sheet fixed at the foot of the screen, collapsed to its toggle or
 * expanded as a modal dialog up to 85dvh; from `xl` up it is a sticky column beside the
 * document. While it is expanded it publishes its height as `--margin-sheet-height`, and the
 * editor's selection bar docks 12 px above the sheet's top edge (`styles.css`), so the bar
 * neither covers the sheet's controls nor leaves the reader's reach.
 */
export function MarginSheet({
  collapsed = false,
  desktopControl,
  drawer = false,
  model,
}: MarginSheetProps): ReactNode {
  const {
    actions,
    composer,
    items: {
      actionFailure,
      answeredAsksPending,
      asksPending,
      commentsError,
      commentsPending,
      decisions,
      historicalAsks,
      marginRef,
      isClosed,
      issueError,
      owner,
      issuePending,
      keptDrafts,
      openAskCount,
      pendingActionIds,
      pinned,
      pinnedConversations,
      pinnedIds,
      needsYou,
      onSelectCard,
      resolvedThreads,
      foldedAskCount,
      threads,
      viewerLogin,
      visibleArtifact,
    },
    filter,
    placement,
    selection,
    sheet,
    tab,
  } = model;
  const isCompactViewport = useMediaQuery(COMPACT_VIEWPORT_QUERY);
  const isPhoneViewport = useMediaQuery(PHONE_VIEWPORT_QUERY);
  // Below `xl` the margin is a sheet at the foot of the screen, except in Details, which is the
  // same box at every width.
  const compactSheet = isCompactViewport && !drawer;
  // Details is on screen only while it is open: a composer mounted in a closed Details takes the
  // focus once the drawer opens for it (`Margin.tsx`), not while it is hidden.
  const drawerOpen = useMarginDrawer()?.open === true;
  const sheetDragOrigin = useRef<number | undefined>(undefined);
  const sheetDragMoved = useRef(false);
  const dialog = useDialog<HTMLElement>({
    onClose: () => sheet.toggle(false),
    open: sheet.expanded && compactSheet,
  });

  const reviewToggleLabel = `${sheet.expanded ? "Close" : "Open"} review panel (${openAskCount} open ${
    openAskCount === 1 ? "ask" : "asks"
  })`;
  const phoneThread = sheet.thread;
  if (phoneThread !== undefined && visibleArtifact === undefined) {
    throw new Error("A phone margin thread requires its visible artifact.");
  }
  const threadDialog = useDialog<HTMLElement>({
    onClose: sheet.closeThread,
    open: isPhoneViewport && phoneThread !== undefined,
  });
  // What the margin's own tabs give way to: the rail it collapses to, a compact sheet nothing
  // has named a card in and the reader has not opened, or a phone margin thread.
  //
  // A shut compact sheet is what a reader opening a document meets. Its tabs are
  // `display: none` there - the sheet is 64 px of `Review (N)`, 44 on a phone, until it is
  // dragged up - and
  // the cards behind them cost what a card costs: a read of every ask's thread
  // (`GET /asks/{id}`, one each), and every ask and comment body drawn through the headless
  // engine, on the thread the document beside them is waiting for. On a phone opening a
  // document of five decision blocks that was five reads and about a second of main thread in
  // front of the document's own first draw (dispatch://INFRA-341, R11). None of it is on
  // screen, so none of it is drawn until the reader opens the sheet; the count beside `Review`
  // is the margin's own asks query, which is read either way, and the composer below keeps its
  // draft as it does under the rail.
  //
  // A card the route named is the exception, and is drawn in the shut sheet as it always was:
  // the landing selects it, places it and scrolls the sheet to it before the reader opens the
  // sheet over it (`useMarginListeners`, `e2e/deep-links.e2e.ts`, "an iPhone secondary-document
  // link scrolls its selected card after opening the review sheet"), and a card that is not
  // there yet is a landing that cannot happen.
  //
  // The new issue page's Details drawer is no shut sheet: the sheet is its body at every width,
  // so the rule is the compact sheet's alone (`compactSheet`).
  const unnamedShutSheet =
    compactSheet && !sheet.expanded && selection.selectedItemId === undefined;
  const covered = collapsed || unnamedShutSheet || (isPhoneViewport && phoneThread !== undefined);
  // Whether a reader sees the margin: the desktop column up, a compact sheet they have opened, or
  // Details open and drawing the conversation, which Details draws only once the document's editor
  // is bound (`useDetailsDrawsConversation` in `margin-context.tsx`, which `DetailsFill` in
  // `issue-page/slots/details.tsx` draws by: the margin's `documentBridge`, which
  // `ProofDocument`'s `bindEditor` sets after the build). While a reader sees it, its comments and
  // its answered asks are read before the document's first batch (`useMarginItems`,
  // `features/doc/first-batch.ts`), and the open document places the margin's cards against its
  // blocks and marks (`features/doc/ProofDocument.tsx`'s `placed`, dispatch://INFRA-341
  // requirement R11). Details open during the first load's build draws none of the margin's cards,
  // so the reads wait for the first batch there. The composer below reads `seen` for its
  // `onScreen`.
  const { setPlacesItems } = useMargin();
  const detailsDrawsConversation = useDetailsDrawsConversation();
  const seen = drawer
    ? drawerOpen && detailsDrawsConversation
    : sheet.expanded || !isCompactViewport;
  // A margin with no owner shows nothing, and a covered one has given its room away.
  const placesItems = owner !== undefined && !covered && seen;
  useEffect(() => {
    setPlacesItems(placesItems);
    return () => setPlacesItems(false);
  }, [placesItems, setPlacesItems]);
  // Collapsed below `xl`, the sheet is a bar fixed at the foot of the screen, in the foot's page
  // layer (`hooks/useFootInset.ts` lists the foot's layers and the elements in each), so the hook
  // holds its band, its box on the screen, and the floats keep clear of it. Expanded, it is a
  // modal sheet over the page, and adds nothing. The dialog's container is this same `<aside>`.
  useFootInset(dialog.containerRef, compactSheet && owner !== undefined && !sheet.expanded, "page");
  // How tall the sheet stands while it is the modal one, on the document element as
  // `--margin-sheet-height`. The editor's selection bar is no element of ours - the library
  // fixes it at the foot of a touch screen, where this sheet is - so `styles.css` lifts the bar
  // above the sheet by this height, and the reader can still send a selection of the document to
  // the margin's view while the panel is open (dispatch://INFRA-5200). A ResizeObserver on the
  // sheet publishes it, since the sheet's height follows its content up to 85dvh with no render
  // of this component: a card's own state, a picture loading, or the dvh changing as a phone's
  // toolbar collapses all change it.
  const modalSheet = compactSheet && sheet.expanded;
  useLayoutEffect(() => {
    const root = document.documentElement;
    const element = dialog.containerRef.current;
    if (!modalSheet || element === null) return;
    const publish = () => {
      const height = Math.round(element.getBoundingClientRect().height);
      root.style.setProperty("--margin-sheet-height", `${height}px`);
    };
    publish();
    const sizes = new ResizeObserver(publish);
    sizes.observe(element);
    return () => {
      sizes.disconnect();
      root.style.removeProperty("--margin-sheet-height");
    };
  }, [dialog.containerRef, modalSheet]);
  // A phone's sheet on an issue's page holds the document's own chrome (`review-sheet-slots.ts`):
  // the toolbar's controls and the open-decisions bar in its body, and the version the page shows
  // and the count of open decisions in its collapsed bar. Only there: the issue's header and tab
  // strip stand above the document there, and the first block stood under the sheet. A project
  // document's page has neither above its document, and keeps its toolbar and bar where they are.
  const phoneSheet = compactSheet && isPhoneViewport && owner?.kind === "issue";
  const versionSlot = useRef<HTMLSpanElement>(null);
  const countSlot = useRef<HTMLSpanElement>(null);
  const controlsSlot = useRef<HTMLDivElement>(null);
  const decisionsSlot = useRef<HTMLDivElement>(null);
  // A control in the sheet that takes the reader into the document (`leave`): its move waits for
  // the sheet to shut. The dialog gives the focus back to the toggle in its passive cleanup
  // (`useDialog`), which runs before this effect of the same commit, so the move's own scroll
  // and focus are the last the reader is given rather than the toggle's.
  const sheetRef = useRef(sheet);
  sheetRef.current = sheet;
  const pendingMove = useRef<(() => void) | undefined>(undefined);
  useEffect(() => {
    if (sheet.expanded) return;
    const move = pendingMove.current;
    pendingMove.current = undefined;
    move?.();
  }, [sheet.expanded]);
  useLayoutEffect(() => {
    const version = versionSlot.current;
    const count = countSlot.current;
    const controls = controlsSlot.current;
    const decisions = decisionsSlot.current;
    if (
      !phoneSheet ||
      version === null ||
      count === null ||
      controls === null ||
      decisions === null
    ) {
      return;
    }
    const leave = (move: () => void) => {
      if (!sheetRef.current.expanded) {
        move();
        return;
      }
      pendingMove.current = move;
      sheetRef.current.toggle(false);
    };
    return publishReviewSheetSlots({ controls, count, decisions, leave, version });
  }, [phoneSheet]);

  return (
    <>
      {sheet.expanded && compactSheet ? (
        <div
          aria-hidden="true"
          className={`fixed inset-0 z-[9] ${backdrop50}`}
          onClick={() => sheet.toggle(false)}
        />
      ) : null}
      {/* biome-ignore lint/a11y/useAriaPropsSupportedByRole: aria-modal only applies while the compact review sheet is an open dialog */}
      <aside
        aria-label={compactSheet ? "Review panel" : "Review margin"}
        aria-modal={sheet.expanded && compactSheet ? true : undefined}
        className={
          drawer
            ? "min-h-0 flex-1 overflow-y-auto px-4 pt-1.5 pb-8"
            : `fixed inset-x-0 bottom-0 ${sheet.expanded && isCompactViewport ? "z-30" : "z-10"} border-t shadow-[0_-8px_24px_rgba(15,23,42,0.08)] ${card} ${
                owner === undefined
                  ? "hidden"
                  : sheet.expanded
                    ? "max-h-[85dvh] overflow-y-auto p-4"
                    : // 64 px, and 44 px on a phone: a touch target's floor, which leaves the
                      // document's first block whole above the sheet at 390 by 664 px
                      // (`review-sheet-slots.ts`).
                      "h-16 overflow-hidden max-md:h-11"
              } xl:sticky xl:top-0 xl:block xl:h-dvh xl:max-h-dvh xl:w-full xl:overflow-y-auto xl:border-t-0 xl:border-l xl:p-4 xl:shadow-none`
        }
        data-expanded={sheet.expanded ? "true" : "false"}
        data-testid="margin-sheet"
        id="review-margin"
        ref={(element) => {
          dialog.containerRef.current = element;
          marginRef.current = element;
        }}
        role={sheet.expanded && compactSheet ? "dialog" : undefined}
      >
        {compactSheet ? (
          <>
            {/* On a phone the collapsed bar is the toggle's 44 px alone, so the handle stands
                over the top of the toggle rather than above it. */}
            <div
              aria-hidden="true"
              className={`mx-auto mb-1 h-1 w-10 rounded-full ${dragHandleBg} ${
                sheet.expanded
                  ? ""
                  : "max-md:pointer-events-none max-md:absolute max-md:inset-x-0 max-md:top-1 max-md:mb-0"
              }`}
            />
            <button
              aria-expanded={sheet.expanded}
              aria-label={reviewToggleLabel}
              className={`flex h-16 w-full items-center justify-between gap-3 px-4 text-left text-sm font-semibold max-md:h-11 ${textPrimaryOnSurface}`}
              onClick={() => {
                if (sheetDragMoved.current) {
                  sheetDragMoved.current = false;
                  return;
                }
                sheet.toggle();
              }}
              onPointerDown={(event) => {
                sheetDragOrigin.current = event.clientY;
              }}
              onPointerUp={(event) => {
                const origin = sheetDragOrigin.current;
                sheetDragOrigin.current = undefined;
                if (origin === undefined || Math.abs(event.clientY - origin) < 12) {
                  return;
                }
                sheetDragMoved.current = true;
                sheet.toggle(event.clientY < origin);
              }}
              type="button"
            >
              <span className="shrink-0">Review ({openAskCount})</span>
              {/* The version the page shows and the count of open decisions, which the document
                  draws here on a phone (`review-sheet-slots.ts`): words of the bar, which a cut
                  version name ends in an ellipsis of. */}
              {phoneSheet ? (
                <span className="flex min-w-0 items-center gap-2 text-xs font-medium">
                  <span className="min-w-0 truncate" ref={versionSlot} />
                  <span className="shrink-0" ref={countSlot} />
                </span>
              ) : null}
            </button>
            {/* The document's toolbar controls and its open-decisions bar, on a phone, in the
                order the page draws them beside the document at every other width. */}
            {phoneSheet ? (
              <div className="space-y-3 px-4 pb-3" hidden={!sheet.expanded}>
                <div ref={controlsSlot} />
                <div ref={decisionsSlot} />
              </div>
            ) : null}
          </>
        ) : null}
        {/* The tabs and what they show. Mounted under a phone margin thread and under the
            collapsed rail too, holding only the composer then, so an unsent draft survives
            whatever the margin shows over it; a send it has out, and that send's refusal, are the
            held-send store's, and survive anything. */}
        <SheetBody
          className={sheet.expanded ? "px-4 pb-4 xl:px-0 xl:pb-0" : "hidden xl:block"}
          drawer={drawer}
          hidden={covered}
        >
          {covered ? null : (
            <>
              <div className={`flex items-center justify-between border-b ${borderDefault}`}>
                <div className="flex" role="tablist">
                  {(owner?.kind === "document"
                    ? (["comments"] as MarginTab[])
                    : owner?.kind === "issue"
                      ? (["comments", "pinned"] as MarginTab[])
                      : []
                  ).map((name) => (
                    <button
                      aria-selected={tab.value === name}
                      className={
                        tab.value === name
                          ? `min-h-11 border-b-2 px-3 py-2 text-sm font-semibold ${activeTabIndicatorBorder} ${activeTabIndicatorText}`
                          : `min-h-11 px-3 py-2 text-sm ${textSecondaryOnSurface}`
                      }
                      key={name}
                      onClick={() => tab.set(name)}
                      role="tab"
                      type="button"
                    >
                      {name === "comments" ? "Comments" : "Pinned"}
                    </button>
                  ))}
                </div>
                {desktopControl === undefined ? null : (
                  <span className="hidden xl:block">{desktopControl}</span>
                )}
              </div>
              {owner === undefined ? (
                <p className={`pt-3 text-sm ${textMutedOnSurface}`}>
                  Open an issue or document to review its margin.
                </p>
              ) : null}
              {owner?.kind === "issue" && visibleArtifact === undefined && issuePending ? (
                <p className={`pt-3 text-sm ${textMutedOnSurface}`}>Loading margin…</p>
              ) : null}
              {owner?.kind === "issue" && visibleArtifact === undefined && issueError ? (
                <div className="pt-3">
                  <QueryError
                    message="Could not load this issue's margin."
                    onRetry={actions.onRetryIssue}
                  />
                </div>
              ) : null}
              {tab.value === "pinned" && owner?.kind === "issue" ? (
                <PinnedTab
                  conversations={pinnedConversations}
                  events={pinned}
                  onUnpin={actions.onUnpin}
                  onUnpinConversation={actions.onUnpinConversation}
                  pinnedIds={pinnedIds}
                />
              ) : null}
              {filter.blockId === undefined ? null : (
                <div
                  className={`mt-3 flex items-center justify-between gap-2 text-sm ${textMutedOnSurface}`}
                >
                  <span>References for this block</span>
                  <button
                    className={`min-h-11 font-medium ${textPrimaryOnSurface}`}
                    onClick={filter.clear}
                    type="button"
                  >
                    Clear filter
                  </button>
                </div>
              )}
            </>
          )}
          {/* Keyed by its document and its compose: a newer compose on the document is the host's
              move for the one before it, so it draws a composer of its own, which takes nothing
              of that compose's draft, while Send, a kind switch and a re-written mark keep the
              one the reader is writing in. */}
          {composer === undefined ? null : (
            <MarginComposerSlot
              composer={composer}
              hidden={tab.value !== "comments"}
              isClosed={isClosed}
              key={`${composer.anchor.artifact}:${composer.seq ?? ""}`}
              onClose={actions.closeComposer}
              onKindChange={actions.onComposerKindChange}
              onScreen={!covered && tab.value === "comments" && seen}
            />
          )}
          {/* With no compose open on the document, what the margin's compose kept for its words
              is drawn here: the compose that held it ended with nothing the reader pressed - a
              reload, a sign-in after a lapse, its issue's closing - and nothing else would show
              it until the next compose on the document. */}
          {composer === undefined && visibleArtifact !== undefined ? (
            <MarginKeptDrafts
              drafts={keptDrafts}
              hidden={tab.value !== "comments"}
              onDiscard={actions.discardKeptDraft}
            />
          ) : null}
          {tab.value === "comments" &&
          !covered &&
          owner !== undefined &&
          visibleArtifact !== undefined ? (
            <CommentsTab
              actionFailure={actionFailure}
              answeredAsksPending={answeredAsksPending}
              artifactSlug={visibleArtifact.slug}
              asksPending={asksPending}
              commentsError={commentsError}
              commentsPending={commentsPending}
              decisions={decisions}
              expandedThreadKey={selection.expandedThreadKey}
              editingCommentId={selection.editingCommentId}
              savingCommentEditId={selection.savingCommentEditId}
              onEditingChange={actions.onEditingChange}
              historicalAsks={historicalAsks}
              hoveredItemId={selection.hoveredItemId}
              hoveredMarkId={selection.hoveredMarkId}
              isClosed={isClosed}
              owner={owner}
              markPlacements={placement.markPlacements}
              blockPlacements={placement.blockPlacements}
              needsYou={needsYou}
              onAction={actions.onAction}
              onEdit={actions.onEdit}
              onRetryAction={actions.onRetryAction}
              onRetryAnsweredAsk={actions.onRetryAnsweredAsk}
              onRetryComments={actions.onRetryComments}
              onSelectCard={onSelectCard}
              onToggleResolved={actions.onToggleResolved}
              onToggleThread={actions.onToggleThread}
              pendingActionIds={pendingActionIds}
              resolvedThreads={resolvedThreads}
              foldedAskCount={foldedAskCount}
              selectedItemId={selection.selectedItemId}
              showResolved={selection.showResolved}
              threads={threads}
              viewerLogin={viewerLogin}
            />
          ) : null}
        </SheetBody>
        {isPhoneViewport &&
        phoneThread !== undefined &&
        owner !== undefined &&
        visibleArtifact !== undefined ? (
          <section
            aria-label="Thread"
            aria-modal="true"
            className={`fixed inset-0 z-20 flex flex-col ${card}`}
            role="dialog"
            ref={threadDialog.containerRef}
          >
            <header className={`flex items-center border-b px-4 py-3 ${borderDefault}`}>
              <button
                className={`min-h-11 text-sm font-medium disabled:cursor-not-allowed disabled:opacity-50 ${textPrimaryOnSurface}`}
                disabled={sheet.replySending}
                onClick={sheet.closeThread}
                type="button"
              >
                Back
              </button>
            </header>
            <div className="min-h-0 flex-1 overflow-y-auto px-4">
              <ThreadCard
                actionFailure={actionFailure?.id === phoneThread.key ? actionFailure : undefined}
                artifactSlug={visibleArtifact.slug}
                className="min-h-full pb-32"
                composerClassName={`fixed inset-x-0 bottom-0 z-10 border-t px-4 pt-4 pb-2 ${FOOT_COMPOSER_FRAME_BOUND} ${card} ${borderDefault}`}
                expanded
                hovered={selection.hoveredMarkId === phoneThread.anchor?.mark_id}
                editingCommentId={selection.editingCommentId}
                savingCommentEditId={selection.savingCommentEditId}
                onEditingChange={actions.onEditingChange}
                isClosed={isClosed}
                onAction={actions.onAction}
                onEdit={actions.onEdit}
                onRetryAction={actions.onRetryAction}
                onSelect={() =>
                  onSelectCard(phoneThread.key, phoneThread.anchor?.block_id ?? undefined)
                }
                onToggle={sheet.closeThread}
                owner={owner}
                pendingAction={pendingActionIds.has(phoneThread.key)}
                replyMutationKey={marginReplySendKey(phoneThread.key)}
                thread={phoneThread}
                viewerLogin={viewerLogin}
              />
            </div>
          </section>
        ) : null}
      </aside>
    </>
  );
}

/**
 * The box under the sheet's head: the margin's tabs, its composer and its cards. Everywhere but
 * the new issue page's Details it is the sheet's own box. In Details it draws only what the page
 * put in the margin's slot (`margin-slot.tsx`), which is handed this content and answers what is
 * drawn in its place, and nothing while the page has put nothing there, so the content is drawn
 * once per load rather than plain first and again inside the page's panel (R14).
 */
function SheetBody({
  children,
  className,
  drawer,
  hidden,
}: {
  children: ReactNode;
  className: string;
  drawer: boolean;
  hidden: boolean;
}): ReactNode {
  const fill = useMarginSlotFill();
  if (!drawer) {
    return (
      <div className={className} hidden={hidden}>
        {children}
      </div>
    );
  }
  return fill === undefined ? null : <div hidden={hidden}>{fill(children)}</div>;
}
