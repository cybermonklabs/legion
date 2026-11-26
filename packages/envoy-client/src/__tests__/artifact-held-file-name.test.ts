import { beforeEach, describe, expect, test } from "bun:test";
import { createHash } from "node:crypto";
import { readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { keyedLayer } from "../../test/keyed-layer";
import { routeAnswer } from "../../test/route-answers";
import { runDispatchCli } from "../dispatch-cli";
import { resetAdviceMemory } from "../dispatch-execute";

// `dispatch artifact` under a name its owner already holds is refused before the upload is sent,
// and the refusal names only a remedy Dispatch takes and prints no token. The command checks a
// Markdown text first, with a round trip that stores nothing, and then reads what the owner holds;
// this stub answers that round trip 404, as a Dispatch without the route does, so the text goes
// unchecked and the read of the held name decides. Of the stored kind, a file or picture is
// replaced with --force and the token the writer's own `dispatch doc-read` printed, so a writer
// whose read is stale is refused 409 rather than handed the newest token; where the standing
// version states no token, --force alone. Of another kind, by Dispatch's rule (declared Markdown
// is a document, PNG, JPEG, GIF or WebP bytes a picture, anything else a file), the name cannot
// take it, so the refusal names another name and nothing else. Each row runs the command whole,
// through runDispatchCli, against a stub Dispatch that keeps DSP-1's artifacts and answers reads
// and uploads as Dispatch does.

const makeDirectory = temporaryDirectories("artifact-held-file-name");

/** The Dispatch bearer each command is handed. */
const TOKEN = fixtureSecret("dsp");

type Kind = "doc" | "file" | "image";

interface StoredVersion {
  readonly number: number;
  readonly bytes: Uint8Array;
  readonly mime: string;
  /** Whether the stub states this version's token: false for a version stored with no hash. */
  readonly hashed: boolean;
}

interface StoredArtifact {
  readonly id: string;
  readonly name: string;
  readonly slug: string;
  readonly kind: Kind;
  readonly versions: StoredVersion[];
}

/** The eight bytes every PNG starts with. */
const PNG = new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]);

const ascii = (text: string) => [...new TextEncoder().encode(text)];

/** Bytes each picture format begins with, as Go's http.DetectContentType matches them: the
 *  `jpeg`, `gif` and `webp` keys, with `riff` a RIFF file that says WEBP but holds no `VP` chunk
 *  after it, which Go reads as no picture. A WebP's signature is 14 bytes long. */
const HEADS = {
  png: [...PNG],
  jpeg: [0xff, 0xd8, 0xff, 0xe0],
  gif: ascii("GIF89a"),
  webp: [...ascii("RIFF"), 0x24, 0x00, 0x00, 0x00, ...ascii("WEBPVP8 ")],
  riff: [...ascii("RIFF"), 0x24, 0x00, 0x00, 0x00, ...ascii("WEBP")],
} as const;

const bytesOf = (format: keyof typeof HEADS, text: string) =>
  new Uint8Array([...HEADS[format], ...ascii(text)]);
const pictureOf = (text: string) => bytesOf("png", text);
const textOf = (text: string) => new TextEncoder().encode(`${text}\n`);
const hexOf = (bytes: Uint8Array) => createHash("sha256").update(bytes).digest("hex");

/** Whether this stub is a Dispatch that states file tokens (this build) or one from before them,
 *  which refuses any token on a file's upload 400 INVALID_PRECONDITION. */
let statesTokens = true;
let held: StoredArtifact[] = [];
/** Every upload Dispatch was sent: its name and the precondition it named. */
const uploads: Array<{ readonly name: string; readonly precondition: unknown }> = [];
let stateDir = "";

