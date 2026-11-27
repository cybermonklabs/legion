import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useLayoutEffect, useRef, useState } from "react";

import { whoAmIQuery } from "../../../api/queries";
import type { CommentWithTrees, IssueDetails, MessageWithTrees } from "../../../api/types";
import { compareTimestamps } from "../../../lib/timestamps";
import { pageInkText, pageQuietText } from "../../../theme/classes";
import { newestAttempt } from "../../agents/exchange-delivery";
import { deliveryMode } from "../../conversation/delivery";
import { DashboardAskBody } from "../../dashboard/DashboardAskBody";
import { isViewer } from "../../refs/actor";
import { postDeletion } from "../../refs/post-marks";
import { EVENT_FRESH } from "../../refs/reference-target";
import { takesTypedText } from "../../shell/keymap";
import { useMarkThreadRead } from "../../threads/useMarkThreadRead";
import { authorText, pageChrome } from "../page-structure";
import { CONTROL_LABELS } from "../page-words";
import { CommentBox, CommentEditBox } from "./CommentBox";
import { type MessageAddress, MessageBox } from "./MessageBox";
import { depthStyle, ENTRY_CONTROL, ENTRY_INDENT, ReadMarkFoot, RecordEntry } from "./RecordEntry";
import { useRowNames } from "./ThreadList";
import { KindWords } from "./ThreadRow";
import type { ThreadRecord } from "./thread-records";
import { nameOf, threadRow } from "./thread-row";

/** A comment or a message of a record. */
type Post = CommentWithTrees | MessageWithTrees;

/** One entry of a record where the record draws it: its post, and its depth under the root. */
interface Placed {
  readonly post: Post;
  readonly depth: number;
}

/**
 * The entries of `record` in the order an opened record draws them (R9): each reply under the
 * entry it answers, after that entry's earlier replies, at one depth more, to every depth the
 * record holds. A reply that answers an entry the record does not hold - a record a reply with a
 * quote begins, whose root answers its post - stands under the root. An ask's root is the ask
 * itself, which the record draws apart, so its replies start at depth 1 under it.
 */
function placedEntries(record: ThreadRecord): readonly Placed[] {
  const rootId = record.root.id;
  const ids = new Set([rootId, ...record.replies.map((post) => post.id)]);
  const children = new Map<string, Post[]>();
  for (const post of record.replies) {
    // A message answers its `in_reply_to`, and a comment its `answers`, which the server keeps
    // beside the thread's head in `reply_to`.
    const parent = "in_reply_to" in post ? post.in_reply_to : post.answers;
    const under = parent !== null && parent !== undefined && ids.has(parent) ? parent : rootId;
    children.set(under, [...(children.get(under) ?? []), post]);
  }
  const placed: Placed[] = [];
  const walk = (id: string, depth: number): void => {
    for (const post of children.get(id) ?? []) {
      placed.push({ depth, post });
      walk(post.id, depth + 1);
    }
  };
  if (record.kind !== "ask") placed.push({ depth: 0, post: record.root });
  walk(rootId, 1);
  return placed;
}

/** Where a message thread's replies go (R9, `conversation/send-request.ts`): the target of the
 *  thread's root, in the mode of the thread's newest delivery attempt, as Dashboard's thread item
 *  and the Inbox address one. A thread whose root names no target sends neither. */
function messageThread(
  record: ThreadRecord & { readonly kind: "message" },
  thread: readonly ThreadRecord[]
): MessageAddress["thread"] {
  const messages = thread.flatMap((one) =>
    one.kind === "message" ? [one.root, ...one.replies] : []
  );
  const root = messages.find((message) => message.id === record.thread) ?? record.root;
  const target = root.target;
  if (target === null) return undefined;
  const last = newestAttempt(messages.length === 0 ? [record.root, ...record.replies] : messages);
  return { delivery: deliveryMode(last?.delivery ?? "") ?? "steer", target };
}

/** The box a reader opened in the record: a reply under an entry, or the reader's own comment
 *  open to edit. One stands open at a time (R9). */
type OpenBox = { readonly kind: "reply" | "edit"; readonly id: string };

