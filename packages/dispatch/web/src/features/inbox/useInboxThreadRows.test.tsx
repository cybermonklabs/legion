/**
 * `useInboxThreadRows` over the Dismissed view's paged read (INFRA-450): the pages are flattened
 * once per set of pages, so the rows the hook hands the motion hold keep their identity between
 * renders.
 *
 * The hold adopts what it is given by identity and sets state whenever it differs
 * (`motion-hold.ts`), so a flatten made afresh on every render sets state on every render and
 * never settles - React ends that loop by throwing "Maximum update depth exceeded" and the page
 * draws its error boundary instead of the reader's dismissed threads.
 */

import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, waitFor } from "@testing-library/react";
import { type ReactNode, useRef } from "react";

import { correction, correctionFeed, DELETION } from "../../__tests__/correction-feed";
import { api } from "../../api/client";
import { useEventFeed } from "../../api/event-feed";
import {
  dismissedThreadsQuery,
  inboxThreadsKey,
  inboxThreadsQuery,
  whoAmIQuery,
} from "../../api/queries";
import type { MyRepliesResponse, MyReplyRow } from "../../api/types";
import type { InboxShow } from "../refs/routes";
import { replyRow } from "../threads/replies-fixture";
import { rowId } from "./list-row";
import { ThreadTitle } from "./ThreadTitle";
import { useInboxThreadRows } from "./useInboxThreadRows";

const AS_OF = "2026-10-12T02:00:00Z";

/** Every API mock a test installed, restored after it. */
const restores: { mockRestore: () => void }[] = [];

afterEach(() => {
  cleanup();
  for (const mock of restores.splice(0)) mock.mockRestore();
});

function dismissedPage(rows: readonly MyReplyRow[]): MyRepliesResponse {
  return { as_of: AS_OF, limit: 0, offset: 0, rows: [...rows], total: rows.length };
}

/** A client holding the sign-in read and one settled page of the Dismissed read, so the hook
 *  renders over pages it never has to fetch. */
function seeded(rows: readonly MyReplyRow[]): QueryClient {
  restores.push(spyOn(api, "listMyReplies").mockResolvedValue(dismissedPage(rows)));
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(whoAmIQuery().queryKey, { event_head: "0", kind: "user", login: "alice" });
  client.setQueryData(dismissedThreadsQuery().queryKey, {
    pageParams: [undefined],
    pages: [dismissedPage(rows)],
  });
  return client;
}

/** How many times the harness has rendered, which is what a hook that sets state on every render
 *  runs away with. */
let renders = 0;

function Harness(): ReactNode {
  renders += 1;
  const listRef = useRef<HTMLUListElement>(null);
  const threads = useInboxThreadRows({ asksOnly: false, listRef, show: "dismissed" });
  return (
    <ul ref={listRef}>
      {threads.rows.map((row) => (
        <li data-inbox-row={rowId(row)} key={rowId(row)} />
      ))}
    </ul>
  );
}

function rowIds(): string[] {
  return [...document.querySelectorAll("[data-inbox-row]")].map(
    (row) => row.getAttribute("data-inbox-row") ?? ""
  );
}

test("the Dismissed pages are flattened once, so a second render over them sets no state", async () => {
  const client = seeded([replyRow("c1"), replyRow("c9")]);
  renders = 0;
  const view = render(
    <QueryClientProvider client={client}>
      <Harness />
    </QueryClientProvider>
  );

  try {
    await waitFor(() => expect(rowIds()).toEqual(["reply:c1", "reply:c9"]));
    const settled = renders;

    // One render asked for, one render made: the rows the hold compares are the same array, so
    // its layout effect has nothing to adopt and asks for no render of its own.
    view.rerender(
      <QueryClientProvider client={client}>
        <Harness />
      </QueryClientProvider>
    );
    expect(renders).toBe(settled + 1);
    expect(rowIds()).toEqual(["reply:c1", "reply:c9"]);
  } finally {
    view.unmount();
  }
});

