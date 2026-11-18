/**
 * The draft matrix, Inbox message box row (INFRA-1503).
 *
 * Every composer host is held to one rule (packages/dispatch/AGENTS.md, "Who moved the
 * composer's address"): the reader's own move with the composer's own control takes the draft
 * with it, and "a move of the host's ... carries nothing: the text stays with the address it was
 * written for, and is there when the reader comes back to it." A refused or stopped send's note
 * stays at its own address. Each cell writes a draft in one state, typed and never sent, refused
 * by Dispatch, or stopped by the reader, makes one move, and asserts where the text ends.
 *
 * Host: the Inbox reading column's box on an issue message thread (`MessageThreadBody` in
 * `features/inbox/ThreadBody.tsx`), a standing box: the thread's own reply to its root message,
 * which nothing ends, drawn afresh for each row the column moves to (`ReadingPane`) and keyed by
 * its thread. The comment thread's box and the Agent row's box are the thread-box file's rows.
 *
 * A host move that takes the composer off the screen is checked on return alone, where the field
 * holds the draft with its refusal or stopped note.
 *
 * | Move                                             | typed  | refused | stopped |
 * | ------------------------------------------------ | ------ | ------- | ------- |
 * | Back, then back                                  | no (1) | no (1)  | no (1)  |
 * | The column moves to another row, then back (off) | cell   | cell    | cell    |
 * | Host moves it over a refusal (elsewhere)         | no (2) | no (2)  | no (2)  |
 * | Thread collapsed by the host                     | no (3) | no (3)  | no (3)  |
 * | Cancel reply                                     | no (4) | no (4)  | no (4)  |
 * | Escape in the field                              | cell   | cell    | cell    |
 * | Close, then Discard draft?                       | no (5) | no (5)  | no (5)  |
 * | Agents pick                                      | no (6) | no (6)  | no (6)  |
 * | Typed while elsewhere, then unmounted            | -      | no (2)  | -       |
 * | The thread's read fails or waits, then recovers  | no (7) | no (7)  | no (7)  |
 * | The thread's replies are ordered the other way   | no (8) | no (8)  | no (8)  |
 * | The Inbox resized, closed or left, then back     | no (9) | no (9)  | no (9)  |
 * | The thread's root message is no longer read      | no (10)| no (10) | no (10) |
 * | The page reloaded, then back (off)               | cell   | cell    | cell    |
 *
 * (off) marks a host move that takes the box off the screen: the column draws another thread's
 * box in its place. The reload row ends the whole page, as any reload does (`endAsAReload`), and
 * loads it again over the same tab: a new query client and write store over the tab's
 * `sessionStorage`, with the reader signed in as before. It is the box's lifetime the census finds
 * (`lifetime.ts`).
 *
 * The moves this host cannot make:
 * 1. The box draws no Back of its own; Escape from the reading column takes focus back to its row
 *    (`Inbox`'s `back`), and below the column's width there is no column to leave (11).
 * 2. The box always answers its thread's root message (`replyTo` names `thread.thread_id`'s
 *    message), so a newer message in the thread does not move it, and it is never elsewhere.
 * 3. The column draws no collapse of the thread; leaving it is the move to another row.
 * 4. A standing box's reply is its own address, so it draws no Cancel reply (asserted absent).
 * 5. The box has no anchor, so it draws no Close (asserted absent below).
 * 6. The issue picker is an Agents row's (`AgentMessageComposer`), and an issue message thread's
 *    box is a `MentionComposer` of its own, no Agents row's composer.
 * 7. `MessageThreadBody` draws a loading line or an error in place of the thread while the
 *    issue's messages are read or their read fails, which unmounts the box as the move to
 *    another row does, and the box drawn when the read lands again is the one that row draws.
 * 8. Which end of the thread the box sits at (`composerFirst`, from `useThreadOrder`) is read
 *    when the box's thread is drawn and again only when who is signed in changes
 *    (`useUserPreference`), so it never moves a box that is on screen; the shell draws no
 *    thread before it knows who is signed in, and another person is the sign-in file's row.
 * 9. The Inbox draws a thread's box in the reading column above the column's width and in the
 *    row's card below it (`paneShown`), in a drawer that closing takes away (`InboxDrawer`), and
 *    on its own page (`app.tsx`'s `/` and `/credentials/*`); a resize across that width, closing
 *    the drawer, leaving the page, a column with no row selected or an empty list, and the
 *    Inbox's loading line or error unmount the box as the move to another row does, and the box
 *    drawn on return is the one that row draws, at the same thread's send name.
 * 10. The box answers its thread's root message and is drawn only while the read holds it
 *    (`root === undefined` draws none), so a read without the root leaves no message to answer,
 *    and a read that holds it again draws the box at the same thread, as the move to another row
 *    and back does.
 *
 * The forms the composer census found, where the layout draws, hides or moves this composer
 * otherwise than the rows above, which the rows do not make:
 * 11. Below the reading column's width, a phone's included, the Inbox draws no column: each thread
 *    row is a card that draws its thread's own box (`ThreadRowCard`, `renderRow`'s `paneShown`),
 *    so no move takes a box from one row to another there, and each box stays at its thread while
 *    its row is listed. What ends a card's box is (9)'s: leaving the page, closing the drawer the
 *    compact header's Peek opens (`inboxDrawerOpen`), the row leaving the list. The box's own
 *    moves are the same `MessageThreadBody`'s as the rows above. The layout's other branches in
 *    `Inbox` and `usePaneSelection` pick, focus and scroll the column's row and draw the list
 *    beside the column, and move no box; the app shell's forms are the sign-in file's.
 *
 * `draft-matrix.message-box.coverage.ts` holds each census item this file answers for to a test
 * above or to a numbered reason, and is the source this header is the prose of.
 */
