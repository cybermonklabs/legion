/**
 * What the draft matrix files (`draft-matrix.*.test.tsx`, INFRA-1503) share: the refusal their
 * writes get and the notes a composer shows about a send it holds, the draft a cell writes and the
 * states it holds it in, the lines a cell reads to tell whether a draft is kept and the reply
 * field it reads a draft back from, what a composer draws of a reply kept for a comment or message
 * that left the list and whether the tab keeps a text, the comments the Conversation's hosts
 * answer, the Conversation page as each Conversation host draws it (and as
 * `ConversationTab.test.tsx` draws it), the margin's page over two threads and the documents it
 * registers (and as `Margin.test.tsx` registers them), the Inbox reading column the two Inbox
 * files draw, the Planner session an Agents row draws, and the reload a reload row makes. A
 * stopped send is seeded with `stop-late-write.ts`.
 */
import { expect, spyOn } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import type { UserEvent } from "@testing-library/user-event";
import { type ReactNode, useEffect } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { ApiError, api } from "../api/client";
import type {
  Actor,
  Agent,
  Artifact,
  Comment,
  DispatchEvent,
  Handoff,
  IssueAskThreadRow,
  MessageRead,
  MyReplyRow,
  UserState,
} from "../api/types";
import { ConversationTab } from "../features/conversation/ConversationTab";
import { draftStoresReloaded, draftStoresSignedIn } from "../features/conversation/unsent-drafts";
import { DocumentRuntime } from "../features/doc/runtime";
import { ReadingPane } from "../features/inbox/ReadingPane";
import { Margin } from "../features/margin/Margin";
import { type DocumentBridge, MarginProvider, useMargin } from "../features/margin/margin-context";
import {
  comment,
  issue,
  specArtifact,
  stubDocumentBridge,
} from "../features/margin/margin-fixture";
import { buildIssuePath } from "../features/refs/page-routes";
import { KeymapProvider } from "../features/shell/KeymapProvider";
import { SendStoppedError } from "../features/writes/keyed-write";
import { entryNames, removeEntry } from "../features/writes/tab-storage";
import { writesOf } from "../features/writes/write-store";
import { commentDeliveryFields } from "./comment-fixture";
import { fakeDocumentRuntime } from "./document-runtime";
import { graphNode } from "./graph-fixture";

/** Dispatch refusing a send outright, as a 400 does: no attempt follows under its key. */
export const REFUSAL = new ApiError(400, {
  code: "INPUT_REFUSED",
  error: "Dispatch refused the message",
});

/** What a composer says of a send Dispatch refused, and of a send the reader stopped, at the
 *  send's own address. */
export const REFUSED_NOTE = "Couldn't send — Dispatch refused the message";
export const STOPPED_NOTE = new SendStoppedError().message;

/** What a composer says of a stopped send the reader took to another address with its own
 *  control, the send having gone `sent`: as a reply, or as a direct message (`SendMovedError`). */
export function movedNote(sent: string): string {
  return (
    `Dispatch has not confirmed this message, sent ${sent}: it may have been delivered there. ` +
    "Sending it from here is a new message, which can be delivered a second time if the first one was."
  );
}

/** The draft a cell writes. */
export const BODY = "Draft the reader wrote";

/** The states a cell holds its draft in: typed and never sent, refused by Dispatch, or stopped
 *  by the reader. */
export type Held = "typed" | "refused" | "stopped";
export const HELD: readonly Held[] = ["typed", "refused", "stopped"];

/** Which note `place` shows about a send it holds. */
export function noteIn(place: HTMLElement): Held | "none" {
  const shown = within(place);
  if (shown.queryByText(REFUSED_NOTE) !== null) return "refused";
  return shown.queryByText(STOPPED_NOTE) === null ? "none" : "stopped";
}

/** What a composer that comes back after a host's move holds: `BODY`, with the note its refused
 *  or stopped send had. */
export function expectedBack(held: Held): { note: Held | "none"; text: string } {
  return { note: held === "typed" ? "none" : held, text: BODY };
}

