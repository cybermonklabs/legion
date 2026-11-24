import { beforeEach, describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { runDispatchCli } from "../dispatch-cli";
import { resetAdviceMemory } from "../dispatch-execute";

// `dispatch doc-read` of an uploaded file or picture names the version it read by its hash in the
// header, on the line under the header's first: the latest version's token in a `Document token:`
// line, which the next version's upload names as its precondition, and a named version's hash in
// a `Content: sha256:` line, as document reads print them. The line goes in the header, so the
// text after it is the stored file unchanged, a leading byte order mark included, but for the line
// feed the dispatch command adds after a file that ends without one. The header's own wording is
// dispatch-execute.test.ts's to pin; these rows read only where the hash line sits, what it
// holds, and the requests made. The command runs whole, through runDispatchCli, against a stub
// Dispatch that records every path it is asked.

const makeDirectory = temporaryDirectories("doc-read-file-token");

/** The Dispatch bearer each command is handed. */
const TOKEN = fixtureSecret("dsp");

const hexOf = (digit: string) => digit.repeat(64);

/** A version as Dispatch states it: its hash, and its token where `token` is true. */
const version = (number: number, mime: string, size: number, digit: string, token = true) => ({
  number,
  mime,
  size,
  sha256: hexOf(digit),
  ...(token ? { token: `sha256:${hexOf(digit)}` } : {}),
});

/** The eight bytes every PNG starts with, which is what makes these bytes a picture. */
const PNG = [0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a];

/** The stored bytes of sweep.json's two versions: the second ends with a line feed, which the
 *  read must keep as the file's last byte. */
const SWEEP = ['{"rows":1}', '{"rows": [1, 2]}\n'] as const;

/** A file that begins with a UTF-8 byte order mark, which the read keeps. */
const WITH_BOM = new Uint8Array([0xef, 0xbb, 0xbf, 0x41, 0x0a]);

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
      versions: [version(1, "application/json", 10, "1"), version(2, "application/json", 17, "2")],
    },
    {
      id: "image-8",
      slug: "chart-png",
      name: "chart.png",
      kind: "image",
      primary: false,
      versions: [version(1, "image/png", 12, "3")],
    },
    {
      id: "image-9",
      slug: "huge-png",
      name: "huge.png",
      kind: "image",
      primary: false,
      versions: [version(1, "image/png", 9_000_000, "4")],
    },
    {
      id: "file-10",
      slug: "blob-bin",
      name: "blob.bin",
      kind: "file",
      primary: false,
      versions: [version(1, "application/octet-stream", 2, "5")],
    },
    {
      id: "file-12",
      slug: "bom-txt",
      name: "bom.txt",
      kind: "file",
      primary: false,
      versions: [version(1, "text/plain", 5, "7")],
    },
    {
      // What a Dispatch that states no token for a file's version answers.
      id: "file-11",
      slug: "old-txt",
      name: "old.txt",
      kind: "file",
      primary: false,
      versions: [version(1, "text/plain", 4, "6", false)],
    },
  ],
  open_asks: [],
};

