// Red test of t-ci-1-final2's D-2 (acceptance of cybermonklabs/legion#4184 at 7735fa52b4), now a
// file of this package's suite.
//
// The rule it holds, as merged with main's keyed-layer memory: a 2xx that names no key back is no
// answer of the call's where the process knows this Dispatch runs the keyed layer, whatever it
// holds, so the command prints that the write may or may not have taken effect, and exits 1. A
// cache or a front that answers a write by its URL with another call's stored answer, naming no
// key, is the case it is about: at 7735fa52b4 the command printed such an answer as this call's
// write ("Created ACME-7: Another call's title", "Posted comment <another call's id>", exit 0)
// while nothing of this call was stored, and both rows failed. Where the process knows nothing of
// the layer, the same answer is what a Dispatch without the layer answers, and the command prints
// it as stored: that case is each row's control.
//
// Run it from packages/envoy-client: `bun test src/__tests__/acceptance-tci1-final2-red.test.ts`.
import { afterAll, describe, expect, test } from "bun:test";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { leastAnswerFor } from "../../test/route-answers";
import { runDispatchCli } from "../dispatch-cli";
import { forgetKeyedLayerMemory, rememberKeyedLayer } from "../dispatch-http";

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
    for (const layer of ["known", "not known"] as const) {
      const known = layer === "known";
      test(`dispatch ${argv[0]}, the keyed layer ${layer}, prints it as ${known ? "no answer of its own" : "stored"}`, async () => {
        writes.length = 0;
        const origin = `http://127.0.0.1:${dispatch.port}`;
        forgetKeyedLayerMemory();
        if (known) rememberKeyedLayer({ [origin]: new Date().toISOString() });
        let out = "";
        const code = await runDispatchCli(
          [...argv],
          {
            PATH: process.env.PATH,
            HOME: state,
            DISPATCH_STATE_DIR: state,
            DISPATCH_HOST: "omp",
            DISPATCH_SESSION_ID: `acceptance-red-final2-${known ? "known" : "unknown"}`,
            DISPATCH_URL: origin,
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
        forgetKeyedLayerMemory();
        expect({
          asked: writes.length > 0,
          code,
          printsTheOtherCall: out.includes(other),
          unknown: out.includes("may or may not"),
        }).toEqual(
          known
            ? { asked: true, code: 1, printsTheOtherCall: false, unknown: true }
            : { asked: true, code: 0, printsTheOtherCall: true, unknown: false }
        );
      }, 30_000);
    }
  }
});
