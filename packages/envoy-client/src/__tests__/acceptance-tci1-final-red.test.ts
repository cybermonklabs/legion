// Red test of t-ci-1-final's D-1 (acceptance of cybermonklabs/legion#4184 at eeec17b2e3).
//
// An empty id flag that the command puts in a URL path, as an empty variable of a step writes it,
// is refused before any request, naming the flag, as the head now does for --issue, --project and
// --check. Today `dispatch result --retract ""` asks `POST /api/v1/results//retract` and
// `dispatch resolve-comment --comment ""` asks `POST /api/v1/comments//resolve`. Dispatch's own
// router (Go's http.ServeMux) answers each with a 307 to the cleaned path, and the command prints
// "...answered 307 Temporary Redirect ..., redirecting to <Dispatch's own origin>, which looks like a
// proxy or gateway page rather than Dispatch's own answer, so a retry gets the same answer until
// the Dispatch URL, or whatever answers in its place, is fixed.": exit 1, with advice to fix a URL
// that is right, and no word of the empty flag.
//
// Run it: copy this file to packages/envoy-client/src/__tests__/ and, from packages/envoy-client,
// `bun test src/__tests__/acceptance-tci1-final-red.test.ts`.
import { afterAll, describe, expect, test } from "bun:test";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { runDispatchCli } from "../dispatch-cli";
import { parseCommand } from "../dispatch-command";

const noFiles = {
  readText: (path: string): string => {
    throw new Error(`unexpected read ${path}`);
  },
};

// A stand-in that answers as Dispatch's router does: a path with an empty segment is answered 307
// to its cleaned form, and every other request 404 with Dispatch's error shape.
const asked: string[] = [];
const dispatch = Bun.serve({
  hostname: "127.0.0.1",
  port: 0,
  async fetch(request) {
    await request.arrayBuffer();
    const url = new URL(request.url);
    asked.push(`${request.method} ${url.pathname}`);
    if (url.pathname.includes("//")) {
      return new Response(null, {
        status: 307,
        headers: { Location: url.pathname.replace(/\/{2,}/g, "/") },
      });
    }
    return Response.json({ code: "NOT_FOUND", error: "not found" }, { status: 404 });
  },
});
const state = mkdtempSync(join(tmpdir(), "acceptance-tci1-final-"));
afterAll(() => {
  dispatch.stop(true);
  rmSync(state, { recursive: true, force: true });
});

const CASES = [
  { argv: ["result", "--retract", "", "--reason", "Wrong commit."], flag: "--retract" },
  { argv: ["resolve-comment", "--comment", ""], flag: "--comment" },
] as const;

describe("an empty id flag that goes into a URL path", () => {
  for (const { argv, flag } of CASES) {
    test(`${argv.join(" ")} is refused by the parse, naming ${flag}`, () => {
      const parsed = parseCommand(argv, noFiles);
      expect(parsed.kind).toBe("refused");
      const problems = parsed.kind === "refused" ? parsed.problems : [];
      expect(problems.some((problem) => problem.startsWith(`${flag} is empty`))).toBe(true);
    });

    test(`${argv.join(" ")} sends nothing, exits 2 and names ${flag}, never the Dispatch URL`, async () => {
      asked.length = 0;
      let out = "";
      const code = await runDispatchCli(
        argv,
        {
          PATH: process.env.PATH,
          HOME: state,
          DISPATCH_STATE_DIR: state,
          DISPATCH_HOST: "omp",
          DISPATCH_SESSION_ID: "acceptance-red",
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
        code,
        asked,
        namesFlag: out.includes(`${flag} is empty`),
        blamesUrl: out.includes("until the Dispatch URL"),
      }).toEqual({
        code: 2,
        asked: [],
        namesFlag: true,
        blamesUrl: false,
      });
    }, 30_000);
  }
});
