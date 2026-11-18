import { type ReactNode, useMemo } from "react";

import type { Ask } from "../../api/types";
import { EmptyState } from "../../components/EmptyState";
import { QueryError } from "../../components/QueryError";
import { useSending } from "../../hooks/useSending";
import {
  borderDefault,
  dangerText,
  highlightRing,
  quoteAccentBorder,
  quoteBodyText,
  textMutedOnSurfaceMuted,
  textPrimaryOnSurface,
} from "../../theme/classes";
import type { ComposerAnchor, ComposerKind, ComposerOwner } from "../conversation/composer-model";
import { MentionComposer } from "../conversation/MentionComposer";
import type { SaveCommentEdit } from "../conversation/send-request";
import type { AwayDraft } from "../conversation/unsent-drafts";
import { AskCard } from "../inbox/AskCard";
import { textSourceOf } from "../refs/text-references";
import { Unfurl } from "../refs/Unfurl";
import { DecisionList } from "./DecisionCard";
import { marginComposeKeptKey, marginComposeSendKey, useMargin } from "./margin-context";
import { ThreadList } from "./ThreadList";
import type { CommentActionFailure } from "./useCommentActionQueue";
import type {
  DecisionMarginItem,
  MarginItemAction,
  MarginOwner,
  MarkPlacement,
  Thread,
} from "./useMarginItems";

/** The composer on the open document: the compose the reader has open, or the one the held-send
 *  store holds for the document - out, or refused - from Send on. At most one per document. */
export interface MarginComposer {
  anchor: ComposerAnchor;
  kind: ComposerKind;
  /** Where its sends go: the owner it was opened under. */
  owner: ComposerOwner;
  /** The compose it is, by the number it opened under (`PendingCompose.seq`): the open compose,
   *  and after its Send the held send's, which keeps the number. A send held since before a
   *  reload has none. `MarginSheet` keys the composer by it, so each compose draws its own. */
  seq: number | undefined;
  /** A newer selection-bar action had to wait for its send. */
  turnedAway: boolean;
}

interface CommentsTabProps {
  actionFailure: CommentActionFailure | undefined;
  answeredAsksPending: boolean;
  artifactSlug: string;
  asksPending: boolean;
  commentsError: boolean;
  commentsPending: boolean;
  decisions: readonly DecisionMarginItem[];
  expandedThreadKey: string | undefined;
  editingCommentId: string | undefined;
  savingCommentEditId: string | undefined;
  onEditingChange(id: string | undefined): void;
  historicalAsks: Ask[];
  hoveredItemId: string | undefined;
  hoveredMarkId: string | undefined;
  isClosed: boolean;
  owner: MarginOwner;
  blockPlacements: ReadonlyMap<string, MarkPlacement>;
  markPlacements: ReadonlyMap<string, MarkPlacement>;
  needsYou: Ask[];
  onAction: (id: string, action: MarginItemAction) => void;
  onEdit: SaveCommentEdit;
  onRetryAction: () => void;
  onRetryAnsweredAsk: (() => void) | undefined;
  onRetryComments: () => void;
  onSelectCard: (id: string, blockID: string | undefined) => void;
  onToggleThread: (key: string) => void;
  onToggleResolved: () => void;
  pendingActionIds: ReadonlySet<string>;
  resolvedThreads: Thread[];
  foldedAskCount: number;
  selectedItemId: string | undefined;
  showResolved: boolean;
  threads: Thread[];
  viewerLogin: string;
}

/** The composer on the open document. `MarginSheet` keeps it mounted, hidden, under the Pinned
 *  tab, a phone margin thread and the rail the margin collapses to, so an unsent draft stays with
 *  it; a send it has out, and that send's refusal, are the held-send store's, so they stay with
 *  the document whatever unmounts the composer. On a closed issue a comment compose - one the
 *  selection bar opened by quote, since the closed document takes no mark - shows and sends; a
 *  suggestion or ask compose shows only its send or its refusal. While a newer selection-bar
 *  action waits on its send, it says so. It takes focus the first time it is on screen: a compose
 *  mounts it before the margin has opened the compact sheet, left its rail or the Pinned tab for
 *  it, and a field out of sight takes no focus. */
