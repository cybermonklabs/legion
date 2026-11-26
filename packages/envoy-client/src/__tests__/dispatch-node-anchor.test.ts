import { afterAll, beforeEach, describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { CommentSchema, CommentWithNodeAnchorSchema } from "@legion/contracts";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { runDispatchCli } from "../dispatch-cli";
import { resetAdviceMemory } from "../dispatch-execute";

// A comment on the text of a lens, a finding, a risk, a constraint or a result anchors on that
// node (R31 of the issue tree's spec). The comment reads answer it with `anchor: null`, because a
// deployed client parses a comment with the strict `CommentSchema`, and only a caller that sends
// `?node_anchor=true` gets the member `node_anchor`: the node, its state, the quote. So
// `dispatch read --ref` of a comment and of an issue's comments send the parameter and print the
// node, its state and the quote on each comment's anchor line. This fake Dispatch answers the
// member as this release does, to a read that asks for it alone, and records each request.

interface Recorded {
  readonly method: string;
  readonly path: string;
  readonly query: string;
}

const LENS = "7c1e0000-0000-4000-8000-000000000002";
const SPEC = "5a5e0000-0000-4000-8000-000000000001";
const ON_LENS = "11111111-1111-4111-8111-111111111111";
const ON_BLOCK = "22222222-2222-4222-8222-222222222222";
const ALICE = { kind: "user", id: "alice@example.com" } as const;

/** A comment row of CORE-1 as every comment read answers it. */
function commentRow(id: string, body: string, anchor: unknown, createdAt: string) {
  return {
    id,
    issue_key: "CORE-1",
    artifact_id: null,
    author: ALICE,
    body,
    anchor,
    reply_to: null,
    answers: null,
    ask_id: null,
    turn: null,
    resolved: false,
    resolved_by: null,
    resolved_at: null,
    edited_at: null,
    deleted_at: null,
    deleted_by: null,
    suggestion: null,
    created_at: createdAt,
    mentions: [],
    deliveries: [],
    reply_block: null,
  };
}

/** A comment of this fake: the row every comment read answers, and the node its anchor names as
 *  a read that asks answers it. */
interface FakeComment {
  readonly row: { readonly id: string; readonly created_at: string } & Record<string, unknown>;
  readonly nodeAnchor: unknown;
}

/** A comment on the text of a lens, quoting its third `bound`: the reads answer its anchor null. */
const onLens: FakeComment = {
  row: commentRow(ON_LENS, "What bounds it?", null, "2026-11-25T09:00:00Z"),
  nodeAnchor: {
    node: { kind: "lens", id: LENS, state: "live" },
    quote: "bound",
    occurrence: 2,
    orphaned: false,
  },
};

/** A comment on a block of the spec that a delete has since taken gone: its document anchor names
 *  that block, and a document anchor stores no occurrence. */
const onBlock: FakeComment = {
  row: commentRow(
    ON_BLOCK,
    "Which key?",
    {
      artifact_id: SPEC,
      block_id: "r1",
      mark_id: "m-1",
      version: 3,
      quote: "the retry is bound",
      orphaned: true,
    },
    "2026-11-25T10:00:00Z"
  ),
  nodeAnchor: {
    node: { kind: "block", id: "CORE-1/spec#r1", state: "gone" },
    quote: "the retry is bound",
    occurrence: null,
    orphaned: true,
  },
};

/** A comment as this fake answers it: with `node_anchor` to a read that sent `node_anchor=true`,
 *  as Dispatch of this release does, and without it to any other read, as the release before
 *  answers every read; either way the strict shape a client reads that form with. */
function answered(comment: FakeComment, asked: boolean) {
  return asked
    ? CommentWithNodeAnchorSchema.parse({ ...comment.row, node_anchor: comment.nodeAnchor })
    : CommentSchema.parse(comment.row);
}

const requests: Recorded[] = [];
const makeDirectory = temporaryDirectories("dispatch-node-anchor");

const server = Bun.serve({
  port: 0,
  fetch(request) {
    const { pathname, search, searchParams } = new URL(request.url);
    requests.push({ method: request.method, path: pathname, query: search });
    const asked = searchParams.get("node_anchor") === "true";
    if (request.method === "GET" && pathname === `/api/v1/comments/${ON_LENS}`) {
      return Response.json({ comment: answered(onLens, asked), replies: [] });
    }
    if (request.method === "GET" && pathname === "/api/v1/issues/CORE-1/comments") {
      return Response.json([answered(onLens, asked), answered(onBlock, asked)]);
    }
    if (request.method === "GET" && pathname === "/api/v1/issues/CORE-1") {
      return Response.json({
        key: "CORE-1",
        project: "CORE",
        title: "Retry bounds",
        status: "in_progress",
        priority: null,
        assignee: null,
        claim: null,
        labels: [],
        route: null,
        parent: null,
        external_links: [],
        artifacts: [],
        open_asks: [],
        last_seq: 0,
      });
    }
    if (request.method === "GET" && pathname === "/api/v1/references") {
      return Response.json({ node: { kind: "comment", id: ON_LENS }, edges: [] });
    }
    return Response.json(
      { error: `unexpected ${request.method} ${pathname}${search}` },
      { status: 500 }
    );
  },
});

afterAll(() => server.stop(true));

let stateDir = "";
beforeEach(() => {
  requests.length = 0;
  stateDir = makeDirectory();
  resetAdviceMemory();
});

async function read(ref: string): Promise<{ code: number; output: string }> {
  let output = "";
  const code = await runDispatchCli(
    ["read", "--ref", ref],
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
        output += text;
      },
      stderr: (text) => {
        output += text;
      },
      readText: (path) => readFileSync(path, "utf-8"),
      cwd: stateDir,
    }
  );
  return { code, output };
}

