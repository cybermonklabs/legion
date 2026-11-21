/** How `e2e/acceptance/access-lib.sh`'s `await_index_settled` holds a driver's compare of two
 *  servers until both have settled their meaning index, so the two are never read while one is
 *  still windowing or embedding what a write left it: on CI one server answered a block
 *  `"embedding":"embedded"` where the other answered `"pending"`, and search coverage
 *  `"current":1` where the other answered 2.
 *
 *  Each row starts two stand-in servers, `main` and `twin`, as a driver's two Dispatch servers,
 *  and a stand-in `psql` for each one's database, and runs a driver of its own in bash that
 *  sources the library and calls the wait. Each stand-in answers its coverage route from a list
 *  of reports, one a read, keeping the last: lacking for a few reads and then nothing waiting, or
 *  lacking for ever. Its blocks read `"pending"` until it has answered the last report, and
 *  `"embedded"` after. The stand-in `psql` answers each server's window markers from a list the
 *  row writes, one line a read, the last kept. A row's driver compares the two servers as
 *  project-access.sh's walk does, the coverage route and a document's blocks beside each other,
 *  so a compare made while either is still working differs. */

import { afterAll, beforeAll, expect, test } from "bun:test";
import { chmodSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import type { Server } from "bun";
import { rowLimit, runBounded } from "./bounded-subprocess";

const library = fileURLToPath(new URL("../acceptance/access-lib.sh", import.meta.url));
const scratch = temporaryDirectories("access-lib-index-settled");

/** The counts one kind, or the total, of a coverage report holds. */
interface Counts {
  readonly items: number;
  readonly current: number;
  readonly lacking: number;
  readonly failed: number;
  readonly oldest_lacking_seconds: number | null;
}

const counts = (items: number, current: number, failed = 0): Counts => ({
  items,
  current,
  lacking: items - current - failed,
  failed,
  oldest_lacking_seconds: items - current - failed > 0 ? 4 : null,
});
const nothing = counts(0, 0);

/** A coverage report as GET /api/v1/search/coverage answers it, with its three items in two
 *  kinds: one issue and two windows of a document's blocks, of which `current` hold a current
 *  vector and `failed` were given up on. The issue is embedded first. `identity` is the
 *  embedder's, null where the server runs none. */
function report(current: number, failed = 0, identity: string | null = "dev:hashed-words-v1") {
  const issue = Math.min(current, 1);
  return {
    identity,
    place: identity === null ? null : "dev",
    model: identity === null ? null : "hashed-words-v1",
    total: counts(3, current, failed),
    kinds: {
      issue: counts(1, issue),
      document_block: counts(2, current - issue, failed),
      comment_block: nothing,
      message_block: nothing,
      ask_part: nothing,
    },
    not_indexed: [{ kind: "handoff", items: 1 }],
  };
}

/** What one stand-in server answers: its coverage reports, one a read and the last kept, or a
 *  status it answers every coverage read with; and how many coverage reads it has answered. */
interface Stand {
  coverage: readonly unknown[] | { readonly status: number };
  reads: number;
}

const stands: Record<"main" | "twin", Stand> = {
  main: { coverage: [report(3)], reads: 0 },
  twin: { coverage: [report(3)], reads: 0 },
};
const servers: Server<undefined>[] = [];

beforeAll(() => {
  for (const name of ["main", "twin"] as const) {
    servers.push(
      Bun.serve({
        hostname: "127.0.0.1",
        port: 0,
        fetch(request) {
          const stand = stands[name];
          const url = new URL(request.url);
          if (url.pathname === "/api/v1/search/coverage") {
            // `in` narrows the read-only list away, which Array.isArray does not.
            if ("status" in stand.coverage) {
              return Response.json({ code: "SEARCH_COVERAGE_TIMEOUT" }, stand.coverage);
            }
            stand.reads++;
            return Response.json(stand.coverage[Math.min(stand.reads, stand.coverage.length) - 1]);
          }
          if (url.pathname === "/api/v1/artifacts/spec/blocks") {
            return Response.json([
              {
                id: "b-first-paragraph",
                type: "paragraph",
                from: 0,
                to: 20,
                // A stand-in that has answered its last report has settled its index.
                embedding:
                  Array.isArray(stand.coverage) && stand.reads >= stand.coverage.length
                    ? "embedded"
                    : "pending",
              },
            ]);
          }
          return new Response("not found", { status: 404 });
        },
      })
    );
  }
});

afterAll(() => {
  for (const server of servers) void server.stop(true);
});

/** The stand-in psql: it answers the window-marker read of `stub://NAME` with the first line of
 *  $MARKERS/NAME, which it then drops unless it is the last, and writes NAME to $MARKERS/reads. */
const psql = `#!/usr/bin/env bash
name=\${1#stub://}
file=$MARKERS/$name
printf '%s\\n' "$name" >>"$MARKERS/reads"
first=$(head -n 1 "$file")
if [ "$(wc -l <"$file")" -gt 1 ]; then
  tail -n +2 "$file" >"$file.rest" && mv "$file.rest" "$file"
fi
printf '%s\\n' "$first"
`;

/** The driver each row runs: the wait, unless WAIT is empty, and then the compare of the two
 *  servers' coverage and blocks, beside each other, as project-access.sh's walk compares them. */
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
  source "$2"
  build_url[main]=$3
  build_url[twin]=$4
  main_database_url=stub://main
  twin_database_url=stub://twin
  if [ -n "$5" ]; then await_index_settled "$5" main twin; fi
  for path in /api/v1/search/coverage /api/v1/artifacts/spec/blocks; do
    for server in main twin; do
      send "$server" carol GET "$path"
      keep_answer "$server" "$server"
    done
    same_answers "carol GET $path (beside the twin)" main twin
  done
  echo "differed $differed of $compared"
  [ "$differed" -eq 0 ] || cat "$work/differences"
`;

interface Run {
  readonly code: number;
  readonly stdout: string;
  readonly stderr: string;
  /** How long the run took, in seconds. */
  readonly seconds: number;
  /** The servers whose window markers the stand-in psql was asked for, in order. */
  readonly markerReads: readonly string[];
}

/** Runs the driver against the stand-ins as they are set, the wait bounded at `bound` seconds
 *  (or no wait, given ""), with `markers` the lines each server's window markers answer, one a
 *  read: "DOCUMENTS COMMENTS MESSAGES ASKS" owed their windows. */
async function drive(
  bound: string,
  markers: Record<"main" | "twin", readonly string[]> = { main: ["0 0 0 0"], twin: ["0 0 0 0"] }
): Promise<Run> {
  const work = scratch();
  const bin = join(work, "bin");
  const markerDirectory = join(work, "markers");
  for (const directory of [bin, markerDirectory, join(work, "cookies"), join(work, "answers")]) {
    mkdirSync(directory);
  }
  writeFileSync(join(bin, "psql"), psql);
  chmodSync(join(bin, "psql"), 0o755);
  writeFileSync(join(markerDirectory, "reads"), "");
  for (const name of ["main", "twin"] as const) {
    writeFileSync(join(markerDirectory, name), `${markers[name].join("\n")}\n`);
    writeFileSync(
      join(work, "cookies", `${name}-carol`),
      "127.0.0.1\tFALSE\t/\tFALSE\t0\tdsession\tcarols-session\n"
    );
    writeFileSync(join(work, `ids-${name}.tsv`), "");
  }
  writeFileSync(join(work, "statuses"), "");
  writeFileSync(join(work, "differences"), "");
  const started = performance.now();
  const run = await runBounded({
    argv: [
      "bash",
      "-c",
      driver,
      "index-settled",
      work,
      library,
      `http://127.0.0.1:${servers[0]?.port}`,
      `http://127.0.0.1:${servers[1]?.port}`,
      bound,
    ],
    env: { ...process.env, PATH: `${bin}:${process.env.PATH ?? ""}`, MARKERS: markerDirectory },
    bound: "short",
    from: import.meta.url,
  });
  return {
    ...run,
    seconds: (performance.now() - started) / 1000,
    markerReads: readFileSync(join(markerDirectory, "reads"), "utf8").split("\n").filter(Boolean),
  };
}

