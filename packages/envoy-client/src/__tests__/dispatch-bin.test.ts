import { afterAll, beforeEach, describe, expect, test } from "bun:test";
import { join } from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { fakeSearchResponse } from "./fake-search-response";

// The `dispatch` entry is its own process in production, and every omp session reads it through a
// pipe: Bun's `process.exit` drops what a pipe has not yet taken, so the entry must wait for its
// writes. These tests run the real entry and read its output through a pipe.

const ENTRY = join(import.meta.dir, "..", "..", "bin", "dispatch.ts");
const PROJECTS = 4000;

/** One answered ask, as a server from before the server-side fix sends it: its answer chose no
 *  option, and carries `selected: null` rather than an empty list. */
const ASK_ID = "aaaaaaaa-0000-4000-8000-000000000341";
const ASK_REF = `dispatch://CORE-7/ask/${ASK_ID}`;
const ANSWER_TEXT = "Ship the JSON API once the fix lands.";
const ANSWER = { user: "sami", selected: null, text: ANSWER_TEXT, at: "2026-09-09T00:00:00Z" };
const ASK = {
  id: ASK_ID,
  issue_key: "CORE-7",
  author: { kind: "session", id: "author-1" },
  question: "Which API should ship first?",
  options: [{ label: "JSON API" }, { label: "MCP API" }],
  multiple: false,
  urgency: "med",
  anchor: null,
  state: "answered",
  answer: ANSWER,
  created_at: "2026-09-08T00:00:00Z",
};
const QUERY = "json api";

const makeDirectory = temporaryDirectories("dispatch-bin");

/** Every request the stub served, as `METHOD /path?query`, since the last test began. */
let requests: string[] = [];

beforeEach(() => {
  requests = [];
});

const server = Bun.serve({
  port: 0,
  fetch(request) {
    const url = new URL(request.url);
    requests.push(`${request.method} ${url.pathname}${url.search}`);
    if (request.method !== "GET") return Response.json({ error: "unexpected" }, { status: 500 });
    if (url.pathname === "/api/v1/projects") {
      return Response.json(
        Array.from({ length: PROJECTS }, (_, index) => ({
          key: `P${index}`,
          name: `Project number ${index}`,
          open_asks: 0,
          gated_steps: [],
          created_at: "2026-09-01T00:00:00Z",
        }))
      );
    }
    // `dispatch read --ref` of a whole ask id reads the ask (`readAskRef`), then its reference
    // graph in both directions (`readCommand`); the ask's texts embed no picture to read.
    if (url.pathname === `/api/v1/asks/${ASK_ID}`) {
      return Response.json({ ask: ASK, replies: [], answers: [] });
    }
    if (url.pathname === "/api/v1/references") {
      return Response.json({ node: { kind: "ask", id: ASK_ID }, edges: [] });
    }
    // `dispatch search --query` is one read of `/api/v1/search` (`searchCommand`).
    if (url.pathname === "/api/v1/search") {
      return Response.json(
        fakeSearchResponse([
          {
            kind: "ask",
            owner: { kind: "issue", key: "CORE-7", title: "Public API", status: "in_progress" },
            id: ASK_ID,
            snippet: "Which <mark>API</mark> should ship first?",
            rank: 0.5,
            href: `/issues/CORE-7/ask/${ASK_ID}`,
            ask: { state: "answered", answer: ANSWER },
          },
        ])
      );
    }
    return Response.json({ error: "unexpected" }, { status: 500 });
  },
});

afterAll(() => server.stop(true));

async function runEntry(
  args: readonly string[]
): Promise<{ code: number; stdout: Uint8Array; stderr: string }> {
  const state = makeDirectory();
  // Bun.spawn's own stdout pipe drains fast enough to hide the loss, so the entry writes into a
  // shell pipe to `cat`, the way a bash tool captures it; pipefail keeps the entry's exit code.
  // The shell runs the same Bun as the test, not whatever `bun` its PATH resolves, and passes the
  // command's own arguments on after the entry.
  const child = Bun.spawn(
    [
      "bash",
      "-c",
      'set -o pipefail; bun=$1; shift; "$bun" "$0" "$@" | cat',
      ENTRY,
      process.execPath,
      ...args,
    ],
    {
      env: {
        PATH: process.env.PATH,
        HOME: state,
        DISPATCH_STATE_DIR: state,
        DISPATCH_HOST: "omp",
        DISPATCH_SESSION_ID: "ses-bin",
        DISPATCH_URL: `http://127.0.0.1:${server.port}`,
        DISPATCH_TOKEN: "test-token",
      },
      stdout: "pipe",
      stderr: "pipe",
    }
  );
  const [stdout, stderr, code] = await Promise.all([
    // Bun 1.3 returns an ArrayBuffer from `.bytes()` for some chunkings, so wrap the buffer.
    new Response(child.stdout).arrayBuffer().then((buffer) => new Uint8Array(buffer)),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  return { code, stdout, stderr };
}

describe("the dispatch entry", () => {
  // The 30 s limit is a hang guard, not a budget: the row runs the entry as a child process and
  // reads its whole pipe, and bun's unwritten 5,000 ms per-test default is a limit on that work
  // rather than on a hang. Measured in a 2-CPU pod: 0.36 s idle; under 16 busy loops (8 x nproc),
  // 6.71 s at most in a whole-suite run and 6.94 s with this file alone, which passes that
  // default. It fires only if the child never exits or its pipe never ends. The rows below run
  // the same entry against the same stub, and take the same guard.
  test("writes all of its output to a pipe, however long", async () => {
    const { code, stdout, stderr } = await runEntry(["projects"]);
    expect(stderr).toBe("");
    expect(code).toBe(0);
    const text = new TextDecoder().decode(stdout);
    expect(stdout.length).toBeGreaterThan(64 * 1024);
    expect(text).toContain(`P${PROJECTS - 1}`);
    expect(text.endsWith("\n")).toBe(true);
  }, 30_000);

  // A server from before the server-side fix sends an answer that chose no option with
  // `selected: null`. Each read that prints the answer still prints its text, and exits 0. The
  // printed text is read first, so a read that failed shows what it printed instead.
  test("dispatch read --ref prints an answer whose selected is null, and exits 0", async () => {
    const { code, stdout, stderr } = await runEntry(["read", "--ref", ASK_REF]);
    const text = new TextDecoder().decode(stdout);
    expect(text).toContain(`- Text: ${ANSWER_TEXT}\n`);
    expect(text).toContain("- Selected: none\n");
    expect({ code, stderr }).toEqual({ code: 0, stderr: "" });
    // The two directions of the graph are read at once, so their order is not the read's.
    expect([...requests].sort()).toEqual([
      `GET /api/v1/asks/${ASK_ID}`,
      `GET /api/v1/references?${new URLSearchParams({ from: ASK_REF })}`,
      `GET /api/v1/references?${new URLSearchParams({ to: ASK_REF })}`,
    ]);
  }, 30_000);

  test("dispatch search prints an answer whose selected is null, and exits 0", async () => {
    const { code, stdout, stderr } = await runEntry(["search", "--query", QUERY]);
    const text = new TextDecoder().decode(stdout);
    expect(text).toContain(`  Answer text: ${ANSWER_TEXT}\n`);
    expect(text).toContain("options chosen: none\n");
    expect({ code, stderr }).toEqual({ code: 0, stderr: "" });
    expect(requests).toEqual([`GET /api/v1/search?${new URLSearchParams({ q: QUERY })}`]);
  }, 30_000);
});