/** The reply field in `place`: a thread card's, or a thread view's. */
export function replyField(place: HTMLElement): HTMLTextAreaElement {
  return within(place).getByRole<HTMLTextAreaElement>("textbox", { name: "Reply" });
}

/** What the reply composer in `place` holds where it shows again: its text, and the note it shows
 *  about a send it holds. */
export function heldIn(place: HTMLElement): { note: Held | "none"; text: string } {
  return { note: noteIn(place), text: replyField(place).value };
}

/** Whether a line the reader can see says the earlier draft is kept and names where it was
 *  written - one of `names`, its message's or comment's author or words - so the reader can find
 *  it again. A status inside something hidden is no role the query finds. */
export function keptLineNaming(...names: string[]): boolean {
  return screen.queryAllByRole("status").some((line) => {
    const text = line.textContent ?? "";
    return /\bkept\b/.test(text) && names.some((name) => text.includes(name));
  });
}

/** Whether the reader can see that `draft` is kept: a line on screen says so, or the draft is
 *  still in a field on screen, as it is where a list shows its thread open. A status or a field
 *  inside something hidden is no role the query finds, so one under a covered or folded part of
 *  the page does not count. */
export function keptInSight(draft: string): boolean {
  return (
    screen.queryAllByRole("status").some((line) => /\bkept\b/.test(line.textContent ?? "")) ||
    screen.queryAllByRole<HTMLTextAreaElement>("textbox").some((box) => box.value === draft)
  );
}

/** What a composer says of a reply it keeps for alice's `parent` once that has left the list
 *  (`GoneReply`), before the reply's own text, which the same status draws after it. */
export function goneLead(parent: "comment" | "message"): string {
  return `The ${parent} by alice that your reply answered is no longer listed. Your reply is kept here:`;
}

/** What `place` draws of a reply it keeps for a comment or message that has left the list: the
 *  status that holds the reply's text, `BODY`, and the presses beside it; and whether any status
 *  on the page, in `place` or beside it, still asks the reader to press a Reply (`KeptReply`'s
 *  `Press Reply on that …`), which the gone comment or message no longer draws. */
export function goneIn(place: HTMLElement): {
  discard: boolean;
  line: string | undefined;
  pressReply: boolean;
  use: boolean;
} {
  const shown = within(place);
  return {
    discard: shown.queryByRole("button", { name: "Discard my draft" }) !== null,
    line: shown
      .queryAllByRole("status")
      .map((status) => status.textContent ?? "")
      .find((text) => text.includes(BODY)),
    pressReply: screen
      .queryAllByRole("status")
      .some((status) => /\bPress Reply\b/.test(status.textContent ?? "")),
    use: shown.queryByRole("button", { name: "Use it here" }) !== null,
  };
}

/** Whether an entry of the tab's `sessionStorage` holds `text`, for a reload to bring back. */
export function inTheTab(text: string): boolean {
  return entryNames(sessionStorage, "").some((name) =>
    (sessionStorage.getItem(name) ?? "").includes(text)
  );
}

export const alice: Actor = { id: "alice", kind: "user" };

function conversationComment(id: string, body: string, createdAt: string, author: Actor): Comment {
  return {
    ...commentDeliveryFields(),
    anchor: null,
    ask_id: null,
    author,
    body,
    created_at: createdAt,
    edited_at: null,
    id,
    issue_key: "CORE-1",
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    turn: null,
  };
}

/** Two comments on CORE-1 by two people, so a line naming the first comment's author names it
 *  alone. */
export const commentA = conversationComment(
  "comment-a",
  "Comment A",
  "2026-09-20T00:00:00Z",
  alice
);
export const commentB = conversationComment("comment-b", "Comment B", "2026-09-20T00:01:00Z", {
  id: "bob",
  kind: "user",
});

