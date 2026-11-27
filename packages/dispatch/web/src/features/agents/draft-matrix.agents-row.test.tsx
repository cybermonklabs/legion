/**
 * The draft matrix, Agents row (INFRA-1503).
 *
 * Every composer host is held to one rule (`.dispatch/architecture/dispatch-web.md`, "A composer's
 * draft - typed, refused or stopped - goes where the person takes it, and stays where the host
 * moved away from"): the reader's own move with the composer's own control takes the draft with
 * it, and a move of the host's carries nothing: the text stays with the address it was written
 * for, and is there when the reader comes back to it. A refused or stopped send's note
 * stays at its own address. Each cell writes a draft in one state, typed and never sent, refused
 * by Dispatch, or stopped by the reader, makes one move, and asserts where the text ends.
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
 * | The Agents page left, then back (off)          | cell   | cell    | cell    |
 * | The row first opened                           | no (4) | no (4)  | no (4)  |
 * | The row folded, filtered out or scrolled past  | no (5) | no (5)  | no (5)  |
 * | The session leaves the list, or none is listed | no (6) | no (6)  | no (6)  |
 * | A list item drawn as a group's heading         | no (7) | no (7)  | no (7)  |
 * | The page reloaded, then back (off)             | cell   | cell    | cell    |
 * | The message replied to leaves the list         | cell   | cell    | cell    |
 * | ... then Cancel reply                          | cell   | no (9)  | no (9)  |
 *
 * (off) marks a host move that takes the composer off the screen: the folded row hides it, and
 * leaving the page (`app.tsx`'s `/agents` route) unmounts the page and the composer, so the way
 * back opens the row again. (on) marks one that keeps the same composer on screen at another
 * address. A refused or stopped draft stays in its field there, with Send saying where it was
 * written, so only the typed draft needs the line. The reload row ends the whole app, as any
 * reload does (`endAsAReload`), and loads it again over the same tab: a new query client and write
 * store over the tab's `sessionStorage`, and the app signing the same reader in again. It is the
 * composer's lifetime the census finds (`lifetime.ts`).
 *
 * The two rows about the message replied to are a host move of another kind: the composer stays
 * where it is, and a refetch of the session's conversation answers it without the message a kept
 * reply answers - the list the row draws, which a Clear, a fold or Show older only show less or
 * more of - so the Reply the kept line names is gone. The composer draws a typed reply itself
 * then, saying the message it answered is no longer listed, with Discard my draft, and on the
 * row's own address, where Cancel reply takes it, with Use it here too. A refused or stopped reply
 * stays in the field there, as in the elsewhere row, and Send says the message it answered is no
 * longer listed in place of naming that message's Reply, with Discard draft beside its note.
 *
 * The numbered reasons - the moves this host cannot make, and the moves and forms the composer
 * census found that the rows above do not make - are `REASONS` in
 * `draft-matrix.agents-row.coverage.ts`, which holds each census item this file answers for to a
 * test above or to one of them.
 */
