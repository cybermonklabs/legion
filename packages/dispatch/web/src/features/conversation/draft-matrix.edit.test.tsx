/**
 * The draft matrix, comment edit row (INFRA-1503).
 *
 * Every composer host is held to one rule (packages/dispatch/AGENTS.md, "Who moved the
 * composer's address"): "a move of the host's ... carries nothing: the text stays with the
 * address it was written for, and is there when the reader comes back to it." An edit's address
 * is the comment it edits, and what the reader wrote there was written over one body of that
 * comment. A kept edit comes back in the field only while the comment still holds that body. If
 * another writer changed the comment meanwhile, the field shows the newer body, the kept edit is
 * offered beside it with words saying the comment changed since the edit was written, and nothing
 * is saved over the newer body until the reader presses. Each cell writes an edit in one state -
 * typed and never saved, refused by Dispatch, or stopped by the reader - takes it off the screen
 * with the host's move, leaves the comment's body as it was or has another writer change it,
 * comes back, and asserts what the editor holds and that no save went out meanwhile.
 *
 * Hosts: a comment's thread card draws the editor (`ThreadCard`'s Edit, `MentionComposer`'s
 * `edit`) in the Conversation's list, in the Conversation's phone thread view, in the margin's
 * list, and in the margin's phone thread view.
 *
 * | Host · move (off)                    | base      | typed | refused | stopped |
 * | ------------------------------------ | --------- | ----- | ------- | ------- |
 * | Conversation card · Collapse, Expand | unchanged | cell  | cell    | cell    |
 * | Conversation card · Collapse, Expand | changed   | cell  | cell    | cell    |
 * | Conversation phone · Back, then back | unchanged | cell  | cell    | cell    |
 * | Conversation phone · Back, then back | changed   | cell  | cell    | cell    |
 * | margin card · other thread, back     | unchanged | cell  | cell    | cell    |
 * | margin card · other thread, back     | changed   | cell  | cell    | cell    |
 * | margin phone · Back, then back       | unchanged | cell  | cell    | cell    |
 * | margin phone · Back, then back       | changed   | cell  | cell    | cell    |
 *
 * The base is the comment's body the edit was written over: "unchanged" where nobody changed it
 * while the edit was away, "changed" where another writer did. (off) marks a host move that
 * takes the editor off the screen: Collapse thread and another thread's opening close the card
 * the editor is in, and Back takes the thread view off the page. Every move of this row is one,
 * so each cell is checked on return alone. A stopped save is held for its comment as a refused
 * one is (`HeldSends`): the editor opens on it when its thread does, with the stopped note in
 * place of the refusal's.
 *
 * The second half keeps the editor on screen while another writer changes the comment under it.
 * Changed to newer text, the field shows the comment as it reads now and the edit waits beside it
 * with the same words as on return, since that is what the away cells show for the same change.
 * Changed to the very text the reader wrote, there is nothing to offer: the field shows that text
 * and no line says the comment changed. Each cell then takes the editor off the screen with the
 * host's move and back, and asserts the same both times, with no save in between.
 *
 * | Host · changed on screen, then move  | typed | typed | refused | refused | stopped | stopped |
 * |                                      | newer | own   | newer   | own     | newer   | own     |
 * | ------------------------------------ | ----- | ----- | ------- | ------- | ------- | ------- |
 * | Conversation card · Collapse, Expand | cell  | cell  | cell    | cell    | cell    | cell    |
 * | Conversation phone · Back, then back | cell  | cell  | cell    | cell    | cell    | cell    |
 * | margin card · other thread, back     | cell  | cell  | cell    | cell    | cell    | cell    |
 * | margin phone · Back, then back       | cell  | cell  | cell    | cell    | cell    | cell    |
 *
 * "newer" is newer text another writer gave the comment, "own" the text the reader wrote.
 *
 * The third part moves a thread's one editor (`editingCommentId`, one per thread) to the reader's
 * own reply in the same thread with the reply's Edit, the card's control and so the host's move,
 * and back with the comment's own Edit. The comment's edit is kept and comes back over the body it
 * was written over; a refused or stopped save holds its editor open throughout; and the reply's
 * editor opens on the reply's own body, never on the comment's edit.
 *
 * | Host · the reply's Edit, then the comment's Edit | typed  | refused | stopped |
 * | ------------------------------------------------ | ------ | ------- | ------- |
 * | Conversation card                                | cell   | cell    | cell    |
 * | Conversation phone thread                        | cell   | cell    | cell    |
 * | margin card                                      | cell   | cell    | cell    |
 * | margin phone thread                              | cell   | cell    | cell    |
 *
 * The fourth part ends the whole page, as any reload does (`endAsAReload`), and loads it again
 * over the same tab: a new query client and write store over the tab's `sessionStorage`, with the
 * reader signed in as before. It is the editor's lifetime the census finds (`lifetime.ts`). The
 * comment's thread, opened again, holds the edit over the body it was written over, in the editor
 * a held save opens on its own, else in the one the comment's Edit opens.
 *
 * | Host · the page reloaded, then back | typed | refused | stopped |
 * | ----------------------------------- | ----- | ------- | ------- |
 * | Conversation card                   | cell  | cell    | cell    |
 * | Conversation phone thread           | cell  | cell    | cell    |
 * | margin card                         | cell  | cell    | cell    |
 * | margin phone thread                 | cell  | cell    | cell    |
 *
 * The hosts that cannot make the move: the docked composer, the phone thread composer, an Agents
 * row and an Inbox thread box are never given an `edit`, so they draw no editor to move.
 *
 * The numbered reasons - the moves and forms the composer census found that these rows do not
 * make - are `REASONS` in `draft-matrix.edit.coverage.ts`, which holds each census item this file
 * answers for to a test above or to one of them.
 */
