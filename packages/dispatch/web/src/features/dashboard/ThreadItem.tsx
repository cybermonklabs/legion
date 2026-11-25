import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useState } from "react";
import { Link } from "react-router-dom";

import { askThreadQuery, myRepliesKey } from "../../api/queries";
import type {
  Actor,
  AskWithTrees,
  CommentWithTrees,
  DisplayTree,
  MessageWithTrees,
  MyReplyRow,
} from "../../api/types";
import { compareTimestamps } from "../../lib/timestamps";
import { pageInkText, pageMutedText, pageQuietText } from "../../theme/classes";
import { newestAttempt } from "../agents/exchange-delivery";
import { AskBody } from "../ask-body/AskBody";
import { deliveryMode } from "../conversation/delivery";
import { KeptReplyToDeleted } from "../conversation/KeptDraftOffers";
import { MentionComposer } from "../conversation/MentionComposer";
import { AskReplyComposer, takesReply } from "../inbox/AskThread";
import { useAskAnswerForm } from "../inbox/useAskAnswerForm";
import { DASHBOARD_WORDS } from "../issue-page/page-words";
import { RowText } from "../issue-page/RowText";
import { useAgentText } from "../refs/AgentName";
import { agentsQuery, EMPTY_AGENTS } from "../refs/agent-registry";
import { postDeletion } from "../refs/post-marks";
import { commentQuery, EVENT_FRESH, messageQuery } from "../refs/reference-target";
import { buildIssuePath } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { type TextSource, textSourceOf } from "../refs/text-references";
import { answerAsk, createReply, declineAsk, getAskThread } from "./ask-writes";
import { rowText } from "./row-text";

/**
 * A thread of what waits on the reader (dispatch://INFRA-5702/spec#b-r-in-place): a row of the
 * reader's unread `me/replies`, read through its thread's own route - the ask, the comment or the
 * message - and never through the issue's whole comments or messages list. It draws the thread's
 * first entry and each write the reader has not read, each whole, the reply control the Inbox
 * draws for that kind of thread, and a link to the whole thread at its own address. Drawing it
 * marks nothing read: a reply the reader sends moves their read mark as their own write does,
 * and the item leaves.
 */

/** The read of a thread's own route: the one this item draws from, which Dashboard waits on
 *  before it asks for the names of what the thread cites. */
export function threadQuery(issueKey: string, row: Pick<MyReplyRow, "kind" | "thread_id">) {
  switch (row.kind) {
    case "ask":
      return askThreadQuery(row.thread_id);
    case "message":
      return { ...messageQuery(issueKey, row.thread_id), ...EVENT_FRESH };
    default:
      return { ...commentQuery(row.thread_id), ...EVENT_FRESH };
  }
}

/** One write of a thread as the item draws it. */
interface Entry {
  readonly author: Actor;
  readonly body: string;
  readonly created_at: string;
  readonly id: string;
  readonly tree?: DisplayTree;
}

/** The writes of `entries` the reader has not read: each newer than the row's read mark that the
 *  reader did not write. */
export function unreadEntries<Write extends Entry>(
  row: Pick<MyReplyRow, "read_through">,
  entries: readonly Write[],
  viewer: string | undefined
): readonly Write[] {
  const mark = row.read_through;
  return entries.filter(
    (entry) =>
      !(entry.author.kind === "user" && entry.author.id === viewer) &&
      (mark === undefined || compareTimestamps(entry.created_at, mark) > 0)
  );
}

function EntryView({ entry, source }: { entry: Entry; source: TextSource | undefined }): ReactNode {
  const text = useAgentText();
  return (
    <li className="space-y-1" data-thread-entry={entry.id}>
      <p className={`text-xs ${pageQuietText}`}>
        <span className={`font-medium ${pageMutedText}`}>{text.author(entry.author)}</span>
        {" · "}
        <Timestamp at={entry.created_at} />
      </p>
      <div className={`text-sm ${pageInkText}`}>
        <RowText markdown={entry.body} source={source} tabStops tree={entry.tree} />
      </div>
    </li>
  );
}

