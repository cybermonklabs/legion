import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { type CommentReply, DispatchRefusedError, dispatchRepliesOf } from "../dispatch-read";

/**
 * `dispatchRepliesOf`, which the one-claim-run rule (`claimRunOf` in `reserve.ts`) reads its
 * replies from, against a fake Dispatch that serves `GET /api/v1/comments/<id>` in the shape the
 * real route answers: `{comment, replies}`, the replies oldest first.
 */

const TOKEN = "fake-bearer";
const CLAIM = "3f1c2a4b-5d6e-4f70-8a9b-0c1d2e3f4a5b";
const QUIET = "9e8d7c6b-5a4f-4e3d-8c2b-1a0f9e8d7c6b";

const REPLIES: CommentReply[] = [
  {
    id: "a1b2c3d4-0000-4000-8000-000000000001",
    body: "approval-eval claim run starts",
    created_at: "2026-11-20T10:00:00.000001Z",
    edited_at: null,
  },
  {
    id: "a1b2c3d4-0000-4000-8000-000000000002",
    body: "approval-eval claim run starts, again",
    created_at: "2026-11-20T10:00:05Z",
    edited_at: "2026-11-20T10:01:00Z",
  },
];

/** Each request the fake served: its path as sent and its authorization header. */
const seen: { path: string; authorization: string | null }[] = [];

/** The fake Dispatch while it runs: what the teardown stops, unset when the setup failed first. */
let server: { stop(closeActiveConnections?: boolean): void | Promise<void> } | undefined;
let directory: string | undefined;
const before = {
  url: process.env.DISPATCH_URL,
  tokenFile: process.env.DISPATCH_TOKEN_FILE,
};

beforeAll(async () => {
  const started = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    fetch(request) {
      const url = new URL(request.url);
      seen.push({ path: url.pathname, authorization: request.headers.get("authorization") });
      if (request.headers.get("authorization") !== `Bearer ${TOKEN}`) {
        return new Response("no bearer", { status: 401 });
      }
      if (url.pathname === `/api/v1/comments/${CLAIM}`) {
        return Response.json({ comment: { id: CLAIM, body: "the claim" }, replies: REPLIES });
      }
      if (url.pathname === `/api/v1/comments/${QUIET}`) {
        return Response.json({ comment: { id: QUIET, body: "no reply yet" }, replies: [] });
      }
      return Response.json({ code: "NOT_FOUND", error: "not found" }, { status: 404 });
    },
  });
  server = started;
  directory = await mkdtemp(join(tmpdir(), "approval-eval-dispatch-read-"));
  const tokenFile = join(directory, "token");
  await writeFile(tokenFile, `${TOKEN}\n`);
  process.env.DISPATCH_URL = `http://127.0.0.1:${started.port}`;
  process.env.DISPATCH_TOKEN_FILE = tokenFile;
});

afterAll(async () => {
  await server?.stop(true);
  if (before.url === undefined) delete process.env.DISPATCH_URL;
  else process.env.DISPATCH_URL = before.url;
  if (before.tokenFile === undefined) delete process.env.DISPATCH_TOKEN_FILE;
  else process.env.DISPATCH_TOKEN_FILE = before.tokenFile;
  if (directory !== undefined) await rm(directory, { recursive: true, force: true });
});

describe("dispatchRepliesOf", () => {
  test("reads the comment's thread with the bearer and hands back its replies in order", async () => {
    seen.length = 0;
    expect(await dispatchRepliesOf(CLAIM)).toEqual(REPLIES);
    expect(seen).toEqual([{ path: `/api/v1/comments/${CLAIM}`, authorization: `Bearer ${TOKEN}` }]);
  });

  test("a thread with no reply yet is an empty list", async () => {
    expect(await dispatchRepliesOf(QUIET)).toEqual([]);
  });

  test("an id is sent as one path segment, never as a route of its own", async () => {
    seen.length = 0;
    await expect(dispatchRepliesOf(`${CLAIM}/../../issues`)).rejects.toThrow(DispatchRefusedError);
    expect(seen.map((one) => one.path)).toEqual([
      `/api/v1/comments/${encodeURIComponent(`${CLAIM}/../../issues`)}`,
    ]);
  });

  test("a comment Dispatch does not hold is refused at once, not read as no reply", async () => {
    seen.length = 0;
    const refused = dispatchRepliesOf("00000000-0000-4000-8000-000000000000");
    await expect(refused).rejects.toThrow(DispatchRefusedError);
    await expect(refused).rejects.toMatchObject({ status: 404 });
    expect(seen).toHaveLength(1);
  });
});
