/** How `e2e/acceptance/access-lib.sh`'s `same_answers` judges a hidden request beside a missing
 *  one (c-private-unseen), each answer kept by `keep_answer` and masked with its own fill. Two
 *  answers sent byte for byte alike are the same answer: a caller cannot tell them apart, whatever
 *  the masks would make of them. A refusal that names the caller's own token list names PRIV in
 *  both answers, and the hidden answer's mask, which replaces PRIV, would otherwise part them.
 *  Answers sent otherwise are compared masked, so a leak still fails. The headers are compared
 *  too, masked as the body is: a header whose value differs between two answers of one server
 *  (its date, its length, its digest) keeps its line with the value masked, so a header one
 *  answer sends and the other does not still differs, and a line the two servers are declared to
 *  answer apart is held to each one's own value and dropped before the compare.
 *
 *  Each row sources the library alone in bash, with a work directory of its own and no server, and
 *  reads the count of answers that differed. The rows of `event_log_reaches` and `indexed`, the
 *  library's two waits on a server, answer from a Bun server the row starts. */

import { afterAll, beforeAll, expect, test } from "bun:test";
import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import type { Server } from "bun";
import { rowLimit, runBounded } from "./bounded-subprocess";

const library = fileURLToPath(new URL("../acceptance/access-lib.sh", import.meta.url));
const scratch = temporaryDirectories("access-lib-same-answers");

/** A response's header block as curl's `-D` writes it. */
const headerBlock = (...lines: string[]) =>
  ["HTTP/1.1 403 Forbidden", ...lines, "", ""].join("\r\n");

interface Answer {
  readonly body: string;
  readonly headers?: string;
}

/** The count of differing answers `same_answers` reaches for one hidden and one missing answer,
 *  each a 403 whose body and headers are given, masked with the fills `key PRIV` and
 *  `key ZZNONE`, with the declared lines `lines`, the hidden answer's and the missing one's. */
async function differed(
  hidden: Answer,
  missing: Answer,
  lines: readonly [string, string] = ["", ""]
): Promise<number> {
  const work = scratch();
  mkdirSync(join(work, "answers"));
  // An answer given no headers sends a server's plain ones, its own date and length among them.
  for (const [name, answer, second] of [
    ["hidden", hidden, "00"],
    ["missing", missing, "01"],
  ] as const) {
    writeFileSync(
      join(work, `${name}.headers`),
      answer.headers ??
        headerBlock(
          "Content-Type: application/json",
          "Dispatch-Private-Projects: 1",
          `Date: Fri, 20 Nov 2026 09:00:${second} GMT`,
          `Content-Length: ${answer.body.length}`
        )
    );
  }
  const script = `
    set -euo pipefail
    work=$1
    run_epoch=$(date +%s)
    source "$2"
    : >"$work/ids-main.tsv"
    printf 'key\\tPRIV\\n' >"$work/fill-hidden"
    printf 'key\\tZZNONE\\n' >"$work/fill-missing"
    status=403
    printf '%s' "$3" >"$work/body"
    cp "$work/hidden.headers" "$work/headers"
    keep_answer hidden main "$work/fill-hidden"
    printf '%s' "$4" >"$work/body"
    cp "$work/missing.headers" "$work/headers"
    keep_answer missing main "$work/fill-missing"
    same_answers "the row" hidden missing "$5" "$6"
    printf '%s' "$differed"
  `;
  const run = await runBounded({
    argv: [
      "bash",
      "-c",
      script,
      "same-answers",
      work,
      library,
      hidden.body,
      missing.body,
      ...lines,
    ],
    bound: "short",
    from: import.meta.url,
  });
  expect(run.stderr).toBe("");
  expect(run.code).toBe(0);
  return Number(run.stdout);
}

const refusal = (key: string) =>
  `{"code":"PROJECT_SCOPE_DEPLOYMENT","error":"PUT /api/v1/projects/{key}/legion/daemon belongs to the deployment: this request's credential may write project ${key} and nothing else"}`;

test(
  "two answers sent alike are the same, though each one's mask would part them",
  async () => {
    // Both refusals name the caller's token list, PRIV, which the hidden answer's fill names too.
    expect(await differed({ body: refusal("PRIV") }, { body: refusal("PRIV") })).toBe(0);
  },
  rowLimit("short")
);

test(
  "two answers that name each request's own key are the same once each is masked",
  async () => {
    expect(await differed({ body: refusal("PRIV") }, { body: refusal("ZZNONE") })).toBe(0);
  },
  rowLimit("short")
);

test(
  "an answer that names a hidden object a missing one does not is a difference",
  async () => {
    expect(
      await differed(
        { body: '{"title":"The issue of PRIV","key":"PRIV"}' },
        { body: '{"code":"NOT_FOUND","key":"ZZNONE"}' }
      )
    ).toBe(1);
  },
  rowLimit("short")
);

test(
  "headers apart only in the date, the length and the digest are the same",
  async () => {
    const body = refusal("PRIV");
    const hidden = headerBlock(
      "Content-Type: application/json",
      "Date: Fri, 20 Nov 2026 09:00:00 GMT",
      "Content-Length: 10",
      'Etag: "aaaa"'
    );
    const missing = headerBlock(
      "Content-Type: application/json",
      "Date: Fri, 20 Nov 2026 09:00:07 GMT",
      "Content-Length: 12",
      'Etag: "bbbb"'
    );
    expect(await differed({ body, headers: hidden }, { body, headers: missing })).toBe(0);
  },
  rowLimit("short")
);

