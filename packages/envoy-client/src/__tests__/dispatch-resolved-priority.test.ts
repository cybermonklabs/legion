import { afterAll, beforeEach, describe, expect, test } from "bun:test";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { runDispatchCli } from "../dispatch-cli";
import { resetAdviceMemory } from "../dispatch-execute";

// The priority an issue takes from the issues above it, as the `dispatch` command prints it
// (dispatch://INFRA-5793, J1). An issue with no priority of its own takes the priority of the
// nearest issue above it that has one: Dispatch answers it as `resolved_priority`, with the key of
// the issue it comes from as `priority_from` (null where the priority is the issue's own or there
// is none), beside `priority`, which stays the issue's own. Every line that prints a priority
// prints the resolved one and, where it is inherited, the issue it comes from: `P0 (from ACME-1)`.
// A Dispatch older than the walk sends neither field, and the command then prints the issue's own
// priority, as it did before.
//
// One stub Dispatch serves the whole file, answering as the server answers, and each row drives the
// real CLI through `runDispatchCli` and reads what it printed, in the shape of
// `priorities-command.test.ts`.

const makeDirectory = temporaryDirectories("dispatch-resolved-priority");

/** The Dispatch bearer each command is handed. */
const TOKEN = fixtureSecret("dsp");

const AT = "2026-11-27T00:00:00Z";
const ASSIGNEE = "bob@example.com";

/** The priority fields of one issue as a current Dispatch answers them. A Dispatch older than the
 *  walk answers the issue's own priority alone, with neither resolved field. */
interface Priorities {
  readonly priority: number | null;
  readonly resolved_priority?: number | null;
  readonly priority_from?: string | null;
}

/** One issue of the stub's project: what its read, its listing row and its asks print. */
interface StubIssue extends Priorities {
  readonly title: string;
  readonly status: string;
  readonly parent: string | null;
}

/** Project ACME's issues: a root at its own P0, a child under it with none of its own, a root at
 *  its own P2, a root with none, and an issue as an older Dispatch answers it, at its own P1. */
const ISSUES: Record<string, StubIssue> = {
  "ACME-1": {
    title: "Import every project",
    status: "in_progress",
    parent: null,
    priority: 0,
    resolved_priority: 0,
    priority_from: null,
  },
  "ACME-2": {
    title: "Wire the importer",
    status: "todo",
    parent: "ACME-1",
    priority: null,
    resolved_priority: 0,
    priority_from: "ACME-1",
  },
  "ACME-3": {
    title: "Ship the parser",
    status: "todo",
    parent: null,
    priority: 2,
    resolved_priority: 2,
    priority_from: null,
  },
  "ACME-4": {
    title: "Tidy the docs",
    status: "backlog",
    parent: null,
    priority: null,
    resolved_priority: null,
    priority_from: null,
  },
  "ACME-5": { title: "Read from an older server", status: "todo", parent: null, priority: 1 },
};

/** The keys the project's listing answers: every issue but the older server's. */
const LISTED = ["ACME-1", "ACME-2", "ACME-3", "ACME-4"];

/** The priority fields of one issue, exactly as the stub holds them: an older server's lacks the
 *  two resolved fields, rather than carrying them as undefined. */
function prioritiesOf(key: string): Priorities {
  const { priority, resolved_priority, priority_from } = ISSUES[key] ?? { priority: null };
  return {
    priority,
    ...(resolved_priority === undefined ? {} : { resolved_priority }),
    ...(priority_from === undefined ? {} : { priority_from }),
  };
}

/** One issue as `GET /api/v1/issues/{key}` answers it: the fields `dispatch read` insists on. */
function issueBody(key: string) {
  const issue = ISSUES[key];
  return {
    key,
    project: "ACME",
    title: issue?.title,
    status: issue?.status,
    ...prioritiesOf(key),
    assignee: ASSIGNEE,
    claim: null,
    labels: [],
    components: { mode: "inherit", ids: [], inherited_from: null, unknown: [] },
    route: null,
    parent: issue?.parent ?? null,
    blocked_by: [],
    external_links: [],
    artifacts: [],
    open_asks: [],
    repo: null,
    resolved_repo: null,
    last_seq: 0,
    advances: [],
    advances_revision: 0,
  };
}

/** One row of the project's listing, as `GET /api/v1/issues` answers it. */
function summaryRow(key: string) {
  const issue = ISSUES[key];
  return {
    key,
    title: issue?.title,
    status: issue?.status,
    ...prioritiesOf(key),
    parent: issue?.parent ?? null,
    labels: [],
    open_asks: 0,
    claim: null,
    route: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
    updated_at: AT,
    progress: { tasks: null, children: null },
  };
}

/** The ask ids of the open-asks and Inbox rows: one on the inheriting child, one on the root that
 *  holds its own P2. */
