import { afterAll, describe, expect, test } from "bun:test";
import { join } from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";

// The `dispatch` entry is its own process in production, and every omp session reads it through a
// pipe: Bun's `process.exit` drops what a pipe has not yet taken, so the entry must wait for its
// writes. This test runs the real entry and reads its output through a pipe, and holds that stored
// content or a body that is not JSON reaches the pipe as its bytes, unchanged (`stdoutBytes`).

const ENTRY = join(import.meta.dir, "..", "..", "bin", "dispatch.ts");
const PROJECTS = 4000;

/** A picture version's route, and its bytes: a PNG's signature, then two bytes no UTF-8 text
 *  holds, which a round trip through text would change. */
const PICTURE_VERSION = "/api/v1/artifacts/01234567-0000-4000-8000-000000000003/versions/1";
const PNG = Uint8Array.of(0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0xff, 0xfe);
/** A file version stored as JSON, and its bytes: an integer past 2^53 and a decimal with a
 *  trailing zero, which a parse and a print again would change. */
const DATA_VERSION = "/api/v1/artifacts/01234567-0000-4000-8000-000000000004/versions/1";
const DATA_JSON = new TextEncoder().encode('{"id":12345678901234567890,"ratio":1.10,"name":"x"}');

/** Each file version the stub answers, as `serveFileVersion` does: its stored bytes under its
 *  stored type, as an attachment. */
const FILES: Readonly<
  Record<string, { readonly bytes: Uint8Array<ArrayBuffer>; readonly type: string }>
> = {
  [PICTURE_VERSION]: { bytes: PNG, type: "image/png" },
  [DATA_VERSION]: { bytes: DATA_JSON, type: "application/json;charset=utf-8" },
};

const makeDirectory = temporaryDirectories("dispatch-bin");

/** The Dispatch bearer the entry is handed. */
const TOKEN = fixtureSecret("dsp");

const server = Bun.serve({
  port: 0,
  fetch(request) {
    const { pathname } = new URL(request.url);
    if (pathname === "/api/v1/projects" && request.method === "GET") {
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
    const file = FILES[pathname];
    if (file !== undefined && request.method === "GET") {
      return new Response(file.bytes, {
        headers: {
          "Content-Type": file.type,
          "Content-Disposition": `attachment; filename="file"; filename*=UTF-8''file`,
        },
      });
    }
    return Response.json({ error: "unexpected" }, { status: 500 });
  },
});

afterAll(() => server.stop(true));

/** The session an Oh My Pi shell names, which a run with a host is handed. */
const OMP = { DISPATCH_HOST: "omp", DISPATCH_SESSION_ID: "ses-bin" };

/** Runs `dispatch <args>` with `host`'s variables beside Dispatch's URL and bearer. */
async function runEntry(
  args: readonly string[],
  host: Readonly<Record<string, string>>
): Promise<{ code: number; stdout: Uint8Array; stderr: string }> {
  const state = makeDirectory();
  // Bun.spawn's own stdout pipe drains fast enough to hide the loss, so the entry writes into a
  // shell pipe to `cat`, the way a bash tool captures it; pipefail keeps the entry's exit code.
  // The shell runs the same Bun as the test, not whatever `bun` its PATH resolves.
  const child = Bun.spawn(
    [
      "bash",
      "-c",
      'set -o pipefail; runtime="$1"; shift; "$runtime" "$0" "$@" | cat',
      ENTRY,
      process.execPath,
      ...args,
    ],
    {
      env: {
        PATH: process.env.PATH,
        HOME: state,
        DISPATCH_STATE_DIR: state,
        ...host,
        DISPATCH_URL: `http://127.0.0.1:${server.port}`,
        DISPATCH_TOKEN: TOKEN,
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
  // default. It fires only if the child never exits or its pipe never ends.
  test("writes all of its output to a pipe, however long", async () => {
    const { code, stdout, stderr } = await runEntry(["projects"], OMP);
    expect(stderr).toBe("");
    expect(code).toBe(0);
    const text = new TextDecoder().decode(stdout);
    expect(stdout.length).toBeGreaterThan(64 * 1024);
    expect(text).toContain(`P${PROJECTS - 1}`);
    expect(text.endsWith("\n")).toBe(true);
  }, 30_000);

  test.each([
    ["a picture, with a host", PICTURE_VERSION, OMP],
    ["a picture, with no host", PICTURE_VERSION, {}],
    ["a file stored as JSON, with a host", DATA_VERSION, OMP],
    ["a file stored as JSON, with no host", DATA_VERSION, {}],
  ] as const)("writes stored content to a pipe as its bytes, unchanged: %s", async (_, path, host) => {
    const { code, stdout, stderr } = await runEntry(["api", "--path", path], host);
    expect(stderr).toBe("");
    expect(code).toBe(0);
    expect(stdout).toEqual(FILES[path]?.bytes);
  });
});
