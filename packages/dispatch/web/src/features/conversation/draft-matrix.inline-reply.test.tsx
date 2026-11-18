/**
 * The draft matrix, inline reply row (INFRA-1503).
 *
 * Every composer host is held to one rule (packages/dispatch/AGENTS.md, "Who moved the
 * composer's address"): the reader's own move with the composer's own control takes the draft
 * with it, and "a move of the host's ... carries nothing: the text stays with the address it was
 * written for, and is there when the reader comes back to it." A refused or stopped send's note
 * stays at its own address. An inline reply has no other composer for its own controls to carry
 * the draft to, so its Cancel reply and Escape end the reply and keep the draft for the thread,
 * which the closed card says (`keptReply`). Each cell writes a draft in one state, typed and
 * never sent, refused by Dispatch, or stopped by the reader, makes one move, and asserts where
 * the text ends.
 *
 * Host: a comment thread card's own reply composer in the Conversation (`ThreadCard`'s inline
 * reply under `CommentTurn`), whose thread the Conversation's Expand thread and Collapse thread
 * open and close, and whose comment's Reply hands the reply to another composer.
 *
 * A host move that takes the composer off the screen is checked on return alone, where the field
 * holds the draft with its refusal or stopped note; a host move that keeps the same composer on
 * screen at another address is checked on return too and, while away, for a line on screen that
 * says the earlier text is kept and names its comment's author or words.
 *
 * | Move                                            | typed  | refused | stopped |
 * | ----------------------------------------------- | ------ | ------- | ------- |
 * | Back, then back (phone thread view) (off)       | cell   | cell    | cell    |
 * | Reply on its comment, then back (off)           | cell   | cell    | cell    |
 * | Reply on its comment, then back (phone) (off)   | cell   | cell    | cell    |
 * | Host moves it over a refusal (elsewhere)        | no (1) | no (1)  | no (1)  |
 * | Thread collapsed by the host, then opened (off) | cell   | cell    | cell    |
 * | Cancel reply                                    | cell   | cell    | cell    |
 * | Escape in the field                             | cell   | cell    | cell    |
 * | Close, then Discard draft?                      | no (2) | no (2)  | no (2)  |
 * | Agents pick                                     | no (3) | no (3)  | no (3)  |
 * | Typed while elsewhere, then unmounted           | -      | no (1)  | -       |
 * | The card leaves the list, then comes back       | no (4) | no (4)  | no (4)  |
 * | The Conversation's reads fail or wait           | no (5) | no (5)  | no (5)  |
 * | Issue left, or another tab, then back           | no (6) | no (6)  | no (6)  |
 * | The thread's replies ordered the other way      | no (7) | no (7)  | no (7)  |
 *
 * (off) marks a host move that takes the card's reply off the screen: Back hides the thread
 * view, Collapse thread unmounts the reply, and the comment's Reply hides it while another
 * composer answers the comment with a prefill of its own, not the card's draft: the docked
 * composer above the phone layout, and on a phone the thread view's own (`usePhoneThread`). Every
 * host move of this row is one, so none needs a line while away.
 *
 * The moves this host cannot make:
 * 1. The card's reply always answers its own thread's root on its own issue, so no move of the
 *    host's changes its address, and it is never elsewhere.
 * 2. An inline reply is compact, so it draws no Close (asserted absent below).
 * 3. The issue picker is an Agents row's (`AgentMessageComposer`), and a thread card draws no
 *    Agents row's composer.
 *
 * The moves the composer census found that the rows above do not make:
 * 4. A card is drawn per item of the Conversation's list, keyed by it (`CommentTurn`): the list's
 *    All, Open and Closed filter, and a thread the list no longer holds, unmount the card and its
 *    reply as Collapse thread does, and the card drawn when it is listed again opens on the same
 *    thread's send name.
 * 5. The Conversation draws a loading line or an error in place of the list while its reads are
 *    out or failed (`loading`, `failed`), which unmounts the reply as Collapse thread does.
 * 6. Another issue's address draws the issue page afresh (`IssueDetail` keyed by `route.key`),
 *    unmounting the card and its reply as Collapse thread does, and another tab hides the
 *    Conversation's panel with the reply mounted (`draft-matrix.issue-page.test.tsx` makes both
 *    moves for the docked and phone thread composers).
 * 7. Which end of the thread the reply sits at (`composerFirst`, from `useThreadOrder`) is read
 *    when the card is drawn and again only when who is signed in changes (`useUserPreference`),
 *    so it never moves a reply that is on screen.
 *
 * The forms the composer census found, where the layout draws, hides or moves this composer
 * otherwise than the rows above, which the rows do not make:
 * 8. On a phone the comment's Expand thread opens the full-screen thread view in place of opening
 *    the card in the list (`toggleThread`'s `isPhone`), and its Reply answers in the view's own
 *    composer (`onReply`, and Reply's `!isPhone`), so the first row and the phone Reply row are the
 *    phone's forms of the Collapse thread and Reply rows. A comment link opens the view as Expand
 *    thread does (`ConversationTab`'s focus effect), and a decision landing on the comment closes
 *    it as Back does (`phone.hide`).
 * 9. The view's card is drawn only while a thread is open (`phoneThread`) and keyed by its thread
 *    (`key={phoneThread.id}`): another thread is opened after Back, and widening the page past
 *    the phone layout closes the view as Back does unless a send from it is out
 *    (`usePhoneThread`'s `outlived`), the first row's move either way.
 * 10. The read mark's ref on the newest card (`register`) moves to the view's copy of it while a
 *    thread is open; it marks the issue read and moves no reply. The margin's thread reply is the
 *    margin file's, and the app shell's forms are the sign-in file's.
 *
 * `draft-matrix.inline-reply.coverage.ts` holds each census item this file answers for to a test
 * above or to a numbered reason, and is the source this header is the prose of.
 */
