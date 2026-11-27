import { hashKey, useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useCallback, useContext, useId, useMemo, useState } from "react";

import type { WriteBounds } from "../../../api/client";
import { whoAmIQuery } from "../../../api/queries";
import type { Anchor, Comment, IssueDetails } from "../../../api/types";
import { commentWritten } from "../../../api/write-events";
import { pageRowActs } from "../../../theme/classes";
import { useHeldSend } from "../../conversation/held-sends";
import {
  type CarriedBlockIds,
  type SaveCommentEdit,
  saveCommentEdit,
} from "../../conversation/send-request";
import { useUnsentDrafts } from "../../conversation/unsent-drafts";
import { marginReplySendKey } from "../../margin/margin-context";
import { ThreadCard } from "../../margin/ThreadCard";
import {
  type CommentActionFailure,
  useCommentActionQueue,
} from "../../margin/useCommentActionQueue";
import type { MarginItemAction, MarginOwner, ThreadComment } from "../../margin/useMarginItems";
import { useAgentText } from "../../refs/AgentName";
import { EVENT_FRESH } from "../../refs/reference-target";
import type { TreeNode } from "../issue-tree";
import { LensChain } from "../lenses/LensChain";
import { LensLines } from "../lenses/LensLines";
import { LensActContext, LensMenu, lensActFor } from "../lenses/LensMenu";
import { NODE_ROW_LAYOUT, RowMargin, rowNameIds, type Tone } from "../NodeRow";
import { authorText, pageChrome } from "../page-structure";
import { authorKind, isReader, LENS_WORDS } from "../page-words";
import { RowText } from "../RowText";
import { quoteLine } from "./quote-line";
import type { TextThread } from "./text-threads";
import { threadCommentNode } from "./thread-node";

/**
 * One thread on text as the column draws it beside its node (R4), and each node's sheet below
 * 1024 px: `main`'s `ThreadCard`, fixed open (`fixedOpen`, with `expanded` and a toggle that does
 * nothing), with every control it draws on `main` - the reply composer, Resolve or Reopen, Edit
 * of the reader's own comment, Accept and Reject of a suggestion, the copy reference - but
 * Resolve and Reopen on a thread rooted in a message or in a reply with a quote (`rootActions`),
 * and a message thread's reply writes a message (`Thread.kind`). Its writes are the margin's own
 * (`useCommentActionQueue`, `saveCommentEdit`) on the comments read the page already holds
 * (`["comments", key]`), and they read again what the margin's writes read again: no read of
 * their own. Under each comment of the thread stand its lenses (R7), each drawn as part W draws a
 * lens's row - its verdict, its kind's words, author, time and state, its lines and its chain -
 * with its findings and risks whole under it.
 */

/** What every card of one column or sheet shares: the issue's writes on its comments, the edit in
 *  progress and the reader. */
export interface ThreadCardHost {
  readonly actionFailure: CommentActionFailure | undefined;
  /** The slug of the document `anchor` is in, for the orphan notice's link; undefined for an
   *  anchor on no document of the issue. */
  readonly artifactSlugOf: (anchor: Anchor | null) => string | undefined;
  readonly editingCommentId: string | undefined;
  readonly isClosed: boolean;
  readonly onAction: (id: string, action: MarginItemAction) => void;
  readonly onEdit: SaveCommentEdit;
  readonly onEditingChange: (id: string | undefined) => void;
  readonly onRetryAction: () => void;
  readonly owner: MarginOwner;
  readonly pendingActionIds: ReadonlySet<string>;
  readonly savingCommentEditId: string | undefined;
  readonly viewerLogin: string;
}

/**
 * The writes and the state the cards of `issue` share, as the margin keeps them for its own cards
 * (`useMarginItems`, `useMarginSheet`): Resolve, Reopen, Accept and Reject queued one at a time
 * with their optimistic answer written into the comments read, and an edit's save, each reading
 * the issue's comments again once Dispatch has taken it.
 */
