import { afterAll, beforeEach, describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { runDispatchCli } from "../dispatch-cli";
import { fieldFlag } from "../dispatch-command";
import { resetAdviceMemory } from "../dispatch-execute";

// An issue's relations as the `dispatch` command writes and reads them (R9, R10 of the issue-tree
// spec). A wait is one relation, needs, the one spelling Dispatch reads and answers
// (dispatch://INFRA-1098 R27): the create and the update send it alone, and
// dispatch-needs-spelling.test.ts holds that no request carries blocked_by.

interface Recorded {
  readonly method: string;
  readonly path: string;
  readonly body: unknown;
}

const requests: Recorded[] = [];
const makeDirectory = temporaryDirectories("dispatch-issue-relations");

/** DSP-1 as this fake answers it, its waits under needs. */
function issueBody() {
  return {
    key: "DSP-1",
    project: "DSP",
    title: "Native workspace",
    status: "in_progress",
    priority: null,
    assignee: null,
    claim: null,
    labels: [],
    components: { mode: "inherit", ids: [], inherited_from: null, unknown: [] },
    route: null,
    parent: null,
    needs: ["DSP-2", "OTHER-3"],
    duplicate_of: "DSP-4",
    filed_from: "dispatch://DSP-5/comment/0b6f3c3e-2f43-4a6c-9b0e-7a2f8e1d4c55",
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

/** The edges DSP-1 writes, as the references read of a Dispatch of the issue tree answers them:
 *  its parent, its wait on each issue it needs, the issue it repeats and the comment it was filed
 *  out of. */
const linkEdges = [
  ["child_of", "issue", "DSP-7", "dispatch://DSP-7"],
  ["needs", "issue", "DSP-2", "dispatch://DSP-2"],
  ["needs", "issue", "OTHER-3", "dispatch://OTHER-3"],
  ["duplicate_of", "issue", "DSP-4", "dispatch://DSP-4"],
  [
    "filed_from",
    "comment",
    "0b6f3c3e-2f43-4a6c-9b0e-7a2f8e1d4c55",
    "dispatch://DSP-5/comment/0b6f3c3e-2f43-4a6c-9b0e-7a2f8e1d4c55",
  ],
].map(([kind, nodeKind, id, ref]) => ({
  kind,
  direction: "out",
  node: { kind: nodeKind, id, ref },
  created_at: "2026-11-17T10:00:00Z",
  source_seq: null,
}));

const server = Bun.serve({
  port: 0,
  async fetch(request) {
    const { pathname, searchParams } = new URL(request.url);
    const text = await request.text();
    const body = text === "" ? undefined : JSON.parse(text);
    requests.push({ method: request.method, path: pathname, body });
    if (pathname === "/api/v1/issues" && request.method === "POST") {
      return Response.json({ ...issueBody(), key: "DSP-12", title: body.title }, { status: 201 });
    }
    if (
      pathname === "/api/v1/issues/DSP-1" &&
      (request.method === "PATCH" || request.method === "GET")
    ) {
      return Response.json(issueBody());
    }
    if (pathname === "/api/v1/issues/DSP-1/events" && request.method === "GET") {
      return Response.json([]);
    }
    if (pathname === "/api/v1/references" && request.method === "GET") {
      const from = searchParams.get("from") === "dispatch://DSP-1";
      return Response.json({
        node: { kind: "issue", id: "DSP-1", ref: "dispatch://DSP-1" },
        edges: from ? linkEdges : [],
      });
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

async function run(argv: readonly string[]): Promise<{ code: number; stdout: string }> {
  let stdout = "";
  const code = await runDispatchCli(
    argv,
    {
      HOME: stateDir,
      DISPATCH_HOST: "omp",
      DISPATCH_SESSION_ID: "ses-1",
      DISPATCH_URL: `http://127.0.0.1:${server.port}`,
      DISPATCH_TOKEN: fixtureSecret("dispatch"),
      DISPATCH_STATE_DIR: stateDir,
    },
    {
      stdout: (text) => {
        stdout += text;
      },
      stderr: () => {},
      readText: (path) => readFileSync(path, "utf-8"),
      cwd: stateDir,
    }
  );
  return { code, stdout };
}

/** The one request of `method` to `path` the run made. */
function sent(method: string, path: string): Recorded {
  const matching = requests.filter((request) => request.method === method && request.path === path);
  expect(matching, `${method} ${path}`).toHaveLength(1);
  return matching[0] as Recorded;
}

describe("an issue's relations through the dispatch command", () => {
  test("the flags are the fields' own: --blocked-by, --duplicate-of and --filed-from", () => {
    for (const tool of ["dispatch_issue", "dispatch_issue_update"] as const) {
      expect(fieldFlag(tool, "blocked_by"), tool).toBe("--blocked-by");
      expect(fieldFlag(tool, "duplicate_of"), tool).toBe("--duplicate-of");
      expect(fieldFlag(tool, "filed_from"), tool).toBe("--filed-from");
    }
  });

  test("dispatch issue sends needs, and its relations", async () => {
    const { code } = await run([
      "issue",
      "--project",
      "DSP",
      "--title",
      "Waits on two",
      "--blocked-by",
      "DSP-2",
      "--blocked-by",
      "OTHER-3",
      "--duplicate-of",
      "DSP-4",
      "--filed-from",
      "dispatch://DSP-5/comment/0b6f3c3e-2f43-4a6c-9b0e-7a2f8e1d4c55",
    ]);

    expect(code).toBe(0);
    const body = sent("POST", "/api/v1/issues").body as Record<string, unknown>;
    expect(body.needs).toEqual(["DSP-2", "OTHER-3"]);
    expect(body.duplicate_of).toBe("DSP-4");
    expect(body.filed_from).toBe("dispatch://DSP-5/comment/0b6f3c3e-2f43-4a6c-9b0e-7a2f8e1d4c55");
  });

  test("dispatch issue-update sends needs and prints the waits the answer holds", async () => {
    const { code, stdout } = await run([
      "issue-update",
      "--issue",
      "DSP-1",
      "--blocked-by",
      "DSP-2",
      "--blocked-by",
      "OTHER-3",
    ]);

    expect(code).toBe(0);
    const body = sent("PATCH", "/api/v1/issues/DSP-1").body as Record<string, unknown>;
    expect(body.needs).toEqual(["DSP-2", "OTHER-3"]);
    expect(body).not.toHaveProperty("blocked_by");
    expect(stdout).toContain("blocked by DSP-2, OTHER-3");
  });

  test("dispatch issue-update sends duplicate_of and filed_from, and clears each with an empty value", async () => {
    const written = await run([
      "issue-update",
      "--issue",
      "DSP-1",
      "--duplicate-of",
      "DSP-4",
      "--filed-from",
      "dispatch://DSP-5/comment/0b6f3c3e-2f43-4a6c-9b0e-7a2f8e1d4c55",
    ]);
    expect(written.code).toBe(0);
    expect(sent("PATCH", "/api/v1/issues/DSP-1").body).toMatchObject({
      duplicate_of: "DSP-4",
      filed_from: "dispatch://DSP-5/comment/0b6f3c3e-2f43-4a6c-9b0e-7a2f8e1d4c55",
    });
    expect(written.stdout).toContain("duplicate of DSP-4");

    requests.length = 0;
    const cleared = await run([
      "issue-update",
      "--issue",
      "DSP-1",
      "--duplicate-of",
      "",
      "--filed-from",
      "",
    ]);
    expect(cleared.code).toBe(0);
    const body = sent("PATCH", "/api/v1/issues/DSP-1").body as Record<string, unknown>;
    expect(body.duplicate_of).toBeNull();
    expect(body.filed_from).toBeNull();
  });

  test("dispatch read --issue prints child_of, needs, duplicate_of and filed_from under Links", async () => {
    const { code, stdout } = await run(["read", "--issue", "DSP-1"]);

    // The read ends with the edges the issue writes, each under the kind the server names it by.
    expect(stdout).toEndWith(
      [
        "Links:",
        "- child_of issue dispatch://DSP-7 (2026-11-17T10:00:00Z)",
        "- needs issue dispatch://DSP-2 (2026-11-17T10:00:00Z)",
        "- needs issue dispatch://OTHER-3 (2026-11-17T10:00:00Z)",
        "- duplicate_of issue dispatch://DSP-4 (2026-11-17T10:00:00Z)",
        "- filed_from comment dispatch://DSP-5/comment/0b6f3c3e-2f43-4a6c-9b0e-7a2f8e1d4c55 (2026-11-17T10:00:00Z)\n",
      ].join("\n")
    );
    expect(code).toBe(0);
  });
});