import { expect, type Mock, spyOn, test } from "bun:test";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";

import {
  alice,
  BODY,
  commentA,
  expectedBack,
  HELD,
  type Held,
  keptInSight,
  listTurn,
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
import { stubMatchMedia } from "../margin/margin-fixture";
import { threadReplySendKey } from "./usePhoneThread";

const HOST = "inline reply";

interface Page {
  readonly createComment: Mock<typeof api.createComment>;
  readonly restore: () => void;
  readonly user: UserEvent;
}

/**
 * The Conversation of CORE-1 over Comment A, at a phone width or above it. Every comment the
 * page writes is refused, so a send pressed here comes back refused; with `stopped`, the card's
 * reply of `BODY` to Comment A is sent through the held-send store under the card's send name
 * before the page is drawn and stopped once late, as the reader's Stop trying does.
 */
async function openConversation(stopped: boolean, phone = false): Promise<Page> {
  const user = userEvent.setup();
  const queryClient = newQueryClient();
  const reads = stubLists({ comments: [commentA] });
  const createComment = spyOn(api, "createComment");
  if (stopped) {
    await stopSend(
      queryClient,
      threadReplySendKey(["conversation-composer", "CORE-1"], commentA.id),
      sentRequest(BODY, { actor: alice, excerpt: "", id: commentA.id, parentKind: "comment" }),
      [createComment]
    );
  }
  createComment.mockRejectedValue(REFUSAL);
  const restoreViewport = stubMatchMedia(phone);
  const view = render(tab(NOTHING_READ, true, queryClient));
  await waitFor(() =>
    expect(document.querySelector('[data-turn="comment:comment-a"]')).not.toBe(null)
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

/** Comment A's turn in the list, outside any thread view. */
const turn = () => listTurn(commentA.id);

const replyField = (place: HTMLElement) =>
  within(place).getByRole<HTMLTextAreaElement>("textbox", { name: "Reply" });

/** Opens Comment A's thread with the host's Expand thread and answers where its card's reply
 *  shows: the turn above the phone layout, the thread view on a phone. */
async function expand(user: UserEvent, phone: boolean): Promise<HTMLElement> {
  await user.click(within(turn()).getByRole("button", { name: "Expand thread" }));
  return phone ? screen.findByRole("dialog", { name: "Thread" }) : turn();
}

/** Opens Comment A's thread and puts `BODY` in its card's reply, in the state `held` names; a
 *  stopped reply is the one `openConversation` seeded. Answers where the reply shows. */
async function holdInCard(page: Page, held: Held, phone = false): Promise<HTMLElement> {
  const place = await expand(page.user, phone);
  if (held === "stopped") {
    expect(noteIn(place)).toBe("stopped");
    return place;
  }
  const field = replyField(place);
  await page.user.click(field);
  await page.user.paste(BODY);
  if (held === "refused") {
    const form = field.closest("form");
    if (form === null) throw new Error("expected the card's reply form");
    await page.user.click(within(form).getByRole("button", { name: "Send" }));
    await within(place).findByText(REFUSED_NOTE);
  }
  return place;
}

/** What the reply composer holds where it shows again. */
function heldIn(place: HTMLElement): { note: Held | "none"; text: string } {
  return { note: noteIn(place), text: replyField(place).value };
}

// On a phone the card's reply lives in the thread view, which Back takes off the page. Back is the
// host's move: the reply is held for the thread, and Expand thread brings it back with its note.
for (const held of HELD) {
  test(`${HOST} · Back, then back (phone thread view) · ${held}`, async () => {
    const page = await openConversation(held === "stopped", true);
    try {
      const dialog = await holdInCard(page, held, true);
      await page.user.click(within(dialog).getByRole("button", { name: "Back" }));
      await waitFor(() => expect(screen.queryByRole("dialog", { name: "Thread" })).toBe(null));
      expect(heldIn(await expand(page.user, true))).toEqual(expectedBack(held));
    } finally {
      page.restore();
    }
  });
}

// Above the phone layout a comment's Reply answers in the docked composer, which hides the card's
// own reply while it does (`hideReplyComposer`). That is the host's move, and the card's reply is
// off the screen meanwhile: it waits for the docked one's Cancel reply, which brings it back.
for (const held of HELD) {
  test(`${HOST} · Reply on its comment, then back · ${held}`, async () => {
    const page = await openConversation(held === "stopped");
    try {
      await holdInCard(page, held);
      await page.user.click(within(turn()).getByRole("button", { name: "Reply" }));
      const docked = screen.getByRole("textbox", { name: "Comment" }).closest("form");
      if (docked === null) throw new Error("expected the docked composer");
      // The move this row is about: the card's reply hidden, the docked composer on the comment.
      const away = {
        cardReply: within(turn()).queryAllByRole("textbox", { name: "Reply" }).length,
        dockedQuotes: within(docked).queryByText(/Comment A/) !== null,
      };
      await page.user.click(within(docked).getByRole("button", { name: "Cancel reply" }));
      expect({ away, back: heldIn(turn()) }).toEqual({
        away: { cardReply: 0, dockedQuotes: true },
        back: expectedBack(held),
      });
    } finally {
      page.restore();
    }
  });
}

// On a phone the comment's Reply answers in the thread view's own composer (`usePhoneThread`'s
// `beginReply`) rather than the docked one, and that composer hides the card's reply in the view
// while it answers the thread (`hideReplyComposer`, from `phoneReplyTargetsThread`). That is the
// host's move, as above the phone layout: the card's reply waits, mounted, for the thread
// composer's Cancel reply, which brings it back with its note.
for (const held of HELD) {
  test(`${HOST} · phone · Reply on its comment, then back (phone thread view) · ${held}`, async () => {
    const page = await openConversation(held === "stopped", true);
    try {
      const dialog = await holdInCard(page, held, true);
      await page.user.click(within(dialog).getByRole("button", { name: "Reply" }));
      // The move this row is about: the card's reply hidden, the thread composer answering.
      const away = {
        cardReply: within(dialog).queryAllByRole("textbox", { name: "Reply" }).length,
        threadComposer: within(dialog).queryAllByRole("form", { name: "Comment composer" }).length,
      };
      await page.user.click(within(dialog).getByRole("button", { name: "Cancel reply" }));
      expect({ away, back: heldIn(dialog) }).toEqual({
        away: { cardReply: 0, threadComposer: 1 },
        back: expectedBack(held),
      });
    } finally {
      page.restore();
    }
  });
}

for (const held of HELD) {
  test(`${HOST} · thread collapsed by the host, then opened · ${held}`, async () => {
    const page = await openConversation(held === "stopped");
    try {
      await holdInCard(page, held);
      await page.user.click(within(turn()).getByRole("button", { name: "Collapse thread" }));
      expect(heldIn(await expand(page.user, false))).toEqual(expectedBack(held));
    } finally {
      page.restore();
    }
  });
}

// Cancel reply and Escape in the field are the reader's own, and an inline reply has no other
// composer to take its draft to: the reply ends, the draft is kept for the thread with its note,
// the closed card says so, and Expand thread brings it back.
for (const held of HELD) {
  test(`${HOST} · Cancel reply · ${held}`, async () => {
    const page = await openConversation(held === "stopped");
    try {
      const place = await holdInCard(page, held);
      await page.user.click(within(place).getByRole("button", { name: "Cancel reply" }));
      const kept = keptInSight(BODY);
      const back = heldIn(await expand(page.user, false));
      expect({ back, kept }).toEqual({ back: expectedBack(held), kept: true });
    } finally {
      page.restore();
    }
  });
}

for (const held of HELD) {
  test(`${HOST} · Escape in the field · ${held}`, async () => {
    const page = await openConversation(held === "stopped");
    try {
      const place = await holdInCard(page, held);
      await page.user.click(replyField(place));
      await page.user.keyboard("{Escape}");
      const kept = keptInSight(BODY);
      const back = heldIn(await expand(page.user, false));
      expect({ back, kept }).toEqual({ back: expectedBack(held), kept: true });
    } finally {
      page.restore();
    }
  });
}

test(`${HOST} · Close, then Discard draft? · cannot make: the card's reply draws no Close`, async () => {
  const page = await openConversation(false);
  try {
    const place = await holdInCard(page, "typed");
    const form = replyField(place).closest("form");
    if (form === null) throw new Error("expected the card's reply form");
    expect(within(form).queryAllByRole("button", { name: "Close" }).length).toBe(0);
  } finally {
    page.restore();
  }
});
