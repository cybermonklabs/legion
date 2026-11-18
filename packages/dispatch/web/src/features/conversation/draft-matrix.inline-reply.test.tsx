/**
 * The draft matrix, inline reply row (INFRA-1503).
 *
 * Every composer host is held to one rule (packages/dispatch/AGENTS.md, "Who moved the
 * composer's address"): the reader's own move with the composer's own control takes the draft
 * with it, and a move of the host's carries nothing, so the text stays with the address it was
 * written for and is there when the reader comes back to it (AGENTS.md:116-119). A refused or
 * stopped send's note stays at its own address. An inline reply has no other composer for its own
 * controls to carry the draft to, so its Cancel reply and Escape end the reply and keep the draft
 * for the thread, which the closed card says (`keptReply`). Each cell writes a draft in one state,
 * typed and never sent, refused by Dispatch, or stopped by the reader, makes one move, and
 * asserts where the text ends. A cell that fails at the code it was written against is a hole in
 * the rule, kept red on purpose until the code is fixed.
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
 * view, Collapse thread unmounts the reply, and the comment's Reply hides it while the docked
 * composer answers the comment with a prefill of its own, not the card's draft. Every host move
 * of this row is one, so none needs a line while away.
 *
 * The moves this host cannot make:
 * 1. The card's reply always answers its own thread's root on its own issue, so no move of the
 *    host's changes its address, and it is never elsewhere.
 * 2. An inline reply is compact, so it draws no Close (asserted absent below).
 * 3. The card draws no issue picker (asserted absent below).
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
 */