const versionOf = (artifact: StoredArtifact, version: StoredVersion) => ({
  number: version.number,
  named: false,
  summary: null,
  authors: [{ kind: "session", id: "ses-0" }],
  created_at: "2026-11-24T00:00:00Z",
  size: version.bytes.length,
  mime: version.mime,
  ...(artifact.kind !== "doc" && version.hashed ? { sha256: hexOf(version.bytes) } : {}),
  ...(artifact.kind !== "doc" && version.hashed && statesTokens
    ? { token: `sha256:${hexOf(version.bytes)}` }
    : {}),
});

const artifactOf = (artifact: StoredArtifact) => ({
  id: artifact.id,
  issue_key: "DSP-1",
  project: "DSP",
  session_id: null,
  slug: artifact.slug,
  name: artifact.name,
  kind: artifact.kind,
  primary: false,
  created_by: { kind: "session", id: "ses-0" },
  created_at: "2026-11-24T00:00:00Z",
  versions: artifact.versions.map((version) => versionOf(artifact, version)),
});

/** The kind Dispatch stores an upload as: its rule, in artifactKind, which sniffs the bytes with
 *  Go's http.DetectContentType and takes its PNG, JPEG, GIF and WebP answers as a picture. */
function kindOf(mime: string, bytes: Uint8Array): Kind {
  if (mime.split(";")[0]?.trim() === "text/markdown") return "doc";
  const at = (offset: number, expected: readonly number[]) =>
    expected.every((byte, index) => bytes[offset + index] === byte);
  const picture =
    at(0, HEADS.png) ||
    at(0, [0xff, 0xd8, 0xff]) ||
    at(0, ascii("GIF87a")) ||
    at(0, ascii("GIF89a")) ||
    (at(0, ascii("RIFF")) && at(8, ascii("WEBPVP")));
  return picture ? "image" : "file";
}

const refusal = (status: number, code: string, error: string) =>
  Response.json({ code, error }, { status });

/** The upload's route, whose declared answer each stored upload serves (`routeAnswer`). */
const UPLOAD = "POST /api/v1/issues/{key}/artifacts";

/** One upload, answered as Dispatch answers it. */
function store(name: string, kind: Kind, bytes: Uint8Array, mime: string, precondition: unknown) {
  uploads.push({ name, precondition });
  const document = (precondition as { document?: string } | undefined)?.document;
  const target = held.find((artifact) => artifact.name === name || artifact.slug === name);
  if (target === undefined) {
    if (document !== undefined) {
      return refusal(409, "PRECONDITION_FAILED", `"${name}" has no token for an upload to name`);
    }
    const created: StoredArtifact = {
      id: `artifact-${held.length + 1}`,
      name,
      slug: name.replaceAll(".", "-"),
      kind,
      versions: [{ number: 1, bytes, mime, hashed: true }],
    };
    held.push(created);
    return Response.json(
      routeAnswer(
        UPLOAD,
        {
          artifact: artifactOf(created),
          version: versionOf(created, created.versions[0] as StoredVersion),
        },
        201
      ),
      { status: 201 }
    );
  }
  if (target.kind !== kind) {
    return refusal(
      400,
      "ARTIFACT_KIND_MISMATCH",
      `"${name}" names a ${target.kind}; this upload is a ${kind}: give it another name`
    );
  }
  const standing = target.versions.at(-1);
  if (document !== undefined && target.kind !== "doc") {
    if (!statesTokens) {
      return refusal(
        400,
        "INVALID_PRECONDITION",
        "only a document has a version token; this upload is stored as a file"
      );
    }
    if (standing === undefined || !standing.hashed) {
      return refusal(409, "PRECONDITION_FAILED", `"${name}" has no token for an upload to name`);
    }
    if (document !== `sha256:${hexOf(standing.bytes)}`) {
      return refusal(
        409,
        "PRECONDITION_FAILED",
        `file upload precondition failed: "${name}" stands at another version`
      );
    }
  }
  const next: StoredVersion = { number: (standing?.number ?? 0) + 1, bytes, mime, hashed: true };
  target.versions.push(next);
  return Response.json(
    routeAnswer(UPLOAD, { artifact: artifactOf(target), version: versionOf(target, next) }, 201),
    { status: 201 }
  );
}

