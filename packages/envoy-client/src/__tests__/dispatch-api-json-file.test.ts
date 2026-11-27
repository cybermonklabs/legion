// packages/envoy-client/src/__tests__/dispatch-api-json-file.test.ts
//
// The tester's red test at c8e0065f (INFRA-270): a file version whose stored type is JSON. Dispatch
// answers a file version with the file's own bytes, under the type the file was stored with and
// `Content-Disposition: attachment` (`serveFileVersion`,
// packages/envoy/internal/dispatch/api/artifact_files.go). A file uploaded as `data.json` is
// stored as `application/json`, so its version comes back typed JSON. The README says
// `dispatch api` prints a file version's bytes unchanged, and R4 says it prints the body of the
// answer. At c8e0065f the command parses such a body and prints it again: a 64-bit integer loses
// its last digits, `1.10` becomes `1.1`, the spacing changes, and a file that holds one JSON
// string loses its quotes, all with exit 0. Measured live against dispatch:local at c8e0065f on
// 2026-11-25T15:52:57Z: a file holding `{"id":12345678901234567890,...}` printed
// `"id": 12345678901234567000`.

import { describe, expect, test } from "bun:test";
import { runDispatchCli } from "../dispatch-cli";
import { brokerStandIns } from "./broker-stand-ins";

const { servers, tempDir, stubBroker, podEnvironment } = brokerStandIns("dispatch-api-json-file");

/** A JSON file's bytes as a person uploaded them: an integer past 2^53, a decimal with a trailing
 *  zero and the file's own spacing, each of which a parse and a print again would change. */
const DATA_JSON = new TextEncoder().encode(
  '{"id":12345678901234567890,"ratio":1.10, "name":"x"}\n'
);
/** A JSON file that holds one string, which a parse would print without its quotes. */
const STRING_JSON = new TextEncoder().encode('"just a JSON string"');

const DATA_VERSION = "/api/v1/artifacts/01234567-0000-4000-8000-0000000000a1/versions/1";
const STRING_VERSION = "/api/v1/artifacts/01234567-0000-4000-8000-0000000000a2/versions/1";

/** A Dispatch that answers the two file versions as `serveFileVersion` does: the stored bytes
 *  under the stored type, with the headers it sets on every file version. */
function stubDispatch(): { url: string } {
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: (request) => {
      const { pathname } = new URL(request.url);
      const file =
        pathname === DATA_VERSION
          ? { name: "data.json", bytes: DATA_JSON }
          : pathname === STRING_VERSION
            ? { name: "string.json", bytes: STRING_JSON }
            : undefined;
      if (file === undefined) {
        return Response.json({ code: "NOT_FOUND", error: "not found" }, { status: 404 });
      }
      return new Response(file.bytes, {
        headers: {
          "Content-Type": "application/json; charset=utf-8",
          "Cache-Control": "private, max-age=31536000, immutable",
          "Content-Disposition": `attachment; filename="${file.name}"; filename*=UTF-8''${file.name}`,
          "X-Content-Type-Options": "nosniff",
        },
      });
    },
  });
  servers.push(server);
  return { url: `http://127.0.0.1:${server.port}` };
}

/** Runs the CLI as the bin runs it, keeping standard output's bytes as written. */
async function dispatch(
  argv: readonly string[],
  env: Record<string, string | undefined>
): Promise<{ code: number; bytes: Uint8Array; stdout: string; stderr: string }> {
  const written: Uint8Array[] = [];
  let stderr = "";
  const cwd = tempDir();
  const code = await runDispatchCli(argv, env, {
    stdout: (text) => {
      written.push(new TextEncoder().encode(text));
    },
    stdoutBytes: (bytes) => {
      written.push(bytes.slice());
    },
    stderr: (text) => {
      stderr += text;
    },
    readText: () => "",
    cwd,
  });
  const bytes = new Uint8Array(Buffer.concat(written));
  return { code, bytes, stdout: new TextDecoder().decode(bytes), stderr };
}

const RUNS = [
  [
    "with a host",
    (broker: string, server: string) => ({
      ...podEnvironment(broker, server),
      DISPATCH_HOST: "omp",
      DISPATCH_SESSION_ID: "ses-1",
      DISPATCH_STATE_DIR: tempDir(),
    }),
  ],
  [
    "with no host",
    (broker: string, server: string) => ({
      ...podEnvironment(broker, server),
      DISPATCH_STATE_DIR: tempDir(),
    }),
  ],
] as const;

describe("a file version stored as JSON", () => {
  test.each(
    RUNS
  )("its bytes go to standard output unchanged, and it exits 0: %s", async (_, environment) => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const ran = await dispatch(
      ["api", "--path", DATA_VERSION],
      environment(broker.url, server.url)
    );
    expect(ran.code, ran.stdout).toBe(0);
    expect(ran.stdout).toBe(new TextDecoder().decode(DATA_JSON));
    expect(ran.bytes).toEqual(DATA_JSON);
  });

  test.each(
    RUNS
  )("a file that holds one JSON string keeps its quotes: %s", async (_, environment) => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const ran = await dispatch(
      ["api", "--path", `${STRING_VERSION}?download=true`],
      environment(broker.url, server.url)
    );
    expect(ran.code, ran.stdout).toBe(0);
    expect(ran.bytes).toEqual(STRING_JSON);
  });
});
