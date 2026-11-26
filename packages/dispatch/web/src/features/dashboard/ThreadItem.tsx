import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useState } from "react";
import { Link } from "react-router-dom";

import { askThreadQuery, myRepliesKey } from "../../api/queries";
import type {
  Actor,
  CommentWithTrees,
  DisplayTree,
  MessageWithTrees,
  MyReplyRow,
} from "../../api/types";
import { pageInkText, pageMutedText, pageQuietText } from "../../theme/classes";
import { newestAttempt } from "../agents/exchange-delivery";
import type { ReplyTarget } from "../conversation/composer-model";
import { deliveryMode } from "../conversation/delivery";
import { KeptReplyToDeleted } from "../conversation/KeptDraftOffers";
import { MentionComposer } from "../conversation/MentionComposer";
import { AskReplyComposer, takesReply } from "../inbox/AskThread";
import { DASHBOARD_WORDS } from "../issue-page/page-words";
import { RowText } from "../issue-page/RowText";
import { useAgentText } from "../refs/AgentName";
import { agentsQuery, EMPTY_AGENTS } from "../refs/agent-registry";
import { postDeletion } from "../refs/post-marks";
import { commentQuery, EVENT_FRESH, messageQuery } from "../refs/reference-target";
import { buildIssuePath } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { type TextSource, textSourceOf } from "../refs/text-references";
import { unreadWrites } from "../threads/thread-layout";
import { DashboardAskBody } from "./DashboardAskBody";

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

/** The writes of `entries` the reader has not read: each the thread's row reads as unread
 *  (`unreadWrites`, the one rule every surface reads it by) that the reader did not write. */
function unreadEntries<Write extends Entry>(
  row: MyReplyRow,
  entries: readonly Write[],
  viewer: string | undefined
): readonly Write[] {
  const unread = unreadWrites(row, entries);
  return entries.filter(
    (entry) => unread.has(entry.id) && !(entry.author.kind === "user" && entry.author.id === viewer)
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
export function replySendKey(threadId: string): readonly unknown[] {
  return ["dashboard-thread-reply", threadId];
}

const noReturn = (): void => {};

/** A comment's or a message's thread, from its own read (`CommentThread`, `MessageThread`): the
 *  root and each reply the reader has not read, the Inbox's reply control for the thread - or,
 *  where the root's author deleted it, the reply a draft kept for it - and the link to it. */
function ReplyThread({
  closed,
  issueKey,
  parentKind,
  readKey,
  replies,
  root,
  thread,
}: {
  closed: boolean;
  issueKey: string;
  /** What the root is, which names each entry's source, the reply's parent and the address. */
  parentKind: "comment" | "message";
  /** The key of the thread's own read, which a sent reply reads again. */
  readKey: readonly unknown[];
  /** The replies the reader has not read. */
  replies: readonly (CommentWithTrees | MessageWithTrees)[];
  root: CommentWithTrees | MessageWithTrees;
  /** How a reply into a targeted message's thread is delivered. */
  thread?: ReplyTarget["thread"];
}): ReactNode {
  const queryClient = useQueryClient();
  const { agents, reach } = useReachedAgents();
  const entry = (post: CommentWithTrees | MessageWithTrees) => (
    <EntryView
      entry={{ ...post, tree: post.body_tree }}
      key={post.id}
      source={textSourceOf(parentKind, post.id, post.issue_key)}
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
          agentNames="cache"
          mutationKey={replySendKey(root.id)}
          parentKind={parentKind}
        />
      ) : (
        <fieldset
          className="m-0 min-w-0 border-0 p-0"
          onFocus={reach}
          onPointerEnter={reach}
          onTouchStart={reach}
        >
          <MentionComposer
            agentNames="cache"
            agents={agents}
            box="inbox-thread-reply"
            closed={closed}
            inline
            mutationKey={replySendKey(root.id)}
            onClose={noReturn}
            onSent={() => {
              void queryClient.invalidateQueries({ queryKey: readKey });
              void queryClient.invalidateQueries({ queryKey: myRepliesKey });
            }}
            owner={{ issueKey, kind: "issue" }}
            replyParent={root}
            replyTo={{
              actor: root.author,
              excerpt: "",
              id: root.id,
              parentKind,
              ...(thread === undefined ? {} : { thread }),
            }}
            standing
          />
        </fieldset>
      )}
      <ThreadLink to={buildIssuePath({ id: root.id, key: issueKey, kind: parentKind })} />
    </div>
  );
}

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
  const read = useQuery({ ...commentQuery(row.thread_id), ...EVENT_FRESH }).data;
  if (read === undefined) return null;
  const root = read.comment;
  return (
    <ReplyThread
      closed={closed}
      issueKey={issueKey}
      parentKind="comment"
      readKey={commentQuery(root.id).queryKey}
      replies={unreadEntries(row, read.replies ?? [], viewer)}
      root={root}
    />
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
  const read = useQuery({ ...messageQuery(issueKey, row.thread_id), ...EVENT_FRESH }).data;
  if (read === undefined) return null;
  const root = read.message;
  const all = read.replies ?? [];
  // A reply into a targeted thread is delivered as the thread's newest attempt was, which is how
  // the Inbox's thread addresses one.
  const last = newestAttempt([root, ...all]);
  const target = root.target;
  return (
    <ReplyThread
      closed={closed}
      issueKey={issueKey}
      parentKind="message"
      readKey={messageQuery(issueKey, root.id).queryKey}
      replies={unreadEntries(row, all, viewer)}
      root={root}
      thread={
        target === null || target === undefined
          ? undefined
          : { delivery: deliveryMode(last?.delivery ?? "") ?? "steer", target }
      }
    />
  );
}

/** An ask's thread, from its own read: the ask by its body (`DashboardAskBody`), each reply the
 *  reader has not read, the reply control while the ask takes one, and the link to it. Named
 *  apart from the Inbox's `AskThread`, which the composer census tells mounts apart by. */
function AskThreadItem({
  issueKey,
  row,
  viewer,
}: {
  issueKey: string;
  row: MyReplyRow;
  viewer: string | undefined;
}): ReactNode {
  // The thread is this item's own read, under the key the ask's form reads it by
  // (`DashboardAskBody`), so the form reads no thread of its own and the ask it draws is this one.
  const read = useQuery(askThreadQuery(row.thread_id)).data;
  if (read === undefined) return null;
  const ask = read.ask;
  const replies = unreadEntries(row, read.replies ?? [], viewer);
  return (
    <div className="space-y-2">
      <DashboardAskBody ask={ask} />
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
      {takesReply(ask) ? <AskReplyComposer ask={ask} uploadOwner={{ issue: issueKey }} /> : null}
      <ThreadLink to={buildIssuePath({ id: ask.id, key: issueKey, kind: "ask" })} />
    </div>
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
      return <AskThreadItem issueKey={issueKey} row={row} viewer={viewer} />;
    case "comment":
      return <CommentThread closed={closed} issueKey={issueKey} row={row} viewer={viewer} />;
    case "message":
      return <MessageThread closed={closed} issueKey={issueKey} row={row} viewer={viewer} />;
    default:
      return null;
  }
}