const CHILD_ASK = "11111111-1111-4111-8111-111111111111";
const OWN_ASK = "22222222-2222-4222-8222-222222222222";
const CHILD_QUESTION = "Which importer do we keep?";
const OWN_QUESTION = "Ship the parser before the importer?";

/** An open ask of this session, as `GET /api/v1/asks/open` answers it, on the issue `key`. */
function openAsk(id: string, key: string, question: string) {
  return {
    id,
    issue_key: key,
    ref: `/issues/${key}/asks/${id}`,
    kind: "question",
    urgency: "med",
    question,
    created_at: AT,
    age_seconds: 3600,
    ...prioritiesOf(key),
    owner: { issue: { key, title: ISSUES[key]?.title } },
    human_replied: false,
    last_reply: null,
    waiting_on: "human",
    waiting_on_person: ASSIGNEE,
  };
}

/** A row of `GET /api/v1/inbox`, asked of the person, on the issue `key`. */
function inboxRow(id: string, key: string, question: string) {
  return {
    id,
    author: { kind: "session", id: "ses-9" },
    kind: "question",
    options: [],
    multiple: false,
    urgency: "med",
    anchor: null,
    state: "open",
    answer: null,
    opened_event_id: 1,
    created_at: AT,
    edited_at: null,
    ...prioritiesOf(key),
    snoozed_until: null,
    thread: { replies: [], edits: [], answers: [], followers: [] },
    issue_key: key,
    issue: { key, title: ISSUES[key]?.title, assignee: ASSIGNEE },
    question,
    waiting_on: "human",
  };
}

const requests: string[] = [];

const server = Bun.serve({
  port: 0,
  fetch(request) {
    const { pathname, search, searchParams } = new URL(request.url);
    requests.push(`${request.method} ${pathname}${search}`);
    if (request.method !== "GET") {
      return Response.json({ error: `unexpected ${request.method} ${pathname}` }, { status: 500 });
    }
    if (pathname === "/api/v1/issues") {
      const rows = LISTED.map(summaryRow);
      if (!searchParams.has("limit")) return Response.json(rows);
      return Response.json({
        issues: rows,
        total: rows.length,
        limit: Number(searchParams.get("limit")),
        offset: Number(searchParams.get("offset")),
      });
    }
    const issue = pathname.match(/^\/api\/v1\/issues\/(ACME-\d+)(\/[a-z]+)?$/);
    if (issue !== null && issue[1] !== undefined && issue[1] in ISSUES) {
      const [, key, part] = issue;
      if (part === undefined) return Response.json(issueBody(key));
      if (part === "/events") return Response.json([]);
      if (part === "/coverage") return Response.json({ gaps: [] });
      if (part === "/references") {
        return Response.json({ node: { kind: "issue", id: key }, edges: [], members: [] });
      }
    }
    if (pathname === "/api/v1/references") {
      return Response.json({ node: { kind: "issue", id: "ACME-1" }, edges: [], members: [] });
    }
    if (pathname === "/api/v1/asks/open") {
      return Response.json({
        session_id: "ses-1",
        as_of: AT,
        opened_since: false,
        count: 2,
        waiting_on_human: 2,
        waiting_on_agent: 0,
        asks: [
          openAsk(CHILD_ASK, "ACME-2", CHILD_QUESTION),
          openAsk(OWN_ASK, "ACME-3", OWN_QUESTION),
        ],
      });
    }
    if (pathname === "/api/v1/whoami") {
      return Response.json({ kind: "agent", owner: ASSIGNEE, service: null, scopes: ["person"] });
    }
    if (pathname === "/api/v1/inbox") {
      return Response.json([
        inboxRow(CHILD_ASK, "ACME-2", CHILD_QUESTION),
        inboxRow(OWN_ASK, "ACME-3", OWN_QUESTION),
      ]);
    }
    if (pathname === "/api/v1/credential-requests") return Response.json({ pending: [] });
    if (pathname === "/api/v1/me/replies") {
      return Response.json({ rows: [], total: 0, limit: 0, offset: 0, as_of: AT });
    }
    return Response.json({ error: `unexpected ${request.method} ${pathname}` }, { status: 500 });
  },
});

afterAll(() => server.stop(true));

let stateDir = "";
beforeEach(() => {
  requests.length = 0;
  stateDir = makeDirectory();
  resetAdviceMemory();
});

async function run(argv: readonly string[]): Promise<{
  code: number;
  stdout: string;
  stderr: string;
}> {
  let stdout = "";
  let stderr = "";
  const code = await runDispatchCli(
    argv,
    {
      HOME: stateDir,
      DISPATCH_URL: `http://127.0.0.1:${server.port}`,
      DISPATCH_TOKEN: TOKEN,
      DISPATCH_STATE_DIR: stateDir,
      DISPATCH_HOST: "omp",
      DISPATCH_SESSION_ID: "ses-1",
    },
    {
      stdout: (text) => {
        stdout += text;
      },
      stderr: (text) => {
        stderr += text;
      },
      readText: () => "",
      cwd: stateDir,
    }
  );
  return { code, stdout, stderr };
}

