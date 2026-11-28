import type { ReactNode } from "react";

import { compareTimestamps } from "../../lib/timestamps";
import { EVENT_FRESH } from "../refs/reference-target";
import { useMarkThreadRead } from "../threads/useMarkThreadRead";
import type { TreeNode } from "./issue-tree";
import { conversationItem } from "./outline-writer";

/** Where the outline marks one thread read: the issue it is on, the thread's id, and the time of
 *  the newest write in it the outline draws. */
export interface ThreadMark {
  readonly at: string;
  readonly issueKey: string;
  readonly threadId: string;
}

/**
 * Each thread of the outline by the node that draws its newest write (`useMarkThreadRead`, the
 * rule every surface that draws a thread marks its Inbox row read by). A thread is an ask, a
 * comment or a message no other conversation entry holds, with every reply drawn under it at any
 * depth; an ask's answer is one of its writes, drawn in the ask's own row. A thread with no reply
 * and no answer has nothing to read past its head, and marks nothing, as the threads it replaced
 * in the panel did not.
 *
 * `root` is the issue's node, whose id is the issue's key.
 */
export function threadMarks(root: TreeNode): ReadonlyMap<string, ThreadMark> {
  const marks = new Map<string, ThreadMark>();
  const visit = (node: TreeNode): void => {
    const item = conversationItem(node);
    if (item === undefined) {
      for (const child of node.children) visit(child);
      return;
    }
    let newest: { readonly at: string; readonly node: string } | undefined;
    const consider = (id: string, at: string | undefined) => {
      if (at !== undefined && (newest === undefined || compareTimestamps(at, newest.at) > 0)) {
        newest = { at, node: id };
      }
    };
    const replies = (entry: TreeNode): void => {
      for (const child of entry.children) {
        if (conversationItem(child) === undefined) {
          visit(child);
          continue;
        }
        consider(child.id, child.at);
        consider(child.id, child.answer?.at);
        replies(child);
      }
    };
    consider(node.id, node.answer?.at);
    replies(node);
    if (newest !== undefined) {
      marks.set(newest.node, { at: newest.at, issueKey: root.id, threadId: item.id });
    }
  };
  visit(root);
  return marks;
}

/**
 * The read mark of the thread whose newest write is this row: once the row's foot has been on
 * screen for a second, the thread's Inbox row is read through that write. It takes no room: one
 * pixel along the foot of the row's card.
 *
 * It reads who is reading and the reader's unread set on the issue as the tree's own reads are
 * kept fresh (`EVENT_FRESH`): the stage draws its row again at each press of a view, and under the
 * app's 30 s `staleTime` a row drawn more than 30 s after the page loaded would ask for both again,
 * though neither has changed: the feed's rules refresh the unread set whenever an event changes it
 * (`api/event-refresh.ts`), and who is reading does not change while the page is open.
 */
export function ThreadReadMark({ mark }: { mark: ThreadMark }): ReactNode {
  const { ref } = useMarkThreadRead({
    enabled: true,
    freshness: EVENT_FRESH,
    newestShownAt: mark.at,
    owner: { issue: mark.issueKey },
    threadId: mark.threadId,
  });
  return (
    <span
      aria-hidden="true"
      className="pointer-events-none absolute inset-x-0 bottom-0 h-px"
      data-thread-read={mark.threadId}
      ref={ref}
    />
  );
}