const served: Record<string, readonly [string, Uint8Array<ArrayBuffer>]> = {
  "/api/v1/artifacts/file-7/versions/1": ["application/json", new TextEncoder().encode(SWEEP[0])],
  "/api/v1/artifacts/file-7/versions/2": ["application/json", new TextEncoder().encode(SWEEP[1])],
  "/api/v1/artifacts/image-8/versions/1": ["image/png", new Uint8Array([...PNG, 0, 0, 0, 0])],
  "/api/v1/artifacts/file-10/versions/1": [
    "application/octet-stream",
    new Uint8Array([0xff, 0xfe]),
  ],
  "/api/v1/artifacts/file-11/versions/1": ["text/plain", new TextEncoder().encode("old\n")],
  "/api/v1/artifacts/file-12/versions/1": ["text/plain", WITH_BOM],
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

/** The read's second line, where the header names the version's hash. */
const secondLine = (stdout: string) => stdout.split("\n")[1];

describe("dispatch doc-read of an uploaded file or picture", () => {
  // R1: the latest version's token on the header's second line, and after the header's blank
  // line the stored file whole, ending where it ends.
  test("prints the latest version's token in the header, and the stored file after it", async () => {
    const { code, stdout } = await run([
      "doc-read",
      "--issue",
      "DSP-1",
      "--artifact",
      "sweep-json",
    ]);

    expect(code, stdout).toBe(0);
    expect(secondLine(stdout)).toBe(`Document token: sha256:${hexOf("2")}`);
    expect(stdout.split("\n")[2]).toBe("");
    expect(stdout.endsWith(`\n\n${SWEEP[1]}`)).toBe(true);
    expect(stdout.split("\n").filter((line) => line.startsWith("Document token:"))).toHaveLength(1);
    expect(requests).toEqual([
      "GET /api/v1/issues/DSP-1",
      "GET /api/v1/artifacts/file-7/versions/2",
    ]);
  });

  // R1: a named version's hash in a Content line, as a named document version's read prints it,
  // and no token, which names the latest version alone.
  test("prints a named version's hash in a Content line, and no token", async () => {
    const { code, stdout } = await run([
      "doc-read",
      "--ref",
      "dispatch://DSP-1/artifact/sweep-json@v1",
    ]);

    expect(code, stdout).toBe(0);
    expect(secondLine(stdout)).toBe(`Content: sha256:${hexOf("1")}`);
    expect(stdout).not.toContain("Document token:");
    // The file ends without a line feed, and the dispatch command ends every output with one.
    expect(stdout.endsWith(`\n\n${SWEEP[0]}\n`)).toBe(true);
  });

  test("prints a shown picture's token in the header", async () => {
    const { code, stdout } = await run(["doc-read", "--issue", "DSP-1", "--artifact", "chart-png"]);

    expect(code, stdout).toBe(0);
    expect(secondLine(stdout)).toBe(`Document token: sha256:${hexOf("3")}`);
  });

  // The token comes from the version list, so a picture too large to show is not fetched for it.
  test("prints the token of a picture too large to show without fetching it", async () => {
    const { code, stdout } = await run(["doc-read", "--issue", "DSP-1", "--artifact", "huge-png"]);

    expect(code, stdout).toBe(0);
    expect(secondLine(stdout)).toBe(`Document token: sha256:${hexOf("4")}`);
    expect(requests).toEqual(["GET /api/v1/issues/DSP-1"]);
  });

  test("prints the token of a file that is not UTF-8 text", async () => {
    const { code, stdout } = await run(["doc-read", "--issue", "DSP-1", "--artifact", "blob-bin"]);

    expect(code, stdout).toBe(0);
    expect(secondLine(stdout)).toBe(`Document token: sha256:${hexOf("5")}`);
  });

  // R1: the text after the header is the stored file, so a leading byte order mark stays.
  test("keeps a file's leading byte order mark", async () => {
    const { code, stdout } = await run(["doc-read", "--issue", "DSP-1", "--artifact", "bom-txt"]);

    expect(code, stdout).toBe(0);
    const printed = new TextEncoder().encode(stdout);
    expect([...printed.subarray(printed.length - WITH_BOM.length)]).toEqual([...WITH_BOM]);
    // U+FEFF is the byte order mark as text: EF BB BF in UTF-8.
    expect(stdout.endsWith("\n\n\uFEFFA\n")).toBe(true);
  });

  // A version Dispatch states no token for takes none on an upload, so no line is printed.
  test("prints no token line when Dispatch states none", async () => {
    const { code, stdout } = await run(["doc-read", "--issue", "DSP-1", "--artifact", "old-txt"]);

    expect(code, stdout).toBe(0);
    expect(stdout).not.toContain("Document token:");
    expect(stdout.endsWith("\n\nold\n")).toBe(true);
  });
});
