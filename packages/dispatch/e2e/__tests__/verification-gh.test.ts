/** The setup of `e2e/acceptance/verification.sh` reads the default-branch head of PROJECT's default
 *  repository with gh. Whatever answers gh's call in GitHub's place holds gh's token and writes
 *  gh's own words (its stderr) and its answer, so the setup prints neither, nor any of an answer
 *  but a commit: it keeps both in a file beside its work directory, which its failure names.
 *
 *  Each row runs the script itself against a stand-in Dispatch, which answers GET /api/v1/projects
 *  with the project EXAMPLE and its default repository, and a stand-in gh first on PATH, which
 *  answers the two reads as the row says and records each call. A stand-in bun ends the run at the
 *  first `dispatch` command after the setup, so no row runs the CLI. A row that puts the marker
 *  not-a-secret-token in gh's words or its answer holds that no run of eight of its characters
 *  (redacttest.Piece's run) is in what the script printed, stdout and stderr together; its twin,
 *  in the same row, holds that the marker did reach the script: the file the failure names holds
 *  it, or the next call of gh carries it. The control row, where gh answers a branch and a commit,
 *  holds that the stand-ins are the ones called and the script reads their commit. */

import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import type { Server } from "bun";
import { rowLimit, runBounded } from "./bounded-subprocess";

const dispatchPackage = fileURLToPath(new URL("../..", import.meta.url));
const scratch = temporaryDirectories("verification-gh");

const marker = "not-a-secret-token";
const repository = "example-owner/example-repo";
const commit = "0123456789abcdef0123456789abcdef01234567";
const branchCall = `api repos/${repository} --jq .default_branch`;
/** The call that reads the head of `branch`. */
const headCall = (branch: string) => `api repos/${repository}/commits/${branch} --jq .sha`;

/** Each run of eight characters of the marker that `text` holds. */
function markerRuns(text: string): string[] {
  const runs: string[] = [];
  for (let start = 0; start + 8 <= marker.length; start++) {
    const run = marker.slice(start, start + 8);
    if (text.includes(run)) runs.push(run);
  }
  return runs;
}

/** What the stand-in gh does for one read: prints `out` as its answer and `err` on stderr, and
 *  exits `code`. */
interface Read {
  readonly out: string;
  readonly err?: string;
  readonly code?: number;
}

/** The requests the stand-in Dispatch answered, `METHOD path`. */
const seen: string[] = [];
let dispatch: Server<undefined> | undefined;
beforeAll(() => {
  dispatch = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    fetch(request) {
      const url = new URL(request.url);
      seen.push(`${request.method} ${url.pathname}`);
      if (request.method === "GET" && url.pathname === "/api/v1/projects") {
        return Response.json([{ key: "EXAMPLE", default_repo: repository }]);
      }
      return new Response("no route", { status: 404 });
    },
  });
});
afterAll(() => {
  void dispatch?.stop(true);
});

/** One run of the script with the stand-ins, gh answering the branch read as `branch` and the head
 *  read as `head`: what it printed, stdout and stderr together, its exit code, the stand-in gh's
 *  calls, and the text of the file the failure names, or null where it names none. */
async function setupRun(
  branch: Read,
  head: Read
): Promise<{ code: number; output: string; calls: string[]; kept: string | null }> {
  const bin = scratch("bin");
  const gh = scratch("gh");
  for (const [name, read] of [
    ["branch", branch],
    ["head", head],
  ] as const) {
    writeFileSync(join(gh, `${name}.out`), read.out);
    writeFileSync(join(gh, `${name}.err`), read.err ?? "");
    writeFileSync(join(gh, `${name}.code`), String(read.code ?? 0));
  }
  writeFileSync(
    join(bin, "gh"),
    [
      "#!/usr/bin/env bash",
      `printf '%s\\n' "$*" >>'${gh}/calls'`,
      `case "$1 $2" in`,
      `  "api repos/${repository}") read=branch ;;`,
      `  "api repos/${repository}/commits/"*) read=head ;;`,
      `  *) echo "stand-in gh: no answer for this call" >&2; exit 3 ;;`,
      "esac",
      `cat '${gh}'/"$read.out"`,
      `cat '${gh}'/"$read.err" >&2`,
      `exit "$(cat '${gh}'/"$read.code")"`,
      "",
    ].join("\n"),
    { mode: 0o755 }
  );
  writeFileSync(
    join(bin, "bun"),
    "#!/bin/sh\necho 'stand-in bun: the run ends at its first dispatch command'\nexit 1\n",
    { mode: 0o755 }
  );
  const { code, stdout, stderr } = await runBounded({
    argv: ["bash", "e2e/acceptance/verification.sh"],
    bound: "short",
    cwd: dispatchPackage,
    env: {
      DISPATCH_URL: `http://127.0.0.1:${dispatch?.port}`,
      DISPATCH_TOKEN: "verification-gh-bearer",
      PROJECT: "EXAMPLE",
      HOME: scratch("home"),
      PATH: `${bin}:${process.env.PATH ?? ""}`,
      TMPDIR: scratch("tmp"),
    },
    from: import.meta.url,
  });
  const output = `${stdout}${stderr}`;
  const callsFile = join(gh, "calls");
  const calls = existsSync(callsFile)
    ? readFileSync(callsFile, "utf8")
        .split("\n")
        .filter((line) => line !== "")
    : [];
  const keptPath = /gh's answer and its own words are in (\S+)/.exec(output)?.[1];
  const kept =
    keptPath !== undefined && existsSync(keptPath) ? readFileSync(keptPath, "utf8") : null;
  return { code, output, calls, kept };
}