/** Comment `id`'s turn in the Conversation's list, outside any thread view. */
export function listTurn(id: string): HTMLElement {
  const found = [...document.querySelectorAll<HTMLElement>(`[data-turn="comment:${id}"]`)].find(
    (element) => element.closest('[role="dialog"]') === null
  );
  if (found === undefined) throw new Error(`expected ${id}'s turn`);
  return found;
}

export interface Lists {
  agents?: Agent[];
  asks?: IssueAskThreadRow[];
  comments?: Comment[];
  /** The issue's handoffs with their bodies: the list answers them as they are (a body is extra
   *  to a summary), and `getHandoff` answers one by id. */
  handoffs?: Handoff[];
  /** The issue's newest event, which a dwell on the newest card marks read through. */
  head?: DispatchEvent[];
  messages?: MessageRead[];
  /** The first event the reader has not read: its moment is the unread boundary. */
  unread?: DispatchEvent[];
}

/** The four reads the Conversation builds its list from, plus the two one-row event reads that
 *  carry the unread boundary and the issue's head, the read of one handoff's body, and the blocks
 *  read an opened comment editor makes, answered with no block ids. The returned `lists` is live:
 *  a test that changes it changes what the next refetch answers. */
export function stubLists(initial: Lists = {}): { lists: Lists; restore: () => void } {
  const lists: Lists = { ...initial };
  const spies = [
    spyOn(api, "listIssueAskThreads").mockImplementation(async () => lists.asks ?? []),
    spyOn(api, "listComments").mockImplementation(async () => lists.comments ?? []),
    spyOn(api, "listIssueMessages").mockImplementation(async () => lists.messages ?? []),
    spyOn(api, "listHandoffs").mockImplementation(async () => lists.handoffs ?? []),
    spyOn(api, "getHandoff").mockImplementation(async (id) => {
      const found = lists.handoffs?.find((row) => row.id === id);
      if (found === undefined) throw new ApiError(404, { error: `no handoff ${id}` });
      return found;
    }),
    spyOn(api, "listAgents").mockImplementation(async () => lists.agents ?? []),
    spyOn(api, "commentBlocks").mockImplementation(async (id) => ({
      blocks: [],
      indexed: false,
      item: graphNode({ id, issue_key: "CORE-1", kind: "comment" }),
    })),
    spyOn(api, "getIssueEvents").mockImplementation(async (_key, options = {}) =>
      options.order === "desc" ? (lists.head ?? []) : (lists.unread ?? [])
    ),
  ];
  return {
    lists,
    restore: () => {
      for (const spy of spies) spy.mockRestore();
    },
  };
}

/** A page's query client whose reads are never retried and never go stale. */
export function newQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
}

/** The login of the reader every cell writes as. */
const READER_LOGIN = "alice";

/**
 * `queryClient` signed in as the reader, as the app's shell signs in on each load of the page
 * (`AuthGate`, `AuthenticatedApp`): the sign-in check's answer, which stamps each write with the
 * reader's login so that the tab's `sessionStorage` keeps it; every store told who is signed in;
 * and the writes the tab kept taken up again (`Writes.resume`). A reload row signs in the page it
 * starts on so, and the page the reload loads, whose query client is new, and so are its write
 * store and its unsent drafts, over the same tab; the answer and reply drafts a reload emptied
 * (`endAsAReload`) take up what the tab kept for the reader at this sign-in.
 */
export function signedInAsReader(queryClient: QueryClient): QueryClient {
  queryClient.setQueryData(["whoami"], { event_head: "0", kind: "user", login: READER_LOGIN });
  writesOf(queryClient).signedIn(READER_LOGIN);
  draftStoresSignedIn(queryClient, READER_LOGIN);
  writesOf(queryClient).resume(READER_LOGIN);
  return queryClient;
}