import { beforeAll, expect, type Mock, spyOn, test } from "bun:test";
import { type MutationKey, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import {
  BODY,
  endAsAReload,
  goneIn,
  goneLead,
  HELD,
  type Held,
  inTheTab,
  keptLineNaming,
  movedNote,
  noteIn,
  planner,
  REFUSAL,
  REFUSED_NOTE,
} from "../../__tests__/draft-matrix-fixture";
import { panelOf, pickNamed } from "../../__tests__/list-picks";
import { messageCorrectionFields } from "../../__tests__/message-fixture";
import { answerAgentStates } from "../../__tests__/send-answers";
import { sentRequest, stopSend } from "../../__tests__/stop-late-write";
import { api } from "../../api/client";
import type { IssueSummary, Message } from "../../api/types";
import { AuthGate } from "../../app";
import type { ReplyTarget } from "../conversation/composer-model";
import type { SentRequest } from "../conversation/send-request";
import { warmMarkdownRenderer } from "../refs/markdown-engine";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

// The composer draws the quote of the message it answers through the Markdown engine, a module
// loaded on first use (`markdown-engine.ts`). A cell reads what the composer quotes in the commit
// after its press, which a first load of the engine can come after, so the engine is loaded
// before the rows begin, as in `draft-matrix.docked.test.tsx`.
beforeAll(warmMarkdownRenderer);

const HOST = "Agents row";

/** The row's send name (`agentSendKey`). */
const ROW_KEY: MutationKey = ["agent-composer", "planner-session"];

/** What the composer says of a stopped send the reader took elsewhere with the composer's own
 *  control, off a reply or off the direct message. */
const MOVED_REPLY_NOTE = movedNote("as a reply");
const MOVED_MESSAGE_NOTE = movedNote("as a direct message");

/** What Send says while the host has the composer away from where its refused draft lives. */
const WRITTEN_AS_REPLY =
  "This message was written as a reply. Reply to that message again to send it, or discard it.";
const WRITTEN_AS_MESSAGE =
  "This message was written as a direct message. Go back to it to send it, or discard it.";
/** What Send says instead once the message that reply answered is no longer listed, where no
 *  Reply goes back to it. */
const WRITTEN_FOR_A_GONE_MESSAGE =
  "This message was written as a reply to a message that is no longer listed. Discard it to write here.";

type Note = Held | "moved reply" | "moved message" | "none";

const coreIssue: IssueSummary = {
  advances: [],
  advances_revision: 0,
  assignee: null,
  claim: null,
  components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
  key: "CORE-1",
  project: "CORE",
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
    ...messageCorrectionFields(),
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
  return sentRequest(BODY, replyTo, {
    owner: { kind: "session", sessionId: "planner-session" },
    plainDelivery: "aside",
  });
}

interface Page {
  readonly createAgentMessage: Mock<typeof api.createAgentMessage>;
  /** Reads the session's conversation again, as a refetch does, and answers it without Message
   *  A: its turn, and its Reply with it, leave the row. */
  readonly dropMessageA: () => Promise<void>;
  /** Draws the app again over the same client, as a reader coming back to the page. */
  readonly mount: () => void;
  readonly queryClient: QueryClient;
  /** Ends the app as a reload does and loads it again over the same tab (`endAsAReload`): a new
   *  query client, whose sign-in check answers the same reader. */
  readonly reload: () => void;
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
  const newClient = () => new QueryClient({ defaultOptions: { queries: { retry: false } } });
  let queryClient = newClient();
  // Newest first, as Dispatch answers: the row folds every exchange but the newest.
  const messages = spyOn(api, "listAgentMessages").mockResolvedValue([
    { message: messageB, replies: [] },
    { message: messageA, replies: [] },
  ]);
  const reads = [
    spyOn(api, "whoAmI").mockResolvedValue({ event_head: "0", kind: "user", login: "alice" }),
    spyOn(api, "getInbox").mockResolvedValue([]),
    spyOn(api, "listAgents").mockResolvedValue([planner]),
    messages,
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
    dropMessageA: async () => {
      messages.mockResolvedValue([{ message: messageB, replies: [] }]);
      await act(() =>
        queryClient.invalidateQueries({ queryKey: ["agents", "planner-session", "messages"] })
      );
      await waitFor(() =>
        expect(row().querySelector('[data-turn="message:message-1"]')).toBe(null)
      );
    },
    mount,
    get queryClient() {
      return queryClient;
    },
    reload: () => {
      endAsAReload(unmount);
      unmount = () => {};
      queryClient = newClient();
      mount();
    },
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
  await stopSend(page.queryClient, ROW_KEY, sent, [page.createAgentMessage]);
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
  const held = noteIn(composer());
  if (held !== "none") return held;
  const shown = within(composer());
  if (shown.queryByText(MOVED_REPLY_NOTE) !== null) return "moved reply";
  return shown.queryByText(MOVED_MESSAGE_NOTE) === null ? "none" : "moved message";
}

function retryOffered(): boolean {
  return within(composer()).queryByRole("button", { name: "Retry" }) !== null;
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

// Another message's Reply is the host's move, which carries nothing, and the same composer stays
// on screen at Message B's address. The reply typed for Message A is kept for A (`awayName` in
// `useKeptDrafts`), a line says so and names A (`keptAwayLine`), and A's Reply puts it back in
// the field in place of a new reply's prefill.
test(`${HOST} · Reply on another message, then back · typed`, async () => {
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

// While the host has the composer away from the session its refused direct message is for,
// nothing typed is written into the refusal's draft: what the reader types for Message B is that
// reply's, and it is kept for it as the page unmounts (`keepAway` in `useKeptDrafts`). The unmount
// takes the composer off the screen, so the cell checks the return alone: coming back to Message B
// finds it, and the refusal comes back with the page as well.
test(`${HOST} · typed while a refusal is held elsewhere, then the composer unmounts · refused`, async () => {
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

/** Goes to the Inbox by the shell's Inbox link and back to the Agents page through the
 *  navigation drawer, as a reader on a narrow window does. */
async function leaveTheAgentsPageAndComeBack(user: UserEvent): Promise<void> {
  await user.click(screen.getByRole("link", { name: "Inbox" }));
  await waitFor(() => expect(screen.queryByRole("region", { name: "Agents" })).toBe(null));
  await user.click(screen.getByRole("button", { name: "Open navigation" }));
  await user.click(await screen.findByRole("link", { name: /^Agents/ }));
}

// The Agents page is the route element of `/agents` (`app.tsx`), so leaving it for another page
// unmounts the page, its rows and the composer, and the reader's way back opens the row again.
// `.dispatch/architecture/dispatch-web.md` holds it to "a composer its host moves from reply to
// reply, or takes off the page ... keeps what the field holds for the reply it answers, or for
// none."
for (const held of HELD) {
  test(`${HOST} · the Agents page left, then back · ${held}`, async () => {
    const page = await openAgents();
    try {
      await holdAtSession(page, held);
      await leaveTheAgentsPageAndComeBack(page.user);
      await openRow(page.user);
      expect({ note: note(), quoted: quoted(), text: field().value }).toEqual({
        note: held === "typed" ? "none" : held,
        quoted: "none",
        text: BODY,
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
      const trigger = within(row()).getByRole("button", { name: pickNamed("Choose issue") });
      await page.user.click(trigger);
      // The list is portalled out of the row, so its option is found in the panel the trigger
      // names.
      const option = `${coreIssue.key} · ${coreIssue.title}`;
      await page.user.click(await within(panelOf(trigger)).findByRole("option", { name: option }));
      // The issue's seed names Planner as the link a pick writes (`seededDraft`).
      await waitFor(() =>
        expect(field().value).toBe(`[@Planner](dispatch://agent/planner-session) ${BODY}`)
      );
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

// A reload ends the whole app and loads it again over the same tab (`endAsAReload`): a new query
// client, and the app signing in again as the same reader (`AuthGate`). Opening the row again,
// the way back to the composer, finds the message the reader wrote to the session, with its note.
for (const held of HELD) {
  test(`${HOST} · the page reloaded, then back · ${held}`, async () => {
    const page = await openAgents();
    try {
      await holdAtSession(page, held);
      page.reload();
      await openRow(page.user);
      expect({ note: note(), quoted: quoted(), text: field().value }).toEqual({
        note: held === "typed" ? "none" : held,
        quoted: "none",
        text: BODY,
      });
    } finally {
      page.restore();
    }
  });
}

// A refetch can answer the session's conversation without the message a kept reply answers, and
// the kept line's way back, that message's Reply, goes with it. The composer, still answering
// Message B, draws the reply kept for Message A itself then, saying A is no longer listed, with
// Discard my draft, which forgets it in the page and in the tab. On a reply it offers no Use it
// here.
test(`${HOST} · the message replied to leaves the list · typed`, async () => {
  const page = await openAgents();
  try {
    await holdAtReplyA(page, "typed");
    await reply(page.user, messageB.id);
    const prefill = field().value;
    await page.dropMessageA();
    expect({ ...goneIn(composer()), kept: inTheTab(BODY), quoted: quoted() }).toEqual({
      discard: true,
      kept: true,
      line: `${goneLead("message")}${BODY}`,
      pressReply: false,
      quoted: "B",
      use: false,
    });
    await page.user.click(within(composer()).getByRole("button", { name: "Discard my draft" }));
    const after = { ...goneIn(composer()), kept: inTheTab(BODY), quoted: quoted() };
    expect({ ...after, text: field().value }).toEqual({
      discard: false,
      kept: false,
      line: undefined,
      pressReply: false,
      quoted: "B",
      text: prefill,
      use: false,
    });
  } finally {
    page.restore();
  }
});

// A refused or stopped reply to Message A stays in the field when Message B's Reply moves the
// composer off it, and Send says where it was written and which Reply brings it back. A refetch
// then answers the conversation without A, and that Reply goes with it: Send says the message the
// reply answered is no longer listed and names no Reply, the draft stays in the field, and Discard
// draft beside its note drops it, in the page and in the tab.
for (const held of ["refused", "stopped"] as const) {
  test(`${HOST} · the message replied to leaves the list · ${held}`, async () => {
    const page = await openAgents();
    try {
      await holdAtReplyA(page, held);
      await reply(page.user, messageB.id);
      await page.dropMessageA();
      const shown = within(composer());
      expect({
        discard: shown.queryByRole("button", { name: "Discard draft" }) !== null,
        gone: shown.queryByText(WRITTEN_FOR_A_GONE_MESSAGE) !== null,
        named: shown.queryByText(WRITTEN_AS_REPLY) !== null,
        note: note(),
        retry: retryOffered(),
        text: field().value,
      }).toEqual({ discard: true, gone: true, named: false, note: held, retry: false, text: BODY });
      await page.user.click(shown.getByRole("button", { name: "Discard draft" }));
      expect({ kept: inTheTab(BODY), note: note(), text: field().value }).toEqual({
        kept: false,
        note: "none",
        text: "",
      });
    } finally {
      page.restore();
    }
  });
}

// Cancel reply takes the composer to the row's own address, the session, where a reply whose
// message left the row can go on as a direct message: Use it here puts it in the field, which
// holds nothing of the reader's own, and it is kept for Message A no longer, in the page or in
// the tab.
test(`${HOST} · the message replied to leaves the list, then Cancel reply · typed`, async () => {
  const page = await openAgents();
  try {
    await holdAtReplyA(page, "typed");
    await reply(page.user, messageB.id);
    await page.dropMessageA();
    await page.user.click(within(composer()).getByRole("button", { name: "Cancel reply" }));
    expect({ ...goneIn(composer()), quoted: quoted(), text: field().value }).toEqual({
      discard: true,
      line: `${goneLead("message")}${BODY}`,
      pressReply: false,
      quoted: "none",
      text: "",
      use: true,
    });
    await page.user.click(within(composer()).getByRole("button", { name: "Use it here" }));
    const after = { ...goneIn(composer()), kept: inTheTab(BODY), quoted: quoted() };
    expect({ ...after, text: field().value }).toEqual({
      discard: false,
      kept: false,
      line: undefined,
      pressReply: false,
      quoted: "none",
      text: BODY,
      use: false,
    });
  } finally {
    page.restore();
  }
});