/**
 * The opened record (dispatch://INFRA-5704/spec#b-r-open, R9), in the main column: its kind as
 * its row names it, the words a record on text quotes above its root, then each entry whole in
 * R9's order (`placedEntries`), siblings at their depths, then the box at its foot.
 *
 * An ask is drawn by the ask body (`DashboardAskBody`, `rowText` as its renderer and its writes
 * from `ask-body/ask-writes.ts`), then its replies from its thread in the `asks?thread=1` page.
 * While the ask is open the ask body's answer box is the record's one box at rest, and no box
 * stands at its foot or under the ask itself.
 *
 * The box at the foot writes a reply to the record's root. A reply control opens a box under its
 * entry, which writes a reply to that entry and goes when it sends or the reader leaves it, and
 * Edit opens the reader's own comment in an editor in its words' place. One such box is open at a
 * time, so opening one closes the other, and the draft of the one that goes is kept under its send
 * name as the box keeps it (`CommentBox`, `MessageBox`).
 *
 * The read mark: once the record's newest entry has stood on screen for a second, the page marks
 * the thread read through that entry's time (`useMarkThreadRead`, with the issue as owner and the
 * server's thread id). Nothing else in Threads writes a mark, and a row drawn writes none. A row
 * the reader pressed far down the list leaves the page scrolled past the top of the main column,
 * where the record stands, so the record is brought into view as it opens: its entries stand on
 * screen, and the newest of them can dwell (R9).
 *
 * Escape with the caret in a box takes the caret out of it, and the record and the box's words
 * stay (R12): the page's Escape that closes the record waits for the next press, with no caret in
 * a box (`ThreadsView`). A box that goes on Escape, one under an entry, goes as it does.
 *
 * `thread` is every record the view holds of the record's thread on the server, which a record a
 * reply with a quote begins reads its thread's target from. `focus` is the entry the address the
 * record opened at focuses, a message `/messages/<id>` named (R12), which the record focuses as it
 * opens.
 */
