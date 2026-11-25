/**
 * The draft matrix, phone thread row (INFRA-1503).
 *
 * Every composer host is held to one rule (`.dispatch/architecture/dispatch-web.md`, "A composer's
 * draft - typed, refused or stopped - goes where the person takes it, and stays where the host
 * moved away from"): the reader's own move with the composer's own control takes the draft with
 * it, and a move of the host's carries nothing: the text stays with the address it was written
 * for, and is there when the reader comes back to it. A refused or stopped send's note
 * stays at its own address. Each cell writes a draft in one state - typed and never sent,
 * refused by Dispatch, or stopped by the reader - makes one move, and asserts where the text
 * ends.
 *
 * Host: on a phone, a comment's Reply answers in the comment's full-screen thread, by the thread
 * view's own composer (`usePhoneThread`, `ConversationTab`'s thread dialog).
 *
 * A host move that takes the composer off the screen is checked on return alone, where the field
 * holds the draft with its refusal or stopped note; a host move that keeps the same composer on
 * screen at another address is checked on return too and, while away, for a line on screen that
 * says the earlier text is kept and names its comment's author or words.
 *
 * | Move                                     | typed  | refused | stopped |
 * | ---------------------------------------- | ------ | ------- | ------- |
 * | Back, then back (off)                    | cell   | cell    | cell    |
 * | Reply on another comment, then back (on) | cell   | cell    | cell    |
 * | Host moves it over a refusal (elsewhere) | no (1) | no (1)  | no (1)  |
 * | Thread collapsed by the host             | no (2) | no (2)  | no (2)  |
 * | Cancel reply                             | cell   | cell    | cell    |
 * | Escape in the field                      | cell   | cell    | cell    |
 * | Close, then Discard draft?               | no (3) | no (3)  | no (3)  |
 * | Agents pick                              | no (4) | no (4)  | no (4)  |
 * | Typed while elsewhere, then unmounted    | -      | no (1)  | -       |
 * | Issue left, then back (off)              | (5)    | (5)     | (5)     |
 * | Another tab, then back (hidden)          | (5)    | (5)     | (5)     |
 * | Another comment's thread opened (hidden) | no (6) | no (6)  | no (6)  |
 * | The Conversation's reads fail or wait    | no (7) | no (7)  | no (7)  |
 * | The page reloaded, then back (off)       | cell   | cell    | cell    |
 * | The comment replied to leaves the list   | cell   | no (1)  | no (1)  |
 * | Put away, then its comment leaves        | cell   | cell    | cell    |
 *
 * (off) marks a host move that takes the composer off the screen: Back hides the thread view,
 * and the docked composer it uncovers answers the issue, not the comment. (on) marks one that
 * keeps the same thread composer on screen at another comment's address. A refused or stopped
 * draft never leaves its field there, since another comment's Reply opens the held comment's
 * thread instead, so only the typed draft needs the line. The reload row ends the whole page, as
 * any reload does (`endAsAReload`), and loads it again over the same tab: a new query client and
 * write store over the tab's `sessionStorage`, with the reader signed in as before. It is the
 * composer's lifetime the census finds (`lifetime.ts`).
 *
 * The two rows about the comment replied to are a host move of another kind: a refetch answers
 * the comments without the one a kept reply answers, so the Reply its line names is gone. A reply
 * the thread composer keeps for that comment, as it answers another, is drawn there with its text,
 * a line saying the comment is no longer listed, and Discard my draft. A reply put away by Cancel
 * reply over the docked composer's own draft (`usePhoneThread`'s `putAway`) is drawn so in the
 * docked composer, in place of the line that names that comment's Reply; it offers no Use it
 * here, since the docked composer's field holds a draft of its own.
 *
 * The numbered reasons - the moves this host cannot make, and the moves and forms the composer
 * census found above `ConversationTab` and in it that the rows do not make - are `REASONS` in
 * `draft-matrix.phone-thread.coverage.ts`, which holds each census item this file answers for to
 * a test above or to one of them.
 */
import { beforeAll, expect, type Mock, spyOn, test } from "bun:test";
import type { MutationKey } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";

