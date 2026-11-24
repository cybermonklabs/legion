/** `bun e2e/read-token-live.ts`: the live proof that the read-only tokens a running secrets broker
 *  mints read a running Dispatch and write nothing. Dispatch serves a read-only token on every GET
 *  route it classes as a read (`readClasses`, packages/envoy/internal/dispatch/api/read_classes.go)
 *  and answers it 403 READ_ONLY_TOKEN on every other route and on GET /ws/live. It answers one
 *  that carries a cut 403 AS_OF_TOKEN_NOT_SERVED on every read but GET /api/v1/whoami. Four tokens
 *  show it:
 *
 *  - The read entry's (BROKER_EXCHANGE_READ_ACCOUNTS). Its pod's trade asks for no scope and gets
 *    the scope `read` alone, with no person and no project. It reads the issue and is refused a
 *    comment and GET /ws/live.
 *  - The example entry's (BROKER_EXCHANGE_ACCOUNTS), traded asking for `read`. The token keeps the
 *    person, the session and the project the entry names, under the project scope `read-named`.
 *    It reads the issue and is refused the same comment and GET /ws/live. The same pod's ordinary
 *    token then sends that comment and is answered 2xx, so the refusal is the scope's and not the
 *    write's.
 *  - A machine login's, traded asking for `read`. The script logs a machine in as
 *    `agent-secrets machine login` does and has `agent-secrets-devrelay` approve it as
 *    dev-broker.sh's approver. The machine's key holds the login's launcher credential, so it
 *    signs the trade alone, and the token is bound to a key made for its holder, which signs a
 *    second proof of the trade (`DPoP-Holder`).
 *  - The same machine login's, traded asking for `read` with a cut.
 *
 *  The broker's introspection answer for each of the two machine login tokens must be the one
 *  packages/envoy/internal/ciphertoken/testdata/narrowed-answers.json holds for that kind, and the
 *  cut token's GET /api/v1/whoami must name the cut.
 *
 *  Run it from packages/dispatch once two stacks are up on this machine:
 *
 *  1. The broker: `bash ../envoy/scripts/dev-broker.sh`, with
 *     `DEV_BROKER_POSTGRES_URL=postgres://<role>@<host>:<port>/<database>` where there is no
 *     Docker. Its banner prints `AGENT_SECRETS_URL`, `AGENT_SECRETS_ACCOUNT_TOKEN_FILE` and
 *     `DEV_BROKER_DIR`, which holds `read-token` (the read entry's pod token), `exchange-token`
 *     (the example entry's, a pod the script registered under `example-worker-session`),
 *     `account-token` (the caller "dispatch", which introspects) and `bin/agent-secrets-devrelay`.
 *  2. Dispatch, reading that broker: `DISPATCH_E2E_AGENT_SECRETS_URL=$AGENT_SECRETS_URL
 *     DISPATCH_E2E_AGENT_SECRETS_ACCOUNT_TOKEN_FILE=$AGENT_SECRETS_ACCOUNT_TOKEN_FILE
 *     bun run dispatch:local`, with `DATABASE_URL=<a loopback database of its own>` where there is
 *     no Docker. Its "Run a spec against it" line prints the variables a command reaches it by;
 *     this script reads `DISPATCH_E2E_PORT` of them.
 *  3. `<the variables that line prints> AGENT_SECRETS_URL=<the broker> DEV_BROKER_DIR=<its
 *     workdir> bun e2e/read-token-live.ts`.
 *
 *  It writes into that Dispatch: project EXAMPLE, which is the example entry's project so that
 *  entry's token may write there; an issue of EXAMPLE; and the example entry's comment on it. It
 *  writes a machine login into the broker. Project EXAMPLE then exists, so a second run needs
 *  `bun run dispatch:local` restarted, which empties the instance.
 *
 *  It prints one line per request with the status and the code it was answered, and exits 1 when
 *  any answer is not the one it wants, or when a step before them fails. */
