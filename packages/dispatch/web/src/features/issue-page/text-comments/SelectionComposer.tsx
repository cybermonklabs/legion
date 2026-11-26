import { hashKey, type MutationKey, useQueryClient } from "@tanstack/react-query";
import { type KeyboardEvent, type ReactNode, useEffect, useRef, useState } from "react";

import { ApiError } from "../../../api/client";
import { primarySpec } from "../../../api/issue-cache";
import type { IssueDetails } from "../../../api/types";
import type { ComposerAnchor, ComposerKind } from "../../conversation/composer-model";
import { composerSend, useHeldSend } from "../../conversation/held-sends";
import { MentionComposer } from "../../conversation/MentionComposer";
import type { SentRequest } from "../../conversation/send-request";
import { ofKind, writesOf } from "../../writes/write-store";
import { MARGIN_ANSWERS } from "../page-words";
import type { TreeAnswerNode } from "../tree-read/tree-read";
import { quotedBlock } from "./on-text";
import { quoteLine } from "./quote-line";
import {
  type ComposerAddress,
  composerAddressOf,
  type QuotedWords,
  subjectOf,
  writeFor,
} from "./text-comment-write";
import type { ViewThreads } from "./useTextThreads";

/**
 * The composer a comment on selected words is written in (R5), as the column draws it beside the
 * node (`TextMargin.tsx`) and the node's sheet below 1024 px (`NodeThreads.tsx`): `MentionComposer`
 * under the composer's send name, which keeps the draft under that name at each change, so the
 * composer drawn at the same words in the other place opens on it when the window crosses
 * 1024 px. It is handed the anchor or the reply target of the node's write
 * (`text-comment-write.ts`) and sends them through its own path (`send-request.ts`, held by
 * `held-sends.ts`), so its draft is kept, retried and refused as every composer's is. An anchored
 * composer is headed by the quote `MentionComposer` draws over its field; a reply's, which it
 * draws none for, by the quoted words cut to one line (`quoteLine`).
 * Escape closes it with the draft kept and gives the focus to the node's row.
 *
 * Once the send lands, the composer closes and the page lands on the new thread's root, or says
 * a reply's quote was not kept (`landingOf`); while Dispatch's last refusal says the words are
 * gone, one sentence says so beside the composer's own words.
 *
 * A block's editor opens it too, on the words its selection bar or its chord asked for
 * (`EditorCompose`, through `composeForMark`'s place, `TextMargin.tsx`): with the kind the bar
 * chose, anchored to what the bar wrote - its mark alone, or in a closed issue's document the
 * quote - and its send out keeps the mark in the document.
 */

/** A compose a block's editor asked for (`MarkComposePlace`): the anchor its selection bar wrote,
 *  its mark or, in a closed issue's document, its quote, and the kind the bar chose, with the
 *  editor's promise, which the composer resolves as its send goes out and the column rejects as
 *  the composer closes unsent (`holdComposer`). */
export interface EditorCompose {
  readonly anchor: ComposerAnchor;
  readonly kind: ComposerKind;
  readonly reject: (reason: Error) => void;
  readonly resolve: () => void;
}

/** The words a person selected and the node they are in, by its tree id: what the control hands
 *  the column to open its composer on (`TextComposerRequest`), and the compose a block's editor
 *  asked for on them (`mark`). */
export type SelectedWords = QuotedWords & {
  readonly mark?: EditorCompose;
  readonly nodeId: string;
};

/** What the page does once a send lands: the new thread's root takes the focus once the re-read
 *  draws it, or, for a reply whose quote the server did not keep, one sentence says so. */
export type TextComposerOutcome = { readonly root: string } | { readonly notice: string };

/** One key for each selection a composer is opened on, which names its draft and its send: each
 *  mark of an editor's is a selection of its own. */
export function wordsKeyOf(words: SelectedWords): string {
  const selected = [words.nodeId, words.block, words.occurrence, words.quote];
  return JSON.stringify(
    words.mark === undefined ? selected : [...selected, words.mark.anchor.mark_id ?? null]
  );
}

/** The refusals that say the server found no such words: 400 `INVALID_ANCHOR` on an anchor
 *  (INFRA-1253), 404 `TARGET_NOT_FOUND` on a reply (INFRA-5703). */
function wordsGone(error: unknown): boolean {
  return (
    error instanceof ApiError &&
    ((error.status === 400 && error.code === "INVALID_ANCHOR") ||
      (error.status === 404 && error.code === "TARGET_NOT_FOUND"))
  );
}

/** A post as Dispatch answers its write, as far as the composer reads it: its id, and for a reply
 *  the block it answers (`reply_block`). */
