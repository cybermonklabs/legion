import { afterEach, beforeEach, type Mock, spyOn } from "bun:test";

import { graphNode } from "../../__tests__/graph-fixture";
import { api } from "../../api/client";
import type { InboxRow } from "../../api/types";

/** The mocks every Inbox render needs: the signed-in login, and the credential requests waiting
 *  on it; an open ask card also reads its owner's subscribers and its backlinks. */
export interface InboxApiMocks {
  whoAmI: Mock<typeof api.whoAmI>;
  getIssueSubscribers: Mock<typeof api.getIssueSubscribers>;
  getArtifactSubscribers: Mock<typeof api.getArtifactSubscribers>;
  getReferences: Mock<typeof api.getReferences>;
  getCredentialPending: Mock<typeof api.getCredentialPending>;
}

/** Installs the shared fixtures `Inbox.test.tsx` and `InboxDrawer.test.tsx` both build on: call
 *  once at module scope. The returned object is stable across the file's `beforeEach` cycles even
 *  though what it holds is replaced each time, so a test may still override one mock's resolution
 *  in place (`mocks.getCredentialPending.mockResolvedValue(...)`). */
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
