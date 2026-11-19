import { afterEach, expect, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { connect } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { forwardingProxy } from "./dispatch-keyed-comment";

const check = join(import.meta.dir, "dispatch-keyed-comment.ts");
const servers: Server<undefined>[] = [];
const scratchDirectories: string[] = [];

afterEach(() => {
  for (const server of servers.splice(0)) server.stop(true);
  for (const directory of scratchDirectories.splice(0))
    rmSync(directory, { force: true, recursive: true });
});

function tokenFile(): string {
  const directory = mkdtempSync(join(tmpdir(), "dispatch-keyed-comment-test-"));
  scratchDirectories.push(directory);
  const file = join(directory, "token");
  writeFileSync(file, "agent-token\n");
  return file;
}

async function run(...args: string[]): Promise<{ code: number; stderr: string; lines: unknown[] }> {
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

// --url may carry a user and a password: one that is no http or https URL with a host is a usage
// error naming its length and the cause the one function found. The password is made for this run.
test("a --url that is no http or https URL with a host is a usage error naming its length and cause", async () => {
  const password = crypto.randomUUID();
  const said: { code: number; holds: boolean; named: boolean }[] = [];
  const values = [
    `made-up-user:${password}@127.0.0.1:1`,
    `ftp://made-up-user:${password}@host.example`,
  ];
  for (const url of values) {
    const { code, stderr } = await run(
      "--url",
      url,
      "--bearer-file",
      tokenFile(),
      "--issue",
      "CORE-1"
    );
    said.push({
      code,
      holds: stderr.includes(password),
      named: stderr.includes(
        `--url must be Dispatch's http or https address: its value is ${url.length} characters long`
      ),
    });
  }
  expect(said).toEqual(values.map(() => ({ code: 2, holds: false, named: true })));
});

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

test("a comment posted twice under one key against a keyed Dispatch is made once and replayed", async () => {
  const dispatch = standIn({ health: healthy, keyed: true });
  const { code, lines } = await run(
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
});

test("a Dispatch that makes the repeat a second time fails the check, naming the replay", async () => {
  const dispatch = standIn({ health: healthy, keyed: false });
  const { code, lines } = await run(
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
});

test("a /healthz without idempotency_keys fails the check before any write", async () => {
  const dispatch = standIn({
    health: { body: { db: true, ok: true, schema_version: 117 }, status: 200 },
    keyed: true,
  });
  const { code, lines } = await run(
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
});

// A Dispatch whose database or NATS is down answers /healthz 503, and its body still says it
// judges the key: the field is a fact of the build, not of its health.
test("a 503 /healthz that reports idempotency_keys is read by its body", async () => {
  const dispatch = standIn({
    health: { body: { db: false, idempotency_keys: true, ok: false }, status: 503 },
    keyed: true,
  });
  const { code, lines } = await run(
    "--url",
    dispatch.url,
    "--bearer-file",
    tokenFile(),
    "--issue",
    "CORE-1"
  );
  expect(code).toBe(0);
  expect(lines[0]).toEqual({ healthz: { idempotency_keys: true, status: 503 } });
});

// The command's own retry: the proxy loses its first answer, and it asks again under that key.
test("--cli runs dispatch comment through a proxy that loses its first answer, and it lands once", async () => {
  const dispatch = standIn({ health: healthy, keyed: true });
  const { code, lines } = await run(
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
});

test("a check with no issue is a usage error", async () => {
  const { code, stderr } = await run("--url", "http://127.0.0.1:1", "--bearer-file", tokenFile());
  expect(code).toBe(2);
  expect(stderr).toContain("--issue is required");
});

// The proxy the --cli check runs the command through sends each request to --url's own origin,
// with the request's path and query written after it: a request whose path begins `//` reaches
// --url's own server under that path, and --url's user part is not sent on. The request is written
// on a socket, since Bun's fetch would send the path with one slash. The control is the same path
// read against --url as a base, which names the other host. The password is made for this test.
test("the proxy forwards a path that begins // to --url's own origin", async () => {
  const seen: { path: string; authorization: string | null }[] = [];
  const dispatch = Bun.serve({
    fetch(request) {
      const asked = new URL(request.url);
      seen.push({
        authorization: request.headers.get("authorization"),
        path: asked.pathname + asked.search,
      });
      return new Response("dispatch");
    },
    hostname: "127.0.0.1",
    port: 0,
  });
  servers.push(dispatch);
  const url = new URL(`http://made-up-user:not-a-secret-pw-6c1d@127.0.0.1:${dispatch.port}/`);
  const path = "//another.example/api/v1/issues?project=X";
  expect(new URL(path, url).host).toBe("another.example");
  const asked: string[] = [];
  const proxy = forwardingProxy(url, async (_request, incoming, response) => {
    asked.push(incoming.pathname + incoming.search);
    return response;
  });
  servers.push(proxy);
  const { promise, resolve, reject } = Promise.withResolvers<string>();
  const socket = connect(proxy.port, "127.0.0.1", () =>
    socket.write(`GET ${path} HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n`)
  );
  let answer = "";
  socket.on("data", (chunk) => {
    answer += chunk;
  });
  socket.on("end", () => resolve(answer));
  socket.on("error", reject);
  const written = await promise;
  expect({
    answered: written.split("\r\n")[0],
    asked,
    body: written.split("\r\n\r\n")[1],
    // Bun's fetch, which the proxy forwards with, sends a leading `//` as one slash.
    seen: seen.map((one) => ({ ...one, path: one.path.replace(/^\/+/, "/") })),
  }).toEqual({
    answered: "HTTP/1.1 200 OK",
    asked: [path],
    body: "dispatch",
    seen: [{ authorization: null, path: "/another.example/api/v1/issues?project=X" }],
  });
});