import {
  alice,
  BODY,
  commentA,
  commentB,
  endAsAReload,
  goneIn,
  goneLead,
  HELD,
  type Held,
  inTheTab,
  keptLineNaming,
  listTurn,
  movedNote,
  NOTHING_READ,
  newQueryClient,
  noteIn,
  REFUSAL,
  REFUSED_NOTE,
  signedInAsReader,
  stubLists,
  tab,
} from "../../__tests__/draft-matrix-fixture";
import { sentRequest, stopSend } from "../../__tests__/stop-late-write";
import { api } from "../../api/client";
import { PHONE_WIDTH, stubViewport } from "../margin/margin-fixture";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import type { ReplyTarget } from "./composer-model";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

const HOST = "phone thread";

/** The phone thread composer's send name (`usePhoneThread`'s `composerKey`). */
const THREAD_KEY: MutationKey = ["conversation-composer", "CORE-1", "phone-thread"];

/** What a composer says of a stopped reply the reader took off its comment with the composer's
 *  own control. */
const MOVED_NOTE = movedNote("as a reply");

/** Comment A as the thread composer answers it, the reply a stopped send below was sent as. */
const replyToA: ReplyTarget = {
  actor: alice,
  excerpt: "Comment A",
  id: commentA.id,
  parentKind: "comment",
};

interface Page {
  readonly createComment: Mock<typeof api.createComment>;
  /** Reads the issue's comments again, as a refetch does, and answers them without Comment A:
   *  its card, and its Reply with it, leave the list. */
  readonly dropCommentA: () => Promise<void>;
  /** Reads the issue's comments again, as a refetch does, and answers Comment A deleted by its
   *  author: its card keeps its place, and Dispatch takes no reply to it. */
  readonly deleteCommentA: () => Promise<void>;
  /** Ends the page as a reload does and loads it again over the same tab, signed in as the same
   *  reader, with a new query client (`endAsAReload`, `signedInAsReader`). */
  readonly reload: () => Promise<void>;
  readonly restore: () => void;
  readonly user: UserEvent;
}

/** Waits for both comments to be listed. */
async function listed(): Promise<void> {
  await waitFor(() =>
    expect(document.querySelector('[data-turn="comment:comment-b"]')).not.toBe(null)
  );
}

/**
 * The Conversation of CORE-1 on a phone, over Comment A and Comment B. Every comment the page
 * writes is refused, so a send pressed here comes back refused; with `stopped`, a reply of `BODY`
 * to Comment A is sent through the held-send store under the thread composer's name before the
 * page is drawn and stopped once late, as the reader's Stop trying does. With `signedIn` the page
 * is signed in as the reader, as a reload row's is, so the tab keeps what it sends.
 */
async function openPhone(stopped: boolean, signedIn = false): Promise<Page> {
  const user = userEvent.setup();
  let queryClient = signedIn ? signedInAsReader(newQueryClient()) : newQueryClient();
  const reads = stubLists({ comments: [commentA, commentB] });
  const createComment = spyOn(api, "createComment");
  if (stopped) {
    await stopSend(queryClient, THREAD_KEY, sentRequest(BODY, replyToA), [createComment]);
  }
  createComment.mockRejectedValue(REFUSAL);
  const restoreViewport = stubViewport(PHONE_WIDTH);
  let view = render(tab(NOTHING_READ, true, queryClient));
  await listed();
  return {
    createComment,
    dropCommentA: async () => {
      reads.lists.comments = [commentB];
      await act(() => queryClient.invalidateQueries({ queryKey: ["comments", "CORE-1"] }));
      await waitFor(() =>
        expect(document.querySelector('[data-turn="comment:comment-a"]')).toBe(null)
      );
    },
    deleteCommentA: async () => {
      reads.lists.comments = [
        {
          ...commentA,
          body: "",
          deleted_at: "2026-09-20T00:05:00Z",
          deleted_by: alice,
        },
        commentB,
      ];
      await act(() => queryClient.invalidateQueries({ queryKey: ["comments", "CORE-1"] }));
      await waitFor(() => expect(listTurn(commentA.id).textContent).toContain("Deleted by alice"));
    },
    reload: async () => {
      endAsAReload(() => view.unmount());
      queryClient = signedInAsReader(newQueryClient());
      view = render(tab(NOTHING_READ, true, queryClient));
      await listed();
    },
    restore: () => {
      view.unmount();
      restoreViewport();
      createComment.mockRestore();
      reads.restore();
    },
    user,
  };
}