import { expect, jest, type Mock, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import { realClock } from "../../__tests__/fake-clock";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { ApiError, api } from "../../api/client";
import type { Actor, Comment } from "../../api/types";
import { stubMatchMedia } from "../margin/margin-fixture";
import { KeymapProvider } from "../shell/KeymapProvider";
import { SendStoppedError } from "../writes/keyed-write";
import { ConversationTab } from "./ConversationTab";
import { heldSends } from "./held-sends";
import type { SentRequest } from "./send-request";
import { threadReplySendKey } from "./usePhoneThread";

const HOST = "inline reply";

const alice: Actor = { id: "alice", kind: "user" };

/** Dispatch refusing the send outright, as a 400 does: no attempt follows under its key. */
const REFUSAL = new ApiError(400, { code: "INPUT_REFUSED", error: "Dispatch refused the message" });

/** What each note says in the reply composer: the refusal, and a stopped send at its own
 *  address. */
const REFUSED_NOTE = "Couldn't send — Dispatch refused the message";
const STOPPED_NOTE = new SendStoppedError().message;

const BODY = "Draft the reader wrote";

type Held = "typed" | "refused" | "stopped";
const HELD: readonly Held[] = ["typed", "refused", "stopped"];

const commentA: Comment = {
  ...commentDeliveryFields(),
  anchor: null,
  ask_id: null,
  author: alice,
  body: "Comment A",
  created_at: "2026-09-20T00:00:00Z",
  edited_at: null,
  id: "comment-a",
  issue_key: "CORE-1",
  reply_to: null,
  resolved: false,
  resolved_at: null,
  resolved_by: null,
  suggestion: null,
  turn: null,
};

/** A reply of `BODY` to Comment A from its card, as the card's Send makes it. */
const replyToA: SentRequest = {
  anchor: undefined,
  ask: { multiple: false, options: [], urgency: "med" },
  draft: { body: BODY, mentions: [], replacement: "" },
  edit: undefined,
  kind: "comment",
  owner: { issueKey: "CORE-1", kind: "issue" },
  plainDelivery: "steer",
  replyTo: { actor: alice, excerpt: "", id: commentA.id, parentKind: "comment" },
  sendKey: "stopped-send",
};

interface Page {
  readonly createComment: Mock<typeof api.createComment>;
  readonly restore: () => void;
  readonly user: UserEvent;
}

/**
 * The Conversation of CORE-1 over Comment A, at a phone width or above it. Every comment the
 * page writes is refused, so a send pressed here comes back refused; with `stopped`, the card's
 * reply to Comment A is sent through the held-send store under the card's send name before the
 * page is drawn and stopped once late, as the reader's Stop trying does.
 */
async function openConversation(stopped: boolean, phone = false): Promise<Page> {
  const user = userEvent.setup();
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const reads = [
    spyOn(api, "listIssueAskThreads").mockResolvedValue([]),
    spyOn(api, "listComments").mockResolvedValue([commentA]),
    spyOn(api, "listIssueMessages").mockResolvedValue([]),
    spyOn(api, "listHandoffs").mockResolvedValue([]),
    spyOn(api, "listAgents").mockResolvedValue([]),
    spyOn(api, "getIssueEvents").mockResolvedValue([]),
  ];
  const createComment = spyOn(api, "createComment");
  if (stopped) {
    createComment.mockRejectedValue(
      new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" })
    );
    jest.useFakeTimers();
    try {
      act(() => {
        heldSends(queryClient).send(
          threadReplySendKey(["conversation-composer", "CORE-1"], commentA.id),
          replyToA
        );
      });
      await stopTheLateWrite(queryClient);
    } finally {
      realClock();
    }
  }
  createComment.mockRejectedValue(REFUSAL);
  const restoreViewport = stubMatchMedia(phone);
  const view = render(
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      <KeymapProvider>
        <QueryClientProvider client={queryClient}>
          <ConversationTab
            issueArtifacts={new Map()}
            isClosed={false}
            issueKey="CORE-1"
            state={{ "CORE-1": { dismissed: [], last_read_seq: 0, pinned: false, seq: 0 } }}
            visible
          />
        </QueryClientProvider>
      </KeymapProvider>
    </MemoryRouter>
  );
  await waitFor(() =>
    expect(document.querySelector('[data-turn="comment:comment-a"]')).not.toBe(null)
  );
  return {
    createComment,
    restore: () => {
      view.unmount();
      restoreViewport();
      createComment.mockRestore();
      for (const read of reads) read.mockRestore();
    },
    user,
  };
}

/** Comment A's turn in the list, outside any thread view. */
function turn(): HTMLElement {
  const found = [...document.querySelectorAll<HTMLElement>('[data-turn="comment:comment-a"]')].find(
    (element) => element.closest('[role="dialog"]') === null
  );
  if (found === undefined) throw new Error("expected comment-a's turn");
  return found;
}

const replyField = (place: HTMLElement) =>
  within(place).getByRole<HTMLTextAreaElement>("textbox", { name: "Reply" });

/** Which note `place` shows about a send it holds. */
function note(place: HTMLElement): Held | "none" {
  const shown = within(place);
  if (shown.queryByText(REFUSED_NOTE) !== null) return "refused";
  return shown.queryByText(STOPPED_NOTE) === null ? "none" : "stopped";
}

/** Whether the reader can see that `draft` is kept: a line on screen says so, or the draft is
 *  still in a field on screen. A status or a field inside something hidden is no role the query
 *  finds, so one under a covered or folded part of the page does not count. */
function keptInSight(draft: string): boolean {
  return (
    screen.queryAllByRole("status").some((line) => /\bkept\b/.test(line.textContent ?? "")) ||
    screen.queryAllByRole<HTMLTextAreaElement>("textbox").some((box) => box.value === draft)
  );
}

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
    expect(note(place)).toBe("stopped");
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
  return { note: note(place), text: replyField(place).value };
}

const expectedBack = (held: Held): { note: Held | "none"; text: string } => ({
  note: held === "typed" ? "none" : held,
  text: BODY,
});

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

test(`${HOST} · Agents pick · cannot make: the card draws no issue picker`, async () => {
  const page = await openConversation(false);
  try {
    const place = await holdInCard(page, "typed");
    expect(within(place).queryAllByRole("button", { name: "Choose issue" }).length).toBe(0);
  } finally {
    page.restore();
  }
});
