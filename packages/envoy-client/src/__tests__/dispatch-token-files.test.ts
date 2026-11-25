// packages/envoy-client/src/__tests__/dispatch-token-files.test.ts
//
// The `dispatch` command's token files: DISPATCH_BROKER_TOKEN_FILE names a broker access token
// traded elsewhere, and DISPATCH_BROKER_DPOP_KEY_FILE the P-256 key it is bound to. The command
// reads both again before each request and sends the token to DISPATCH_URL alone.
//
// Every command here runs with only the settings it needs beside DISPATCH_URL and the two files:
// DISPATCH_HOST, DISPATCH_SESSION_ID, an ENVOY_CONFIG_FILE naming an envoy.json that holds no
// dispatch.token, and DISPATCH_STATE_DIR, which keeps the session's state in this test's own
// directory rather than the user's.

import { beforeEach, describe, expect, test } from "bun:test";
import {
  createHash,
  createPublicKey,
  generateKeyPairSync,
  type KeyObject,
  verify,
  type webcrypto,
} from "node:crypto";
import { existsSync, mkdirSync, readFileSync, renameSync, rmSync, writeFileSync } from "node:fs";
import * as path from "node:path";
import { stubDeviceLoginBroker } from "@legion/cipher-client/test/device-logins";
import { DISPATCH_READ_ONLY_TOKEN } from "@legion/contracts";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { type FakeDispatchCall, startFakeDispatch } from "../../test/fake-dispatch";
import { runDispatchCli } from "../dispatch-cli";
import { activeDispatchSettings } from "../dispatch-config";
import { resetAdviceMemory } from "../dispatch-execute";
import { DispatchClient } from "../dispatch-http";
import { sessionDispatchSettings, sessionSettings } from "../session-settings";
import { brokerStandIns } from "./broker-stand-ins";

const { servers, tempDir, stubBroker } = brokerStandIns("dispatch-token-files");

const TOKEN_FILE = "DISPATCH_BROKER_TOKEN_FILE";
const KEY_FILE = "DISPATCH_BROKER_DPOP_KEY_FILE";

beforeEach(() => {
  // Each command is its own process in production: nothing the library remembers carries over.
  resetAdviceMemory();
});

/** Dispatch's refusal of a write under a read-only token, worded as its gate words it. */
const READ_ONLY_REFUSAL = {
  code: DISPATCH_READ_ONLY_TOKEN,
  error:
    "this request's credential is a read-only token, which may call only the GET routes Dispatch classes as reads: nothing ran",
};

/** One request a stand-in was sent: its method and URL, and the credential it carried. */
interface Presented {
  readonly method: string;
  readonly url: URL;
  readonly authorization: string | null;
  readonly dpop: string | null;
}

function presentedOf(call: Pick<FakeDispatchCall, "method" | "url" | "headers">): Presented {
  return {
    method: call.method,
    url: call.url,
    authorization: call.headers.get("Authorization"),
    dpop: call.headers.get("DPoP"),
  };
}

const issue = {
  key: "DSP-1",
  title: "Token files",
  status: "in_progress",
  priority: null,
  assignee: null,
  claim: null,
  components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
  route: null,
  parent: null,
  open_asks: [],
  last_seq: 0,
  external_links: [],
  labels: [],
  artifacts: [],
  advances: [],
  advances_revision: 0,
  repo: null,
  resolved_repo: null,
};

/** What the stand-in Dispatch answers each read it serves, by pathname: the project list and
 *  what `dispatch read --issue DSP-1` asks for. */
const READS: Readonly<Record<string, unknown>> = {
  "/api/v1/projects": [
    { key: "CORE", name: "Core", open_asks: 0, created_at: "2026-09-01T00:00:00Z" },
  ],
  "/api/v1/issues/DSP-1": issue,
  "/api/v1/issues/DSP-1/events": [],
  "/api/v1/issues/DSP-1/references": { members: [], truncated: false },
  "/api/v1/issues/DSP-1/coverage": {
    issue: "DSP-1",
    spec_artifact_id: null,
    has_plan: false,
    requirements: [],
    gaps: [],
  },
  "/api/v1/references": { node: { kind: "issue", id: "DSP-1" }, edges: [] },
};

/** How the stand-in Dispatch answers: its reads, a posted message, and the read-only refusal for
 *  a comment. Anything else is a 404 that is no "no route" answer, which a client would ask again
 *  as a rolling release asks. */
function dispatchAnswer(call: FakeDispatchCall): Response {
  const { pathname } = call.url;
  const read = READS[pathname];
  if (call.method === "GET" && read !== undefined) return Response.json(read);
  if (call.method === "POST" && pathname === "/api/v1/issues/DSP-1/comments") {
    return Response.json(READ_ONLY_REFUSAL, { status: 403 });
  }
  if (call.method === "POST" && pathname === "/api/v1/issues/DSP-1/messages") {
    return Response.json({
      id: "message-1",
      issue_key: "DSP-1",
      advice: { issue_status: "in_progress", session_writes_since_human: 0, your_open_asks: [] },
    });
  }
  return Response.json(
    { code: "NOT_FOUND", error: `the stand-in serves no ${call.method} ${pathname}` },
    { status: 404 }
  );
}

/** The stand-in Dispatch, which records each request's `Authorization` and `DPoP`. */
function standInDispatch(): { readonly url: string; readonly presented: Presented[] } {
  const presented: Presented[] = [];
  const server = startFakeDispatch({
    handler: (call) => {
      presented.push(presentedOf(call));
      return dispatchAnswer(call);
    },
  });
  servers.push(server);
  return { url: server.url, presented };
}

/** The stand-in Envoy listener, which records each request's `Authorization` and `DPoP` and
 *  answers that the session holds no subscription. */
function standInListener(): { readonly url: string; readonly presented: Presented[] } {
  const presented: Presented[] = [];
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: (request) => {
      presented.push(
        presentedOf({ method: request.method, url: new URL(request.url), headers: request.headers })
      );
      return Response.json({ session_id: "ses-1", machine_id: "m", dir: "/", topics: [] });
    },
  });
  servers.push(server);
  return { url: `http://127.0.0.1:${server.port}`, presented };
}