import { expect, type Mock, spyOn, test } from "bun:test";
import type { MutationKey, QueryClient } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";

import {
  commentA,
  endAsAReload,
  HELD,
  type Held,
  listTurn,
  marginCard,
  marginClient,
  marginPage,
  NOTHING_READ,
  newQueryClient,
  noteIn,
  openMarginThread,
  REFUSAL,
  REFUSED_NOTE,
  RegisterDocument,
  STOPPED_NOTE,
  signedInAsReader,
  stubLists,
  tab,
  threadA,
  threadB,
} from "../../__tests__/draft-matrix-fixture";
import { graphNode } from "../../__tests__/graph-fixture";
import { sentRequest, stopSend } from "../../__tests__/stop-late-write";
import { api } from "../../api/client";
import type { Comment } from "../../api/types";
import { DESKTOP_WIDTH, issue, PHONE_WIDTH, stubViewport } from "../margin/margin-fixture";
import { saveCommentEdit } from "./send-request";

/** The send name of a comment's editor (`ThreadCard`'s `COMMENT_EDIT_SEND_KEY`). */
const editKey = (id: string): MutationKey => ["comment-edit", id];

/** What the reader writes in the editor, and the body another writer gives the comment. */
const BODY = "Edit the reader wrote";
const NEWER = "The comment as another writer changed it";

/** The body of the reader's own reply in the edited comment's thread. */
const REPLY_BODY = "A reply the reader wrote";

type Base = "unchanged" | "changed";
const BASES: readonly Base[] = ["unchanged", "changed"];

interface Page {
  /** Has another writer change the edited comment to `body`, as a refetch would bring it. */
  readonly changeBody: (body: string) => void;
  /** Every save of an edit the page made: the request log a cell reads. */
  readonly editComment: Mock<typeof api.editComment>;
  /** Ends the page as a reload does and loads it again over the same tab, signed in as the same
   *  reader, with a new query client (`endAsAReload`, `signedInAsReader`). */
  readonly reload: () => Promise<void>;
  readonly restore: () => void;
  readonly user: UserEvent;
}

