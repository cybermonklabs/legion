// packages/envoy-client/src/__tests__/dispatch-api.test.ts
//
// `dispatch api --path <route> [--method POST|PUT|PATCH|DELETE] [--body-file <file>|-]` sends one
// request of Dispatch's HTTP API with the credential every `dispatch` command resolves, and prints
// the answer's body: Dispatch's JSON indented, and any other body of a 2xx byte for byte, stored
// content (an attachment) never parsed whatever its type; a 2xx whose body holds the credential it
// presented is refused. These rows drive it through `runDispatchCli` against a stub secrets broker
// (`stubBroker`) and a stub Dispatch of this file's own, which keeps each request it was sent and
// answers by path: its own whoami, the 401 and the 404 captured from a production Dispatch, a
// proxy's 502 page, a bodiless 204, a redirect, file versions as `serveFileVersion` answers them,
// a document version's JSON and its Markdown download, typed JSON that is empty or cut, plain text
// to any method, and an echo of the presented credential. One table runs each kind of answer with
// a host, with none and under Claude Code. The other rows hold the paths it refuses before
// anything is sent, the credential it presents and the session its trade names, with a host and
// without one, and the AskGate rule: in a session whose gate is on it sends GET alone, and names
// the command that makes the write, or says that no command the session may run makes it; the
// gate sits in the command itself, under a host and with none. A command whose spec binds its
// routes is held to the same rule, so the refusal never names one.

import { describe, expect, test } from "bun:test";
import { createHash } from "node:crypto";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { SUBJECT_TOKEN_TYPE_GITHUB } from "@legion/cipher-client/wire";
import { IDEMPOTENCY_KEY_HEADER } from "@legion/contracts";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { processStart } from "../askgate-marker";
import { cliRefusals, runDispatchCli } from "../dispatch-cli";
import { type ResultEntry, readResultsSince } from "../dispatch-session-state";
import { brokerStandIns } from "./broker-stand-ins";

const { servers, tempDir, stubBroker, podEnvironment } = brokerStandIns("dispatch-api");

/** The session the rows with a host act as. */
const SESSION = "ses-1";

/** What Dispatch answers a request with no credential, captured from a production Dispatch. */
const NO_IDENTITY = { code: "NO_IDENTITY", error: "unauthorized" };
/** What Dispatch answers `GET /api/v1/no-such-route`, captured from a production Dispatch. */
const NO_ROUTE = {
  code: "NOT_FOUND",
  error: "no route for GET /api/v1/no-such-route",
  hint: "GET /api/v1 lists every route",
};
/** The stub's whoami. */
const WHOAMI = {
  session: SESSION,
  owner: null,
  service: "system:serviceaccount:legion:legion-worker",
  project: "ACME",
};
/** A proxy's page in Dispatch's place, which is no JSON. */
const PROXY_PAGE = "<html><body><h1>502 Bad Gateway</h1></body></html>";
/** A JSON answer in Dispatch's place that carries a `code` and is no refusal of Dispatch's,
 *  which always carries its `error`. */
const FOREIGN_JSON = { code: "UPSTREAM_DOWN", message: "the upstream is down" };
/** A document version's Markdown, as `?download=true` answers it: no newline at its end, so one
 *  the command added would show. */
const MARKDOWN = "# Plan\n\nShip the client first.";
/** The same version as Dispatch's JSON, read without `?download=true`. */
const DOCUMENT = { number: 3, markdown: MARKDOWN };
/** A picture version's bytes as Dispatch stores them: a PNG's signature, then two bytes no UTF-8
 *  text holds, which a round trip through text would change. */
const PNG = Uint8Array.of(0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0xff, 0xfe);
/** A file stored as JSON, as a person uploaded it: an integer past 2^53, a decimal with a trailing
 *  zero and the file's own spacing, each of which a parse and a print again would change. */
const DATA_JSON = new TextEncoder().encode('{"id":12345678901234567890,"ratio":1.10,"name":"x"}');
/** A file stored as JSON that holds one string, which a parse would print without its quotes. */
const STRING_JSON = new TextEncoder().encode('"just a JSON string"');
/** A file stored as JSON that holds a list in its own spacing. */
const LIST_JSON = new TextEncoder().encode("[1, 2,   3]");
/** A binary file's bytes, none of them UTF-8 text, and more of them than Claude Code shows of an
 *  output (`CLAUDE_OUTPUT_MAX`), so a cut or a move to a file would show. */
const BINARY = Uint8Array.from({ length: 30_000 }, (_, index) => 0x80 + (index % 0x80));
/** The type a file stored as JSON answers under, as `serveFileVersion` writes the stored one. */
const JSON_FILE_TYPE = "application/json;charset=utf-8";
/** The version of a document, of a picture, of three files stored as JSON and of a binary file
 *  the stub answers. */
const DOCUMENT_VERSION = "/api/v1/artifacts/01234567-0000-4000-8000-000000000002/versions/3";
const PICTURE_VERSION = "/api/v1/artifacts/01234567-0000-4000-8000-000000000003/versions/1";
const DATA_VERSION = "/api/v1/artifacts/01234567-0000-4000-8000-000000000004/versions/1";
const STRING_VERSION = "/api/v1/artifacts/01234567-0000-4000-8000-000000000005/versions/1";
const LIST_VERSION = "/api/v1/artifacts/01234567-0000-4000-8000-000000000006/versions/2";
const BINARY_VERSION = "/api/v1/artifacts/01234567-0000-4000-8000-000000000007/versions/1";
/** A write's answer in plain text, as something no Dispatch route answers yet could. */
const ACCEPTED = "accepted";
/** A body typed JSON that was cut short, so it does not parse. */
const CUT_JSON = '{"issues": [{"key": "ACME-1"';

/** One request a stub server was sent, as it arrived. */
interface Sent {
  readonly method: string;
  readonly url: URL;
  readonly headers: Headers;
  readonly body: string;
}

/**
 * A Dispatch that keeps every request it is sent and answers by path: `/api/v1`, whoami, the
 * captured 401 (`/api/v1/signed-out`) and 404 (`/api/v1/no-such-route`), a proxy's 502 page
 * (`/api/v1/proxied`), a JSON 503 that is no refusal of Dispatch's (`/api/v1/foreign`), a bodiless
 * 204 (`/api/v1/nothing`), a 307 to `redirectTo` (`/api/v1/moved`), a document version as its
 * JSON or, with `?download=true`, its Markdown as an attachment (`DOCUMENT_VERSION`), each file
 * version as `serveFileVersion` answers it (`fileVersion`), typed JSON cut short
 * (`/api/v1/cut`) or empty (`/api/v1/empty`), plain text to any method (`/api/v1/accepted`), the
 * bare token (`/api/v1/echo-token`) or the DPoP proof (`/api/v1/echo-proof`) it was sent, echoed
 * in a plain-text 200, and any other request with the method and path it was sent.
 */
