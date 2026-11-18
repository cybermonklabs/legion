/**
 * The draft matrix, phone thread row (INFRA-1503).
 *
 * Every composer host is held to one rule (packages/dispatch/AGENTS.md, "Who moved the
 * composer's address"): the reader's own move with the composer's own control takes the draft
 * with it, and "a move of the host's ... carries nothing: the text stays with the address it was
 * written for, and is there when the reader comes back to it." A refused or stopped send's note
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
 *
 * (off) marks a host move that takes the composer off the screen: Back hides the thread view,
 * and the docked composer it uncovers answers the issue, not the comment. (on) marks one that
 * keeps the same thread composer on screen at another comment's address. A refused or stopped
 * draft never leaves its field there, since another comment's Reply opens the held comment's
 * thread instead, so only the typed draft needs the line.
 *
 * The moves this host cannot make:
 * 1. Every Reply the thread composer answers is redirected onto the comment its refusal answers
 *    (`redirectedReplyTarget`), so the host never moves it while a refusal is held.
 * 2. The thread view draws its comment forced open with no Collapse thread (`forceExpanded`), so
 *    Back, the first row, is its one way out.
 * 3. The thread composer has no anchor, so it draws no Close (asserted absent below).
 * 4. The issue picker is an Agents row's (`AgentMessageComposer`), and the thread view draws no
 *    Agents row's composer.
 *
 * The moves the composer census found above `ConversationTab`, and in it:
 * 5. `draft-matrix.issue-page.test.tsx` makes these cells, with the issue page drawn around the
 *    Conversation: another issue's address draws the page afresh (`IssueDetail` keyed by
 *    `route.key`), and another tab hides the Conversation's panel and keeps it drawn. The
 *    composer's `owner` and send name follow `issueKey`, which only that remount changes.
 * 6. Another comment's thread opened with Expand thread, while the composer answers Comment A,
 *    hides the composer (`hidden={!phoneReplyTargetsThread}`) and keeps it mounted at A's address,
 *    so nothing it holds moves; A's Reply shows it again, as the first row's return does.
 * 7. The Conversation draws a loading line or an error in place of the list while its reads are
 *    out or failed (`loading`, `failed`), which unmounts the thread composer as leaving the issue
 *    does, and the composer drawn when they land again is the one the issue-left row draws on
 *    return.
 *
 * The forms the composer census found, where the layout draws, hides or moves this composer
 * otherwise than the rows above, which the rows do not make:
 * 8. A comment link opens the thread view as it lands (`ConversationTab`'s focus effect), as the
 *    comment's Reply opens it here, and a decision landing on the comment closes the view as Back
 *    does (`phone.hide`), the first row's move; the composer stays mounted while its reply is set
 *    (`phone.replyTo`).
 * 9. Widening the page past the phone layout closes the view as Back does, unless a send from it
 *    is out, which keeps the view open at any width until Back (`usePhoneThread`'s `outlived`);
 *    the composer stays mounted while its reply is set, whatever the width. The Jump to latest
 *    pill and the read mark's ref on the newest card move no composer, and the app shell's forms
 *    are the sign-in file's.
 *
 * `draft-matrix.phone-thread.coverage.ts` holds each census item this file answers for to a test
 * above or to a numbered reason, and is the source this header is the prose of.
 */
import { expect, type Mock, spyOn, test } from "bun:test";
import type { MutationKey } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";

import {
  alice,
  BODY,
  commentA,
  commentB,
  HELD,
  type Held,
  keptLineNaming,
  listTurn,
  movedNote,
  NOTHING_READ,
  newQueryClient,
  noteIn,
  REFUSAL,
  REFUSED_NOTE,
  stubLists,
  tab,
} from "../../__tests__/draft-matrix-fixture";
import { sentRequest, stopSend } from "../../__tests__/stop-late-write";
import { api } from "../../api/client";
import { PHONE_WIDTH, stubViewport } from "../margin/margin-fixture";
import type { ReplyTarget } from "./composer-model";

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
  readonly restore: () => void;
  readonly user: UserEvent;
}

/**
 * The Conversation of CORE-1 on a phone, over Comment A and Comment B. Every comment the page
 * writes is refused, so a send pressed here comes back refused; with `stopped`, a reply of `BODY`
 * to Comment A is sent through the held-send store under the thread composer's name before the
 * page is drawn and stopped once late, as the reader's Stop trying does.
 */
async function openPhone(stopped: boolean): Promise<Page> {
  const user = userEvent.setup();
  const queryClient = newQueryClient();
  const reads = stubLists({ comments: [commentA, commentB] });
  const createComment = spyOn(api, "createComment");
  if (stopped) {
    await stopSend(queryClient, THREAD_KEY, sentRequest(BODY, replyToA), [createComment]);
  }
  createComment.mockRejectedValue(REFUSAL);
  const restoreViewport = stubViewport(PHONE_WIDTH);
  const view = render(tab(NOTHING_READ, true, queryClient));
  await waitFor(() =>
    expect(document.querySelector('[data-turn="comment:comment-b"]')).not.toBe(null)
  );
  return {
    createComment,
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