/**
 * Ends the page as a reload does, whoever starts it: the browser's own reload, or the app's after
 * a deploy (`DeploymentResilience`). The page is told it goes (`pagehide`), and what the tab's
 * `sessionStorage` holds then is all the next load of the page finds. `end` takes the whole tree
 * away, and React runs every unmount in it, which a reload never does, so whatever those unmounts
 * write there is put back as the page left it. The answer and reply drafts, which modules hold
 * and so outlive the tree, forget what they hold as the next page's would start without it, and
 * keep what the tab keeps (`DraftStore.reload`), so only what the tab kept comes back.
 */
export function endAsAReload(end: () => void): void {
  act(() => {
    window.dispatchEvent(new Event("pagehide"));
  });
  const left = entryNames(sessionStorage, "").map(
    (name) => [name, sessionStorage.getItem(name) ?? ""] as const
  );
  end();
  draftStoresReloaded();
  for (const name of entryNames(sessionStorage, "")) removeEntry(sessionStorage, name);
  for (const [name, value] of left) sessionStorage.setItem(name, value);
}

/** The reader's state of CORE-1 with nothing read, dismissed or pinned. */
export const NOTHING_READ: UserState = {
  "CORE-1": { dismissed: [], last_read_seq: 0, pinned: false, seq: 0 },
};

/** CORE-1's Conversation tab as the issue page draws it, over `queryClient`. */
export function tab(
  state: UserState,
  visible: boolean,
  queryClient: QueryClient,
  isClosed = false,
  issueArtifacts: ReadonlyMap<string, Artifact> = new Map(),
  focusItemId?: string
): ReactNode {
  return (
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      <KeymapProvider>
        <QueryClientProvider client={queryClient}>
          <ConversationTab
            focusItemId={focusItemId}
            issueArtifacts={issueArtifacts}
            isClosed={isClosed}
            issueKey="CORE-1"
            state={state}
            visible={visible}
          />
        </QueryClientProvider>
      </KeymapProvider>
    </MemoryRouter>
  );
}

/** The margin fixture's thread on CORE-1's spec, and a second one on another mark of it. */
export const threadA: Comment = comment;
export const threadB: Comment = {
  ...comment,
  anchor: comment.anchor === null ? null : { ...comment.anchor, mark_id: "m-2", quote: "fox" },
  body: "and how?",
  id: "comment-2",
};

/** The document the margin's threads are on, registered as the document page registers it. */
export function RegisterDocument(): ReactNode {
  const { registerDocument } = useMargin();
  useEffect(() => {
    registerDocument(stubDocumentBridge(specArtifact.id));
    return () => registerDocument(undefined);
  }, [registerDocument]);
  return null;
}

/** Registers the bridge of the document the route shows, as each document page registers its own:
 *  `second`, CORE-2's spec, while the reader is on CORE-2, and `first`, CORE-1's, on every other
 *  route. A route's bridge registers as its page draws and unregisters as the page goes. */
export function RegisterRouteDocument({
  first,
  second,
}: {
  readonly first: DocumentBridge;
  readonly second: DocumentBridge;
}): ReactNode {
  const { registerDocument } = useMargin();
  const { pathname } = useLocation();
  const bridge = pathname === buildIssuePath({ key: "CORE-2", kind: "issue" }) ? second : first;
  useEffect(() => {
    registerDocument(bridge);
    return () => registerDocument(undefined);
  }, [bridge, registerDocument]);
  return null;
}

/** Thread `id`'s card in the margin's list, looked up afresh, since opening a thread draws its
 *  card again. */
export function marginCard(id: string): HTMLElement {
  return screen.getByTestId(`margin-comment-${id}`);
}

/** Opens margin thread `id` from its collapsed card, as a click on it does. */
export async function openMarginThread(user: UserEvent, id: string): Promise<void> {
  await user.click(within(marginCard(id)).getByRole("button", { expanded: false }));
}

/** A query client for the margin of CORE-1's spec over `comments`: the issue, the reader's
 *  Inbox, asks and state, and the comments read already, as the issue page has them by the time
 *  the margin draws. */
export function marginClient(comments: readonly Comment[]): QueryClient {
  const queryClient = newQueryClient();
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["inbox"], []);
  queryClient.setQueryData(["asks", issue.key], []);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", issue.key], comments);
  return queryClient;
}