function stubDispatch(redirectTo = "http://127.0.0.1:9"): { url: string; sent: Sent[] } {
  const sent: Sent[] = [];
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: async (request) => {
      const url = new URL(request.url);
      sent.push({
        method: request.method,
        url,
        headers: new Headers(request.headers),
        body: await request.text(),
      });
      switch (url.pathname) {
        case "/api/v1":
          return Response.json({ routes: ["GET /api/v1/whoami"] });
        case "/api/v1/whoami":
          return Response.json(WHOAMI);
        case "/api/v1/signed-out":
          return Response.json(NO_IDENTITY, { status: 401 });
        case "/api/v1/no-such-route":
          return Response.json(NO_ROUTE, { status: 404 });
        case "/api/v1/proxied":
          return new Response(PROXY_PAGE, {
            status: 502,
            headers: { "Content-Type": "text/html" },
          });
        case "/api/v1/foreign":
          return Response.json(FOREIGN_JSON, { status: 503 });
        case "/api/v1/nothing":
          return new Response(null, { status: 204 });
        case "/api/v1/moved":
          return new Response(null, {
            status: 307,
            headers: { Location: `${redirectTo}/api/v1/whoami` },
          });
        case DOCUMENT_VERSION:
          return url.searchParams.get("download") === "true"
            ? fileVersion(new TextEncoder().encode(MARKDOWN), "text/markdown; charset=utf-8")
            : Response.json(DOCUMENT);
        case PICTURE_VERSION:
          return fileVersion(PNG, "image/png");
        case DATA_VERSION:
          return fileVersion(DATA_JSON, JSON_FILE_TYPE);
        case STRING_VERSION:
          return fileVersion(STRING_JSON, JSON_FILE_TYPE);
        case LIST_VERSION:
          return fileVersion(LIST_JSON, JSON_FILE_TYPE);
        case BINARY_VERSION:
          return fileVersion(BINARY, "application/octet-stream");
        case "/api/v1/cut":
          return new Response(CUT_JSON, { headers: { "Content-Type": "application/json" } });
        case "/api/v1/empty":
          return new Response("", { headers: { "Content-Type": "application/json" } });
        case "/api/v1/accepted":
          return new Response(ACCEPTED, { headers: { "Content-Type": "text/plain" } });
        case "/api/v1/echo-token": {
          const token = request.headers.get("Authorization")?.split(" ")[1];
          return new Response(`echoed request token: ${token}`, {
            headers: { "Content-Type": "text/plain" },
          });
        }
        case "/api/v1/echo-proof":
          return new Response(`you sent DPoP: ${request.headers.get("DPoP")}`, {
            headers: { "Content-Type": "text/plain" },
          });
        default:
          return Response.json({ method: request.method, path: url.pathname });
      }
    },
  });
  servers.push(server);
  return { url: `http://127.0.0.1:${server.port}`, sent };
}

/** A file version's answer as `serveFileVersion` writes it: the stored bytes under the stored
 *  type, as an attachment, with the headers it sets on every file version. */
function fileVersion(bytes: Uint8Array<ArrayBuffer>, type: string): Response {
  return new Response(bytes, {
    headers: {
      "Content-Type": type,
      "Cache-Control": "private, max-age=31536000, immutable",
      "Content-Disposition": `attachment; filename="file"; filename*=UTF-8''file`,
      "X-Content-Type-Options": "nosniff",
    },
  });
}

/** What one `dispatch` call printed and exited with, and how often it read standard input. */
interface Ran {
  readonly code: number;
  readonly stdout: string;
  /** Standard output as the bytes written to it, its text and its bytes in the order written. */
  readonly bytes: Uint8Array;
  readonly stderr: string;
  readonly stdinReads: number;
}

/** Runs `dispatch <argv>` in `env`, with `stdin` as standard input and `cwd` as its directory. */
async function dispatch(
  argv: readonly string[],
  env: Record<string, string | undefined>,
  stdin = "",
  cwd = tempDir()
): Promise<Ran> {
  const written: Uint8Array[] = [];
  let stderr = "";
  let stdinReads = 0;
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
    readText: (path) => {
      if (path !== "-") return readFileSync(resolve(cwd, path), "utf8");
      stdinReads += 1;
      return stdin;
    },
    cwd,
  });
  const bytes = new Uint8Array(Buffer.concat(written));
  return { code, stdout: new TextDecoder().decode(bytes), bytes, stderr, stdinReads };
}

/** The claims of a JWT, a DPoP proof here. */
function claimsOf(jwt: string | null): Record<string, unknown> {
  return JSON.parse(Buffer.from((jwt ?? "").split(".")[1] ?? "", "base64url").toString("utf8"));
}

/** A pod's Oh My Pi session `SESSION`: its host and its session id, its state under
 *  `stateDir`, and its Dispatch credential traded at `brokerUrl` for the pod's identity. */
function sessionEnvironment(
  brokerUrl: string,
  dispatchUrl: string,
  stateDir: string
): Record<string, string | undefined> {
  return {
    ...podEnvironment(brokerUrl, dispatchUrl),
    DISPATCH_HOST: "omp",
    DISPATCH_SESSION_ID: SESSION,
    DISPATCH_STATE_DIR: stateDir,
  };
}

/** A job no host runs, as a script or a GitHub Actions step is: the pod's identity to trade and
 *  Dispatch's URL, with no host and no session id. */
function jobEnvironment(
  brokerUrl: string,
  dispatchUrl: string,
  stateDir: string
): Record<string, string | undefined> {
  return { ...podEnvironment(brokerUrl, dispatchUrl), DISPATCH_STATE_DIR: stateDir };
}

/** The session the rows under Claude Code act as. */
const CLAUDE_SESSION = "claude-ses";

/** A Claude Code session `CLAUDE_SESSION`: the job's environment under Claude Code's host and
 *  session id, where a long output is cut and written to a file (`fitOutput`). */
function claudeEnvironment(
  brokerUrl: string,
  dispatchUrl: string,
  stateDir: string
): Record<string, string | undefined> {
  return {
    ...jobEnvironment(brokerUrl, dispatchUrl, stateDir),
    DISPATCH_HOST: "claude",
    CLAUDE_CODE_SESSION_ID: CLAUDE_SESSION,
  };
}

/**
 * The AskGate record of session `id` under `stateDir`, in the format the gate writes and as
 * dispatch-askgate.test.ts writes it: one owner file naming its process and its marker store, in
 * block mode. The owner is this test process by default, which stands in for a live Oh My Pi.
 * Answers the owner file's path.
 */
function writeRecord(
  stateDir: string,
  id: string,
  owner: { readonly pid: number; readonly start: string } = {
    pid: process.pid,
    start: processStart(process.pid) ?? "",
  }
): string {
  const store = join(stateDir, "askgate", `${owner.pid}-${owner.start}`);
  const file = join(stateDir, "sessions", id, "askgate", `${owner.pid}-${owner.start}.json`);
  mkdirSync(dirname(file), { recursive: true });
  writeFileSync(file, JSON.stringify({ v: 2, ...owner, store, mode: "block" }));
  mkdirSync(join(store, "markers"), { recursive: true });
  return file;
}

/** Session `id`'s ledger under `stateDir`, one entry per line, as the CLI appends it; none where
 *  no line was ever written. */
function ledgerOf(stateDir: string, id: string): ResultEntry[] {
  return readResultsSince(join(stateDir, "sessions", id), 0, (problem) => {
    throw new Error(problem);
  }).entries;
}

/** A file of `text` in a directory of this test's own, by its absolute path. */
function bodyFile(text: string): string {
  const file = join(tempDir(), "body.json");
  writeFileSync(file, text);
  return file;
}

/** A comment's body, as a script posts one. */
const COMMENT = JSON.stringify({ body: "Looks good." });

/** Why the gate refuses a write, as the refusal and its ledger line's AskGate mark say it. */
const GATED_WHY =
  "this session's AskGate reviews Dispatch writes, so dispatch api sends only GET here";

/** How the gate refuses a write a hand-written command makes: before the command it names. */
const GATED = `dispatch api was not called: ${GATED_WHY}; make this write with `;

/** How the gate refuses a write no hand-written command makes, whole: no command the session may
 *  run makes it, since the gate refuses that write to a command whose spec binds its route too. */
const GATED_NO_COMMAND = `dispatch api was not called: ${GATED_WHY}; no dispatch command this session may run makes this write`;

