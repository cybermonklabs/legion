/** How `e2e/acceptance/access-lib.sh`'s `compare`, which the blob driver (c-blob-unchanged) reads
 *  every route through, treats a route the base build does not serve. A route this build's own
 *  route index (GET /api/v1) lists and the base's does not is read at neither build, and named
 *  once on a line `SKIP new route: METHOD PATTERN`. Every other route is compared as before: one
 *  both indexes list, whose answer may have changed or may be a hidden object's 404, and one
 *  neither index lists. An index that cannot be read ends the run UNKNOWN, exit 2, with no route
 *  skipped.
 *
 *  Each row starts nothing of its own. Two Bun servers, `base` and `head`, answer GET /api/v1 with
 *  the index the row sets, and each other path with the answer the row sets, else the router's
 *  no-route 404. The row sources the library in bash, as the driver does, with a work directory
 *  of its own, and reads the counts and lines it printed and the paths each server was asked. */

import { afterAll, beforeAll, expect, test } from "bun:test";
import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import type { Server } from "bun";
import { rowLimit, runBounded } from "./bounded-subprocess";

const library = fileURLToPath(new URL("../acceptance/access-lib.sh", import.meta.url));
const scratch = temporaryDirectories("access-lib-new-routes");

type Build = "base" | "head";

/** What one stand-in server answers: GET /api/v1 with `index`, as raw bytes, and each other path
 *  with its entry in `answers`; every path it was asked, in order, goes to `asked`. */
interface Stand {
  index: { status: number; body: string };
  answers: Record<string, { status: number; body: unknown }>;
  asked: string[];
}

const stands: Record<Build, Stand> = {
  base: { index: { status: 200, body: "" }, answers: {}, asked: [] },
  head: { index: { status: 200, body: "" }, answers: {}, asked: [] },
};
const servers: Partial<Record<Build, Server<undefined>>> = {};

beforeAll(() => {
  for (const build of ["base", "head"] as const) {
    servers[build] = Bun.serve({
      hostname: "127.0.0.1",
      port: 0,
      fetch(request) {
        const path = new URL(request.url).pathname;
        const stand = stands[build];
        stand.asked.push(path);
        if (path === "/api/v1") {
          return new Response(stand.index.body, {
            status: stand.index.status,
            headers: { "Content-Type": "application/json" },
          });
        }
        const answer = stand.answers[path];
        if (answer !== undefined) return Response.json(answer.body, { status: answer.status });
        // The router's answer to a path no route serves (routes/router.go, writeNoRoute).
        return Response.json(
          {
            code: "NOT_FOUND",
            error: `no route for ${request.method} ${path}`,
            hint: "GET /api/v1, with a credential, lists every route",
          },
          { status: 404 }
        );
      },
    });
  }
});

afterAll(() => {
  for (const server of Object.values(servers)) void server?.stop(true);
});

/** A route index as GET /api/v1 answers it: the index's own row, then each GET pattern given. */
const indexOf = (...patterns: string[]) =>
  JSON.stringify({
    docs: "the docs",
    routes: ["/api/v1", ...patterns].map((path) => ({ method: "GET", path, auth: "any" })),
  });

const issue = "/api/v1/issues/{key}";
const nodes = "/api/v1/people/{login}/nodes";

/** Sets each server's index and answers, and forgets what each was asked. */
function serve(base: Omit<Stand, "asked">, head: Omit<Stand, "asked">) {
  stands.base = { ...base, asked: [] };
  stands.head = { ...head, asked: [] };
}

/** Runs `calls` after read_route_indexes, as the blob driver runs its reads, and prints the counts
 *  the library keeps, on a last line `new_routes=N skipped=N compared=N differed=N`. */
async function run(calls: string) {
  const work = scratch();
  for (const directory of ["cookies", "answers"]) mkdirSync(join(work, directory));
  for (const build of ["base", "head"]) {
    for (const person of ["alice", "carol"]) {
      writeFileSync(
        join(work, "cookies", `${build}-${person}`),
        `127.0.0.1\tFALSE\t/\tFALSE\t0\tdsession\t${person}s-session\n`
      );
    }
    writeFileSync(join(work, `ids-${build}.tsv`), "");
  }
  for (const file of ["statuses", "differences"]) writeFileSync(join(work, file), "");
  const script = `
    set -euo pipefail
    work=$1
    call_seconds=5
    run_epoch=$(date +%s)
    check='route-index'
    fail() {
      echo "FAIL: $*" >&2
      exit 1
    }
    note() { echo "   $*"; }
    source "$2"
    build_url[base]=$3
    build_url[head]=$4
    read_route_indexes
    ${calls}
    echo "new_routes=$new_routes skipped=$skipped_requests compared=$compared differed=$differed"
  `;
  return runBounded({
    argv: [
      "bash",
      "-c",
      script,
      "new-routes",
      work,
      library,
      `http://127.0.0.1:${servers.base?.port}`,
      `http://127.0.0.1:${servers.head?.port}`,
    ],
    bound: "short",
    from: import.meta.url,
  });
}

