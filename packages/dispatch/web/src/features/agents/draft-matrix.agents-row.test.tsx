/**
 * The draft matrix, Agents row (INFRA-1503).
 *
 * Every composer host is held to one rule (packages/dispatch/AGENTS.md, "Who moved the
 * composer's address"): the reader's own move with the composer's own control takes the draft
 * with it, and a move of the host's carries nothing, so the text stays with the address it was
 * written for and is there when the reader comes back to it (AGENTS.md:116-119). A refused or
 * stopped send's note stays at its own address. Each cell writes a draft in one state, typed and
 * never sent, refused by Dispatch, or stopped by the reader, makes one move, and asserts where
 * the text ends. A cell that fails at the code it was written against is a hole in the rule, kept
 * red on purpose until the code is fixed.
 *
 * Host: an Agents row's composer (`AgentMessageComposer` in `AgentsPage`'s row), one box for the
 * session, whose reply the row's Reply buttons move, whose issue picker is the reader's own
 * control, and which the row's toggle folds away.
 *
 * A host move that takes the composer off the screen is checked on return alone, where the field
 * holds the draft with its refusal or stopped note; a host move that keeps the same composer on
 * screen at another address is checked on return too and, while away, for a line on screen that
 * says the earlier text is kept and names its message's author or words.
 *
 * | Move                                           | typed  | refused | stopped |
 * | ---------------------------------------------- | ------ | ------- | ------- |
 * | Back, then back                                | no (1) | no (1)  | no (1)  |
 * | Reply on another message, then back (on)       | cell   | cell    | cell    |
 * | Host moves it over a refusal (elsewhere) (on)  | no (2) | cell    | cell    |
 * | Row collapsed by the host, then opened (off)   | cell   | cell    | cell    |
 * | Cancel reply                                   | cell   | cell    | cell    |
 * | Escape over a reply                            | cell   | cell    | cell    |
 * | Escape over a message, then Discard            | cell   | cell    | cell    |
 * | Close, then Discard draft?                     | no (3) | no (3)  | no (3)  |
 * | Agents pick                                    | cell   | cell    | cell    |
 * | Typed while elsewhere, then unmounted          | -      | cell    | -       |
 *
 * (off) marks a host move that takes the composer off the screen: the folded row hides it. (on)
 * marks one that keeps the same composer on screen at another address. A refused or stopped
 * draft stays in its field there, with Send saying where it was written, so only the typed draft
 * needs the line.
 *
 * The moves this host cannot make:
 * 1. The Agents page has no thread view and no Back of its own; the row's composer stays on the
 *    page.
 * 2. Elsewhere needs a held refusal; the typed version is the unmount row.
 * 3. The row's composer has no anchor, so it draws no Close (asserted absent below).
 */
