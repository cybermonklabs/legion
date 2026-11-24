// packages/envoy-client/src/__tests__/dispatch-api.test.ts
//
// `dispatch api <METHOD> <path> [--body-file <file>|-]` sends one request of Dispatch's HTTP API
// with the credential every `dispatch` command resolves, and prints the answer's body. These rows
// drive it through `runDispatchCli` against a stub secrets broker (`stubBroker`) and a stub
// Dispatch of this file's own, which keeps each request it was sent and answers by path: its own
// whoami, the 401 and the 404 captured from a production Dispatch, a proxy's 502 page and a
// redirect. They hold the paths it refuses before anything is sent, the credential it
// presents and the session its trade names, with a host and without one, and the AskGate rule: in
// a session whose gate is on it sends GET alone, and names the command that makes the write.

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
 * (`/api/v1/proxied`), a JSON 503 that is no refusal of Dispatch's (`/api/v1/foreign`), a 307 to
 * `redirectTo` (`/api/v1/moved`), and any other request with the method and path it was sent.
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
        case "/api/v1/moved":
          return new Response(null, {
            status: 307,
            headers: { Location: `${redirectTo}/api/v1/whoami` },
          });
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
  let stdout = "";
  let stderr = "";
  let stdinReads = 0;
  const code = await runDispatchCli(argv, env, {
    stdout: (text) => {
      stdout += text;
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
  return { code, stdout, stderr, stdinReads };
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

describe("a request", () => {
  test("GET /api/v1/whoami trades a token, sends one request with a new DPoP proof, prints the body and exits 0", async () => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const ran = await dispatch(
      ["api", "GET", "/api/v1/whoami"],
      sessionEnvironment(broker.url, server.url, tempDir())
    );
    expect(ran.code, ran.stderr).toBe(0);
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
      ["api", "GET", "/api/v1/issues?project=ACME&status=open"],
      jobEnvironment(broker.url, server.url, tempDir())
    );
    expect(ran.code, ran.stderr).toBe(0);
    const [sent] = server.sent;
    expect(sent?.url.pathname).toBe("/api/v1/issues");
    expect(sent?.url.search).toBe("?project=ACME&status=open");
    expect(claimsOf(sent?.headers.get("DPoP") ?? null).htu).toBe(`${server.url}/api/v1/issues`);
  });

  test("a POST sends the body of --body-file <file> as application/json", async () => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const ran = await dispatch(
      ["api", "POST", "/api/v1/issues/ACME-1/comments", "--body-file", bodyFile(COMMENT)],
      jobEnvironment(broker.url, server.url, tempDir())
    );
    expect(ran.code, ran.stderr).toBe(0);
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

  test("a POST sends standard input with --body-file -, and reads none without it", async () => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const env = jobEnvironment(broker.url, server.url, tempDir());
    const piped = await dispatch(
      ["api", "POST", "/api/v1/issues/ACME-1/comments", "--body-file", "-"],
      env,
      COMMENT
    );
    expect(piped.code, piped.stderr).toBe(0);
    expect(piped.stdinReads).toBe(1);
    expect(server.sent[0]?.body).toBe(COMMENT);
    expect(server.sent[0]?.headers.get("Content-Type")).toBe("application/json");

    const bare = await dispatch(["api", "POST", "/api/v1/issues/ACME-1/claim"], env, COMMENT);
    expect(bare.code, bare.stderr).toBe(0);
    expect(bare.stdinReads).toBe(0);
    expect(server.sent[1]?.body).toBe("");
  });

  test.each([
    ["a 404", "/api/v1/no-such-route", NO_ROUTE, "404 NOT_FOUND"],
    ["a 401", "/api/v1/signed-out", NO_IDENTITY, "401 NO_IDENTITY"],
  ])("%s exits 1 naming its code, with Dispatch's body on standard output", async (_, path, body, named) => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const ran = await dispatch(
      ["api", "GET", path],
      jobEnvironment(broker.url, server.url, tempDir())
    );
    expect(ran.code).toBe(1);
    expect(JSON.parse(ran.stdout)).toEqual(body);
    expect(ran.stderr).toBe(`dispatch api: GET ${path} answered ${named}\n`);
  });

  test("a 502 with no JSON exits 1 naming the status", async () => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const ran = await dispatch(
      ["api", "GET", "/api/v1/proxied"],
      jobEnvironment(broker.url, server.url, tempDir())
    );
    expect(ran.code).toBe(1);
    expect(ran.stdout).toBe(PROXY_PAGE);
    expect(ran.stderr).toBe("dispatch api: GET /api/v1/proxied answered 502 with no code\n");
  });

  test("a JSON answer with a code but no Dispatch error exits 1 naming no code", async () => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const ran = await dispatch(
      ["api", "GET", "/api/v1/foreign"],
      jobEnvironment(broker.url, server.url, tempDir())
    );
    expect(ran.code).toBe(1);
    expect(JSON.parse(ran.stdout)).toEqual(FOREIGN_JSON);
    expect(ran.stderr).toBe("dispatch api: GET /api/v1/foreign answered 503 with no code\n");
  });

  test("/api/v1 itself is sent", async () => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const ran = await dispatch(
      ["api", "GET", "/api/v1"],
      jobEnvironment(broker.url, server.url, tempDir())
    );
    expect(ran.code, ran.stderr).toBe(0);
    expect(server.sent.map((sent) => sent.url.pathname)).toEqual(["/api/v1"]);
  });

  test("a redirect is not followed", async () => {
    const broker = stubBroker(300);
    const elsewhere = stubDispatch();
    const server = stubDispatch(elsewhere.url);
    const ran = await dispatch(
      ["api", "GET", "/api/v1/moved"],
      jobEnvironment(broker.url, server.url, tempDir())
    );
    expect(ran.code).toBe(1);
    expect(ran.stderr).toBe("dispatch api: GET /api/v1/moved answered 307 with no code\n");
    expect(server.sent).toHaveLength(1);
    expect(elsewhere.sent).toHaveLength(0);
  });

  test("a set DISPATCH_TOKEN is sent in place of a trade", async () => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const bearer = fixtureSecret("dsp");
    const ran = await dispatch(["api", "GET", "/api/v1/whoami"], {
      ...jobEnvironment(broker.url, server.url, tempDir()),
      DISPATCH_TOKEN: bearer,
    });
    expect(ran.code, ran.stderr).toBe(0);
    expect(broker.trades).toHaveLength(0);
    expect(server.sent[0]?.headers.get("Authorization")).toBe(`Bearer ${bearer}`);
    expect(server.sent[0]?.headers.get("DPoP")).toBeNull();
  });
});

