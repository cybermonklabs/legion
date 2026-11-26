import { afterAll, beforeEach, describe, expect, test } from "bun:test";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { bearerCredential } from "../bearer";
import { executeDispatchTool, resetAdviceMemory } from "../dispatch-execute";

// `dispatch inbox` against a Dispatch of the release before (`inboxReadOfTheReleaseBefore`). The
// plugins release from main as this merges, and Dispatch's own server is deployed by hand later,
// so for a while the command runs against a server that has no route for the one read,
// `GET /api/v1/me/inbox`. It then makes the two reads it made before and prints what it printed
// then: the same lines it prints from the one read, which `dispatch-inbox.test.ts` pins.
//
// This file answers those two reads as a server of the release before answers them, so the
// acceptance driver's search for callers of the reads this release replaced spares it by its path
// (`packages/dispatch/e2e/acceptance/one-inbox-callers.sh`, `callers_spared`), and the root's pull
// request deletes it with the fallback (`root-deletes.txt`, `dispatch-inbox-fallback`).

const cwd = temporaryDirectory("dispatch-inbox-release-before");

/** The Dispatch bearer the command is handed. */
const TOKEN = fixtureSecret("dsp");

const VIEWER = "alice@example.com";
const HUMAN_ASK = "11111111-1111-4111-8111-111111111111";
const AGENT_ASK = "55555555-5555-4555-8555-555555555555";
const THREAD = "66666666-6666-4666-8666-666666666666";

/** An ask row, with the fields the summary reads; the thread and the rest of an ask ride along in
 *  the real answer and are not read here. */
function askRow(row: Record<string, unknown>): Record<string, unknown> {
  return {
    author: { kind: "session", id: "ses-9" },
    kind: "question",
    options: [],
    multiple: false,
    urgency: "med",
    anchor: null,
    state: "open",
    answer: null,
    opened_event_id: 1,
    created_at: "2026-11-01T00:00:00Z",
    edited_at: null,
    priority: null,
    snoozed_until: null,
    thread: { replies: [], edits: [], answers: [], followers: [] },
    ...row,
  };
}

/** The person's asks: one that waits on them, and one on a project document an agent owes. */
const ASKS = [
  askRow({
    id: HUMAN_ASK,
    issue_key: "DSP-1",
    issue: { key: "DSP-1", title: "Native workspace", assignee: VIEWER },
    priority: 1,
    question: "Which store do we keep?",
    waiting_on: "human",
  }),
  askRow({
    id: AGENT_ASK,
    issue_key: null,
    document: { project: "CORE", slug: "design-notes", name: "design.md" },
    question: "Is this still the plan?",
    waiting_on: "agent",
  }),
];

/** The person's one unread thread, its newest reply five and a half minutes before `now`, so its
 *  age reads `5m` whichever of the reads answers it. */
function threadRow(now: number): Record<string, unknown> {
  return {
    kind: "comment",
    thread_id: THREAD,
    ref: `/issues/DSP-6/comments/${THREAD}`,
    subject: "The staging box lost its disk again",
    issue: { key: "DSP-6", title: "Staging keeps falling over" },
    priority: 2,
    project: "DSP",
    read_through: new Date(now - 30 * 60_000).toISOString(),
    unread: true,
    unread_count: 1,
    reply: {
      id: "99999999-9999-4999-8999-999999999999",
      kind: "reply",
      author: { kind: "user", id: "bob@example.com" },
      first_line: "I swapped the disk this morning.",
      created_at: new Date(now - 5 * 60_000 - 30_000).toISOString(),
    },
  };
}

/** What the stub answers each read with: the one read, or a router with no route for it and the
 *  two reads of the release before; and, beside either, the one read's own refusal of a
 *  credential without the person scope. */
type Server = "current" | "release-before" | "scope-refused";
let serving: Server = "current";
const requests: string[] = [];

