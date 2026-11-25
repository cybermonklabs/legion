import { afterAll, beforeAll, describe, expect, setDefaultTimeout, test } from "bun:test";
import { writeFileSync } from "node:fs";
import { join } from "node:path";
import { HANG_GUARD_MS } from "@legion/test-fixtures/hang-guard";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";

/**
 * Where `askClaude` asks the model gateway: the Messages route joined onto ANTHROPIC_BASE_URL's
 * path, its query kept, against a stand-in gateway served under a path and a query that answers
 * 404 anywhere else, as a gateway behind a path-routed proxy would.
 *
 * `askClaude` reads its key from a fixed file this machine need not hold, so each call runs in a
 * child Bun that answers that one read with a made-up key (`mock.module` of `node:fs/promises`,
 * which would reach every later file of this process if it ran here) and prints what the call
 * returned, or the message it threw.
 */

// The rows wait on a child process and a local server, which a loaded machine slows. Their bound
// is a hang guard, not a verdict.
setDefaultTimeout(HANG_GUARD_MS);

const MODELS = join(import.meta.dir, "..", "models.ts");
const KEY_FILE = "/var/run/model-gateway-key/key";
const MODEL = "anthropic/claude-made-up";

/** Every request the stand-in took, as `<method> <path><query>`. */
const asked: string[] = [];
/** Where the stand-in is served: under `mount`, with `query` on every request. */
let served = { mount: "", query: "" };

const server = Bun.serve({
  hostname: "127.0.0.1",
  port: 0,
  async fetch(request) {
    const url = new URL(request.url);
    asked.push(`${request.method} ${url.pathname}${url.search}`);
    await request.text();
    if (
      request.method === "POST" &&
      url.pathname === `${served.mount}/v1/messages` &&
      url.search === served.query
    ) {
      return Response.json({
        model: MODEL,
        content: [{ type: "text", text: "Yes" }],
        stop_reason: "end_turn",
        usage: { input_tokens: 3, output_tokens: 1 },
      });
    }
    return Response.json({ type: "error", error: { message: "not found" } }, { status: 404 });
  },
});
const origin = `http://127.0.0.1:${server.port}`;

const directory = temporaryDirectory("approval-eval-models-url-join");
const child = join(directory, "ask-claude.ts");

beforeAll(() => {
  writeFileSync(
    child,
    `import { mock } from "bun:test";
import * as files from "node:fs/promises";
const readFile = files.readFile as (...args: unknown[]) => unknown;
mock.module("node:fs/promises", () => ({
  ...files,
  readFile: (path: unknown, ...rest: unknown[]) =>
    path === ${JSON.stringify(KEY_FILE)} ? Promise.resolve("gateway-key-made-up\\n") : readFile(path, ...rest),
}));
// Imported after the mock above, which a static import would precede: models.ts then reads the
// mocked module from its first line.
const { askClaude } = await import(${JSON.stringify(MODELS)});
try {
  const answer = await askClaude({
    model: ${JSON.stringify(MODEL)},
    prompt: { system: "Answer Yes or No.", user: "Yes?", labels: ["Yes", "No"] },
    bounds: { timeMs: 10_000 },
  });
  process.stdout.write(JSON.stringify({ chosen: answer.chosen, fault: answer.fault }));
} catch (error) {
  process.stdout.write(JSON.stringify({ threw: error instanceof Error ? error.message : String(error) }));
}
`
  );
});

afterAll(() => server.stop(true));

/** What one `askClaude` under ANTHROPIC_BASE_URL `base` asked, the stand-in served under `mount`
 *  and `query`, and what it printed. */
async function askedBy(base: string, mount: string, query: string) {
  served = { mount, query };
  asked.length = 0;
  const run = Bun.spawn([process.execPath, child], {
    env: { ANTHROPIC_BASE_URL: base, HOME: directory, PATH: process.env.PATH ?? "" },
    stderr: "pipe",
    stdout: "pipe",
    // A call the stand-in does not answer retries for half a minute; the rows need none of it.
    timeout: 20_000,
  });
  const [stdout, stderr] = await Promise.all([
    new Response(run.stdout).text(),
    new Response(run.stderr).text(),
    run.exited,
  ]);
  return { asked: [...asked], printed: stdout === "" ? { stderr } : JSON.parse(stdout) };
}

describe("askClaude's request", () => {
  test("joins the Messages route onto a configured URL's path and keeps its query", async () => {
    const { asked, printed } = await askedBy(
      `${origin}/prefix?key=k3y-made-up`,
      "/prefix",
      "?key=k3y-made-up"
    );
    expect(asked).toEqual(["POST /prefix/v1/messages?key=k3y-made-up"]);
    expect(printed).toEqual({ chosen: ["Yes"], fault: null });
  });

  test("keeps an ANTHROPIC_BASE_URL query whole, a trailing slash included", async () => {
    const { asked, printed } = await askedBy(
      `${origin}/prefix/?key=k3y-made-up/`,
      "/prefix",
      "?key=k3y-made-up/"
    );
    expect(asked).toEqual(["POST /prefix/v1/messages?key=k3y-made-up/"]);
    expect(printed).toEqual({ chosen: ["Yes"], fault: null });
  });

  test("asks a plain origin at the Messages route itself", async () => {
    const { asked, printed } = await askedBy(origin, "", "");
    expect(asked).toEqual(["POST /v1/messages"]);
    expect(printed).toEqual({ chosen: ["Yes"], fault: null });
  });

  test("refuses an ANTHROPIC_BASE_URL that is no absolute URL, quoting none of it", async () => {
    const { asked, printed } = await askedBy("not-a-url?key=k3y-made-up", "", "");
    expect(asked).toEqual([]);
    expect(printed).toEqual({
      threw: "ANTHROPIC_BASE_URL is not an absolute URL with a host (not printed)",
    });
  });
});
