/** The Inbox the search rows are read against: eight questions across two projects, three agent
 *  authors, two assignees and one project document, plus one pending credential request.
 *
 *  Two spec files share it. `inbox-search.stub.e2e.ts` holds the rows that read only what the
 *  page draws from that one answer; `inbox-search.e2e.ts` holds the one row whose claim is about
 *  a write in flight.
 *
 *  Every name here is made up: this repository is public, so nothing names a real person, host or
 *  project. The seed is built from `e2e/api.ts`, `e2e/seed.ts` and `e2e/users.ts` alone (plus the
 *  fake broker's one credential request, which no Dispatch write can seed).
 */
import { expect, type Locator, type Page } from "@playwright/test";

import {
  createArtifactAsk,
  createAsk,
  createComment,
  createIssue,
  createProject,
  createProjectDocument,
  getAsk,
  mintAgentToken,
  patchIssue,
} from "./api";
import { setPendingCredentialRequests } from "./fake-broker-helpers";

// ---------------------------------------------------------------------------
// The three agents whose asks these files list. Each word of a title and of a
// session id belongs to that author alone, so a narrowing that keeps a row
// proves which field carried it.
// ---------------------------------------------------------------------------

/** The agent that wrote four of the questions; its title is two words nothing else holds. */
export const PLANNER = {
  actor: {
    id: "e2e-nightly-planner",
    kind: "session" as const,
    origin: { session_title: "Nightly planner" },
  },
  as: "agent" as const,
};

/** The agent that wrote the two questions on the retry budget. */
export const AUDITOR = {
  actor: {
    id: "e2e-morning-auditor",
    kind: "session" as const,
    origin: { session_title: "Morning auditor" },
  },
  as: "agent" as const,
};

/** The session id of the agent whose write carries an owner: a personal token mints that owner,
 *  so the ask records `author.owner`, which no body may set (`bearerSessionActor`). */
const RUNNER_SESSION = "e2e-sweep-runner";

/** A session id no ask names, for the agent filter's own empty state. */
export const SILENT_SESSION = "e2e-silent-session";

/** The address every row but the drawer's reads: the cleared assignee filter, so the question on
 *  the project document - which nobody holds, a document having no assignee - and the unassigned
 *  issue's question are listed beside the four Alice holds. */
export const INBOX = "/?assignee=everyone";

/** The pending credential request the fake broker lists for Alice throughout, so the empty state
 *  the typed text replaces is the silent one a pending request leaves behind. */
const CREDENTIAL_RECORD = "record-search-alice";

export interface Seed {
  /** `LEDGER-2`, third question: "Does the budget reset after a successful write?" */
  budgetAskId: string;
  /** `LEDGER-1`, the only row whose title holds `clock` and `drift`. */
  clockAskId: string;
  /** `MIRROR-2`, the unassigned issue's question, by the agent whose owner is bob. */
  leaseAskId: string;
  /** How many rows the seeded Inbox lists at `INBOX`. */
  listed: number;
  /** `MIRROR-1`, the one row waiting on its agent. */
  replayAskId: string;
  /** `LEDGER-2`, first question: the only row holding both `ledger` and `lease`. */
  retryAskId: string;
  /** The question on the project document `Mirror runbook`, which has no issue. */
  runbookAskId: string;
  /** `LEDGER-2`, second question. */
  wakeAskId: string;
  /** `MIRROR-1`, second question. */
  windowAskId: string;
}

/**
 * Eight questions across two projects, three agent authors, two assignees and one project
 * document, plus one pending credential request.
 *
 * Alice holds `LEDGER-1` and `LEDGER-2` (four questions), Bob holds `MIRROR-1` (two), nobody
 * holds `MIRROR-2` (one, so its row offers "Assign to me"), and the project document's question
 * is nobody's. One of Bob's waits on its agent, because Alice replied to it: `?section=needs-you`
 * has a row to drop.
 */