/** Dispatch's routes as this test serves them, before the keyed layer (`stubDispatch`). */
const answerDispatch = async (input: string | URL | Request, init?: RequestInit) => {
  const request = new Request(input, init);
  const { pathname } = new URL(request.url);
  if (request.method === "GET" && pathname === "/api/v1/issues/DSP-1/artifacts") {
    return Response.json(held.map(artifactOf));
  }
  if (request.method === "GET" && pathname === "/api/v1/issues/DSP-1") {
    return Response.json({
      key: "DSP-1",
      primary_artifact_id: null,
      artifacts: held.map(artifactOf),
      open_asks: [],
    });
  }
  const read = pathname.match(/^\/api\/v1\/artifacts\/([^/]+)\/versions\/(\d+)$/);
  if (request.method === "GET" && read !== null) {
    const version = held.find((artifact) => artifact.id === read[1])?.versions[Number(read[2]) - 1];
    if (version === undefined) return refusal(404, "NOT_FOUND", "no version");
    return new Response(new Uint8Array(version.bytes), {
      status: 200,
      headers: { "Content-Type": version.mime },
    });
  }
  if (request.method === "POST" && pathname === "/api/v1/issues/DSP-1/artifacts") {
    if (request.headers.get("content-type")?.startsWith("application/json") === true) {
      const body = (await request.json()) as {
        name: string;
        content: string;
        precondition?: unknown;
      };
      return store(body.name, "doc", textOf(body.content), "text/markdown", body.precondition);
    }
    // The type the part declares, as the server reads it off the part's header: Bun's parsed
    // File reports a type guessed from the file name instead, which is not what was sent.
    const raw = await request.clone().text();
    const declared =
      raw.match(/name="file"; filename="[^"]*"\r\nContent-Type: ([^\r\n]+)/)?.[1] ??
      "application/octet-stream";
    const form = await request.formData();
    const file = form.get("file") as File;
    const bytes = new Uint8Array(await file.arrayBuffer());
    const field = form.get("precondition");
    return store(
      String(form.get("name")),
      kindOf(declared, bytes),
      bytes,
      declared,
      field === null ? undefined : JSON.parse(String(field))
    );
  }
  return refusal(404, "NOT_FOUND", "not found");
};

// Behind Dispatch's keyed layer: each answer of the upload route, its refusals among them, names the
// upload's key back, as Dispatch's do; the router's 404 for a route it has not names none.
const stubDispatch = keyedLayer(answerDispatch, {
  judged: (response) => response.status !== 404,
}) as typeof fetch;

beforeEach(() => {
  statesTokens = true;
  held = [
    {
      id: "doc-1",
      name: "plan.md",
      slug: "plan-md",
      kind: "doc",
      versions: [{ number: 1, bytes: textOf("# Plan"), mime: "text/markdown", hashed: false }],
    },
    {
      id: "file-1",
      name: "notes",
      slug: "notes",
      kind: "file",
      versions: [{ number: 1, bytes: textOf("first"), mime: "text/plain", hashed: true }],
    },
    {
      id: "image-1",
      name: "chart",
      slug: "chart",
      kind: "image",
      versions: [{ number: 1, bytes: pictureOf("first"), mime: "image/png", hashed: true }],
    },
  ];
  uploads.length = 0;
  stateDir = makeDirectory();
  resetAdviceMemory();
});

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

/** `bytes` written to a file named `file` in the row's directory, for --path. */
function pathOf(file: string, bytes: Uint8Array): string {
  const path = join(stateDir, file);
  writeFileSync(path, bytes);
  return path;
}

/** An upload under `name` of `source` (--path of a file, or --content), plus `flags`. */
function upload(name: string, source: readonly string[], flags: readonly string[] = []) {
  return run(["artifact", "--issue", "DSP-1", "--name", name, ...source, ...flags]);
}