describe("the paths it refuses", () => {
  test.each([
    ["an absolute URL", ["GET", "https://other.example/api/v1/whoami"]],
    ["a URL with no scheme", ["GET", "//other.example/api/v1/whoami"]],
    ["a path outside /api/v1", ["GET", "/healthz"]],
    ["a dot segment", ["GET", "/api/v1/../healthz"]],
    ["an encoded dot segment", ["GET", "/api/v1/%2e%2e/healthz"]],
    ["a path that only begins as /api/v1 does", ["GET", "/api/v1x"]],
    ["another version's path", ["GET", "/api/v2/whoami"]],
    ["a path in backslashes", ["GET", "\\\\other.example/api/v1/whoami"]],
    ["a missing method", ["/api/v1/whoami"]],
    ["an unknown method", ["FETCH", "/api/v1/whoami"]],
    ["a body with GET", ["GET", "/api/v1/whoami", "--body-file", "-"]],
  ])("each path outside /api/v1/ of this Dispatch is refused and nothing is sent: %s", async (_, argv) => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const ran = await dispatch(
      ["api", ...argv],
      jobEnvironment(broker.url, server.url, tempDir()),
      COMMENT
    );
    expect(ran.code).toBe(2);
    expect(ran.stderr).toStartWith("dispatch api was not called: ");
    expect(ran.stdout).toBe("");
    expect(ran.stdinReads).toBe(0);
    expect(server.sent).toHaveLength(0);
    expect(broker.trades).toHaveLength(0);
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
    const ran = await dispatch(["api", "GET", "/api/v1/whoami"], {
      ...jobEnvironment(broker.url, server.url, tempDir()),
      ENVOY_CONFIG_FILE: config,
      ...named,
    });
    expect(ran.code, ran.stderr).toBe(0);
    expect(server.sent).toHaveLength(1);
    expect(broker.trades.map((trade) => trade.form.get("session"))).toEqual([session]);
  });

  test("with no DISPATCH_URL it names DISPATCH_URL and the credential variables", async () => {
    const ran = await dispatch(["api", "GET", "/api/v1/whoami"], {
      HOME: tempDir(),
      DISPATCH_STATE_DIR: tempDir(),
    });
    expect(ran.code).toBe(2);
    expect(ran.stderr).toBe(
      "dispatch api was not called: no Dispatch is set; set DISPATCH_URL, and a credential (DISPATCH_TOKEN, DISPATCH_TOKEN_FILE, DISPATCH_BROKER_TOKEN_FILE and DISPATCH_BROKER_DPOP_KEY_FILE, or the broker's AGENT_SECRETS_URL and AGENT_SECRETS_MACHINE_IDENTITY)\n"
    );
  });

  test("a GitHub Actions run with no DISPATCH_HOST writes through dispatch api", async () => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const github = stubGitHub();
    const ran = await dispatch(
      ["api", "POST", "/api/v1/issues/ACME-1/comments", "--body-file", bodyFile(COMMENT)],
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
    expect(ran.code, ran.stderr).toBe(0);
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
    const api = await dispatch(["api", "GET", "/api/v1/whoami"], env);
    const whoami = await dispatch(["whoami"], env);
    expect(whoami.code).toBe(2);
    expect(api.code).toBe(2);
    expect(api.stderr).toBe(whoami.stdout);
    expect(server.sent).toHaveLength(0);
  });

  test.each([
    "DISPATCH_SESSION_ID",
    "ENVOY_SESSION_ID",
    "CLAUDE_CODE_SESSION_ID",
    "CODEX_THREAD_ID",
  ])("a session id sessionDirectory refuses is a usage error naming its variable: %s", async (variable) => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const stateDir = tempDir();
    const ran = await dispatch(["api", "GET", "/api/v1/whoami"], {
      ...jobEnvironment(broker.url, server.url, stateDir),
      [variable]: "../elsewhere",
    });
    expect(ran.code).toBe(2);
    expect(ran.stderr).toStartWith("dispatch api was not called: ");
    expect(ran.stderr).toContain(variable);
    expect(ran.stderr).not.toContain("../elsewhere");
    expect(server.sent).toHaveLength(0);
    // The session the refusal would be recorded for is the id refused, so no ledger is written.
    expect(existsSync(join(stateDir, "sessions"))).toBe(false);
  });
});