/** The ask a command whose spec binds its routes acts on here: `dispatch decline-ask` writes it,
 *  `POST /api/v1/asks/{id}/decline`, and `dispatch read-ask` reads it. */
const ASK = "01234567-0000-4000-8000-000000000009";

/** `dispatch decline-ask`'s write of `ASK`, which no hand-written command makes. */
const DECLINE = ["decline-ask", "--ask", ASK, "--note", "Not mine to decide."];

/** Why the gate refuses `DECLINE`, as the refusal and its ledger line's AskGate mark say it. */
const DECLINE_GATED_WHY =
  "this session's AskGate reviews Dispatch writes, so dispatch decline-ask sends only GET here";

/** How the gate refuses `DECLINE` in a session whose AskGate record is live. */
const DECLINE_GATED = `dispatch decline-ask was not called: ${DECLINE_GATED_WHY}`;

/** The refusal of a body with no method, the contract's rule (`dispatch_api`). */
const BODY_WITHOUT_METHOD =
  "body goes only with method: a call that names no method is a GET, which sends no body.";

/** The characters a URL parser rewrites in a path, which the contract's rule refuses there. */
const REWRITTEN = ['"', "<", ">", "^", "`", "{", "}"];

/** Each of `REWRITTEN` the URL parser percent-encodes in a query too, as it writes it there; it
 *  writes the rest as they are. */
const QUERY_WRITTEN: Readonly<Partial<Record<string, string>>> = {
  '"': "%22",
  "<": "%3C",
  ">": "%3E",
};

describe("a request", () => {
  test("--path /api/v1/whoami trades a token, sends one GET with a new DPoP proof, prints the answer and exits 0", async () => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const ran = await dispatch(
      ["api", "--path", "/api/v1/whoami"],
      sessionEnvironment(broker.url, server.url, tempDir())
    );
    expect(ran.code, ran.stdout).toBe(0);
    expect(JSON.parse(ran.stdout)).toEqual(WHOAMI);
    expect(ran.stderr).toBe("");
    expect(broker.trades).toHaveLength(1);
    const [trade] = broker.trades;
    // The trade names the session the host's session id names.
    expect(trade?.form.get("session")).toBe(SESSION);
    expect(server.sent).toHaveLength(1);
    const [sent] = server.sent;
    expect(sent?.method).toBe("GET");
    expect(sent?.headers.get("Authorization")).toBe(`DPoP ${trade?.token}`);
    const proof = claimsOf(sent?.headers.get("DPoP") ?? null);
    expect(proof.htm).toBe("GET");
    expect(proof.htu).toBe(`${server.url}/api/v1/whoami`);
    expect(proof.ath).toBe(
      createHash("sha256")
        .update(trade?.token ?? "")
        .digest("base64url")
    );
    // A proof of its own, not the trade's.
    expect(proof.jti).toEqual(expect.any(String));
    expect(proof.jti).not.toBe(trade?.proof.jti);
    // One request, no key: a script that repeats it decides that itself.
    expect(sent?.headers.get(IDEMPOTENCY_KEY_HEADER)).toBeNull();
  });

  test("a GET with a query sends the query and signs the URL without it", async () => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const ran = await dispatch(
      ["api", "--path", "/api/v1/issues?project=ACME&status=open"],
      jobEnvironment(broker.url, server.url, tempDir())
    );
    expect(ran.code, ran.stdout).toBe(0);
    const [sent] = server.sent;
    expect(sent?.url.pathname).toBe("/api/v1/issues");
    expect(sent?.url.search).toBe("?project=ACME&status=open");
    expect(claimsOf(sent?.headers.get("DPoP") ?? null).htu).toBe(`${server.url}/api/v1/issues`);
  });

  test("--method POST sends the body of --body-file <file> as application/json", async () => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const ran = await dispatch(
      [
        "api",
        "--method",
        "POST",
        "--path",
        "/api/v1/issues/ACME-1/comments",
        "--body-file",
        bodyFile(COMMENT),
      ],
      jobEnvironment(broker.url, server.url, tempDir())
    );
    expect(ran.code, ran.stdout).toBe(0);
    expect(JSON.parse(ran.stdout)).toEqual({
      method: "POST",
      path: "/api/v1/issues/ACME-1/comments",
    });
    expect(ran.stdinReads).toBe(0);
    expect(server.sent).toHaveLength(1);
    const [sent] = server.sent;
    expect(sent?.method).toBe("POST");
    expect(sent?.body).toBe(COMMENT);
    expect(sent?.headers.get("Content-Type")).toBe("application/json");
    expect(sent?.headers.get(IDEMPOTENCY_KEY_HEADER)).toBeNull();
    expect(claimsOf(sent?.headers.get("DPoP") ?? null).htm).toBe("POST");
  });

  test("--method POST sends standard input with --body-file -, and reads none without it", async () => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const env = jobEnvironment(broker.url, server.url, tempDir());
    const piped = await dispatch(
      ["api", "--method", "POST", "--path", "/api/v1/issues/ACME-1/comments", "--body-file", "-"],
      env,
      COMMENT
    );
    expect(piped.code, piped.stdout).toBe(0);
    expect(piped.stdinReads).toBe(1);
    expect(server.sent[0]?.body).toBe(COMMENT);
    expect(server.sent[0]?.headers.get("Content-Type")).toBe("application/json");

    const bare = await dispatch(
      ["api", "--method", "POST", "--path", "/api/v1/issues/ACME-1/claim"],
      env,
      COMMENT
    );
    expect(bare.code, bare.stdout).toBe(0);
    expect(bare.stdinReads).toBe(0);
    expect(server.sent[1]?.body).toBe("");
    expect(server.sent[1]?.headers.get("Content-Type")).toBeNull();
  });

  test.each([
    ["a GET's 401", ["--path", "/api/v1/signed-out"], "NO_IDENTITY: unauthorized\n"],
    [
      "a write's 401",
      ["--method", "DELETE", "--path", "/api/v1/signed-out"],
      "NO_IDENTITY: unauthorized\n",
    ],
  ])("%s exits 1, printing Dispatch's error with its code, sent once", async (_, argv, printed) => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const ran = await dispatch(["api", ...argv], jobEnvironment(broker.url, server.url, tempDir()));
    expect(ran.code).toBe(1);
    expect(ran.stdout).toBe(printed);
    expect(server.sent).toHaveLength(1);
  });

  test("a JSON answer with a code but no Dispatch error exits 1 naming the status, not its code", async () => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const ran = await dispatch(
      ["api", "--path", "/api/v1/foreign"],
      jobEnvironment(broker.url, server.url, tempDir())
    );
    expect(ran.code).toBe(1);
    expect(ran.stdout).toContain("/api/v1/foreign answered 503");
    expect(ran.stdout).not.toStartWith("UPSTREAM_DOWN");
  });

  test("/api/v1 itself is sent", async () => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const ran = await dispatch(
      ["api", "--path", "/api/v1"],
      jobEnvironment(broker.url, server.url, tempDir())
    );
    expect(ran.code, ran.stdout).toBe(0);
    expect(server.sent.map((sent) => sent.url.pathname)).toEqual(["/api/v1"]);
  });

  test("a redirect is not followed", async () => {
    const broker = stubBroker(300);
    const elsewhere = stubDispatch();
    const server = stubDispatch(elsewhere.url);
    const ran = await dispatch(
      ["api", "--path", "/api/v1/moved"],
      jobEnvironment(broker.url, server.url, tempDir())
    );
    expect(ran.code).toBe(1);
    expect(ran.stdout).toContain("/api/v1/moved answered 307");
    expect(ran.stdout).toContain("which this client does not follow");
    expect(server.sent).toHaveLength(1);
    expect(elsewhere.sent).toHaveLength(0);
  });

  test("a set DISPATCH_TOKEN is sent in place of a trade", async () => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const bearer = fixtureSecret("dsp");
    const ran = await dispatch(["api", "--path", "/api/v1/whoami"], {
      ...jobEnvironment(broker.url, server.url, tempDir()),
      DISPATCH_TOKEN: bearer,
    });
    expect(ran.code, ran.stdout).toBe(0);
    expect(broker.trades).toHaveLength(0);
    expect(server.sent[0]?.headers.get("Authorization")).toBe(`Bearer ${bearer}`);
    expect(server.sent[0]?.headers.get("DPoP")).toBeNull();
  });
});