/** Saves `BODY` over `edited`'s body through the held-send store under its editor's name, to a
 *  server that is down, and stops the save once it is late, as the reader's Stop trying does:
 *  the store holds it stopped for the comment, and the editor opens on it when its thread does.
 *  Every save after it is refused. */
async function stopTheEditSave(
  queryClient: QueryClient,
  editComment: Mock<typeof api.editComment>,
  edited: Comment
): Promise<void> {
  const edit = { base: edited.body, carried: undefined, id: edited.id, save: saveCommentEdit };
  await stopSend(queryClient, editKey(edited.id), sentRequest(BODY, null, { edit }), [editComment]);
  editComment.mockRejectedValue(REFUSAL);
}

/** The reader's Comment A, which the Conversation's cells edit. */
const conversationComment: Comment = commentA;

/** The reader's own reply under Comment A, whose Edit the reply-row cells press. */
const conversationReply: Comment = {
  ...conversationComment,
  body: REPLY_BODY,
  created_at: "2026-09-20T00:05:00Z",
  id: "comment-a-reply",
  reply_to: conversationComment.id,
};

/** The Conversation of CORE-1 over the reader's Comment A, at a phone width or above it; with
 *  `stopped`, an edit of Comment A whose save the reader stopped is held before it is drawn, and
 *  with `withReply`, Comment A's thread holds the reader's reply under it. Every save is
 *  refused, and the comment's blocks answer it not indexed, so a save carries no block ids. The
 *  reader is signed in, since a card draws Edit for the reader's own comments alone. */
async function openConversation(
  phone: boolean,
  stopped: boolean,
  withReply = false
): Promise<Page> {
  const user = userEvent.setup();
  let queryClient = signedInAsReader(newQueryClient());
  const reads = stubLists({
    comments: withReply ? [conversationComment, conversationReply] : [conversationComment],
  });
  const editComment = spyOn(api, "editComment").mockRejectedValue(REFUSAL);
  if (stopped) await stopTheEditSave(queryClient, editComment, conversationComment);
  const restoreViewport = stubViewport(phone ? PHONE_WIDTH : DESKTOP_WIDTH);
  let view = render(tab(NOTHING_READ, true, queryClient));
  const listed = () =>
    waitFor(() => expect(document.querySelector('[data-turn="comment:comment-a"]')).not.toBe(null));
  await listed();
  return {
    changeBody: (body) => {
      const changed = [{ ...conversationComment, body, edited_at: "2026-09-21T00:00:00Z" }];
      reads.lists.comments = changed;
      act(() => {
        queryClient.setQueryData(["comments", "CORE-1"], changed);
      });
    },
    editComment,
    reload: async () => {
      endAsAReload(() => view.unmount());
      queryClient = signedInAsReader(newQueryClient());
      view = render(tab(NOTHING_READ, true, queryClient));
      await listed();
    },
    restore: () => {
      view.unmount();
      restoreViewport();
      editComment.mockRestore();
      reads.restore();
    },
    user,
  };
}

/** Comment A's turn in the Conversation's list, outside any thread view. */
const turn = () => listTurn(commentA.id);

/** The reader's own reply in thread A, whose Edit the reply-row cells press. */
const marginReply: Comment = {
  ...threadA,
  anchor: null,
  body: REPLY_BODY,
  created_at: "2026-09-09T00:05:00Z",
  id: "comment-1-reply",
  reply_to: threadA.id,
};

/** The margin of CORE-1's spec over threads A and B, at a phone width or above it; with
 *  `stopped`, an edit of thread A's comment whose save the reader stopped is held before it is
 *  drawn, and with `withReply`, thread A holds the reader's reply under its comment. Every save
 *  is refused, and the comment's blocks answer it not indexed, so a save carries no block ids.
 *  The reader is signed in, since a card draws Edit for the reader's own comments alone. */