interface WrittenPost {
  readonly id: string;
  readonly reply_block?: Parameters<typeof quotedBlock>[0] | null;
}

function writtenPost(answer: unknown): answer is WrittenPost {
  return (
    typeof answer === "object" &&
    answer !== null &&
    "id" in answer &&
    typeof answer.id === "string" &&
    (!("reply_block" in answer) || typeof answer.reply_block === "object")
  );
}

/** Whether a reply's answered block kept the quote the send carried (`reply_block.quote`, G4). */
function quoteKept(block: WrittenPost["reply_block"]): boolean {
  const quote = block === null || block === undefined ? undefined : quotedBlock(block).quote;
  return typeof quote === "string" && quote !== "";
}

/** What a send that landed leaves the page to do, from Dispatch's answer `answer` to `sent`: the
 *  new thread's root takes the focus, or, for a reply whose answered block holds no quote the send
 *  carried - an older build answers so while two builds serve one database - one sentence says
 *  the words were not kept, and the reply stands as a row under what it answers (decision 465
 *  (4)). Undefined for an answer that names no post. */
function landingOf(answer: unknown, sent: SentRequest): TextComposerOutcome | undefined {
  if (!writtenPost(answer)) return undefined;
  return sent.replyTo?.quote !== undefined && !quoteKept(answer.reply_block)
    ? { notice: MARGIN_ANSWERS.quote_not_kept }
    : { root: answer.id };
}

/**
 * Calls `landed` with Dispatch's answer and the request it answered each time a send under
 * `mutationKey` lands while the caller is mounted. The composer's own outcome of a send
 * (`useComposerSend`) carries no answer, and the page lands on the post the answer names.
 */
function useLandedSends(
  mutationKey: MutationKey,
  landed: (answer: unknown, sent: SentRequest) => void
): void {
  const client = useQueryClient();
  const latest = useRef(landed);
  latest.current = landed;
  const name = hashKey(mutationKey);
  useEffect(
    () =>
      writesOf(client).onOutcome((outcome) => {
        if (outcome.kind !== "landed") return;
        const write = ofKind(outcome.write, composerSend);
        if (write !== undefined && hashKey(write.detail.mutationKey) === name) {
          latest.current(outcome.answer, write.detail.request);
        }
      }),
    [client, name]
  );
}

/** The `tree` artifact of the issue `issue` in the view's answer: the answer's own for the issue it
 *  is of, else that issue's node's. */
function treeOf(view: ViewThreads, issue: string): string | null | undefined {
  const answer = view.answer;
  if (answer === undefined) return undefined;
  if (answer.issue === issue) return answer.tree;
  return answer.nodes?.find((node) => node.supertag === "issue" && node.id === issue)?.tree;
}

/** Where a send on selected words goes: the address `MentionComposer` sends with, on the route of
 *  the node's own issue. */
export interface SendTarget {
  readonly address: ComposerAddress;
  readonly issue: string;
}

/**
 * Where a send on `words` of `node`, a node of `view` on the page of `issue`, goes: the node's write
 * (`writeFor`) and what it names (`composerAddressOf`), or null where no write stands on the node
 * or the read gives it nothing to name. The control draws itself only where this answers one.
 */
export function sendTargetOf(
  view: ViewThreads,
  node: TreeAnswerNode,
  issue: IssueDetails,
  words: QuotedWords
): SendTarget | null {
  const write = writeFor(subjectOf(node, issue.key));
  if (write === null) return null;
  const spec = write.issue === issue.key ? primarySpec(issue)?.id : undefined;
  const address = composerAddressOf(
    write,
    { ...node, ...(spec === undefined ? {} : { spec }), tree: treeOf(view, write.issue) },
    words
  );
  return address === null ? null : { address, issue: write.issue };
}

/**
 * Where the composer opened on `words` of the issue `issue`'s page sends (`sendTargetOf`), taken
 * from the view while it draws the node, and from where it last drew it once it is folded or paged
 * away, where the composer stays. Null in a mount that never met the node drawn. A compose of a
 * block's editor sends its own anchor on the page's issue, whose document the editor holds,
 * whether the node is drawn or not.
 */
function useSendTarget({
  issue,
  view,
  words,
}: {
  readonly issue: IssueDetails;
  readonly view: ViewThreads;
  readonly words: SelectedWords;
}): SendTarget | null {
  const drawn = view.nodes.get(words.nodeId);
  const target =
    words.mark !== undefined
      ? { address: { anchor: words.mark.anchor, replyTo: null }, issue: issue.key }
      : drawn === undefined
        ? undefined
        : sendTargetOf(view, drawn, issue, words);
  // The target as it was while each read of the node builds it afresh with the same values, so
  // the composer's anchor and reply keep their identity.
  const [last, setLast] = useState<SendTarget | null>(null);
  const same = target !== undefined && JSON.stringify(target) === JSON.stringify(last);
  if (target !== undefined && !same) setLast(target);
  return target === undefined || same ? last : target;
}