/** The three ways the command runs, each with the session whose ledger it writes: as an Oh My Pi
 *  session, in a job no host runs (which writes none), and as a Claude Code session, whose output
 *  past `CLAUDE_OUTPUT_MAX` is cut and written to a file (`fitOutput`, `writeLongOutput`). */
const RUNS = [
  ["with a host", sessionEnvironment, SESSION],
  ["with no host", jobEnvironment, undefined],
  ["under Claude Code", claudeEnvironment, CLAUDE_SESSION],
] as const;

/** One kind of answer: the call, its exit, its standard output exactly, and what a session's
 *  ledger records of it. */
interface AnswerKind {
  readonly kind: string;
  readonly argv: readonly string[];
  readonly code: number;
  /** Standard output, the stored bytes where the answer is stored content; `url` is the stub
   *  Dispatch's. */
  readonly stdout: string | Uint8Array | ((url: string) => string);
  /** The ledger line's details, or `refusal` for a refusal, whose line holds its text. */
  readonly ledger: Readonly<Record<string, unknown>> | "refusal";
  readonly bearer?: boolean;
}

/** A 2xx body's details in the ledger: its path, its type and its size, never its bytes. */
function bodyDetails(
  argv: readonly string[],
  type: string | undefined,
  body: string | Uint8Array
): Record<string, unknown> {
  const method = methodOf(argv);
  return {
    path: argv[argv.indexOf("--path") + 1],
    ...(method === "GET" ? {} : { method }),
    ...(type === undefined ? {} : { content_type: type }),
    bytes: typeof body === "string" ? Buffer.byteLength(body) : body.byteLength,
  };
}

/** The method `argv` sends: the one `--method` names, else GET. */
function methodOf(argv: readonly string[]): string {
  const at = argv.indexOf("--method");
  return at === -1 ? "GET" : (argv[at + 1] ?? "");
}

/** The row of a 2xx answer whose body is printed byte for byte as `body`, typed `type`. */
function asItCame(
  kind: string,
  argv: readonly string[],
  type: string | undefined,
  body: string | Uint8Array
): AnswerKind {
  return { kind, argv, code: 0, stdout: body, ledger: bodyDetails(argv, type, body) };
}

/** The row of a 200 whose body echoes what the request presented on `route`: the command
 *  refuses it, naming the status, that a write may have been made, to a GET as to a POST, and no
 *  retry, and prints nothing of the body. */
function echoed(kind: string, method: "GET" | "POST", bearer: boolean): AnswerKind {
  const path = bearer ? "/api/v1/echo-token" : "/api/v1/echo-proof";
  return {
    kind,
    argv: method === "GET" ? ["--path", path] : ["--method", method, "--path", path],
    code: 1,
    stdout:
      "CREDENTIAL_ECHOED: The answer had HTTP status 200 (2xx), so a write may have been made. The body held a credential this command presented. It is not printed. Something in Dispatch's place appears to have echoed the request. Check whether any write took effect before sending it again.\n",
    ledger: "refusal",
    bearer,
  };
}

/** Each kind of answer the command can be given, and what it prints and exits with. */
const ANSWER_KINDS: readonly AnswerKind[] = [
  {
    kind: "Dispatch's JSON is printed indented, as every command prints it",
    argv: ["--path", DOCUMENT_VERSION],
    code: 0,
    stdout: `${JSON.stringify(DOCUMENT, null, 2)}\n`,
    ledger: { path: DOCUMENT_VERSION },
  },
  asItCame(
    "a 2xx typed JSON whose body does not parse is printed as it came",
    ["--path", "/api/v1/cut"],
    "application/json",
    CUT_JSON
  ),
  asItCame(
    "a 2xx typed JSON with an empty body prints nothing",
    ["--path", "/api/v1/empty"],
    "application/json",
    ""
  ),
  asItCame(
    "a file stored as JSON keeps its 64-bit integer, its 1.10 and its spacing",
    ["--path", DATA_VERSION],
    JSON_FILE_TYPE,
    DATA_JSON
  ),
  asItCame(
    "a file that holds one JSON string, read with download=true, keeps its quotes",
    ["--path", `${STRING_VERSION}?download=true`],
    JSON_FILE_TYPE,
    STRING_JSON
  ),
  asItCame(
    "a file that holds a JSON list keeps its spacing",
    ["--path", LIST_VERSION],
    JSON_FILE_TYPE,
    LIST_JSON
  ),
  asItCame("a picture's bytes go out unchanged", ["--path", PICTURE_VERSION], "image/png", PNG),
  asItCame(
    "a binary file longer than Claude Code shows goes out whole",
    ["--path", BINARY_VERSION],
    "application/octet-stream",
    BINARY
  ),
  asItCame(
    "a document's Markdown download is printed exactly as it came",
    ["--path", `${DOCUMENT_VERSION}?download=true`],
    "text/markdown; charset=utf-8",
    new TextEncoder().encode(MARKDOWN)
  ),
  asItCame(
    "a POST answered 200 in plain text is printed as it came",
    ["--method", "POST", "--path", "/api/v1/accepted"],
    "text/plain",
    ACCEPTED
  ),
  asItCame(
    "a bodiless 204 prints nothing",
    ["--method", "DELETE", "--path", "/api/v1/nothing"],
    undefined,
    ""
  ),
  {
    kind: "Dispatch's refusal is printed with its code",
    argv: ["--path", "/api/v1/no-such-route"],
    code: 1,
    stdout: "NOT_FOUND: no route for GET /api/v1/no-such-route; GET /api/v1 lists every route\n",
    ledger: "refusal",
  },
  {
    kind: "a gateway's page is refused naming its status",
    argv: ["--path", "/api/v1/proxied"],
    code: 1,
    stdout: (url) =>
      `HTTP_502: GET ${url}/api/v1/proxied answered 502 Bad Gateway with a body that is not Dispatch's error JSON ("502 Bad Gateway"), which looks like a proxy or gateway page rather than Dispatch's own answer, so a retry may succeed.\n`,
    ledger: "refusal",
  },
  echoed("a GET answered 200 with the bearer it presented", "GET", true),
  echoed("a POST answered 200 with the bearer it presented", "POST", true),
  echoed("a GET answered 200 with the DPoP proof it presented", "GET", false),
  echoed("a POST answered 200 with the DPoP proof it presented", "POST", false),
];