import { expect, jest, type Mock, spyOn, test } from "bun:test";
import { type MutationKey, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { realClock } from "../../__tests__/fake-clock";
import { answerAgentStates } from "../../__tests__/send-answers";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { ApiError, api } from "../../api/client";
import type { Agent, IssueSummary, Message } from "../../api/types";
import { AuthGate } from "../../app";
import type { ReplyTarget } from "../conversation/composer-model";
import { heldSends } from "../conversation/held-sends";
import type { SentRequest } from "../conversation/send-request";
import { SendStoppedError } from "../writes/keyed-write";

const HOST = "Agents row";

/** The row's send name (`agentSendKey`). */
const ROW_KEY: MutationKey = ["agent-composer", "planner-session"];

/** Dispatch refusing the send outright, as a 400 does: no attempt follows under its key. */
const REFUSAL = new ApiError(400, { code: "INPUT_REFUSED", error: "Dispatch refused the message" });

/** What each note says in the composer: the refusal, a stopped send at its own address, and a
 *  stopped send the reader took elsewhere with the composer's own control, off a reply or off
 *  the direct message. */
const REFUSED_NOTE = "Couldn't send — Dispatch refused the message";
const STOPPED_NOTE = new SendStoppedError().message;
const moved = (sent: string) =>
  `Dispatch has not confirmed this message, sent ${sent}: it may have been delivered there. ` +
  "Sending it from here is a new message, which can be delivered a second time if the first one was.";
const MOVED_REPLY_NOTE = moved("as a reply");
const MOVED_MESSAGE_NOTE = moved("as a direct message");

/** What Send says while the host has the composer away from where its refused draft lives. */
const WRITTEN_AS_REPLY =
  "This message was written as a reply. Reply to that message again to send it, or discard it.";
const WRITTEN_AS_MESSAGE =
  "This message was written as a direct message. Go back to it to send it, or discard it.";

const BODY = "Draft the reader wrote";

type Held = "typed" | "refused" | "stopped";
type Note = Held | "moved reply" | "moved message" | "none";
const HELD: readonly Held[] = ["typed", "refused", "stopped"];

const planner: Agent = {
  capabilities: ["aside", "btw"],
  dir: "/workspaces/planner",
  host: "build-host",
  last_activity: new Date(Date.now() - 60_000).toISOString(),
  last_seen: Date.now() - 30_000,
  machine_id: "build-host",
  open_asks: 0,
  roles: ["planner"],
  session_id: "planner-session",
  state: "running",
  title: "Planner",
};

const coreIssue: IssueSummary = {
  advances: [],
  advances_revision: 0,
  assignee: null,
  claim: null,
  components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
  key: "CORE-1",
  labels: [],
  last_seq: 0,
  open_asks: 0,
  progress: { tasks: null, children: null },
  parent: null,
  blocked_by: [],
  primary_artifact_id: "artifact-1",
  priority: null,
  rank: "U",
  route: null,
  repo: null,
  resolved_repo: null,
  route_holder: null,
  route_status: null,
  route_status_reason: null,
  status: "todo",
  title: "Core work",
  updated_at: "2026-09-14T00:00:00Z",
  closed_at: null,
  created_by: null,
  created_at: "2026-09-14T00:00:00Z",
};

function message(id: string, body: string, createdAt: string, author: string): Message {
  return {
    author: { id: author, kind: "user" },
    body,
    broadcast_id: null,
    created_at: createdAt,
    deliveries: [],
    id,
    in_reply_to: null,
    issue_key: null,
    reply_block: null,
    target: "session:planner-session",
  };
}

/** Two messages by two people, so a line naming the first message's author names it alone. */
const messageA = message("message-1", "Status A?", "2026-09-14T00:00:00Z", "alice");
const messageB = message("message-2", "Status B?", "2026-09-14T00:05:00Z", "bob");

/** Message A as the row's composer answers it. */
const replyToA: ReplyTarget = {
  actor: { id: "alice", kind: "user" },
  excerpt: messageA.body,
  id: messageA.id,
  parentKind: "message",
  thread: { delivery: "aside", target: "session:planner-session" },
};

/** A send of `BODY` from the row's composer, to the session or as a reply. */
function request(replyTo: ReplyTarget | null): SentRequest {
  return {
    anchor: undefined,
    ask: { multiple: false, options: [], urgency: "med" },
    draft: { body: BODY, mentions: [], replacement: "" },
    edit: undefined,
    kind: "comment",
    owner: { kind: "session", sessionId: "planner-session" },
    plainDelivery: "aside",
    replyTo,
    sendKey: "stopped-send",
  };
}

interface Page {
  readonly createAgentMessage: Mock<typeof api.createAgentMessage>;
  /** Draws the app again over the same client, as a reader coming back to the page. */
  readonly mount: () => void;
  readonly queryClient: QueryClient;
  readonly restore: () => void;
  /** Takes the app off the page, as a reader leaving it does. */
  readonly unmount: () => void;
  readonly user: UserEvent;
}

/**
 * The Agents page with the Planner's row, whose conversation holds Message A and Message B, the
 * row opened. Every write is refused, so a send pressed here comes back refused.
 */
async function openAgents(): Promise<Page> {
  const user = userEvent.setup();
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const reads = [
    spyOn(api, "whoAmI").mockResolvedValue({ event_head: "0", kind: "user", login: "alice" }),
    spyOn(api, "getInbox").mockResolvedValue([]),
    spyOn(api, "listAgents").mockResolvedValue([planner]),
    // Newest first, as Dispatch answers: the row folds every exchange but the newest.
    spyOn(api, "listAgentMessages").mockResolvedValue([
      { message: messageB, replies: [] },
      { message: messageA, replies: [] },
    ]),
    spyOn(api, "listIssues").mockResolvedValue([coreIssue]),
    spyOn(api, "listProjects").mockResolvedValue([]),
    spyOn(api, "getMyAgentState").mockResolvedValue({}),
  ];
  answerAgentStates().mockImplementation(async () => ({ unread_replies: 0 }));
  const createAgentMessage = spyOn(api, "createAgentMessage").mockRejectedValue(REFUSAL);
  const createComment = spyOn(api, "createComment").mockRejectedValue(REFUSAL);
  let unmount = () => {};
  const mount = () => {
    const view = render(
      <MemoryRouter initialEntries={["/agents"]}>
        <QueryClientProvider client={queryClient}>
          <AuthGate />
        </QueryClientProvider>
      </MemoryRouter>
    );
    unmount = () => view.unmount();
  };
  mount();
  await openRow(user);
  return {
    createAgentMessage,
    mount,
    queryClient,
    restore: () => {
      unmount();
      createAgentMessage.mockRestore();
      createComment.mockRestore();
      for (const read of reads) read.mockRestore();
    },
    unmount: () => unmount(),
    user,
  };
}

/**
 * Sends `sent` through the held-send store under the row's name, as its composer's Send does, to
 * a server that is down, and stops it once late, as the reader's Stop trying does. It is made
 * once the signed-in page is up: the app takes up the writes of the login it signs in as, and a
 * write made before that is not the row's to show.
 */
async function stopHere(page: Page, sent: SentRequest): Promise<void> {
  page.createAgentMessage.mockRejectedValue(
    new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" })
  );
  jest.useFakeTimers();
  try {
    act(() => {
      heldSends(page.queryClient).send(ROW_KEY, sent);
    });
    await stopTheLateWrite(page.queryClient);
  } finally {
    realClock();
  }
  page.createAgentMessage.mockRejectedValue(REFUSAL);
  await waitFor(() => expect(note()).toBe("stopped"));
}

/** The Planner's row. */
function row(): HTMLElement {
  const region = screen.getByRole("region", { name: "Agents" });
  const found = within(region).getByRole("heading", { name: "Planner" }).closest("article");
  if (found === null) throw new Error("Planner's row missing");
  return found;
}

/** Opens the Planner's row and its folded older exchange, and waits for both messages. */
async function openRow(user: UserEvent): Promise<void> {
  const region = await screen.findByRole("region", { name: "Agents" });
  await within(region).findByRole("heading", { name: "Planner" });
  await user.click(within(row()).getByRole("button", { name: "Planner" }));
  await user.click(await within(row()).findByRole("button", { name: "Show 1 older" }));
  await waitFor(() =>
    expect(row().querySelector('[data-turn="message:message-1"]')).not.toBe(null)
  );
}

const composer = () => within(row()).getByRole("form", { name: "Comment composer" });
const field = () =>
  within(composer()).getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" });

async function reply(user: UserEvent, id: string): Promise<void> {
  const turn = row().querySelector<HTMLElement>(`[data-turn="message:${id}"]`);
  if (turn === null) throw new Error(`expected ${id}'s turn`);
  await user.click(within(turn).getAllByRole("button", { name: "Reply" })[0] as HTMLElement);
}

/** Writes `body` over whatever the field holds. */
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
  if (shown.queryByText(/Status A\?/) !== null) return "A";
  return shown.queryByText(/Status B\?/) === null ? "none" : "B";
}