function newKey(): KeyObject {
  return generateKeyPairSync("ec", { namedCurve: "P-256" }).privateKey;
}

function pkcs8Of(key: KeyObject): string {
  return key.export({ type: "pkcs8", format: "pem" }).toString();
}

function publicJwkOf(key: KeyObject): webcrypto.JsonWebKey {
  const { kty, crv, x, y } = createPublicKey(key).export({ format: "jwk" });
  return { kty, crv, x, y };
}

/** The two files of one copy, and what they hold. */
interface Copy {
  readonly dir: string;
  readonly tokenFile: string;
  readonly keyFile: string;
  readonly token: string;
  readonly key: KeyObject;
}

function copyFiles(): Copy {
  const dir = tempDir();
  const token = fixtureSecret("brk");
  const key = newKey();
  const tokenFile = path.join(dir, "token");
  const keyFile = path.join(dir, "key.pem");
  writeFileSync(tokenFile, `${token}\n`);
  writeFileSync(keyFile, pkcs8Of(key));
  return { dir, tokenFile, keyFile, token, key };
}

/** Replaces `file` as a driver does: a new file beside it, renamed over it. */
function replaceByRename(file: string, contents: string): void {
  const next = `${file}.next`;
  writeFileSync(next, contents);
  renameSync(next, file);
}

/** The environment of a command whose only Dispatch settings are DISPATCH_URL and `copy`'s two
 *  files, with `extra` over it (an undefined value unsets a variable). */
function commandEnvironment(
  copy: Copy,
  dispatchUrl: string,
  extra: Record<string, string | undefined> = {}
): Record<string, string | undefined> {
  const envoyJson = path.join(copy.dir, "envoy.json");
  if (!existsSync(envoyJson)) writeFileSync(envoyJson, "{}\n");
  return {
    DISPATCH_HOST: "omp",
    DISPATCH_SESSION_ID: "ses-1",
    ENVOY_CONFIG_FILE: envoyJson,
    DISPATCH_STATE_DIR: path.join(copy.dir, "state"),
    DISPATCH_URL: dispatchUrl,
    [TOKEN_FILE]: copy.tokenFile,
    [KEY_FILE]: copy.keyFile,
    ...extra,
  };
}

/** Runs `dispatch <argv>` in this process, with `fetchImpl` as its fetch where one is given. */
async function run(
  argv: readonly string[],
  env: Record<string, string | undefined>,
  cwd: string,
  fetchImpl?: typeof fetch
): Promise<{ readonly code: number; readonly stdout: string; readonly stderr: string }> {
  let stdout = "";
  let stderr = "";
  const code = await runDispatchCli(argv, env, {
    stdout: (text) => {
      stdout += text;
    },
    stderr: (text) => {
      stderr += text;
    },
    readText: (file) => readFileSync(file, "utf8"),
    cwd,
    ...(fetchImpl === undefined ? {} : { fetchImpl }),
  });
  return { code, stdout, stderr };
}

/** A DPoP proof's public key and claims, and whether that key signed it. */
interface Proof {
  readonly jwk: webcrypto.JsonWebKey;
  readonly claims: Readonly<Record<string, unknown>>;
  readonly signed: boolean;
}

function proofOf(dpop: string | null | undefined): Proof {
  const [header = "", claims = "", signature = ""] = (dpop ?? "").split(".");
  const read = (part: string) => JSON.parse(Buffer.from(part, "base64url").toString("utf8"));
  const { jwk } = read(header) as { jwk: webcrypto.JsonWebKey };
  const signed = verify(
    "sha256",
    Buffer.from(`${header}.${claims}`),
    { key: createPublicKey({ key: jwk, format: "jwk" }), dsaEncoding: "ieee-p1363" },
    Buffer.from(signature, "base64url")
  );
  return { jwk, claims: read(claims), signed };
}

/** `presented` carried `token` as `DPoP <token>` and a proof made with `key`, naming that token
 *  and the request's own method and URL on `dispatchUrl`. */
function expectFileCredential(
  presented: Presented,
  token: string,
  key: KeyObject,
  dispatchUrl: string
): void {
  const where = `${presented.method} ${presented.url.pathname}`;
  expect([where, presented.authorization]).toEqual([where, `DPoP ${token}`]);
  const proof = proofOf(presented.dpop);
  expect([where, proof.signed]).toEqual([where, true]);
  expect([where, proof.jwk]).toEqual([where, publicJwkOf(key)]);
  expect([where, proof.claims]).toMatchObject([
    where,
    {
      ath: createHash("sha256").update(token).digest("base64url"),
      htm: presented.method,
      htu: `${dispatchUrl}${presented.url.pathname}`,
    },
  ]);
}

/** Polls `done` until it holds, failing after five seconds. What it waits on is a file another
 *  process writes, which sends this one no event to await, so it is read every 5 ms. */
async function until(done: () => boolean): Promise<void> {
  const deadline = Date.now() + 5_000;
  while (!done()) {
    if (Date.now() > deadline) throw new Error("timed out waiting");
    await Bun.sleep(5);
  }
}

/** The first run of eight characters of `bytes` that `text` holds, or undefined. */
function quotedRun(text: string, bytes: string): string | undefined {
  for (let at = 0; at + 8 <= bytes.length; at += 1) {
    const run = bytes.slice(at, at + 8);
    if (text.includes(run)) return run;
  }
  return undefined;
}

/** What one inventory row changes about a command that would otherwise start. */
interface RowChange {
  readonly env?: Record<string, string | undefined>;
  readonly cwd?: string;
  /** Holds after the command when nothing the row wrote was run. */
  readonly ranNothing?: () => boolean;
}

/** What a row is given: the copy and the stand-ins the command would reach. */
interface RowScene {
  readonly copy: Copy;
  readonly brokerUrl: string;
}

/** The inventory: each setting the token files refuse beside them, each of the two set alone, and
 *  DISPATCH_URL unset, with the words the refusal names it in. */
