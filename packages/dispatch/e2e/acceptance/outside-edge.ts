/** The outside end-to-end driver: what works for a person from outside, through the edge that
 *  serves them, on the surface they use. It runs where they would, on a machine of the public
 *  internet, and drives the real pages in a real browser (Chromium, through Playwright), the real
 *  broker client and the real relay client. Run it from packages/dispatch with
 *  `bun e2e/acceptance/outside-edge.ts [run | approve <code> | revoke | t5]`.
 *
 *  `run`, the default, makes four steps, each a row of its own:
 *    1. sign-in   the owner's link enrolls a virtual passkey (Chromium's virtual authenticator,
 *                 `e2e/virtual-passkey.ts`), which signs the person in at the link's origin, the
 *                 outside origin; then `GET /api/v1/projects` must list exactly OUTSIDE_PROJECT
 *                 (row `projects`, which names each project too many or missing).
 *    2. device login  the broker client (`@legion/cipher-client/device-login`) asks
 *                 OUTSIDE_BROKER_URL for a device login of this machine for the signed-in person,
 *                 whose page must be on the outside origin; the browser types the code on that
 *                 page and approves it, and the poll must answer a DPoP-bound pair (row
 *                 `device-login`). With OUTSIDE_TAILNET_USER_CODE set, the same page then
 *                 approves that code too, in the same browser session, and prints
 *                 `APPROVED <code>` (row `tailnet-code`), else fails naming the code.
 *    3. relay     the device login is traded for the session's broker credential, the session
 *                 opens its relay socket at OUTSIDE_LISTENER_URL and registers there
 *                 (`@legion/envoy-client/listener-relay`, `consumer-delivery`), and sends
 *                 `outside-e2e ping <nonce> at <Unix ms>` to OUTSIDE_PEER_SESSION; the peer's
 *                 reply, `outside-e2e pong <nonce> at <Unix ms>`, must reach the session on the
 *                 socket within OUTSIDE_EDGE_REPLY_SECONDS (row `relay`), else the row fails
 *                 naming the peer. The peer's `outside-e2e hello <nonce> …` that follows is
 *                 answered `outside-e2e reply <nonce> at <Unix ms>`, so the peer does not wait.
 *    4. limits    every request of the run, the browser's and this process's, is counted
 *                 (`outside-edge-count.ts`) and judged against the edge's rate rules in
 *                 OUTSIDE_EDGE_LIMITS (`outside-edge-limits.ts` says the file and the rule): a
 *                 row per threshold, which fails above half of it, naming the host; and a line per
 *                 host with its peak per minute.
 *  A step whose step before it failed is UNKNOWN, never run on nothing.
 *
 *  Inputs, each refused by name, UNKNOWN and exit 2, when missing or malformed:
 *    OUTSIDE_INVITE_URL     the owner's enrollment link, https://<outside origin>/enroll#<token>.
 *                           It is single-use, so a run spends it; the run never prints it.
 *    OUTSIDE_PROJECT        the one project the person's list must name.
 *    OUTSIDE_PEER_SESSION   the session that answers `outside-e2e ping <nonce> at <ms>` with
 *                           `outside-e2e pong <nonce> at <ms>` to its sender.
 *    OUTSIDE_BROKER_URL     the secrets broker's origin for the device login and the trade.
 *    OUTSIDE_LISTENER_URL   the Envoy listener's origin for the relay socket and the sends.
 *    OUTSIDE_EDGE_LIMITS    the rate rules file.
 *    OUTSIDE_EDGE_STATE     a directory of this run's own, made mode 0700 when missing, where the
 *                           run keeps, each mode 0600: the browser's signed-in state
 *                           (`browser.json`), its cookie as a Cookie header (`cookie`), the
 *                           person (`person`), the outside origin (`origin`), the passkey the
 *                           virtual authenticator made (`passkey.json`), the device login
 *                           (`device-login.json`) and each code it approved with its credential
 *                           (`approved.tsv`). `approve`, `revoke` and `t5` read them, and so does
 *                           a later step that needs the person's cookie.
 *  Optional: OUTSIDE_TAILNET_USER_CODE (above); OUTSIDE_EDGE_REPLY_SECONDS (30 when unset; 1 to
 *  600); OUTSIDE_EDGE_TSV, the file the rows are also written to (`<state>/outside-edge.tsv` when
 *  unset). OUTSIDE_EDGE_ALLOW_HTTP=1 lets each origin be http://127.0.0.1 or http://localhost,
 *  and nothing else, for this driver's own test.
 *
 *  `approve <code>` approves one more device-login code on the outside origin's machine-login
 *  page, in the browser state `run` kept in OUTSIDE_EDGE_STATE, and records its credential; it
 *  prints `APPROVED <code>` and exits 0, or `FAIL: <code> ...` (1) or `UNKNOWN: <code> ...` (2).
 *  Another check that starts a device login of its own calls it with the code it was given.
 *
 *  `revoke` lists the person's machine logins (`GET /api/v1/machine-logins`) with the kept cookie
 *  and revokes each credential `approved.tsv` holds (`POST /api/v1/machine-logins/{id}/revoke`),
 *  a row each, `revoke-<credential id>`, printed and written to OUTSIDE_EDGE_REVOCATIONS
 *  (`<state>/outside-edge-revocations.tsv` when unset). A credential the list does not hold
 *  fails its row: the person's own list is where they end it. A row passes only when the list,
 *  read again after the revoke answered 2xx, no longer holds the credential; a list that gave no
 *  answer, before or after, reads UNKNOWN. OUTSIDE_EDGE_REVOKE_KEEP names one approved code whose
 *  login it leaves live, with no row: the login a delisting window measures ending, which the
 *  person's removal ends and not this revoke.
 *
 *  `t5` is the browser's half of a delisting window: with OUTSIDE_T5_DEADLINE (Unix seconds) and
 *  OUTSIDE_T5_TSV, it asks `/auth/whoami` with the kept cookie at most every 5 s until it is
 *  answered 401, then tries the kept passkey in a fresh browser; rows `T5-cookie` and
 *  `T5-passkey`, each naming `at=<Unix seconds>` (its own doc says each verdict).
 *
 *  Output: each row as `row<TAB>want<TAB>got<TAB>PASS|FAIL|UNKNOWN` on stdout and in the TSV
 *  file, then the verdict: `PASS` (exit 0) when every row passed, `FAIL: <rows>` (exit 1) when
 *  any failed, else `UNKNOWN: <rows>` (exit 2). It prints no token, cookie or link, and no name
 *  of the machine it runs on: its output can be posted where anyone reads it. The link's token
 *  is withheld from every row even where a browser's error or a page's text names it, as an
 *  error of a page that did not load names the URL it was loading.
 */