/** Sets each stand-in's coverage and clears its count of reads. */
function arm(main: Stand["coverage"], twin: Stand["coverage"]): void {
  stands.main = { coverage: main, reads: 0 };
  stands.twin = { coverage: twin, reads: 0 };
}

test(
  "the wait reads both servers until both have nothing waiting, and the compare then reads them alike",
  async () => {
    // main embeds a step ahead of the twin, as two servers working on their own clocks do: read
    // at once, the two count different vectors current.
    arm([report(1), report(2), report(3)], [report(0), report(0), report(1), report(2), report(3)]);
    const run = await drive("20");
    expect(run.stderr).toBe("");
    expect(run.code).toBe(0);
    expect(run.stdout).toContain("differed 0 of 2");
    // Each server was read until it answered its last report, and once more by the compare: the
    // wait read main three times and the twin five, and returned only then.
    expect(stands.main.reads).toBe(4);
    expect(stands.twin.reads).toBe(6);
    expect(run.stdout).toContain("main's meaning index settled after");
    expect(run.stdout).toMatch(/main's meaning index settled after \d+s \(3 polls\)/);
    expect(run.stdout).toMatch(/twin's meaning index settled after \d+s \(5 polls\)/);
  },
  rowLimit("short")
);

test(
  "a compare with no wait reads the two servers while one is still embedding, and differs",
  async () => {
    // The row above with the wait left out, as the drivers compared before: this is what the
    // wait is for, and what the row above would read if it returned at once.
    arm([report(1), report(2), report(3)], [report(0), report(0), report(1), report(2), report(3)]);
    const run = await drive("");
    expect(run.stderr).toBe("");
    expect(run.code).toBe(0);
    expect(run.stdout).toContain("differed 1 of 2");
    expect(run.stdout).toContain('"current":1');
    expect(run.stdout).toContain('"current":0');
  },
  rowLimit("short")
);

test(
  "a server that never settles fails the run within the bound, naming it and what it still lacks",
  async () => {
    arm([report(1), report(3)], [report(0), report(1, 1)]);
    const run = await drive("2");
    expect(run.code).toBe(1);
    expect(run.seconds).toBeLessThan(2 + 10);
    expect(run.stderr).toContain("FAIL: timed out after 2s");
    expect(run.stderr).toContain(
      "twin: its coverage route counts 3 items, 1 current, 1 lacking, 1 failed in all: document_block 2 items, 0 current, 1 lacking, 1 failed"
    );
    // main settled and is named as settled, not among the servers the failure names.
    expect(run.stdout).toMatch(/main's meaning index settled after \d+s \(2 polls\)/);
    expect(run.stderr).not.toContain("main:");
    expect(run.stdout).not.toContain("differed");
  },
  rowLimit("short")
);

test(
  "a server whose coverage has nothing lacking is not settled while its window arm still owes windows",
  async () => {
    // The coverage route counts a window kind's windows, so an item the window arm has not
    // windowed yet is no item of its: main's coverage reads nothing lacking from the first read,
    // and its markers owe two comments their windows for three reads.
    arm([report(3)], [report(3)]);
    const run = await drive("20", {
      main: ["0 2 0 0", "0 2 0 0", "0 2 0 0", "0 0 0 0"],
      twin: ["0 0 0 0"],
    });
    expect(run.stderr).toBe("");
    expect(run.code).toBe(0);
    expect(run.stdout).toMatch(/main's meaning index settled after \d+s \(4 polls\)/);
    expect(run.stdout).toMatch(/twin's meaning index settled after \d+s \(1 polls\)/);
    // The markers are read first, and the route only once they owe nothing.
    expect(run.markerReads.filter((name) => name === "main")).toHaveLength(4);
    expect(stands.main.reads).toBe(1 + 1);
    // Markers that never clear fail the run, naming what the arm still owes.
    arm([report(3)], [report(3)]);
    const stuck = await drive("1", { main: ["1 0 2 0"], twin: ["0 0 0 0"] });
    expect(stuck.code).toBe(1);
    expect(stuck.stderr).toContain(
      "main: its window arm owes 1 documents, 0 comments, 2 messages and 0 asks their windows"
    );
  },
  rowLimit("short", "short")
);

test(
  "a server that runs no embedder is not waited on for vectors, and is waited on for its windows",
  async () => {
    // The twin's coverage names no identity: nothing it counts will ever be current.
    arm([report(3)], [report(0, 0, null)]);
    const run = await drive("20", { main: ["0 0 0 0"], twin: ["0 0 1 0", "0 0 1 0", "0 0 0 0"] });
    expect(run.stderr).toBe("");
    expect(run.code).toBe(0);
    expect(run.stdout).toMatch(/twin's meaning index settled after \d+s \(3 polls\)/);
    expect(stands.twin.reads).toBe(1 + 1);
  },
  rowLimit("short")
);

test(
  "a coverage route that counts no item, or answers no report, does not settle its server",
  async () => {
    arm([{ ...report(0), total: nothing }], { status: 503 });
    const run = await drive("1");
    expect(run.code).toBe(1);
    expect(run.stderr).toContain(
      "main: its coverage route counts no item, so it says nothing of what a compare reads"
    );
    expect(run.stderr).toContain(
      'twin: GET /api/v1/search/coverage answered HTTP 503: {"code":"SEARCH_COVERAGE_TIMEOUT"}'
    );
  },
  rowLimit("short")
);