async function openMargin(phone: boolean, stopped: boolean, withReply = false): Promise<Page> {
  const user = userEvent.setup();
  let comments: readonly Comment[] = withReply
    ? [threadA, marginReply, threadB]
    : [threadA, threadB];
  let queryClient = signedInAsReader(marginClient(comments));
  const reads = [
    spyOn(api, "getIssue").mockResolvedValue(issue),
    spyOn(api, "listComments").mockImplementation(async () => [...comments]),
    spyOn(api, "commentBlocks").mockImplementation(async (id) => ({
      blocks: [],
      indexed: false,
      item: graphNode({ id, issue_key: "CORE-1", kind: "comment" }),
    })),
  ];
  const editComment = spyOn(api, "editComment").mockRejectedValue(REFUSAL);
  if (stopped) await stopTheEditSave(queryClient, editComment, threadA);
  const restoreViewport = stubViewport(phone ? PHONE_WIDTH : DESKTOP_WIDTH);
  let view = render(marginPage(queryClient, <RegisterDocument />));
  await waitFor(() => expect(marginCard(threadA.id)).toBeInTheDocument());
  return {
    changeBody: (body) => {
      comments = [{ ...threadA, body, edited_at: "2026-09-21T00:00:00Z" }, threadB];
      act(() => {
        queryClient.setQueryData(["comments", issue.key], comments);
      });
    },
    editComment,
    reload: async () => {
      endAsAReload(() => view.unmount());
      queryClient = signedInAsReader(marginClient(comments));
      view = render(marginPage(queryClient, <RegisterDocument />));
      await waitFor(() => expect(marginCard(threadA.id)).toBeInTheDocument());
    },
    restore: () => {
      view.unmount();
      restoreViewport();
      editComment.mockRestore();
      for (const read of reads) read.mockRestore();
    },
    user,
  };
}

/** Where the thread view a phone opens shows, once it does. */
const threadView = () => screen.findByRole("dialog", { name: "Thread" });

async function leaveThreadView(user: UserEvent): Promise<void> {
  await user.click(within(await threadView()).getByRole("button", { name: "Back" }));
  await waitFor(() => expect(screen.queryByRole("dialog", { name: "Thread" })).toBe(null));
}

/** A host of the editor: how its page opens, where the edited comment's thread shows, and the
 *  host's own move that takes the editor off the screen. */
interface Host {
  readonly away: (page: Page) => Promise<void>;
  /** Where the edited comment's thread shows while it is on screen, looked up afresh. */
  readonly here: () => HTMLElement;
  readonly move: string;
  readonly name: string;
  /** Opens the host's page; with `stopped`, over an edit of the comment whose save the reader
   *  stopped, and with `withReply`, with the reader's own reply in the comment's thread. */
  readonly open: (stopped: boolean, withReply?: boolean) => Promise<Page>;
  /** Opens the edited comment's thread and answers where it shows. */
  readonly show: (page: Page) => Promise<HTMLElement>;
}

const HOSTS: readonly Host[] = [
  {
    away: async ({ user }) => {
      await user.click(within(turn()).getByRole("button", { name: "Collapse thread" }));
    },
    here: turn,
    move: "Collapse thread, then Expand thread",
    name: "Conversation card",
    open: (stopped, withReply) => openConversation(false, stopped, withReply),
    show: async ({ user }) => {
      await user.click(within(turn()).getByRole("button", { name: "Expand thread" }));
      return turn();
    },
  },
  {
    away: ({ user }) => leaveThreadView(user),
    here: () => screen.getByRole("dialog", { name: "Thread" }),
    move: "Back, then back",
    name: "Conversation phone thread",
    open: (stopped, withReply) => openConversation(true, stopped, withReply),
    show: async ({ user }) => {
      await user.click(within(turn()).getByRole("button", { name: "Expand thread" }));
      return threadView();
    },
  },
  {
    away: ({ user }) => openMarginThread(user, threadB.id),
    here: () => marginCard(threadA.id),
    move: "another thread opened, then back",
    name: "margin card",
    open: (stopped, withReply) => openMargin(false, stopped, withReply),
    show: async ({ user }) => {
      await openMarginThread(user, threadA.id);
      return marginCard(threadA.id);
    },
  },
  {
    away: ({ user }) => leaveThreadView(user),
    here: () => screen.getByRole("dialog", { name: "Thread" }),
    move: "Back, then back",
    name: "margin phone thread",
    open: (stopped, withReply) => openMargin(true, stopped, withReply),
    show: async ({ user }) => {
      // The thread view a tap on the card opens, or the card itself where the list shows it open
      // already once the reader is back from the thread view.
      if (marginCard(threadA.id).getAttribute("aria-expanded") === "true") {
        return marginCard(threadA.id);
      }
      await openMarginThread(user, threadA.id);
      return threadView();
    },
  },
];