import { expect, spyOn, test } from "bun:test";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";

import {
  BODY,
  drawInboxColumn,
  HELD,
  holdInInboxBox,
  inboxBoxField,
  inboxNote,
  inboxPane,
  newQueryClient,
  REFUSAL,
  signedInAsReader,
} from "../../__tests__/draft-matrix-fixture";
import { sentRequest, stopSend } from "../../__tests__/stop-late-write";
import { api } from "../../api/client";
import type {
  IssueDetails,
  Message,
  MessageRead,
  MyReplyRow,
  MyReplyThreadRow,
} from "../../api/types";
import { installInboxApiMocks } from "./inbox-fixture";
import { threadSendKey } from "./ThreadBody";

installInboxApiMocks();

const HOST = "Inbox message box";

const bob = { id: "bob", kind: "user" } as const;

function issueMessage(id: string, issueKey: string): Message {
  return {
    author: bob,
    body: "Can someone look at the build?",
    broadcast_id: null,
    created_at: "2026-10-12T00:00:00Z",
    deliveries: [],
    id,
    in_reply_to: null,
    issue_key: issueKey,
    reply_block: null,
    target: null,
  };
}

/** Each issue's message conversation: its root message and no replies yet. */
const conversations: Record<string, MessageRead[]> = {
  "CORE-1": [{ message: issueMessage("message-1", "CORE-1"), replies: [] }],
  "CORE-2": [{ message: issueMessage("message-9", "CORE-2"), replies: [] }],
};

function messageRow(overrides: Partial<MyReplyThreadRow>): MyReplyThreadRow {
  return {
    issue: { key: "CORE-1", title: "Fix the thing" },
    kind: "message",
    read_through: "2026-10-12T00:30:00Z",
    ref: "/issues/CORE-1?message=message-1",
    reply: {
      author: bob,
      created_at: "2026-10-12T01:00:00Z",
      first_line: "Can someone look at the build?",
      id: "message-1",
      kind: "reply",
    },
    subject: "The build",
    thread_id: "message-1",
    unread: false,
    unread_count: 0,
    version: 1,
    ...overrides,
  };
}

/** The message thread the cells write in, and the one the column moves to. */
const firstRow = messageRow({});
const otherRow = messageRow({
  issue: { key: "CORE-2", title: "Other issue" },
  ref: "/issues/CORE-2?message=message-9",
  thread_id: "message-9",
});

/** A reply of `BODY` from the first thread's box to its root message. */
const messageReply = sentRequest(BODY, {
  actor: bob,
  excerpt: "",
  id: "message-1",
  parentKind: "message",
});

interface Column {
  /** The column moved to another row, as the Inbox moves it: the same column, drawn again. */
  readonly move: (row: MyReplyRow) => void;
  /** Ends the page as a reload does and loads it again over the same tab on `row`, signed in as
   *  the same reader, with a new query client (`endAsAReload`, `signedInAsReader`). */
  readonly reload: (row: MyReplyRow) => void;
  readonly restore: () => void;
  readonly user: UserEvent;
}

/**
 * The Inbox reading column on `row`, over the two message threads. Every write is refused, so a
 * send pressed here comes back refused; with `stopped`, the first thread's reply is sent through
 * the held-send store under its box's name before the column is drawn and stopped once late, as
 * the reader's Stop trying does, so the box mounts showing it.
 */