const REFUSED: readonly {
  readonly row: string;
  readonly named: string;
  readonly change: (scene: RowScene) => RowChange;
}[] = [
  {
    row: "DISPATCH_TOKEN_BROKER",
    named: "DISPATCH_TOKEN_BROKER is set",
    change: () => ({ env: { DISPATCH_TOKEN_BROKER: "1" } }),
  },
  {
    row: "DISPATCH_TOKEN_BROKER_READ",
    named: "DISPATCH_TOKEN_BROKER_READ is set",
    change: () => ({ env: { DISPATCH_TOKEN_BROKER_READ: "1" } }),
  },
  {
    row: "DISPATCH_TOKEN_BROKER_AS_OF",
    named: "DISPATCH_TOKEN_BROKER_AS_OF is set",
    change: () => ({ env: { DISPATCH_TOKEN_BROKER_AS_OF: "2026-11-19T00:00:00Z" } }),
  },
  {
    row: "DISPATCH_TOKEN",
    named: "DISPATCH_TOKEN is set",
    change: () => ({ env: { DISPATCH_TOKEN: fixtureSecret("dsp") } }),
  },
  {
    row: "DISPATCH_TOKEN_FILE",
    named: "DISPATCH_TOKEN_FILE is set",
    change: ({ copy }) => {
      const bearer = path.join(copy.dir, "dispatch-token");
      writeFileSync(bearer, `${fixtureSecret("dsp")}\n`);
      return { env: { DISPATCH_TOKEN_FILE: bearer } };
    },
  },
  {
    row: "AGENT_SECRETS_URL",
    named: "AGENT_SECRETS_URL is set",
    change: ({ brokerUrl }) => ({ env: { AGENT_SECRETS_URL: brokerUrl } }),
  },
  {
    row: "AGENT_SECRETS_MACHINE_IDENTITY",
    named: "AGENT_SECRETS_MACHINE_IDENTITY is set",
    change: () => ({ env: { AGENT_SECRETS_MACHINE_IDENTITY: "kubernetes" } }),
  },
  {
    row: "the chosen envoy.json's dispatch.token",
    named: "dispatch.token is set in <chosen>",
    change: ({ copy }) => {
      // A command, which the check reads by the field's presence and never runs.
      const marker = path.join(copy.dir, "command-ran");
      const chosen = path.join(copy.dir, "chosen.json");
      writeFileSync(chosen, JSON.stringify({ dispatch: { token: `!touch ${marker}` } }));
      return { env: { ENVOY_CONFIG_FILE: chosen }, ranNothing: () => !existsSync(marker) };
    },
  },
  {
    row: "the checkout's .envoy/envoy.json's dispatch.token",
    named: "dispatch.token is set in <checkout>",
    change: ({ copy }) => {
      const checkout = path.join(copy.dir, "checkout");
      mkdirSync(path.join(checkout, ".envoy"), { recursive: true });
      writeFileSync(
        path.join(checkout, ".envoy", "envoy.json"),
        JSON.stringify({ dispatch: { token: fixtureSecret("chk") } })
      );
      return { cwd: checkout };
    },
  },
  {
    row: "DISPATCH_BROKER_TOKEN_FILE alone",
    named: `${KEY_FILE} is not set`,
    change: () => ({ env: { [KEY_FILE]: undefined } }),
  },
  {
    row: "DISPATCH_BROKER_DPOP_KEY_FILE alone",
    named: `${TOKEN_FILE} is not set`,
    change: () => ({ env: { [TOKEN_FILE]: undefined } }),
  },
  {
    row: "DISPATCH_URL unset",
    named: "DISPATCH_URL is not set",
    change: () => ({ env: { DISPATCH_URL: undefined } }),
  },
];

/** `named` as the refusal writes it for `copy`, its files' paths filled in. */
function wordsOf(named: string, copy: Copy, cwd: string): string {
  return named
    .replace("<chosen>", path.join(copy.dir, "chosen.json"))
    .replace("<checkout>", path.join(cwd, ".envoy", "envoy.json"));
}

/** The public origin the rows of DISPATCH_PUBLIC_URL beside the token files set. */
const PUBLIC_ORIGIN = "https://dispatch.example.org";

/** A DISPATCH_URL whose host is a Kubernetes Service name, as a pod reaches Dispatch. */
const SERVICE_URL = "http://dispatch.dispatch.svc.cluster.local:8766";

/** One request the command asked its fetch for: the URL it named and the proof it carried. */
interface Asked {
  readonly method: string;
  readonly url: URL;
  readonly dpop: string | null;
}

/**
 * A fetch for the command that records what it asked for, then forwards each request to the
 * stand-in at `standIn` as a proxy serving Dispatch at `dispatchUrl` would: the path below
 * `mount`, `dispatchUrl`'s own path ("" for an origin), and the query, on the stand-in's origin.
 * No request leaves the process, so a host `dispatchUrl` names need not resolve.
 */
function forwardingFetch(
  dispatchUrl: string,
  standIn: string
): { readonly fetchImpl: typeof fetch; readonly asked: Asked[]; readonly mount: string } {
  const asked: Asked[] = [];
  const mount = new URL(dispatchUrl).pathname.replace(/\/+$/, "");
  const fetchImpl = (async (input: string | URL | Request, init?: RequestInit) => {
    const url = new URL(String(input));
    asked.push({
      method: init?.method ?? "GET",
      url,
      dpop: new Headers(init?.headers).get("DPoP"),
    });
    const forwarded = new URL(standIn);
    forwarded.pathname = url.pathname.startsWith(mount)
      ? url.pathname.slice(mount.length)
      : url.pathname;
    forwarded.search = url.search;
    return await fetch(forwarded, init);
  }) as typeof fetch;
  return { fetchImpl, asked, mount };
}

/** `asked` as a row names it: its method, path and query. */
function shownOf({ method, url }: Asked): string {
  return `${method} ${url.pathname}${url.search}`;
}