const editField = (place: HTMLElement) =>
  within(place).queryByRole<HTMLTextAreaElement>("textbox", { name: "Edit comment" });

/** Opens the editor on the comment `place` shows and writes `BODY` over its body, in the state
 *  `held` names. A stopped edit is the one the page was opened over, whose editor is open on its
 *  comment already. */
async function writeEdit(page: Page, place: HTMLElement, held: Held): Promise<void> {
  if (held === "stopped") {
    await within(place).findByText(STOPPED_NOTE);
    return;
  }
  // The comment's own Edit comes first, above any reply's.
  const [own] = within(place).getAllByRole("button", { name: "Edit" });
  if (own === undefined) throw new Error("expected the comment's Edit");
  await page.user.click(own);
  const field = editField(place);
  if (field === null) throw new Error("expected the editor");
  await page.user.click(field);
  await page.user.keyboard("{Control>}a{/Control}");
  await page.user.paste(BODY);
  if (held === "refused") {
    const form = field.closest("form");
    if (form === null) throw new Error("expected the editor's form");
    await page.user.click(within(form).getByRole("button", { name: "Save" }));
    await within(place).findByText(REFUSED_NOTE);
  }
}

/** Whether a line in `place` says the comment changed since the edit was written, and offers the
 *  edit itself beside the newer body. A status inside something hidden is no role the query
 *  finds. */
function offered(place: HTMLElement): boolean {
  return within(place)
    .queryAllByRole("status")
    .some((line) => {
      const text = line.textContent ?? "";
      return /\bchanged since\b/.test(text) && text.includes(BODY);
    });
}

/** What the editor in `place` shows - the field's text, and whether the edit is offered beside
 *  it - and how many saves of an edit the page has made so far. */
function inEditor(
  page: Page,
  place: HTMLElement
): { offered: boolean; saves: number; text: string } {
  return {
    offered: offered(place),
    saves: page.editComment.mock.calls.length,
    text: editField(place)?.value ?? "(no editor on screen)",
  };
}

for (const host of HOSTS) {
  for (const held of HELD) {
    for (const base of BASES) {
      test(`${host.name} edit · ${host.move} · ${held} · base ${base}`, async () => {
        const page = await host.open(held === "stopped");
        try {
          await writeEdit(page, await host.show(page), held);
          // Counted here, so a refused cell's own Save, and a stopped one's attempts, are not
          // saves made while away.
          const saves = page.editComment.mock.calls.length;
          await host.away(page);
          if (base === "changed") page.changeBody(NEWER);
          const place = await host.show(page);
          const shown = inEditor(page, place);
          if (base === "unchanged") {
            expect({ ...shown, note: noteIn(place) }).toEqual({
              note: held === "typed" ? "none" : held,
              offered: false,
              saves,
              text: BODY,
            });
          } else {
            expect(shown).toEqual({ offered: true, saves, text: NEWER });
          }
        } finally {
          page.restore();
        }
      });
    }
  }
}

/** What another writer changes the comment to while the editor is on screen: newer text, or the
 *  very text the reader wrote, which leaves nothing to offer. */
type Change = "newer text" | "the edit's own text";
const CHANGES: readonly Change[] = ["newer text", "the edit's own text"];