export function useThreadCardHost(issue: IssueDetails): ThreadCardHost {
  const client = useQueryClient();
  const key = issue.key;
  const viewerLogin = useQuery({ ...whoAmIQuery(), ...EVENT_FRESH }).data?.login ?? "";
  const [editingCommentId, setEditingCommentId] = useState<string>();
  const [savingCommentEditId, setSavingCommentEditId] = useState<string>();
  const { actionFailure, mutateItem, pendingActionIds, retryItem } = useCommentActionQueue<{
    previous: Comment[] | undefined;
  }>({
    holds: ({ id, kind }) => ({ endsOn: commentWritten(id, kind), keys: [["comments", key]] }),
    issue: key,
    onError: (_error, _input, context) => {
      client.setQueryData<Comment[]>(["comments", key], context?.previous);
      void client.invalidateQueries({ queryKey: ["comments", key] });
    },
    onMutate: async ({ id, kind }) => {
      await client.cancelQueries({ queryKey: ["comments", key] });
      const previous = client.getQueryData<Comment[]>(["comments", key]);
      client.setQueryData<Comment[]>(["comments", key], (current) =>
        current?.map((comment) => {
          if (comment.id !== id) return comment;
          if (kind === "reopen") {
            return { ...comment, resolved: false, resolved_at: null, resolved_by: null };
          }
          if (kind === "resolve") return { ...comment, resolved: true };
          return comment.suggestion === null
            ? comment
            : {
                ...comment,
                resolved: true,
                suggestion: { ...comment.suggestion, accepted: kind === "accept" },
              };
        })
      );
      return { previous };
    },
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ["comments", key] });
      void client.invalidateQueries({ queryKey: ["issue", key] });
    },
  });
  const onEdit = useCallback(
    async (id: string, body: string, carried: CarriedBlockIds | undefined, bounds: WriteBounds) => {
      setSavingCommentEditId(id);
      try {
        const saved = await saveCommentEdit(id, body, carried, bounds);
        void client.invalidateQueries({ queryKey: ["comments", key] });
        return saved;
      } finally {
        setSavingCommentEditId(undefined);
      }
    },
    [client, key]
  );
  const artifacts = issue.artifacts;
  const artifactSlugOf = useCallback(
    (anchor: Anchor | null) =>
      anchor === null
        ? undefined
        : artifacts?.find((artifact) => artifact.id === anchor.artifact_id)?.slug,
    [artifacts]
  );
  return useMemo(
    () => ({
      actionFailure,
      artifactSlugOf,
      editingCommentId,
      isClosed: issue.closed_at !== null,
      onAction: (id: string, action: MarginItemAction) => mutateItem({ id, kind: action }),
      onEdit,
      onEditingChange: setEditingCommentId,
      onRetryAction: retryItem,
      owner: { key, kind: "issue" },
      pendingActionIds,
      savingCommentEditId,
      viewerLogin,
    }),
    [
      actionFailure,
      artifactSlugOf,
      editingCommentId,
      issue.closed_at,
      key,
      mutateItem,
      onEdit,
      pendingActionIds,
      retryItem,
      savingCommentEditId,
      viewerLogin,
    ]
  );
}

/** The card's toggle, which a fixed-open card draws no control for: it stays open (R4). */
function keepOpen(): void {}

/**
 * The card of `thread`, with the item `currentReplyId` names marked as the card marks the reply a
 * link names (`aria-current`). Its stored text is marked as authors' text by the box the column
 * and the sheet draw it in, inside `NoTooltips`, as `IssueMarginCards` draws the margin's cards.
 * It draws first the thread's quoted words cut to one line (`quoteLine`, R4), while a
 * suggestion's diff strikes through the whole quote, the words Accept replaces.
 */
export function TextThreadCard({
  currentReplyId,
  host,
  thread,
}: {
  readonly currentReplyId?: string;
  readonly host: ThreadCardHost;
  readonly thread: TextThread;
}): ReactNode {
  const card = thread.thread;
  const root = card.root.comment.id;
  const replyKey = marginReplySendKey(card.key);
  // The card's reply composer stands open from the load (R4), so it is handed no agents, and reads
  // no list of them for its mention picker, until the reader uses it: the focus in a composer of
  // the card, or a reply of theirs that the held-send store or the kept drafts hold under the
  // thread's reply send name. So a page of cards sends no read that the page without them does
  // not (R9).
  const [used, setUsed] = useState(false);
  const replyHeld = useHeldSend(replyKey) !== undefined;
  const replyKept = useUnsentDrafts().useKept(hashKey(replyKey));
  return (
    <div
      className="contents"
      onFocusCapture={(event) => {
        if (event.target instanceof Element && event.target.closest("form") !== null) {
          setUsed(true);
        }
      }}
    >
      <ThreadCard
        actionFailure={host.actionFailure?.id === root ? host.actionFailure : undefined}
        artifactSlug={host.artifactSlugOf(card.anchor)}
        {...(currentReplyId === undefined ? {} : { currentReplyId })}
        editingCommentId={host.editingCommentId}
        expanded
        fixedOpen
        hovered={false}
        isClosed={host.isClosed}
        onAction={host.onAction}
        onEdit={host.onEdit}
        onEditingChange={host.onEditingChange}
        onRetryAction={host.onRetryAction}
        onToggle={keepOpen}
        owner={host.owner}
        pendingAction={host.pendingActionIds.has(root)}
        rootActions={!thread.replyRoot && thread.kind !== "message"}
        pulseOrphanBlock={false}
        quoteLine={quoteLine(thread.quote)}
        // Under each comment of the thread, its lenses (R7): the card's one place under each
        // comment's own body, which the margin fills with deliveries where an owner has them.
        renderDeliveries={(comment: ThreadComment) => (
          <>
            <ThreadActs comment={comment} kind={thread.kind} />
            <ThreadLenses lenses={thread.lenses.get(comment.id)} />
          </>
        )}
        replyAgents={used || replyHeld || replyKept}
        replyMutationKey={replyKey}
        savingCommentEditId={host.savingCommentEditId}
        thread={card}
        viewerLogin={host.viewerLogin}
      />
    </div>
  );
}