/** What a row of DISPATCH_PUBLIC_URL beside the token files must hold. */
type PublicUrlHolds =
  /** The command starts: each request goes to DISPATCH_URL's origin, and its proof names
   *  `proofOrigin` followed by the request's own path, never its query. `write` runs a write
   *  after the read, `query` reads the requests that carry a query alone, and `unquoted` is a
   *  word no line the command prints holds. */
  | {
      readonly kind: "starts";
      readonly proofOrigin: string;
      readonly write?: true;
      readonly query?: true;
      readonly unquoted?: string;
    }
  /** The command exits 2 before any request, its refusal holding each of `named`. */
  | { readonly kind: "refused"; readonly named: readonly string[] }
  /** The credential the command would use refuses a request to each of `refused`. */
  | { readonly kind: "credential"; readonly refused: readonly string[] };

/** One row of DISPATCH_PUBLIC_URL beside the token files. In a URL, `<stand-in>` is the stand-in
 *  Dispatch's origin and `<port>` its port. */
interface PublicUrlRow {
  readonly row: string;
  readonly dispatchUrl: string;
  /** Unset where undefined. */
  readonly publicUrl: string | undefined;
  readonly holds: PublicUrlHolds;
}

/** The inventory of DISPATCH_PUBLIC_URL beside the token files: a DISPATCH_URL on its origin and
 *  one whose host is a Service name start, the second's proofs naming the public origin; every
 *  other DISPATCH_URL, and a DISPATCH_PUBLIC_URL that is no http(s) URL, is refused; and the
 *  credential sends the token to DISPATCH_URL's origin alone. */
const PUBLIC_URL_ROWS: readonly PublicUrlRow[] = [
  {
    row: "P1",
    dispatchUrl: "<stand-in>",
    publicUrl: undefined,
    holds: { kind: "starts", proofOrigin: "<stand-in>" },
  },
  {
    row: "P2",
    dispatchUrl: "<stand-in>",
    publicUrl: "<stand-in>",
    holds: { kind: "starts", proofOrigin: "<stand-in>" },
  },
  {
    row: "P3",
    dispatchUrl: SERVICE_URL,
    publicUrl: PUBLIC_ORIGIN,
    holds: { kind: "starts", proofOrigin: PUBLIC_ORIGIN, write: true },
  },
  {
    row: "P4",
    dispatchUrl: SERVICE_URL,
    publicUrl: PUBLIC_ORIGIN,
    holds: { kind: "starts", proofOrigin: PUBLIC_ORIGIN, query: true },
  },
  {
    row: "P5",
    dispatchUrl: `${SERVICE_URL}/prefix`,
    publicUrl: PUBLIC_ORIGIN,
    holds: { kind: "starts", proofOrigin: PUBLIC_ORIGIN },
  },
  {
    row: "P6",
    dispatchUrl: SERVICE_URL,
    publicUrl: "https://reader@dispatch.example.org:8443/base?view=mine#top",
    holds: { kind: "starts", proofOrigin: "https://dispatch.example.org:8443", unquoted: "reader" },
  },
  {
    row: "P7",
    dispatchUrl: "http://DISPATCH.Dispatch.SVC.cluster.local:8766",
    publicUrl: PUBLIC_ORIGIN,
    holds: { kind: "starts", proofOrigin: PUBLIC_ORIGIN },
  },
  ...(
    [
      ["R1", "https://dispatch.example.net"],
      ["R2", "http://127.0.0.1:<port>"],
      ["R3", "http://svc.cluster.local:8766"],
      ["R4", "http://dispatch.svc.cluster.local.evil.example"],
      ["R5", "http://evil-svc.cluster.local"],
      ["R6", "http://dispatch.dispatch.svc.cluster.local.:8766"],
      ["R7", "http://a.svc.cluster.local@evil.example"],
      // The public host with another scheme, then with another port.
      ["R8", "http://dispatch.example.org"],
      ["R9", "https://dispatch.example.org:9443"],
    ] as const
  ).map(
    ([row, dispatchUrl]): PublicUrlRow => ({
      row,
      dispatchUrl,
      publicUrl: PUBLIC_ORIGIN,
      holds: { kind: "refused", named: ["DISPATCH_URL", "DISPATCH_PUBLIC_URL"] },
    })
  ),
  ...["dispatch.example.org", "ftp://dispatch.example.org", "https://dispatch.example.org:0"].map(
    (publicUrl): PublicUrlRow => ({
      row: `R10 ${publicUrl}`,
      dispatchUrl: SERVICE_URL,
      publicUrl,
      holds: { kind: "refused", named: ["DISPATCH_PUBLIC_URL must be an http or https URL"] },
    })
  ),
  {
    row: "T1",
    dispatchUrl: SERVICE_URL,
    publicUrl: PUBLIC_ORIGIN,
    holds: {
      kind: "credential",
      refused: [`${PUBLIC_ORIGIN}/api/v1/projects`, "<stand-in>/api/v1/projects"],
    },
  },
];