// First, before any module that takes `fetch` or `WebSocket`: importing it wraps both, and the
// relay client takes its WebSocket constructor when its module loads (its header says why).
import "./outside-edge-count";
import { randomBytes } from "node:crypto";
import {
  appendFileSync,
  chmodSync,
  existsSync,
  mkdirSync,
  readFileSync,
  writeFileSync,
} from "node:fs";
import { join } from "node:path";
import {
  type DeviceAuthorization,
  type DeviceLogin,
  deviceLoginHost,
  finishDeviceLogin,
  startDeviceLogin,
} from "@legion/cipher-client/device-login";
import { EnvelopeSchema, legacyAgentSubject } from "@legion/contracts";
import { brokerCredential } from "@legion/envoy-client/broker-credential";
import {
  createConsumerDelivery,
  type InboundUnavailable,
  registerWithRelay,
} from "@legion/envoy-client/consumer-delivery";
import { connectListenerRelay, type ListenerRelay } from "@legion/envoy-client/listener-relay";
import { createEnvoyClient } from "@legion/envoy-client/transport";
import { type BrowserContext, chromium, type Page } from "@playwright/test";
import { addVirtualPasskey } from "../virtual-passkey";
import { count, counted } from "./outside-edge-count";
import {
  judgeLimits,
  type Limit,
  LimitsError,
  parseLimits,
  type Row,
  verdictOf,
} from "./outside-edge-limits";

/** The name every device login of this driver gives its machine, in place of this machine's own
 *  host name, which its output must not carry. */
const DEVICE_HOST = deviceLoginHost("outside-edge-e2e");
/** How long one page step waits for what it waits on. */
const PAGE_MS = 30_000;
/** How long the relay socket's first open and the device login's poll may take at most. */
const OPEN_MS = 30_000;
const LOGIN_MS = 5 * 60_000;

/** A run that cannot start: what it could not read, which is UNKNOWN. */
class Refused extends Error {}

/** What a row writes in place of a secret it would otherwise carry. */
const WITHHELD = "<withheld>";

/** Prints and keeps the rows of one run, writing each to `tsv` as it is made. Every field of a
 *  row is written with each of `withheld` replaced by WITHHELD. */
class Rows {
  readonly rows: Row[] = [];
  constructor(
    readonly tsv: string | undefined,
    private readonly withheld: readonly string[] = []
  ) {
    if (tsv !== undefined) writeFileSync(tsv, "", { mode: 0o600 });
  }

  add(made: Row): Row {
    const field = (text: string) =>
      this.withheld
        .reduce((at, secret) => at.replaceAll(secret, WITHHELD), text)
        .replaceAll(/[\t\r\n]+/g, " ");
    const row: Row = {
      row: field(made.row),
      want: field(made.want),
      got: field(made.got),
      verdict: made.verdict,
    };
    const line = [row.row, row.want, row.got, row.verdict].join("\t");
    console.log(line);
    if (this.tsv !== undefined) appendFileSync(this.tsv, `${line}\n`);
    this.rows.push(row);
    return row;
  }

  /** Prints the verdict line and answers the exit code. */
  finish(): number {
    const verdict = verdictOf(this.rows);
    const named = (which: Row["verdict"]) =>
      this.rows
        .filter((row) => row.verdict === which)
        .map((row) => row.row)
        .join(", ");
    if (verdict === "PASS") console.log("PASS");
    else console.log(`${verdict}: ${named(verdict) || "no row was made"}`);
    return verdict === "PASS" ? 0 : verdict === "FAIL" ? 1 : 2;
  }
}

/** Whether `error`, or an error it was caused by, is a request that reached nobody: no
 *  connection, no name, a reset or a timeout. Such a step proved nothing, so it is UNKNOWN; any
 *  answer at all is evidence, so every other error is a FAIL. */
function unreached(error: unknown): boolean {
  for (let at: unknown = error; at instanceof Error; at = at.cause) {
    const code = "code" in at ? at.code : undefined;
    if (
      at.name === "TimeoutError" ||
      code === "ECONNREFUSED" ||
      code === "ConnectionRefused" ||
      code === "ENOTFOUND" ||
      code === "ECONNRESET" ||
      code === "ETIMEDOUT" ||
      code === "FailedToOpenSocket"
    ) {
      return true;
    }
  }
  return false;
}