// One table of the answers the command can be given, each run with a host, with none and under
// Claude Code (whose long-output handling must not cut or move an answer written as bytes).
// Dispatch's JSON is printed indented; every other 2xx body goes to standard output as the bytes
// that came, alone, with nothing added (review 6059716082, finding 1), stored content (an
// attachment) never parsed whatever its type (test handoff v9); each exits 0. A refusal exits 1
// and is the one thing printed, and a 2xx that echoes the credential the command presented is one.
describe("each kind of answer", () => {
  test.each(
    ANSWER_KINDS.flatMap((answer) =>
      RUNS.map(
        ([run, environment, session]) => [answer.kind, run, answer, environment, session] as const
      )
    )
  )("%s: %s", async (_, __, answer, environment, session) => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const stateDir = tempDir();
    const bearer = answer.bearer ? fixtureSecret("dsp") : undefined;
    const ran = await dispatch(["api", ...answer.argv], {
      ...environment(broker.url, server.url, stateDir),
      DISPATCH_TOKEN: bearer,
    });
    const printed = typeof answer.stdout === "function" ? answer.stdout(server.url) : answer.stdout;
    const expected = typeof printed === "string" ? new TextEncoder().encode(printed) : printed;
    expect(ran.code, ran.stdout.slice(0, 300)).toBe(answer.code);
    // Standard output is exactly the answer or the refusal, its sha256 the stored bytes' own.
    expect(ran.bytes).toEqual(expected);
    expect(createHash("sha256").update(ran.bytes).digest("hex")).toBe(
      createHash("sha256").update(expected).digest("hex")
    );
    expect(ran.stderr).toBe("");
    expect(server.sent.map((sent) => sent.method)).toEqual([methodOf(answer.argv)]);
    const headers = server.sent[0]?.headers;
    const token = bearer ?? broker.trades[0]?.token;
    expect(token).toBeTruthy();
    expect(headers?.get("Authorization")).toBe(
      `${bearer === undefined ? "DPoP" : "Bearer"} ${token}`
    );
    const proof = headers?.get("DPoP");
    if (bearer === undefined) expect(proof).toBeTruthy();
    else expect(proof).toBeNull();
    for (const credential of [token, proof]) {
      if (credential) {
        expect(ran.stdout.includes(credential) || ran.stderr.includes(credential)).toBe(false);
      }
    }
    if (session === undefined) {
      expect(existsSync(join(stateDir, "sessions"))).toBe(false);
      return;
    }
    // The ledger records a refusal's text and an answer's details, never a body's bytes.
    expect(ledgerOf(stateDir, session)).toEqual([
      {
        at: expect.any(String),
        tool: "dispatch_api",
        ...(answer.ledger === "refusal"
          ? { error: ran.stdout.trimEnd() }
          : { details: answer.ledger }),
      },
    ]);
    // No answer is moved to a file: under Claude Code too, a long one is written whole.
    expect(existsSync(join(stateDir, "sessions", session, "out"))).toBe(false);
  });

  test("with a host, a note of the CLI's own goes to standard error beside bytes, and before a JSON answer as for every command", async () => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const env = sessionEnvironment(broker.url, server.url, tempDir());
    // A checkout's OpenCode-era file, which the CLI names on every call and does not read.
    const cwd = tempDir();
    mkdirSync(join(cwd, ".opencode"), { recursive: true });
    writeFileSync(join(cwd, ".opencode", "envoy.json"), "{}");

    const picture = await dispatch(["api", "--path", PICTURE_VERSION], env, "", cwd);
    expect(picture.code, picture.stdout).toBe(0);
    expect(picture.bytes).toEqual(PNG);
    expect(picture.stderr).toStartWith(
      `dispatch: ${join(cwd, ".opencode", "envoy.json")} is not read`
    );

    const whoami = await dispatch(["api", "--path", "/api/v1/whoami"], env, "", cwd);
    expect(whoami.code, whoami.stdout).toBe(0);
    expect(whoami.stdout).toBe(`${picture.stderr}${JSON.stringify(WHOAMI, null, 2)}\n`);
    expect(whoami.stderr).toBe("");
  });
});

describe("the calls it refuses", () => {
  test.each([
    ["an absolute URL", ["--path", "https://other.example/api/v1/whoami"]],
    ["a URL with no scheme", ["--path", "//other.example/api/v1/whoami"]],
    ["a path outside /api/v1", ["--path", "/healthz"]],
    ["a dot segment", ["--path", "/api/v1/../healthz"]],
    ["an encoded dot segment", ["--path", "/api/v1/%2e%2e/healthz"]],
    ["a path that only begins as /api/v1 does", ["--path", "/api/v1x"]],
    ["another version's path", ["--path", "/api/v2/whoami"]],
    ["a path in backslashes", ["--path", "\\\\other.example/api/v1/whoami"]],
    ["no path", ["--method", "POST"]],
    ["an unknown method", ["--method", "FETCH", "--path", "/api/v1/whoami"]],
    ["GET, which is no value of method", ["--method", "GET", "--path", "/api/v1/whoami"]],
  ])("each is a usage error and nothing is sent: %s", async (_, argv) => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const ran = await dispatch(
      ["api", ...argv],
      jobEnvironment(broker.url, server.url, tempDir()),
      COMMENT
    );
    expect(ran.code).toBe(2);
    expect(ran.stdout).toStartWith("dispatch api was not called: ");
    expect(ran.stderr).toBe("");
    expect(ran.stdinReads).toBe(0);
    expect(server.sent).toHaveLength(0);
    expect(broker.trades).toHaveLength(0);
  });

  test("the old positional form is a usage error of the contract's parser", async () => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const ran = await dispatch(
      ["api", "GET", "/api/v1/whoami"],
      jobEnvironment(broker.url, server.url, tempDir())
    );
    expect(ran.code).toBe(2);
    expect(ran.stdout).toStartWith("dispatch api was not called: 2 problems\n");
    expect(ran.stdout).toContain("unexpected argument GET: every value follows its flag");
    expect(server.sent).toHaveLength(0);
  });

  test("a body with no method is refused, read from a file or from standard input, and nothing is sent", async () => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const env = jobEnvironment(broker.url, server.url, tempDir());
    for (const operand of [bodyFile(COMMENT), "-"]) {
      const ran = await dispatch(
        ["api", "--path", "/api/v1/whoami", "--body-file", operand],
        env,
        COMMENT
      );
      expect({ operand, code: ran.code }).toEqual({ operand, code: 2 });
      expect(ran.stdout).toStartWith("dispatch api was not called: 1 problem\n");
      expect(ran.stdout).toContain(BODY_WITHOUT_METHOD);
    }
    expect(server.sent).toHaveLength(0);
    expect(broker.trades).toHaveLength(0);
  });

  test.each(REWRITTEN)(
    "a route holding %s is refused naming the path, and in the query it is sent as the URL parser writes it",
    async (character) => {
      const broker = stubBroker(300);
      const server = stubDispatch();
      const env = jobEnvironment(broker.url, server.url, tempDir());
      const path = `/api/v1/issues/a${character}b`;
      const refused = await dispatch(["api", "--path", path], env);
      expect(refused.code).toBe(2);
      expect(refused.stdout).toContain(
        `path ${JSON.stringify(path)} holds before its query a character a URL parser rewrites in a path`
      );
      expect(refused.stdout).toContain("percent-encode it");
      expect(server.sent).toHaveLength(0);

      const taken = await dispatch(["api", "--path", `/api/v1/whoami?q=a${character}b`], env);
      expect(taken.code, taken.stdout).toBe(0);
      expect(server.sent.map((sent) => sent.url.pathname)).toEqual(["/api/v1/whoami"]);
      // The rule leaves the query alone; the URL parser percent-encodes ", < and > in it.
      expect(server.sent[0]?.url.search).toBe(`?q=a${QUERY_WRITTEN[character] ?? character}b`);
    }
  );
});