/** Presses a comment's Reply in the list and answers the thread view it opens. */
async function reply(user: UserEvent, id: string): Promise<HTMLElement> {
  await user.click(within(listTurn(id)).getByRole("button", { name: "Reply" }));
  return screen.findByRole("dialog", { name: "Thread" });
}

async function back(user: UserEvent, dialog: HTMLElement): Promise<void> {
  await user.click(within(dialog).getByRole("button", { name: "Back" }));
  await waitFor(() => expect(screen.queryByRole("dialog", { name: "Thread" })).toBe(null));
}

const threadField = (dialog: HTMLElement) =>
  within(dialog).getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" });

/** Which comment the thread view shows. */
function threadOf(dialog: HTMLElement): string {
  return dialog.querySelector("[data-turn]")?.getAttribute("data-turn") ?? "none";
}

/** Which note `place` shows about a send it holds. */
function note(place: HTMLElement): Held | "moved" | "none" {
  const held = noteIn(place);
  if (held !== "none") return held;
  return within(place).queryByText(MOVED_NOTE) === null ? "none" : "moved";
}

/** Whether a field on the page, shown or hidden, holds `text`. */
function inAField(text: string): boolean {
  return [...document.querySelectorAll("textarea")].some((box) => box.value.includes(text));
}

/** Opens Comment A's thread with its Reply and puts `BODY` in the thread composer, in the state
 *  `held` names; a stopped reply is the one `openPhone` seeded. Answers the thread view. */
async function holdInThreadA(page: Page, held: Held): Promise<HTMLElement> {
  const dialog = await reply(page.user, commentA.id);
  if (held === "stopped") {
    expect(note(dialog)).toBe("stopped");
    return dialog;
  }
  const field = threadField(dialog);
  await page.user.click(field);
  await page.user.keyboard("{Control>}a{/Control}");
  await page.user.paste(BODY);
  if (held === "refused") {
    await page.user.click(within(dialog).getByRole("button", { name: "Send" }));
    await within(dialog).findByText(REFUSED_NOTE);
  }
  return dialog;
}

// Back is the host's move and takes the thread composer off the screen: the reply is held for
// Comment A, and that comment's Reply brings it back with its note.
for (const held of HELD) {
  test(`${HOST} · Back, then back · ${held}`, async () => {
    const page = await openPhone(held === "stopped");
    try {
      await back(page.user, await holdInThreadA(page, held));
      const dialog = await reply(page.user, commentA.id);
      expect({ note: note(dialog), text: threadField(dialog).value }).toEqual({
        note: held === "typed" ? "none" : held,
        text: BODY,
      });
    } finally {
      page.restore();
    }
  });
}

// A reply typed, then Back, then Reply on another comment: that Reply is the host's move, which
// carries nothing, and the same thread composer is on screen at Comment B's address. The reply
// typed for Comment A is kept for A (`awayName` in `useKeptDrafts`), a line says so and names A
// (`keptAwayLine`), and A's Reply puts it back in the field in place of a new reply's prefill.
test(`${HOST} · Reply on another comment, then back · typed`, async () => {
  const page = await openPhone(false);
  try {
    await back(page.user, await holdInThreadA(page, "typed"));
    const other = await reply(page.user, commentB.id);
    const away = { kept: keptLineNaming("alice", commentA.body), thread: threadOf(other) };
    await back(page.user, other);
    const dialog = await reply(page.user, commentA.id);
    expect({ away, back: { text: threadField(dialog).value, thread: threadOf(dialog) } }).toEqual({
      away: { kept: true, thread: "comment:comment-b" },
      back: { text: BODY, thread: "comment:comment-a" },
    });
  } finally {
    page.restore();
  }
});

// A refused or stopped reply holds the thread composer on its comment: another comment's Reply
// opens the held comment's thread instead, with the draft and its note.
for (const held of ["refused", "stopped"] as const) {
  test(`${HOST} · Reply on another comment, then back · ${held}`, async () => {
    const page = await openPhone(held === "stopped");
    try {
      const dialogA = await holdInThreadA(page, held);
      // Counted from here, so the attempts a seeded stopped send made before it stopped are not.
      const sent = page.createComment.mock.calls.length;
      await back(page.user, dialogA);
      const dialog = await reply(page.user, commentB.id);
      expect({
        note: note(dialog),
        sent: page.createComment.mock.calls.length,
        text: threadField(dialog).value,
        thread: threadOf(dialog),
      }).toEqual({ note: held, sent, text: BODY, thread: "comment:comment-a" });
    } finally {
      page.restore();
    }
  });
}