describe("the dispatch command's token files", () => {
  test("the token files refuse every setting beside them", async () => {
    // The broker's address is a name only: a trade needs AGENT_SECRETS_URL and an identity
    // beside DISPATCH_URL, and no command here sets all three ("the stand-in broker gets no
    // request" does).
    const brokerUrl = "http://broker.test";
    // One stand-in Dispatch and listener for every row: a row sends nothing when neither hears
    // more than it had heard before the row ran.
    const dispatch = standInDispatch();
    const listener = standInListener();
    // Each row's problems, so a failure names every row that fails rather than the first.
    const problems: string[] = [];
    for (const { row, named, change } of REFUSED) {
      const copy = copyFiles();
      const heard = dispatch.presented.length + listener.presented.length;
      const changed = change({ copy, brokerUrl });
      const cwd = changed.cwd ?? copy.dir;
      const env = commandEnvironment(copy, dispatch.url, {
        ENVOY_URL: listener.url,
        ...changed.env,
      });
      const { code, stdout } = await run(["projects"], env, cwd);
      const sent = dispatch.presented.length + listener.presented.length - heard;
      const wrong = [
        code === 2 ? [] : [`exits ${code}`],
        // The refusal is the token files', and names this row's setting and no other's.
        stdout.includes(TOKEN_FILE) && stdout.includes(KEY_FILE) ? [] : ["names no token file"],
        stdout.includes(wordsOf(named, copy, cwd)) ? [] : [`does not say "${named}"`],
        REFUSED.filter(
          (other) => other.named !== named && stdout.includes(wordsOf(other.named, copy, cwd))
        ).map((other) => `also says "${other.named}"`),
        sent === 0 ? [] : [`sends ${sent} requests`],
        changed.ranNothing?.() === false ? ["runs the envoy.json's command"] : [],
      ].flat();
      if (wrong.length > 0) problems.push(`${row}: ${wrong.join("; ")}`);
    }
    expect(problems).toEqual([]);

    // Every setting at once (each of the two files alone excludes the other): each is named. With
    // DISPATCH_URL unset among them nothing can reach the stand-in Dispatch, so the rows above,
    // each refused with DISPATCH_URL set but for its own, are what hold the command to sending
    // nothing.
    const copy = copyFiles();
    const together = REFUSED.filter(({ row }) => row !== "DISPATCH_BROKER_DPOP_KEY_FILE alone");
    const changes = together.map(({ change }) => change({ copy, brokerUrl }));
    const cwd = changes.find((changed) => changed.cwd !== undefined)?.cwd ?? copy.dir;
    const env = commandEnvironment(
      copy,
      dispatch.url,
      Object.assign({}, ...changes.map((changed) => changed.env ?? {}))
    );
    const { code, stdout } = await run(["projects"], env, cwd);
    expect(code).toBe(2);
    for (const { named } of together) expect(stdout).toContain(wordsOf(named, copy, cwd));

    // A checkout's file that cannot be read is refused too: whether it sets dispatch.token
    // cannot be told.
    const broken = path.join(copy.dir, "broken-checkout");
    mkdirSync(path.join(broken, ".envoy"), { recursive: true });
    writeFileSync(path.join(broken, ".envoy", "envoy.json"), "{ not json");
    const unread = await run(["projects"], commandEnvironment(copy, dispatch.url), broken);
    expect(unread.code).toBe(2);
    expect(unread.stdout).toContain(`${path.join(broken, ".envoy", "envoy.json")}: invalid JSON`);
    expect(unread.stdout).toContain("so whether it sets dispatch.token cannot be told");
    expect(dispatch.presented).toEqual([]);
  });

  test("a read and a write with only DISPATCH_URL and the two files send the file's token and a proof made with the file's key", async () => {
    const dispatch = standInDispatch();
    const copy = copyFiles();
    const env = commandEnvironment(copy, dispatch.url);

    const read = await run(["read", "--issue", "DSP-1"], env, copy.dir);
    expect(read.code, read.stdout).toBe(0);
    expect(read.stdout).toContain("Token files");
    const reads = dispatch.presented.splice(0);
    expect(reads.map(({ method, url }) => `${method} ${url.pathname}`)).toContain(
      "GET /api/v1/issues/DSP-1"
    );
    for (const presented of reads)
      expectFileCredential(presented, copy.token, copy.key, dispatch.url);

    const write = await run(["comment", "--issue", "DSP-1", "--body", "Read only."], env, copy.dir);
    expect(write.stdout).toContain("READ_ONLY_TOKEN:");
    expect(write.code).toBe(1);
    const writes = dispatch.presented.splice(0);
    expect(writes.map(({ method, url }) => `${method} ${url.pathname}`)).toContain(
      "POST /api/v1/issues/DSP-1/comments"
    );
    for (const presented of writes) {
      expectFileCredential(presented, copy.token, copy.key, dispatch.url);
    }

    // The token goes to DISPATCH_URL's origin alone, whatever URL a caller names.
    const config = activeDispatchSettings(
      (
        await sessionDispatchSettings({ path: env.ENVOY_CONFIG_FILE ?? "", error: null }, env, {
          cwd: copy.dir,
          tokenFiles: true,
        })
      ).dispatch
    );
    if (config === null) throw new Error("Dispatch is off");
    const elsewhere = `http://localhost:${new URL(dispatch.url).port}/api/v1/projects`;
    const refusal = await Promise.resolve()
      .then(() => config.credential.headers("GET", elsewhere))
      .then(
        () => "",
        (error: unknown) => String(error)
      );
    expect(refusal).toContain("DISPATCH_URL");
    expect(refusal).not.toContain(copy.token);
  });

  test("a write whose result names an owner sends the listener nothing of the file's token", async () => {
    const dispatch = standInDispatch();
    const listener = standInListener();
    const copy = copyFiles();
    const env = commandEnvironment(copy, dispatch.url, { ENVOY_URL: listener.url });

    const posted = await run(["message", "--issue", "DSP-1", "--body", "hello"], env, copy.dir);
    expect(posted.code, posted.stdout).toBe(0);
    // The result names DSP-1 as an issue the session is not subscribed to: the listener was read.
    expect(posted.stdout).toContain("not subscribed to DSP-1");
    expect(listener.presented.map(({ url }) => url.pathname)).toContain("/v1/interests/ses-1");
    for (const { authorization, dpop } of listener.presented) {
      expect([authorization, dpop]).toEqual([null, null]);
    }
    for (const presented of dispatch.presented) {
      expectFileCredential(presented, copy.token, copy.key, dispatch.url);
    }
  });

  test("the stand-in broker gets no request", async () => {
    const broker = stubBroker(900);
    const dispatch = standInDispatch();
    const listener = standInListener();
    const copy = copyFiles();
    const projected = path.join(copy.dir, "projected");
    writeFileSync(projected, `${fixtureSecret("proj")}\n`);

    // The case where a trade could happen: AGENT_SECRETS_URL and an identity the broker takes,
    // a kubernetes identity with the projected token it presents, beside DISPATCH_URL. Its
    // broker check is what holds a Dispatch call beside the token files to trading nothing,
    // so it comes first: it fails if the files stop refusing.
    const refused = await run(
      ["projects"],
      commandEnvironment(copy, dispatch.url, {
        ENVOY_URL: listener.url,
        AGENT_SECRETS_URL: broker.url,
        AGENT_SECRETS_MACHINE_IDENTITY: "kubernetes",
        AGENT_SECRETS_MACHINE_TOKEN_FILE: projected,
      }),
      copy.dir
    );
    expect([broker.trades, dispatch.presented, listener.presented]).toEqual([[], [], []]);
    expect(refused.code).toBe(2);
    expect(refused.stdout).toContain("AGENT_SECRETS_URL is set");
    expect(refused.stdout).toContain("AGENT_SECRETS_MACHINE_IDENTITY is set");

    const env = commandEnvironment(copy, dispatch.url, { ENVOY_URL: listener.url });
    expect((await run(["projects"], env, copy.dir)).code).toBe(0);
    expect((await run(["message", "--issue", "DSP-1", "--body", "hi"], env, copy.dir)).code).toBe(
      0
    );
    expect((await run(["comment", "--issue", "DSP-1", "--body", "x"], env, copy.dir)).code).toBe(1);
    expect(dispatch.presented.length).toBeGreaterThan(2);
    expect(listener.presented.length).toBeGreaterThan(0);
    expect(broker.trades).toEqual([]);
  });

  test("`dispatch login` refuses beside the token files", async () => {
    // A broker that approves a device login at its first poll: beside the files the command asks
    // it nothing, so no device grant starts and no login is kept on the copy.
    const broker = stubDeviceLoginBroker();
    servers.push(broker);
    const copy = copyFiles();
    const loginFile = path.join(tempDir(), "legion", "device-login.json");
    const env = commandEnvironment(copy, "http://dispatch.test", {
      HOME: tempDir(),
      AGENT_SECRETS_URL: broker.url,
      AGENT_SECRETS_MACHINE_IDENTITY: "device-login",
      AGENT_SECRETS_DEVICE_LOGIN_FILE: loginFile,
    });
    const beside = ["AGENT_SECRETS_URL is set", "AGENT_SECRETS_MACHINE_IDENTITY is set"];

    const login = await run(["login", "--device", "--person", "ada@example.com"], env, copy.dir);
    expect(login.code, login.stdout).toBe(2);
    expect([login.stdout.includes(TOKEN_FILE), login.stdout.includes(KEY_FILE)]).toEqual([
      true,
      true,
    ]);
    // Each setting set beside the files is named as every other command's refusal names it, and
    // no setting that is not set.
    for (const { named } of REFUSED) {
      expect([named, login.stdout.includes(named)]).toEqual([named, beside.includes(named)]);
    }
    expect([broker.authorizations, broker.polls, broker.refreshes, broker.trades]).toEqual([
      [],
      [],
      [],
      [],
    ]);
    // No login file, no lock and no copy of a login: not even the login's directory is made.
    expect(existsSync(path.dirname(loginFile))).toBe(false);

    // `dispatch login --help` answers as before.
    const help = await run(["login", "--help"], env, copy.dir);
    expect(help.code, help.stdout).toBe(0);
    expect(help.stdout).toContain("Usage: dispatch login --device [--person <address>]");
  });

  test("a tool's dry run beside the token files prints the arguments", async () => {
    // Beside the settings the token files refuse, a dry run that does not ask Dispatch prints the
    // arguments, as it does with Dispatch off. It returns before any request.
    const copy = copyFiles();
    const env = commandEnvironment(copy, "http://dispatch.test", {
      AGENT_SECRETS_URL: "http://broker.test",
      AGENT_SECRETS_MACHINE_IDENTITY: "device-login",
    });
    const dry = await run(
      ["message", "--issue", "DSP-1", "--body", "hi", "--dry-run"],
      env,
      copy.dir
    );
    expect(dry.code, dry.stdout).toBe(0);
    expect(JSON.parse(dry.stdout)).toEqual({ issue: "DSP-1", body: "hi" });
  });

  test("a loop of reads sees the renamed token", async () => {
    const dispatch = standInDispatch();
    const copy = copyFiles();
    // Token n of the loop is `<base>-n`: the file starts at 0, and the renamer counts up from 1.
    const base = fixtureSecret("loop");
    const first = `${base}-0`;
    writeFileSync(copy.tokenFile, `${first}\n`);
    const log = path.join(copy.dir, "renamed");
    writeFileSync(log, "");
    /** The number of the last token the renamer landed, 0 before its first. */
    const landed = (): number => Number(readFileSync(log, "utf8").trim().split("\n").at(-1) || 0);
    /** The number of the token a request carried. */
    const carried = ({ authorization }: Presented): number =>
      Number((authorization ?? "").slice(`DPoP ${base}-`.length));

    // The credential the command resolves, held for the whole loop: one process, 50 reads.
    const env = commandEnvironment(copy, dispatch.url);
    const config = activeDispatchSettings(
      (
        await sessionDispatchSettings({ path: env.ENVOY_CONFIG_FILE ?? "", error: null }, env, {
          cwd: copy.dir,
          tokenFiles: true,
        })
      ).dispatch
    );
    if (config === null) throw new Error("Dispatch is off");
    const client = new DispatchClient(config.url, config.credential);

    // Another process renames a new token over the file every 20 ms, and logs each one once it
    // has landed.
    const renamer = Bun.spawn(
      [
        process.execPath,
        "-e",
        [
          'const { appendFileSync, renameSync, writeFileSync } = require("node:fs");',
          "const file = process.env.RENAMED_FILE;",
          "let n = 0;",
          "setInterval(() => {",
          "  n += 1;",
          '  writeFileSync(file + ".next", process.env.RENAMED_BASE + "-" + n + "\\n");',
          '  renameSync(file + ".next", file);',
          '  appendFileSync(process.env.RENAMED_LOG, n + "\\n");',
          "}, 20);",
        ].join("\n"),
      ],
      {
        env: { RENAMED_FILE: copy.tokenFile, RENAMED_BASE: base, RENAMED_LOG: log },
        stdout: "ignore",
        stderr: "inherit",
      }
    );
    try {
      await until(() => landed() > 0);
      for (let read = 0; read < 50; read += 1) {
        const before = landed();
        // Every read succeeds: a failure throws here.
        await client.listProjects();
        const sent = dispatch.presented.at(-1);
        if (sent === undefined) throw new Error("the read sent nothing");
        expect([read, carried(sent) >= before]).toEqual([read, true]);
        // Every tenth read but the last waits for the next rename, so renames land all through
        // the loop.
        if (read % 10 === 9 && read < 49) await until(() => landed() > before);
      }
    } finally {
      renamer.kill();
      await renamer.exited;
    }
    expect(dispatch.presented).toHaveLength(50);
    // The renames landed while the loop ran, and each request's proof names its own token.
    expect(new Set(dispatch.presented.map(carried)).size).toBeGreaterThan(2);
    for (const presented of dispatch.presented) {
      const token = (presented.authorization ?? "").slice("DPoP ".length);
      expectFileCredential(presented, token, copy.key, dispatch.url);
    }
  });

  test("two commands in one process read both files again", async () => {
    const dispatch = standInDispatch();
    const copy = copyFiles();
    const env = commandEnvironment(copy, dispatch.url);

    expect((await run(["projects"], env, copy.dir)).code).toBe(0);
    const first = dispatch.presented.splice(0);

    const nextToken = fixtureSecret("brk");
    const nextKey = newKey();
    replaceByRename(copy.tokenFile, `${nextToken}\n`);
    replaceByRename(copy.keyFile, pkcs8Of(nextKey));

    expect((await run(["projects"], env, copy.dir)).code).toBe(0);
    const second = dispatch.presented.splice(0);

    expect([first.length, second.length]).toEqual([1, 1]);
    for (const presented of first) {
      expectFileCredential(presented, copy.token, copy.key, dispatch.url);
    }
    for (const presented of second) {
      expectFileCredential(presented, nextToken, nextKey, dispatch.url);
    }
  });

  test("a bad file names the file and quotes none of its bytes", async () => {
    const rsa = generateKeyPairSync("rsa", { modulusLength: 2048 }).privateKey;
    const p384 = generateKeyPairSync("ec", { namedCurve: "P-384" }).privateKey;
    const cases: readonly {
      readonly label: string;
      readonly setting: typeof TOKEN_FILE | typeof KEY_FILE;
      readonly contents: string;
      readonly unreadable?: boolean;
    }[] = [
      { label: "an empty token file", setting: TOKEN_FILE, contents: "" },
      { label: "a blank token file", setting: TOKEN_FILE, contents: " \n\t \n" },
      {
        label: "a token file that cannot be read",
        setting: TOKEN_FILE,
        contents: `${fixtureSecret("brk")}\n`,
        unreadable: true,
      },
      {
        label: "an RSA key (PKCS#1)",
        setting: KEY_FILE,
        contents: rsa.export({ type: "pkcs1", format: "pem" }).toString(),
      },
      { label: "an RSA key (PKCS#8)", setting: KEY_FILE, contents: pkcs8Of(rsa) },
      {
        label: "a SEC1 EC key",
        setting: KEY_FILE,
        contents: newKey().export({ type: "sec1", format: "pem" }).toString(),
      },
      { label: "a PKCS#8 key of another curve", setting: KEY_FILE, contents: pkcs8Of(p384) },
      {
        label: "text that is no PEM",
        setting: KEY_FILE,
        contents: `${fixtureSecret("junk")}\n${fixtureSecret("junk")}\n`,
      },
      {
        label: "a key file that cannot be read",
        setting: KEY_FILE,
        contents: pkcs8Of(newKey()),
        unreadable: true,
      },
    ];
    for (const { label, setting, contents, unreadable } of cases) {
      const dispatch = standInDispatch();
      const copy = copyFiles();
      const file = setting === TOKEN_FILE ? copy.tokenFile : copy.keyFile;
      writeFileSync(file, contents);
      if (unreadable === true) {
        // A directory where the file was, which no process reads as a file, root included: a
        // file's mode does not stop root, so a test run as root would read it.
        rmSync(file);
        mkdirSync(file);
      }
      const { code, stdout, stderr } = await run(
        ["projects"],
        commandEnvironment(copy, dispatch.url),
        copy.dir
      );
      const output = `${stdout}${stderr}`;
      expect([label, code]).toEqual([label, 1]);
      expect([label, output.includes(setting), output.includes(file)]).toEqual([label, true, true]);
      expect([label, quotedRun(output, contents)]).toEqual([label, undefined]);
      expect([label, dispatch.presented]).toEqual([label, []]);
    }
  });

  test("a plugin's session refuses the token files", async () => {
    const copy = copyFiles();
    const url = "https://dispatch.example";
    const env = commandEnvironment(copy, url);
    const chosen = { path: env.ENVOY_CONFIG_FILE ?? "", error: null } as const;
    for (const [label, set, named] of [
      ["both files", env, [TOKEN_FILE, KEY_FILE]],
      ["the token file alone", { ...env, [KEY_FILE]: undefined }, [TOKEN_FILE]],
      ["the key file alone", { ...env, [TOKEN_FILE]: undefined }, [KEY_FILE]],
      [
        "both files beside DISPATCH_TOKEN",
        { ...env, DISPATCH_TOKEN: fixtureSecret("dsp") },
        [TOKEN_FILE, KEY_FILE],
      ],
    ] as const) {
      for (const dispatch of [
        (await sessionDispatchSettings(chosen, set, { cwd: copy.dir })).dispatch,
        (await sessionSettings(chosen, set, { cwd: copy.dir, nats: false })).dispatch,
      ]) {
        expect([label, dispatch.enabled]).toEqual([label, false]);
        const error = dispatch.enabled ? "" : (dispatch.error ?? "");
        for (const name of named)
          expect([label, error]).toEqual([label, expect.stringContaining(name)]);
        expect([label, error]).toEqual([label, expect.stringContaining("dispatch command")]);
      }
    }

    const command = await sessionDispatchSettings(chosen, env, { cwd: copy.dir, tokenFiles: true });
    if (!command.dispatch.enabled) throw new Error(`Dispatch is off: ${command.dispatch.error}`);
    expect(command.dispatch.url).toBe(url);
    expect(command.dispatch.credential.scheme).toBe("DPoP");
    const headers = await command.dispatch.credential.headers("GET", `${url}/api/v1/projects`);
    expectFileCredential(
      presentedOf({
        method: "GET",
        url: new URL(`${url}/api/v1/projects`),
        headers: new Headers(headers),
      }),
      copy.token,
      copy.key,
      url
    );
  });

  // The 30 s limit is a hang guard, not a budget: the test runs 20 commands in this process, a read
  // for every row but T1 and P3's write, and bun's unwritten 5,000 ms per-test default is a limit
  // on that work rather than on a hang. It took 0.25 s with this file alone, and 4.71 s with a
  // biome check running beside it in a pod six agents share.
  test("beside DISPATCH_PUBLIC_URL the token files take its origin or a Service name's, whose proofs name the public origin, and refuse any other DISPATCH_URL", async () => {
    // One line for each fact a row found, beside the line it must find. Every line names its row,
    // so a failure names every row that fails rather than the first.
    const seen: string[] = [];
    const wanted: string[] = [];
    for (const entry of PUBLIC_URL_ROWS) {
      const { row, holds } = entry;
      const standIn = standInDispatch();
      const fill = (url: string): string =>
        url.replace("<stand-in>", standIn.url).replace("<port>", new URL(standIn.url).port);
      const dispatchUrl = fill(entry.dispatchUrl);
      const copy = copyFiles();
      const env = commandEnvironment(copy, dispatchUrl, {
        DISPATCH_PUBLIC_URL: entry.publicUrl === undefined ? undefined : fill(entry.publicUrl),
      });

      if (holds.kind === "credential") {
        // The credential the command resolves for these settings, asked for each URL it refuses.
        const config = activeDispatchSettings(
          (
            await sessionDispatchSettings({ path: env.ENVOY_CONFIG_FILE ?? "", error: null }, env, {
              cwd: copy.dir,
              tokenFiles: true,
            })
          ).dispatch
        );
        seen.push(`${row} presents ${config?.credential.scheme ?? "nothing: Dispatch is off"}`);
        wanted.push(`${row} presents DPoP`);
        for (const url of holds.refused.map(fill)) {
          const message = await Promise.resolve()
            .then(() => config?.credential.headers("GET", url))
            .then(
              () => "",
              (error: unknown) => String(error)
            );
          seen.push(
            `${row} GET ${url}: refused ${message !== ""}, naming DISPATCH_URL ${message.includes("DISPATCH_URL")}, quoting the token ${quotedRun(message, copy.token) !== undefined}`
          );
          wanted.push(
            `${row} GET ${url}: refused true, naming DISPATCH_URL true, quoting the token false`
          );
        }
        continue;
      }

      const { fetchImpl, asked, mount } = forwardingFetch(dispatchUrl, standIn.url);
      const read = await run(["read", "--issue", "DSP-1"], env, copy.dir, fetchImpl);
      const output = `${read.stdout}${read.stderr}`;
      if (holds.kind === "refused") {
        const heard = standIn.presented.map(({ method, url }) => `${method} ${url.pathname}`);
        seen.push(
          `${row} exits ${read.code}`,
          ...holds.named
            .filter((name) => output.includes(name))
            .map((name) => `${row} names ${name}`),
          `${row} asks for ${asked.map(shownOf).join(", ") || "nothing"}`,
          `${row} has the stand-in hear ${heard.join(", ") || "nothing"}`
        );
        wanted.push(
          `${row} exits 2`,
          ...holds.named.map((name) => `${row} names ${name}`),
          `${row} asks for nothing`,
          `${row} has the stand-in hear nothing`
        );
        continue;
      }

      // The command started: each request it asked for is on DISPATCH_URL's origin, and its proof
      // names the row's proof origin followed by the request's own path, never its query.
      const origin = new URL(dispatchUrl).origin;
      const proofOrigin = fill(holds.proofOrigin);
      const noteRequests = (as: string, sent: readonly Asked[]): void => {
        for (const one of sent) {
          const htu = one.dpop === null ? "no proof" : proofOf(one.dpop).claims.htu;
          seen.push(`${as} ${shownOf(one)} goes to ${one.url.origin}, its proof naming ${htu}`);
          wanted.push(
            `${as} ${shownOf(one)} goes to ${origin}, its proof naming ${proofOrigin}${one.url.pathname}`
          );
        }
      };
      const reads = asked.splice(0);
      const issueRead = `GET ${mount}/api/v1/issues/DSP-1`;
      seen.push(
        `${row} exits ${read.code}`,
        ...(reads.map(shownOf).includes(issueRead) ? [`${row} asks for ${issueRead}`] : [])
      );
      wanted.push(`${row} exits 0`, `${row} asks for ${issueRead}`);
      if (holds.query === true) {
        // This row's requests are the ones that carry a query.
        const queried = reads.filter(({ url }) => url.search !== "");
        if (queried.length > 0) seen.push(`${row} asks for a URL with a query`);
        wanted.push(`${row} asks for a URL with a query`);
        noteRequests(row, queried);
      } else {
        noteRequests(row, reads);
      }
      const { unquoted } = holds;
      if (unquoted !== undefined) {
        for (const line of output.split("\n").filter((printed) => printed.includes(unquoted))) {
          seen.push(`${row} prints ${JSON.stringify(line)}`);
        }
      }

      if (holds.write === true) {
        // The stand-in refuses the comment as a read-only token's, so the command exits 1.
        const write = await run(
          ["comment", "--issue", "DSP-1", "--body", "Read only."],
          env,
          copy.dir,
          fetchImpl
        );
        const writes = asked.splice(0);
        const comment = `POST ${mount}/api/v1/issues/DSP-1/comments`;
        seen.push(
          `${row} write exits ${write.code}`,
          ...(writes.map(shownOf).includes(comment) ? [`${row} write asks for ${comment}`] : [])
        );
        wanted.push(`${row} write exits 1`, `${row} write asks for ${comment}`);
        noteRequests(`${row} write`, writes);
      }
    }
    expect(seen).toEqual(wanted);
  }, 30_000);
});