function ThreadLink({ to }: { to: string }): ReactNode {
  return (
    <Link className={`text-xs font-medium underline ${pageMutedText}`} to={to}>
      {DASHBOARD_WORDS.thread_link}
    </Link>
  );
}

/** The agents a reply's mentions offer, from the client's cache of `GET /api/v1/agents`: the read
 *  is enabled only once the reader reaches for the reply control - by focus, pointer or touch -
 *  so drawing it sends none (R8). */
function useReachedAgents(): { agents: typeof EMPTY_AGENTS; reach: () => void } {
  const [reached, setReached] = useState(false);
  const agents = useQuery({ ...agentsQuery(), enabled: reached }).data ?? EMPTY_AGENTS;
  return { agents, reach: () => setReached(true) };
}

/** Where a reply's send is held, by the thread it answers. */
function replySendKey(threadId: string): readonly unknown[] {
  return ["dashboard-thread-reply", threadId];
}

const noReturn = (): void => {};

function CommentThread({
  closed,
  issueKey,
  row,
  viewer,
}: {
  closed: boolean;
  issueKey: string;
  row: MyReplyRow;
  viewer: string | undefined;
}): ReactNode {
  const queryClient = useQueryClient();
  const { agents, reach } = useReachedAgents();
  const read = useQuery({ ...commentQuery(row.thread_id), ...EVENT_FRESH }).data;
  if (read === undefined) return null;
  const root: CommentWithTrees = read.comment;
  const replies = unreadEntries(row, read.replies ?? [], viewer);
  const entry = (comment: CommentWithTrees) => (
    <EntryView
      entry={{ ...comment, tree: comment.body_tree }}
      key={comment.id}
      source={textSourceOf("comment", comment.id, comment.issue_key)}
    />
  );
  return (
    <div className="space-y-2">
      <ol className="space-y-2">
        {entry(root)}
        {replies.map(entry)}
      </ol>
      {postDeletion(root) !== null ? (
        <KeptReplyToDeleted
          actor={root.author}
          mutationKey={replySendKey(root.id)}
          parentKind="comment"
        />
      ) : (
        <fieldset
          className="m-0 min-w-0 border-0 p-0"
          onFocus={reach}
          onPointerEnter={reach}
          onTouchStart={reach}
        >
          <MentionComposer
            agents={agents}
            box="inbox-thread-reply"
            closed={closed}
            inline
            kind="comment"
            mutationKey={replySendKey(root.id)}
            onClose={noReturn}
            onSent={() => {
              void queryClient.invalidateQueries({ queryKey: ["comment", root.id] });
              void queryClient.invalidateQueries({ queryKey: myRepliesKey });
            }}
            owner={{ issueKey, kind: "issue" }}
            replyParent={root}
            replyTo={{ actor: root.author, excerpt: "", id: root.id, parentKind: "comment" }}
            standing
          />
        </fieldset>
      )}
      <ThreadLink to={buildIssuePath({ id: root.id, key: issueKey, kind: "comment" })} />
    </div>
  );
}