const failing: Read = { out: "", err: `gh: ${marker} (HTTP 502)\n`, code: 1 };
const notFound: Read = { out: "", err: "gh: Not Found (HTTP 404)\n", code: 1 };

describe.skipIf(skipWithout("bash", "curl", "jq", "sha256sum", "cut"))(
  "verification.sh's setup prints no word of gh's",
  () => {
    for (const row of [
      {
        name: "a branch read gh failed, saying the marker",
        branch: failing,
        head: { out: `${commit}\n` },
        prints: `FAIL: setup read ${repository}'s default branch with gh: gh failed or answered no branch; gh's answer and its own words are in `,
        calls: [branchCall],
      },
      {
        name: "a head read gh failed, saying the marker",
        branch: { out: "main\n" },
        head: failing,
        prints: `FAIL: setup read ${repository}'s default-branch head with gh: gh failed; gh's answer and its own words are in `,
        calls: [branchCall, headCall("main")],
      },
      {
        name: "a head read gh answered with the marker, which is no commit",
        branch: { out: "main\n" },
        head: { out: `${marker}\n` },
        prints: `FAIL: setup read ${repository}'s default-branch head with gh: gh answered no commit (40 lowercase hex digits); gh's answer and its own words are in `,
        calls: [branchCall, headCall("main")],
      },
      {
        name: "a branch gh named with the marker, whose head read then failed",
        branch: { out: `${marker}\n` },
        head: notFound,
        prints: `FAIL: setup read ${repository}'s default-branch head with gh: gh failed; gh's answer and its own words are in `,
        calls: [branchCall, headCall(marker)],
      },
      {
        name: "a branch gh named with the marker, whose head read then answered no commit",
        branch: { out: `${marker}\n` },
        head: { out: "main\n" },
        prints: `FAIL: setup read ${repository}'s default-branch head with gh: gh answered no commit (40 lowercase hex digits); gh's answer and its own words are in `,
        calls: [branchCall, headCall(marker)],
      },
    ]) {
      test(
        `${row.name}: the setup prints no run of the marker, and names the file that keeps it`,
        async () => {
          const run = await setupRun(row.branch, row.head);
          expect(markerRuns(run.output), `the script printed:\n${run.output}`).toEqual([]);
          expect(run.output).toContain(row.prints);
          expect(run.code).toBe(1);
          // The control: the stand-in gh was asked, and only for the reads the row names.
          expect(run.calls).toEqual(row.calls);
          // The twin: the file the failure names keeps the marker gh said or answered.
          expect(run.kept, `the script printed:\n${run.output}`).toContain(marker);
        },
        // The script, a bounded child under the short bound.
        rowLimit("short")
      );
    }

    test(
      "a branch gh named with the marker, then a commit: the setup prints the commit and no run of the marker",
      async () => {
        const run = await setupRun({ out: `${marker}\n` }, { out: `${commit}\n` });
        expect(markerRuns(run.output), `the script printed:\n${run.output}`).toEqual([]);
        expect(run.output).toContain(
          `PASS: setup read ${repository}'s default-branch head ${commit} with gh`
        );
        // The twin: the branch gh named reached the script, which read that branch's head.
        expect(run.calls).toEqual([branchCall, headCall(marker)]);
        expect(run.kept).toBeNull();
      },
      // The script, a bounded child under the short bound.
      rowLimit("short")
    );

    test(
      "the control: a branch and a commit gh answered take the run past the setup, at the stand-in's commit",
      async () => {
        const run = await setupRun({ out: "main\n" }, { out: `${commit}\n` });
        expect(seen).toContain("GET /api/v1/projects");
        expect(run.calls).toEqual([branchCall, headCall("main")]);
        expect(run.output).toContain(`${commit} with gh`);
        expect(run.output).toContain("stand-in bun: the run ends at its first dispatch command");
        expect(run.code).toBe(1);
      },
      // The script, a bounded child under the short bound.
      rowLimit("short")
    );
  }
);