/** What the reader's own move off the thread's reply leaves in the docked composer: the draft,
 *  a refusal with its Retry, and a stopped reply saying it may have been delivered as the reply. */
const ownMoveToDocked: Record<Held, { note: Held | "moved" | "none"; retry: boolean }> = {
  refused: { note: "refused", retry: true },
  stopped: { note: "moved", retry: true },
  typed: { note: "none", retry: false },
};

/** The docked composer as Back shows it, and what it holds. */
function docked(): { note: Held | "moved" | "none"; retry: boolean; text: string } {
  const form = screen.getByRole("form", { name: "Comment composer" });
  return {
    note: note(form),
    retry: within(form).queryByRole("button", { name: "Retry" }) !== null,
    text: within(form).getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" }).value,
  };
}

for (const held of HELD) {
  test(`${HOST} · Cancel reply · ${held}`, async () => {
    const page = await openPhone(held === "stopped");
    try {
      const dialog = await holdInThreadA(page, held);
      await page.user.click(within(dialog).getByRole("button", { name: "Cancel reply" }));
      const ended = within(dialog).queryAllByRole("textbox", { name: "Comment" }).length;
      await back(page.user, dialog);
      expect({ ...docked(), ended }).toEqual({ ...ownMoveToDocked[held], ended: 0, text: BODY });
    } finally {
      page.restore();
    }
  });
}

// Escape in the thread composer's field is the reader's own move as Cancel reply is. The thread
// view may close on the same key; where it stays, Back shows the docked composer.
for (const held of HELD) {
  test(`${HOST} · Escape in the field · ${held}`, async () => {
    const page = await openPhone(held === "stopped");
    try {
      const dialog = await holdInThreadA(page, held);
      await page.user.click(threadField(dialog));
      await page.user.keyboard("{Escape}");
      const open = screen.queryByRole("dialog", { name: "Thread" });
      if (open !== null) await back(page.user, open);
      expect(docked()).toEqual({ ...ownMoveToDocked[held], text: BODY });
    } finally {
      page.restore();
    }
  });
}

test(`${HOST} · Close, then Discard draft? · cannot make: the thread composer draws no Close`, async () => {
  const page = await openPhone(false);
  try {
    const dialog = await holdInThreadA(page, "typed");
    expect(within(dialog).queryAllByRole("button", { name: "Close" }).length).toBe(0);
  } finally {
    page.restore();
  }
});

// A reload ends the whole page and loads it again over the same tab (`endAsAReload`), with the
// reader signed in as before. Comment A's Reply, the way back to the reply, opens its thread with
// the reply the reader wrote there and its note.
for (const held of HELD) {
  test(`${HOST} · the page reloaded, then back · ${held}`, async () => {
    const page = await openPhone(held === "stopped", true);
    try {
      await holdInThreadA(page, held);
      await page.reload();
      const dialog = await reply(page.user, commentA.id);
      expect({ note: note(dialog), text: threadField(dialog).value }).toEqual({
        note: held === "typed" ? "none" : held,
        text: BODY,
      });
    } finally {
      page.restore();
    }
  });
}

// A reply typed in Comment A's thread, then Back and Comment B's Reply, the host's move, which
// keeps the reply for A (`awayName` in `useKeptDrafts`). A refetch then answers the comments
// without A, and A's Reply, the kept line's way back, goes with it. The thread composer, on B,
// draws the reply kept for A itself then, saying A is no longer listed, with Discard my draft,
// which forgets it in the page and in the tab. On a reply it offers no Use it here.
test(`${HOST} · the comment replied to leaves the list · typed`, async () => {
  const page = await openPhone(false, true);
  try {
    await back(page.user, await holdInThreadA(page, "typed"));
    const dialog = await reply(page.user, commentB.id);
    await page.dropCommentA();
    expect({ ...goneIn(dialog), kept: inTheTab(BODY), thread: threadOf(dialog) }).toEqual({
      discard: true,
      kept: true,
      line: `${goneLead("comment")}${BODY}`,
      pressReply: false,
      thread: "comment:comment-b",
      use: false,
    });
    await page.user.click(within(dialog).getByRole("button", { name: "Discard my draft" }));
    expect({ ...goneIn(dialog), kept: inTheTab(BODY), typed: inAField(BODY) }).toEqual({
      discard: false,
      kept: false,
      line: undefined,
      pressReply: false,
      typed: false,
      use: false,
    });
  } finally {
    page.restore();
  }
});

