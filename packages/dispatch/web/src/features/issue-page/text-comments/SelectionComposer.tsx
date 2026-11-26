import { useQueryClient } from "@tanstack/react-query";
import { type KeyboardEvent, type ReactNode, useState } from "react";

import { ApiError, api, type WriteBounds } from "../../../api/client";
import type { Comment, IssueDetails, Message } from "../../../api/types";
import { MentionComposer } from "../../conversation/MentionComposer";
import { MARGIN_ANSWERS } from "../page-words";
import type { TreeAnswerNode } from "../tree-read/tree-read";
import { issueCommentsQuery, issueMessagesQuery } from "../useIssueTree";
import { quotedBlock } from "./on-text";
import { quoteLine } from "./quote-line";
import {
  type LiveWrite,
  MOUNTED_GATES,
  type QuotedWords,
  subjectOf,
  textRequestOf,
  writeFor,
} from "./text-comment-write";
import type { ViewThreads } from "./useTextThreads";

/**
 * The composer a comment on selected words is written in (R5), as the column draws it beside the
 * node (`TextMargin.tsx`) and the node's sheet below 1024 px (`NodeThreads.tsx`): the quoted words
 * cut to one line (`quoteLine`), then `MentionComposer`, which keeps the draft under the
 * composer's send name at each change, so the composer drawn at the same words in the other place
 * opens on it when the window crosses 1024 px.
 * Escape closes it with the draft kept and gives the focus to the node's row.
 *
 * Its send (`useSelectionSend`) writes the request of the node's kind (`text-comment-write.ts`).
 * `MentionComposer` sends through its own path, which carries none of R5's fields until pull
 * request 3125 is on the base, so the composer's Send takes this send at G5, and the page mounts
 * the control that opens this composer only where `GATES.COMPOSER` is on.
 */

/** The words a person selected and the node they are in, by its tree id: what the control hands
 *  the column to open its composer on (`TextComposerRequest`). */
export type SelectedWords = QuotedWords & { readonly nodeId: string };

/** What the page does once a send lands: the new thread's root takes the focus once the re-read
 *  draws it, or, for a reply whose quote the server did not keep, one sentence says so. */
export type TextComposerOutcome = { readonly root: string } | { readonly notice: string };

/** One key for each selection a composer is opened on, which names its draft and its send. */
export function wordsKeyOf(words: SelectedWords): string {
  return JSON.stringify([words.nodeId, words.block, words.occurrence, words.quote]);
}

/** The body `api.createComment` takes. A node anchor (G2) and a reply's quote and occurrence (G4)
 *  are fields the generated types gain with those gates. */
type CommentInput = Parameters<typeof api.createComment>[1];

/** The refusals that say the server found no such words: 400 `INVALID_ANCHOR` on an anchor
 *  (INFRA-1253), 404 `TARGET_NOT_FOUND` on a reply (INFRA-5703). */
function wordsGone(error: unknown): boolean {
  return (
    error instanceof ApiError &&
    ((error.status === 400 && error.code === "INVALID_ANCHOR") ||
      (error.status === 404 && error.code === "TARGET_NOT_FOUND"))
  );
}

/** Whether a reply's answered block kept the quote the send carried (`reply_block.quote`, G4). */
function quoteKept(block: Parameters<typeof quotedBlock>[0] | null): boolean {
  const quote = block === null ? undefined : quotedBlock(block).quote;
  return typeof quote === "string" && quote !== "";
}

/** The `tree` artifact of the issue `issue` in the view's answer: the answer's own for the issue it
 *  is of, else that issue's node's. */
function treeOf(view: ViewThreads, issue: string): string | null | undefined {
  const answer = view.answer;
  if (answer === undefined) return undefined;
  if (answer.issue === issue) return answer.tree;
  return answer.nodes?.find((node) => node.supertag === "issue" && node.id === issue)?.tree;
}

/** The send of a composer of this part and what it said last. */
interface SelectionSend {
  /** The sentence the last refusal left in the composer, with the role `status`: the words are no
   *  longer in the text. */
  readonly said: string | undefined;
  /** Writes `body` on the selected words with the composer's write bounds, as an edit's save
   *  takes them (`SaveCommentEdit`), and re-reads the comments or the messages, and the issue's
   *  tree reads, as a margin write does. On 201 the composer closes and the page lands on the new
   *  thread's root, or says a reply's quote was not kept; a refusal rejects, so the composer keeps
   *  the draft and draws its own words, after the sentence where the words are gone. */
  readonly send: (body: string, bounds: WriteBounds) => Promise<Comment | Message>;
}

/**
 * The send of the composer opened on `words` of the issue `issue`'s page: the write of the node
 * the words are in (`writeFor`), taken from the view while it draws the node, and from where it
 * last drew it once it is folded or paged away, where the composer stays.
 */
