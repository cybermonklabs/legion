/**
 * What the draft matrix files (`draft-matrix.*.test.tsx`, INFRA-1503) share: the refusal their
 * writes get and the notes a composer shows about a send it holds, the draft a cell writes and the
 * states it holds it in, the lines a cell reads to tell whether a draft is kept, the comments the
 * Conversation's hosts answer, the Conversation page as each Conversation host draws it (and as
 * `ConversationTab.test.tsx` draws it), the margin's page over two threads, the Planner session
 * an Agents row draws, and the reload a reload row makes. A stopped send is seeded with
 * `stop-late-write.ts`.
 */
import { spyOn } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, screen, within } from "@testing-library/react";
import type { UserEvent } from "@testing-library/user-event";
import { type ReactNode, useEffect } from "react";
import { MemoryRouter } from "react-router-dom";

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
  UserState,
} from "../api/types";
import { ConversationTab } from "../features/conversation/ConversationTab";
import { unsentDraftsOf } from "../features/conversation/unsent-drafts";
import { answerDraftsSignedIn } from "../features/inbox/answer-drafts";
import { replyDraftsSignedIn } from "../features/inbox/reply-drafts";
import { Margin } from "../features/margin/Margin";
import { MarginProvider, useMargin } from "../features/margin/margin-context";
import {
  comment,
  issue,
  specArtifact,
  stubDocumentBridge,
} from "../features/margin/margin-fixture";
import { buildIssuePath } from "../features/refs/routes";
import { KeymapProvider } from "../features/shell/KeymapProvider";
import { SendStoppedError } from "../features/writes/keyed-write";
import { writesOf } from "../features/writes/write-store";
import { commentDeliveryFields } from "./comment-fixture";
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
 * store and its unsent drafts, over the same tab.
 */
export function signedInAsReader(queryClient: QueryClient): QueryClient {
  queryClient.setQueryData(["whoami"], { event_head: "0", kind: "user", login: READER_LOGIN });
  writesOf(queryClient).signedIn(READER_LOGIN);
  unsentDraftsOf(queryClient).signedIn(READER_LOGIN);
  answerDraftsSignedIn(READER_LOGIN);
  replyDraftsSignedIn(READER_LOGIN);
  writesOf(queryClient).resume(READER_LOGIN);
  return queryClient;
}

/**
 * Ends the page as a reload does, whoever starts it: the browser's own reload, or the app's after
 * a deploy (`DeploymentResilience`). The page is told it goes (`pagehide`), and what the tab's
 * `sessionStorage` holds then is all the next load of the page finds. `end` takes the whole tree
 * away, and React runs every unmount in it, which a reload never does, so whatever those unmounts
 * write there is put back as the page left it.
 */
export function endAsAReload(end: () => void): void {
  act(() => {
    window.dispatchEvent(new Event("pagehide"));
  });
  const names = Array.from({ length: sessionStorage.length }, (_, index) =>
    sessionStorage.key(index)
  ).filter((name): name is string => name !== null);
  const left = names.map((name) => [name, sessionStorage.getItem(name) ?? ""] as const);
  end();
  sessionStorage.clear();
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
