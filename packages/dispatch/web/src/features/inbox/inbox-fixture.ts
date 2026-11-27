import { afterEach, beforeEach, type Mock, spyOn } from "bun:test";

import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import { graphNode } from "../../__tests__/graph-fixture";
import { messageCorrectionFields } from "../../__tests__/message-fixture";
import { api } from "../../api/client";
import type {
  Agent,
  Comment,
  InboxRow,
  IssueDetails,
  Message,
  MyRepliesResponse,
  MyReplyRow,
} from "../../api/types";
import { plannerAgentRow, replyRow } from "../threads/replies-fixture";

/** The mocks every Inbox render needs: the signed-in login, and the credential requests waiting
 *  on it; an open ask card also reads its owner's subscribers and its backlinks. */
export interface InboxApiMocks {
  whoAmI: Mock<typeof api.whoAmI>;
  getIssueSubscribers: Mock<typeof api.getIssueSubscribers>;
  getArtifactSubscribers: Mock<typeof api.getArtifactSubscribers>;
  getReferences: Mock<typeof api.getReferences>;
  getCredentialPending: Mock<typeof api.getCredentialPending>;
}

/** Installs the shared fixtures `Inbox.test.tsx`, `Inbox.reading-view.test.tsx` and
 *  `InboxDrawer.test.tsx` all build on: call once at module scope. The returned object is stable
 *  across the file's `beforeEach` cycles even though what it holds is replaced each time, so a
 *  test may still override one mock's resolution in place
 *  (`mocks.getCredentialPending.mockResolvedValue(...)`). */
export function installInboxApiMocks(): InboxApiMocks {
  const mocks = {} as InboxApiMocks;
  beforeEach(() => {
    window.localStorage.clear();
    mocks.whoAmI = spyOn(api, "whoAmI").mockResolvedValue({
      event_head: "0",
      kind: "user",
      login: "alice",
    });
    mocks.getCredentialPending = spyOn(api, "getCredentialPending").mockResolvedValue({
      pending: [],
    });
    mocks.getIssueSubscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
    mocks.getArtifactSubscribers = spyOn(api, "getArtifactSubscribers").mockResolvedValue([]);
    mocks.getReferences = spyOn(api, "getReferences").mockResolvedValue({
      edges: [],
      node: graphNode({ id: "", kind: "ask" }),
    });
  });
  afterEach(() => {
    mocks.whoAmI.mockRestore();
    mocks.getCredentialPending.mockRestore();
    mocks.getIssueSubscribers.mockRestore();
    mocks.getArtifactSubscribers.mockRestore();
    mocks.getReferences.mockRestore();
  });
  return mocks;
}

/** The default assignee of the project every fixture row belongs to: the third and last step the
 *  server resolves a question's person through. Nobody's own login, so a row that falls to it is
 *  nobody else's. */
export const FIXTURE_PROJECT_DEFAULT = "carol";

/** Resolves a fixture row's `waiting_on_person` the way the server resolves it
 *  (`coalesce(a.asked_of, i.assignee, p.default_assignee)`): whoever the ask was handed to, else
 *  the assignee of its issue, else `projectDefault`, the default assignee of the project the ask
 *  belongs to - its issue's, or for a question on a project document that document's. A row that
 *  names the field itself is left alone, so a test can say who waits on it outright. Every Inbox
 *  row a test builds goes through this, so no fixture waits on somebody the server would not
 *  name.
 *
 *  Every project names a default, so this release's server resolves a person for every question.
 *  `projectDefault` is `null` only for a row a test wants resolved to nobody, which is what a
 *  server written before that rule answers and what the `unassigned` filter and "Assign to me"
 *  are written against: the field is then absent, as the server sends it.
 */
export function withWaitingOnPerson(
  row: InboxRow,
  projectDefault: string | null = FIXTURE_PROJECT_DEFAULT
): InboxRow {
  if (row.waiting_on_person !== undefined) return row;
  const person = row.asked_of ?? row.issue?.assignee ?? projectDefault;
  return person === null ? row : { ...row, waiting_on_person: person };
}

