import { beforeEach, describe, expect, test } from "bun:test";
import { readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { runDispatchCli } from "../dispatch-cli";
import { resetAdviceMemory } from "../dispatch-execute";

// `dispatch artifact` under the name of a file its owner already holds is refused before anything
// is sent, and the refusal names only a remedy Dispatch takes: `--force` with the token of the
// version it replaces, which the owner's listing states and the refusal names, or `--force` alone
// from a Dispatch that states no token for a file and refuses one on its upload. Each test sends
// exactly what the refusal says, read out of it, and is taken. The command runs whole, through
// runDispatchCli, against a stub Dispatch that answers a file's upload as Dispatch does: the token
// of the version that stands is taken, any other is refused 409, and a Dispatch that states no
// token refuses every token on a file 400.

const makeDirectory = temporaryDirectories("artifact-held-file-name");

/** The Dispatch bearer each command is handed. */
const TOKEN = fixtureSecret("dsp");

const NOTES_TOKEN = `sha256:${"a".repeat(64)}`;

interface StatedVersion {
  readonly number: number;
  readonly named: boolean;
  readonly summary: null;
  readonly authors: readonly { readonly kind: string; readonly id: string }[];
  readonly created_at: string;
  readonly size: number;
  readonly mime: string;
  readonly sha256?: string;
  readonly token?: string;
}

const version = (number: number, token?: string): StatedVersion => ({
  number,
  named: false,
  summary: null,
  authors: [{ kind: "session", id: "ses-0" }],
  created_at: "2026-11-24T00:00:00Z",
  size: 6,
  mime: "text/plain",
  ...(token === undefined ? {} : { sha256: token.slice("sha256:".length), token }),
});

const artifact = (id: string, name: string, versions: readonly StatedVersion[]) => ({
  id,
  issue_key: "DSP-1",
  project: "DSP",
  session_id: null,
  slug: name.replace(".", "-"),
  name,
  kind: "file",
  primary: false,
  created_by: { kind: "session", id: "ses-0" },
  created_at: "2026-11-24T00:00:00Z",
  versions,
});

/** DSP-1's files: notes.txt from a Dispatch that states each version's token, and old.txt from
 *  one that states none. */
const held = [
  artifact("file-1", "notes.txt", [version(1, NOTES_TOKEN)]),
  artifact("file-2", "old.txt", [version(1)]),
];

interface Upload {
  readonly name: string;
  readonly precondition: unknown;
}

const uploads: Upload[] = [];
let stateDir = "";

beforeEach(() => {
  uploads.length = 0;
  stateDir = makeDirectory();
  resetAdviceMemory();
});

const stubDispatch = (async (input: string | URL | Request, init?: RequestInit) => {
  const request = new Request(input, init);
  const { pathname } = new URL(request.url);
  if (request.method === "GET" && pathname === "/api/v1/issues/DSP-1/artifacts") {
    return Response.json(held);
  }
  if (request.method === "POST" && pathname === "/api/v1/issues/DSP-1/artifacts") {
    const form = await request.formData();
    const name = String(form.get("name"));
    const field = form.get("precondition");
    const precondition = field === null ? undefined : JSON.parse(String(field));
    uploads.push({ name, precondition });
    const target = held.find((file) => file.name === name);
    if (target === undefined)
      return Response.json({ code: "NOT_FOUND", error: "no" }, { status: 404 });
    const standing = target.versions.at(-1)?.token;
    if (precondition !== undefined && standing === undefined) {
      return Response.json(
        {
          code: "INVALID_PRECONDITION",
          error:
            "invalid document edit precondition: only a document has a version token; this upload is stored as a file",
        },
        { status: 400 }
      );
    }
    if (precondition !== undefined && precondition.document !== standing) {
      return Response.json(
        {
          code: "PRECONDITION_FAILED",
          error: "document edit precondition failed: document changed",
        },
        { status: 409 }
      );
    }
    return Response.json(
      { artifact: target, version: version(2), advice: { issue_status: "in_progress" } },
      { status: 201 }
    );
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

/** The upload of a file's second version under `name`, plus the flags given. */
function upload(name: string, flags: readonly string[]): Promise<{ code: number; stdout: string }> {
  const path = join(stateDir, name);
  writeFileSync(path, "second\n");
  return run(["artifact", "--issue", "DSP-1", "--name", name, "--path", path, ...flags]);
}

/** The refusal's own line, the problem the command names for `name`, without the allowed flags
 *  and the example the command prints under every refusal. */
function refusalOf(stdout: string, name: string): string {
  const line = stdout
    .split("\n")
    .find((text) => text.startsWith(`- issue DSP-1 already holds ${name} `));
  expect(line, stdout).toBeDefined();
  return line ?? "";
}

describe("dispatch artifact under the name of a file the owner holds", () => {
  // R3: the refusal names --force and the token of the version it replaces, and the upload that
  // sends exactly those is taken.
  test("names the standing version's token, and the upload that sends exactly that is taken", async () => {
    const refused = await upload("notes.txt", []);
    expect(refused.code, refused.stdout).toBe(2);
    const refusal = refusalOf(refused.stdout, "notes.txt");
    expect(refusal).toContain("(slug notes-txt, kind file) at version 1");
    expect(uploads).toEqual([]);

    const advised = refusal.match(/--force --precondition-json '(\{[^']*\})'/);
    expect(advised, refusal).not.toBeNull();
    const precondition = advised?.[1] ?? "";
    expect(JSON.parse(precondition)).toEqual({ document: NOTES_TOKEN });

    const taken = await upload("notes.txt", ["--force", "--precondition-json", precondition]);
    expect(taken.code, taken.stdout).toBe(0);
    expect(taken.stdout).toStartWith("Uploaded notes.txt as version 2");
    expect(uploads).toEqual([{ name: "notes.txt", precondition: { document: NOTES_TOKEN } }]);
  });

  // R3, from a Dispatch that states no token for a file: it refuses any token on a file's upload,
  // so the refusal names --force alone, and the upload that sends it is taken.
  test("names --force alone where Dispatch states no token, and the upload that sends it is taken", async () => {
    const refused = await upload("old.txt", []);
    expect(refused.code, refused.stdout).toBe(2);
    const refusal = refusalOf(refused.stdout, "old.txt");
    expect(refusal).toContain("send the upload again with force (--force) alone");
    expect(refusal).not.toContain("--precondition-json");

    const taken = await upload("old.txt", ["--force"]);
    expect(taken.code, taken.stdout).toBe(0);
    expect(taken.stdout).toStartWith("Uploaded old.txt as version 2");
    expect(uploads).toEqual([{ name: "old.txt", precondition: undefined }]);
  });

  // A file holds no text dispatch doc-edit changes, so the refusal never sends the writer there.
  test("does not name dispatch doc-edit for a file", async () => {
    const refused = await upload("notes.txt", []);
    expect(refusalOf(refused.stdout, "notes.txt")).not.toContain("doc-edit");
  });
});