export function MarginComposerSlot({
  composer,
  hidden,
  isClosed,
  onClose,
  onKindChange,
  onScreen,
}: {
  composer: MarginComposer;
  hidden: boolean;
  isClosed: boolean;
  onClose: (composer: MarginComposer) => void;
  onKindChange: (artifact: string, kind: ComposerKind) => string | undefined;
  /** The reader can see it: the margin's tabs are not covered, the compact sheet is open, and the
   *  Comments tab shows. */
  onScreen: boolean;
}): ReactNode {
  const { anchorRefusal } = useMargin();
  const sendKey = useMemo(
    () => marginComposeSendKey(composer.anchor.artifact),
    [composer.anchor.artifact]
  );
  // Where a stopped compose of this document waits once a newer compose took its place.
  const keptKey = useMemo(
    () => marginComposeKeptKey(composer.anchor.artifact),
    [composer.anchor.artifact]
  );
  const { sending } = useSending(sendKey);
  return (
    // The margin scrolls this into its scrollport when it opens: a composer the reader started
    // from the document renders at the top of the margin's scroll content, which can be thousands
    // of pixels above wherever the margin is parked.
    <div data-margin-composer="" hidden={hidden}>
      {composer.turnedAway && sending ? (
        <p className={`pt-3 text-sm ${textMutedOnSurfaceMuted}`} role="status">
          Still sending this one. Select the text again once it's sent.
        </p>
      ) : null}
      <MentionComposer
        anchor={composer.anchor}
        anchorRefusal={anchorRefusal}
        autoFocus={onScreen}
        closed={isClosed}
        frame="pt-3"
        kind={composer.kind}
        mutationKey={sendKey}
        onClose={() => onClose(composer)}
        onSent={() => {}}
        owner={composer.owner}
        replyTo={null}
        stoppedAside={keptKey}
        onKindChange={(kind) => onKindChange(composer.anchor.artifact, kind)}
      />
    </div>
  );
}

/** How a kept draft names the kind it was written as, where it is not a comment. */
const KEPT_KIND_WORDS: Record<ComposerKind, string> = {
  ask: " as an ask,",
  comment: "",
  suggestion: " as a suggestion,",
};

/**
 * What the margin's compose kept for words of the open document (`AwayDraft.anchor`), while no
 * compose is open on it (`useMarginSheet`'s `keptDrafts`). A reload, a sign-in after a lapse and
 * an issue's closing each end a compose with nothing the reader pressed, and the next compose on
 * the document would offer only the newest of its drafts, so the margin draws each here with the
 * words it was written for, the kind it was written as, and Discard my draft. A compose opened on
 * the document offers it beside its field, where Use my draft takes it in.
 */
export function MarginKeptDrafts({
  drafts,
  hidden,
  onDiscard,
}: {
  drafts: readonly AwayDraft[];
  hidden: boolean;
  onDiscard: (away: AwayDraft) => void;
}): ReactNode {
  const kept = drafts.filter(
    (away): away is AwayDraft & { anchor: ComposerAnchor } => away.anchor !== undefined
  );
  if (kept.length === 0) return null;
  return (
    <div className="space-y-3 pt-3" hidden={hidden}>
      {kept.map((away) => (
        <div
          className="space-y-2"
          key={`${away.anchor.mark_id ?? ""}:${away.anchor.occurrence ?? ""}:${away.anchor.quote}`}
        >
          <div className={`space-y-1 text-sm ${textMutedOnSurfaceMuted}`} role="status">
            <p>
              {`You wrote this draft${KEPT_KIND_WORDS[away.kind ?? "comment"]} for the words “${away.anchor.quote}”. It is kept here, and offered in the next compose you open on this document:`}
            </p>
            <blockquote
              className={`border-l-2 pl-2 whitespace-pre-wrap ${quoteAccentBorder} ${quoteBodyText}`}
            >
              {away.draft.body}
            </blockquote>
            {away.draft.replacement === "" ? null : (
              <>
                <p>Replace with:</p>
                <blockquote
                  className={`border-l-2 pl-2 whitespace-pre-wrap ${quoteAccentBorder} ${quoteBodyText}`}
                >
                  {away.draft.replacement}
                </blockquote>
              </>
            )}
          </div>
          <button
            className={`text-sm font-medium underline ${dangerText}`}
            onClick={() => onDiscard(away)}
            type="button"
          >
            Discard my draft
          </button>
        </div>
      ))}
    </div>
  );
}

export function MarginAskCard({
  artifactSlug,
  ask,
  owner,
  section,
  selected,
}: {
  artifactSlug: string;
  ask: Ask;
  owner: MarginOwner;
  /** Which group the card is listed under: an open ask waiting on the reader, or a decided one. */
  section: "needs-you" | "decided";
  selected: boolean;
}): ReactNode {
  return (
    <div
      aria-current={selected ? "true" : undefined}
      className={selected ? `rounded-xl ${highlightRing}` : undefined}
      data-margin-item={ask.id}
      data-margin-section={section}
    >
      <AskCard
        ask={ask}
        artifactSlug={artifactSlug}
        documentLink={false}
        markRead
        owner={owner.kind === "document" ? owner : undefined}
        variant="compact"
      />
      <Unfurl body={ask.question} source={textSourceOf("ask", ask.id)} />
    </div>
  );
}

