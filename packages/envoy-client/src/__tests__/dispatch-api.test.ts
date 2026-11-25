// packages/envoy-client/src/__tests__/dispatch-api.test.ts
//
// `dispatch api --path <route> [--method POST|PUT|PATCH|DELETE] [--body-file <file>|-]` sends one
// request of Dispatch's HTTP API with the credential every `dispatch` command resolves, and prints
// the answer's body: Dispatch's JSON, or any other body of a 2xx as it came. These rows drive it
// through `runDispatchCli` against a stub secrets broker (`stubBroker`) and a stub Dispatch of
// this file's own, which keeps each request it was sent and answers by path: its own whoami, the
// 401 and the 404 captured from a production Dispatch, a proxy's 502 page, a bodiless 204, a
// redirect, a document version's Markdown download, a picture version's bytes and a write's
// answer in plain text. They hold the paths it refuses before anything is sent, the credential it
// presents and the session its trade names, with a host and without one, and the AskGate rule: in
// a session whose gate is on it sends GET alone, and names the command that makes the write; the
// gate sits in the command itself, under a host and with none.

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
/** A picture version's bytes as Dispatch stores them: a PNG's signature, then two bytes no UTF-8
 *  text holds, which a round trip through text would change. */
const PNG = Uint8Array.of(0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0xff, 0xfe);
/** The version of a document, and of a picture, the stub answers. */
const DOCUMENT_VERSION = "/api/v1/artifacts/01234567-0000-4000-8000-000000000002/versions/3";
const PICTURE_VERSION = "/api/v1/artifacts/01234567-0000-4000-8000-000000000003/versions/1";
/** A write's answer in plain text, as something no Dispatch route answers yet could. */
const ACCEPTED = "accepted";
/** A plain-text answer longer than the most Claude Code shows (`CLAUDE_OUTPUT_MAX`). */
const LONG = "one line of a long answer\n".repeat(1_500);

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
 * JSON or, with `?download=true`, its Markdown (`DOCUMENT_VERSION`), a picture version's bytes
 * (`PICTURE_VERSION`), plain text to any method (`/api/v1/accepted`, `/api/v1/long`), and any
 * other request with the method and path it was sent.
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
            ? new Response(MARKDOWN, {
                headers: { "Content-Type": "text/markdown; charset=utf-8" },
              })
            : Response.json({ number: 3, markdown: MARKDOWN });
        case PICTURE_VERSION:
          return new Response(PNG, { headers: { "Content-Type": "image/png" } });
        case "/api/v1/accepted":
          return new Response(ACCEPTED, { headers: { "Content-Type": "text/plain" } });
        case "/api/v1/long":
          return new Response(LONG, { headers: { "Content-Type": "text/plain" } });
        default:
          return Response.json({ method: request.method, path: url.pathname });
      }
    },
  });
  servers.push(server);
  return { url: `http://127.0.0.1:${server.port}`, sent };
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

