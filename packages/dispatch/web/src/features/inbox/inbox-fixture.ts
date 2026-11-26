import { afterEach, beforeEach, type Mock, spyOn } from "bun:test";
import type { OpenAsksResponse } from "@legion/contracts";
import type { QueryClient } from "@tanstack/react-query";

import { graphNode } from "../../__tests__/graph-fixture";
import { api } from "../../api/client";
import { meInboxQuery, openAsksKey, openAsksQuery } from "../../api/queries";
import type { MeInboxEntry, MeInboxResponse, MyReplyRow, OpenAskRow } from "../../api/types";
import { askProject, askWaitingOnPerson } from "./list-row";
import { meInboxAskEntry } from "./one-read-cache";

/** The mocks every Inbox render needs: the signed-in login, the credential requests waiting on
 *  it, and the one read, which the page draws the reader's own rows from (`answerOneRead`; empty
 *  until a test says what it holds); an open ask card also reads its owner's subscribers and its
 *  backlinks. */
export interface InboxApiMocks {
  whoAmI: Mock<typeof api.whoAmI>;
  getIssueSubscribers: Mock<typeof api.getIssueSubscribers>;
  getArtifactSubscribers: Mock<typeof api.getArtifactSubscribers>;
  getReferences: Mock<typeof api.getReferences>;
  getCredentialPending: Mock<typeof api.getCredentialPending>;
  getMeInbox: Mock<typeof api.getMeInbox>;
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
    mocks.getMeInbox = answerOneRead({});
  });
  afterEach(() => {
    mocks.whoAmI.mockRestore();
    mocks.getCredentialPending.mockRestore();
    mocks.getIssueSubscribers.mockRestore();
    mocks.getArtifactSubscribers.mockRestore();
    mocks.getReferences.mockRestore();
    mocks.getMeInbox.mockRestore();
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
  row: OpenAskRow,
  projectDefault: string | null = FIXTURE_PROJECT_DEFAULT
): OpenAskRow {
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
  overrides: Partial<OpenAskRow> = {},
  projectDefault: string | null = FIXTURE_PROJECT_DEFAULT
): OpenAskRow {
  const row: OpenAskRow = {
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

/** Resolves `getAsk` for each row's full thread from the same fixtures the open asks' read
 *  answers. */
export function mockAskReads(rows: readonly OpenAskRow[]) {
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

/** The `as_of` every answer of the one read these fixtures make carries: the read mark Mark all
 *  read sends from it. */
export const ONE_READ_AS_OF = "2026-10-12T02:00:00Z";

/** The entry the one read (`GET /api/v1/me/inbox`) lists a row of the Inbox by: an open ask that
 *  waits on the reader by the ask itself, a Reply row by its thread, and an Agent row by its
 *  conversation with the session, each at the moment it last moved. */
export function oneReadEntry(row: OpenAskRow | MyReplyRow): MeInboxEntry {
  if (!("thread_id" in row)) return meInboxAskEntry(row);
  const moment = row.reply.created_at;
  if (row.kind === "agent") {
    return {
      agent: row as MeInboxEntry["agent"] & MyReplyRow,
      moment,
      node: { id: row.thread_id, kind: "message" },
      relations: [],
    };
  }
  return { moment, node: { id: row.thread_id, kind: row.kind }, relations: [], thread: row };
}

/** What the one read holds for its reader: the open asks that wait on them, their Reply and
 *  Agent rows in the Inbox, read or not, and the rows they dismissed out of it. */
export interface OneReadHeld {
  readonly asks?: readonly OpenAskRow[];
  readonly dismissed?: readonly MyReplyRow[];
  readonly threads?: readonly MyReplyRow[];
}

/** One answer of the one read listing `rows`, in that order, for `person`. */
export function oneReadAnswer(
  rows: readonly (OpenAskRow | MyReplyRow)[],
  { asOf = ONE_READ_AS_OF, person = "alice" }: { asOf?: string; person?: string } = {}
): MeInboxResponse {
  const entries = rows.map(oneReadEntry);
  return { as_of: asOf, entries, horizon: 1, limit: 0, offset: 0, person, total: entries.length };
}

/** The Dismissed view's cached read (`meInboxDismissedQuery`) as one settled page listing `rows`. */
export function oneReadDismissedPages(rows: readonly MyReplyRow[]) {
  return { pageParams: [undefined], pages: [oneReadAnswer(rows)] };
}

/**
 * What the one read answers a read of `query` with while Dispatch holds `held`: `state=inbox`
 * (`meInboxQuery`) every ask and every thread row; `state=unread` the unread thread rows, those
 * of one issue or document where `issue` or `artifact` narrows it (`meInboxOwnerQuery`, the
 * document's by the thread's anchor), as many as `limit` names with every one counted in
 * `total`, so the count's one-entry page (`meInboxCountQuery`) reads how many are unread; and
 * `state=dismissed` (`meInboxDismissedQuery`) the dismissed rows, on one page.
 */
export function oneReadReply(
  query: {
    readonly artifact?: string;
    readonly issue?: string;
    readonly limit?: number;
    readonly state?: string;
  },
  held: OneReadHeld,
  { asOf = ONE_READ_AS_OF, person = "alice" }: { asOf?: string; person?: string } = {}
): MeInboxResponse {
  const threads = held.threads ?? [];
  if (query.state === "unread") {
    const unread = threads.filter(
      (thread) =>
        thread.unread &&
        (query.issue === undefined || thread.issue?.key === query.issue) &&
        (query.artifact === undefined || thread.anchor?.artifact_id === query.artifact)
    );
    const page = query.limit === undefined ? unread : unread.slice(0, query.limit);
    return {
      ...oneReadAnswer(page, { asOf, person }),
      limit: query.limit ?? 0,
      total: unread.length,
    };
  }
  if (query.state === "dismissed") return oneReadAnswer(held.dismissed ?? [], { asOf, person });
  return oneReadAnswer([...(held.asks ?? []), ...threads], { asOf, person });
}

/** Answers `api.getMeInbox` as Dispatch answers each of the page's reads of it (`oneReadReply`)
 *  from what `held` holds, read again at each call, so a test that moves what the server holds
 *  moves what the next read answers. */
export function answerOneRead(
  held: OneReadHeld | (() => OneReadHeld | Promise<OneReadHeld>),
  options: { asOf?: string; person?: string } = {}
): Mock<typeof api.getMeInbox> {
  return spyOn(api, "getMeInbox").mockImplementation(async (query = {}) =>
    oneReadReply(query, typeof held === "function" ? await held() : held, options)
  );
}

/** `GET /api/v1/asks/open`'s answer around `rows`, with the counts the read carries beside them.
 *  The page reads each row's Inbox ask fields alone (`openAsksQuery`), so the rows are answered as
 *  those, and a test comparing what the page cached with its fixture rows compares like with
 *  like. */
export function openAsksAnswer(rows: readonly OpenAskRow[]): OpenAsksResponse {
  return {
    as_of: ONE_READ_AS_OF,
    asks: [...rows] as OpenAsksResponse["asks"],
    count: rows.length,
    opened_since: false,
    session_id: "",
    waiting_on_agent: rows.filter((row) => row.waiting_on === "agent").length,
    waiting_on_human: rows.filter((row) => row.waiting_on === "human").length,
  };
}

/** Answers `api.listOpenAsks` as Dispatch answers each read of it from `rows`, read again at each
 *  call, so a test that moves what the server holds moves what the next read answers: `project`
 *  keeps that project's asks, and `waiting_on` those that wait on that person
 *  (`askWaitingOnPerson`). */
export function answerOpenAsks(
  rows: readonly OpenAskRow[] | (() => readonly OpenAskRow[] | Promise<readonly OpenAskRow[]>)
): Mock<typeof api.listOpenAsks> {
  return spyOn(api, "listOpenAsks").mockImplementation(async (query = {}) => {
    const held = typeof rows === "function" ? await rows() : rows;
    return openAsksAnswer(
      held.filter(
        (row) =>
          (query.project === undefined || askProject(row) === query.project) &&
          (query.waiting_on === undefined || askWaitingOnPerson(row) === query.waiting_on)
      )
    );
  });
}

/**
 * The page's two reads of the open asks, answered from one list of rows as Dispatch answers
 * them: the read of the open asks (`GET /api/v1/asks/open`) every one of `rows`, and the one
 * read, which the page draws the reader's own rows from, those of them that wait on `viewer`
 * (`askWaitingOnPerson`), with `threads` as its Reply and Agent rows. `rows` may be a function,
 * read at each call, for a test that moves what the server holds. Answers the open asks' mock;
 * the one read's is the same spy each call to `answerOneRead` makes, which the file's own
 * restore of it - `installInboxApiMocks`' or the test's - puts back.
 */
export function mockInboxReads(
  rows: readonly OpenAskRow[] | (() => readonly OpenAskRow[] | Promise<readonly OpenAskRow[]>),
  { threads = [], viewer = "alice" }: { threads?: readonly MyReplyRow[]; viewer?: string } = {}
): Mock<typeof api.listOpenAsks> {
  const held = async () => (typeof rows === "function" ? await rows() : rows);
  answerOneRead(
    async () => ({
      asks: (await held()).filter((row) => askWaitingOnPerson(row) === viewer),
      threads,
    }),
    { person: viewer }
  );
  return answerOpenAsks(held);
}

/** Lands a new answer of the page's two reads of the open asks in `queryClient`, as a refresh
 *  brings what Dispatch holds now: the read of the open asks every one of `rows`, and the one
 *  read those of them that wait on `viewer`, beside the Reply and Agent rows it already holds. */
export function landInboxReads(
  queryClient: QueryClient,
  rows: readonly OpenAskRow[],
  viewer = "alice"
): void {
  queryClient.setQueryData<OpenAskRow[]>(openAsksQuery().queryKey, [...rows]);
  queryClient.setQueryData<MeInboxResponse>(meInboxQuery().queryKey, (held) => {
    const mine = rows.filter((row) => askWaitingOnPerson(row) === viewer);
    const threads = (held?.entries ?? []).filter((entry) => entry.ask === undefined);
    const answer = oneReadAnswer(mine, { asOf: held?.as_of, person: viewer });
    const entries = [...(answer.entries ?? []), ...threads];
    return { ...answer, entries, total: entries.length };
  });
}

/** Reads the page's two reads of the open asks again, as a refresh does: what Dispatch holds
 *  now reaches the page through both, the reader's own rows through the one read. */
export async function refetchInboxReads(queryClient: QueryClient): Promise<void> {
  await Promise.all([
    queryClient.refetchQueries({ queryKey: openAsksKey }),
    queryClient.refetchQueries({ queryKey: meInboxQuery().queryKey }),
  ]);
}