const stub = Bun.serve({
  port: 0,
  fetch(request) {
    const { pathname, search, searchParams } = new URL(request.url);
    requests.push(`${request.method} ${pathname}${search}`);
    const now = Date.now();
    if (pathname === "/api/v1/whoami") {
      return Response.json({ kind: "agent", owner: VIEWER, service: null, scopes: ["person"] });
    }
    // A Dispatch with no secrets broker, which has no credential requests.
    if (pathname === "/api/v1/credential-requests") return Response.json(null);
    if (pathname === "/api/v1/me/inbox") {
      if (serving === "release-before") {
        // A router with no route for the read answers it so, in the words the client reads as
        // an answer of no route (`isNoRoute`).
        return Response.json(
          { code: "NOT_FOUND", error: `no route for ${request.method} ${pathname}` },
          { status: 404 }
        );
      }
      if (serving === "scope-refused") {
        return Response.json(
          {
            code: "OAUTH_SCOPE_REQUIRED",
            error:
              "this request's credential does not hold the person scope, which this route requires",
          },
          { status: 403 }
        );
      }
      const entries =
        searchParams.get("state") === "inbox"
          ? ASKS.map((ask) => ({
              ask,
              moment: ask.created_at,
              node: { id: ask.id, kind: "ask" },
              relations: [],
            }))
          : [
              {
                thread: threadRow(now),
                moment: new Date(now).toISOString(),
                node: { id: THREAD, kind: "comment" },
                relations: [],
              },
            ];
      return Response.json({
        person: VIEWER,
        as_of: new Date(now).toISOString(),
        horizon: 1,
        total: entries.length,
        limit: 0,
        offset: 0,
        entries,
      });
    }
    // The release before answers the asks that wait on the caller as a list of the rows
    // themselves, and the unread threads as a page of rows, unpaged as the command asks for them.
    if (serving === "release-before" && pathname === "/api/v1/inbox") return Response.json(ASKS);
    if (serving === "release-before" && pathname === "/api/v1/me/replies") {
      return Response.json({
        rows: [threadRow(now)],
        total: 1,
        limit: 0,
        offset: 0,
        as_of: new Date(now).toISOString(),
      });
    }
    return Response.json({ error: `unexpected ${request.method} ${pathname}` }, { status: 500 });
  },
});

afterAll(() => stub.stop(true));

beforeEach(() => {
  serving = "current";
  requests.length = 0;
  resetAdviceMemory();
});

/** `dispatch inbox` as the tool runs it against the stub. */
function runInbox() {
  return executeDispatchTool({
    tool: "dispatch_inbox",
    args: {},
    cwd,
    host: "omp",
    sessionId: "ses-1",
    config: { url: `http://127.0.0.1:${stub.port}`, credential: bearerCredential(TOKEN) },
    env: {},
  });
}

describe("dispatch inbox against a Dispatch of the release before", () => {
  test("makes the two reads of the release before and prints what it prints from the one read", async () => {
    const current = await runInbox();
    serving = "release-before";
    requests.length = 0;

    const before = await runInbox();

    expect(before.text).toBe(current.text);
    expect(before.text.split("\n")).toEqual([
      "Needs you 1",
      `- DSP-1 · P1 · question · Which store do we keep? · dispatch://DSP-1/ask/${HUMAN_ASK}`,
      "",
      "Waiting on agents 1",
      `- design-notes · P- · question · Is this still the plan? · dispatch://CORE/artifact/design-notes/ask/${AGENT_ASK}`,
      "",
      "Later 0",
      "",
      "Unread threads 1",
      `- DSP-6 · comment · The staging box lost its disk again · user bob@example.com · 5m · /issues/DSP-6/comments/${THREAD}`,
      "",
      "Credential requests 0",
    ]);
    expect(before.warnings).toBeUndefined();
    expect(before.details).toMatchObject({ asks: 2, unread_threads: 1, credential_requests: 0 });
    // The one read, asked once in each of its two states, since the command waits for no roll
    // before it makes the two reads it made before: each of the six requests is sent once.
    expect([...requests].sort()).toEqual([
      "GET /api/v1/credential-requests?approver=me",
      "GET /api/v1/inbox?assignee=me",
      "GET /api/v1/me/inbox?state=inbox",
      "GET /api/v1/me/inbox?state=unread",
      "GET /api/v1/me/replies?kind=reply%2Cagent",
      "GET /api/v1/whoami",
    ]);
  });

  test("any other refusal of the one read is the command's, and it makes neither read of the release before", async () => {
    serving = "scope-refused";

    await expect(runInbox()).rejects.toThrow("OAUTH_SCOPE_REQUIRED");
    // The command's refusal comes with the one read's, before its other reads need have
    // answered, so only the read it refused with is sure to have been asked; neither read of
    // the release before is asked at all.
    expect(requests).toContain("GET /api/v1/me/inbox?state=inbox");
    expect(
      requests.filter(
        (line) => line.startsWith("GET /api/v1/inbox") || line.startsWith("GET /api/v1/me/replies")
      )
    ).toEqual([]);
  });
});
