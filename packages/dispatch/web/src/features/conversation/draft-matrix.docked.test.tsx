/**
 * The draft matrix, docked composer row (INFRA-1503).
 *
 * Every composer host is held to one rule (packages/dispatch/AGENTS.md, "Who moved the
 * composer's address"): the reader's own move with the composer's own control takes the draft
 * with it, and "a move of the host's ... carries nothing: the text stays with the address it was
 * written for, and is there when the reader comes back to it." A refused or stopped send's note
 * stays at its own address. Each cell below writes a draft in one state - typed and never sent,
 * refused by Dispatch, or stopped by the reader - makes one move, and asserts where the text
 * ends.
 *
 * Host: the Conversation's docked composer, one box for the issue whose reply a message's Reply
 * moves, and above the phone layout a comment's Reply too (`ConversationTab`'s `beginReply`).
 *
 * A host move that takes the composer off the screen is checked on return alone, where the field
 * holds the draft with its refusal or stopped note; a host move that keeps the same composer on
 * screen at another address is checked on return too and, while away, for a line on screen that
 * says the earlier text is kept and names its message's author or words.
 *
 * | Move                                     | typed  | refused | stopped |
 * | ---------------------------------------- | ------ | ------- | ------- |
 * | Back, then back                          | no (1) | no (1)  | no (1)  |
 * | Reply on another message, then back (on) | cell   | cell    | cell    |
 * | Host moves it over a refusal (elsewhere) | no (2) | cell    | cell    |
 * | Thread collapsed by the host             | no (3) | no (3)  | no (3)  |
 * | Cancel reply                             | cell   | cell    | cell    |
 * | Escape over a reply                      | cell   | cell    | cell    |
 * | Escape over a comment, then Discard      | cell   | cell    | cell    |
 * | Close, then Discard draft?               | no (4) | no (4)  | no (4)  |
 * | Agents pick                              | no (5) | no (5)  | no (5)  |
 * | Typed while elsewhere, then unmounted    | -      | cell    | -       |
 * | Issue left, then back (off)              | (6)    | (6)     | (6)     |
 * | Another tab, then back (hidden)          | (6)    | (6)     | (6)     |
 * | The Conversation's reads fail or wait    | no (7) | no (7)  | no (7)  |
 *
 * (on) marks a host move that keeps the same composer on screen at another address. A refused or
 * stopped draft never leaves its field there (another message's Reply is redirected onto it, and
 * the elsewhere row keeps it in the field with Send saying where it was written), so only the
 * typed draft needs the line.
 *
 * On a phone a comment's Reply answers in the comment's full-screen thread, by the thread view's
 * own composer (`usePhoneThread`), rather than moving this one, which changes what that move does
 * here; it is a row of its own, checked on return for the draft on the issue with its note:
 *
 * | Phone move                              | typed | refused | stopped |
 * | --------------------------------------- | ----- | ------- | ------- |
 * | A comment's Reply, then Back (hidden)   | cell  | cell    | cell    |
 *
 * (hidden) marks a host move that keeps the composer mounted but out of sight.
 *
 * The moves this host cannot make:
 * 1. Above the phone layout no thread view covers the docked composer; on a phone a comment's
 *    Reply covers it with one, and Back uncovers it, the phone row.
 * 2. Elsewhere needs a held refusal; the typed version is the unmount row.
 * 3. The docked composer sits outside every thread card, so no Collapse thread moves it.
 * 4. The docked composer has no anchor, so it draws no Close (asserted absent below).
 * 5. The issue picker is an Agents row's (`AgentMessageComposer`), and the Conversation draws no
 *    Agents row's composer.
 *
 * The moves the composer census found above `ConversationTab`, and in it:
 * 6. `draft-matrix.issue-page.test.tsx` makes these cells, with the issue page drawn around the
 *    Conversation: another issue's address draws the page afresh (`IssueDetail` keyed by
 *    `route.key`), and another tab hides the Conversation's panel and keeps it drawn. The
 *    composer's `owner` and send name follow `issueKey`, which only that remount changes.
 * 7. The Conversation draws a loading line or an error in place of the list while its reads are
 *    out or failed (`loading`, `failed`), which unmounts the docked composer as leaving the issue
 *    does, and the composer drawn when they land again is the one the issue-left row draws on
 *    return.
 *
 * The forms the composer census found, where the layout draws, hides or moves this composer
 * otherwise than the rows above, which the rows do not make:
 * 8. On a phone a comment link opens the comment's thread view as it lands (`ConversationTab`'s
 *    focus effect), which covers this composer as the phone row's Reply does, and a decision
 *    landing on the comment closes the view as Back does (`phone.hide`). A message's Reply moves
 *    this composer at every width, as the rows above do.
 * 9. Widening the page past the phone layout closes the thread view as Back does, unless a send
 *    from it is out, which keeps it open until Back (`usePhoneThread`'s `outlived`); this
 *    composer stays mounted at the issue throughout. The Jump to latest pill, which waits under a
 *    thread view for Back, and the read mark's ref on the newest card (`register`), which moves to
 *    the view's copy of it, move no composer. The app shell's forms are the sign-in file's.
 *
 * `draft-matrix.docked.coverage.ts` holds each census item this file answers for to a test above
 * or to a numbered reason, and is the source this header is the prose of.
 */