export function OpenedRecord({
  focus,
  issue,
  record,
  thread,
}: {
  readonly focus?: string;
  readonly issue: IssueDetails;
  readonly record: ThreadRecord;
  readonly thread: readonly ThreadRecord[];
}): ReactNode {
  const names = useRowNames(issue.project);
  const viewer = useQuery({ ...whoAmIQuery(), ...EVENT_FRESH }).data?.login;
  const [open, setOpen] = useState<OpenBox>();
  const article = useRef<HTMLElement>(null);
  const focused = useRef<HTMLLIElement>(null);
  // Once, as the record opens: the view keys a record by its id, so another record mounts anew.
  useLayoutEffect(() => {
    article.current?.scrollIntoView({ block: "nearest" });
    focused.current?.focus();
  }, []);
  const closed = issue.closed_at !== null;
  // The record's newest write, which the read mark reads through (R9): an entry, or an ask's
  // answer or resolution, which the ask's own entry draws.
  const newest = record.writes.reduce(
    (latest, write) => (compareTimestamps(write.at, latest.at) > 0 ? write : latest),
    record.writes[0] ?? { at: record.lastActivity, author: record.root.author, id: record.root.id }
  );
  const { ref: markRef } = useMarkThreadRead({
    enabled: true,
    freshness: EVENT_FRESH,
    newestShownAt: newest.at,
    owner: { issue: issue.key },
    threadId: record.thread,
  });
  const leave = () => setOpen(undefined);
  const askOpen = record.kind === "ask" && record.root.state === "open";
  const messageTarget = record.kind === "message" ? messageThread(record, thread) : undefined;

  /** The box a reply control opens under `post`, at one depth more than it. */
  const replyBox = (post: Post, depth: number): ReactNode => (
    <li
      className={`min-w-0 ${ENTRY_INDENT}`}
      data-thread-box={post.id}
      key={`box-${post.id}`}
      style={depthStyle(depth + 1)}
      {...(record.kind === "message" ? {} : authorText)}
    >
      {record.kind === "message" ? (
        <MessageBox
          address={{ inReplyTo: post.id, thread: messageTarget }}
          autoFocus
          issueKey={issue.key}
          label={CONTROL_LABELS.reply}
          onLeave={leave}
          onSent={leave}
          sendKey={["threads-message", record.id, post.id]}
        />
      ) : (
        <CommentBox
          closed={closed}
          issueKey={issue.key}
          onLeave={leave}
          parent={post}
          parentKind="comment"
          sendKey={["threads-reply", record.id, post.id]}
        />
      )}
    </li>
  );

  const entries = placedEntries(record).flatMap(({ depth, post }) => {
    const own =
      record.kind !== "message" &&
      !closed &&
      postDeletion(post) === null &&
      isViewer(post.author, viewer);
    const editing = open?.kind === "edit" && open.id === post.id;
    const drawn = (
      <RecordEntry
        author={nameOf(post.author, names)}
        depth={depth}
        {...(editing
          ? {
              edit: (
                <div {...authorText}>
                  <CommentEditBox
                    body={post.body}
                    id={post.id}
                    inAsk={record.kind === "ask" || ("ask_id" in post && post.ask_id !== null)}
                    issueKey={issue.key}
                    onLeave={leave}
                  />
                </div>
              ),
            }
          : {})}
        {...(focus === post.id ? { focusRef: focused } : {})}
        key={post.id}
        kind={record.kind === "message" ? "message" : "comment"}
        {...(newest.id === post.id ? { markRef } : {})}
        {...(own ? { onEdit: () => setOpen({ id: post.id, kind: "edit" }) } : {})}
        onReply={() => setOpen({ id: post.id, kind: "reply" })}
        post={post}
      />
    );
    return open?.kind === "reply" && open.id === post.id ? [drawn, replyBox(post, depth)] : [drawn];
  });

  const foot: ReactNode =
    record.kind === "message" ? (
      <MessageBox
        address={{ inReplyTo: record.root.id, thread: messageTarget }}
        issueKey={issue.key}
        label={CONTROL_LABELS.reply}
        sendKey={["threads-message", record.id, "foot"]}
      />
    ) : askOpen ? null : (
      <div {...authorText}>
        <CommentBox
          closed={closed}
          issueKey={issue.key}
          parent={record.root}
          parentKind={record.kind}
          sendKey={["threads-reply", record.id, "foot"]}
        />
      </div>
    );

  return (
    <article
      className="min-w-0 space-y-3"
      data-thread-record={record.id}
      onKeyDown={(event) => {
        // A `MentionComposer` box takes Escape on its own form and marks it handled, so the
        // page's keys never hear it: the record takes the caret out here, and marks the press
        // handled so the page's Escape does not close the record on it as well. A field holds
        // the caret where it takes typed text as the page's keys read it (`takesTypedText`), so
        // Escape on a checkbox or a button is the page's.
        if (event.key !== "Escape" || event.nativeEvent.isComposing) return;
        if (!(event.target instanceof HTMLElement) || !takesTypedText(event.target)) return;
        event.preventDefault();
        event.target.blur();
      }}
      ref={article}
    >
      <h2 className={`text-[15px] font-semibold ${pageInkText}`} {...pageChrome}>
        <KindWords text={threadRow(record, names)} />
      </h2>
      {record.onText && record.quote !== undefined ? (
        <blockquote
          className={`border-l-2 pl-2 text-sm ${pageQuietText}`}
          data-thread-quote=""
          {...authorText}
        >
          {/* The words quoted are the document's or the post's own, drawn as they stand. */}
          {/* anchor-quote */}
          {record.quote}
        </blockquote>
      ) : null}
      <ol className="min-w-0 space-y-3">
        {record.kind === "ask" ? (
          <li className="min-w-0 space-y-1" data-depth={0} data-thread-entry={record.root.id}>
            <DashboardAskBody ask={record.root} />
            {askOpen ? null : (
              <p className="flex gap-2 text-xs">
                <button
                  className={ENTRY_CONTROL}
                  onClick={() => setOpen({ id: record.root.id, kind: "reply" })}
                  type="button"
                  {...pageChrome}
                >
                  {CONTROL_LABELS.reply}
                </button>
              </p>
            )}
            {newest.id === record.root.id ? <ReadMarkFoot markRef={markRef} /> : null}
          </li>
        ) : null}
        {record.kind === "ask" && open?.kind === "reply" && open.id === record.root.id ? (
          <li
            className={`min-w-0 ${ENTRY_INDENT}`}
            data-thread-box={record.root.id}
            style={depthStyle(1)}
            {...authorText}
          >
            <CommentBox
              closed={closed}
              issueKey={issue.key}
              onLeave={leave}
              parent={record.root}
              parentKind="ask"
              sendKey={["threads-reply", record.id, record.root.id]}
            />
          </li>
        ) : null}
        {entries}
      </ol>
      {foot === null ? null : <div data-thread-box="foot">{foot}</div>}
    </article>
  );
}