test(
  "a header one answer sends and the other does not is a difference, its value masked or not",
  async () => {
    const body = refusal("PRIV");
    const without = headerBlock("Content-Type: application/json", "Content-Length: 10");
    for (const extra of ['Etag: "aaaa"', "X-Planted: 1"]) {
      const withIt = headerBlock("Content-Type: application/json", "Content-Length: 10", extra);
      expect(await differed({ body, headers: withIt }, { body, headers: without })).toBe(1);
    }
  },
  rowLimit("short", "short")
);

test(
  "a header that names each request's own key is the same once masked, and one naming PRIV alone differs",
  async () => {
    const body = refusal("PRIV");
    const naming = (key: string) =>
      headerBlock("Content-Type: application/json", `Location: /api/v1/projects/${key}`);
    expect(
      await differed({ body, headers: naming("PRIV") }, { body, headers: naming("ZZNONE") })
    ).toBe(0);
    expect(
      await differed(
        { body, headers: naming("PRIV") },
        { body, headers: headerBlock("Content-Type: application/json") }
      )
    ).toBe(1);
  },
  rowLimit("short", "short")
);

test(
  "a line each server is declared to answer apart is dropped, and a server that answers another differs",
  async () => {
    const body = refusal("PRIV");
    const saying = (value: string) =>
      headerBlock("Content-Type: application/json", `Dispatch-Private-Projects: ${value}`);
    const declared = ["dispatch-private-projects: 1", "dispatch-private-projects: 0"] as const;
    expect(
      await differed({ body, headers: saying("1") }, { body, headers: saying("0") }, declared)
    ).toBe(0);
    expect(await differed({ body, headers: saying("1") }, { body, headers: saying("0") })).toBe(1);
    expect(
      await differed({ body, headers: saying("0") }, { body, headers: saying("0") }, declared)
    ).toBe(1);
  },
  rowLimit("short", "short", "short")
);

test(
  "a pair whose body and headers both differ is one answer that differs",
  async () => {
    expect(
      await differed(
        { body: '{"key":"PRIV","title":"seen"}', headers: headerBlock("X-Planted: 1") },
        { body: '{"code":"NOT_FOUND"}', headers: headerBlock() }
      )
    ).toBe(1);
  },
  rowLimit("short")
);

/** What the stand-in server answers GET /api/v1/events/log and GET /api/v1/search/coverage with,
 *  which each row sets, and the requests it took. */
let eventLog: { status: number; body: unknown } = { status: 200, body: [] };
let coverage: unknown = {};
const asked: string[] = [];
let server: Server<undefined> | undefined;

beforeAll(() => {
  server = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    fetch(request) {
      const url = new URL(request.url);
      asked.push(`${url.pathname}${url.search} ${request.headers.get("cookie") ?? ""}`);
      if (url.pathname === "/api/v1/events/log") {
        return Response.json(eventLog.body, { status: eventLog.status });
      }
      if (url.pathname === "/api/v1/search/coverage") return Response.json(coverage);
      return new Response("not found", { status: 404 });
    },
  });
});

afterAll(() => {
  void server?.stop(true);
});

/** Whether the library's CALL, a wait's one probe, holds against the stand-in server as `main`,
 *  signed in as carol by the cookie the jar holds. */
async function holds(call: string): Promise<boolean> {
  const work = scratch();
  mkdirSync(join(work, "cookies"));
  writeFileSync(
    join(work, "cookies", "main-carol"),
    "127.0.0.1\tFALSE\t/\tFALSE\t0\tdsession\tcarols-session\n"
  );
  const script = `
    set -euo pipefail
    work=$1
    call_seconds=5
    source "$2"
    build_url[main]=$3
    if ${call}; then echo holds; else echo fails; fi
  `;
  const run = await runBounded({
    argv: ["bash", "-c", script, "waits", work, library, `http://127.0.0.1:${server?.port}`],
    bound: "short",
    from: import.meta.url,
  });
  expect(run.stderr).toBe("");
  expect(run.code).toBe(0);
  return run.stdout.trim() === "holds";
}

test(
  "event_log_reaches holds once the caller's log reads an event at the id or above",
  async () => {
    asked.length = 0;
    eventLog = { status: 200, body: [{ id: 4 }] };
    expect(await holds("event_log_reaches main carol 5")).toBe(false);
    expect(asked).toEqual(["/api/v1/events/log?after=4&limit=1 dsession=carols-session"]);
    eventLog = { status: 200, body: [{ id: 5 }] };
    expect(await holds("event_log_reaches main carol 5")).toBe(true);
    eventLog = { status: 500, body: [{ id: 9 }] };
    expect(await holds("event_log_reaches main carol 5")).toBe(false);
  },
  rowLimit("short", "short", "short")
);

test(
  "indexed holds only once the build's meaning index counts items and owes none a vector",
  async () => {
    // The shape a build serves: a kind no search leg indexes is in not_indexed and never in the
    // totals, so it decides nothing.
    const counts = (items: number, lacking: number) => ({
      total: { items, current: items - lacking, lacking, failed: 0 },
      not_indexed: [{ kind: "handoff", items: 1 }],
    });
    coverage = counts(3, 1);
    expect(await holds("indexed main")).toBe(false);
    coverage = counts(3, 0);
    expect(await holds("indexed main")).toBe(true);
    coverage = counts(0, 0);
    expect(await holds("indexed main")).toBe(false);
  },
  rowLimit("short", "short", "short")
);
