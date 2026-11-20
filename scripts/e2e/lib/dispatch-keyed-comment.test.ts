import { afterEach, expect, test } from "bun:test";
import { mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { command, type Row, rowMs, rowOf, type Wait } from "@legion/test-fixtures/row-waits";
import type { Server } from "bun";
import { noHookLimit, removeScratch } from "../../../packages/dispatch/e2e/__tests__/step-end";
import { DISPATCH_TOOL_DEADLINE_MS } from "../../../packages/envoy-client/src/dispatch-http";

const check = join(import.meta.dir, "dispatch-keyed-comment.ts");
const servers: Server<undefined>[] = [];
const scratchDirectories: string[] = [];

/** What the rows of this file wait for, each row its own list of them, which its limit is the sum
 *  of (`rowMs` of `@legion/test-fixtures/row-waits`) and which `run` counts as the row makes them
 *  (`rowOf`). The first is the check, a process of its own, which starts nothing that waits but
 *  under `--cli`. */
const theCheck = command("the check");
/** Under `--cli`, the check starts this checkout's `dispatch comment`, a process of its own. */
const theCommand = command("the dispatch command");
/** And it waits for that command's write: the first POST, whose answer the proxy loses, the
 *  command's probe of `/healthz`, which tells it that Dispatch keys its writes, and its asks again
 *  under its key on the schedule of `DISPATCH_WRITE_RETRY`. The command's client ends every one of
 *  them at its own deadline, counted from when it was made, which is this wait's bound. */
const theWrite: Wait = {
  what: "the dispatch command's write and its asks again",
  ms: DISPATCH_TOOL_DEADLINE_MS,
};
/** A row that runs the check once, without `--cli`. */
const oneCheck = [theCheck];
/** A row that runs the check once under `--cli`. */
const cliCheck = [theCheck, theCommand, theWrite];

// Each row's scratch goes in a process of its own, which may take what the CI step leaves, or the
// limit of this file's shortest rows, one check's, and the hook has no limit of Bun's
// (step-end.ts).
afterEach(async () => {
  for (const server of servers.splice(0)) server.stop(true);
  await removeScratch(scratchDirectories.splice(0), rowMs(oneCheck));
}, noHookLimit);

function tokenFile(): string {
  const directory = mkdtempSync(join(tmpdir(), "dispatch-keyed-comment-test-"));
  scratchDirectories.push(directory);
  const file = join(directory, "token");
  writeFileSync(file, "agent-token\n");
  return file;
}

/** Runs the check with `args`, and counts against `row` what the row then waits for: the check,
 *  and under `--cli` the command it starts and that command's write. */
async function run(
  row: Row,
  ...args: string[]
): Promise<{ code: number; stderr: string; lines: unknown[] }> {
  row.charge(theCheck);
  if (args.includes("--cli")) {
    row.charge(theCommand);
    row.charge(theWrite);
  }
  const child = Bun.spawn([process.execPath, check, ...args], { stderr: "pipe", stdout: "pipe" });
  const [code, stderr, stdout] = await Promise.all([
    child.exited,
    new Response(child.stderr).text(),
    new Response(child.stdout).text(),
  ]);
  return {
    code,
    lines: stdout
      .split("\n")
      .filter((line) => line !== "")
      .map((line) => JSON.parse(line)),
    stderr,
  };
}

interface StandIn {
  readonly url: string;
  /** Every comment POST it took: its key and whether it made a comment. */
  readonly posts: { key: string | null; made: boolean }[];
  readonly comments: { id: string; body: string }[];
}

/** A Dispatch that keys comments as the real one does (`keyed`), or one that ignores the header
 *  and makes every POST it gets; its `/healthz` answers `health`. */
function standIn(options: {
  keyed: boolean;
  health: { status: number; body: Record<string, unknown> };
}): StandIn {
  const posts: StandIn["posts"] = [];
  const comments: StandIn["comments"] = [];
  const answers = new Map<string, { id: string; body: string }>();
  const server = Bun.serve({
    async fetch(request) {
      const url = new URL(request.url);
      if (url.pathname === "/healthz") {
        return Response.json(options.health.body, { status: options.health.status });
      }
      if (url.pathname !== "/api/v1/issues/CORE-1/comments") {
        return Response.json({ code: "NOT_FOUND", error: "not found" }, { status: 404 });
      }
      if (request.headers.get("authorization") !== "Bearer agent-token") {
        return Response.json({ code: "NO_IDENTITY", error: "no identity" }, { status: 401 });
      }
      if (request.method === "GET") return Response.json(comments);
      const key = request.headers.get("idempotency-key");
      const input = (await request.json()) as { body: string };
      const stored = options.keyed && key !== null ? answers.get(key) : undefined;
      if (stored !== undefined) {
        posts.push({ key, made: false });
        return Response.json(stored, {
          headers: { "Idempotency-Key": key ?? "", "Idempotency-Replayed": "?1" },
          status: 201,
        });
      }
      const comment = { body: input.body, id: crypto.randomUUID() };
      comments.push(comment);
      posts.push({ key, made: true });
      if (options.keyed && key !== null) answers.set(key, comment);
      return Response.json(comment, {
        headers: options.keyed && key !== null ? { "Idempotency-Key": key } : {},
        status: 201,
      });
    },
    hostname: "127.0.0.1",
    port: 0,
  });
  servers.push(server);
  return { comments, posts, url: `http://127.0.0.1:${server.port}` };
}

const healthy = { body: { db: true, idempotency_keys: true, ok: true }, status: 200 };

test(
  "a comment posted twice under one key against a keyed Dispatch is made once and replayed",
  async () => {
    const dispatch = standIn({ health: healthy, keyed: true });
    const { code, lines } = await run(
      rowOf(oneCheck),
      "--url",
      dispatch.url,
      "--bearer-file",
      tokenFile(),
      "--issue",
      "CORE-1"
    );
    expect(code).toBe(0);
    const key = dispatch.posts[0]?.key;
    expect(key).toMatch(/^deploy-check-[0-9a-f-]{36}$/);
    expect(dispatch.posts).toEqual([
      { key, made: true },
      { key, made: false },
    ]);
    expect(dispatch.comments).toHaveLength(1);
    expect(lines).toEqual([
      { healthz: { idempotency_keys: true, status: 200 } },
      { echo: key, id: dispatch.comments[0]?.id, post: 1, replayed: null, status: 201 },
      { echo: key, id: dispatch.comments[0]?.id, post: 2, replayed: "?1", status: 201 },
      { comments_with_body: 1 },
      { ok: true },
    ]);
  },
  rowMs(oneCheck)
);

test(
  "a Dispatch that makes the repeat a second time fails the check, naming the replay",
  async () => {
    const dispatch = standIn({ health: healthy, keyed: false });
    const { code, lines } = await run(
      rowOf(oneCheck),
      "--url",
      dispatch.url,
      "--bearer-file",
      tokenFile(),
      "--issue",
      "CORE-1"
    );
    expect(code).toBe(1);
    expect(dispatch.comments).toHaveLength(2);
    expect(lines.at(-1)).toEqual({
      failed: expect.stringContaining("not Dispatch's stored 201 marked ?1"),
    });
  },
  rowMs(oneCheck)
);

test(
  "a /healthz without idempotency_keys fails the check before any write",
  async () => {
    const dispatch = standIn({
      health: { body: { db: true, ok: true, schema_version: 117 }, status: 200 },
      keyed: true,
    });
    const { code, lines } = await run(
      rowOf(oneCheck),
      "--url",
      dispatch.url,
      "--bearer-file",
      tokenFile(),
      "--issue",
      "CORE-1"
    );
    expect(code).toBe(1);
    expect(dispatch.posts).toEqual([]);
    expect(lines).toEqual([
      { healthz: { idempotency_keys: null, status: 200 } },
      { failed: expect.stringContaining("does not answer idempotency_keys: true") },
    ]);
  },
  rowMs(oneCheck)
);

// A Dispatch whose database or NATS is down answers /healthz 503, and its body still says it
// judges the key: the field is a fact of the build, not of its health.
test(
  "a 503 /healthz that reports idempotency_keys is read by its body",
  async () => {
    const dispatch = standIn({
      health: { body: { db: false, idempotency_keys: true, ok: false }, status: 503 },
      keyed: true,
    });
    const { code, lines } = await run(
      rowOf(oneCheck),
      "--url",
      dispatch.url,
      "--bearer-file",
      tokenFile(),
      "--issue",
      "CORE-1"
    );
    expect(code).toBe(0);
    expect(lines[0]).toEqual({ healthz: { idempotency_keys: true, status: 503 } });
  },
  rowMs(oneCheck)
);

// The command's own retry: the proxy loses its first answer, and it asks again under that key. The
// row waits until the comment has landed once, which takes the check, the command and its write.
test(
  "--cli runs dispatch comment through a proxy that loses its first answer, and it lands once",
  async () => {
    const dispatch = standIn({ health: healthy, keyed: true });
    const { code, lines } = await run(
      rowOf(cliCheck),
      "--url",
      dispatch.url,
      "--bearer-file",
      tokenFile(),
      "--issue",
      "CORE-1",
      "--cli"
    );
    expect({ code, lines }).toMatchObject({ code: 0 });
    const key = dispatch.posts[0]?.key;
    expect(key).toEqual(expect.any(String));
    expect(dispatch.posts).toEqual([
      { key, made: true },
      { key, made: false },
    ]);
    expect(dispatch.comments).toHaveLength(1);
    expect(lines).toContainEqual({
      proxy: {
        answered: 502,
        key,
        method: "POST",
        path: "/api/v1/issues/CORE-1/comments",
        replayed: null,
        status: 201,
      },
    });
    expect(lines).toContainEqual({
      proxy: {
        key,
        method: "POST",
        path: "/api/v1/issues/CORE-1/comments",
        replayed: "?1",
        status: 201,
      },
    });
    expect(lines).toContainEqual({
      cli: { code: 0, stderr: "", stdout: expect.stringContaining("Posted comment") },
    });
    expect(lines.slice(-2)).toEqual([{ comments_with_body: 1 }, { ok: true }]);
  },
  rowMs(cliCheck)
);

test(
  "a check with no issue is a usage error",
  async () => {
    const { code, stderr } = await run(
      rowOf(oneCheck),
      "--url",
      "http://127.0.0.1:1",
      "--bearer-file",
      tokenFile()
    );
    expect(code).toBe(2);
    expect(stderr).toContain("--issue is required");
  },
  rowMs(oneCheck)
);