import { createHash, generateKeyPairSync, randomUUID, sign } from "node:crypto";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import {
  DISPATCH_AS_OF_TOKEN_NOT_SERVED,
  DISPATCH_READ_ONLY_TOKEN,
  IDEMPOTENCY_KEY_HEADER,
} from "@legion/contracts";
import { credentialDeadline } from "@legion/envoy-client/bearer";
import { brokerCredential } from "@legion/envoy-client/broker-credential";
// packages/dispatch does not depend on @legion/cipher-client, so the client the `dispatch` command
// trades with is read from its source.
import { type DpopKey, importDpopKey, newDpopKey } from "../../cipher-client/src/dpop";
import type { ReadTrade } from "../../cipher-client/src/environment";
import type { TokenSource } from "../../cipher-client/src/source";
import { type BrokerToken, BrokerTokenClient } from "../../cipher-client/src/token-client";
import { SCOPE_READ } from "../../cipher-client/src/wire";
import { baseUrl, createIssue, createProject, listComments } from "./api";
import { harnessPorts } from "./harness-ports";

/** The example entry's project (dev-broker.sh's EXCHANGE_PROJECT): its named token writes it. */
const project = "EXAMPLE";
/** The session the read entry fixes (dev-broker.sh's READ_SESSION). */
const readSession = "example-reader-session";
/** The session dev-broker.sh registered the example entry's pod under (EXCHANGE_SESSION). */
const namedSession = "example-worker-session";
/** The person who approves the machine login (dev-broker.sh's APPROVER_LOGIN), whom the golden
 *  answers name. */
const approver = "ada@example.com";
/** The session the machine login's two tokens are traded under, this run's own. */
const machineSession = `read-token-live-${randomUUID()}`;
/** The cut the golden answer `machine-login-with-a-cut` names. */
const goldenCut = "2026-11-18T09:30:00.123456Z";
/** The golden introspection answers of a token narrowed to reading, by kind of trade. */
const goldenFile = fileURLToPath(
  new URL("../../envoy/internal/ciphertoken/testdata/narrowed-answers.json", import.meta.url)
);
/** The comment the tokens send, this run's own, so the count alice reads is of this run's. */
const commentBody = `Written by read-token-live.ts, run ${randomUUID()}.`;

/** A machine login's OAuth client and scope (`oauthwire.ClientID`, `oauthwire.Scope`). */
const machineClientId = "agent-secrets-helper";
const machineScope = "machine";
/** The device authorization grant a machine login polls with (RFC 8628). */
const deviceCodeGrant = "urn:ietf:params:oauth:grant-type:device_code";
/** The `typ` of a credential-request object (`record.RequestTyp`). */
const requestTyp = "agent-secrets-request+jwt";
/** How long a request object lives (`record`'s `maxRequestLifetime`). */
const requestLifetimeS = 600;
/** The `subject_token_type` of a machine login's access token traded at the exchange
 *  (`oauthwire.SubjectTokenTypeAccessToken`, RFC 8693's own name). */
const accessTokenType = "urn:ietf:params:oauth:token-type:access_token";
/** How far the introspection answer's `exp` may be from the expiry the trade's answer gave: the
 *  client counts `expires_in` from its own clock, once the answer is read. */
const expirySlackS = 5;

/** `name`'s value, trimmed, or a refusal that says where it comes from. */
function required(name: string, from: string): string {
  const value = (process.env[name] ?? "").trim();
  if (value === "") throw new Error(`${name} is required: ${from}`);
  return value;
}

/** How long one request may take, its broker trade included: the credential waits on all of it
 *  but the request's 1 s reserve (`credentialDeadline`), and the request goes in that reserve. */
const REQUEST_DEADLINE_MS = 30_000;

/** The headers that authenticate one request to Dispatch: `Authorization` and a DPoP proof for
 *  that very method and URL. */
type Presenter = (
  method: string,
  url: string,
  deadline: AbortSignal
) => Promise<Readonly<Record<string, string>>>;

/** The broker credential of a pod whose projected token is `tokenFile`, trading under `session`,
 *  asking for `read` where it is given. The environment is this script's own and carries nothing
 *  of the shell's, and the person trade is off, so a sign-in kept under this machine's HOME is
 *  never traded in the pod's place. */
