// Red test of t-ci-1-final2's D-2 (acceptance of cybermonklabs/legion#4184 at 7735fa52b4).
//
// The coordinator's rule: a 2xx that names no key back counts as stored only when the answer holds
// what this call sent; each other answer prints write_outcome_unknown. A cache or a front that
// answers a write by its URL with another call's stored answer, naming no key, is the case the
// rule is for. The head compares only what `rowHolds` declares, and for most writes that is the
// key of the URL's own path, or for `dispatch issue` the project alone, which another call to the
// same URL always holds. So the command prints another call's write as this call's: "Created
// ACME-7: Another call's title" and "Posted comment <another call's id>", exit 0, while nothing of
// this call was stored.
//
// Run it: copy this file to packages/envoy-client/src/__tests__/ and, from packages/envoy-client,
// `bun test src/__tests__/acceptance-tci1-final2-red.test.ts`.
import { afterAll, describe, expect, test } from "bun:test";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { leastAnswerFor } from "../../test/route-answers";
import { runDispatchCli } from "../dispatch-cli";

// Another call's stored answer of each write, as a cache keyed by the URL keeps it: the same
// project or issue as this call's URL, another call's title, body and id. It names no key back.
const OTHER_CALLS: Record<string, Record<string, unknown>> = {
  "POST /api/v1/issues": { key: "ACME-7", project: "ACME", title: "Another call's title" },
  "POST /api/v1/issues/DSP-1/comments": {
    id: "0bbbbbbb-0000-4000-8000-000000000007",
    issue_key: "DSP-1",
    body: "Another call's body",
  },
};
const writes: string[] = [];
const dispatch = Bun.serve({
  hostname: "127.0.0.1",
  port: 0,
  async fetch(request) {
    await request.arrayBuffer();
    const url = new URL(request.url);
    const route = `${request.method} ${url.pathname}`;
    if (url.pathname === "/healthz") return Response.json({ ok: true, idempotency_keys: true });
    let answer: { status: number; body: unknown } | undefined;
    try {
      answer = leastAnswerFor(request.method, url.pathname);
    } catch {
      answer = { status: 200, body: [] };
    }
    if (answer === undefined) {
      return Response.json({ code: "NOT_FOUND", error: `no route for ${route}` }, { status: 404 });
    }
    const other = OTHER_CALLS[route];
    if (other === undefined) return Response.json(answer.body, { status: answer.status });
    writes.push(route);
    const body = { ...(answer.body as Record<string, unknown>), ...other };
    return Response.json(body, { status: answer.status });
  },
});
const state = mkdtempSync(join(tmpdir(), "acceptance-tci1-final2-"));
afterAll(() => {
  dispatch.stop(true);
  rmSync(state, { recursive: true, force: true });
});

const CASES = [
  {
    argv: ["issue", "--project", "ACME", "--title", "This call's title", "--force"],
    other: "Another call's title",
  },
  {
    argv: ["comment", "--issue", "DSP-1", "--body", "This call's body"],
    other: "0bbbbbbb-0000-4000-8000-000000000007",
  },
] as const;

describe("another call's stored answer, naming no key back", () => {
  for (const { argv, other } of CASES) {
    test(`dispatch ${argv[0]} prints it as no answer of its own, never as stored`, async () => {
      writes.length = 0;
      let out = "";
      const code = await runDispatchCli(
        [...argv],
        {
          PATH: process.env.PATH,
          HOME: state,
          DISPATCH_STATE_DIR: state,
          DISPATCH_HOST: "omp",
          DISPATCH_SESSION_ID: "acceptance-red-final2",
          DISPATCH_URL: `http://127.0.0.1:${dispatch.port}`,
          DISPATCH_TOKEN: fixtureSecret("session"),
        },
        {
          stdout: (text: string) => {
            out += text;
          },
          stderr: (text: string) => {
            out += text;
          },
          readText: (path: string) => readFileSync(path, "utf-8"),
          cwd: state,
        }
      );
      expect({
        asked: writes.length > 0,
        code,
        printsTheOtherCall: out.includes(other),
        unknown: out.includes("may or may not"),
      }).toEqual({ asked: true, code: 1, printsTheOtherCall: false, unknown: true });
    }, 30_000);
  }
});