const skips = (stdout: string) => stdout.split("\n").filter((line) => line.startsWith("SKIP "));
const counts = (stdout: string) => stdout.trim().split("\n").at(-1);

test(
  "a route only this build's index lists is read at neither build, and named once",
  async () => {
    serve(
      { index: { status: 200, body: indexOf(issue) }, answers: {} },
      {
        index: { status: 200, body: indexOf(issue, nodes) },
        answers: { "/api/v1/people/carol@example.com/nodes": { status: 200, body: { rows: [] } } },
      }
    );
    const result = await run(`
      compare alice GET /api/v1/people/carol@example.com/nodes
      compare carol GET /api/v1/people/alice@example.com/nodes
      compare carol GET '/api/v1/people/carol@example.com/nodes?limit=5'
    `);
    expect(result.stderr).toBe("");
    expect(result.code).toBe(0);
    expect(skips(result.stdout)).toEqual([`SKIP new route: GET ${nodes}`]);
    expect(counts(result.stdout)).toBe("new_routes=1 skipped=3 compared=0 differed=0");
    // Each server was asked its index alone: no request reached the new route at either build.
    expect(stands.base.asked).toEqual(["/api/v1"]);
    expect(stands.head.asked).toEqual(["/api/v1"]);
  },
  rowLimit("short")
);

test(
  "a route both indexes list is compared: an answer that changed differs, and so does a hidden object's 404",
  async () => {
    const before = { key: "PUB-1", title: "The issue of PUB" };
    serve(
      {
        index: { status: 200, body: indexOf(issue) },
        answers: {
          "/api/v1/issues/PUB-1": { status: 200, body: before },
          "/api/v1/issues/PRIV-1": {
            status: 200,
            body: { key: "PRIV-1", title: "The issue of PRIV" },
          },
        },
      },
      {
        index: { status: 200, body: indexOf(issue, nodes) },
        answers: {
          "/api/v1/issues/PUB-1": { status: 200, body: { ...before, added: true } },
          "/api/v1/issues/PRIV-1": {
            status: 404,
            body: { code: "ISSUE_NOT_FOUND", error: "issue PRIV-1 not found" },
          },
        },
      }
    );
    const result = await run(`
      compare carol GET /api/v1/issues/PUB-1
      compare carol GET /api/v1/issues/PRIV-1
    `);
    expect(result.stderr).toBe("");
    expect(result.code).toBe(0);
    expect(skips(result.stdout)).toEqual([]);
    expect(counts(result.stdout)).toBe("new_routes=0 skipped=0 compared=2 differed=2");
  },
  rowLimit("short")
);

test(
  "a route neither index lists is compared as before",
  async () => {
    serve(
      {
        index: { status: 200, body: indexOf(issue) },
        answers: { "/auth/whoami": { status: 200, body: { login: "carol@example.com" } } },
      },
      {
        index: { status: 200, body: indexOf(issue, nodes) },
        answers: {
          "/auth/whoami": { status: 200, body: { login: "carol@example.com", added: true } },
        },
      }
    );
    const result = await run("compare carol GET /auth/whoami");
    expect(result.stderr).toBe("");
    expect(result.code).toBe(0);
    expect(skips(result.stdout)).toEqual([]);
    expect(counts(result.stdout)).toBe("new_routes=0 skipped=0 compared=1 differed=1");
  },
  rowLimit("short")
);

for (const [name, build, index] of [
  ["answers 500", "base", { status: 500, body: '{"code":"INTERNAL"}' }],
  ["answers 401", "base", { status: 401, body: '{"code":"UNAUTHENTICATED"}' }],
  ["answers no JSON", "base", { status: 200, body: "<html>not an index</html>" }],
  ["answers no array of routes", "base", { status: 200, body: '{"routes":"none"}' }],
  ["answers an empty index", "base", { status: 200, body: '{"routes":[]}' }],
  [
    "does not list GET /api/v1 itself",
    "base",
    { status: 200, body: JSON.stringify({ routes: [{ method: "GET", path: issue }] }) },
  ],
  ["answers 500, at this build", "head", { status: 500, body: '{"code":"INTERNAL"}' }],
] as const) {
  test(
    `an index that ${name} is UNKNOWN, exit 2, and skips no route`,
    async () => {
      const readable = { status: 200, body: indexOf(issue, nodes) };
      serve(
        { index: build === "base" ? index : readable, answers: {} },
        { index: build === "head" ? index : readable, answers: {} }
      );
      const result = await run(`
        compare carol GET /api/v1/people/carol@example.com/nodes
        echo reached the reads
      `);
      expect(result.code).toBe(2);
      expect(result.stderr).toStartWith(
        `UNKNOWN route-index: the ${build} build's route index could not be read, so no route can be told new: `
      );
      expect(result.stdout).not.toContain("SKIP");
      expect(result.stdout).not.toContain("reached the reads");
    },
    rowLimit("short")
  );
}
