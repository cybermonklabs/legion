/**
 * The draft matrix, desktop docked composer row (INFRA-1503).
 *
 * Every composer host is held to one rule (packages/dispatch/AGENTS.md, "Who moved the
 * composer's address"): the reader's own move with the composer's own control takes the draft
 * with it, and a move of the host's carries nothing, so the text stays with the address it was
 * written for and is there when the reader comes back to it (AGENTS.md:116-119). A refused or
 * stopped send's note stays at its own address. Each cell below writes a draft in one state -
 * typed and never sent, refused by Dispatch, or stopped by the reader - makes one move, and
 * asserts where the text ends. A cell that fails at the code it was written against is a hole in
 * the rule, kept red on purpose until the code is fixed.
 *
 * Host: the Conversation's docked composer above the phone layout, one box for the issue whose
 * reply a message's Reply moves (`ConversationTab`'s `beginReply`).
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
 *
 * (on) marks a host move that keeps the same composer on screen at another address. A refused or
 * stopped draft never leaves its field there (another message's Reply is redirected onto it, and
 * the elsewhere row keeps it in the field with Send saying where it was written), so only the
 * typed draft needs the line.
 *
 * The moves this host cannot make:
 * 1. Above the phone layout no thread view covers the docked composer, and on a phone Back only
 *    uncovers it, never moving its address.
 * 2. Elsewhere needs a held refusal; the typed version is the unmount row.
 * 3. The docked composer sits outside every thread card, so no Collapse thread moves it.
 * 4. The docked composer has no anchor, so it draws no Close (asserted absent below).
 * 5. The docked composer draws no issue picker (asserted absent below).
 */