async function openColumn(row: MyReplyRow, stopped: boolean): Promise<Column> {
  const user = userEvent.setup();
  const client = signedInAsReader(newQueryClient());
  const reads = [
    // The box reads one field of the issue: whether it is closed.
    spyOn(api, "getIssue").mockResolvedValue({ closed_at: null } as unknown as IssueDetails),
    spyOn(api, "listIssueMessages").mockImplementation(
      async (issueKey) => conversations[issueKey] ?? []
    ),
    spyOn(api, "listAgents").mockResolvedValue([]),
  ];
  const createComment = spyOn(api, "createComment");
  const createMessage = spyOn(api, "createMessage");
  if (stopped) {
    await stopSend(client, threadSendKey("message-1"), messageReply, [
      createComment,
      createMessage,
    ]);
  }
  createComment.mockRejectedValue(REFUSAL);
  createMessage.mockRejectedValue(REFUSAL);
  const column = drawInboxColumn(client, row, () => signedInAsReader(newQueryClient()));
  return {
    move: column.move,
    reload: column.reload,
    restore: () => {
      column.unmount();
      createComment.mockRestore();
      createMessage.mockRestore();
      for (const read of reads) read.mockRestore();
    },
    user,
  };
}

// The column moving to another row is the host's move, and it takes the box off the screen: the
// box's draft waits under its send name, and the box drawn when the column comes back shows it.
for (const held of HELD) {
  test(`${HOST} · the column moves to another row, then back · ${held}`, async () => {
    const column = await openColumn(firstRow, held === "stopped");
    try {
      await holdInInboxBox(column.user, held);
      column.move(otherRow);
      const other = await waitFor(() => {
        const box = inboxPane().querySelector('[data-inbox-thread="message-9"]');
        if (box === null) throw new Error("expected the other thread");
        return inboxBoxField().value;
      });
      column.move(firstRow);
      await waitFor(() =>
        expect(inboxPane().querySelector('[data-inbox-thread="message-1"]')).not.toBe(null)
      );
      expect({ back: { note: inboxNote(), text: inboxBoxField().value }, other }).toEqual({
        back: { note: held === "typed" ? "none" : held, text: BODY },
        other: "",
      });
    } finally {
      column.restore();
    }
  });
}

// A standing box's Escape ends nothing: the draft, and a note with it, stay in the box.
for (const held of HELD) {
  test(`${HOST} · Escape in the field · ${held}`, async () => {
    const column = await openColumn(firstRow, held === "stopped");
    try {
      await holdInInboxBox(column.user, held);
      await column.user.click(inboxBoxField());
      await column.user.keyboard("{Escape}");
      expect({ note: inboxNote(), text: inboxBoxField().value }).toEqual({
        note: held === "typed" ? "none" : held,
        text: BODY,
      });
    } finally {
      column.restore();
    }
  });
}

test(`${HOST} · Cancel reply · cannot make: a standing box draws no Cancel reply`, async () => {
  const column = await openColumn(firstRow, false);
  try {
    await holdInInboxBox(column.user, "typed");
    expect(within(inboxPane()).queryAllByRole("button", { name: "Cancel reply" }).length).toBe(0);
  } finally {
    column.restore();
  }
});

test(`${HOST} · Close, then Discard draft? · cannot make: the box draws no Close`, async () => {
  const column = await openColumn(firstRow, false);
  try {
    await holdInInboxBox(column.user, "typed");
    expect(within(inboxPane()).queryAllByRole("button", { name: "Close" }).length).toBe(0);
  } finally {
    column.restore();
  }
});

// A reload ends the whole page and loads it again over the same tab (`endAsAReload`), with the
// reader signed in as before. The column drawn on the thread again, the way back to the box,
// shows the reply the reader wrote there, with its note.
for (const held of HELD) {
  test(`${HOST} · the page reloaded, then back · ${held}`, async () => {
    const column = await openColumn(firstRow, held === "stopped");
    try {
      await holdInInboxBox(column.user, held);
      column.reload(firstRow);
      const field = await within(
        await screen.findByTestId("inbox-pane")
      ).findByRole<HTMLTextAreaElement>("textbox", { name: "Reply" });
      expect({ note: inboxNote(), text: field.value }).toEqual({
        note: held === "typed" ? "none" : held,
        text: BODY,
      });
    } finally {
      column.restore();
    }
  });
}