/** Fails unless the run sent a comment read, and each one it sent asks for node anchors. */
function expectEveryCommentReadAsksForNodeAnchors(): void {
  const sent = requests.map((request) => `${request.method} ${request.path}${request.query}`);
  const reads = requests.filter(
    (request) =>
      request.method === "GET" &&
      (request.path.startsWith("/api/v1/comments/") ||
        request.path === "/api/v1/issues/CORE-1/comments")
  );
  expect(reads.length, `the run sent no comment read: ${sent.join(", ")}`).toBeGreaterThan(0);
  for (const { path, query } of reads) {
    expect(
      new URLSearchParams(query).get("node_anchor"),
      `GET ${path}${query} was sent without node_anchor=true`
    ).toBe("true");
  }
}

/** The line under a comment's header that names the node its anchor is on, then the quote. */
const LENS_LINE = `> bound · on lens/${LENS} (live)`;
const BLOCK_LINE = "> the retry is bound · on CORE-1/spec#r1 (gone)";

describe("dispatch read of a comment anchored on a node", () => {
  test("dispatch read --ref of a comment asks for its node and prints the node, its state and the quote", async () => {
    const ref = `dispatch://CORE-1/comment/${ON_LENS}`;
    const { code, output } = await read(ref);

    expectEveryCommentReadAsksForNodeAnchors();
    const anchored = `${ON_LENS} · user alice@example.com\n${LENS_LINE}\n`;
    expect(
      output,
      `dispatch read --ref ${ref} printed no anchor line ${JSON.stringify(LENS_LINE)} under the comment:\n${output}`
    ).toContain(anchored);
    expect(code, output).toBe(0);
  });

  test("dispatch read --ref of an issue's comments asks for their nodes and prints each one's node, state and quote", async () => {
    const ref = "dispatch://CORE-1/comments";
    const { code, output } = await read(ref);

    expectEveryCommentReadAsksForNodeAnchors();
    for (const [comment, line] of [
      [onLens, LENS_LINE],
      [onBlock, BLOCK_LINE],
    ] as const) {
      const anchored = `${comment.row.id} · user alice@example.com · ${comment.row.created_at}\n${line}\n`;
      expect(
        output,
        `dispatch read --ref ${ref} printed no anchor line ${JSON.stringify(line)} under ${comment.row.id}:\n${output}`
      ).toContain(anchored);
    }
    expect(code, output).toBe(0);
  });
});
