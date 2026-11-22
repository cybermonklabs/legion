import { beforeEach, describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { runDispatchCli } from "../dispatch-cli";
import { resetAdviceMemory } from "../dispatch-execute";

// `dispatch doc-read` of an uploaded file or picture prints the token of the version it read, in
// the `Document token:` line a document read prints, so the next version's upload can name it as
// `--precondition-json '{"document":"sha256:…"}'`. The token is the one Dispatch states for that
// version in the artifact's version list; a Dispatch that states none prints no line. The command
// runs whole, through runDispatchCli, against a stub Dispatch that records every path it is asked.

const makeDirectory = temporaryDirectories("doc-read-file-token");

/** The Dispatch bearer each command is handed. */
const TOKEN = fixtureSecret("dsp");

const tokenOf = (digit: string) => `sha256:${digit.repeat(64)}`;

/** The eight bytes every PNG starts with, which is what makes these bytes a picture. */
const PNG = [0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a];

const issue = {
  key: "DSP-1",
  primary_artifact_id: "spec-1",
  artifacts: [
    { id: "spec-1", slug: "spec", name: "spec.md", kind: "doc", primary: true, versions: [] },
    {
      id: "file-7",
      slug: "sweep-json",
      name: "sweep.json",
      kind: "file",
      primary: false,
      versions: [
        { number: 1, mime: "application/json", size: 10, token: tokenOf("1") },
        { number: 2, mime: "application/json", size: 17, token: tokenOf("2") },
      ],
    },
    {
      id: "image-8",
      slug: "chart-png",
      name: "chart.png",
      kind: "image",
      primary: false,
      versions: [{ number: 1, mime: "image/png", size: 12, token: tokenOf("3") }],
    },
    {
      id: "image-9",
      slug: "huge-png",
      name: "huge.png",
      kind: "image",
      primary: false,
      versions: [{ number: 1, mime: "image/png", size: 9_000_000, token: tokenOf("4") }],
    },
    {
      id: "file-10",
      slug: "blob-bin",
      name: "blob.bin",
      kind: "file",
      primary: false,
      versions: [{ number: 1, mime: "application/octet-stream", size: 2, token: tokenOf("5") }],
    },
    {
      // What a Dispatch that states no token for a file's version answers.
      id: "file-11",
      slug: "old-txt",
      name: "old.txt",
      kind: "file",
      primary: false,
      versions: [{ number: 1, mime: "text/plain", size: 4 }],
    },
  ],
  open_asks: [],
};

const served: Record<string, readonly [string, Uint8Array<ArrayBuffer>]> = {
  "/api/v1/artifacts/file-7/versions/1": [
    "application/json",
    new TextEncoder().encode('{"rows":1}'),
  ],
  "/api/v1/artifacts/file-7/versions/2": [
    "application/json",
    new TextEncoder().encode('{"rows": [1, 2]}\n'),
  ],
  "/api/v1/artifacts/image-8/versions/1": ["image/png", new Uint8Array([...PNG, 0, 0, 0, 0])],
  "/api/v1/artifacts/file-10/versions/1": [
    "application/octet-stream",
    new Uint8Array([0xff, 0xfe]),
  ],
  "/api/v1/artifacts/file-11/versions/1": ["text/plain", new TextEncoder().encode("old\n")],
};

const requests: string[] = [];
let stateDir = "";

beforeEach(() => {
  requests.length = 0;
  stateDir = makeDirectory();
  // Each run is its own process in production: nothing the library remembers carries over.
  resetAdviceMemory();
});

/** A Dispatch that answers DSP-1's read and its files' versions, and nothing else. */
const stubDispatch = (async (input: string | URL | Request, init?: RequestInit) => {
  const request = new Request(input, init);
  const { pathname } = new URL(request.url);
  requests.push(`${request.method} ${pathname}`);
  if (request.method === "GET" && pathname === "/api/v1/issues/DSP-1") {
    return Response.json(issue);
  }
  const file = served[pathname];
  if (request.method === "GET" && file !== undefined) {
    return new Response(file[1], { status: 200, headers: { "Content-Type": file[0] } });
  }
  return Response.json({ error: "not found", code: "NOT_FOUND" }, { status: 404 });
}) as typeof fetch;

async function run(argv: readonly string[]): Promise<{ code: number; stdout: string }> {
  let stdout = "";
  const code = await runDispatchCli(
    argv,
    {
      HOME: stateDir,
      DISPATCH_HOST: "omp",
      DISPATCH_SESSION_ID: "ses-1",
      DISPATCH_URL: "http://127.0.0.1:9",
      DISPATCH_TOKEN: TOKEN,
      DISPATCH_STATE_DIR: stateDir,
    },
    {
      stdout: (text) => {
        stdout += text;
      },
      stderr: () => {},
      readText: (path) => readFileSync(path, "utf-8"),
      cwd: stateDir,
      fetchImpl: stubDispatch,
    }
  );
  return { code, stdout };
}

describe("dispatch doc-read of an uploaded file or picture", () => {
  // R1: the latest version's token, after the file's text, as a document read prints its own.
  test("prints the latest version's token after a file's text", async () => {
    const { code, stdout } = await run([
      "doc-read",
      "--issue",
      "DSP-1",
      "--artifact",
      "sweep-json",
    ]);

    expect(code, stdout).toBe(0);
    expect(stdout).toStartWith(
      'File sweep.json: application/json, version 2, 17 bytes.\n\n{"rows": [1, 2]}\n\n\n' +
        `Document token: ${tokenOf("2")}\n`
    );
    expect(requests).toEqual([
      "GET /api/v1/issues/DSP-1",
      "GET /api/v1/artifacts/file-7/versions/2",
    ]);
  });

  // R1: a named version's own token, which an upload naming it is refused for once a newer
  // version stands (R2).
  test("prints a named version's own token", async () => {
    const { code, stdout } = await run([
      "doc-read",
      "--ref",
      "dispatch://DSP-1/artifact/sweep-json@v1",
    ]);

    expect(code, stdout).toBe(0);
    expect(stdout).toStartWith(
      'File sweep.json: application/json, version 1 of 2, 10 bytes.\n\n{"rows":1}\n\n' +
        `Document token: ${tokenOf("1")}\n`
    );
    expect(stdout).not.toContain(tokenOf("2"));
  });

  test("prints a shown picture's token", async () => {
    const { code, stdout } = await run(["doc-read", "--issue", "DSP-1", "--artifact", "chart-png"]);

    expect(code, stdout).toBe(0);
    expect(stdout).toStartWith("Picture chart.png: image/png, version 1, 12 bytes.\n");
    expect(stdout).toContain(`\n\nDocument token: ${tokenOf("3")}\n`);
  });

  // The token comes from the version list, so a picture too large to show is not fetched for it.
  test("prints the token of a picture too large to show without fetching it", async () => {
    const { code, stdout } = await run(["doc-read", "--issue", "DSP-1", "--artifact", "huge-png"]);

    expect(code, stdout).toBe(0);
    expect(stdout).toContain(
      `picture (version 1). GET /api/v1/artifacts/image-9/versions/1 serves its bytes.\n\nDocument token: ${tokenOf("4")}\n`
    );
    expect(requests).toEqual(["GET /api/v1/issues/DSP-1"]);
  });

  test("prints the token of a file that is not UTF-8 text", async () => {
    const { code, stdout } = await run(["doc-read", "--issue", "DSP-1", "--artifact", "blob-bin"]);

    expect(code, stdout).toBe(0);
    expect(stdout).toStartWith(
      "blob.bin is an uploaded application/octet-stream file (version 1, 2 bytes) that is not UTF-8 text, so " +
        "dispatch doc-read cannot show it. GET /api/v1/artifacts/file-10/versions/1 serves its bytes.\n\n" +
        `Document token: ${tokenOf("5")}\n`
    );
  });

  // A Dispatch that states no token for a file's version refuses one on its upload, so nothing is
  // printed for a caller to name.
  test("prints no token line when Dispatch states none", async () => {
    const { code, stdout } = await run(["doc-read", "--issue", "DSP-1", "--artifact", "old-txt"]);

    expect(code, stdout).toBe(0);
    expect(stdout).toStartWith("File old.txt: text/plain, version 1, 4 bytes.\n\nold\n");
    expect(stdout).not.toContain("Document token:");
  });
});