function MessageThread({
  closed,
  issueKey,
  row,
  viewer,
}: {
  closed: boolean;
  issueKey: string;
  row: MyReplyRow;
  viewer: string | undefined;
}): ReactNode {
  const queryClient = useQueryClient();
  const { agents, reach } = useReachedAgents();
  const read = useQuery({ ...messageQuery(issueKey, row.thread_id), ...EVENT_FRESH }).data;
  if (read === undefined) return null;
  const root: MessageWithTrees = read.message;
  const all = read.replies ?? [];
  const replies = unreadEntries(row, all, viewer);
  // A reply into a targeted thread is delivered as the thread's newest attempt was, which is how
  // the Inbox's thread addresses one.
  const last = newestAttempt([root, ...all]);
  const target = root.target;
  const entry = (message: MessageWithTrees) => (
    <EntryView
      entry={{ ...message, tree: message.body_tree }}
      key={message.id}
      source={textSourceOf("message", message.id, message.issue_key)}
    />
  );
  return (
    <div className="space-y-2">
      <ol className="space-y-2">
        {entry(root)}
        {replies.map(entry)}
      </ol>
      {postDeletion(root) !== null ? (
        <KeptReplyToDeleted
          actor={root.author}
          mutationKey={replySendKey(root.id)}
          parentKind="message"
        />
      ) : (
        <fieldset
          className="m-0 min-w-0 border-0 p-0"
          onFocus={reach}
          onPointerEnter={reach}
          onTouchStart={reach}
        >
          <MentionComposer
            agents={agents}
            box="inbox-thread-reply"
            closed={closed}
            inline
            mutationKey={replySendKey(root.id)}
            onClose={noReturn}
            onSent={() => {
              void queryClient.invalidateQueries({
                queryKey: ["issue-message", issueKey, root.id],
              });
              void queryClient.invalidateQueries({ queryKey: myRepliesKey });
            }}
            owner={{ issueKey, kind: "issue" }}
            replyParent={root}
            replyTo={{
              actor: root.author,
              excerpt: "",
              id: root.id,
              parentKind: "message",
              ...(target === null || target === undefined
                ? {}
                : { thread: { delivery: deliveryMode(last?.delivery ?? "") ?? "steer", target } }),
            }}
            standing
          />
        </fieldset>
      )}
      <ThreadLink to={buildIssuePath({ id: root.id, key: issueKey, kind: "message" })} />
    </div>
  );
}

function AskThreadBody({
  ask,
  issueKey,
  replies,
}: {
  ask: AskWithTrees;
  issueKey: string;
  replies: readonly CommentWithTrees[];
}): ReactNode {
  // The thread is this item's own read (`threadQuery`), under the key the form reads it by.
  const form = useAskAnswerForm({
    answer: answerAsk,
    ask,
    createReply,
    getAskThread,
    readThread: false,
  });
  return (
    <div className="space-y-2">
      <AskBody
        agentNames="cache"
        ask={ask}
        decline={declineAsk}
        documentLink
        form={form}
        frame="card"
        renderText={rowText}
        urgencyWord
      />
      {replies.length === 0 ? null : (
        <ol className="space-y-2">
          {replies.map((reply) => (
            <EntryView
              entry={{ ...reply, tree: reply.body_tree }}
              key={reply.id}
              source={textSourceOf("comment", reply.id, reply.issue_key)}
            />
          ))}
        </ol>
      )}
      {takesReply(form.displayedAsk) ? (
        <AskReplyComposer ask={form.displayedAsk} uploadOwner={{ issue: issueKey }} />
      ) : null}
      <ThreadLink to={buildIssuePath({ id: ask.id, key: issueKey, kind: "ask" })} />
    </div>
  );
}

function AskThread({
  issueKey,
  row,
  viewer,
}: {
  issueKey: string;
  row: MyReplyRow;
  viewer: string | undefined;
}): ReactNode {
  const read = useQuery(askThreadQuery(row.thread_id)).data;
  if (read === undefined) return null;
  return (
    <AskThreadBody
      ask={read.ask}
      issueKey={issueKey}
      replies={unreadEntries(row, read.replies ?? [], viewer)}
    />
  );
}

/** One thread item, by its kind. A row of any other kind draws nothing here. */
export function ThreadItem({
  closed,
  issueKey,
  row,
  viewer,
}: {
  /** The issue is closed, which takes replies all the same; the composer says so itself. */
  closed: boolean;
  issueKey: string;
  row: MyReplyRow;
  viewer: string | undefined;
}): ReactNode {
  switch (row.kind) {
    case "ask":
      return <AskThread issueKey={issueKey} row={row} viewer={viewer} />;
    case "comment":
      return <CommentThread closed={closed} issueKey={issueKey} row={row} viewer={viewer} />;
    case "message":
      return <MessageThread closed={closed} issueKey={issueKey} row={row} viewer={viewer} />;
    default:
      return null;
  }
}