// R2 and R3 of dispatch://INFRA-1523, and the architect's decision of 2026-11-20 00:23Z. Neither
// Inbox read says that a refetch failed: each keeps the rows it held, so a post's correction
// writes its own facts into the rows the cache holds (`markCorrectedPost`). A row whose root is
// the post reads as edited, then, once deleted, as the deleted line with no word of its subject;
// a row whose latest reply is the post keeps no first line of it. The Inbox list holds a comment
// thread's row and the Dismissed list a message thread's, and every refetch fails.

const COMMENT_ROOT = "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d";
const MESSAGE_ROOT = "b2c3d4e5-f6a7-4b8c-9d0e-1f2a3b4c5d6e";
const LAST_REPLY = "c3d4e5f6-a7b8-4c9d-8e1f-2a3b4c5d6e7f";

/** One of the reader's two lists as its rows draw their titles. */
function ThreadTitles({ show }: { show: InboxShow }): ReactNode {
  const listRef = useRef<HTMLUListElement>(null);
  const threads = useInboxThreadRows({ asksOnly: false, listRef, show });
  return (
    <ul ref={listRef}>
      {threads.rows.map((row) =>
        row.kind === "thread" ? (
          <li data-inbox-row={rowId(row)} key={rowId(row)}>
            <ThreadTitle className="" thread={row.thread} />
          </li>
        ) : null
      )}
    </ul>
  );
}

test("a thread row whose refetch fails after its post's edit and delete reads as edited, then deleted", async () => {
  const commentThread = replyRow(COMMENT_ROOT, {
    reply: { ...replyRow(COMMENT_ROOT).reply, first_line: "Holding it.", id: LAST_REPLY },
    subject: "Hold the deploy.",
  });
  const messageThread = replyRow(MESSAGE_ROOT, {
    kind: "message",
    ref: `/issues/CORE-1/messages/${MESSAGE_ROOT}`,
    subject: "Ship on Friday.",
  });
  let failing = false;
  restores.push(
    spyOn(api, "listMyReplies").mockImplementation(async (page) => {
      if (failing) throw new Error("the Inbox read failed");
      return dismissedPage([page.state === "dismissed" ? messageThread : commentThread]);
    })
  );
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(whoAmIQuery().queryKey, { event_head: "0", kind: "user", login: "alice" });
  const feed = correctionFeed(client);
  function Page(): ReactNode {
    useEventFeed();
    return (
      <>
        <ThreadTitles show="inbox" />
        <ThreadTitles show="dismissed" />
      </>
    );
  }
  const view = render(
    <QueryClientProvider client={client}>
      <Page />
    </QueryClientProvider>
  );
  const title = (id: string) =>
    document.querySelector(`[data-inbox-row="reply:${id}"]`)?.textContent ?? null;

  try {
    await waitFor(() =>
      expect([title(COMMENT_ROOT), title(MESSAGE_ROOT)]).toEqual([
        "Hold the deploy.",
        "Ship on Friday.",
      ])
    );
    feed.start();
    failing = true;

    await feed.deliver(correction("comment", COMMENT_ROOT, true), [inboxThreadsKey]);
    await feed.deliver(correction("message", MESSAGE_ROOT, true), [inboxThreadsKey]);
    expect([title(COMMENT_ROOT), title(MESSAGE_ROOT)]).toEqual([
      "Hold the deploy. · edited",
      "Ship on Friday. · edited",
    ]);

    await feed.deliver(correction("comment", COMMENT_ROOT, false), [inboxThreadsKey]);
    await feed.deliver(correction("message", MESSAGE_ROOT, false), [inboxThreadsKey]);
    expect([title(COMMENT_ROOT), title(MESSAGE_ROOT)]).toEqual([
      "Deleted by alice",
      "Deleted by alice",
    ]);

    // The delete of the row's latest reply takes its first line, which the row's card prints.
    await feed.deliver(correction("comment", LAST_REPLY, false), [inboxThreadsKey]);
    expect(client.getQueryData(inboxThreadsQuery().queryKey)?.rows?.[0]?.reply).toEqual({
      ...commentThread.reply,
      ...DELETION,
      first_line: "",
    });
  } finally {
    view.unmount();
    feed.restore();
    client.clear();
  }
});