/** A single open ask on an issue, overridable per test. The person the row waits on is resolved
 *  as the server resolves it (`coalesce(a.asked_of, i.assignee, p.default_assignee)`), so an
 *  override that moves the issue's assignee, or hands the ask to somebody, moves that person
 *  with it, and an issue nobody holds falls to `projectDefault` as the server's third step does;
 *  an override naming `waiting_on_person` itself wins. */
export function issueAsk(
  overrides: Partial<InboxRow> = {},
  projectDefault: string | null = FIXTURE_PROJECT_DEFAULT
): InboxRow {
  const row: InboxRow = {
    anchor: null,
    answer: null,
    artifact_id: null,
    asked_of: null,
    asked_of_at: null,
    asked_of_by: null,
    author: { id: "session-1", kind: "session" },
    block_id: null,
    created_at: "2026-09-11T00:00:00Z",
    edited_at: null,
    id: "ask-a",
    issue: { assignee: "alice", key: "CORE-1", title: "Fix the thing" },
    issue_key: "CORE-1",
    kind: "question",
    last_reply: null,
    multiple: false,
    opened_event_id: 1,
    options: [],
    thread: {
      answers: [],
      claimant: null,
      edits: [],
      followers: [],
      replies: [],
      route: null,
      route_status: null,
      route_holder: null,
      route_status_reason: null,
    },
    question: "Which approach?",
    state: "open",
    waiting_on: "human",
    priority: null,
    snoozed_until: null,
    urgency: "med",
    ...overrides,
  };
  return withWaitingOnPerson(row, projectDefault);
}

/** Resolves `getAsk` for each row's full thread from the same fixtures `getInbox` already seeded. */
export function mockAskReads(rows: readonly InboxRow[]) {
  return spyOn(api, "getAsk").mockImplementation(async (id: string) => {
    const ask = rows.find((row) => row.id === id);
    if (ask === undefined) throw new Error(`no fixture for ${id}`);
    return {
      ask,
      answers: [],
      claimant: null,
      edits: [],
      followers: [],
      replies: [],
      route: null,
      route_status: null,
      route_holder: null,
      route_status_reason: null,
    };
  });
}

/** The width the Inbox shows its reading column from (`READING_PANE_QUERY`, 1280 px). happy-dom
 *  answers `matchMedia` from the window it is given, so the window is the whole fixture: this
 *  widens it to 1440 px, and answers what narrows it back. */
export function widenToReadingPane(): () => void {
  const window_ = window as unknown as {
    happyDOM: { setViewport(size: { width: number }): void };
    innerWidth: number;
  };
  const was = window_.innerWidth;
  window_.happyDOM.setViewport({ width: 1440 });
  return () => window_.happyDOM.setViewport({ width: was });
}

/** One row of the list by its ask id, as the list draws it now: a short row holds no card, so
 *  there is no `ask-<id>` to find it by. */
export function shortRow(askId: string): HTMLElement {
  const row = document.querySelector<HTMLElement>(`[data-inbox-row="${askId}"]`);
  if (row === null) throw new Error(`${askId} row missing`);
  return row;
}

/** The threads whose reply box stands for the thread (`ThreadBody`'s `standing` boxes), each
 *  drawn by a composer of its own kind: a comment thread on an issue, the same on an issue that
 *  is closed, which still takes a comment, an issue's message conversation, and an Agent row's
 *  one-off message to a live session. */
export const REPLY_BOX_THREADS = [
  "a comment thread",
  "a comment thread on a closed issue",
  "a message thread",
  "an Agent row's thread",
] as const;

/** The session an Agent row's thread was sent to, live and taking a message in either mode. */
const LIVE_PLANNER: Agent = {
  capabilities: ["steer", "btw"],
  dir: "/w",
  host: "m",
  last_activity: null,
  last_seen: Date.now(),
  machine_id: "m",
  open_asks: 0,
  roles: [],
  session_id: "planner",
  state: "live",
  title: "Planner",
};