describe("the command prints the priority an issue takes, and where it comes from", () => {
  test("dispatch read prints an inherited priority with its issue, an own one alone, and none for none", async () => {
    for (const [key, reading, line] of [
      ["ACME-2", "a P0 it takes from ACME-1", "Priority: P0 (from ACME-1)"],
      ["ACME-3", "a P2 of its own", "Priority: P2"],
      ["ACME-4", "no priority on its chain", undefined],
      ["ACME-5", "an older Dispatch's own P1, with no resolved_priority", "Priority: P1"],
    ] as const) {
      const { code, stdout, stderr } = await run(["read", "--issue", key]);

      expect(code, `${key}: ${stderr}${stdout}`).toBe(0);
      const printed = stdout.split("\n").filter((text) => text.startsWith("Priority:"));
      expect(printed, `the Priority line dispatch read prints for ${key}, ${reading}`).toEqual(
        line === undefined ? [] : [line]
      );
    }
  });

  test("dispatch issues prints each row's resolved priority and, where it is inherited, its issue", async () => {
    const { code, stdout, stderr } = await run(["issues", "--project", "ACME"]);

    expect(stderr).toBe("");
    expect(code).toBe(0);
    // The row keeps the form it has: key, status, priority, title, parent. The inherited priority
    // stands where the priority stands, with the issue it comes from beside it.
    expect(stdout.trimEnd().split("\n")).toEqual([
      "4 issues in ACME",
      "ACME-1 [in_progress] P0 Import every project",
      "ACME-2 [todo] P0 (from ACME-1) Wire the importer · child of ACME-1",
      "ACME-3 [todo] P2 Ship the parser",
      "ACME-4 [backlog] Tidy the docs",
    ]);
  });

  test("dispatch issues --json carries each row's resolved priority and its issue beside its own priority", async () => {
    const { code, stdout } = await run(["issues", "--project", "ACME", "--json"]);

    expect(code).toBe(0);
    const printed = JSON.parse(stdout) as { readonly issues: readonly Record<string, unknown>[] };
    expect(
      printed.issues.map((row) => ({
        key: row.key,
        priority: row.priority,
        resolved_priority: row.resolved_priority,
        priority_from: row.priority_from,
      })),
      "each JSON row's own priority, resolved priority and the issue it comes from"
    ).toEqual([
      { key: "ACME-1", priority: 0, resolved_priority: 0, priority_from: null },
      { key: "ACME-2", priority: null, resolved_priority: 0, priority_from: "ACME-1" },
      { key: "ACME-3", priority: 2, resolved_priority: 2, priority_from: null },
      { key: "ACME-4", priority: null, resolved_priority: null, priority_from: null },
    ]);
  });

  test("dispatch open-asks prints the same words on an ask of an issue that inherits its priority", async () => {
    const { code, stdout, stderr } = await run(["open-asks"]);

    expect(stderr).toBe("");
    expect(code).toBe(0);
    const lines = stdout.split("\n");
    expect(
      lines.find((line) => line.includes(CHILD_QUESTION)),
      "the open ask on ACME-2, which takes P0 from ACME-1"
    ).toStartWith(
      `- 1h · P0 (from ACME-1) · ACME-2: Wire the importer · asked of ${ASSIGNEE} · ${CHILD_QUESTION} · `
    );
    expect(
      lines.find((line) => line.includes(OWN_QUESTION)),
      "the open ask on ACME-3, which holds its own P2"
    ).toStartWith(
      `- 1h · P2 · ACME-3: Ship the parser · asked of ${ASSIGNEE} · ${OWN_QUESTION} · `
    );
  });

  test("dispatch inbox prints the same words on an ask of an issue that inherits its priority", async () => {
    const { code, stdout, stderr } = await run(["inbox"]);

    expect(stderr).toBe("");
    expect(code).toBe(0);
    const lines = stdout.split("\n");
    expect(
      lines.find((line) => line.includes(CHILD_QUESTION)),
      "the Inbox row of the ask on ACME-2, which takes P0 from ACME-1"
    ).toBe(
      `- ACME-2 · P0 (from ACME-1) · question · ${CHILD_QUESTION} · dispatch://ACME-2/ask/${CHILD_ASK}`
    );
    expect(
      lines.find((line) => line.includes(OWN_QUESTION)),
      "the Inbox row of the ask on ACME-3, which holds its own P2"
    ).toBe(`- ACME-3 · P2 · question · ${OWN_QUESTION} · dispatch://ACME-3/ask/${OWN_ASK}`);
  });
});