import { expect, jest, type Mock, spyOn, test } from "bun:test";
import { type MutationKey, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { realClock } from "../../__tests__/fake-clock";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { ApiError, api } from "../../api/client";
import type { Actor, Message } from "../../api/types";
import { KeymapProvider } from "../shell/KeymapProvider";
import { SendStoppedError } from "../writes/keyed-write";
import { ConversationTab } from "./ConversationTab";
import type { ReplyTarget } from "./composer-model";
import { heldSends } from "./held-sends";
import type { SentRequest } from "./send-request";

const HOST = "docked composer";

const alice: Actor = { id: "alice", kind: "user" };

/** The docked composer's send name (`ConversationTab`'s `sendKey`). */
const DOCKED_KEY: MutationKey = ["conversation-composer", "CORE-1"];

/** Dispatch refusing the send outright, as a 400 does: no attempt follows under its key. */
const REFUSAL = new ApiError(400, { code: "INPUT_REFUSED", error: "Dispatch refused the message" });

/** What each note says in the composer: the refusal, a stopped send at its own address, and a
 *  stopped reply the reader took off its message with the composer's own control. */
const REFUSED_NOTE = "Couldn't send — Dispatch refused the message";
const STOPPED_NOTE = new SendStoppedError().message;
const MOVED_NOTE =
  "Dispatch has not confirmed this message, sent as a reply: it may have been delivered there. " +
  "Sending it from here is a new message, which can be delivered a second time if the first one was.";

/** What Send says while the host has the composer away from the issue its refused comment is for. */
const WRITTEN_FOR_ISSUE =
  "This message was written for CORE-1. Go back to it to send it, or discard it.";

const BODY = "Draft the reader wrote";

type Held = "typed" | "refused" | "stopped";
const HELD: readonly Held[] = ["typed", "refused", "stopped"];

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

/** A send of `BODY` from the docked composer, to the issue or as a reply. */
function request(replyTo: ReplyTarget | null): SentRequest {
  return {
    anchor: undefined,
    ask: { multiple: false, options: [], urgency: "med" },
    draft: { body: BODY, mentions: [], replacement: "" },
    edit: undefined,
    kind: "comment",
    owner: { issueKey: "CORE-1", kind: "issue" },
    plainDelivery: "steer",
    replyTo,
    sendKey: "stopped-send",
  };
}

interface Page {
  readonly createComment: Mock<typeof api.createComment>;
  readonly createMessage: Mock<typeof api.createMessage>;
  /** Draws the Conversation again over the same client, as a reader coming back to the tab. */
  readonly mount: () => void;
  readonly restore: () => void;
  /** Takes the Conversation off the page, as a reader leaving the tab does. */
  readonly unmount: () => void;
  readonly user: UserEvent;
}

/**
 * The Conversation of CORE-1 above the phone layout, over Message A and Message B. Every write
 * the page makes is refused, so a send pressed here comes back refused; `stopped`, when given,
 * is sent through the held-send store under the docked composer's name before the page is drawn
 * and stopped once late, as the reader's Stop trying does, so the composer mounts showing it.
 */
async function openDocked(stopped?: SentRequest): Promise<Page> {
  const user = userEvent.setup();
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const reads = [
    spyOn(api, "listIssueAskThreads").mockResolvedValue([]),
    spyOn(api, "listComments").mockResolvedValue([]),
    spyOn(api, "listIssueMessages").mockResolvedValue([
      { message: messageA, replies: [] },
      { message: messageB, replies: [] },
    ]),
    spyOn(api, "listHandoffs").mockResolvedValue([]),
    spyOn(api, "listAgents").mockResolvedValue([]),
    spyOn(api, "getIssueEvents").mockResolvedValue([]),
  ];
  const createComment = spyOn(api, "createComment");
  const createMessage = spyOn(api, "createMessage");
  if (stopped !== undefined) {
    const down = new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" });
    createComment.mockRejectedValue(down);
    createMessage.mockRejectedValue(down);
    jest.useFakeTimers();
    try {
      act(() => {
        heldSends(queryClient).send(DOCKED_KEY, stopped);
      });
      await stopTheLateWrite(queryClient);
    } finally {
      realClock();
    }
  }
  createComment.mockRejectedValue(REFUSAL);
  createMessage.mockRejectedValue(REFUSAL);
  let unmount = () => {};
  const mount = () => {
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
      createComment.mockRestore();
      createMessage.mockRestore();
      for (const read of reads) read.mockRestore();
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
  const shown = within(composer());
  if (shown.queryByText(REFUSED_NOTE) !== null) return "refused";
  if (shown.queryByText(STOPPED_NOTE) !== null) return "stopped";
  return shown.queryByText(MOVED_NOTE) === null ? "none" : "moved";
}

function retryOffered(): boolean {
  return within(composer()).queryByRole("button", { name: "Retry" }) !== null;
}

/** Whether a line the reader can see says the earlier draft is kept and names where it was
 *  written - one of `names`, its message's author or words - so the reader can find it again. */
function keptLineNaming(...names: string[]): boolean {
  return screen.queryAllByRole("status").some((line) => {
    const text = line.textContent ?? "";
    return /\bkept\b/.test(text) && names.some((name) => text.includes(name));
  });
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

// Review finding 1, at a desktop width: a message's Reply is the host's move, which carries
// nothing, and the same composer stays on screen at Message B's address, so the reply typed for
// Message A should wait for A and a line should say so, naming A. The prefill of the new reply
// (`MentionComposer`'s reply effect, reached through `beginReply`) writes over the field instead,
// and nothing keeps what was there.
test(`${HOST} · Reply on another message, then back · typed · review finding 1: the prefill writes over the reply and nothing keeps it`, async () => {
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
    const page = await openDocked(held === "stopped" ? request(replyToA) : undefined);
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
    const page = await openDocked(held === "stopped" ? request(null) : undefined);
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

// Review hole (b): while the composer is elsewhere nothing typed is written into the refusal's
// draft (`keepDraft` returns early), and the docked composer keeps no unsent draft of its own, so
// what the reader types for Message B over a refusal for the issue is kept nowhere once the
// composer unmounts. The unmount takes the composer off the screen, so the cell checks the return
// alone: coming back to Message B should find it.
test(`${HOST} · typed while a refusal is held elsewhere, then the composer unmounts · refused · kept nowhere`, async () => {
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
    // The refusal comes back with the tab, so what is missing is the typed text alone.
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
    const page = await openDocked(held === "stopped" ? request(replyToA) : undefined);
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
    const page = await openDocked(held === "stopped" ? request(replyToA) : undefined);
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
    const page = await openDocked(held === "stopped" ? request(null) : undefined);
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

test(`${HOST} · Agents pick · cannot make: the composer draws no issue picker`, async () => {
  const page = await openDocked();
  try {
    expect(screen.queryAllByRole("button", { name: "Choose issue" }).length).toBe(0);
  } finally {
    page.restore();
  }
});