function podCredential(
  brokerUrl: string,
  tokenFile: string,
  session: string,
  read?: ReadTrade
): Presenter {
  const credential = brokerCredential(
    {
      AGENT_SECRETS_URL: brokerUrl,
      AGENT_SECRETS_MACHINE_IDENTITY: "kubernetes",
      AGENT_SECRETS_MACHINE_TOKEN_FILE: tokenFile,
      PERSON_SIGNIN_TRADE: "off",
    },
    session,
    read
  );
  return async (method, url, deadline) =>
    await credential.headers(
      method,
      url,
      undefined,
      credentialDeadline(deadline, REQUEST_DEADLINE_MS)
    );
}

/** A token in hand, presented with a proof of the key it is bound to. */
function heldToken(token: BrokerToken, key: DpopKey): Presenter {
  return async (method, url) => ({
    Authorization: `DPoP ${token.accessToken}`,
    DPoP: await key.proof({ accessToken: token.accessToken, method, url }),
  });
}

/** What Dispatch answered one request: its status, its error code ("" for an answer with none)
 *  and its body where that is a JSON object (empty for one that is not). */
interface Answer {
  readonly status: number;
  readonly code: string;
  readonly body: Readonly<Record<string, unknown>>;
}

/** `value` where it is a JSON object, each member unknown until read; else undefined. */
function asObject(value: unknown): Readonly<Record<string, unknown>> | undefined {
  return typeof value === "object" && value !== null && !Array.isArray(value)
    ? Object.fromEntries(Object.entries(value))
    : undefined;
}

/** `text` parsed, where it is a JSON object; else undefined. */
function jsonObject(text: string): Readonly<Record<string, unknown>> | undefined {
  try {
    return asObject(JSON.parse(text));
  } catch {
    return undefined;
  }
}

/** Sends `method path` to Dispatch with the headers `presenter` makes for that very URL. */
async function present(
  presenter: Presenter,
  method: string,
  path: string,
  body?: object,
  headers: Readonly<Record<string, string>> = {}
): Promise<Answer> {
  const url = new URL(path, baseUrl).toString();
  const deadline = AbortSignal.timeout(REQUEST_DEADLINE_MS);
  const response = await fetch(url, {
    body: body === undefined ? undefined : JSON.stringify(body),
    headers: {
      ...(body === undefined ? {} : { "Content-Type": "application/json" }),
      ...headers,
      ...(await presenter(method, url, deadline)),
    },
    method,
    signal: deadline,
  });
  // An answer that is not JSON carries no code.
  const answer = jsonObject(await response.text()) ?? {};
  const code = typeof answer.code === "string" ? answer.code : "";
  return { body: answer, code, status: response.status };
}

/** The answer a request must get: a status, or any 2xx, and the code where one is wanted. */
interface Want {
  readonly status: number | "2xx";
  readonly code?: string;
}

let mismatches = 0;

/** Prints one line, what was asked, what came back and what was wanted, and counts it when it
 *  is not what was wanted. */
function report(what: string, ok: boolean, got: string, wanted: string): void {
  if (!ok) mismatches++;
  console.log(`${ok ? "ok  " : "FAIL"} ${what}: ${got} (want ${wanted})`);
}

/** Reports one request's answer against `want`. */
function answered(what: string, answer: Answer, want: Want): void {
  const statusOk =
    want.status === "2xx"
      ? answer.status >= 200 && answer.status < 300
      : answer.status === want.status;
  report(
    what,
    statusOk && (want.code === undefined || answer.code === want.code),
    `${answer.status} ${answer.code || "-"}`,
    `${want.status}${want.code === undefined ? "" : ` ${want.code}`}`
  );
}

/** The RFC 7638 thumbprint of a P-256 public JWK, which the broker binds a token to (`jkt`). */
function thumbprint(jwk: DpopKey["publicJwk"]): string {
  const members = JSON.stringify({ crv: jwk.crv, kty: jwk.kty, x: jwk.x, y: jwk.y });
  return createHash("sha256").update(members).digest("base64url");
}

/** The JSON body of a broker answer, or a refusal naming the request and what came back. */
async function brokerJson(
  what: string,
  response: Response
): Promise<Readonly<Record<string, unknown>>> {
  const answer = jsonObject(await response.text());
  if (answer === undefined) {
    throw new Error(`${what}: ${response.status}, an answer that is not a JSON object`);
  }
  return answer;
}

/** A machine login: the key it is bound to, which holds its launcher credential, and its access
 *  token. */