import { expect, type Mock, spyOn, test } from "bun:test";
import type { MutationKey } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";

import {
  alice,
  BODY,
  commentA,
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
import type { Actor, Message } from "../../api/types";
import { PHONE_WIDTH, stubViewport } from "../margin/margin-fixture";
import type { ReplyTarget } from "./composer-model";
import type { SentRequest } from "./send-request";

const HOST = "docked composer";

/** The docked composer's send name (`ConversationTab`'s `sendKey`). */
const DOCKED_KEY: MutationKey = ["conversation-composer", "CORE-1"];

/** What the composer says of a stopped reply the reader took off its message with the
 *  composer's own control. */
const MOVED_NOTE = movedNote("as a reply");

/** What Send says while the host has the composer away from the issue its refused comment is for. */
const WRITTEN_FOR_ISSUE =
  "This message was written for CORE-1. Go back to it to send it, or discard it.";

function message(id: string, body: string, createdAt: string, author: Actor): Message {
  return {
    author,
    body,
    broadcast_id: null,
    created_at: createdAt,
    deliveries: [],
    id,
    in_reply_to: null,
    issue_key: "CORE-1",
    reply_block: null,
    target: null,
  };
}

/** Two messages by two people, so a line naming the first message's author names it alone. */
const messageA = message("message-1", "Message A", "2026-09-09T00:00:00Z", alice);
const messageB = message("message-2", "Message B", "2026-09-09T00:05:00Z", {
  id: "bob",
  kind: "user",
});

/** Message A as the docked composer answers it, the reply a stopped send below was sent as. */
const replyToA: ReplyTarget = {
  actor: alice,
  excerpt: "Message A",
  id: messageA.id,
  parentKind: "message",
};

interface Page {
  readonly createComment: Mock<typeof api.createComment>;
  readonly createMessage: Mock<typeof api.createMessage>;
  /** Draws the Conversation again over the same client, as a reader coming back to the issue. */
  readonly mount: () => void;
  readonly restore: () => void;
  /** Takes the Conversation off the page, as a reader leaving the issue does. */
  readonly unmount: () => void;
  readonly user: UserEvent;
}

/**
 * The Conversation of CORE-1 over Message A and Message B, above the phone layout or, with
 * `phone`, on a phone, where Comment A is listed too. Every write the page makes is refused, so a
 * send pressed here comes back refused; `stopped`, when given, is sent through the held-send store
 * under the docked composer's name before the page is drawn and stopped once late, as the
 * reader's Stop trying does, so the composer mounts showing it.
 */
async function openDocked(stopped?: SentRequest, phone = false): Promise<Page> {
  const user = userEvent.setup();
  const queryClient = newQueryClient();
  const reads = stubLists({
    comments: phone ? [commentA] : [],
    messages: [
      { message: messageA, replies: [] },
      { message: messageB, replies: [] },
    ],
  });
  const createComment = spyOn(api, "createComment");
  const createMessage = spyOn(api, "createMessage");
  if (stopped !== undefined) {
    await stopSend(queryClient, DOCKED_KEY, stopped, [createComment, createMessage]);
  }
  createComment.mockRejectedValue(REFUSAL);
  createMessage.mockRejectedValue(REFUSAL);
  // Above the phone layout the page keeps the test DOM's own viewport, as it always has.
  const restoreViewport = phone ? stubViewport(PHONE_WIDTH) : () => {};
  let unmount = () => {};
  const mount = () => {
    const view = render(tab(NOTHING_READ, true, queryClient));
    unmount = () => view.unmount();
  };
  mount();
  await listed();
  return {
    createComment,
    createMessage,
    mount,
    restore: () => {
      unmount();
      restoreViewport();
      createComment.mockRestore();
      createMessage.mockRestore();
      reads.restore();
    },
    unmount: () => unmount(),
    user,
  };
}

/** Waits for both messages to be listed. */
async function listed(): Promise<void> {
  await waitFor(() =>
    expect(document.querySelector('[data-turn="message:message-2"]')).not.toBe(null)
  );
}

function turn(id: string): HTMLElement {
  const element = document.querySelector<HTMLElement>(`[data-turn="message:${id}"]`);
  if (element === null) throw new Error(`expected ${id}'s turn`);
  return element;
}

const composer = () => screen.getByRole("form", { name: "Comment composer" });
const field = () =>
  within(composer()).getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" });

async function reply(user: UserEvent, id: string): Promise<void> {
  await user.click(within(turn(id)).getByRole("button", { name: "Reply" }));
}

/** Writes `body` over whatever the field holds, as a reader replacing a prefill does. */
async function write(user: UserEvent, body: string): Promise<void> {
  await user.click(field());
  await user.keyboard("{Control>}a{/Control}");
  await user.paste(body);
}

async function sendRefused(user: UserEvent): Promise<void> {
  await user.click(within(composer()).getByRole("button", { name: "Send" }));
  await within(composer()).findByText(REFUSED_NOTE);
}

/** Which message the composer quotes as the one it answers, if any. */
function quoted(): "A" | "B" | "none" {
  const shown = within(composer());
  if (shown.queryByText(/Message A/) !== null) return "A";
  return shown.queryByText(/Message B/) === null ? "none" : "B";
}

/** Which note the composer shows about a send it holds. */
function note(): Held | "moved" | "none" {
  const held = noteIn(composer());
  if (held !== "none") return held;
  return within(composer()).queryByText(MOVED_NOTE) === null ? "none" : "moved";
}

function retryOffered(): boolean {
  return within(composer()).queryByRole("button", { name: "Retry" }) !== null;
}

/** Puts `BODY` in the docked composer as a reply to Message A, in the state `held` names. A
 *  stopped reply is the one `openDocked` seeded, which the composer mounted with. */
async function holdAtReplyA(page: Page, held: Held): Promise<void> {
  if (held === "stopped") {
    expect(note()).toBe("stopped");
    return;
  }
  await reply(page.user, messageA.id);
  await write(page.user, BODY);
  if (held === "refused") await sendRefused(page.user);
}

/** Puts `BODY` in the docked composer as a comment on the issue, in the state `held` names. */
async function holdAtIssue(page: Page, held: Held): Promise<void> {
  if (held === "stopped") {
    expect(note()).toBe("stopped");
    return;
  }
  await write(page.user, BODY);
  if (held === "refused") await sendRefused(page.user);
}

// At a desktop width a message's Reply is the host's move, which carries nothing, and the same
// composer stays on screen at Message B's address. The reply typed for Message A is kept for A
// (`awayName` in `useKeptDrafts`), a line says so and names A (`keptAwayLine`), and A's Reply
// puts it back in the field in place of a new reply's prefill.
test(`${HOST} · Reply on another message, then back · typed`, async () => {
  const page = await openDocked();
  try {
    await holdAtReplyA(page, "typed");
    await reply(page.user, messageB.id);
    const away = { kept: keptLineNaming("alice", messageA.body), quoted: quoted() };
    await reply(page.user, messageA.id);
    const back = { quoted: quoted(), text: field().value };
    expect({ away, back }).toEqual({
      away: { kept: true, quoted: "B" },
      back: { quoted: "A", text: BODY },
    });
  } finally {
    page.restore();
  }
});

// A refused or stopped reply holds the composer on its message: another message's Reply is
// redirected onto it (`redirectedReplyTarget`), so the draft and its note never leave.
for (const held of ["refused", "stopped"] as const) {
  test(`${HOST} · Reply on another message, then back · ${held}`, async () => {
    const page = await openDocked(held === "stopped" ? sentRequest(BODY, replyToA) : undefined);
    try {
      await holdAtReplyA(page, held);
      await reply(page.user, messageB.id);
      const away = { note: note(), quoted: quoted(), text: field().value };
      await reply(page.user, messageA.id);
      const back = { note: note(), quoted: quoted(), text: field().value };
      expect({ away, back }).toEqual({
        away: { note: held, quoted: "A", text: BODY },
        back: { note: held, quoted: "A", text: BODY },
      });
    } finally {
      page.restore();
    }
  });
}

// A comment refused or stopped on the issue, and then a message's Reply: the host has moved the
// composer off the issue, so Send says where the draft was written and offers no Retry there.
// Cancel reply takes the composer back to the issue, where the note and its Retry are again.
for (const held of ["refused", "stopped"] as const) {
  test(`${HOST} · host moves it over a refusal (elsewhere), then back · ${held}`, async () => {
    const page = await openDocked(held === "stopped" ? sentRequest(BODY, null) : undefined);
    try {
      await holdAtIssue(page, held);
      await reply(page.user, messageB.id);
      const away = {
        elsewhere: within(composer()).queryByText(WRITTEN_FOR_ISSUE) !== null,
        quoted: quoted(),
        retry: retryOffered(),
        text: field().value,
      };
      await page.user.click(within(composer()).getByRole("button", { name: "Cancel reply" }));
      const back = { note: note(), quoted: quoted(), retry: retryOffered(), text: field().value };
      expect({ away, back }).toEqual({
        away: { elsewhere: true, quoted: "B", retry: false, text: BODY },
        back: { note: held, quoted: "none", retry: true, text: BODY },
      });
    } finally {
      page.restore();
    }
  });
}

// While the host has the composer away from the issue its refused comment is for, nothing typed
// is written into the refusal's draft: what the reader types for Message B is that reply's, and
// it is kept for it as the composer unmounts (`keepAway` in `useKeptDrafts`). The unmount takes
// the composer off the screen, so the cell checks the return alone: coming back to Message B
// finds it, and the refusal comes back with the issue as well.
test(`${HOST} · typed while a refusal is held elsewhere, then the composer unmounts · refused`, async () => {
  const page = await openDocked();
  const typed = "Reply meant for Message B";
  try {
    await holdAtIssue(page, "refused");
    await reply(page.user, messageB.id);
    await write(page.user, typed);
    page.unmount();
    page.mount();
    await listed();
    await reply(page.user, messageB.id);
    const back = screen
      .getAllByRole<HTMLTextAreaElement>("textbox")
      .some((box) => box.value.includes(typed));
    expect({ back, refusal: note() }).toEqual({ back: true, refusal: "refused" });
  } finally {
    page.restore();
  }
});

/** What the reader's own move off a reply leaves: the draft goes with it to the issue, a refusal
 *  with its Retry, and a stopped reply saying it may have been delivered as the reply. */
const ownMoveOffReply: Record<Held, { note: Held | "moved" | "none"; retry: boolean }> = {
  refused: { note: "refused", retry: true },
  stopped: { note: "moved", retry: true },
  typed: { note: "none", retry: false },
};

for (const held of HELD) {
  test(`${HOST} · Cancel reply · ${held}`, async () => {
    const page = await openDocked(held === "stopped" ? sentRequest(BODY, replyToA) : undefined);
    try {
      await holdAtReplyA(page, held);
      const sent = page.createMessage.mock.calls.length;
      await page.user.click(within(composer()).getByRole("button", { name: "Cancel reply" }));
      expect({
        note: note(),
        quoted: quoted(),
        retry: retryOffered(),
        sent: page.createMessage.mock.calls.length,
        text: field().value,
      }).toEqual({ ...ownMoveOffReply[held], quoted: "none", sent, text: BODY });
    } finally {
      page.restore();
    }
  });
}

// Escape over a reply ends the reply as Cancel reply does, so the draft goes along the same way.
for (const held of HELD) {
  test(`${HOST} · Escape over a reply · ${held}`, async () => {
    const page = await openDocked(held === "stopped" ? sentRequest(BODY, replyToA) : undefined);
    try {
      await holdAtReplyA(page, held);
      await page.user.click(field());
      await page.user.keyboard("{Escape}");
      expect({
        note: note(),
        quoted: quoted(),
        retry: retryOffered(),
        text: field().value,
      }).toEqual({ ...ownMoveOffReply[held], quoted: "none", text: BODY });
    } finally {
      page.restore();
    }
  });
}

// Escape over a comment with no reply to end asks first, and only Discard drops the draft, its
// note with it.
for (const held of HELD) {
  test(`${HOST} · Escape over a comment, then Discard · ${held}`, async () => {
    const page = await openDocked(held === "stopped" ? sentRequest(BODY, null) : undefined);
    try {
      await holdAtIssue(page, held);
      await page.user.click(field());
      await page.user.keyboard("{Escape}");
      const asked = {
        prompt: within(composer()).queryByText("Discard draft?") !== null,
        text: field().value,
      };
      // A role's name given as a string is matched whole, so this is not `Discard draft`.
      await page.user.click(within(composer()).getByRole("button", { name: "Discard" }));
      expect({ after: { note: note(), text: field().value }, asked }).toEqual({
        after: { note: "none", text: "" },
        asked: { prompt: true, text: BODY },
      });
    } finally {
      page.restore();
    }
  });
}

test(`${HOST} · Close, then Discard draft? · cannot make: the composer draws no Close`, async () => {
  const page = await openDocked();
  try {
    expect(within(composer()).queryAllByRole("button", { name: "Close" }).length).toBe(0);
  } finally {
    page.restore();
  }
});

// On a phone a comment's Reply answers in the comment's full-screen thread, by the thread view's
// own composer (`usePhoneThread`'s `beginReply`), where above the phone layout it moves this
// composer onto the comment. So on a phone it is a host's move that covers this composer, mounted
// and hidden (`hidden={phoneThread !== undefined}`), and leaves its address alone: the Reply takes
// none of the draft into the thread's composer, and Back shows the draft again on the issue with
// its note and its Retry.
for (const held of HELD) {
  test(`${HOST} · phone · a comment's Reply, then Back · ${held}`, async () => {
    const page = await openDocked(held === "stopped" ? sentRequest(BODY, null) : undefined, true);
    try {
      await holdAtIssue(page, held);
      await page.user.click(within(listTurn(commentA.id)).getByRole("button", { name: "Reply" }));
      const thread = await screen.findByRole("dialog", { name: "Thread" });
      const away = {
        docked: screen
          .queryAllByRole("form", { name: "Comment composer" })
          .filter((form) => !thread.contains(form)).length,
        inThread: within(thread)
          .queryAllByRole<HTMLTextAreaElement>("textbox")
          .some((box) => box.value.includes(BODY)),
      };
      await page.user.click(within(thread).getByRole("button", { name: "Back" }));
      await waitFor(() => expect(screen.queryByRole("dialog", { name: "Thread" })).toBe(null));
      const back = {
        answersComment: within(composer()).queryByText(/Comment A/) !== null,
        note: note(),
        retry: retryOffered(),
        text: field().value,
      };
      expect({ away, back }).toEqual({
        away: { docked: 0, inThread: false },
        back: {
          answersComment: false,
          note: held === "typed" ? "none" : held,
          retry: held !== "typed",
          text: BODY,
        },
      });
    } finally {
      page.restore();
    }
  });
}