export async function seedInbox(): Promise<Seed> {
  await createProject({ key: "LEDGER", name: "Ledger service" });
  await createProject({ key: "MIRROR", name: "Mirror service" });
  // The owner a session's ask carries is the owner of the credential that wrote it, never a
  // field of the body, so bob's own token is what puts `bob` in that row's author.
  const bobsToken = await mintAgentToken("search-seed", "bob");
  const runner = {
    actor: {
      id: RUNNER_SESSION,
      kind: "session" as const,
      origin: { session_title: "Sweep runner" },
    },
    as: "agent" as const,
    token: bobsToken,
  };

  const clockIssue = await createIssue({ project: "LEDGER", title: "Clock drift" });
  await patchIssue(clockIssue.key, { priority: 0 });
  const clock = await createAsk(
    clockIssue.key,
    { question: "Which source of truth does the ledger read at startup?" },
    PLANNER
  );

  const retryIssue = await createIssue({ project: "LEDGER", title: "Retry budget" });
  await patchIssue(retryIssue.key, { priority: 1 });
  const retry = await createAsk(
    retryIssue.key,
    { question: "How many attempts before the lease is dropped?" },
    AUDITOR
  );
  const wake = await createAsk(
    retryIssue.key,
    { question: "Should a refused attempt page the operator when the drift widens?" },
    AUDITOR
  );
  const budget = await createAsk(
    retryIssue.key,
    { question: "Does the budget reset after a successful write?" },
    PLANNER
  );

  const replayIssue = await createIssue(
    { project: "MIRROR", title: "Replay window" },
    {
      login: "bob",
    }
  );
  expect(replayIssue.assignee).toBe("bob");
  await patchIssue(replayIssue.key, { priority: 2 });
  const replay = await createAsk(
    replayIssue.key,
    { question: "Does a cold start need a second pass?" },
    PLANNER
  );
  const window = await createAsk(
    replayIssue.key,
    { question: "Which window does the mirror replay first?" },
    PLANNER
  );
  // A person's reply hands the turn to the agent (`askReplyTurn`), which is the one row
  // `?section=needs-you` drops.
  await createComment(
    replayIssue.key,
    { ask_id: replay.id, body: "Cold start from which snapshot?" },
    { login: "alice" }
  );
  await expect.poll(() => getAsk(replay.id)).toMatchObject({ ask: { waiting_on: "agent" } });

  // The shared token holds nobody, so this issue is unassigned and its row offers "Assign to me".
  const leaseIssue = await createIssue(
    { project: "MIRROR", title: "Lease handover" },
    { as: "agent" }
  );
  expect(leaseIssue.assignee).toBeNull();
  await patchIssue(leaseIssue.key, { priority: 3 });
  const lease = await createAsk(
    leaseIssue.key,
    { question: "Who holds the lease during a cold handover?" },
    runner
  );
  expect(lease.author).toMatchObject({ owner: "bob" });

  const document = await createProjectDocument("MIRROR", {
    content: "# Mirror runbook\n\nThe evening sweep replays what the ledger wrote.\n",
    name: "Mirror runbook",
  });
  const runbook = await createArtifactAsk(
    document.artifact.id,
    { question: "Who signs off the evening checklist?" },
    PLANNER
  );

  await setPendingCredentialRequests([
    {
      approve_url: "https://secrets.internal.example/approve/search",
      approver: "alice",
      identifiers: ["LEDGER_API_KEY"],
      kind: "agent_secret",
      record_id: CREDENTIAL_RECORD,
      requested_at: new Date().toISOString(),
      session: { enrollment: null, request: null },
    },
  ]);

  return {
    budgetAskId: budget.id,
    clockAskId: clock.id,
    leaseAskId: lease.id,
    listed: 8,
    replayAskId: replay.id,
    retryAskId: retry.id,
    runbookAskId: runbook.id,
    wakeAskId: wake.id,
    windowAskId: window.id,
  };
}

/** Every row the list is drawing, at either width. */
export function rows(page: Page): Locator {
  return page.locator("[data-inbox-row]");
}

/** The one line the list shows in place of its bands when no band has a row. */
export function emptyState(page: Page): Locator {
  return page.getByRole("region", { name: "Inbox empty state" });
}

/** Opens the Inbox and waits until the seeded list has been drawn, so a count taken right after
 *  a narrowing cannot be the count of a list that had not arrived yet. */
export async function openInbox(page: Page, listed: number, address = INBOX): Promise<void> {
  await page.goto(address);
  await expect(rows(page)).toHaveCount(listed);
}