/** CORE-1's page with the margin drawn over `queryClient`, and `children` - the document, and the
 *  controls a test presses - inside the margin's provider beside it. */
export function marginPage(queryClient: QueryClient, children: ReactNode): ReactNode {
  return (
    <MemoryRouter initialEntries={[buildIssuePath({ key: issue.key, kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          {children}
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

/** The Inbox's reading column, as `ReadingPane` draws it. */
export function inboxPane(): HTMLElement {
  return screen.getByTestId("inbox-pane");
}

/** The field of the column's box: `Reply` in a comment or message thread, `Comment` in an Agent
 *  row's thread. */
export function inboxBoxField(name: "Reply" | "Comment" = "Reply"): HTMLTextAreaElement {
  return within(inboxPane()).getByRole<HTMLTextAreaElement>("textbox", { name });
}

/** Which note the column shows about a send its box holds. */
export function inboxNote(): Held | "none" {
  return noteIn(inboxPane());
}

/** Puts `BODY` in the column's box, whose field is named `name`, in the state `held` names; a
 *  stopped reply is one the file seeded under the box's send name (`threadSendKey`) before the
 *  column was drawn. */
export async function holdInInboxBox(
  user: UserEvent,
  held: Held,
  name: "Reply" | "Comment" = "Reply"
): Promise<void> {
  const field = await within(
    await screen.findByTestId("inbox-pane")
  ).findByRole<HTMLTextAreaElement>("textbox", { name });
  if (held === "stopped") {
    await waitFor(() => expect(inboxNote()).toBe("stopped"));
    return;
  }
  await user.click(field);
  await user.keyboard("{Control>}a{/Control}");
  await user.paste(BODY);
  if (held === "refused") {
    await user.click(within(inboxPane()).getByRole("button", { name: "Send" }));
    await within(inboxPane()).findByText(REFUSED_NOTE);
  }
}

/** The Inbox reading column a file drew (`drawInboxColumn`). */
export interface InboxColumn {
  /** The query client the column is drawn over now: a reload draws it over a new one. */
  readonly client: QueryClient;
  /** The column moved to another row, as the Inbox moves it: the same column, drawn again. */
  readonly move: (row: MyReplyRow) => void;
  /** Ends the page as a reload does and loads it again over the same tab on `row`, with the
   *  query client the file's `newClient` makes for the next load (`endAsAReload`). */
  readonly reload: (row: MyReplyRow) => void;
  readonly unmount: () => void;
}

/** The Inbox reading column on `row` over `client`, as the Inbox draws it beside its list, for the
 *  reader: inside the margin's provider, over a document runtime for a thread written in a
 *  document. A reload draws it again over `newClient()`, as the next load of the tab makes its own
 *  query client. */
export function drawInboxColumn(
  client: QueryClient,
  row: MyReplyRow,
  newClient: () => QueryClient
): InboxColumn {
  let shownClient = client;
  const fake = fakeDocumentRuntime({ text: "The live document" });
  const tree = (shown: MyReplyRow) => (
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      <QueryClientProvider client={shownClient}>
        <DocumentRuntime.Provider value={fake.runtime}>
          <MarginProvider>
            <ReadingPane
              onAnswered={() => {}}
              onAssignWrite={() => {}}
              onSnoozeLive={() => {}}
              row={{ kind: "thread", thread: shown }}
              viewer={READER_LOGIN}
            />
          </MarginProvider>
        </DocumentRuntime.Provider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  let view = render(tree(row));
  return {
    get client() {
      return shownClient;
    },
    move: (shown) => view.rerender(tree(shown)),
    reload: (shown) => {
      endAsAReload(() => view.unmount());
      shownClient = newClient();
      view = render(tree(shown));
    },
    unmount: () => view.unmount(),
  };
}

/** A running session the Agents page draws a row for, whose composer a test types into. */
export const planner: Agent = {
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
