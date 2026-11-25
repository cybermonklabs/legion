/** How `e2e/acceptance/project-access-walk.sh`'s `walk_rows` reads a GET row whose only read
 *  class is none (RouteProbe's classes): such a row reads no row a caller's access decides and
 *  answers every caller alike, so its answer for PRIV's object can rightly differ from its answer
 *  for an id nobody holds. GET /api/v1/github/app-installation/{owner}/{repo} answers
 *  installed:true for PRIV's repository, the App's, and installed:false for one nobody holds,
 *  whoever asks: the walk held it to the missing repository's answer, and the four unseeing
 *  callers' answers differed. It is read with PRIV's objects on both servers, beside the twin,
 *  which never held PRIV; a row of any other class is still held to a missing object's answer.
 *
 *  Each row starts two stand-in servers, `main` and `twin`, writes the route index and the Probes
 *  the walk reads, and runs the walk in bash, sourcing the library and the walk's file. */

import { afterAll, beforeAll, expect, test } from "bun:test";
import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import type { Server } from "bun";
import { rowLimit, runBounded } from "./bounded-subprocess";

const library = fileURLToPath(new URL("../acceptance/access-lib.sh", import.meta.url));
const walk = fileURLToPath(new URL("../acceptance/project-access-walk.sh", import.meta.url));
const scratch = temporaryDirectories("project-access-walk-class-none");

const installation = "/api/v1/github/app-installation/{owner}/{repo}";
const issue = "/api/v1/issues/{key}";

/** Whether main answers PRIV-1 to an unseeing caller as the issue it is, a leak, rather than as
 *  one nobody holds. */
let mainLeaksPriv = false;
const servers: Server<undefined>[] = [];

beforeAll(() => {
  for (const name of ["main", "twin"] as const) {
    // Every answer says whether the deployment holds a private project: main, which holds
    // PRIV, says 1, and the twin 0 (project-access-walk.sh's main_header and twin_header).
    const headers = { "Dispatch-Private-Projects": name === "main" ? "1" : "0" };
    servers.push(
      Bun.serve({
        hostname: "127.0.0.1",
        port: 0,
        fetch(request) {
          const path = new URL(request.url).pathname;
          // The fake GitHub both servers read has the App installed on PRIV's repository.
          if (path.startsWith("/api/v1/github/app-installation/")) {
            return Response.json({ installed: path.endsWith("/acme/priv-repo") }, { headers });
          }
          const key = path.match(/^\/api\/v1\/issues\/([^/]+)$/)?.[1];
          if (key !== undefined) {
            if (key === "PRIV-1" && name === "main" && mainLeaksPriv) {
              return Response.json({ key, title: "The issue of PRIV" }, { headers });
            }
            return Response.json(
              { code: "ISSUE_NOT_FOUND", error: `issue ${key} not found` },
              { status: 404, headers }
            );
          }
          return new Response("not found", { status: 404, headers });
        },
      })
    );
  }
});

afterAll(() => {
  for (const server of servers) void server.stop(true);
});

/** The walk, as project-access.sh's walk section runs it over the GET rows, and what it counted
 *  and found. */
const driver = `
  set -euo pipefail
  work=$1
  call_seconds=5
  run_epoch=$(date +%s)
  note() { echo "   $*"; }
  fail() {
    echo "FAIL: $*" >&2
    exit 1
  }
  unrun() { printf '%s\\n' "$*" >>"$work/unrun"; }
  source "$2"
  source "$3"
  build_url[main]=$4
  build_url[twin]=$5
  walk_rows 'select(.method == "GET")'
  echo "walked $walked lists $lists differed $differed of $compared"
  cat "$work/differences" "$work/unrun"
`;

/** Runs the walk over the rows `probes` names, each a GET row of the route index, with the Probe
 *  and the read classes the server's own tables give it. */
async function walkRows(probes: readonly { pattern: string; probe: object; classes: string[] }[]) {
  const work = scratch();
  for (const directory of ["cookies", "auth", "answers"]) mkdirSync(join(work, directory));
  writeFileSync(
    join(work, "routes"),
    probes.map(({ pattern }) => JSON.stringify({ method: "GET", path: pattern })).join("\n")
  );
  writeFileSync(
    join(work, "probes.json"),
    JSON.stringify(probes.map((row) => ({ method: "GET", ...row })))
  );
  for (const server of ["main", "twin"]) {
    writeFileSync(
      join(work, "cookies", `${server}-alice`),
      "127.0.0.1\tFALSE\t/\tFALSE\t0\tdsession\talices-session\n"
    );
    for (const caller of ["alice-priv-token", "carol-token", "shared"]) {
      writeFileSync(join(work, "auth", `${server}-${caller}`), `Authorization: Bearer ${caller}\n`);
    }
    writeFileSync(join(work, `ids-${server}.tsv`), "");
  }
  for (const file of ["statuses", "differences", "unrun"]) writeFileSync(join(work, file), "");
  return runBounded({
    argv: [
      "bash",
      "-c",
      driver,
      "walk-class-none",
      work,
      library,
      walk,
      `http://127.0.0.1:${servers[0]?.port}`,
      `http://127.0.0.1:${servers[1]?.port}`,
    ],
    bound: "short",
    from: import.meta.url,
  });
}

const installationRow = {
  pattern: installation,
  probe: { owner: "repository-owner", repo: "repository-name" },
  classes: ["none"],
};
const issueRow = { pattern: issue, probe: { key: "issue" }, classes: ["project"] };

test(
  "a row whose only read class is none is read with PRIV's objects beside the twin, not beside a missing object",
  async () => {
    mainLeaksPriv = false;
    const run = await walkRows([installationRow]);
    expect(run.stderr).toBe("");
    expect(run.code).toBe(0);
    // One row, read as each of the four unseeing callers on both servers, and every pair alike.
    expect(run.stdout).toContain("walked 0 lists 1 differed 0 of 4");
  },
  rowLimit("short")
);

test(
  "a row of any other class is still held to a missing object's answer, and a leak there differs",
  async () => {
    mainLeaksPriv = false;
    const hidden = await walkRows([installationRow, issueRow]);
    expect(hidden.stderr).toBe("");
    expect(hidden.code).toBe(0);
    expect(hidden.stdout).toContain("walked 1 lists 1 differed 0 of 8");
    mainLeaksPriv = true;
    const leaked = await walkRows([installationRow, issueRow]);
    expect(leaked.stderr).toBe("");
    expect(leaked.code).toBe(0);
    expect(leaked.stdout).toContain("walked 1 lists 1 differed 4 of 8");
    expect(leaked.stdout).toContain(
      `alice GET ${issue}: hidden answered HTTP 200, missing HTTP 404`
    );
    expect(leaked.stdout).not.toContain(`GET ${installation}`);
  },
  rowLimit("short", "short")
);