function note(): Note {
  const shown = within(composer());
  if (shown.queryByText(REFUSED_NOTE) !== null) return "refused";
  if (shown.queryByText(STOPPED_NOTE) !== null) return "stopped";
  if (shown.queryByText(MOVED_REPLY_NOTE) !== null) return "moved reply";
  return shown.queryByText(MOVED_MESSAGE_NOTE) === null ? "none" : "moved message";
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

/** Puts `BODY` in the row's composer as a reply to Message A, in the state `held` names. A
 *  stopped reply is sent and stopped once the composer answers Message A, whose field the
 *  stopped send then fills. */
async function holdAtReplyA(page: Page, held: Held): Promise<void> {
  await reply(page.user, messageA.id);
  if (held === "stopped") {
    await stopHere(page, request(replyToA));
    return;
  }
  await write(page.user, BODY);
  if (held === "refused") await sendRefused(page.user);
}

/** Puts `BODY` in the row's composer as a direct message to the session. */
async function holdAtSession(page: Page, held: Held): Promise<void> {
  if (held === "stopped") {
    await stopHere(page, request(null));
    return;
  }
  await write(page.user, BODY);
  if (held === "refused") await sendRefused(page.user);
}

// The same hole as review finding 1, on an Agents row: another message's Reply is the host's
// move, which carries nothing, and the same composer stays on screen at Message B's address, so
// the reply typed for Message A should wait for it and a line should say so, naming A. The new
// reply's prefill writes over the field instead, and nothing keeps it.
test(`${HOST} · Reply on another message, then back · typed · as review finding 1: the prefill writes over the reply and nothing keeps it`, async () => {
  const page = await openAgents();
  try {
    await holdAtReplyA(page, "typed");
    await reply(page.user, messageB.id);
    const away = { kept: keptLineNaming("alice", messageA.body), quoted: quoted() };
    await reply(page.user, messageA.id);
    expect({ away, back: { quoted: quoted(), text: field().value } }).toEqual({
      away: { kept: true, quoted: "B" },
      back: { quoted: "A", text: BODY },
    });
  } finally {
    page.restore();
  }
});

// A row's Reply is not redirected onto a held reply as the Conversation's is, so another
// message's Reply moves the composer off a refused or stopped reply: Send says where the draft was
// written and offers no Retry there, and Message A's Reply brings back its Retry.
for (const held of ["refused", "stopped"] as const) {
  test(`${HOST} · Reply on another message, then back · ${held}`, async () => {
    const page = await openAgents();
    try {
      await holdAtReplyA(page, held);
      await reply(page.user, messageB.id);
      const away = {
        elsewhere: within(composer()).queryByText(WRITTEN_AS_REPLY) !== null,
        quoted: quoted(),
        retry: retryOffered(),
        text: field().value,
      };
      await reply(page.user, messageA.id);
      const back = { note: note(), quoted: quoted(), retry: retryOffered(), text: field().value };
      expect({ away, back }).toEqual({
        away: { elsewhere: true, quoted: "B", retry: false, text: BODY },
        back: { note: held, quoted: "A", retry: true, text: BODY },
      });
    } finally {
      page.restore();
    }
  });
}

// A direct message refused or stopped, and then a message's Reply: the host has moved the
// composer off the session's own address. Cancel reply takes it back there, to the note and its
// Retry.
for (const held of ["refused", "stopped"] as const) {
  test(`${HOST} · host moves it over a refusal (elsewhere), then back · ${held}`, async () => {
    const page = await openAgents();
    try {
      await holdAtSession(page, held);
      await reply(page.user, messageB.id);
      const away = {
        elsewhere: within(composer()).queryByText(WRITTEN_AS_MESSAGE) !== null,
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

// Review hole (b), on an Agents row: while the composer is elsewhere nothing typed is written into
// the refusal's draft (`keepDraft` returns early), and the row's composer keeps no unsent draft of
// its own, so what the reader types for Message B over a refused direct message is kept nowhere
// once the page unmounts. The unmount takes the composer off the screen, so the cell checks the
// return alone: coming back to Message B should find it.
test(`${HOST} · typed while a refusal is held elsewhere, then the composer unmounts · refused · kept nowhere`, async () => {
  const page = await openAgents();
  const typed = "Reply meant for Status B";
  try {
    await holdAtSession(page, "refused");
    await reply(page.user, messageB.id);
    await write(page.user, typed);
    page.unmount();
    page.mount();
    await openRow(page.user);
    await reply(page.user, messageB.id);
    const back = screen
      .getAllByRole<HTMLTextAreaElement>("textbox")
      .some((box) => box.value.includes(typed));
    // The refusal comes back with the page, so what is missing is the typed text alone.
    expect({ back, refusal: note() }).toEqual({ back: true, refusal: "refused" });
  } finally {
    page.restore();
  }
});

// The row's toggle is the host's, and folding the row takes its composer off the screen: the row
// keeps the composer mounted and hidden, so the draft is back with its note when the row opens.
for (const held of HELD) {
  test(`${HOST} · row collapsed by the host, then opened · ${held}`, async () => {
    const page = await openAgents();
    try {
      await holdAtSession(page, held);
      await page.user.click(within(row()).getByRole("button", { name: "Planner" }));
      // The move this row is about: folded, the row shows no composer.
      const folded = within(row()).queryAllByRole("textbox", { name: "Comment" }).length;
      await page.user.click(within(row()).getByRole("button", { name: "Planner" }));
      expect({ back: { note: note(), text: field().value }, folded }).toEqual({
        back: { note: held === "typed" ? "none" : held, text: BODY },
        folded: 0,
      });
    } finally {
      page.restore();
    }
  });
}

/** What the reader's own move off a reply leaves: the draft goes with it to the session, a
 *  refusal with its Retry, and a stopped reply saying it may have been delivered as the reply. */
const ownMoveOffReply: Record<Held, { note: Note; retry: boolean }> = {
  refused: { note: "refused", retry: true },
  stopped: { note: "moved reply", retry: true },
  typed: { note: "none", retry: false },
};

for (const held of HELD) {
  test(`${HOST} · Cancel reply · ${held}`, async () => {
    const page = await openAgents();
    try {
      await holdAtReplyA(page, held);
      await page.user.click(within(composer()).getByRole("button", { name: "Cancel reply" }));
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

for (const held of HELD) {
  test(`${HOST} · Escape over a reply · ${held}`, async () => {
    const page = await openAgents();
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

// Escape over a direct message asks first, and only Discard drops the draft, its note with it.
for (const held of HELD) {
  test(`${HOST} · Escape over a message, then Discard · ${held}`, async () => {
    const page = await openAgents();
    try {
      await holdAtSession(page, held);
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

/** What an issue pick leaves: the draft goes with it under the issue's mention, a refusal with
 *  its Retry, and a stopped message saying it may have been delivered as the direct message. */
const ownMoveToIssue: Record<Held, { note: Note; retry: boolean }> = {
  refused: { note: "refused", retry: true },
  stopped: { note: "moved message", retry: true },
  typed: { note: "none", retry: false },
};

// The issue picker is the reader's own control (`ownMoves`), so the draft goes with the pick.
for (const held of HELD) {
  test(`${HOST} · Agents pick · ${held}`, async () => {
    const page = await openAgents();
    try {
      await holdAtSession(page, held);
      await page.user.click(within(row()).getByRole("button", { name: "Choose issue" }));
      await page.user.selectOptions(
        await within(row()).findByRole("combobox", { name: "Issue" }),
        coreIssue.key
      );
      await waitFor(() => expect(field().value).toBe(`@Planner ${BODY}`));
      expect({ note: note(), retry: retryOffered() }).toEqual(ownMoveToIssue[held]);
    } finally {
      page.restore();
    }
  });
}

test(`${HOST} · Close, then Discard draft? · cannot make: the row's composer draws no Close`, async () => {
  const page = await openAgents();
  try {
    expect(within(composer()).queryAllByRole("button", { name: "Close" }).length).toBe(0);
  } finally {
    page.restore();
  }
});