/**
 * The acts of one comment of a thread that its row would carry (R7): part W's Lens, drawn under
 * the comment, above its lenses, in Conversation and in Graph, where `LensActContext` holds the
 * act (the column provides it from `useLensAct()`, `TextMargin.tsx`), and in no other view. It is
 * invoked on the comment's own address, which a `TreeNode` of the comment carries
 * (`threadCommentNode`), and takes a place in the tab order, as the card's other controls do.
 * "Write a criterion" is part Y's act, which a later gate mounts here.
 */
function ThreadActs({
  comment,
  kind,
}: {
  readonly comment: ThreadComment;
  readonly kind: TextThread["kind"];
}): ReactNode {
  const act = useContext(LensActContext);
  if (act === undefined) return null;
  const node = threadCommentNode(comment, kind, act.issueKey);
  if (!lensActFor(node, act)) return null;
  return (
    <p className={`mt-1 ${pageRowActs}`} data-thread-acts="">
      <LensMenu node={node} tabStops />
    </p>
  );
}

/** The colour of a node a lens's row holds, as `NodeRow.tsx` gives each kind its own. */
function toneOf(node: TreeNode): Tone {
  if (node.type === "constraint") return "statement";
  return node.type === "lens" || node.type === "finding" || node.type === "risk" ? "lens" : "issue";
}

/** The lenses on one comment of a thread, each with its findings and risks, or nothing. */
function ThreadLenses({ lenses }: { readonly lenses: readonly TreeNode[] | undefined }): ReactNode {
  if (lenses === undefined || lenses.length === 0) return null;
  return (
    <ul className="mt-2 flex min-w-0 flex-col gap-1" data-thread-lenses="">
      {lenses.map((lens) => (
        <LensRow key={lens.id} node={lens} />
      ))}
    </ul>
  );
}

/**
 * One node of a lens's row as part W draws it in the outline (`NodeRow.tsx`): its words - a
 * lens's verdict, or the page's words where it holds none - its lines (`LensLines`), its chain
 * (`LensChain`), and beside them its kind's words, author, time and state (`RowMargin`); then each
 * node under it - a lens's findings and risks - whole, a level in. Its controls keep their place
 * in the tab order: no tree's keys reach the column. The margin stands before the words in the
 * DOM, as the outline's does, so that on a phone it floats at the right of the row's top
 * (`NODE_ROW_LAYOUT`), and the row's name reads it after them (`rowNameIds`).
 */
function LensRow({ node }: { readonly node: TreeNode }): ReactNode {
  const agentText = useAgentText();
  const reader = useQuery({ ...whoAmIQuery(), ...EVENT_FRESH }).data?.login;
  const ids = rowNameIds(useId());
  return (
    <li
      aria-labelledby={ids.labelledBy}
      className="min-w-0"
      data-thread-lens={node.id}
      data-thread-lens-read={node.readId}
      data-thread-lens-type={node.type}
    >
      <div className={NODE_ROW_LAYOUT.box}>
        <span aria-hidden="true" className={NODE_ROW_LAYOUT.bullet} />
        <RowMargin
          author={{
            kind: authorKind(node.author),
            name: agentText.author(node.author),
            reader: isReader(node.author, reader),
          }}
          context={false}
          id={ids.margin}
          node={node}
          tabStops
          tone={toneOf(node)}
        />
        <div className={NODE_ROW_LAYOUT.content} id={ids.content}>
          {node.type === "lens" && node.text === "" ? (
            <p className="break-words" {...pageChrome}>
              {LENS_WORDS.no_verdict}
            </p>
          ) : (
            <div className="min-w-0" {...authorText}>
              <RowText markdown={node.text} source={node.source} tabStops tree={node.tree} />
            </div>
          )}
          <LensLines node={node} tabStops />
          <LensChain node={node} tabStops />
        </div>
      </div>
      {node.children.length === 0 ? null : (
        <ul className="ml-[18px] flex min-w-0 flex-col gap-1">
          {node.children.map((child) => (
            <LensRow key={child.id} node={child} />
          ))}
        </ul>
      )}
    </li>
  );
}
