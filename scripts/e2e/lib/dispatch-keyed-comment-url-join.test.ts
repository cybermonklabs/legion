import { afterAll, describe, expect, setDefaultTimeout, test } from "bun:test";
import { writeFileSync } from "node:fs";
import { join } from "node:path";
import { HANG_GUARD_MS } from "@legion/test-fixtures/hang-guard";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";

/**
 * Where the keyed-comment deploy check asks Dispatch: `/healthz`, the comment POSTs and the
 * comment list, and under `--cli` every request its proxy forwards for the `dispatch` command,
 * each joined onto `--url`'s path with its query kept, against a stand-in Dispatch served under a
 * path and a query that answers 404 anywhere else, as a Dispatch behind a path-routed gateway
 * would. The stand-in keys comments as the real one does.
 */

// The rows wait on processes and local servers, which a loaded machine slows. Their bound is a
// hang guard, not a verdict.
setDefaultTimeout(HANG_GUARD_MS);

const check = join(import.meta.dir, "dispatch-keyed-comment.ts");
const ISSUE = "scratch";

/** Every request the stand-in took, as `<method> <path><query>`. */
const asked: string[] = [];
/** Where the stand-in is served: under `mount`, with `query` on every request. */
let served = { mount: "", query: "" };
const comments: { id: string; body: string }[] = [];
const answers = new Map<string, { id: string; body: string }>();

const server = Bun.serve({
  hostname: "127.0.0.1",
  port: 0,
  async fetch(request) {
    const url = new URL(request.url);
    asked.push(`${request.method} ${url.pathname}${url.search}`);
    const route = url.pathname.startsWith(`${served.mount}/`)
      ? url.pathname.slice(served.mount.length)
      : undefined;
    if (route === undefined || url.search !== served.query) {
      return Response.json({ code: "NOT_FOUND", error: "not found" }, { status: 404 });
    }
    if (route === "/healthz") return Response.json({ db: true, idempotency_keys: true, ok: true });
    if (route !== `/api/v1/issues/${ISSUE}/comments`) {
      return Response.json({ code: "NOT_FOUND", error: "not found" }, { status: 404 });
    }
    if (request.headers.get("authorization") !== "Bearer agent-token") {
      return Response.json({ code: "NO_IDENTITY", error: "no identity" }, { status: 401 });
    }
    if (request.method === "GET") return Response.json(comments);
    const key = request.headers.get("idempotency-key");
    const input = (await request.json()) as { body: string };
    const stored = key === null ? undefined : answers.get(key);
    if (stored !== undefined) {
      return Response.json(stored, {
        headers: { "Idempotency-Key": key ?? "", "Idempotency-Replayed": "?1" },
        status: 201,
      });
    }
    const comment = { body: input.body, id: crypto.randomUUID() };
    comments.push(comment);
    if (key !== null) answers.set(key, comment);
    return Response.json(comment, {
      headers: key === null ? {} : { "Idempotency-Key": key },
      status: 201,
    });
  },
});
const origin = `http://127.0.0.1:${server.port}`;

afterAll(() => server.stop(true));

const directory = temporaryDirectory("dispatch-keyed-comment-url-join");

/** One run of the check against `url`, the stand-in served under `mount` and `query`: its exit
 *  code, the last line it printed, and the requests the stand-in took. */
async function askedBy(url: string, mount: string, query: string, ...extra: string[]) {
  served = { mount, query };
  asked.length = 0;
  comments.length = 0;
  answers.clear();
  const tokenFile = join(directory, "token");
  writeFileSync(tokenFile, "agent-token\n");
  const child = Bun.spawn(
    [process.execPath, check, "--url", url, "--bearer-file", tokenFile, "--issue", ISSUE, ...extra],
    { env: { HOME: directory, PATH: process.env.PATH ?? "" }, stderr: "pipe", stdout: "pipe" }
  );
  const [code, stdout] = await Promise.all([
    child.exited,
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
  ]);
  const lines = stdout.split("\n").filter((line) => line !== "");
  const last = lines.at(-1);
  return { asked: [...asked], code, last: last === undefined ? null : JSON.parse(last) };
}

describe("dispatch-keyed-comment's requests", () => {
  test("join each route onto --url's path and keep its query", async () => {
    const { asked, code, last } = await askedBy(
      `${origin}/prefix?key=k3y-made-up`,
      "/prefix",
      "?key=k3y-made-up"
    );
    expect(asked).toEqual([
      "GET /prefix/healthz?key=k3y-made-up",
      `POST /prefix/api/v1/issues/${ISSUE}/comments?key=k3y-made-up`,
      `POST /prefix/api/v1/issues/${ISSUE}/comments?key=k3y-made-up`,
      `GET /prefix/api/v1/issues/${ISSUE}/comments?key=k3y-made-up`,
    ]);
    expect({ code, last }).toEqual({ code: 0, last: { ok: true } });
  });

  test("forward each request of the --cli proxy onto --url's path, its query kept", async () => {
    const { asked, code, last } = await askedBy(
      `${origin}/prefix?key=k3y-made-up`,
      "/prefix",
      "?key=k3y-made-up",
      "--cli"
    );
    expect(asked).toEqual([
      "GET /prefix/healthz?key=k3y-made-up",
      `POST /prefix/api/v1/issues/${ISSUE}/comments?key=k3y-made-up`,
      `POST /prefix/api/v1/issues/${ISSUE}/comments?key=k3y-made-up`,
      `GET /prefix/api/v1/issues/${ISSUE}/comments?key=k3y-made-up`,
    ]);
    expect({ code, last }).toEqual({ code: 0, last: { ok: true } });
  });

  test("ask a plain origin at each route itself", async () => {
    for (const extra of [[], ["--cli"]]) {
      const { asked, code, last } = await askedBy(origin, "", "", ...extra);
      expect(asked, extra.join(" ")).toEqual([
        "GET /healthz",
        `POST /api/v1/issues/${ISSUE}/comments`,
        `POST /api/v1/issues/${ISSUE}/comments`,
        `GET /api/v1/issues/${ISSUE}/comments`,
      ]);
      expect({ code, last }, extra.join(" ")).toEqual({ code: 0, last: { ok: true } });
    }
  });
});