for (const host of HOSTS) {
  for (const held of HELD) {
    for (const change of CHANGES) {
      test(`${host.name} edit · comment changed on screen to ${change}, then ${host.move} · ${held}`, async () => {
        const page = await host.open(held === "stopped");
        try {
          await writeEdit(page, await host.show(page), held);
          // Counted here, so a refused cell's own Save is not a save made since.
          const saves = page.editComment.mock.calls.length;
          const body = change === "newer text" ? NEWER : BODY;
          page.changeBody(body);
          const onScreen = inEditor(page, host.here());
          await host.away(page);
          const back = inEditor(page, await host.show(page));
          const expected = { offered: change === "newer text", saves, text: body };
          expect({ back, onScreen }).toEqual({ back: expected, onScreen: expected });
        } finally {
          page.restore();
        }
      });
    }
  }
}

/** The text of each editor open in `place`, in the order the thread draws them: the comment's
 *  own above its reply's. */
function editorsIn(place: HTMLElement): string {
  return within(place)
    .queryAllByRole<HTMLTextAreaElement>("textbox", { name: "Edit comment" })
    .map((field) => field.value)
    .join(" | ");
}

// A thread keeps one editor open (`editingCommentId`, one per thread's root), so the Edit of the
// reader's reply in the same thread closes the comment's editor: the host's move, since Edit is
// the card's control, not the editor's. The edit is held for the comment, and the comment's own
// Edit brings it back over the body it was written over; a refused or stopped save holds its
// editor open (`useHeldSendsUnder`), so it never leaves. The reply's editor opens on the reply's
// own body, and closes again when the comment's Edit moves the thread's one editor back.
for (const host of HOSTS) {
  for (const held of HELD) {
    test(`${host.name} edit · Edit on the reader's reply in the thread, then on the comment again · ${held}`, async () => {
      const page = await host.open(held === "stopped", true);
      try {
        await writeEdit(page, await host.show(page), held);
        // Counted here, so a refused cell's own Save, and a stopped one's attempts, are not
        // saves made since.
        const saves = page.editComment.mock.calls.length;
        const replies = within(host.here()).getByRole("list", { name: "Replies" });
        await page.user.click(within(replies).getByRole("button", { name: "Edit" }));
        const away = editorsIn(host.here());
        // The comment's own Edit comes first, above the reply's.
        const [own] = within(host.here()).getAllByRole("button", { name: "Edit" });
        if (own === undefined) throw new Error("expected the comment's Edit");
        await page.user.click(own);
        expect({
          away,
          back: editorsIn(host.here()),
          note: noteIn(host.here()),
          saves: page.editComment.mock.calls.length,
        }).toEqual({
          away: held === "typed" ? REPLY_BODY : `${BODY} | ${REPLY_BODY}`,
          back: BODY,
          note: held === "typed" ? "none" : held,
          saves,
        });
      } finally {
        page.restore();
      }
    });
  }
}

// A reload ends the whole page and loads it again over the same tab (`endAsAReload`), with the
// reader signed in as before. The comment's thread, opened again, holds the edit the reader wrote
// over its unchanged body, with its note and no save made meanwhile: in the editor a refused or
// stopped save opens on its own, else in the one the comment's Edit opens, since the comment is
// the editor's address.
for (const host of HOSTS) {
  for (const held of HELD) {
    test(`${host.name} edit · the page reloaded, then back · ${held}`, async () => {
      const page = await host.open(held === "stopped");
      try {
        await writeEdit(page, await host.show(page), held);
        // Counted here, so a refused cell's own Save, and a stopped one's attempts, are not saves
        // made since.
        const saves = page.editComment.mock.calls.length;
        await page.reload();
        const place = await host.show(page);
        if (editField(place) === null) {
          // The comment's own Edit comes first, above any reply's.
          const [own] = within(place).getAllByRole("button", { name: "Edit" });
          if (own === undefined) throw new Error("expected the comment's Edit");
          await page.user.click(own);
        }
        expect({ ...inEditor(page, host.here()), note: noteIn(host.here()) }).toEqual({
          note: held === "typed" ? "none" : held,
          offered: false,
          saves,
          text: BODY,
        });
      } finally {
        page.restore();
      }
    });
  }
}