describe("with no host", () => {
  test.each([
    [
      "the session of DISPATCH_SESSION_ID",
      { DISPATCH_SESSION_ID: "job-run-1", AGENT_SECRETS_SESSION: "job-fixed" },
      "job-run-1",
    ],
    ["the session of AGENT_SECRETS_SESSION", { AGENT_SECRETS_SESSION: "job-fixed" }, "job-fixed"],
    ["no session", {}, null],
  ])("it reads the environment, and its trade names %s", async (_, named, session) => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    // An envoy.json that would turn Dispatch off, which a command with no host does not read.
    const config = join(tempDir(), "envoy.json");
    writeFileSync(config, JSON.stringify({ dispatch: { enabled: false } }));
    const ran = await dispatch(["api", "--path", "/api/v1/whoami"], {
      ...jobEnvironment(broker.url, server.url, tempDir()),
      ENVOY_CONFIG_FILE: config,
      ...named,
    });
    expect(ran.code, ran.stdout).toBe(0);
    expect(server.sent).toHaveLength(1);
    expect(broker.trades.map((trade) => trade.form.get("session"))).toEqual([session]);
  });

  test("with no DISPATCH_URL it names DISPATCH_URL and the credential variables", async () => {
    const ran = await dispatch(["api", "--path", "/api/v1/whoami"], {
      HOME: tempDir(),
      DISPATCH_STATE_DIR: tempDir(),
    });
    expect(ran.code).toBe(2);
    expect(ran.stdout).toBe(
      "dispatch api was not called: no Dispatch is set; set DISPATCH_URL, and a credential (DISPATCH_TOKEN, DISPATCH_TOKEN_FILE, DISPATCH_BROKER_TOKEN_FILE and DISPATCH_BROKER_DPOP_KEY_FILE, or the broker's AGENT_SECRETS_URL and AGENT_SECRETS_MACHINE_IDENTITY)\n"
    );
  });

  test("--dry-run checks the call and prints it, sending nothing", async () => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const ran = await dispatch(
      [
        "api",
        "--method",
        "POST",
        "--path",
        "/api/v1/issues/ACME-1/comments",
        "--body-file",
        bodyFile(COMMENT),
        "--dry-run",
      ],
      jobEnvironment(broker.url, server.url, tempDir())
    );
    expect(ran.code, ran.stdout).toBe(0);
    expect(JSON.parse(ran.stdout)).toEqual({
      method: "POST",
      path: "/api/v1/issues/ACME-1/comments",
      body: COMMENT,
    });
    expect(server.sent).toHaveLength(0);
    expect(broker.trades).toHaveLength(0);
  });

  test("a GitHub Actions run with no DISPATCH_HOST writes through dispatch api", async () => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const github = stubGitHub();
    const ran = await dispatch(
      [
        "api",
        "--method",
        "POST",
        "--path",
        "/api/v1/issues/ACME-1/comments",
        "--body-file",
        bodyFile(COMMENT),
      ],
      {
        HOME: tempDir(),
        DISPATCH_STATE_DIR: tempDir(),
        DISPATCH_URL: server.url,
        AGENT_SECRETS_URL: broker.url,
        AGENT_SECRETS_MACHINE_IDENTITY: "github-actions",
        ACTIONS_ID_TOKEN_REQUEST_URL: github.requestUrl,
        ACTIONS_ID_TOKEN_REQUEST_TOKEN: REQUEST_TOKEN,
        AGENT_SECRETS_MACHINE_AUDIENCE: ACTIONS.audience,
      }
    );
    expect(ran.code, ran.stdout).toBe(0);
    // The job asked GitHub for its token for the broker's audience, and traded that token.
    expect(github.asked).toEqual([
      { authorization: `bearer ${REQUEST_TOKEN}`, audience: ACTIONS.audience },
    ]);
    expect(broker.trades).toHaveLength(1);
    expect(broker.trades[0]?.form.get("subject_token_type")).toBe(SUBJECT_TOKEN_TYPE_GITHUB);
    expect(broker.trades[0]?.form.get("subject_token")).toBe(github.answered[0] ?? "");
    expect(server.sent).toHaveLength(1);
    expect(server.sent[0]?.body).toBe(COMMENT);
    expect(server.sent[0]?.headers.get("Authorization")).toBe(`DPoP ${broker.trades[0]?.token}`);
  });
});

describe("the host and the session", () => {
  test("with a host and no session id it is the usage error the other commands give", async () => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const env = {
      ...sessionEnvironment(broker.url, server.url, tempDir()),
      DISPATCH_SESSION_ID: undefined,
    };
    const api = await dispatch(["api", "--path", "/api/v1/whoami"], env);
    const whoami = await dispatch(["whoami"], env);
    expect(whoami.code).toBe(2);
    expect(api.code).toBe(2);
    expect(api.stdout).toBe(whoami.stdout);
    expect(server.sent).toHaveLength(0);
  });

  test.each(["DISPATCH_SESSION_ID", "ENVOY_SESSION_ID", "CLAUDE_CODE_SESSION_ID"])(
    "with no host, a write with a session id sessionDirectory refuses is a usage error naming its variable, and a GET is sent: %s",
    async (variable) => {
      const broker = stubBroker(300);
      const server = stubDispatch();
      const stateDir = tempDir();
      const env = {
        ...jobEnvironment(broker.url, server.url, stateDir),
        [variable]: "../elsewhere",
      };
      const ran = await dispatch(
        ["api", "--method", "POST", "--path", "/api/v1/issues/ACME-1/claim"],
        env
      );
      expect(ran.code).toBe(2);
      expect(ran.stdout).toStartWith("dispatch api was not called: ");
      expect(ran.stdout).toContain(variable);
      expect(ran.stdout).not.toContain("../elsewhere");
      expect(server.sent).toHaveLength(0);
      // A GET reads no AskGate record, so no session id stops it.
      const read = await dispatch(["api", "--path", "/api/v1/whoami"], env);
      expect(read.code, read.stdout).toBe(0);
      expect(server.sent.map((sent) => sent.method)).toEqual(["GET"]);
      // The session the refusal would be recorded for is the id refused, so no ledger is written.
      expect(existsSync(join(stateDir, "sessions"))).toBe(false);
    }
  );

  test("a CODEX_THREAD_ID sessionDirectory refuses is Codex's session, refused as the other commands refuse it", async () => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const stateDir = tempDir();
    const env = { ...jobEnvironment(broker.url, server.url, stateDir), CODEX_THREAD_ID: "../x" };
    const api = await dispatch(["api", "--path", "/api/v1/whoami"], env);
    const whoami = await dispatch(["whoami"], env);
    expect(api.code).toBe(2);
    expect(api.stdout).toBe(whoami.stdout);
    expect(server.sent).toHaveLength(0);
    expect(existsSync(join(stateDir, "sessions"))).toBe(false);
  });
});