interface MachineLogin {
  readonly key: DpopKey;
  readonly accessToken: string;
}

/**
 * Logs a machine in as `agent-secrets machine login` does (packages/envoy/internal/cipher/helper,
 * `Broker.Login`): a key of its own; a credential-request object for a launcher credential, naming
 * the approver as its `login_hint`, signed with that key (`record.Sign`); the device authorization
 * with the key's thumbprint; and the token poll, with a DPoP proof of that key. In between,
 * `agent-secrets-devrelay machine-approve` approves the login's code as Dispatch's server would.
 */
async function machineLogin(brokerUrl: string, brokerDir: string): Promise<MachineLogin> {
  const { privateKey } = generateKeyPairSync("ec", { namedCurve: "P-256" });
  const key = await importDpopKey(
    privateKey.export({ format: "pem", type: "pkcs8" }).toString(),
    "this run's machine key"
  );
  const jkt = thumbprint(key.publicJwk);
  const iat = Math.floor(Date.now() / 1000);
  const encode = (part: object) => Buffer.from(JSON.stringify(part)).toString("base64url");
  const signingInput = `${encode({ alg: "ES256", jwk: key.publicJwk, typ: requestTyp })}.${encode({
    aud: brokerUrl,
    authorization_details: [{ identifier: "read-token-live", type: "launcher_credential" }],
    exp: iat + requestLifetimeS,
    iat,
    iss: jkt,
    jti: randomUUID(),
    login_hint: approver,
  })}`;
  const signature = sign("sha256", Buffer.from(signingInput), {
    dsaEncoding: "ieee-p1363",
    key: privateKey,
  });
  const authorization = await brokerJson(
    "POST /v1/oauth/device_authorization",
    await fetch(`${brokerUrl}/v1/oauth/device_authorization`, {
      body: new URLSearchParams({
        client_id: machineClientId,
        dpop_jkt: jkt,
        request: `${signingInput}.${signature.toString("base64url")}`,
        scope: machineScope,
      }),
      method: "POST",
      signal: AbortSignal.timeout(REQUEST_DEADLINE_MS),
    })
  );
  const { device_code: deviceCode, interval, user_code: userCode } = authorization;
  if (typeof deviceCode !== "string" || typeof userCode !== "string") {
    throw new Error(
      `POST /v1/oauth/device_authorization answered no device code and user code: ${JSON.stringify(authorization.error ?? authorization)}`
    );
  }

  const devrelay = Bun.spawn(
    [
      join(brokerDir, "bin", "agent-secrets-devrelay"),
      "machine-approve",
      "--code",
      userCode,
      "--login",
      approver,
      "--broker",
      brokerUrl,
      "--account-token-file",
      join(brokerDir, "account-token"),
    ],
    { env: { PATH: process.env.PATH ?? "" }, stderr: "pipe", stdout: "pipe" }
  );
  const [approved, refused, exit] = await Promise.all([
    new Response(devrelay.stdout).text(),
    new Response(devrelay.stderr).text(),
    devrelay.exited,
  ]);
  if (exit !== 0 || jsonObject(approved)?.state !== "approved") {
    throw new Error(`agent-secrets-devrelay machine-approve exited ${exit}: ${refused}${approved}`);
  }

  // The poll: the broker may ask for a nonce first, and answers authorization_pending until the
  // approval it has already taken is read.
  const tokenUrl = `${brokerUrl}/v1/oauth/token`;
  const waitMs = (typeof interval === "number" && interval > 0 ? interval : 1) * 1000;
  const until = Date.now() + 60_000;
  let nonce: string | undefined;
  for (;;) {
    const response = await fetch(tokenUrl, {
      body: new URLSearchParams({
        client_id: machineClientId,
        device_code: deviceCode,
        grant_type: deviceCodeGrant,
      }),
      headers: { DPoP: await key.proof({ method: "POST", nonce, url: tokenUrl }) },
      method: "POST",
      signal: AbortSignal.timeout(REQUEST_DEADLINE_MS),
    });
    nonce = response.headers.get("DPoP-Nonce") ?? nonce;
    const answer = await brokerJson("the machine login's token poll", response);
    if (response.ok && typeof answer.access_token === "string") {
      return { accessToken: answer.access_token, key };
    }
    const again = ["use_dpop_nonce", "authorization_pending", "slow_down"].includes(
      String(answer.error)
    );
    if (!again || Date.now() > until) {
      throw new Error(
        `the machine login's token poll: ${response.status} ${String(answer.error)} ${String(answer.error_description ?? "")}`
      );
    }
    if (answer.error !== "use_dpop_nonce") await Bun.sleep(waitMs);
  }
}