/** The refusal's own line, without the allowed flags and the example every refusal prints. */
function refusalOf(stdout: string, name: string): string {
  const line = stdout
    .split("\n")
    .find((text) => text.startsWith(`- issue DSP-1 already holds ${name} `));
  expect(line, stdout).toBeDefined();
  return line ?? "";
}

/** The token the writer's own `dispatch doc-read` of `slug` printed, read off its header. */
async function readToken(slug: string): Promise<string> {
  const read = await run(["doc-read", "--issue", "DSP-1", "--artifact", slug]);
  expect(read.code, read.stdout).toBe(0);
  const token = read.stdout.split("\n")[1]?.match(/^Document token: (sha256:[0-9a-f]{64})$/)?.[1];
  expect(token, read.stdout).toBeDefined();
  return token ?? "";
}

/** The flags a refusal advises, read out of it: --force with the writer's own token where it names
 *  --precondition-json, --force alone where it says so. */
function advisedFlags(text: string, ownToken: string | undefined): string[] {
  if (text.includes("(--force) alone")) return ["--force"];
  expect(text).toContain("--precondition-json");
  return ["--force", "--precondition-json", JSON.stringify({ document: ownToken })];
}

describe("dispatch artifact under a name the owner holds", () => {
  // R3: the refusal prints no token, and sends the writer to its own read: a writer who read v1
  // and follows it after another session wrote v2 is refused 409, v2 stands, and the same writer
  // reading again is taken.
  test.each([
    ["a file", "notes", "notes.txt", textOf],
    ["a picture", "chart", "chart.png", pictureOf],
  ] as const)("for %s, prints no token, and a stale reader following it cannot overwrite the newer version", async (_label, name, file, contentOf) => {
    const v1Token = await readToken(name);
    const written = await upload(
      name,
      ["--path", pathOf(`b-${file}`, contentOf("from B"))],
      ["--force"]
    );
    expect(written.code, written.stdout).toBe(0);
    const standing = held.find((artifact) => artifact.name === name)?.versions.at(-1);

    const refused = await upload(name, ["--path", pathOf(`a-${file}`, contentOf("from A"))]);
    expect(refused.code, refused.stdout).toBe(2);
    const text = refusalOf(refused.stdout, name);
    expect(text).not.toContain("sha256:");
    expect(text).toContain("dispatch doc-read");
    expect(text).toContain("--force");

    const stale = await upload(
      name,
      ["--path", pathOf(`a-${file}`, contentOf("from A"))],
      advisedFlags(text, v1Token)
    );
    expect(stale.code, stale.stdout).toBe(1);
    expect(stale.stdout).toContain("PRECONDITION_FAILED");
    expect(held.find((artifact) => artifact.name === name)?.versions.at(-1)).toBe(standing);

    const fresh = await upload(
      name,
      ["--path", pathOf(`a-${file}`, contentOf("from A"))],
      advisedFlags(text, await readToken(name))
    );
    expect(fresh.code, fresh.stdout).toBe(0);
    expect(held.find((artifact) => artifact.name === name)?.versions).toHaveLength(3);
  });

  // R3: where the standing version states no token, --force alone, read out of the refusal, is
  // taken: from a Dispatch without file tokens, and for a version stored with no hash.
  test.each([
    [
      "a Dispatch that states no file token",
      () => {
        statesTokens = false;
      },
    ],
    [
      "a version stored with no hash",
      () => {
        const notes = held.find((artifact) => artifact.name === "notes");
        if (notes !== undefined)
          notes.versions[0] = { ...(notes.versions[0] as StoredVersion), hashed: false };
      },
    ],
  ] as const)("advises --force alone for %s, and the upload sending exactly that is taken", async (_label, arrange) => {
    arrange();
    const refused = await upload("notes", ["--path", pathOf("n.txt", textOf("second"))]);
    expect(refused.code, refused.stdout).toBe(2);
    const text = refusalOf(refused.stdout, "notes");
    expect(text).toContain("(--force) alone");
    expect(text).not.toContain("--precondition-json");

    const taken = await upload(
      "notes",
      ["--path", pathOf("n.txt", textOf("second"))],
      advisedFlags(text, undefined)
    );
    expect(taken.code, taken.stdout).toBe(0);
    expect(uploads).toEqual([{ name: "notes", precondition: undefined }]);
  });

  // R3: an upload of another kind than the name holds, by Dispatch's rule, is told to take another
  // name and is offered no flag: Dispatch refuses it under that name with --force too, and takes
  // it under another.
  test.each([
    ["a --content document over a file", "notes", ["--content", "# Notes\n"]],
    ["a --content document over a picture", "chart", ["--content", "# Chart\n"]],
    ["PNG bytes over a file", "notes", ["--path", "png"]],
    ["WebP bytes over a file", "notes", ["--path", "webp"]],
    ["JPEG bytes over a file", "notes", ["--path", "jpeg"]],
    ["GIF bytes over a file", "notes", ["--path", "gif"]],
    ["text bytes over a picture", "chart", ["--path", "text"]],
    ["a RIFF that holds no WebP chunk over a picture", "chart", ["--path", "riff"]],
    ["a file over a document", "plan.md", ["--path", "text"]],
    ["PNG bytes over a document", "plan.md", ["--path", "png"]],
  ] as const)("refuses %s naming another name and no flag", async (_label, name, given) => {
    const source =
      given[0] === "--path"
        ? [
            "--path",
            pathOf(
              `other.${given[1]}`,
              given[1] === "text" ? textOf("other") : bytesOf(given[1], "other")
            ),
          ]
        : [...given];
    const refused = await upload(name, source);
    expect(refused.code, refused.stdout).toBe(2);
    const text = refusalOf(refused.stdout, name);
    expect(text).toContain("Give the upload another name.");
    expect(text).not.toContain("--force");
    expect(text).not.toContain("--precondition-json");
    expect(uploads).toEqual([]);

    const forced = await upload(name, source, ["--force"]);
    expect(forced.code, forced.stdout).toBe(1);
    expect(forced.stdout).toContain("ARTIFACT_KIND_MISMATCH");

    const renamed = await upload(`${name.replace(".md", "")}-other`, source);
    expect(renamed.code, renamed.stdout).toBe(0);
  });

  // R3: a picture of any of the four formats over a held picture, and a RIFF that holds no WebP
  // chunk over a held file, is of the kind the name holds, so the refusal advises the replacement,
  // and the upload sending exactly that, with the writer's own token, is taken as the next version.
  test.each([
    ["WebP bytes over a picture", "chart", "webp"],
    ["JPEG bytes over a picture", "chart", "jpeg"],
    ["GIF bytes over a picture", "chart", "gif"],
    ["a RIFF that holds no WebP chunk over a file", "notes", "riff"],
  ] as const)("advises the replacement for %s, and it is taken", async (_label, name, format) => {
    const source = ["--path", pathOf(`next.${format}`, bytesOf(format, "next"))];
    const refused = await upload(name, source);
    expect(refused.code, refused.stdout).toBe(2);
    const text = refusalOf(refused.stdout, name);
    expect(text).not.toContain("another name");
    expect(text).toContain("--precondition-json");

    const taken = await upload(name, source, advisedFlags(text, await readToken(name)));
    expect(taken.code, taken.stdout).toBe(0);
    expect(taken.stdout).toContain("as version 2");
  });

  // A file holds no text dispatch doc-edit changes, so the refusal never sends the writer there.
  test("does not name dispatch doc-edit for a file", async () => {
    const refused = await upload("notes", ["--path", pathOf("n.txt", textOf("second"))]);
    expect(refusalOf(refused.stdout, "notes")).not.toContain("doc-edit");
  });
});