/**
 * The composer opened on `words`: `MentionComposer` on the route of the node's issue, under a send
 * name of these words, so each selection keeps its own draft, and the sentence Dispatch's last
 * refusal of its send leaves where the words are gone. `onLeave` closes it with the draft kept and
 * gives the focus to the node's row: what Escape does, and the composer's own Close and Discard.
 * `onLand` closes it once its send lands, with where the page lands. A mount that never met its
 * node drawn (`useSendTarget`) has nowhere to send, and closes at once with its draft kept.
 */
export function SelectionComposer({
  issue,
  onLand,
  onLeave,
  view,
  words,
}: {
  readonly issue: IssueDetails;
  readonly onLand: (outcome: TextComposerOutcome) => void;
  readonly onLeave: () => void;
  readonly view: ViewThreads;
  readonly words: SelectedWords;
}): ReactNode {
  const target = useSendTarget({ issue, view, words });
  const sendName: MutationKey = ["text-comment", issue.key, wordsKeyOf(words)];
  const held = useHeldSend(sendName);
  const said =
    held?.status === "refused" && wordsGone(held.refusal) ? MARGIN_ANSWERS.words_gone : undefined;
  // A compose of a block's editor keeps its mark from the moment its send is out, as the margin's
  // compose does: the held send names that mark, through a refusal and its Retry.
  const mark = words.mark;
  const sendOut = held !== undefined;
  useEffect(() => {
    if (sendOut) mark?.resolve();
  }, [mark, sendOut]);
  // `MentionComposer` ends a send that lands with its `onSent`, then its `onClose`; the landing
  // closes the composer with where the page lands, so that `onClose` is no Escape of the reader's.
  const landing = useRef(false);
  useLandedSends(sendName, (answer, sent) => {
    landing.current = true;
    const outcome = landingOf(answer, sent);
    if (outcome === undefined) onLeave();
    else onLand(outcome);
  });
  const leave = useRef(onLeave);
  leave.current = onLeave;
  const nowhere = target === null;
  useEffect(() => {
    if (nowhere) leave.current();
  }, [nowhere]);
  // Escape in the composer closes it and keeps its draft (R5's table), where `MentionComposer`
  // would ask to discard a typed one, or end its reply: before its own listener, unless a list it
  // opened over its field - the mention list, the reference picker - takes the key. Its prompt,
  // `Discard draft?`, closes the composer on Escape with the draft kept as this does, and a
  // refusal's words take no key, so one Escape closes it in every state.
  const leaveOnEscape = (event: KeyboardEvent<HTMLDivElement>) => {
    if (event.key !== "Escape" || event.defaultPrevented || event.nativeEvent.isComposing) return;
    if (event.currentTarget.querySelector('[role="listbox"], [role="dialog"]') !== null) return;
    event.preventDefault();
    event.stopPropagation();
    onLeave();
  };
  return (
    <div className="flex min-w-0 flex-col gap-2" onKeyDownCapture={leaveOnEscape}>
      {target === null || target.address.anchor !== undefined ? null : (
        // The selected words, as the server's plain text reads them: the reading list's
        // `anchor-quote`.
        <blockquote className="border-l-2 pl-2 text-sm">{quoteLine(words.quote)}</blockquote>
      )}
      {target === null ? null : (
        <MentionComposer
          anchor={target.address.anchor}
          autoFocus
          // The writing list's box of a margin's composer opened from a selection, which writes a
          // comment on the words it quotes, as this one does, or a reply to a message's block.
          box="margin-composer"
          kind={words.mark?.kind ?? "comment"}
          mutationKey={sendName}
          onClose={() => {
            if (!landing.current) onLeave();
          }}
          onSent={() => {
            landing.current = true;
          }}
          owner={{ issueKey: target.issue, kind: "issue" }}
          replyTo={target.address.replyTo}
          // The draft is kept under the send name at each change, as a standing box keeps its own,
          // and not by its address only as the composer unmounts: at 1024 px the column and the
          // node's sheet swap their composers at the same words in one commit, and the one that
          // arrives reads the draft as it first draws, before the one that leaves has unmounted.
          // A reply target is the composer's own address, which nothing ends: it offers no Cancel
          // reply and takes no focus for it.
          standing
        />
      )}
      {said === undefined ? null : (
        <p className="text-sm" role="status">
          {said}
        </p>
      )}
    </div>
  );
}