export function CommentsTab({
  actionFailure,
  answeredAsksPending,
  artifactSlug,
  asksPending,
  commentsError,
  commentsPending,
  decisions,
  expandedThreadKey,
  editingCommentId,
  savingCommentEditId,
  onEditingChange,
  historicalAsks,
  hoveredItemId,
  hoveredMarkId,
  isClosed,
  owner,
  blockPlacements,
  markPlacements,
  needsYou,
  onAction,
  onEdit,
  onRetryAction,
  onRetryAnsweredAsk,
  onRetryComments,
  onSelectCard,
  onToggleResolved,
  onToggleThread,
  pendingActionIds,
  resolvedThreads,
  foldedAskCount,
  selectedItemId,
  showResolved,
  threads,
  viewerLogin,
}: CommentsTabProps): ReactNode {
  // Every ask card, open and decided, is one keyed list with the `Needs you` heading as an item
  // in it (the Inbox's pattern): an ask the reader answers moves from the open group to the
  // decided one within the same parent, so React moves its card rather than remounting it, and
  // the thread's reply the reader has started typing stays where they typed it.
  const decided = commentsError
    ? []
    : historicalAsks.filter((ask) => !needsYou.some((open) => open.id === ask.id));
  const askRows: ReactNode[] = [];
  if (needsYou.length > 0) {
    askRows.push(
      <h2 className={`text-sm font-semibold ${textPrimaryOnSurface}`} key="needs-you">
        Needs you
      </h2>
    );
  }
  for (const [section, asks] of [
    ["needs-you", needsYou],
    ["decided", decided],
  ] as const) {
    for (const ask of asks) {
      askRows.push(
        <MarginAskCard
          artifactSlug={artifactSlug}
          ask={ask}
          key={ask.id}
          owner={owner}
          section={section}
          selected={selectedItemId === ask.id || hoveredItemId === ask.id}
        />
      );
    }
    if (section === "needs-you" && asks.length > 0) {
      askRows.push(<hr className={borderDefault} key="needs-you-end" />);
    }
  }
  return (
    <div className="space-y-3 pt-3">
      <section aria-label="Margin review items" className="space-y-3">
        {askRows}
        {onRetryAnsweredAsk === undefined ? null : (
          <QueryError
            message="Could not load this document's asks."
            onRetry={onRetryAnsweredAsk}
            retrying={answeredAsksPending}
          />
        )}
        {commentsError ? (
          <QueryError
            message="Could not load this document's comments."
            onRetry={onRetryComments}
          />
        ) : (
          <>
            <ThreadList
              actionFailure={actionFailure}
              artifactSlug={artifactSlug}
              expandedThreadKey={expandedThreadKey}
              editingCommentId={editingCommentId}
              savingCommentEditId={savingCommentEditId}
              onEditingChange={onEditingChange}
              hoveredItemId={hoveredItemId}
              hoveredMarkId={hoveredMarkId}
              isClosed={isClosed}
              blockPlacements={blockPlacements}
              markPlacements={markPlacements}
              onAction={onAction}
              onEdit={onEdit}
              onRetryAction={onRetryAction}
              onSelect={onSelectCard}
              onToggle={onToggleThread}
              onToggleResolved={onToggleResolved}
              pendingActionIds={pendingActionIds}
              resolvedThreads={resolvedThreads}
              selectedItemId={selectedItemId}
              foldedAskCount={foldedAskCount}
              showResolved={showResolved}
              owner={owner}
              threads={threads}
              viewerLogin={viewerLogin}
            />
            <DecisionList
              blockPlacements={blockPlacements}
              decisions={decisions}
              hoveredItemId={hoveredItemId}
              onSelect={onSelectCard}
              selectedItemId={selectedItemId}
            />
            {threads.length === 0 &&
            resolvedThreads.length === 0 &&
            historicalAsks.length === 0 &&
            needsYou.length === 0 &&
            decisions.length === 0 &&
            !asksPending &&
            !commentsPending &&
            !answeredAsksPending ? (
              <EmptyState
                label="Margin review empty state"
                message="No comments, asks, or suggestions on this document."
              />
            ) : null}
          </>
        )}
      </section>
    </div>
  );
}