/** How the gate refuses a write: before the command it names. */
const GATED = `dispatch api was not called: ${GATED_WHY}; make this write with `;

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
    [
      "a GET's 404",
      ["--path", "/api/v1/no-such-route"],
      "NOT_FOUND: no route for GET /api/v1/no-such-route; GET /api/v1 lists every route\n",
    ],
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

  test("a 502 with no JSON exits 1 naming the status", async () => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const ran = await dispatch(
      ["api", "--path", "/api/v1/proxied"],
      jobEnvironment(broker.url, server.url, tempDir())
    );
    expect(ran.code).toBe(1);
    expect(ran.stdout).toContain("/api/v1/proxied answered 502");
    expect(ran.stdout).toContain("rather than Dispatch's own answer");
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

  test("a bodiless 204 prints an empty answer and exits 0", async () => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const ran = await dispatch(
      ["api", "--method", "DELETE", "--path", "/api/v1/nothing"],
      jobEnvironment(broker.url, server.url, tempDir())
    );
    expect(ran.code, ran.stdout).toBe(0);
    expect(ran.stdout.trim()).toBe("");
    expect(server.sent.map((sent) => sent.method)).toEqual(["DELETE"]);
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

/** The two ways the command runs: as an Oh My Pi session, and in a job no host runs. */
const RUNS = [
  ["with a host", sessionEnvironment],
  ["with no host", jobEnvironment],
] as const;

// Review 6059716082, finding 1: Dispatch answers some routes with a body that is not JSON (a file
// version's bytes under its own MIME type, a document version's Markdown with `?download=true`),
// and the command prints any 2xx's body as it came and exits 0: on standard output the bytes that
// came and nothing else, with no newline added and no round trip through text.
describe("a 2xx answer that is not JSON", () => {
  test.each(
    RUNS
  )("a document version's Markdown download is printed exactly as it came, and it exits 0: %s", async (_, environment) => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const ran = await dispatch(
      ["api", "--path", `${DOCUMENT_VERSION}?download=true`],
      environment(broker.url, server.url, tempDir())
    );
    expect(ran.code, ran.stdout).toBe(0);
    expect(ran.stdout).toBe(MARKDOWN);
    expect(ran.stderr).toBe("");
    expect(server.sent).toHaveLength(1);
  });

  test.each(
    RUNS
  )("a picture version's bytes go to standard output unchanged, and it exits 0: %s", async (_, environment) => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const ran = await dispatch(
      ["api", "--path", PICTURE_VERSION],
      environment(broker.url, server.url, tempDir())
    );
    expect(ran.code, ran.stdout).toBe(0);
    expect(ran.bytes).toEqual(PNG);
    expect(ran.stderr).toBe("");
  });

  test.each(
    RUNS
  )("a POST answered 200 in plain text prints it, and it exits 0: %s", async (_, environment) => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const ran = await dispatch(
      ["api", "--method", "POST", "--path", "/api/v1/accepted", "--body-file", bodyFile(COMMENT)],
      environment(broker.url, server.url, tempDir())
    );
    expect(ran.code, ran.stdout).toBe(0);
    expect(ran.stdout).toBe(ACCEPTED);
    expect(ran.stderr).toBe("");
    expect(server.sent.map((sent) => sent.method)).toEqual(["POST"]);
  });

  test("the same version read without download=true is Dispatch's JSON, printed as every JSON answer is", async () => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const ran = await dispatch(
      ["api", "--path", DOCUMENT_VERSION],
      jobEnvironment(broker.url, server.url, tempDir())
    );
    expect(ran.code, ran.stdout).toBe(0);
    expect(ran.stdout).toBe(`${JSON.stringify({ number: 3, markdown: MARKDOWN }, null, 2)}\n`);
  });

  test("with a host, the ledger records the body's type and size, never its bytes", async () => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const stateDir = tempDir();
    const ran = await dispatch(
      ["api", "--path", PICTURE_VERSION],
      sessionEnvironment(broker.url, server.url, stateDir)
    );
    expect(ran.code, ran.stdout).toBe(0);
    expect(ledgerOf(stateDir, SESSION)).toEqual([
      {
        at: expect.any(String),
        tool: "dispatch_api",
        details: { path: PICTURE_VERSION, content_type: "image/png", bytes: PNG.byteLength },
      },
    ]);
  });

  test("under Claude Code a body longer than the output limit is written whole, and no file holds it", async () => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const stateDir = tempDir();
    const ran = await dispatch(["api", "--path", "/api/v1/long"], {
      ...jobEnvironment(broker.url, server.url, stateDir),
      DISPATCH_HOST: "claude",
      CLAUDE_CODE_SESSION_ID: "claude-ses",
    });
    expect(ran.code, ran.stdout.slice(0, 200)).toBe(0);
    expect(ran.stdout).toBe(LONG);
    expect(ran.stderr).toBe("");
    expect(ledgerOf(stateDir, "claude-ses").map((entry) => entry.details)).toEqual([
      { path: "/api/v1/long", content_type: "text/plain", bytes: LONG.length },
    ]);
    expect(existsSync(join(stateDir, "sessions", "claude-ses", "out"))).toBe(false);
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

  test.each(
    REWRITTEN
  )("a route holding %s is refused naming the path, and in the query it is sent as the URL parser writes it", async (character) => {
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
  });
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

  test.each([
    "DISPATCH_SESSION_ID",
    "ENVOY_SESSION_ID",
    "CLAUDE_CODE_SESSION_ID",
  ])("with no host, a write with a session id sessionDirectory refuses is a usage error naming its variable, and a GET is sent: %s", async (variable) => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const stateDir = tempDir();
    const env = { ...jobEnvironment(broker.url, server.url, stateDir), [variable]: "../elsewhere" };
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
  });

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
  ])("a refusal before any request is recorded for the session the call knows: %s", async (_, argv, named, session) => {
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
  });
});

describe("AskGate", () => {
  test.each([
    ["GET", "/api/v1/whoami", null],
    ["POST", "/api/v1/issues/ACME-1/comments", "dispatch comment"],
    ["PUT", "/api/v1/projects/ACME/settings", "dispatch --help"],
    ["PATCH", "/api/v1/projects/ACME/settings", "dispatch --help"],
    ["DELETE", "/api/v1/projects/ACME/settings", "dispatch --help"],
  ])("in a session AskGate gates, GET is sent and each other method is refused with nothing sent: %s %s", async (method, path, names) => {
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
    if (names === null) {
      expect(ran.code, ran.stdout).toBe(0);
      expect(server.sent).toHaveLength(1);
      return;
    }
    expect(ran.code).toBe(1);
    expect(ran.stdout).toStartWith(GATED);
    expect(ran.stdout).toContain(names);
    expect(server.sent).toHaveLength(0);
    expect(broker.trades).toHaveLength(0);
  });

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

  test.each([
    "DISPATCH_SESSION_ID",
    "ENVOY_SESSION_ID",
    "CLAUDE_CODE_SESSION_ID",
  ])("with no host, a gated %s refuses POST and sends nothing", async (variable) => {
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
  });

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