describe("the session's ledger", () => {
  test("a gated POST's refusal is recorded with AskGate's mark, and a request that was sent as every call's outcome is", async () => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const stateDir = tempDir();
    writeRecord(stateDir, SESSION);
    const env = sessionEnvironment(broker.url, server.url, stateDir);
    const refused = await dispatch(
      [
        "api",
        "--method",
        "POST",
        "--path",
        "/api/v1/issues/ACME-1/comments",
        "--body-file",
        bodyFile(COMMENT),
      ],
      env
    );
    expect(refused.code).toBe(1);
    const logged = ledgerOf(stateDir, SESSION);
    // The mark is what pi-envoy counts an AskGate refusal by, as it counts the CLI's own refusal
    // of a gated write that carries no marker.
    expect(logged).toEqual([
      {
        at: expect.any(String),
        tool: "dispatch_api",
        error: refused.stdout.trimEnd(),
        askgate: { mode: "block", why: GATED_WHY },
      },
    ]);
    expect(cliRefusals(`${logged[0]?.error}\n`)).toHaveLength(1);
    // A request that was sent is recorded as every call's outcome is, with no AskGate mark.
    const missing = await dispatch(["api", "--path", "/api/v1/no-such-route"], env);
    expect(missing.code).toBe(1);
    expect((await dispatch(["api", "--path", "/api/v1/whoami"], env)).code).toBe(0);
    expect(server.sent).toHaveLength(2);
    expect(ledgerOf(stateDir, SESSION).slice(1)).toEqual([
      { at: expect.any(String), tool: "dispatch_api", error: missing.stdout.trimEnd() },
      { at: expect.any(String), tool: "dispatch_api", details: { path: "/api/v1/whoami" } },
    ]);
  });

  test("a write a broken AskGate record refuses is recorded with AskGate's mark naming the record's problem", async () => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const stateDir = tempDir();
    const file = writeRecord(stateDir, SESSION);
    writeFileSync(file, "{not json");
    const refused = await dispatch(
      ["api", "--method", "POST", "--path", "/api/v1/issues/ACME-1/claim"],
      sessionEnvironment(broker.url, server.url, stateDir)
    );
    expect(refused.code).toBe(1);
    expect(server.sent).toHaveLength(0);
    expect(ledgerOf(stateDir, SESSION)).toEqual([
      {
        at: expect.any(String),
        tool: "dispatch_api",
        error: refused.stdout.trimEnd(),
        askgate: { mode: "block", why: expect.stringContaining(file) },
      },
    ]);
  });

  test.each([
    [
      "a path refused with no host",
      ["--path", "/api/v1/../healthz"],
      { DISPATCH_SESSION_ID: "job-run-1" },
      "job-run-1",
    ],
    [
      "an unknown method under Claude Code",
      ["--method", "FETCH", "--path", "/api/v1/whoami"],
      {
        DISPATCH_HOST: "claude",
        CLAUDE_CODE_SESSION_ID: "claude-ses",
        DISPATCH_SESSION_ID: "other",
      },
      "claude-ses",
    ],
    [
      "a path refused under Codex",
      ["--path", "/healthz"],
      { CODEX_THREAD_ID: "codex-thread", DISPATCH_SESSION_ID: "other" },
      "codex-thread",
    ],
    [
      "no Dispatch with no host",
      ["--path", "/api/v1/whoami"],
      { DISPATCH_URL: undefined, DISPATCH_SESSION_ID: "job-run-1" },
      "job-run-1",
    ],
  ])(
    "a refusal before any request is recorded for the session the call knows: %s",
    async (_, argv, named, session) => {
      const broker = stubBroker(300);
      const server = stubDispatch();
      const stateDir = tempDir();
      const ran = await dispatch(["api", ...argv], {
        ...jobEnvironment(broker.url, server.url, stateDir),
        ...named,
      });
      expect(ran.code).toBe(2);
      expect(server.sent).toHaveLength(0);
      expect(ledgerOf(stateDir, session)).toEqual([
        { at: expect.any(String), tool: "dispatch_api", error: ran.stdout.trimEnd() },
      ]);
    }
  );
});

describe("AskGate", () => {
  test.each([
    ["GET", "/api/v1/whoami", null],
    ["POST", "/api/v1/issues/ACME-1/comments", `${GATED}dispatch comment or dispatch suggest`],
    ["PUT", "/api/v1/projects/ACME/settings", GATED_NO_COMMAND],
    ["PATCH", "/api/v1/projects/ACME/settings", GATED_NO_COMMAND],
    ["DELETE", "/api/v1/projects/ACME/settings", GATED_NO_COMMAND],
  ])(
    "in a session AskGate gates, GET is sent and each other method is refused with nothing sent: %s %s",
    async (method, path, refusal) => {
      const broker = stubBroker(300);
      const server = stubDispatch();
      const stateDir = tempDir();
      writeRecord(stateDir, SESSION);
      const ran = await dispatch(
        [
          "api",
          "--path",
          path,
          ...(method === "GET" ? [] : ["--method", method, "--body-file", bodyFile(COMMENT)]),
        ],
        sessionEnvironment(broker.url, server.url, stateDir)
      );
      if (refusal === null) {
        expect(ran.code, ran.stdout).toBe(0);
        expect(server.sent).toHaveLength(1);
        return;
      }
      expect(ran.code).toBe(1);
      expect(ran.stdout.trimEnd()).toBe(refusal);
      expect(server.sent).toHaveLength(0);
      expect(broker.trades).toHaveLength(0);
    }
  );

  test("POST to a document's comments names dispatch comment and dispatch suggest", async () => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const stateDir = tempDir();
    writeRecord(stateDir, SESSION);
    const ran = await dispatch(
      [
        "api",
        "--method",
        "POST",
        "--path",
        "/api/v1/artifacts/01234567-0000-4000-8000-000000000001/comments",
        "--body-file",
        bodyFile(COMMENT),
      ],
      sessionEnvironment(broker.url, server.url, stateDir)
    );
    expect(ran.code).toBe(1);
    expect(ran.stdout).toStartWith(GATED);
    expect(ran.stdout).toContain("dispatch comment");
    expect(ran.stdout).toContain("dispatch suggest");
    expect(server.sent).toHaveLength(0);
  });

  test("a broken AskGate record refuses each method but GET", async () => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const stateDir = tempDir();
    const file = writeRecord(stateDir, SESSION);
    writeFileSync(file, "{not json");
    const env = sessionEnvironment(broker.url, server.url, stateDir);
    const get = await dispatch(["api", "--path", "/api/v1/whoami"], env);
    expect(get.code, get.stdout).toBe(0);
    expect(server.sent).toHaveLength(1);
    for (const method of ["POST", "PUT", "PATCH", "DELETE"]) {
      const ran = await dispatch(
        ["api", "--method", method, "--path", "/api/v1/issues/ACME-1/claim"],
        env
      );
      expect({ method, code: ran.code }).toEqual({ method, code: 1 });
      expect(ran.stdout).toStartWith("dispatch api was not called: ");
      expect(ran.stdout).toContain(file);
    }
    expect(server.sent).toHaveLength(1);
  });

  test("an AskGate record whose owner is gone sends every method", async () => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const stateDir = tempDir();
    const child = Bun.spawn(["true"]);
    await child.exited;
    writeRecord(stateDir, SESSION, { pid: child.pid, start: "1" });
    const env = sessionEnvironment(broker.url, server.url, stateDir);
    for (const method of ["GET", "POST", "PUT", "PATCH", "DELETE"]) {
      const ran = await dispatch(
        [
          "api",
          "--path",
          "/api/v1/issues/ACME-1/claim",
          ...(method === "GET" ? [] : ["--method", method]),
        ],
        env
      );
      expect({ method, code: ran.code, stderr: ran.stderr }).toEqual({
        method,
        code: 0,
        stderr: "",
      });
    }
    expect(server.sent.map((sent) => sent.method)).toEqual([
      "GET",
      "POST",
      "PUT",
      "PATCH",
      "DELETE",
    ]);
  });

  test.each(["DISPATCH_SESSION_ID", "ENVOY_SESSION_ID", "CLAUDE_CODE_SESSION_ID"])(
    "with no host, a gated %s refuses POST and sends nothing",
    async (variable) => {
      const broker = stubBroker(300);
      const server = stubDispatch();
      const stateDir = tempDir();
      writeRecord(stateDir, SESSION);
      const ran = await dispatch(
        [
          "api",
          "--method",
          "POST",
          "--path",
          "/api/v1/issues/ACME-1/comments",
          "--body-file",
          bodyFile(COMMENT),
        ],
        { ...jobEnvironment(broker.url, server.url, stateDir), [variable]: SESSION }
      );
      expect(ran.code).toBe(1);
      expect(ran.stdout).toStartWith(GATED);
      expect(server.sent).toHaveLength(0);
      expect(broker.trades).toHaveLength(0);
    }
  );

  test("with no host and no record, POST is sent", async () => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const ran = await dispatch(
      [
        "api",
        "--method",
        "POST",
        "--path",
        "/api/v1/issues/ACME-1/comments",
        "--body-file",
        bodyFile(COMMENT),
      ],
      { ...jobEnvironment(broker.url, server.url, tempDir()), DISPATCH_SESSION_ID: SESSION }
    );
    expect(ran.code, ran.stdout).toBe(0);
    expect(server.sent.map((sent) => sent.method)).toEqual(["POST"]);
  });
});

