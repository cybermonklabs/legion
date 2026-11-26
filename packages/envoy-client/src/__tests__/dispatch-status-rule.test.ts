import { afterAll, beforeEach, describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import type { StatusChangeEntry } from "@legion/contracts";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { bearerCredential } from "../bearer";
import { runDispatchCli } from "../dispatch-cli";
import { executeDispatchTool, resetAdviceMemory } from "../dispatch-execute";
import { DispatchServiceError } from "../dispatch-http";
import { messageFor } from "../errors";
import { ToolInputError } from "../tool-input-errors";

// The parent-status rule from the `dispatch` command: the three options of `dispatch issue-update`
// (`--include-children`, `--unlink-children`, `--include-parent`), which send
// `POST /api/v1/issues/{key}/status-change` in place of the PATCH, the refusal that names each
// issue holding the rule, `dispatch issues --json`, and a comment of Dispatch's system actor read
// back. One fake Dispatch serves the whole file, as in `priorities-command.test.ts`, and answers
// the route with the answers the real server wrote (`TestStatusChangeAnswersMatchTheirFixtures`
// in packages/envoy/internal/dispatch/api), read from there rather than copied.

interface Recorded {
  readonly method: string;
  readonly path: string;
  readonly search: string;
  readonly body: unknown;
}

/** One answer of the route as the real server wrote it: its status and its body. */
interface Answer {
  readonly status: number;
  readonly body: Record<string, unknown>;
}

const FIXTURES = join(
  import.meta.dir,
  "..",
  "..",
  "..",
  "envoy",
  "internal",
  "dispatch",
  "api",
  "testdata",
  "status-change"
);

function fixture(name: string): Answer {
  return JSON.parse(readFileSync(join(FIXTURES, `${name}.json`), "utf-8")) as Answer;
}

const changed = fixture("changed");
const dryRun = fixture("dry-run");
const conflict = fixture("conflict");
const conflictMidway = fixture("conflict-midway");

/** The refusal text a fixture's answer carries, which the CLI prints after the code. */
function errorOf(answer: Answer): string {
  const { error } = answer.body;
  if (typeof error !== "string") throw new Error("the fixture's answer carries no error text");
  return error;
}

/** `answer` with only the `changed` entries of the issues `keep` names: the route answers each
 *  issue the call changed, so a call that asked one option of the two the fixture's call asked
 *  is answered with that option's entries alone. */
function entriesOf(answer: Answer, keep: readonly string[]): Answer {
  const entries = answer.body.changed as StatusChangeEntry[];
  return {
    status: answer.status,
    body: { ...answer.body, changed: entries.filter((entry) => keep.includes(entry.key)) },
  };
}

/** `answer` as the route answers the same call made with `status`: the issue and each entry carry
 *  it in place of the recorded one. The recorded call with include_children closes the issue,
 *  which only a person signed in at a browser may send, so this command's call of it moves the
 *  issues to another status. */
function movedTo(answer: Answer, status: string): Answer {
  const issue = answer.body.issue as Record<string, unknown>;
  const entries = answer.body.changed as StatusChangeEntry[];
  return {
    status: answer.status,
    body: {
      ...answer.body,
      issue: { ...issue, status },
      changed: entries.map((entry) => ({ ...entry, issue: { ...entry.issue, status }, status })),
    },
  };
}

const requests: Recorded[] = [];
const makeDirectory = temporaryDirectories("dispatch-status-rule");
/** The Dispatch bearer each command is handed. */
const TOKEN = fixtureSecret("dsp");
const AT = "2026-01-01T00:00:01Z";
const MESSAGE_ID = "00000000-0000-4000-8000-0000000000aa";
const COMMENT_ID = "00000000-0000-4000-8000-0000000000bb";

/** One issue of project ACME as a read before the call finds it. The keys, titles and statuses
 *  are the fixtures' own; the statuses before the call are the ones the fixtures' calls moved
 *  them from. */
interface Row {
  readonly key: string;
  readonly title: string;
  readonly status: string;
  readonly parent: string | null;
  readonly labels?: readonly string[];
}

const ACME: readonly Row[] = [
  {
    key: "ACME-1",
    title: "Epic on the backlog",
    status: "backlog",
    parent: null,
    labels: ["epic"],
  },
  { key: "ACME-2", title: "Issue started with its epic", status: "todo", parent: "ACME-1" },
  { key: "ACME-3", title: "Child unlinked from the issue", status: "backlog", parent: "ACME-2" },
  { key: "ACME-4", title: "Parent closed with its tree", status: "in_progress", parent: null },
  { key: "ACME-5", title: "Child in work", status: "todo", parent: "ACME-4" },
  { key: "ACME-6", title: "Grandchild in work", status: "in_progress", parent: "ACME-5" },
  { key: "ACME-7", title: "Parent closed with its children", status: "in_progress", parent: null },
  { key: "ACME-8", title: "First child closed", status: "todo", parent: "ACME-7" },
  { key: "ACME-9", title: "Second child", status: "todo", parent: "ACME-7" },
  { key: "ACME-10", title: "Filed between two steps", status: "triage", parent: "ACME-9" },
];

/** A row of the project's listing, as `GET /api/v1/issues` answers it. */
function summary(row: Row) {
  return {
    key: row.key,
    title: row.title,
    status: row.status,
    priority: null,
    parent: row.parent,
    labels: row.labels ?? [],
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

/** An issue as `GET /api/v1/issues/{key}` answers it: the issue the route's own answer carries,
 *  as the template, with this row's fields and its children. */
function details(row: Row) {
  const template = changed.body.issue as Record<string, unknown>;
  return {
    ...template,
    key: row.key,
    number: Number(row.key.split("-")[1]),
    title: row.title,
    status: row.status,
    parent: row.parent,
    labels: row.labels ?? [],
    artifacts: [],
    open_asks: [],
    children: ACME.filter((child) => child.parent === row.key).map((child) => ({
      key: child.key,
      title: child.title,
      status: child.status,
      active_at: AT,
      external_links: [],
      subtree_done: 0,
      subtree_total: 0,
    })),
  };
}

/** What the listing answers next: the project's issues, or many of them for a long output. */
let listing: readonly Row[] = ACME;
/** What the status change route answers next, or no route, as a build older than it does. */
let statusChange: Answer | "no route" = changed;
/** What a PATCH answers next: the issue, or a refusal. */
let patchAnswer: Answer | undefined;
/** What a create answers next. */
let createAnswer: Answer | undefined;

/** The comment Dispatch's system actor writes on a child an option removed from its parent, in
 *  the words of `statusChangeRun.call` (packages/envoy/internal/dispatch/api/status_change.go). */
const systemComment = {
  id: COMMENT_ID,
  issue_key: "ACME-3",
  artifact_id: null,
  author: { kind: "system", id: "parent-status-rule" },
  body: "Dispatch removed this issue from ACME-2, because alice's status change of ACME-2 passed `unlink_children`. Dispatch keeps a parent's status in line with its children's: a parent is done only when each child is done, and while a child is in work its parent is in work too.",
  anchor: null,
  state: "open",
  reply_to: null,
  created_at: AT,
  updated_at: AT,
};

const server = Bun.serve({
  port: 0,
  async fetch(request) {
    const { pathname, search, searchParams } = new URL(request.url);
    const text = await request.text();
    const body = text === "" ? undefined : JSON.parse(text);
    requests.push({ method: request.method, path: pathname, search, body });
    const answer = (sent: Answer): Response => Response.json(sent.body, { status: sent.status });
    if (pathname === "/api/v1/issues" && request.method === "GET") {
      const rows = listing.map(summary);
      // A request with a page's limit is answered a page, and one without the unpaged array.
      if (!searchParams.has("limit")) return Response.json(rows);
      return Response.json({
        issues: rows,
        total: rows.length,
        limit: Number(searchParams.get("limit")),
        offset: Number(searchParams.get("offset")),
      });
    }
    if (pathname === "/api/v1/issues" && request.method === "POST" && createAnswer !== undefined) {
      return answer(createAnswer);
    }
    const status = pathname.match(/^\/api\/v1\/issues\/(ACME-\d+)\/status-change$/);
    if (status !== null && request.method === "POST") {
      return statusChange === "no route"
        ? Response.json(
            { code: "NOT_FOUND", error: `no route for POST ${pathname}` },
            { status: 404 }
          )
        : answer(statusChange);
    }
    const messages = pathname.match(/^\/api\/v1\/issues\/(ACME-\d+)\/messages$/);
    if (messages !== null && request.method === "POST") {
      return Response.json(
        {
          id: MESSAGE_ID,
          issue_key: messages[1],
          author: body.actor,
          body: body.body,
          to: [],
          created_at: AT,
          deliveries: [],
        },
        { status: 201 }
      );
    }
    const issue = ACME.find((row) => pathname === `/api/v1/issues/${row.key}`);
    if (issue !== undefined && request.method === "GET") return Response.json(details(issue));
    if (issue !== undefined && request.method === "PATCH") {
      return patchAnswer === undefined ? Response.json(details(issue)) : answer(patchAnswer);
    }
    if (pathname === `/api/v1/comments/${COMMENT_ID}`) {
      return Response.json({ comment: systemComment, replies: [] });
    }
    if (pathname === "/api/v1/references") {
      return Response.json({ node: { kind: "comment", id: COMMENT_ID }, edges: [] });
    }
    return Response.json({ error: `unexpected ${request.method} ${pathname}` }, { status: 500 });
  },
});

afterAll(() => server.stop(true));

let stateDir = "";
beforeEach(() => {
  requests.length = 0;
  listing = ACME;
  statusChange = changed;
  patchAnswer = undefined;
  createAnswer = undefined;
  stateDir = makeDirectory();
  resetAdviceMemory();
});

/** The host `run` acts as unless a case names another: Oh My Pi, which never cuts output. */
const omp = { DISPATCH_HOST: "omp", DISPATCH_SESSION_ID: "ses-1" };

async function run(
  argv: readonly string[],
  host: Record<string, string> = omp
): Promise<{ code: number; stdout: string; stderr: string }> {
  let stdout = "";
  let stderr = "";
  const code = await runDispatchCli(
    argv,
    {
      HOME: stateDir,
      DISPATCH_URL: `http://127.0.0.1:${server.port}`,
      DISPATCH_TOKEN: TOKEN,
      DISPATCH_STATE_DIR: stateDir,
      ...host,
    },
    {
      stdout: (text) => {
        stdout += text;
      },
      stderr: (text) => {
        stderr += text;
      },
      readText: (path) => readFileSync(path, "utf-8"),
      cwd: stateDir,
    }
  );
  return { code, stdout, stderr };
}

/** The one request of `method` to `path`, which must be the only one the run made. */
function sent(method: string, path: string): Recorded {
  const matching = requests.filter((request) => request.method === method && request.path === path);
  expect(
    matching.map((request) => request.search),
    `${method} ${path}`
  ).toHaveLength(1);
  return matching[0] as Recorded;
}

/** Every request the run made, as `METHOD path`, in the order it made them. */
function made(): string[] {
  return requests.map((request) => `${request.method} ${request.path}`);
}

describe("dispatch issue-update with an option of the status change", () => {
  // The route answers a call with include_children the entries of its dry run, each with the
  // issue it left beside it, so the dry run's record of the call, moved to the backlog, is the
  // answer here. Each line names the status the issue had before the call, which the read of the
  // project gives, since the answer names only the status after it: ACME-6 is a grandchild of
  // ACME-4.
  test("issue-update --include-children sends the status change and no PATCH", async () => {
    statusChange = movedTo(dryRun, "backlog");
    const { code, stdout } = await run([
      "issue-update",
      "--issue",
      "ACME-4",
      "--status",
      "backlog",
      "--include-children",
    ]);

    expect(code).toBe(0);
    expect(made()).toEqual([
      "GET /api/v1/issues/ACME-4",
      "GET /api/v1/issues",
      "POST /api/v1/issues/ACME-4/status-change",
    ]);
    expect(sent("GET", "/api/v1/issues").search).toBe("?project=ACME");
    const body = sent("POST", "/api/v1/issues/ACME-4/status-change").body as Record<
      string,
      unknown
    >;
    expect(Object.keys(body).sort()).toEqual(["actor", "include_children", "status"]);
    expect(body).toMatchObject({ status: "backlog", include_children: true });
    expect(stdout).toBe(
      [
        "ACME-6: status in_progress -> backlog",
        "ACME-5: status todo -> backlog",
        "ACME-4: status in_progress -> backlog",
        "",
      ].join("\n")
    );
  });

  // Dispatch takes include_children with done only from a person signed in at a browser, and
  // every call of this command presents a token: the call is refused before its reason is posted.
  test("--include-children with --status done is refused before anything is sent", async () => {
    const { code, stdout } = await run([
      "issue-update",
      "--issue",
      "ACME-4",
      "--status",
      "done",
      "--reason",
      "Shipped with its tree.",
      "--include-children",
    ]);

    expect(code).toBe(2);
    expect(stdout).toContain("include_children does not go with status done");
    expect(requests).toEqual([]);
  });

  // A refusal lists every problem of the call, so its caller fixes all of them and calls once
  // (the dispatch skill). With no --reason the same call breaks two parts of the rule: done needs
  // a reason, and include_children does not go with done. Adding the reason alone still leaves the
  // second, so the first refusal names both.
  test("--include-children with --status done and no --reason names both problems before anything is sent", async () => {
    const { code, stdout } = await run([
      "issue-update",
      "--issue",
      "ACME-4",
      "--status",
      "done",
      "--include-children",
    ]);

    expect(code).toBe(2);
    expect(stdout).toContain("status done requires reason");
    expect(stdout).toContain("include_children does not go with status done");
    expect(requests).toEqual([]);
  });

  // The same holds when the call names no owner: with no --issue and no LEGION_ISSUE, a document
  // named rows.jsonl breaks the owner part of dispatch artifact's rule and its Markdown-name part.
  // Supplying the owner alone still leaves the name, so the first refusal names both.
  test("dispatch artifact with no owner and a name that is not Markdown names both problems before anything is sent", async () => {
    const { code, stdout } = await run(["artifact", "--name", "rows.jsonl", "--content", "c"]);

    expect(code).toBe(2);
    expect(stdout).toContain("issue is required; supply issue or set LEGION_ISSUE");
    expect(stdout).toContain('"rows.jsonl" is not a Markdown name');
    expect(stdout).not.toContain("Exactly one of issue and project is required");
    expect(requests).toEqual([]);
  });

  // A call that names no owner keeps every other problem of its rule in the refusal, for every
  // command (the architect's decision of 2026-11-21 on this change's CLI). dispatch issue-update's
  // issue is a field its schema requires, so with neither --issue nor LEGION_ISSUE the reason's and
  // the options' parts must still be named beside the missing issue: supplying the issue alone
  // leaves both, and the next call is refused again for them.
  test("issue-update with no owner, --status done and --include-children and no --reason names every problem before anything is sent", async () => {
    const { code, stdout } = await run(["issue-update", "--status", "done", "--include-children"]);

    expect(code).toBe(2);
    expect(stdout).toContain("issue is required; supply issue or set LEGION_ISSUE");
    expect(stdout).toContain("status done requires reason");
    expect(stdout).toContain("include_children does not go with status done");
    expect(requests).toEqual([]);
  });

  // The same holds for a second command whose schema requires issue: a request for changes with
  // no reason and no owner names the missing reason (`REVIEW_REASON_RULE` in
  // packages/contracts/src/dispatch-tools.ts) beside the missing issue.
  test("review with no owner, --verdict changes_requested and no --reason names both problems before anything is sent", async () => {
    const { code, stdout } = await run([
      "review",
      "--artifact",
      "spec.md",
      "--verdict",
      "changes_requested",
      "--version",
      "1",
    ]);

    expect(code).toBe(2);
    expect(stdout).toContain("issue is required; supply issue or set LEGION_ISSUE");
    expect(stdout).toContain(
      "verdict changes_requested requires reason, a non-empty note saying what must change before the document can be approved."
    );
    expect(requests).toEqual([]);
  });

  // dispatch suggest's rule refuses the owner and the document in one sentence
  // (`NAMED_DOCUMENT_OWNER_RULE` in packages/contracts/src/dispatch-tools.ts). With no owner and
  // neither --artifact nor --ref, the refusal names the unnamed document beside the missing issue:
  // supplying the issue alone leaves the document unnamed, and the next call is refused again.
  test("suggest with no owner and no --artifact or --ref names both problems before anything is sent", async () => {
    const { code, stdout } = await run(["suggest", "--quote", "a", "--replace-with", "b"]);

    expect(code).toBe(2);
    expect(stdout).toContain("issue is required; supply issue or set LEGION_ISSUE");
    expect(stdout).toContain("artifact or ref must name the document");
    expect(requests).toEqual([]);
  });

  test("issue-update --unlink-children sends the status change and no PATCH", async () => {
    // The fixture's call passed include_parent too: this one is answered its unlink alone.
    statusChange = entriesOf(changed, ["ACME-3", "ACME-2"]);
    const { code, stdout } = await run([
      "issue-update",
      "--issue",
      "ACME-2",
      "--status",
      "in_progress",
      "--unlink-children",
    ]);

    expect(code).toBe(0);
    // The issue's own read names its children, which is every issue the option removes, so the
    // call reads nothing more.
    expect(made()).toEqual([
      "GET /api/v1/issues/ACME-2",
      "POST /api/v1/issues/ACME-2/status-change",
    ]);
    const body = sent("POST", "/api/v1/issues/ACME-2/status-change").body as Record<
      string,
      unknown
    >;
    expect(Object.keys(body).sort()).toEqual(["actor", "status", "unlink_children"]);
    expect(body).toMatchObject({ status: "in_progress", unlink_children: true });
    expect(stdout).toBe(
      ["ACME-3: unlinked from ACME-2", "ACME-2: status todo -> in_progress", ""].join("\n")
    );

    // The fixture's own call, both options, prints each issue it changed in the order its steps
    // ran.
    requests.length = 0;
    statusChange = changed;
    const both = await run([
      "issue-update",
      "--issue",
      "ACME-2",
      "--status",
      "in_progress",
      "--unlink-children",
      "--include-parent",
    ]);
    expect(both.code).toBe(0);
    expect(made()).not.toContain("PATCH /api/v1/issues/ACME-2");
    expect(both.stdout).toBe(
      [
        "ACME-1: status backlog -> in_progress",
        "ACME-3: unlinked from ACME-2",
        "ACME-2: status todo -> in_progress",
        "",
      ].join("\n")
    );
  });

  test("issue-update --include-parent sends the status change and no PATCH", async () => {
    statusChange = entriesOf(changed, ["ACME-1", "ACME-2"]);
    const { code, stdout } = await run([
      "issue-update",
      "--issue",
      "ACME-2",
      "--status",
      "in_progress",
      "--include-parent",
    ]);

    expect(code).toBe(0);
    // The issues above ACME-2 are read one parent at a time, and ACME-1 is its root: no listing of
    // the project.
    expect(made()).toEqual([
      "GET /api/v1/issues/ACME-2",
      "GET /api/v1/issues/ACME-1",
      "POST /api/v1/issues/ACME-2/status-change",
    ]);
    const body = sent("POST", "/api/v1/issues/ACME-2/status-change").body as Record<
      string,
      unknown
    >;
    expect(Object.keys(body).sort()).toEqual(["actor", "include_parent", "status"]);
    expect(body).toMatchObject({ status: "in_progress", include_parent: true });
    expect(stdout).toBe(
      ["ACME-1: status backlog -> in_progress", "ACME-2: status todo -> in_progress", ""].join("\n")
    );
  });

  test("the three flags beside another field are refused before anything is sent", async () => {
    for (const [flag, field] of [
      ["--include-children", "include_children"],
      ["--unlink-children", "unlink_children"],
      ["--include-parent", "include_parent"],
    ] as const) {
      for (const [other, value, name] of [
        ["--title", "Renamed", "title"],
        ["--parent", "ACME-4", "parent"],
        ["--priority", "1", "priority"],
        ["--labels-add", "x", "labels_add"],
      ] as const) {
        const { code, stdout } = await run([
          "issue-update",
          "--issue",
          "ACME-2",
          "--status",
          "in_progress",
          flag,
          other,
          value,
        ]);
        expect({ flag, other: name, code }).toEqual({ flag, other: name, code: 2 });
        expect(stdout).toStartWith("dispatch issue-update was not called: 1 problem\n");
        expect(stdout).toContain(
          `- With ${field}, give status alone, and reason when the status is done. This call also names ${name}. Send those fields in a call of their own.`
        );
      }
      // An option says what happens when the status changes, so it needs the status.
      const alone = await run(["issue-update", "--issue", "ACME-2", flag]);
      expect(alone.code).toBe(2);
      expect(alone.stdout).toContain(
        `- status is required with ${field}. Add status, or drop ${field}.`
      );
    }
    expect(requests).toEqual([]);
  });

  test("--include-children with --unlink-children is refused before anything is sent", async () => {
    const { code, stdout } = await run([
      "issue-update",
      "--issue",
      "ACME-4",
      "--status",
      "backlog",
      "--include-children",
      "--unlink-children",
    ]);

    expect(code).toBe(2);
    expect(stdout.split("\n").slice(0, 2)).toEqual([
      "dispatch issue-update was not called: 1 problem",
      "- include_children does not go with unlink_children. The open children move with the issue or leave it, so name one of the two.",
    ]);
    expect(requests).toEqual([]);
  });

  // The CLI's own dry run prints the arguments and sends nothing: it asks Dispatch no dry run
  // of its own, as only a `dispatch result` record's does.
  test("--dry-run with --include-children sends nothing", async () => {
    const { code, stdout } = await run([
      "issue-update",
      "--issue",
      "ACME-4",
      "--status",
      "in_progress",
      "--include-children",
      "--dry-run",
    ]);

    expect(code).toBe(0);
    expect(JSON.parse(stdout)).toEqual({
      issue: "ACME-4",
      status: "in_progress",
      include_children: true,
    });
    expect(requests).toEqual([]);
  });

  // The CLI asks a route no build answered three more times, 1, 2 and 4 s apart (jittered), as
  // every write does, before it says so. runDispatchCli takes no wait of its own, so this row calls
  // the executor it runs with a wait that returns at once (`sleep`), as dispatch-review.test.ts
  // does. The CLI prints `messageFor` of what the executor throws, and exits 1 for any error that
  // is no ToolInputError.
  test("--include-children on a Dispatch without the route is refused after its retries and sends no PATCH", async () => {
    statusChange = "no route";
    const base = `http://127.0.0.1:${server.port}`;
    const waits: number[] = [];
    const error: unknown = await executeDispatchTool({
      tool: "dispatch_issue_update",
      args: { issue: "ACME-4", status: "in_progress", include_children: true },
      cwd: stateDir,
      host: "omp",
      sessionId: "ses-1",
      config: { url: base, credential: bearerCredential(TOKEN) },
      env: {},
      sleep: async (ms) => {
        waits.push(ms);
      },
    }).catch((thrown: unknown) => thrown);

    expect(error).toBeInstanceOf(DispatchServiceError);
    expect(error).not.toBeInstanceOf(ToolInputError);
    expect(messageFor(error)).toBe(
      `NOT_FOUND: Dispatch at ${base} answered no route for POST /api/v1/issues/ACME-4/status-change each time the status change was sent. This build is older than that route, so it takes no include_children, unlink_children or include_parent, and nothing changed. No PATCH was sent in its place, since a PATCH would write the status without the option.`
    );
    expect(waits).toHaveLength(3);
    expect(made()).toEqual([
      "GET /api/v1/issues/ACME-4",
      "GET /api/v1/issues",
      ...Array(4).fill("POST /api/v1/issues/ACME-4/status-change"),
    ]);
  });
});

describe("a PARENT_STATUS_CONFLICT", () => {
  // The line after the issues says what to do next; the flags and commands it names are the
  // stable part, and the words around them are the CLI's to change.
  const childFlags = expect.stringMatching(/--unlink-children\b.*--include-children\b/);
  const parentFlags = expect.stringMatching(/dispatch issue-update --status\b.*--include-parent\b/);
  // A create takes no flag that moves its parent: reopen it, or file under an open one or none.
  const reopenParent = expect.stringMatching(
    /dispatch issue-update --issue ACME-4 --status\b.*\bdispatch issue\b.*--parent\b/
  );

  /** Dispatch's refusal of a write that leaves `of` under ACME-4, done, in the words of
   *  `parentStatusConflict.Error` (packages/envoy/internal/dispatch/api/parent_status_rule.go),
   *  as `TestAReparentAndACreateAreCheckedToo` there holds them for a create under a done parent.
   *  The route's recorded answers name no parent, so this one is written here. */
  function doneParentRefusal(of: string): Answer {
    return {
      status: 409,
      body: {
        code: "PARENT_STATUS_CONFLICT",
        error: `this change would leave a parent's status out of line with its children's. ACME-4 (done) is the parent of ${of} and is done. A parent is done only when each child is done, and while a child is in work, its parent is in work.`,
        issues: [
          {
            key: "ACME-4",
            title: "Parent closed with its tree",
            status: "done",
            relation: "parent",
          },
        ],
      },
    };
  }

  // Whichever command Dispatch refuses, the CLI prints the refusal, each issue that holds the
  // rule as `KEY [status] title`, and what the refused command's caller does next, and exits 1,
  // as it prints a near copy's refusal: `dispatch issue-update` names its flags that move the
  // issues, and a create under a done parent says how to reopen it or file elsewhere.
  test("a PARENT_STATUS_CONFLICT prints each issue that holds it", async () => {
    // The PATCH's refusal, which carries no `changed`, after the close's reason landed.
    const { changed: _, ...patchRefusal } = conflict.body;
    patchAnswer = { status: conflict.status, body: patchRefusal };
    const patch = await run([
      "issue-update",
      "--issue",
      "ACME-4",
      "--status",
      "done",
      "--reason",
      "Shipped.",
    ]);
    expect(patch.code).toBe(1);
    const patchLines = patch.stdout.split("\n");
    expect(patchLines[0]).toStartWith(
      `PARENT_STATUS_CONFLICT: ${errorOf(conflict)} The reason already landed as message ${MESSAGE_ID}`
    );
    expect(patchLines.slice(1)).toEqual(["ACME-5 [todo] Child in work", childFlags, ""]);

    // The status change refused midway: the steps before the refusal changed ACME-8, and the
    // refusal names the child filed between two steps. The fake answers the refusal the real
    // server wrote for such a call, made with include_children: the CLI prints a refusal the same
    // whichever option the call passed, and this command never sends include_children with done.
    statusChange = conflictMidway;
    const midway = await run([
      "issue-update",
      "--issue",
      "ACME-7",
      "--status",
      "done",
      "--reason",
      "Closing the tree.",
      "--unlink-children",
    ]);
    expect(midway.code).toBe(1);
    const midwayLines = midway.stdout.split("\n");
    expect(midwayLines[0]).toStartWith(
      `PARENT_STATUS_CONFLICT: ${errorOf(conflictMidway)} The steps before the refusal changed ACME-8 (status todo -> done); the reason already landed as message ${MESSAGE_ID}`
    );
    expect(midwayLines.slice(1)).toEqual([
      "ACME-10 [triage] Filed between two steps",
      childFlags,
      "",
    ]);

    // A create under a done parent takes none of those flags: it is told to reopen the parent
    // first, or to file the issue under an open parent or none. The reopen takes --include-parent,
    // since the rule refuses a plain status change of that parent under a done issue or one not
    // started.
    const createRefusal = doneParentRefusal("the new issue");
    createAnswer = createRefusal;
    const create = await run([
      "issue",
      "--project",
      "ACME",
      "--title",
      "A child of the parent",
      "--parent",
      "ACME-4",
    ]);
    expect(create.code).toBe(1);
    expect(create.stdout.split("\n")).toEqual([
      `PARENT_STATUS_CONFLICT: ${errorOf(createRefusal)}`,
      "ACME-4 [done] Parent closed with its tree",
      reopenParent,
      "",
    ]);
    expect(create.stdout).toMatch(
      /\bdispatch issue-update --issue ACME-4 --status in_progress --include-parent\b/
    );

    // The same refusal of `dispatch issue-update` names the flag that command takes.
    const reopenRefusal = doneParentRefusal("ACME-5");
    patchAnswer = reopenRefusal;
    const reopen = await run(["issue-update", "--issue", "ACME-5", "--status", "in_progress"]);
    expect(reopen.code).toBe(1);
    const reopenLines = reopen.stdout.split("\n");
    expect(reopenLines[0]).toStartWith(`PARENT_STATUS_CONFLICT: ${errorOf(reopenRefusal)}`);
    expect(reopenLines.slice(1)).toEqual([
      "ACME-4 [done] Parent closed with its tree",
      parentFlags,
      "",
    ]);
  });

  // A reparent is refused for the parent the call names, which the issue is not under yet: a
  // status change of that issue with --include-parent moves the issues above it now and leaves
  // the reparent refused, and issue-update refuses --include-parent beside --parent. So the
  // caller is told to move the named parent first, then to send the reparent again.
  test("a refused reparent says to move the parent it names first, then to send the reparent again", async () => {
    const moveParentFirst = (parent: string) =>
      expect.stringMatching(
        new RegExp(
          `dispatch issue-update --issue ${parent} --status\\b.*\\bdispatch issue-update\\b.*--parent\\b`
        )
      );

    // Under a done parent: ACME-8, under ACME-7, to ACME-4.
    const doneRefusal = doneParentRefusal("ACME-8");
    patchAnswer = doneRefusal;
    const underDone = await run(["issue-update", "--issue", "ACME-8", "--parent", "ACME-4"]);
    expect(underDone.code).toBe(1);
    expect(underDone.stdout.split("\n")).toEqual([
      `PARENT_STATUS_CONFLICT: ${errorOf(doneRefusal)}`,
      "ACME-4 [done] Parent closed with its tree",
      moveParentFirst("ACME-4"),
      "",
    ]);

    // Under a parent out of work: ACME-6, in work, to ACME-1 on the backlog, in the words
    // Dispatch answers such a reparent with.
    const outOfWork: Answer = {
      status: 409,
      body: {
        code: "PARENT_STATUS_CONFLICT",
        error:
          "this change would leave a parent's status out of line with its children's. ACME-1 (backlog) is the parent of ACME-6 and is not in work. A parent is done only when each child is done, and while a child is in work, its parent is in work.",
        issues: [
          { key: "ACME-1", title: "Epic on the backlog", status: "backlog", relation: "parent" },
        ],
      },
    };
    patchAnswer = outOfWork;
    const underBacklog = await run(["issue-update", "--issue", "ACME-6", "--parent", "ACME-1"]);
    expect(underBacklog.code).toBe(1);
    expect(underBacklog.stdout.split("\n")).toEqual([
      `PARENT_STATUS_CONFLICT: ${errorOf(outOfWork)}`,
      "ACME-1 [backlog] Epic on the backlog",
      moveParentFirst("ACME-1"),
      "",
    ]);
  });

  // A reparent that also writes a status is refused for a child against that status. The flags
  // that move the children go with --status alone, so the caller is told to run the status change
  // first, in a call of its own, and the reparent after it, without --status.
  test("a reparent beside --status refused for a child says to run the status change first, then the reparent", async () => {
    const childRefusal: Answer = {
      status: 409,
      body: {
        code: "PARENT_STATUS_CONFLICT",
        error:
          "this change would leave a parent's status out of line with its children's. ACME-5 (todo) is a child of ACME-4 and is in work. A parent is done only when each child is done, and while a child is in work, its parent is in work.",
        issues: [{ key: "ACME-5", title: "Child in work", status: "todo", relation: "child" }],
      },
    };
    patchAnswer = childRefusal;
    const reparent = await run([
      "issue-update",
      "--issue",
      "ACME-4",
      "--parent",
      "ACME-7",
      "--status",
      "backlog",
    ]);
    expect(reparent.code).toBe(1);
    expect(reparent.stdout.split("\n")).toEqual([
      `PARENT_STATUS_CONFLICT: ${errorOf(childRefusal)}`,
      "ACME-5 [todo] Child in work",
      expect.stringMatching(
        /--unlink-children\b.*--include-children\b.*--status alone\b.*\bfirst\b.*\bdispatch issue-update --issue ACME-4 --parent ACME-7\b(?! --status)/
      ),
      "",
    ]);
  });

  // A status change beside another field is refused for its parent. --include-parent goes with
  // --status alone, so the caller is told to send the other change in a call of its own.
  test("a status change beside --title refused for its parent says --include-parent goes with --status alone", async () => {
    const parentRefusal = doneParentRefusal("ACME-5");
    patchAnswer = parentRefusal;
    const retitle = await run([
      "issue-update",
      "--issue",
      "ACME-5",
      "--status",
      "in_progress",
      "--title",
      "Child back in work",
    ]);
    expect(retitle.code).toBe(1);
    expect(retitle.stdout.split("\n")).toEqual([
      `PARENT_STATUS_CONFLICT: ${errorOf(parentRefusal)}`,
      "ACME-4 [done] Parent closed with its tree",
      expect.stringMatching(
        /dispatch issue-update --status\b.*--include-parent\b.*--status alone\b/
      ),
      "",
    ]);
  });

  // Since main's #4896 a parent may be an issue of another project, and Dispatch refuses a
  // credential scoped to one project each step that writes an issue of another project (403
  // PROJECT_SCOPE). The CLI does not know its credential's scope, so where an issue the refusal
  // names, which a flag of the line writes, is in another project than the issue the call names
  // (by --issue, or by LEGION_ISSUE), the line says that such a credential is refused that step,
  // and that a person, or a credential that names no project, must make it. A refusal that names
  // only issues of the call's own project says nothing of projects.
  test("a refusal naming an issue of another project says a credential scoped to one project is refused that step", async () => {
    const scopeRefused = (flags: string, key: string, named: string) =>
      expect.stringMatching(
        new RegExp(
          `${flags}.*\\b${key}\\b.*\\b${named}\\b.*\\b403 PROJECT_SCOPE\\b.*\\ba person\\b.*\\bno project\\b`
        )
      );

    // A status change of ACME-5 refused for its parent BETA-1, which --include-parent would move.
    const otherParent: Answer = {
      status: 409,
      body: {
        code: "PARENT_STATUS_CONFLICT",
        error:
          "this change would leave a parent's status out of line with its children's. BETA-1 (done) is the parent of ACME-5 and is done. A parent is done only when each child is done, and while a child is in work, its parent is in work.",
        issues: [
          { key: "BETA-1", title: "Epic of another project", status: "done", relation: "parent" },
        ],
      },
    };
    patchAnswer = otherParent;
    for (const [argv, host] of [
      [["issue-update", "--issue", "ACME-5", "--status", "in_progress"], omp],
      [["issue-update", "--status", "in_progress"], { ...omp, LEGION_ISSUE: "ACME-5" }],
    ] as const) {
      const refused = await run(argv, host);
      expect(refused.code).toBe(1);
      expect(refused.stdout.split("\n")).toEqual([
        `PARENT_STATUS_CONFLICT: ${errorOf(otherParent)}`,
        "BETA-1 [done] Epic of another project",
        scopeRefused("dispatch issue-update --status\\b.*--include-parent\\b", "BETA-1", "ACME-5"),
        "",
      ]);
    }

    // A status change of ACME-4 refused for its child BETA-7, which --unlink-children and
    // --include-children would write, alone and beside a reparent.
    const otherChild: Answer = {
      status: 409,
      body: {
        code: "PARENT_STATUS_CONFLICT",
        error:
          "this change would leave a parent's status out of line with its children's. BETA-7 (in_progress) is a child of ACME-4 and is in work. A parent is done only when each child is done, and while a child is in work, its parent is in work.",
        issues: [
          {
            key: "BETA-7",
            title: "Child of another project",
            status: "in_progress",
            relation: "child",
          },
        ],
      },
    };
    patchAnswer = otherChild;
    for (const argv of [
      ["issue-update", "--issue", "ACME-4", "--status", "backlog"],
      ["issue-update", "--issue", "ACME-4", "--parent", "ACME-7", "--status", "backlog"],
    ]) {
      const refused = await run(argv);
      expect(refused.code).toBe(1);
      expect(refused.stdout.split("\n")).toEqual([
        `PARENT_STATUS_CONFLICT: ${errorOf(otherChild)}`,
        "BETA-7 [in_progress] Child of another project",
        scopeRefused("--unlink-children\\b.*--include-children\\b", "BETA-7", "ACME-4"),
        "",
      ]);
    }

    // The same refusal for a parent of the call's own project says nothing of projects.
    patchAnswer = doneParentRefusal("ACME-5");
    const ownProject = await run(["issue-update", "--issue", "ACME-5", "--status", "in_progress"]);
    expect(ownProject.code).toBe(1);
    expect(ownProject.stdout).toContain("--include-parent");
    expect(ownProject.stdout).not.toContain("PROJECT_SCOPE");
  });
});

describe("dispatch issues --json", () => {
  test("issues --json prints the listing as one JSON object with each issue's status, parent and labels and the total", async () => {
    listing = ACME.slice(0, 3);
    const { code, stdout } = await run(["issues", "--project", "ACME", "--json"]);

    expect(code).toBe(0);
    expect(sent("GET", "/api/v1/issues").search).toBe("?project=ACME&limit=50&offset=0");
    // One object and nothing else, on one line.
    const printed = JSON.parse(stdout) as {
      readonly issues: readonly Record<string, unknown>[];
      readonly total: number;
      readonly offset: number;
      readonly limit: number;
    };
    expect(stdout).toBe(`${JSON.stringify(printed)}\n`);
    expect(Object.keys(printed)).toEqual(["issues", "total", "offset", "limit"]);
    expect({ total: printed.total, offset: printed.offset, limit: printed.limit }).toEqual({
      total: 3,
      offset: 0,
      limit: 50,
    });
    expect(
      printed.issues.map(({ key, status, parent, labels }) => ({ key, status, parent, labels }))
    ).toEqual([
      { key: "ACME-1", status: "backlog", parent: null, labels: ["epic"] },
      { key: "ACME-2", status: "todo", parent: "ACME-1", labels: [] },
      { key: "ACME-3", status: "backlog", parent: "ACME-2", labels: [] },
    ]);
  });

  // Claude Code reads back at most 30,000 characters of a command's output, so the CLI cuts a
  // longer result at 25,000 and writes it whole to a file. A cut object is no JSON object, so the
  // listing goes out whole.
  test("issues --json is printed whole on a host that cuts long output", async () => {
    listing = Array.from({ length: 400 }, (_, index) => ({
      key: `ACME-${index + 100}`,
      title: `Issue ${index + 100} of the long listing, with a title long enough to fill a row`,
      status: index % 2 === 0 ? "backlog" : "in_progress",
      parent: index === 0 ? null : "ACME-100",
    }));
    const claude = { DISPATCH_HOST: "claude", CLAUDE_CODE_SESSION_ID: "ses-long" };

    // The control: the same listing as lines is cut on that host.
    const lines = await run(["issues", "--project", "ACME", "--all"], claude);
    expect(lines.code).toBe(0);
    expect(lines.stdout).toContain("(the full result, ");
    expect(lines.stdout.length).toBeLessThanOrEqual(25_001);

    const { code, stdout } = await run(["issues", "--project", "ACME", "--all", "--json"], claude);
    expect(code).toBe(0);
    expect(stdout.length).toBeGreaterThan(25_001);
    expect(stdout).not.toContain("(the full result, ");
    const printed = JSON.parse(stdout) as { readonly issues: readonly unknown[]; total: number };
    expect(printed.issues).toHaveLength(400);
    expect(printed.total).toBe(400);
  });
});

describe("a comment of Dispatch's system actor", () => {
  // Each issue an option changes gets a comment written as the system actor
  // `{kind: "system", id: "parent-status-rule"}`, which `dispatch read` names as its author.
  test("dispatch read shows a comment of the system actor with its author", async () => {
    const { code, stdout } = await run([
      "read",
      "--ref",
      `dispatch://ACME-3/comment/${COMMENT_ID}`,
    ]);

    expect(code).toBe(0);
    expect(stdout).toContain(`Comment:\n${COMMENT_ID} · system parent-status-rule\n`);
    expect(stdout).toContain(`Body: ${systemComment.body}\n`);
  });
});