function useSelectionSend({
  issue,
  onLand,
  view,
  words,
}: {
  readonly issue: IssueDetails;
  readonly onLand: (outcome: TextComposerOutcome) => void;
  readonly view: ViewThreads;
  readonly words: SelectedWords;
}): SelectionSend & { readonly write: LiveWrite | null } {
  const client = useQueryClient();
  const [said, setSaid] = useState<string>();
  // The node as the view last drew it: the composer stays where a folded or paged-away row stood,
  // and its send still writes on that node.
  const drawn = view.nodes.get(words.nodeId);
  const tree = drawn === undefined ? undefined : treeOf(view, drawn.issue ?? issue.key);
  const [last, setLast] = useState<{ readonly node: TreeAnswerNode; readonly tree: typeof tree }>();
  if (drawn !== undefined && (last?.node !== drawn || last.tree !== tree)) {
    setLast({ node: drawn, tree });
  }
  const node = drawn === undefined ? last : { node: drawn, tree };
  const write =
    node === undefined ? null : writeFor(subjectOf(node.node, issue.key), MOUNTED_GATES);
  const send = async (body: string, bounds: WriteBounds): Promise<Comment | Message> => {
    if (write === null || node === undefined) {
      throw new Error("These words are in no node a comment is written on.");
    }
    setSaid(undefined);
    const request = textRequestOf(write, { ...node.node, tree: node.tree }, words, body);
    try {
      const answer =
        request.route === "messages"
          ? await api.createMessage(request.issue, request.body, bounds)
          : await api.createComment(request.issue, request.body as CommentInput, bounds);
      void client.invalidateQueries({
        queryKey:
          request.route === "messages"
            ? issueMessagesQuery(request.issue).queryKey
            : issueCommentsQuery(request.issue).queryKey,
      });
      void client.invalidateQueries({ queryKey: ["issue", issue.key] });
      const reply = write.place === "item" && write.request !== "node";
      onLand(
        reply && !quoteKept(answer.reply_block)
          ? { notice: MARGIN_ANSWERS.quote_not_kept }
          : { root: answer.id }
      );
      return answer;
    } catch (error) {
      if (wordsGone(error)) setSaid(MARGIN_ANSWERS.words_gone);
      throw error;
    }
  };
  return { said, send, write };
}

/** What the composer's own send leaves its host to do beyond closing (`onClose`): nothing. */
function nothingMore(): void {}

/**
 * The composer opened on `words`: the quoted words cut to one line, `MentionComposer` on the
 * route of the node's issue, under a send name of these words, so each selection keeps its own
 * draft, and the sentence its last refusal left. `onLeave` closes it with the draft kept and
 * gives the focus to the node's row: what Escape does, and the composer's own Close and Discard.
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
  const { said, write } = useSelectionSend({ issue, onLand, view, words });
  // Escape in the composer closes it and keeps its draft (R5's table), where `MentionComposer`
  // would ask to discard a typed one: before its own listener, unless a list it opened over its
  // field - the mention list, the reference picker - or its own prompt takes the key.
  const leaveOnEscape = (event: KeyboardEvent<HTMLDivElement>) => {
    if (event.key !== "Escape" || event.defaultPrevented || event.nativeEvent.isComposing) return;
    const open = '[role="listbox"], [role="dialog"], [role="alert"]';
    if (event.currentTarget.querySelector(open) !== null) return;
    event.preventDefault();
    event.stopPropagation();
    onLeave();
  };
  return (
    <div className="flex min-w-0 flex-col gap-2" onKeyDownCapture={leaveOnEscape}>
      <blockquote className="border-l-2 pl-2 text-sm">{quoteLine(words.quote)}</blockquote>
      <MentionComposer
        autoFocus
        // The writing list's box of a margin's composer opened from a selection, which writes a
        // comment on the words it quotes, as this one does.
        box="margin-composer"
        kind="comment"
        mutationKey={["text-comment", issue.key, wordsKeyOf(words)]}
        onClose={onLeave}
        onSent={nothingMore}
        owner={{ issueKey: write?.issue ?? issue.key, kind: "issue" }}
        // The draft is kept under the send name at each change, as a standing box keeps its own,
        // and not by its address only as the composer unmounts: at 1024 px the column and the
        // node's sheet swap their composers at the same words in one commit, and the one that
        // arrives reads the draft as it first draws, before the one that leaves has unmounted.
        // Every other effect of `standing` is on a reply target, which this composer never has.
        standing
      />
      {said === undefined ? null : (
        <p className="text-sm" role="status">
          {said}
        </p>
      )}
    </div>
  );
}