// The commands whose specs bind their routes (`DispatchToolSpec.routes`, `routeCommand`) reach
// routes that a gated session could reach before only through `dispatch api`, which AskGate does
// not review either. So each one's write is refused there as `dispatch api`'s is, and the refusal
// of `dispatch api` never names one.
describe("AskGate and the commands whose specs bind their routes", () => {
  test.each([
    ["is live", false],
    ["cannot be read", true],
  ])("with an AskGate record that %s, a bound write is refused with AskGate's mark, and nothing is sent", async (_, broken) => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const stateDir = tempDir();
    const file = writeRecord(stateDir, SESSION);
    if (broken) writeFileSync(file, "{not json");
    const ran = await dispatch(DECLINE, sessionEnvironment(broker.url, server.url, stateDir));
    expect(ran.code).toBe(1);
    expect(ran.stdout).toStartWith("dispatch decline-ask was not called: ");
    expect(ran.stdout.trimEnd()).toEqual(broken ? expect.stringContaining(file) : DECLINE_GATED);
    expect(server.sent).toHaveLength(0);
    expect(broker.trades).toHaveLength(0);
    expect(ledgerOf(stateDir, SESSION)).toEqual([
      {
        at: expect.any(String),
        tool: "dispatch_decline_ask",
        error: ran.stdout.trimEnd(),
        askgate: { mode: "block", why: broken ? expect.stringContaining(file) : DECLINE_GATED_WHY },
      },
    ]);
    expect(cliRefusals(ran.stdout)).toEqual([
      { line: ran.stdout.trimEnd(), command: "decline-ask" },
    ]);
  });

  test("with a live AskGate record, a bound read is sent", async () => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const stateDir = tempDir();
    writeRecord(stateDir, SESSION);
    const ran = await dispatch(
      ["read-ask", "--ask", ASK],
      sessionEnvironment(broker.url, server.url, stateDir)
    );
    expect(ran.code, ran.stdout).toBe(0);
    expect(server.sent.map((sent) => `${sent.method} ${sent.url.pathname}`)).toEqual([
      `GET /api/v1/asks/${ASK}`,
    ]);
  });

  test.each([
    ["no AskGate record", false],
    ["an AskGate record whose owner is gone", true],
  ])("with %s, the bound write is sent", async (_, gone) => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const stateDir = tempDir();
    if (gone) {
      const child = Bun.spawn(["true"]);
      await child.exited;
      writeRecord(stateDir, SESSION, { pid: child.pid, start: "1" });
    }
    const ran = await dispatch(DECLINE, sessionEnvironment(broker.url, server.url, stateDir));
    expect(ran.code, ran.stdout).toBe(0);
    expect(server.sent.map((sent) => `${sent.method} ${sent.url.pathname}`)).toEqual([
      `POST /api/v1/asks/${ASK}/decline`,
    ]);
    expect(JSON.parse(server.sent[0]?.body ?? "{}")).toMatchObject({ note: "Not mine to decide." });
    expect(ledgerOf(stateDir, SESSION)).toEqual([
      { at: expect.any(String), tool: "dispatch_decline_ask", details: expect.any(Object) },
    ]);
  });

  test.each([
    // A route only a command whose spec binds it sends: the refusal says that no command the
    // session may run makes it.
    ["POST", `/api/v1/asks/${ASK}/decline`, "dispatch decline-ask", GATED_NO_COMMAND],
    // A route a hand-written command sends too: the refusal names that command alone.
    ["POST", "/api/v1/markdown/round-trip", "dispatch check-markdown", `${GATED}dispatch artifact`],
  ])("dispatch api's refusal of %s %s never names %s, which is refused there too", async (method, path, bound, refusal) => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const stateDir = tempDir();
    writeRecord(stateDir, SESSION);
    const ran = await dispatch(
      ["api", "--method", method, "--path", path, "--body-file", bodyFile(COMMENT)],
      sessionEnvironment(broker.url, server.url, stateDir)
    );
    expect(ran.code).toBe(1);
    expect(ran.stdout.trimEnd()).toBe(refusal);
    expect(ran.stdout).not.toContain(bound);
    expect(server.sent).toHaveLength(0);
  });
});

test("dispatch --help lists dispatch api, and dispatch api --help gives its flags", async () => {
  const all = await dispatch(["--help"], {});
  expect(all.code).toBe(0);
  expect(all.stdout).toMatch(
    /\n {2}api +Send one request of a route of Dispatch's HTTP API under \/api\/v1, /
  );
  const api = await dispatch(["api", "--help"], {});
  expect(api.code).toBe(0);
  expect(api.stdout).toStartWith("Usage: dispatch api [flags]\n");
  for (const flag of ["--path", "--method", "--body", "--body-file"]) {
    expect(api.stdout).toContain(`\n  ${flag} `);
  }
  expect(api.stderr).toBe("");
});

/** GitHub's answers to a job that asks for its OIDC token, as a real run captured them. */
interface ActionsFixture {
  readonly audience: string;
  readonly answer: { readonly status: number; readonly body: Readonly<Record<string, string>> };
}

const ACTIONS = JSON.parse(
  readFileSync(
    new URL(
      "../../../envoy/internal/cipherclient/testdata/github-actions-token.json",
      import.meta.url
    ),
    "utf8"
  )
) as ActionsFixture;

/** Where the fixture's answer holds the token GitHub answered. */
// biome-ignore lint/suspicious/noTemplateCurlyInString: the fixture writes its placeholder as `${jwt}`, and this is that file's own text rather than a template this suite writes.
const JWT_PLACEHOLDER = "${jwt}";

/** The request token GitHub gives the job. */
const REQUEST_TOKEN = fixtureSecret("request");

/**
 * GitHub's OIDC token endpoint for one job: it answers the fixture's answer with a new token in
 * its placeholder's place, as GitHub mints one per request, and keeps what it was asked with and
 * each token it answered. The stub broker reads no token, so none is signed here.
 */
function stubGitHub(): {
  readonly requestUrl: string;
  readonly asked: { readonly authorization: string | null; readonly audience: string | null }[];
  readonly answered: string[];
} {
  const asked: { readonly authorization: string | null; readonly audience: string | null }[] = [];
  const answered: string[] = [];
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: (request) => {
      asked.push({
        authorization: request.headers.get("Authorization"),
        audience: new URL(request.url).searchParams.get("audience"),
      });
      const runToken = fixtureSecret("run");
      answered.push(runToken);
      const body = Object.fromEntries(
        Object.entries(ACTIONS.answer.body).map(([member, value]) => [
          member,
          value === JWT_PLACEHOLDER ? runToken : value,
        ])
      );
      return Response.json(body, { status: ACTIONS.answer.status });
    },
  });
  servers.push(server);
  return {
    requestUrl: `http://127.0.0.1:${server.port}/idtoken?api-version=example-api-version`,
    asked,
    answered,
  };
}