describe("the session's ledger", () => {
  test("a gated POST's refusal is recorded in the session's ledger with AskGate's mark, and an answer Dispatch sent is not", async () => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const stateDir = tempDir();
    writeRecord(stateDir, SESSION);
    const env = sessionEnvironment(broker.url, server.url, stateDir);
    const refused = await dispatch(
      ["api", "POST", "/api/v1/issues/ACME-1/comments", "--body-file", bodyFile(COMMENT)],
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
        error: refused.stderr.trimEnd(),
        askgate: { mode: "block", why: GATED_WHY },
      },
    ]);
    expect(cliRefusals(`${logged[0]?.error}\n`)).toHaveLength(1);
    // Requests that were sent, whatever Dispatch answered, add nothing.
    expect((await dispatch(["api", "GET", "/api/v1/no-such-route"], env)).code).toBe(1);
    expect((await dispatch(["api", "GET", "/api/v1/whoami"], env)).code).toBe(0);
    expect(server.sent).toHaveLength(2);
    expect(ledgerOf(stateDir, SESSION)).toHaveLength(1);
  });

  test("a write a broken AskGate record refuses is recorded with AskGate's mark naming the record's problem", async () => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const stateDir = tempDir();
    const file = writeRecord(stateDir, SESSION);
    writeFileSync(file, "{not json");
    const refused = await dispatch(
      ["api", "POST", "/api/v1/issues/ACME-1/claim"],
      sessionEnvironment(broker.url, server.url, stateDir)
    );
    expect(refused.code).toBe(1);
    expect(server.sent).toHaveLength(0);
    expect(ledgerOf(stateDir, SESSION)).toEqual([
      {
        at: expect.any(String),
        tool: "dispatch_api",
        error: refused.stderr.trimEnd(),
        askgate: { mode: "block", why: expect.stringContaining(file) },
      },
    ]);
  });

  test.each([
    [
      "a path refused with no host",
      ["GET", "/api/v1/../healthz"],
      { DISPATCH_SESSION_ID: "job-run-1" },
      "job-run-1",
    ],
    [
      "an unknown method under Claude Code",
      ["FETCH", "/api/v1/whoami"],
      {
        DISPATCH_HOST: "claude",
        CLAUDE_CODE_SESSION_ID: "claude-ses",
        DISPATCH_SESSION_ID: "other",
      },
      "claude-ses",
    ],
    [
      "a path refused under Codex",
      ["GET", "/healthz"],
      { CODEX_THREAD_ID: "codex-thread", DISPATCH_SESSION_ID: "other" },
      "codex-thread",
    ],
    [
      "no Dispatch with no host",
      ["GET", "/api/v1/whoami"],
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
      { at: expect.any(String), tool: "dispatch_api", error: ran.stderr.trimEnd() },
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
      ["api", method, path, ...(method === "GET" ? [] : ["--body-file", bodyFile(COMMENT)])],
      sessionEnvironment(broker.url, server.url, stateDir)
    );
    if (names === null) {
      expect(ran.code, ran.stderr).toBe(0);
      expect(server.sent).toHaveLength(1);
      return;
    }
    expect(ran.code).toBe(1);
    expect(ran.stderr).toStartWith(GATED);
    expect(ran.stderr).toContain(names);
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
        "POST",
        "/api/v1/artifacts/01234567-0000-4000-8000-000000000001/comments",
        "--body-file",
        bodyFile(COMMENT),
      ],
      sessionEnvironment(broker.url, server.url, stateDir)
    );
    expect(ran.code).toBe(1);
    expect(ran.stderr).toStartWith(GATED);
    expect(ran.stderr).toContain("dispatch comment");
    expect(ran.stderr).toContain("dispatch suggest");
    expect(server.sent).toHaveLength(0);
  });

  test("a broken AskGate record refuses each method but GET", async () => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const stateDir = tempDir();
    const file = writeRecord(stateDir, SESSION);
    writeFileSync(file, "{not json");
    const env = sessionEnvironment(broker.url, server.url, stateDir);
    const get = await dispatch(["api", "GET", "/api/v1/whoami"], env);
    expect(get.code, get.stderr).toBe(0);
    expect(server.sent).toHaveLength(1);
    for (const method of ["POST", "PUT", "PATCH", "DELETE"]) {
      const ran = await dispatch(["api", method, "/api/v1/issues/ACME-1/claim"], env);
      expect({ method, code: ran.code }).toEqual({ method, code: 1 });
      expect(ran.stderr).toStartWith("dispatch api was not called: ");
      expect(ran.stderr).toContain(file);
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
      const ran = await dispatch(["api", method, "/api/v1/issues/ACME-1/claim"], env);
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
      ["api", "POST", "/api/v1/issues/ACME-1/comments", "--body-file", bodyFile(COMMENT)],
      { ...jobEnvironment(broker.url, server.url, stateDir), [variable]: SESSION }
    );
    expect(ran.code).toBe(1);
    expect(ran.stderr).toStartWith(GATED);
    expect(server.sent).toHaveLength(0);
    expect(broker.trades).toHaveLength(0);
  });

  test("with no host and no record, POST is sent", async () => {
    const broker = stubBroker(300);
    const server = stubDispatch();
    const ran = await dispatch(
      ["api", "POST", "/api/v1/issues/ACME-1/comments", "--body-file", bodyFile(COMMENT)],
      { ...jobEnvironment(broker.url, server.url, tempDir()), DISPATCH_SESSION_ID: SESSION }
    );
    expect(ran.code, ran.stderr).toBe(0);
    expect(server.sent.map((sent) => sent.method)).toEqual(["POST"]);
  });
});

test("dispatch --help names dispatch api, and dispatch api --help gives its usage", async () => {
  const all = await dispatch(["--help"], {});
  expect(all.code).toBe(0);
  expect(all.stdout).toContain("\ndispatch api <METHOD> <path>");
  expect(all.stdout).toContain("(dispatch api --help)");
  const api = await dispatch(["api", "--help"], {});
  expect(api.code).toBe(0);
  expect(api.stdout).toStartWith("Usage: dispatch api <METHOD> <path> [--body-file <file>|-]\n");
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
