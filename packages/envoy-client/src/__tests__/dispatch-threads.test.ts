import { describe, expect, test } from "bun:test";
import { bearerCredential } from "../bearer";
import { commandHelp } from "../dispatch-command";
import type { ExecFn } from "../dispatch-cwd";
import { executeDispatchTool } from "../dispatch-execute";
import { ToolInputError } from "../tool-input-errors";

const config = {
  enabled: true,
  url: "http://dispatch.test",
  credential: bearerCredential("secret"),
  error: null,
};

const repoExec: ExecFn = async () => ({ stdout: "", stderr: "", exitCode: 1 });

function response(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

/** One thread as the two listings answer it: a root comment, its reply count and last activity. */
function thread(
  id: string,
  body: string,
  options: {
    readonly issue?: string;
    readonly quote?: string;
    readonly resolved?: boolean;
    readonly replies?: number;
    readonly last?: string;
  } = {}
) {
  return {
    root: {
      id,
      issue_key: options.issue ?? "LOOP-1",
      artifact_id: null,
      author: { kind: "user", id: "alice@example.com" },
      body,
      anchor:
        options.quote === undefined
          ? null
          : {
              artifact_id: "artifact-1",
              block_id: null,
              mark_id: "m-1",
              version: 1,
              quote: options.quote,
              orphaned: false,
            },
      reply_to: null,
      answers: null,
      ask_id: null,
      turn: null,
      resolved: options.resolved ?? false,
      resolved_by: null,
      resolved_at: null,
      edited_at: null,
      suggestion: null,
      created_at: "2026-11-10T10:00:00Z",
      mentions: [],
      deliveries: [],
      reply_block: null,
    },
    replies: options.replies ?? 0,
    last_activity_at: options.last ?? "2026-11-10T10:00:00Z",
  };
}

/** An issue holding one document, so `--artifact` resolves as it does for every other tool. */
const issueWithDocument = {
  key: "LOOP-1",
  primary_artifact_id: "artifact-1",
  artifacts: [
    {
      id: "artifact-1",
      project: "LOOP",
      issue_key: "LOOP-1",
      slug: "spec",
      name: "spec.md",
      kind: "doc",
      primary: true,
    },
  ],
};

/** A fetch that records what each listing asked for and answers one page. It answers the issue
 *  read `--artifact` resolution makes, which is the one other request these commands send. */
function listing(page: unknown) {
  const requests: string[] = [];
  const fetchImpl = (async (url: RequestInfo | URL): Promise<Response> => {
    const target = new URL(String(url));
    requests.push(target.pathname + target.search);
    if (target.pathname === "/api/v1/issues/LOOP-1") return response(issueWithDocument);
    if (!target.pathname.startsWith("/api/v1/comment-threads/")) {
      throw new Error(`unexpected request: ${target.pathname}`);
    }
    return response(page);
  }) as unknown as typeof fetch;
  return { requests, fetchImpl };
}

describe("the comment-thread listings", () => {
  // The requirement query by target: one request to the route, with the target whole, and a page
  // a reader scans — the thread's reference, who opened it, the entry it is anchored on, its
  // state, its replies and when anything was last written in it. Before this command the loop
  // asked this question of `dispatch search`, a meaning search over the whole corpus.
  test("threads-by-target asks the route for one project's threads and prints the page", async () => {
    const { requests, fetchImpl } = listing({
      threads: [
        thread("11111111-1111-4111-8111-111111111111", "R:1 open sys_open\nmore text", {
          quote: "sys_open holds.",
          replies: 2,
          last: "2026-11-10T11:30:00Z",
        }),
        thread("22222222-2222-4222-8222-222222222222", "R:3 open sys_open, other proposal", {
          issue: "LOOP-2",
          quote: "sys_open holds.",
          resolved: true,
        }),
      ],
      total: 2,
      limit: 50,
      offset: 0,
    });

    const result = await executeDispatchTool({
      tool: "dispatch_threads_by_target",
      args: { project: "LOOP", target: "sys_open holds." },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec,
      fetchImpl,
    });

    expect(requests).toEqual([
      "/api/v1/comment-threads/by-target?project=LOOP&target=sys_open+holds.",
    ]);
    expect(result.text).toBe(
      [
        'Comment threads of project LOOP anchored on "sys_open holds.": 1-2 of 2.',
        '- dispatch://LOOP-1/comment/11111111-1111-4111-8111-111111111111 · user alice@example.com on "sys_open holds." · open',
        "  2 replies, last written in at 2026-11-10T11:30:00Z: R:1 open sys_open",
        '- dispatch://LOOP-2/comment/22222222-2222-4222-8222-222222222222 · user alice@example.com on "sys_open holds." · resolved',
        "  0 replies, last written in at 2026-11-10T10:00:00Z: R:3 open sys_open, other proposal",
      ].join("\n")
    );
    expect(result.details).toEqual({
      project: "LOOP",
      target: "sys_open holds.",
      threads: 2,
      total: 2,
    });
  });

  // The requirement query by the state of the line's thread, scoped to one issue, narrowed to one
  // document and paged: every input reaches the route, and none of them is a status.
  test("threads-by-thread-state carries the issue, the document and the page to the route", async () => {
    const { requests, fetchImpl } = listing({
      threads: [thread("33333333-3333-4333-8333-333333333333", "R:2 open")],
      total: 7,
      limit: 1,
      offset: 4,
    });

    const result = await executeDispatchTool({
      tool: "dispatch_threads_by_thread_state",
      args: {
        issue: "LOOP-1",
        state: "open",
        artifact: "artifact-1",
        limit: 1,
        offset: 4,
      },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec,
      fetchImpl,
    });

    expect(requests).toEqual([
      "/api/v1/issues/LOOP-1",
      "/api/v1/comment-threads/by-thread-state?issue=LOOP-1&artifact=artifact-1&limit=1&offset=4&state=open",
    ]);
    expect(result.text).toBe(
      [
        "Comment threads of LOOP-1 in state open: 5-5 of 7.",
        "- dispatch://LOOP-1/comment/33333333-3333-4333-8333-333333333333 · user alice@example.com · open",
        "  0 replies, last written in at 2026-11-10T10:00:00Z: R:2 open",
      ].join("\n")
    );
    expect(result.details).toEqual({
      issue: "LOOP-1",
      artifact: "artifact-1",
      limit: 1,
      offset: 4,
      state: "open",
      threads: 1,
      total: 7,
    });
  });

  test("a page holding no thread says so rather than printing an empty list", async () => {
    const { fetchImpl } = listing({ threads: [], total: 0, limit: 50, offset: 0 });
    const result = await executeDispatchTool({
      tool: "dispatch_threads_by_target",
      args: { project: "LOOP", target: "sys_read holds." },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec,
      fetchImpl,
    });
    expect(result.text).toBe('No comment thread of project LOOP is anchored on "sys_read holds.".');
  });

  // The scope and the filter are each required, and a requirement's own status word is refused
  // rather than answered: the state this command takes is the thread's.
  test("refuses a call with no scope, with both scopes, or with a requirement status word", async () => {
    const fetchImpl = (() => {
      throw new Error("network must not be called");
    }) as unknown as typeof fetch;
    const refuse = (tool: string, args: Record<string, unknown>) =>
      executeDispatchTool({
        tool,
        args,
        cwd: "/workspace",
        host: "omp",
        config,
        env: {},
        exec: repoExec,
        fetchImpl,
      }).then(
        () => undefined,
        (error: unknown) => error
      );

    const unscoped = await refuse("dispatch_threads_by_target", { target: "sys_open holds." });
    expect(unscoped).toBeInstanceOf(ToolInputError);
    if (!(unscoped instanceof ToolInputError)) throw new Error("expected ToolInputError");
    expect(unscoped.problems.join("\n")).toContain("Exactly one of project and issue is required");

    const bothScopes = await refuse("dispatch_threads_by_target", {
      project: "LOOP",
      issue: "LOOP-1",
      target: "sys_open holds.",
    });
    expect(bothScopes).toBeInstanceOf(ToolInputError);
    if (!(bothScopes instanceof ToolInputError)) throw new Error("expected ToolInputError");
    expect(bothScopes.problems.join("\n")).toContain(
      "exactly one of issue and project is required"
    );

    const statusWord = await refuse("dispatch_threads_by_thread_state", {
      project: "LOOP",
      state: "met",
    });
    expect(statusWord).toBeInstanceOf(ToolInputError);
    if (!(statusWord instanceof ToolInputError)) throw new Error("expected ToolInputError");
    expect(statusWord.problems.join("\n")).toContain("--state");

    const noTarget = await refuse("dispatch_threads_by_target", { project: "LOOP" });
    expect(noTarget).toBeInstanceOf(ToolInputError);
    if (!(noTarget instanceof ToolInputError)) throw new Error("expected ToolInputError");
    expect(noTarget.problems.join("\n")).toContain("--target is required");
  });

  // A reader of --help meets the distinction the route's own description carries, so nobody asks
  // this command for a requirement's status and reads its answer as one.
  test("threads-by-thread-state --help says the state is the thread's own, not a requirement status", () => {
    const help = commandHelp("dispatch_threads_by_thread_state");
    expect(help).toContain("dispatch threads-by-thread-state");
    expect(help).toContain("not the requirement status of the Verified Linux spec loop's lines");
    expect(help).toContain("dispatch://LINUX-1239");
    expect(help).toContain("--state <open|resolved>");
    expect(help).toContain("--project <text>");
    expect(help).toContain("--issue <text>");
    expect(help).toContain("--artifact <text>");
    expect(help).toContain("--limit <n>");
    expect(help).toContain("--offset <n>");
    expect(help).not.toContain("--status");
  });

  test("threads-by-target --help names the target and the scope rule", () => {
    const help = commandHelp("dispatch_threads_by_target");
    expect(help).toContain("dispatch threads-by-target");
    expect(help).toContain("--target <text>");
    expect(help).toContain("Exactly one of project and issue is required");
    expect(help).not.toContain("--status");
  });
});
