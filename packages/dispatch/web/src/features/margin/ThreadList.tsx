import { type ReactNode, useCallback, useMemo } from "react";

import type { SaveCommentEdit } from "../conversation/send-request";
import { useCardStack } from "./card-stack";
import { marginReplySendKey } from "./margin-context";
import { ThreadCard } from "./ThreadCard";
import type { CommentActionFailure } from "./useCommentActionQueue";
import {
  type MarginItemAction,
  type MarginOwner,
  type MarkPlacement,
  type Thread,
  threadMarkId,
} from "./useMarginItems";

interface ThreadListProps {
  actionFailure: CommentActionFailure | undefined;
  artifactSlug: string;
  expandedThreadKey: string | undefined;
  editingCommentId: string | undefined;
  savingCommentEditId: string | undefined;
  onEditingChange(id: string | undefined): void;
  hoveredItemId: string | undefined;
  hoveredMarkId: string | undefined;
  isClosed: boolean;
  owner: MarginOwner;
  blockPlacements: ReadonlyMap<string, MarkPlacement>;
  markPlacements: ReadonlyMap<string, MarkPlacement>;
  onAction(id: string, action: MarginItemAction): void;
  onEdit: SaveCommentEdit;
  onRetryAction(): void;
  onSelect(id: string, blockID: string | undefined): void;
  onToggle(key: string): void;
  onToggleResolved(): void;
  pendingActionIds: ReadonlySet<string>;
  resolvedThreads: Thread[];
  /** Hidden retracted and withdrawn asks; counted in the toggle so a reader can reveal them too. */
  foldedAskCount: number;
  selectedItemId?: string;
  showResolved: boolean;
  threads: Thread[];
  viewerLogin: string;
}

function isAnchoredThread(thread: Thread): boolean {
  return (
    thread.anchor !== null &&
    (!thread.anchor.orphaned || typeof thread.anchor.block_id === "string")
  );
}

function placementFor(
  thread: Thread,
  markPlacements: ReadonlyMap<string, MarkPlacement>,
  blockPlacements: ReadonlyMap<string, MarkPlacement>
): MarkPlacement | undefined {
  if (thread.anchor === null) {
    return undefined;
  }
  if (thread.anchor.orphaned) {
    return typeof thread.anchor.block_id === "string"
      ? blockPlacements.get(thread.anchor.block_id)
      : undefined;
  }
  return markPlacements.get(thread.anchor.mark_id);
}

function anchorElementFor(thread: Thread): HTMLElement | null {
  if (thread.anchor === null) {
    return null;
  }
  if (thread.anchor.orphaned) {
    return typeof thread.anchor.block_id === "string"
      ? document.querySelector<HTMLElement>(
          `[data-block-id="${CSS.escape(thread.anchor.block_id)}"]`
        )
      : null;
  }
  return document.querySelector<HTMLElement>(`[data-id="${CSS.escape(thread.anchor.mark_id)}"]`);
}

/** A thread's key in the anchored region's stack (`useCardStack`). */
const threadKey = (thread: Thread): string => thread.key;

export function ThreadList({
  actionFailure,
  artifactSlug,
  expandedThreadKey,
  editingCommentId,
  savingCommentEditId,
  onEditingChange,
  hoveredItemId,
  hoveredMarkId,
  isClosed,
  blockPlacements,
  markPlacements,
  onAction,
  onEdit,
  onRetryAction,
  onSelect,
  onToggle,
  onToggleResolved,
  owner,
  pendingActionIds,
  resolvedThreads,
  foldedAskCount,
  selectedItemId,
  showResolved,
  threads,
  viewerLogin,
}: ThreadListProps): ReactNode {
  // Which section a thread lives in is a property of its data - it has a live (non-orphaned)
  // anchor or it does not - never of whether its mark has been measured yet. Placements arrive
  // after the document renders; deciding membership by them moved a card from Discussion into
  // Anchored once its placement landed, and React remounts a card that changes parents, dropping
  // its open editor's draft. An anchored thread without a placement yet stacks below the previous
  // card (`useCardStack`) until its mark can be located.
  const { anchored, discussion } = useMemo(
    () => ({
      anchored: threads.filter((thread) => isAnchoredThread(thread)),
      discussion: threads.filter((thread) => !isAnchoredThread(thread)),
    }),
    [threads]
  );
  // A thread wants its mark's height in the document, or its block's for one whose mark is
  // gone; one whose placement has not been measured yet stacks below the card before it.
  const wantedTop = useCallback(
    (thread: Thread, origin: number) => {
      const placement = placementFor(thread, markPlacements, blockPlacements);
      if (placement === undefined) {
        return undefined;
      }
      const mark = anchorElementFor(thread);
      if (mark === null) {
        return placement.top;
      }
      return mark.getBoundingClientRect().top - origin;
    },
    [blockPlacements, markPlacements]
  );
  const { cardRef, height, region, topOf } = useCardStack(anchored, threadKey, wantedTop);

  const card = (thread: Thread, composerClassName?: string) => (
    <ThreadCard
      actionFailure={actionFailure?.id === thread.key ? actionFailure : undefined}
      artifactSlug={artifactSlug}
      composerClassName={composerClassName}
      expanded={expandedThreadKey === thread.key}
      editingCommentId={editingCommentId}
      savingCommentEditId={savingCommentEditId}
      onEditingChange={onEditingChange}
      hovered={hoveredItemId === thread.key || hoveredMarkId === threadMarkId(thread)}
      isClosed={isClosed}
      key={thread.key}
      onAction={onAction}
      onEdit={onEdit}
      onRetryAction={onRetryAction}
      onSelect={() => onSelect(thread.key, thread.anchor?.block_id ?? undefined)}
      onToggle={() => onToggle(thread.key)}
      owner={owner}
      selected={selectedItemId === thread.key}
      pendingAction={pendingActionIds.has(thread.key)}
      replyMutationKey={marginReplySendKey(thread.key)}
      thread={thread}
      viewerLogin={viewerLogin}
    />
  );

  return (
    <div className="space-y-3">
      {anchored.length === 0 ? null : (
        <section
          aria-label="Anchored comments"
          className="relative"
          ref={region}
          style={{
            minHeight: height,
          }}
        >
          {anchored.map((thread) => (
            <div
              key={thread.key}
              ref={cardRef(thread.key)}
              style={{
                left: 0,
                position: "absolute",
                right: 0,
                top: topOf(thread.key),
              }}
            >
              {card(thread)}
            </div>
          ))}
        </section>
      )}
      {discussion.length === 0 ? null : (
        <section aria-label="Discussion" className="space-y-3">
          {discussion.map((thread) => card(thread))}
        </section>
      )}
      {resolvedThreads.length + foldedAskCount === 0 ? null : (
        <>
          <button
            aria-expanded={showResolved}
            className="min-h-11 text-sm font-medium"
            onClick={onToggleResolved}
            type="button"
          >
            Resolved ({resolvedThreads.length + foldedAskCount})
          </button>
          {showResolved ? (
            <section aria-label="Resolved" className="space-y-3">
              {resolvedThreads.map((thread) => card(thread))}
            </section>
          ) : null}
        </>
      )}
    </div>
  );
}