function messageOf(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

/** `body`'s member `name`, read from an answer this driver did not make: undefined where `body`
 *  is no object. */
function member(body: unknown, name: string): unknown {
  return typeof body === "object" && body !== null ? Reflect.get(body, name) : undefined;
}

/** An origin of an input: https, or under OUTSIDE_EDGE_ALLOW_HTTP=1 http to loopback. */
function originOf(name: string, value: string, allowHttp: boolean): URL {
  let url: URL;
  try {
    url = new URL(value);
  } catch {
    throw new Refused(`${name} is no URL`);
  }
  const loopback = url.hostname === "127.0.0.1" || url.hostname === "localhost";
  if (url.protocol !== "https:" && !(allowHttp && url.protocol === "http:" && loopback)) {
    throw new Refused(
      `${name} must be an https URL${allowHttp ? ", or http to 127.0.0.1 or localhost" : ""}`
    );
  }
  if (url.username !== "" || url.password !== "") {
    throw new Refused(`${name} must carry no user name or password`);
  }
  return url;
}

function required(env: NodeJS.ProcessEnv, name: string): string {
  const value = (env[name] ?? "").trim();
  if (value === "") throw new Refused(`${name} is not set`);
  return value;
}

/** Makes `dir` this run's own state directory, mode 0700. */
function stateDirectory(dir: string): string {
  mkdirSync(dir, { recursive: true, mode: 0o700 });
  chmodSync(dir, 0o700);
  return dir;
}

function keep(state: string, name: string, text: string): void {
  writeFileSync(join(state, name), text, { mode: 0o600 });
  chmodSync(join(state, name), 0o600);
}

function kept(state: string, name: string): string {
  const path = join(state, name);
  if (!existsSync(path))
    throw new Refused(`OUTSIDE_EDGE_STATE holds no ${name}: run the driver first`);
  return readFileSync(path, "utf8").trim();
}

/** What `run` reads. */
interface RunInputs {
  readonly invite: URL;
  readonly origin: string;
  readonly project: string;
  readonly peer: string;
  readonly broker: string;
  readonly listener: string;
  readonly limits: readonly Limit[];
  readonly state: string;
  readonly tailnetCode: string | undefined;
  readonly replySeconds: number;
  readonly tsv: string;
}

const PROJECT_KEY = /^[A-Z][A-Z0-9]{0,31}$/;
const USER_CODE = /^[A-Za-z0-9]{4}-?[A-Za-z0-9]{4}$/;

function runInputs(env: NodeJS.ProcessEnv): RunInputs {
  const allowHttp = env.OUTSIDE_EDGE_ALLOW_HTTP === "1";
  const invite = originOf("OUTSIDE_INVITE_URL", required(env, "OUTSIDE_INVITE_URL"), allowHttp);
  if (invite.pathname !== "/enroll" || invite.hash.length <= 1) {
    throw new Refused("OUTSIDE_INVITE_URL is no enrollment link (<origin>/enroll#<token>)");
  }
  const project = required(env, "OUTSIDE_PROJECT");
  if (!PROJECT_KEY.test(project)) throw new Refused("OUTSIDE_PROJECT is no project key");
  const peer = required(env, "OUTSIDE_PEER_SESSION");
  if (/\s/.test(peer)) throw new Refused("OUTSIDE_PEER_SESSION holds white space");
  const broker = originOf("OUTSIDE_BROKER_URL", required(env, "OUTSIDE_BROKER_URL"), allowHttp);
  const listener = originOf(
    "OUTSIDE_LISTENER_URL",
    required(env, "OUTSIDE_LISTENER_URL"),
    allowHttp
  );
  const limitsFile = required(env, "OUTSIDE_EDGE_LIMITS");
  let limits: Limit[];
  try {
    limits = parseLimits(readFileSync(limitsFile, "utf8"));
  } catch (error) {
    if (error instanceof LimitsError) throw new Refused(`OUTSIDE_EDGE_LIMITS: ${error.message}`);
    throw new Refused(`OUTSIDE_EDGE_LIMITS could not be read: ${messageOf(error)}`);
  }
  const state = stateDirectory(required(env, "OUTSIDE_EDGE_STATE"));
  const tailnetCode = (env.OUTSIDE_TAILNET_USER_CODE ?? "").trim() || undefined;
  if (tailnetCode !== undefined && !USER_CODE.test(tailnetCode)) {
    throw new Refused("OUTSIDE_TAILNET_USER_CODE is no device-login code (XXXX-XXXX)");
  }
  const reply = (env.OUTSIDE_EDGE_REPLY_SECONDS ?? "").trim() || "30";
  const replySeconds = Number(reply);
  if (!/^[0-9]+$/.test(reply) || replySeconds < 1 || replySeconds > 600) {
    throw new Refused("OUTSIDE_EDGE_REPLY_SECONDS must be a whole number of seconds from 1 to 600");
  }
  return {
    invite,
    origin: invite.origin,
    project,
    peer,
    broker: broker.origin,
    listener: listener.origin,
    limits,
    state,
    tailnetCode,
    replySeconds,
    tsv: (env.OUTSIDE_EDGE_TSV ?? "").trim() || join(state, "outside-edge.tsv"),
  };
}

/** Counts every request `context`'s pages make, sockets included. */
function countBrowser(context: BrowserContext): void {
  context.on("request", (request) => {
    if (/^(https?|wss?):/.test(request.url())) count(request.url());
  });
  const sockets = (page: Page) => page.on("websocket", (socket) => count(socket.url()));
  context.on("page", sockets);
  for (const page of context.pages()) sockets(page);
}

/** A JSON read the page's own script makes, from its origin with its cookie, so the browser
 *  counts it as it counts the page's other requests. */
async function pageRead(page: Page, path: string): Promise<{ status: number; body: unknown }> {
  return page.evaluate(async (at) => {
    const response = await fetch(at, { headers: { Accept: "application/json" } });
    const text = await response.text();
    let body: unknown = text;
    try {
      body = JSON.parse(text);
    } catch {
      // Not JSON: the text itself is what a refusal says.
    }
    return { status: response.status, body };
  }, path);
}

/** What the machine-login page answered one code. */
type Approval =
  | { readonly credential: string }
  | { readonly refused: string; readonly unread: boolean };

/**
 * Approves `code` on `origin`'s machine-login page as the browser's signed-in person: it types the
 * code, looks it up, presses Approve, and reads the credential the page then names. A page that
 * did not load is unread; one that loaded and did not approve is a refusal, with what it said.
 */
async function approveOnPage(page: Page, origin: string, code: string): Promise<Approval> {
  try {
    await page.goto(`${origin}/credentials/machine`, { timeout: PAGE_MS });
  } catch (error) {
    return { refused: `the machine-login page did not load: ${messageOf(error)}`, unread: true };
  }
  const box = page.getByLabel("Code shown on the machine");
  try {
    await box.waitFor({ timeout: PAGE_MS });
  } catch {
    return { refused: "the machine-login page shows no code box", unread: false };
  }
  await box.fill(code);
  await page.getByRole("button", { name: "Look up" }).click();
  const approve = page.getByRole("button", { name: "Approve" });
  const alert = page.getByRole("alert");
  try {
    await approve.or(alert).first().waitFor({ timeout: PAGE_MS });
  } catch {
    return { refused: "the page showed neither Approve nor a refusal for the code", unread: false };
  }
  if (!(await approve.isVisible())) {
    return {
      refused: `the page refused the code: ${(await alert.first().innerText()).trim()}`,
      unread: false,
    };
  }
  await approve.click();
  const approved = page.getByText(/^Approved\. /);
  try {
    await approved.or(alert).first().waitFor({ timeout: PAGE_MS });
  } catch {
    return { refused: "the page did not say the code was approved", unread: false };
  }
  if (!(await approved.isVisible())) {
    return {
      refused: `the approval was refused: ${(await alert.first().innerText()).trim()}`,
      unread: false,
    };
  }
  const named = await page
    .getByText(/^Credential \S+$/)
    .first()
    .innerText()
    .catch(() => "");
  const credential = /^Credential (\S+)$/.exec(named.trim())?.[1];
  if (credential === undefined) {
    return { refused: "the page approved the code and named no credential", unread: false };
  }
  return { credential };
}

/** Records an approved code and its credential, for `revoke`. */
function recordApproval(state: string, code: string, credential: string): void {
  appendFileSync(join(state, "approved.tsv"), `${code}\t${credential}\n`, { mode: 0o600 });
  chmodSync(join(state, "approved.tsv"), 0o600);
}

/** Step 1: the link enrolls a passkey, and the person's list is exactly the project. Answers the
 *  signed-in person, or undefined when the step did not pass. */
async function signIn(page: Page, context: BrowserContext, inputs: RunInputs, rows: Rows) {
  const want = `the link enrolls a passkey and signs in at ${inputs.origin}`;
  const passkey = await addVirtualPasskey(page);
  try {
    await page.goto(inputs.invite.href, { timeout: PAGE_MS });
  } catch (error) {
    rows.add({
      row: "sign-in",
      want,
      got: `the enrollment page did not load: ${messageOf(error)}`,
      verdict: "UNKNOWN",
    });
    return undefined;
  }
  const create = page.getByRole("button", { name: "Create a passkey" });
  const alert = page.getByRole("alert");
  try {
    await create.or(alert).first().waitFor({ timeout: PAGE_MS });
  } catch {
    rows.add({
      row: "sign-in",
      want,
      got: "no enrollment: the page shows no Create a passkey button",
      verdict: "FAIL",
    });
    return undefined;
  }
  if (!(await create.isVisible())) {
    rows.add({
      row: "sign-in",
      want,
      got: `the page refused the link: ${(await alert.first().innerText()).trim()}`,
      verdict: "FAIL",
    });
    return undefined;
  }
  await create.click();
  const signedIn = page.getByText(/^Signed in as /);
  try {
    await signedIn.or(alert).first().waitFor({ timeout: PAGE_MS });
  } catch {
    rows.add({
      row: "sign-in",
      want,
      got: "the passkey enrolled no sign-in within 30 s",
      verdict: "FAIL",
    });
    return undefined;
  }
  if (!(await signedIn.isVisible())) {
    rows.add({
      row: "sign-in",
      want,
      got: `the enrollment was refused: ${(await alert.first().innerText()).trim()}`,
      verdict: "FAIL",
    });
    return undefined;
  }
  // The passkey itself, kept so `t5` can try it again once the person is off the list: a virtual
  // authenticator lives only as long as its page, and the browser's saved state holds no key.
  const { credentials } = await passkey.cdp.send("WebAuthn.getCredentials", {
    authenticatorId: passkey.authenticatorId,
  });
  if (credentials.length !== 1) {
    rows.add({
      row: "sign-in",
      want,
      got: `the authenticator holds ${credentials.length} credentials after the enrollment, not 1`,
      verdict: "FAIL",
    });
    return undefined;
  }
  keep(inputs.state, "passkey.json", JSON.stringify(credentials[0]));
  const whoami = await pageRead(page, "/auth/whoami");
  const login = member(whoami.body, "login");
  if (whoami.status !== 200 || typeof login !== "string" || login === "") {
    rows.add({
      row: "sign-in",
      want,
      got: `GET /auth/whoami answered ${whoami.status} with no login`,
      verdict: "FAIL",
    });
    return undefined;
  }
  rows.add({
    row: "sign-in",
    want,
    got: `signed in at ${new URL(page.url()).origin} as the invited person`,
    verdict: "PASS",
  });

  const projects = await pageRead(page, "/api/v1/projects");
  const keys = Array.isArray(projects.body)
    ? projects.body.map((one) => String(member(one, "key"))).sort()
    : undefined;
  const wantProjects = `GET /api/v1/projects lists exactly [${inputs.project}]`;
  if (projects.status !== 200 || keys === undefined) {
    rows.add({
      row: "projects",
      want: wantProjects,
      got: `answered ${projects.status}, no list`,
      verdict: "FAIL",
    });
  } else {
    const extra = keys.filter((key) => key !== inputs.project);
    const missing = keys.includes(inputs.project) ? [] : [inputs.project];
    const got =
      `[${keys.join(", ")}]` +
      (extra.length > 0 ? `; listed beside it: ${extra.join(", ")}` : "") +
      (missing.length > 0 ? `; missing: ${missing.join(", ")}` : "");
    rows.add({
      row: "projects",
      want: wantProjects,
      got,
      verdict: extra.length + missing.length === 0 ? "PASS" : "FAIL",
    });
  }

  // What `approve`, `revoke` and a later step read, each this run's own and mode 0600.
  await context.storageState({ path: join(inputs.state, "browser.json") });
  chmodSync(join(inputs.state, "browser.json"), 0o600);
  const cookies = await context.cookies(inputs.origin);
  keep(
    inputs.state,
    "cookie",
    cookies.map((cookie) => `${cookie.name}=${cookie.value}`).join("; ")
  );
  keep(inputs.state, "person", login);
  keep(inputs.state, "origin", inputs.origin);
  return login;
}

/** Step 2: a device login of this machine, approved on the page; then the tailnet code. Answers
 *  the login, or undefined when it was not made. */
async function deviceLogin(
  page: Page,
  person: string,
  inputs: RunInputs,
  rows: Rows
): Promise<DeviceLogin | undefined> {
  const want = `a device login at ${inputs.broker}, approved on ${inputs.origin}, answers a DPoP-bound pair`;
  let started: DeviceAuthorization;
  try {
    started = await startDeviceLogin({ brokerUrl: inputs.broker, person, host: DEVICE_HOST });
  } catch (error) {
    rows.add({
      row: "device-login",
      want,
      got: messageOf(error),
      verdict: unreached(error) ? "UNKNOWN" : "FAIL",
    });
    return undefined;
  }
  const page_ = new URL(started.verificationUri);
  if (page_.origin !== inputs.origin) {
    rows.add({
      row: "device-login",
      want,
      got: `the broker named its page on ${page_.origin}`,
      verdict: "FAIL",
    });
    return undefined;
  }
  const stop = new AbortController();
  const deadline = setTimeout(
    () => stop.abort(new Error("the poll got no approval within 5 minutes")),
    LOGIN_MS
  );
  const polled = finishDeviceLogin(started, {
    path: join(inputs.state, "device-login.json"),
    signal: stop.signal,
  });
  // A failure of the poll is read below, after the page; it must not go unhandled meanwhile.
  polled.catch(() => undefined);
  const approval = await approveOnPage(page, inputs.origin, started.userCode);
  if (!("credential" in approval)) {
    stop.abort(new Error("the page did not approve the code"));
    clearTimeout(deadline);
    rows.add({
      row: "device-login",
      want,
      got: approval.refused,
      verdict: approval.unread ? "UNKNOWN" : "FAIL",
    });
    return undefined;
  }
  recordApproval(inputs.state, started.userCode, approval.credential);
  let login: DeviceLogin;
  try {
    login = await polled;
  } catch (error) {
    rows.add({
      row: "device-login",
      want,
      got: `approved on the page, then the poll: ${messageOf(error)}`,
      verdict: unreached(error) ? "UNKNOWN" : "FAIL",
    });
    return undefined;
  } finally {
    clearTimeout(deadline);
  }
  rows.add({
    row: "device-login",
    want,
    got: `approved on the page as credential ${approval.credential}; the poll answered a DPoP-bound access token and a refresh token`,
    verdict: "PASS",
  });
  return login;
}

/** The tailnet leg's code, approved on the same page in the same browser session and recorded, so
 *  `revoke` ends its login. */
async function tailnetCode(page: Page, code: string, inputs: RunInputs, rows: Rows): Promise<void> {
  const row = "tailnet-code";
  const want = `APPROVED ${code} on ${inputs.origin}'s machine-login page`;
  const approval = await approveOnPage(page, inputs.origin, code);
  if ("credential" in approval) {
    recordApproval(inputs.state, code, approval.credential);
    console.log(`APPROVED ${code}`);
    rows.add({
      row,
      want,
      got: `APPROVED ${code} as credential ${approval.credential}`,
      verdict: "PASS",
    });
    return;
  }
  rows.add({
    row,
    want,
    got: `${code} was not approved: ${approval.refused}`,
    verdict: approval.unread ? "UNKNOWN" : "FAIL",
  });
}

/** Step 3: a message to the peer through the listener, and its reply on the relay socket. */
async function relay(inputs: RunInputs, rows: Rows): Promise<void> {
  const nonce = randomBytes(8).toString("hex");
  const want = `outside-e2e pong ${nonce} from ${inputs.peer} on the relay socket at ${inputs.listener} within ${inputs.replySeconds} s`;
  const session = `outside-e2e-${randomBytes(6).toString("hex")}`;
  const credential = brokerCredential(
    {
      AGENT_SECRETS_URL: inputs.broker,
      AGENT_SECRETS_MACHINE_IDENTITY: "device-login",
      AGENT_SECRETS_DEVICE_LOGIN_FILE: join(inputs.state, "device-login.json"),
      AGENT_SECRETS_PROJECTS: inputs.project,
    },
    session
  );
  const log = (line: string) => console.error(`outside-edge: ${line}`);
  // The run's own outage report: each refusal logged, on stderr, as it comes. A session host
  // tells its person once per outage (`inboundUnavailable`); a one-shot run has nobody to spare.
  const outage: InboundUnavailable = {
    refused: (error) =>
      log(`the relay socket or the registration was refused: ${messageOf(error)}`),
    accepted: () => undefined,
  };
  let socket: ListenerRelay;
  try {
    socket = await connectListenerRelay(
      { url: inputs.listener, session, credential, outage, log },
      AbortSignal.timeout(OPEN_MS)
    );
  } catch (error) {
    rows.add({
      row: "relay",
      want,
      got: `the relay socket did not open: ${messageOf(error)}`,
      verdict: unreached(error) ? "UNKNOWN" : "FAIL",
    });
    return;
  }
  const reply = Promise.withResolvers<string>();
  const encoder = new TextEncoder();
  const client = createEnvoyClient({
    baseUrl: inputs.listener,
    fetch: globalThis.fetch,
    credential,
  });
  const delivery = createConsumerDelivery({
    sessionID: session,
    ledger: { delivered: () => false, liveFailed: async () => false },
    persist: () => true,
    publish: (subject, body) => socket.publish(subject, encoder.encode(body)),
    host: {
      deliver(frame) {
        const envelope = EnvelopeSchema.safeParse(JSON.parse(frame.data));
        if (envelope.success) {
          const from = envelope.data.source_session ?? envelope.data.sender?.session_id;
          const text = (envelope.data.payload ?? envelope.data.payload_summary).trim();
          // The peer's answers: each its word and the nonce, and ` at <Unix ms>` after them.
          const says = (word: string) =>
            text === `outside-e2e ${word} ${nonce}` ||
            text.startsWith(`outside-e2e ${word} ${nonce} `);
          if (from === inputs.peer && says("pong")) reply.resolve(text);
          // The peer then asks too, as it asks the public leg's session; it is answered so it
          // does not wait, and the row stays the pong's.
          if (from === inputs.peer && says("hello")) {
            client
              .send({
                sourceSessionID: session,
                targetSessionID: inputs.peer,
                message: `outside-e2e reply ${nonce} at ${Date.now()}`,
              })
              .catch((error: unknown) =>
                log(`the reply to the peer's hello failed: ${messageOf(error)}`)
              );
          }
        }
        frame.done();
      },
      gap: (notice) => log(notice.message),
      log: (message, error) =>
        log(error === undefined ? message : `${message}: ${messageOf(error)}`),
    },
  });
  try {
    const subscription = socket.subscribe(delivery.subject);
    void (async () => {
      for await (const message of subscription) delivery.receive(message);
    })();
    await socket.flush();
    await registerWithRelay({
      relay: delivery,
      ownAgentSubject: legacyAgentSubject(session),
      followed: [],
      outage,
      send: (topics, request) =>
        client.subscribe({
          sessionID: session,
          directory: "/outside-edge-e2e",
          topics,
          port: 0,
          title: "outside-edge-e2e",
          driving: false,
          delivery: request,
          plugin: { name: "outside-edge-e2e", version: "1" },
        }),
      answered: (result) => delivery.answered(result.delivery),
      // The session follows no topic, so a project refusal has nothing of it to drop: one that
      // names only its own agent subject fails the registration, which is this row's FAIL.
      drop: () => undefined,
      log,
    });
    await client.send({
      sourceSessionID: session,
      targetSessionID: inputs.peer,
      message: `outside-e2e ping ${nonce} at ${Date.now()}`,
    });
  } catch (error) {
    rows.add({
      row: "relay",
      want,
      got: `the session could not register or send: ${messageOf(error)}`,
      verdict: unreached(error) ? "UNKNOWN" : "FAIL",
    });
    await close(delivery, socket, client, session);
    return;
  }
  const late = setTimeout(
    () => reply.reject(new Error(`no reply from ${inputs.peer} within ${inputs.replySeconds} s`)),
    inputs.replySeconds * 1000
  );
  try {
    await reply.promise;
    rows.add({
      row: "relay",
      want,
      got: `outside-e2e pong ${nonce} from ${inputs.peer} on the relay socket`,
      verdict: "PASS",
    });
  } catch (error) {
    rows.add({ row: "relay", want, got: messageOf(error), verdict: "FAIL" });
  } finally {
    clearTimeout(late);
    await close(delivery, socket, client, session);
  }
}

/** Leaves the listener: the registration, the delivery and the socket. */
async function close(
  delivery: { close(): void },
  socket: { close(): Promise<void> },
  client: { unregisterSession(input: { sessionID: string; timeoutMs?: number }): Promise<void> },
  session: string
): Promise<void> {
  delivery.close();
  await client.unregisterSession({ sessionID: session, timeoutMs: 5_000 }).catch(() => undefined);
  await socket.close();
}

async function run(env: NodeJS.ProcessEnv): Promise<number> {
  let inputs: RunInputs;
  try {
    inputs = runInputs(env);
  } catch (error) {
    if (!(error instanceof Refused)) throw error;
    const rows = new Rows(undefined);
    rows.add({
      row: "inputs",
      want: "every input of the driver",
      got: error.message,
      verdict: "UNKNOWN",
    });
    return rows.finish();
  }
  // The link's token, as the URL holds it and decoded, which no row carries: a page that did not
  // load fails with an error naming the URL it was loading.
  const token = inputs.invite.hash.slice(1);
  let decoded = token;
  try {
    decoded = decodeURIComponent(token);
  } catch {
    // A malformed escape: the token is withheld as the URL holds it.
  }
  const rows = new Rows(inputs.tsv, [token, decoded]);
  console.log(`outside-edge: origin ${inputs.origin}`);
  console.log(`outside-edge: time ${new Date().toISOString()}`);
  const browser = await chromium.launch();
  try {
    const context = await browser.newContext();
    countBrowser(context);
    const page = await context.newPage();
    const person = await signIn(page, context, inputs, rows);
    const notRun = (row: string, want: string, why: string) =>
      rows.add({ row, want, got: `not run: ${why}`, verdict: "UNKNOWN" });
    if (person === undefined) {
      notRun("device-login", "a device login approved on the page", "the sign-in did not pass");
      if (inputs.tailnetCode !== undefined)
        notRun("tailnet-code", `APPROVED ${inputs.tailnetCode}`, "the sign-in did not pass");
      notRun("relay", "a reply from the peer on the relay socket", "the sign-in did not pass");
    } else {
      const login = await deviceLogin(page, person, inputs, rows);
      if (inputs.tailnetCode !== undefined)
        await tailnetCode(page, inputs.tailnetCode, inputs, rows);
      if (login === undefined)
        notRun(
          "relay",
          "a reply from the peer on the relay socket",
          "the device login did not pass"
        );
      else await relay(inputs, rows);
    }
  } catch (error) {
    // An error no step read, such as a page closed under a step: the run judged nothing more, and
    // its text goes through the rows, which withhold the token.
    rows.add({
      row: "stopped",
      want: "every step judged",
      got: `the run stopped: ${messageOf(error)}`,
      verdict: "UNKNOWN",
    });
  } finally {
    await browser.close();
  }
  const judged = judgeLimits(inputs.limits, counted);
  for (const { host, perMinute } of judged.hostPeaks) {
    console.log(`outside-edge: peak ${host} ${perMinute} requests in 60 s`);
  }
  for (const row of judged.rows) rows.add(row);
  return rows.finish();
}

/** `approve <code>`: one more code, approved in the kept browser state. */
async function approve(env: NodeJS.ProcessEnv, code: string | undefined): Promise<number> {
  if (code === undefined || !USER_CODE.test(code)) {
    console.log(`UNKNOWN: ${code ?? "(none)"} is no device-login code (XXXX-XXXX)`);
    return 2;
  }
  let state: string;
  let origin: string;
  try {
    state = required(env, "OUTSIDE_EDGE_STATE");
    origin = kept(state, "origin");
    kept(state, "person");
  } catch (error) {
    if (!(error instanceof Refused)) throw error;
    console.log(`UNKNOWN: ${code} not approved: ${error.message}`);
    return 2;
  }
  const browser = await chromium.launch();
  try {
    const context = await browser.newContext({ storageState: join(state, "browser.json") });
    const approval = await approveOnPage(await context.newPage(), origin, code);
    if ("credential" in approval) {
      recordApproval(state, code, approval.credential);
      console.log(`APPROVED ${code}`);
      return 0;
    }
    console.log(
      `${approval.unread ? "UNKNOWN" : "FAIL"}: ${code} not approved: ${approval.refused}`
    );
    return approval.unread ? 2 : 1;
  } finally {
    await browser.close();
  }
}

/** `revoke`: every credential the run approved, ended on the person's own list. */
async function revoke(env: NodeJS.ProcessEnv): Promise<number> {
  let state: string;
  let origin: string;
  let cookie: string;
  let person: string;
  let approvals: string[][];
  try {
    state = required(env, "OUTSIDE_EDGE_STATE");
    origin = kept(state, "origin");
    cookie = kept(state, "cookie");
    person = kept(state, "person");
    const keep = (env.OUTSIDE_EDGE_REVOKE_KEEP ?? "").trim();
    if (keep !== "" && !USER_CODE.test(keep)) {
      throw new Refused("OUTSIDE_EDGE_REVOKE_KEEP is no device-login code (XXXX-XXXX)");
    }
    approvals = kept(state, "approved.tsv")
      .split("\n")
      .filter((line) => line !== "")
      .map((line) => line.split("\t"))
      .filter(([code]) => code !== keep);
  } catch (error) {
    if (!(error instanceof Refused)) throw error;
    const rows = new Rows(undefined);
    rows.add({
      row: "revoke",
      want: "the run's approved credentials",
      got: error.message,
      verdict: "UNKNOWN",
    });
    return rows.finish();
  }
  const rows = new Rows(
    (env.OUTSIDE_EDGE_REVOCATIONS ?? "").trim() || join(state, "outside-edge-revocations.tsv")
  );
  const headers = { Accept: "application/json", Cookie: cookie, Origin: origin };
  /** The person's machine-logins list: the credential ids it holds, or why it gave none, UNKNOWN
   *  where the request reached nobody (`unreached`) and FAIL where it was answered otherwise. */
  const readList = async (): Promise<
    | { readonly listed: readonly string[] }
    | { readonly why: string; readonly verdict: "FAIL" | "UNKNOWN" }
  > => {
    try {
      const answer = await fetch(`${origin}/api/v1/machine-logins`, {
        headers,
        signal: AbortSignal.timeout(PAGE_MS),
      });
      const credentials = member(await answer.json().catch(() => undefined), "credentials");
      if (answer.ok && Array.isArray(credentials)) {
        return { listed: credentials.map((one) => String(member(one, "credential_id"))) };
      }
      return { why: `GET /api/v1/machine-logins answered ${answer.status}`, verdict: "FAIL" };
    } catch (error) {
      return {
        why: `GET /api/v1/machine-logins got no answer: ${messageOf(error)}`,
        verdict: unreached(error) ? "UNKNOWN" : "FAIL",
      };
    }
  };
  const before = await readList();
  for (const [code = "", credential = ""] of approvals) {
    const row = `revoke-${credential}`;
    const want = `the login of ${code} is on the person's machine-logins list and is revoked there`;
    if (!("listed" in before)) {
      rows.add({ row, want, got: before.why, verdict: before.verdict });
      continue;
    }
    if (!before.listed.includes(credential)) {
      rows.add({
        row,
        want,
        got: "the person's machine-logins list does not hold it",
        verdict: "FAIL",
      });
      continue;
    }
    let status: number;
    try {
      const answer = await fetch(
        `${origin}/api/v1/machine-logins/${encodeURIComponent(credential)}/revoke`,
        {
          method: "POST",
          headers: { ...headers, "Content-Type": "application/json" },
          body: JSON.stringify({ approver: person }),
          signal: AbortSignal.timeout(PAGE_MS),
        }
      );
      status = answer.status;
      if (!answer.ok) {
        rows.add({ row, want, got: `listed; the revoke answered ${status}`, verdict: "FAIL" });
        continue;
      }
    } catch (error) {
      rows.add({
        row,
        want,
        got: `listed; the revoke got no answer: ${messageOf(error)}`,
        verdict: "UNKNOWN",
      });
      continue;
    }
    // A 2xx is the route's word alone: the list, read again, is what shows the login ended.
    const after = await readList();
    if (!("listed" in after)) {
      rows.add({
        row,
        want,
        got: `listed; the revoke answered ${status}, then the list read again: ${after.why}`,
        verdict: after.verdict,
      });
    } else if (after.listed.includes(credential)) {
      rows.add({
        row,
        want,
        got: `listed; the revoke answered ${status}, and the list read again still holds it`,
        verdict: "FAIL",
      });
    } else {
      rows.add({
        row,
        want,
        got: `listed; the revoke answered ${status}, and the list read again no longer holds it`,
        verdict: "PASS",
      });
    }
  }
  return rows.finish();
}

/** The credential `run` kept in passkey.json: Chromium's virtual authenticator's own record of
 *  it, which `WebAuthn.addCredential` takes back. */
interface StoredPasskey {
  readonly credentialId: string;
  readonly isResidentCredential: boolean;
  readonly rpId: string;
  readonly privateKey: string;
  readonly userHandle: string;
  readonly signCount: number;
}

/** passkey.json read back, refused when it is not the record `run` wrote. */
function storedPasskey(text: string): StoredPasskey {
  let body: unknown;
  try {
    body = JSON.parse(text);
  } catch {
    throw new Refused("OUTSIDE_EDGE_STATE's passkey.json is no JSON");
  }
  const field = (name: string) => {
    const value = member(body, name);
    if (typeof value !== "string" || value === "") {
      throw new Refused(`OUTSIDE_EDGE_STATE's passkey.json has no ${name}`);
    }
    return value;
  };
  const signCount = member(body, "signCount");
  if (typeof signCount !== "number")
    throw new Refused("OUTSIDE_EDGE_STATE's passkey.json has no signCount");
  return {
    credentialId: field("credentialId"),
    isResidentCredential: member(body, "isResidentCredential") === true,
    rpId: field("rpId"),
    privateKey: field("privateKey"),
    userHandle: field("userHandle"),
    signCount,
  };
}

/** The passkey sign-in's last request. Its answer, and not the page's alert, says whether the
 *  passkey itself was refused: the page alerts on every failure alike. */
const PASSKEY_FINISH = "/auth/passkey/login/finish";

/** The answers of `POST /auth/passkey/login/finish` that refuse the passkey because its person is
 *  gone, each code with its status, as Dispatch writes them (`passkeyRefusals` and
 *  `writePasskeyRefusal` of packages/envoy/internal/dispatch/routes/passkey.go): 401
 *  PASSKEY_UNKNOWN, the passkey deleted with the person's listing, and 403 NOT_LISTED, no access
 *  list names the person any more. Any other answer proves no such refusal: a 404 is the edge's
 *  own deny too, a 429 its rate limit, a 5xx or MEMBERSHIP_UNREADABLE trouble in Dispatch. */
const PASSKEY_REFUSED: Readonly<Record<string, number>> = {
  PASSKEY_UNKNOWN: 401,
  NOT_LISTED: 403,
};

/**
 * `t5`: the browser's half of the public leg's delisting window. It reads the person's cookie,
 * origin and passkey from OUTSIDE_EDGE_STATE, as `run` kept them, and until OUTSIDE_T5_DEADLINE
 * (Unix seconds) asks `GET /auth/whoami` with that cookie at most every 5 s. Once it is answered
 * 401, it tries the passkey sign-in in a fresh browser at the same origin, with the same
 * credential. Two rows go to OUTSIDE_T5_TSV and to stdout, each `got` naming `at=<Unix seconds>`:
 * `T5-cookie`, PASS on the 401 and UNKNOWN when the deadline came first; and `T5-passkey`, PASS
 * only when `POST /auth/passkey/login/finish` answered one of PASSKEY_REFUSED, FAIL when the
 * page signed the person in again or that request answered 2xx, and UNKNOWN otherwise: not tried,
 * an alert with no answer to that request, or any other answer.
 */
async function t5(env: NodeJS.ProcessEnv): Promise<number> {
  const tsv = (env.OUTSIDE_T5_TSV ?? "").trim();
  const rows = new Rows(tsv === "" ? undefined : tsv);
  const wantCookie = "the test person's cookie is answered 401 before the deadline";
  const wantPasskey = "the passkey sign-in is refused after the cookie's 401";
  let origin: string;
  let cookie: string;
  let credential: StoredPasskey;
  let deadlineMs: number;
  try {
    if (tsv === "") throw new Refused("OUTSIDE_T5_TSV is not set");
    const deadline = (env.OUTSIDE_T5_DEADLINE ?? "").trim();
    if (!/^[0-9]+$/.test(deadline))
      throw new Refused("OUTSIDE_T5_DEADLINE is no Unix time in seconds");
    deadlineMs = Number(deadline) * 1000;
    const state = required(env, "OUTSIDE_EDGE_STATE");
    origin = kept(state, "origin");
    cookie = kept(state, "cookie");
    credential = storedPasskey(kept(state, "passkey.json"));
  } catch (error) {
    if (!(error instanceof Refused)) throw error;
    rows.add({ row: "T5-cookie", want: wantCookie, got: error.message, verdict: "UNKNOWN" });
    rows.add({
      row: "T5-passkey",
      want: wantPasskey,
      got: `not tried: ${error.message}`,
      verdict: "UNKNOWN",
    });
    return rows.finish();
  }
  const seconds = () => Math.floor(Date.now() / 1000);
  let refusedAt: number | undefined;
  let last = "no answer yet";
  while (Date.now() < deadlineMs) {
    try {
      const answer = await fetch(`${origin}/auth/whoami`, {
        headers: { Accept: "application/json", Cookie: cookie },
        signal: AbortSignal.timeout(Math.max(1_000, Math.min(10_000, deadlineMs - Date.now()))),
      });
      await answer.body?.cancel();
      last = `${answer.status}`;
      if (answer.status === 401) {
        refusedAt = seconds();
        break;
      }
    } catch (error) {
      last = `no answer (${messageOf(error)})`;
    }
    await Bun.sleep(Math.max(0, Math.min(5_000, deadlineMs - Date.now())));
  }
  if (refusedAt === undefined) {
    rows.add({
      row: "T5-cookie",
      want: wantCookie,
      got: `still ${last} at the deadline`,
      verdict: "UNKNOWN",
    });
    rows.add({
      row: "T5-passkey",
      want: wantPasskey,
      got: "not tried: the cookie was not answered 401 before the deadline",
      verdict: "UNKNOWN",
    });
    return rows.finish();
  }
  rows.add({ row: "T5-cookie", want: wantCookie, got: `401 at=${refusedAt}`, verdict: "PASS" });
  // Both rows are written before the deadline, where the window reads them: each wait of the
  // sign-in ends 2 s before it, and with under 10 s left the sign-in is not tried.
  const left = () => deadlineMs - Date.now() - 2_000;
  if (left() < 8_000) {
    rows.add({
      row: "T5-passkey",
      want: wantPasskey,
      got: `not tried: the 401 came ${Math.round((deadlineMs - Date.now()) / 1000)} s before the deadline`,
      verdict: "UNKNOWN",
    });
    return rows.finish();
  }
  const browser = await chromium.launch();
  try {
    const page = await (await browser.newContext()).newPage();
    const passkey = await addVirtualPasskey(page);
    await passkey.cdp.send("WebAuthn.addCredential", {
      authenticatorId: passkey.authenticatorId,
      credential,
    });
    // Each answer of the sign-in's last request, read as it comes: the page shows its alert only
    // after it, so one the page asked is in by the time the alert is read.
    const finishes: Promise<{ readonly status: number; readonly code: unknown }>[] = [];
    page.on("response", (response) => {
      if (response.request().method() !== "POST") return;
      if (new URL(response.url()).pathname !== PASSKEY_FINISH) return;
      finishes.push(
        response.json().then(
          (body: unknown) => ({ status: response.status(), code: member(body, "code") }),
          () => ({ status: response.status(), code: undefined })
        )
      );
    });
    const bound = () => Math.max(1_000, Math.min(PAGE_MS, left()));
    await page.goto(`${origin}/`, { timeout: bound() });
    const button = page.getByRole("button", { name: "Sign in with a passkey" });
    await button.waitFor({ timeout: bound() });
    await button.click();
    const signedIn = page.getByText(/^Signed in as /);
    const alert = page.getByRole("alert");
    await signedIn.or(alert).first().waitFor({ timeout: bound() });
    const at = seconds();
    const said = (await signedIn.isVisible())
      ? undefined
      : (await alert.first().innerText()).trim();
    const answer = (await Promise.all(finishes)).at(-1);
    const answered =
      answer === undefined
        ? ""
        : `POST ${PASSKEY_FINISH} answered ${answer.status} ${typeof answer.code === "string" ? answer.code : "with no code"}`;
    if (said === undefined) {
      rows.add({
        row: "T5-passkey",
        want: wantPasskey,
        got: `the passkey signed the person in again at=${at}, after the cookie's 401`,
        verdict: "FAIL",
      });
    } else if (answer === undefined) {
      rows.add({
        row: "T5-passkey",
        want: wantPasskey,
        got: `no answer to POST ${PASSKEY_FINISH} at=${at}, so no refusal of the passkey was read; the page said: ${said}`,
        verdict: "UNKNOWN",
      });
    } else if (answer.status >= 200 && answer.status < 300) {
      rows.add({
        row: "T5-passkey",
        want: wantPasskey,
        got: `${answered} at=${at}, a sign-in after the cookie's 401; the page said: ${said}`,
        verdict: "FAIL",
      });
    } else if (
      typeof answer.code === "string" &&
      Object.hasOwn(PASSKEY_REFUSED, answer.code) &&
      PASSKEY_REFUSED[answer.code] === answer.status
    ) {
      rows.add({
        row: "T5-passkey",
        want: wantPasskey,
        got: `refused at=${at}: ${answered}; the page said: ${said}`,
        verdict: "PASS",
      });
    } else {
      rows.add({
        row: "T5-passkey",
        want: wantPasskey,
        got: `${answered} at=${at}, which is no refusal of the passkey itself; the page said: ${said}`,
        verdict: "UNKNOWN",
      });
    }
  } catch (error) {
    rows.add({
      row: "T5-passkey",
      want: wantPasskey,
      got: `the sign-in showed neither a sign-in nor a refusal: ${messageOf(error)}`,
      verdict: "UNKNOWN",
    });
  } finally {
    await browser.close();
  }
  return rows.finish();
}

if (import.meta.main) {
  const [command = "run", argument] = Bun.argv.slice(2);
  const env = process.env;
  let code: number;
  switch (command) {
    case "run":
      code = await run(env);
      break;
    case "approve":
      code = await approve(env, argument);
      break;
    case "revoke":
      code = await revoke(env);
      break;
    case "t5":
      code = await t5(env);
      break;
    default:
      console.log(
        `UNKNOWN: usage: bun outside-edge.ts [run | approve <code> | revoke | t5], not ${command}`
      );
      code = 2;
  }
  process.exit(code);
}