/** What the reader types in the docked composer before a row puts a thread reply away over it. */
const DOCKED_DRAFT = "Docked draft of the reader's own";

// Cancel reply in the thread over a docked composer that holds a draft of its own puts the reply
// away as it stands (`usePhoneThread`'s `putAway`), and the docked composer says so and names the
// comment's Reply as the way back. A refetch then answers the comments without Comment A, and
// that Reply goes. The docked composer draws the reply put away itself then, saying A is no
// longer listed, with Discard my draft, which drops it, a refused or stopped send with it, in the
// page and in the tab, and leaves the docked draft as it was. It offers no Use it here: the docked
// field holds a draft of its own.
for (const held of HELD) {
  test(`${HOST} · put away, then its comment leaves the list · ${held}`, async () => {
    const page = await openPhone(held === "stopped", true);
    try {
      const form = screen.getByRole("form", { name: "Comment composer" });
      const dockedField = within(form).getByRole<HTMLTextAreaElement>("textbox", {
        name: "Comment",
      });
      await page.user.click(dockedField);
      await page.user.paste(DOCKED_DRAFT);
      const dialog = await holdInThreadA(page, held);
      await page.user.click(within(dialog).getByRole("button", { name: "Cancel reply" }));
      await back(page.user, dialog);
      await page.dropCommentA();
      expect({ ...goneIn(form), docked: dockedField.value }).toEqual({
        discard: true,
        docked: DOCKED_DRAFT,
        line: `${goneLead("comment")}${BODY}`,
        pressReply: false,
        use: false,
      });
      await page.user.click(within(form).getByRole("button", { name: "Discard my draft" }));
      expect({
        ...goneIn(form),
        docked: dockedField.value,
        kept: inTheTab(BODY),
        keptLine: within(form)
          .queryAllByRole("status")
          .some((status) => /\bkept\b/.test(status.textContent ?? "")),
        typed: inAField(BODY),
      }).toEqual({
        discard: false,
        docked: DOCKED_DRAFT,
        kept: false,
        keptLine: false,
        line: undefined,
        pressReply: false,
        typed: false,
        use: false,
      });
    } finally {
      page.restore();
    }
  });
}

// Cancel reply in the thread over a docked composer that holds a draft of its own puts the reply
// away (`usePhoneThread`'s `putAway`), and the docked composer names Comment A's Reply as the way
// back. A refetch then answers A deleted by its author: A keeps its place, but Dispatch takes no
// reply to it, so no Reply goes on with the reply put away. The docked composer draws it itself
// then, saying A was deleted and giving no word of A, with Discard my draft, which drops it, a
// refused or stopped send with it, in the page and in the tab, and leaves the docked draft as it
// was.
for (const held of HELD) {
  test(`${HOST} · put away, then its comment is deleted · ${held}`, async () => {
    const page = await openPhone(held === "stopped", true);
    try {
      const form = screen.getByRole("form", { name: "Comment composer" });
      const dockedField = within(form).getByRole<HTMLTextAreaElement>("textbox", {
        name: "Comment",
      });
      await page.user.click(dockedField);
      await page.user.paste(DOCKED_DRAFT);
      const dialog = await holdInThreadA(page, held);
      await page.user.click(within(dialog).getByRole("button", { name: "Cancel reply" }));
      await back(page.user, dialog);
      await page.deleteCommentA();
      const deleted = { ...goneIn(form), docked: dockedField.value };
      const discard = within(form).queryByRole("button", { name: "Discard my draft" });
      if (discard !== null) await page.user.click(discard);
      expect({
        deleted,
        discarded: {
          ...goneIn(form),
          docked: dockedField.value,
          kept: inTheTab(BODY),
          typed: inAField(BODY),
        },
      }).toEqual({
        deleted: {
          discard: true,
          docked: DOCKED_DRAFT,
          line: `The comment by alice that your reply answered was deleted, so it takes no reply. Your reply is kept here:${BODY}`,
          pressReply: false,
          use: false,
        },
        discarded: {
          discard: false,
          docked: DOCKED_DRAFT,
          kept: false,
          line: undefined,
          pressReply: false,
          typed: false,
          use: false,
        },
      });
    } finally {
      page.restore();
    }
  });
}