/** A read token traded for `login`'s access token, with the cut `asOf` where it is given, and the
 *  holder key it is bound to. The trade's DPoP proof is the machine key's, which the login's
 *  access token is bound to; the holder key, made here, signs the second proof. */
async function machineReadToken(
  brokerUrl: string,
  login: MachineLogin,
  asOf?: string
): Promise<{ readonly token: BrokerToken; readonly key: DpopKey }> {
  const subject: TokenSource = {
    describe: () => "this run's machine login",
    read: async () => ({ token: login.accessToken, tokenType: accessTokenType }),
  };
  const client = new BrokerTokenClient({
    asOf,
    brokerUrl,
    holderKey: await newDpopKey(),
    key: login.key,
    scope: SCOPE_READ,
    session: machineSession,
    subject,
  });
  try {
    return { key: client.key, token: await client.token(AbortSignal.timeout(REQUEST_DEADLINE_MS)) };
  } finally {
    client.close();
  }
}

/** Introspects `token` at the broker as the caller "dispatch" does, and reports the answer against
 *  the golden answer `kind`, its placeholders filled in with the holder key's thumbprint, this
 *  run's session and the expiry the trade answered. */
async function introspects(
  what: string,
  brokerUrl: string,
  brokerDir: string,
  traded: { readonly token: BrokerToken; readonly key: DpopKey },
  kind: string
): Promise<void> {
  const accountToken = readFileSync(join(brokerDir, "account-token"), "utf8").trim();
  const answer = await brokerJson(
    "POST /v1/oauth/introspect",
    await fetch(`${brokerUrl}/v1/oauth/introspect`, {
      body: new URLSearchParams({ token: traded.token.accessToken }),
      headers: { Authorization: `Bearer ${accountToken}` },
      method: "POST",
      signal: AbortSignal.timeout(REQUEST_DEADLINE_MS),
    })
  );
  const golden = asObject(asObject(jsonObject(readFileSync(goldenFile, "utf8"))?.answers)?.[kind]);
  if (golden === undefined) throw new Error(`${goldenFile} holds no answer ${kind}`);
  const tradedExp = traded.token.expiresAt.getTime() / 1000;
  const values: Record<string, unknown> = {
    exp:
      typeof answer.exp === "number" && Math.abs(answer.exp - tradedExp) <= expirySlackS
        ? answer.exp
        : Math.round(tradedExp),
    jkt: thumbprint(traded.key.publicJwk),
    session: machineSession,
  };
  const want = Object.fromEntries(
    Object.entries(golden).map(([name, value]) => {
      const placeholder = typeof value === "string" ? /^\$\{(\w+)\}$/.exec(value) : null;
      return [name, placeholder === null ? value : values[placeholder[1] ?? ""]];
    })
  );
  const differ = [...new Set([...Object.keys(want), ...Object.keys(answer)])]
    .filter((name) => JSON.stringify(want[name]) !== JSON.stringify(answer[name]))
    .sort();
  report(
    `POST /v1/oauth/introspect [${what}]`,
    differ.length === 0,
    differ.length === 0 ? `the answer ${kind}` : `an answer that differs at ${differ.join(", ")}`,
    kind
  );
}