/** A message the reader sent on CORE-1, or, with `overrides`, another message. */
function fixtureMessage(id: string, body: string, overrides: Partial<Message> = {}): Message {
  return {
    author: { id: "alice", kind: "user" },
    body,
    broadcast_id: null,
    created_at: "2026-10-12T00:00:00Z",
    deliveries: [],
    ...messageCorrectionFields(),
    id,
    in_reply_to: null,
    issue_key: "CORE-1",
    reply_block: null,
    target: null,
    ...overrides,
  };
}

/** A comment the reader wrote on CORE-1, or, with `overrides`, another comment. */
function fixtureComment(id: string, body: string, overrides: Partial<Comment> = {}): Comment {
  return {
    anchor: null,
    ask_id: null,
    author: { id: "alice", kind: "user" },
    body,
    created_at: "2026-10-12T00:00:00Z",
    edited_at: null,
    id,
    issue_key: "CORE-1",
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    turn: null,
    ...commentDeliveryFields(),
    ...overrides,
  };
}

/** The Inbox's thread read holding `row` alone. */
function threadsOf(row: MyReplyRow): MyRepliesResponse {
  return { as_of: "2026-10-12T02:00:00Z", limit: 200, offset: 0, rows: [row], total: 1 };
}

/** Serves the Inbox one thread of `kind` (`REPLY_BOX_THREADS`) and no question, with every read
 *  its body draws its reply box from: the issue and its comments or messages, or the Agent row's
 *  conversation and its session in the live list. Answers the thread's row id and what puts the
 *  reads back. */
export function serveReplyBoxThread(kind: (typeof REPLY_BOX_THREADS)[number]): {
  readonly id: string;
  readonly restore: () => void;
} {
  const spies: { mockRestore(): void }[] = [spyOn(api, "getInbox").mockResolvedValue([])];
  const restore = () => {
    for (const spy of spies) spy.mockRestore();
  };
  if (kind === "an Agent row's thread") {
    spies.push(
      spyOn(api, "listMyReplies").mockResolvedValue(threadsOf(plannerAgentRow())),
      spyOn(api, "listAgents").mockResolvedValue([LIVE_PLANNER]),
      spyOn(api, "listAgentMessages").mockResolvedValue([
        {
          message: fixtureMessage("message-1", "Have a look at the build", {
            issue_key: null,
            target: "session:planner",
          }),
          replies: [
            fixtureMessage("reply-1", "Looked at it.", {
              author: { id: "planner", kind: "session" },
              created_at: "2026-10-12T01:00:00Z",
              in_reply_to: "message-1",
              issue_key: null,
              target: "session:planner",
            }),
          ],
        },
      ])
    );
    return { id: "agent:message-1", restore };
  }
  const closedAt = kind === "a comment thread on a closed issue" ? "2026-10-12T01:30:00Z" : null;
  spies.push(
    spyOn(api, "getIssue").mockResolvedValue({ closed_at: closedAt } as unknown as IssueDetails)
  );
  const reply = {
    author: { id: "s1", kind: "session" },
    created_at: "2026-10-12T01:00:00Z",
  } as const;
  if (kind === "a message thread") {
    spies.push(
      spyOn(api, "listMyReplies").mockResolvedValue(
        threadsOf(replyRow("m1", { kind: "message", ref: "/issues/CORE-1?message=m1" }))
      ),
      spyOn(api, "listIssueMessages").mockResolvedValue([
        {
          message: fixtureMessage("m1", "Who owns the reply rollout?"),
          replies: [
            fixtureMessage("m1-reply", "Noted, I will look.", { ...reply, in_reply_to: "m1" }),
          ],
        },
      ])
    );
    return { id: "reply:m1", restore };
  }
  spies.push(
    spyOn(api, "listMyReplies").mockResolvedValue(threadsOf(replyRow("c1"))),
    spyOn(api, "listComments").mockResolvedValue([
      fixtureComment("c1", "Who owns the reply rollout?"),
      fixtureComment("c1-reply", "Noted, I will look.", { ...reply, reply_to: "c1" }),
    ])
  );
  return { id: "reply:c1", restore };
}