async function main(): Promise<void> {
  if (process.env.PLAYWRIGHT_BASE_URL) {
    throw new Error(
      "unset PLAYWRIGHT_BASE_URL: this script drives the harness `bun run dispatch:local` started"
    );
  }
  const { dispatch } = harnessPorts();
  if (dispatch.port === 0) {
    throw new Error(
      `${dispatch.variable} is required: the port \`bun run dispatch:local\` prints for it`
    );
  }
  const brokerUrl = required("AGENT_SECRETS_URL", "the broker dev-broker.sh's banner exports");
  const brokerDir = required("DEV_BROKER_DIR", "the workdir dev-broker.sh's banner exports");

  // The issue every token reads and writes a comment on, in the project the example entry writes,
  // made as alice.
  await createProject({ key: project, name: "Example" });
  const issue = await createIssue({ project, title: "Read by a read-only token" });
  console.log(`seeded ${project}: issue ${issue.key}`);

  const issuePath = `/api/v1/issues/${issue.key}`;
  const comments = `${issuePath}/comments`;
  // One key for every POST, as one client sending the same write again would. A key belongs to its
  // caller (requestKeyLogin, internal/dispatch/api), and a read-only token is refused before the
  // key is read, so the named token's write runs under it whatever the refused requests did.
  const comment = { body: commentBody };
  const keyed = { [IDEMPOTENCY_KEY_HEADER]: `read-token-live-${randomUUID()}` };
  const readOnly: Want = { code: DISPATCH_READ_ONLY_TOKEN, status: 403 };

  // The read entry's token and the example entry's read token: each reads the issue, and is
  // refused the comment and the live socket.
  for (const [what, presenter] of [
    ["read entry's token", podCredential(brokerUrl, join(brokerDir, "read-token"), readSession)],
    [
      "example entry's read token",
      podCredential(brokerUrl, join(brokerDir, "exchange-token"), namedSession, {}),
    ],
  ] as const) {
    answered(`GET ${issuePath} [${what}]`, await present(presenter, "GET", issuePath), {
      status: 200,
    });
    answered(
      `POST ${comments} [${what}]`,
      await present(presenter, "POST", comments, comment, keyed),
      readOnly
    );
    answered(`GET /ws/live [${what}]`, await present(presenter, "GET", "/ws/live"), readOnly);
  }
  // The refused writes wrote nothing: alice reads no comment with this run's body on the issue.
  const written = (await listComments(issue.key)).filter((row) => row.body === commentBody);
  report(
    `GET ${comments} [alice]`,
    written.length === 0,
    `${written.length} comment(s) with this run's body`,
    "0"
  );
  answered(
    `POST ${comments} [example entry's token]`,
    await present(
      podCredential(brokerUrl, join(brokerDir, "exchange-token"), namedSession),
      "POST",
      comments,
      comment,
      keyed
    ),
    { status: "2xx" }
  );

  // A machine login's read token, and its read token with a cut.
  const login = await machineLogin(brokerUrl, brokerDir);
  const reader = await machineReadToken(brokerUrl, login);
  const readerWhat = "machine login's read token";
  await introspects(readerWhat, brokerUrl, brokerDir, reader, "machine-login");
  const readerPresents = heldToken(reader.token, reader.key);
  answered(`GET ${issuePath} [${readerWhat}]`, await present(readerPresents, "GET", issuePath), {
    status: 200,
  });
  answered(
    `GET /ws/live [${readerWhat}]`,
    await present(readerPresents, "GET", "/ws/live"),
    readOnly
  );

  const cut = await machineReadToken(brokerUrl, login, goldenCut);
  const cutWhat = "machine login's read token with a cut";
  await introspects(cutWhat, brokerUrl, brokerDir, cut, "machine-login-with-a-cut");
  const cutPresents = heldToken(cut.token, cut.key);
  const whoami = await present(cutPresents, "GET", "/api/v1/whoami");
  const named = whoami.body.cut;
  report(
    `GET /api/v1/whoami [${cutWhat}]`,
    whoami.status === 200 && named === goldenCut,
    `${whoami.status} cut ${typeof named === "string" ? named : "none"}`,
    `200 cut ${goldenCut}`
  );
  answered(`GET ${issuePath} [${cutWhat}]`, await present(cutPresents, "GET", issuePath), {
    code: DISPATCH_AS_OF_TOKEN_NOT_SERVED,
    status: 403,
  });
  answered(`GET /ws/live [${cutWhat}]`, await present(cutPresents, "GET", "/ws/live"), readOnly);
}

let exitCode = 0;
try {
  await main();
  if (mismatches > 0) {
    console.error(`read-token-live: ${mismatches} answer(s) were not the ones wanted`);
    exitCode = 1;
  }
} catch (error) {
  console.error(
    `read-token-live failed: ${error instanceof Error ? error.message : String(error)}`
  );
  exitCode = 1;
}
process.exit(exitCode);
