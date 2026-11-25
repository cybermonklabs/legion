/** The outside end-to-end driver: what works for a person from outside, through the edge that
 *  serves them, on the surface they use. It runs where they would, on a machine of the public
 *  internet, and drives the real pages in a real browser (Chromium, through Playwright), the real
 *  broker client and the real relay client. Run it from packages/dispatch with
 *  `bun e2e/acceptance/outside-edge.ts [run | approve <code> | revoke | grants | t5 | hold | release]`.
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
 *                 `device-login`): a broker nothing answers at the start, or whose last poll got
 *                 no answer at the 5-minute deadline (it answered none, or answered and then fell
 *                 silent), is UNKNOWN, read by the code the broker client keeps of a request that
 *                 got no answer (refused, a name that did not resolve, its deadline passed); a
 *                 refusal, expiry or denial it answered is FAIL, and so is a broker that answered
 *                 every poll to the deadline with no approval; and an approved login the driver
 *                 could not keep in OUTSIDE_EDGE_STATE's `device-login.json` is UNKNOWN, an error
 *                 of its own, with any copy of the login the broker client kept beside that file
 *                 removed. Each approved code's credential is printed on stderr
 *                 (`outside-edge: <code> approved as credential <id>`) before it is written
 *                 down, so one the driver could not record is still named, for a person to end.
 *                 With OUTSIDE_TAILNET_USER_CODE set, the same page then approves that code too,
 *                 in the same browser session, and prints `APPROVED <code>` (row
 *                 `tailnet-code`), else fails naming the code.
 *    3. relay     the device login is traded for the session's broker credential, the session
 *                 opens its relay socket at OUTSIDE_LISTENER_URL and registers there
 *                 (`@legion/envoy-client/listener-relay`, `relay-registration`), keeps the session
 *                 it registered in OUTSIDE_EDGE_STATE (`session`), and sends
 *                 `outside-e2e ping <nonce> at <Unix ms>` to OUTSIDE_PEER_SESSION; the peer's
 *                 reply, `outside-e2e pong <nonce> at <Unix ms>`, must reach the session on the
 *                 socket within OUTSIDE_EDGE_REPLY_SECONDS (row `relay`), else the row fails
 *                 naming the peer. A listener nothing answers, a trade for the session's token
 *                 the broker did not answer, a socket that did not open within 30 s, its open
 *                 still waiting then, or a call that got no answer, refused, unresolved or
 *                 unanswered in time (the code the relay client, the broker client and the Envoy
 *                 client keep of the failure), is UNKNOWN, and so is a session the listener took
 *                 that the driver could not keep in OUTSIDE_EDGE_STATE, an error of its own; an
 *                 answer short of the pong, a refused upgrade, trade or registration among them,
 *                 is FAIL, and so is a call whose status came and whose body did not (the
 *                 listener answered it) and a socket the listener opened and then dropped
 *                 before its flush was answered, since the listener did answer it. The peer's
 *                 `outside-e2e hello <nonce> …` that follows is
 *                 answered `outside-e2e reply <nonce> at <Unix ms>`, so the peer does not wait.
 *                 The session's unregister at the end is printed with its answer
 *                 (`outside-edge: session <session>: unregistered`, or why not).
 *    4. limits    every request of the run, the browser's and this process's, is counted
 *                 (`outside-edge-count.ts`) and judged against the edge's rate rules in
 *                 OUTSIDE_EDGE_LIMITS (`outside-edge-limits.ts` says the file and the rule): a
 *                 row per threshold, which fails above half of it, naming the host; and a line per
 *                 host with its peak per minute. A threshold of a host the run asked nothing, or
 *                 one a run that did not reach every step stayed under, is UNKNOWN, not PASS.
 *  A step whose step before it failed is UNKNOWN, never run on nothing. Step 1 passes only where
 *  the passkey signed the person in at the link's origin, made one passkey, set a cookie there and
 *  named the person (`GET /auth/whoami`); a page or a read that gets no answer within 30 s is
 *  UNKNOWN, and so is a page that holds its main thread past the driver's own bound on a read
 *  through it (35 s) or on the save of the browser's state (30 s), naming what it held, since
 *  Playwright waits on neither with a deadline of its own; any other answer short of that is
 *  FAIL, a read whose status came and whose body did not among them, since the far end answered.
 *  A page or a browser that closes while any step waits on it is UNKNOWN, the driver's own
 *  failure, never a page that showed nothing.
 *
 *  With OUTSIDE_EDGE_STEPS=sign-in, `run` makes step 1 and step 4 alone, for an edge that serves
 *  the outside origin and no broker or listener yet: it reads no OUTSIDE_PEER_SESSION,
 *  OUTSIDE_BROKER_URL or OUTSIDE_LISTENER_URL, stops after the `projects` row, and judges only
 *  the rules of OUTSIDE_EDGE_LIMITS whose host is the link's, the one host those steps ask. It
 *  makes no `device-login` or `relay` row and no row of another host's rule: each is printed as
 *  `outside-edge: not run: <row>: <why>`, a line of no row, so none reads PASS and none holds the
 *  verdict back. A sign-in that did not pass makes `projects` UNKNOWN, not run. A limits file that
 *  names no rule of the link's host is refused, since step 4 would then judge nothing, and so is
 *  OUTSIDE_TAILNET_USER_CODE, an approval those steps do not make. Any other value of
 *  OUTSIDE_EDGE_STEPS is refused; unset or empty, `run` makes every step.
 *
 *  Inputs, each refused by name, UNKNOWN and exit 2, when missing or malformed (the sign-in steps
 *  read no OUTSIDE_PEER_SESSION, OUTSIDE_BROKER_URL or OUTSIDE_LISTENER_URL):
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
 *                           (`device-login.json`), each code it approved with its credential
 *                           (`approved.tsv`), and the session step 3 registered at the listener
 *                           (`session`), kept once the listener accepted its registration,
 *                           which step 3 ends before the run goes on.
 *                           `approve`, `revoke`, `grants` and `t5` read them, and so does a
 *                           later step that needs the person's cookie or a session of theirs.
 *  Optional: OUTSIDE_EDGE_STEPS (above); OUTSIDE_TAILNET_USER_CODE (above);
 *  OUTSIDE_EDGE_REPLY_SECONDS (30 when unset; 1 to 600); OUTSIDE_EDGE_TSV, the file the rows are
 *  also written to (`<state>/outside-edge.tsv` when unset). OUTSIDE_EDGE_ALLOW_HTTP=1 lets each
 *  origin be http://127.0.0.1 or http://localhost, and nothing else, for this driver's own test,
 *  and only with it OUTSIDE_EDGE_LOGIN_SECONDS (1 to 300) shortens the device login's 5-minute
 *  deadline, which that test waits out; without it that variable is refused. The sign-in steps
 *  read neither OUTSIDE_EDGE_REPLY_SECONDS nor OUTSIDE_EDGE_LOGIN_SECONDS. Chromium is started
 *  with none of the OUTSIDE_* variables, and every registration at the listener names the machine
 *  `outside-edge-e2e`.
 *
 *  `approve <code>` approves one more device-login code on the outside origin's machine-login
 *  page, in the browser state `run` kept in OUTSIDE_EDGE_STATE, and records its credential, named
 *  on stderr first as `run` names it; it prints `APPROVED <code>` and exits 0, or
 *  `FAIL: <code> ...` (1) where the page answered short of an approval, or `UNKNOWN: <code> ...`
 *  (2) where the page did not load, could not be driven or closed while a step waited on it, each
 *  on one line, a line break of the page's text made a space. Another check that starts a device
 *  login of its own calls it with the code it was given.
 *
 *  `revoke` lists the person's machine logins (`GET /api/v1/machine-logins`) with the kept cookie
 *  and revokes each credential `approved.tsv` holds (`POST /api/v1/machine-logins/{id}/revoke`),
 *  a row each, `revoke-<credential id>`, printed and written to OUTSIDE_EDGE_REVOCATIONS
 *  (`<state>/outside-edge-revocations.tsv` when unset). A credential the list does not hold
 *  fails its row: the person's own list is where they end it. A row passes only when the list,
 *  read again after the revoke answered 2xx, no longer holds the credential; a list that gave no
 *  answer, before or after, reads UNKNOWN. OUTSIDE_EDGE_REVOKE_KEEP names one approved code whose
 *  login it leaves live, with no row, in either form the driver takes (`ABCD-EFGH`, `abcdefgh`):
 *  the login a delisting window measures ending, which the person's removal ends and not this
 *  revoke. An empty kept cookie is refused, UNKNOWN, as it is by `t5`.
 *
 *  `grants` reads, with the kept cookie, what the person reaches of the credential grants of
 *  sessions that do not act for them: row `grants-list`, `GET
 *  /api/v1/credential-grants?approver=me`, PASS on 200 holding only grants whose enrollment names
 *  the person as its operator, FAIL on 200 holding any other (it names how many, and whether
 *  OUTSIDE_GRANTS_PROBE_ID's grant is among them, and no grant's id or secret); and row
 *  `grants-revoke`, `POST /api/v1/credential-grants/{OUTSIDE_GRANTS_PROBE_ID}/revoke`, PASS only on
 *  Dispatch's own refusal, 403 RESTRICTED_MEMBER, FAIL on a 2xx or on the broker's 403
 *  NOT_APPROVER, which shows Dispatch relayed the revoke. Any other answer, or none, is UNKNOWN;
 *  so is the revoke row with no OUTSIDE_GRANTS_PROBE_ID, or one that is no UUID, which revokes
 *  nothing. The probe is a grant its caller chose as one the broker lets no person end (no
 *  approver, no operator), so a revoke let through ends nothing; where the list just read shows
 *  that grant with `approver` or `enrollment.operator` set, it revokes nothing, UNKNOWN. The rows
 *  go to stdout and to OUTSIDE_EDGE_GRANTS (`<state>/outside-edge-grants.tsv` when unset). An
 *  empty kept cookie is refused, UNKNOWN.
 *
 *  `t5` is the browser's half of a delisting window: with OUTSIDE_T5_DEADLINE (Unix seconds) and
 *  OUTSIDE_T5_TSV, it asks `/auth/whoami` with the kept cookie at most every 5 s until it is
 *  answered 401, then tries the kept passkey in a fresh browser; rows `T5-cookie` and
 *  `T5-passkey`, each naming `at=<Unix seconds>`, both written before the deadline, since every
 *  wait of the sign-in, the read of an answer's body among them, ends 2 s before it (its own doc
 *  says each verdict).
 *
 *  `hold` registers the session `run` kept (`session`) at OUTSIDE_LISTENER_URL again, as step 3
 *  did, with that session's broker credential traded for with the kept device login, and holds
 *  its relay socket and registration live for a check that acts as the person's session. It reads
 *  OUTSIDE_EDGE_STATE, OUTSIDE_PROJECT, OUTSIDE_BROKER_URL and OUTSIDE_LISTENER_URL (and
 *  OUTSIDE_EDGE_ALLOW_HTTP), no link and no peer. It writes its pid and its start (field 22 of
 *  `/proc/<pid>/stat`) to `holder` as it starts, and the same to `held` once the listener took
 *  the registration, and prints `HELD <session>`. While held, it renews the registration every
 *  OUTSIDE_EDGE_RENEW_SECONDS (90 when unset; 1 to 90), as a session host does, and registers the
 *  session again on the socket the relay client opens after a routine close (4401, the socket's
 *  token lapsed; 1012, the listener stops). It holds until SIGTERM, or until
 *  OUTSIDE_EDGE_HOLD_SECONDS have passed (1200 when unset; 60 to 3600), then closes the socket,
 *  unregisters the session, writes `released` and prints `RELEASED <session>`: `released` says
 *  `unregistered`, exit 0, only where the session stayed registered to the end and the listener
 *  answered the unregister; a failed unregister is exit 2 naming it. A registration the listener
 *  did not take is `UNKNOWN: <why>` (2) with no `held`, and so is a flush before it that got no
 *  answer within 10 s, which sends none, and a refusal of the relay subject; while held, any
 *  other close of the socket, a socket not open again within 30 s of a routine close, and a
 *  renewal or registration the listener refused each end the hold `UNKNOWN: …` (2), naming why,
 *  with the session unregistered and registered no more. A SIGTERM before `held` is written
 *  holds nothing: before the registration is sent, none is sent; while the listener answers it,
 *  the session is unregistered once answered; either way `UNKNOWN: no session held: …` (2). Every
 *  end after `holder` is written writes `released` naming it, and removes `held`, so no later
 *  check reads a hold that ended as one that holds; but an error of the hold's own (an
 *  OUTSIDE_EDGE_STATE it cannot write among them), which judged nothing of the edge, is
 *  `UNKNOWN: the hold failed on an error of its own: <its class>` (2), naming the state's file
 *  where the error names one, never exit 1, a FAIL, and a session it registered is unregistered
 *  first.
 *
 *  `release` ends that hold, reading OUTSIDE_EDGE_STATE alone, whether or not it holds yet:
 *  SIGTERM to the pid `holder` names, up to 15 s for it to end, then `RELEASED <session>` (0)
 *  when `released` says `unregistered`, else `UNKNOWN: <why>` (2). No `holder` is
 *  `UNKNOWN: no session is held`; a pid whose process did not start when `holder` says is
 *  `UNKNOWN: the held process is gone`, and is not signalled; and a holder still running after
 *  the 15 s is sent SIGKILL. Once the hold `holder` names has ended, `release` removes `holder`
 *  (and a `held` a killed hold left), so a release after it, as one before a later hold of the
 *  same directory has started, reads no hold rather than this hold's end; `released` stays, for a
 *  check to read. A hold removes the `holder`, `held` and `released` an earlier hold wrote as it
 *  starts, before any other input is read, so a hold refused at its inputs leaves none of them;
 *  but where `holder` names a hold still running, it leaves them to that hold and holds nothing:
 *  `UNKNOWN: no session held: the hold <pid> still holds OUTSIDE_EDGE_STATE; release it first`
 *  (2). So once a later hold of the directory has started, a release reads that hold's end, never
 *  an earlier one's; only before it has written its `holder` can a release still read the end of
 *  a hold that ended by itself and was never released.
 *
 *  Output: each row as `row<TAB>want<TAB>got<TAB>PASS|FAIL|UNKNOWN` on stdout and in the TSV
 *  file, then the verdict: `PASS` (exit 0) when every row passed, `FAIL: <rows>` (exit 1) when
 *  any failed, else `UNKNOWN: <rows>` (exit 2). An error of a command's own, not an answer of the
 *  edge's (an OUTSIDE_EDGE_STATE it cannot write, say), judged nothing, so every command ends it
 *  `UNKNOWN: <command> failed on an error of its own: <its class>` (2), naming OUTSIDE_EDGE_STATE
 *  or its file where the error names one, never exit 1, a FAIL. It prints no token, cookie or
 *  link, and no name of the machine it runs on: its output can be posted where anyone reads it.
 *  The link's token is withheld from every row even where a browser's error or a page's text
 *  names it, as an error of a page that did not load names the URL it was loading. A browser that
 *  does not close within 10 s, as one whose page holds its main thread, holds no verdict back:
 *  the command goes on, naming it on stderr, and its processes are killed as the driver exits.
 */

// First, before any module that takes `fetch` or `WebSocket`: importing it wraps both, and the
// relay client takes its WebSocket constructor when its module loads (its header says why).
import "./outside-edge-count";
import { randomBytes } from "node:crypto";
import {
  appendFileSync,
  chmodSync,
  existsSync,
  lstatSync,
  mkdirSync,
  readdirSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { basename, dirname, join, resolve as resolvePath } from "node:path";
import {
  type DeviceAuthorization,
  DeviceLoginNotKeptError,
  deviceLoginHost,
  finishDeviceLogin,
  type KeptDeviceLogin,
  startDeviceLogin,
} from "@legion/cipher-client/device-login";
import { EnvelopeSchema, legacyAgentSubject, SESSION_LIVE_WINDOW_MS } from "@legion/contracts";
import { errorClass } from "@legion/contracts/log-text";
import type { RequestCredential } from "@legion/envoy-client/bearer";
import { brokerCredential } from "@legion/envoy-client/broker-credential";
import {
  type ConsumerDelivery,
  type ConsumerFrame,
  createConsumerDelivery,
  type InboundUnavailable,
} from "@legion/envoy-client/consumer-delivery";
import { DEFAULT_HEARTBEAT_MS } from "@legion/envoy-client/envoy-settings";
import { connectListenerRelay, type ListenerRelay } from "@legion/envoy-client/listener-relay";
import { registerWithRelay } from "@legion/envoy-client/relay-registration";
import { createEnvoyClient, EnvoyApiError, type EnvoyClient } from "@legion/envoy-client/transport";
import {
  type Browser,
  type BrowserContext,
  chromium,
  type Locator,
  type Page,
} from "@playwright/test";
import { settledState } from "../settled";
import { addVirtualPasskey } from "../virtual-passkey";
import { count, counted } from "./outside-edge-count";
import {
  judgeLimits,
  type Limit,
  LimitsError,
  limitRow,
  parseLimits,
  type Row,
  verdictOf,
} from "./outside-edge-limits";
import { type PageAnswer, readInPage } from "./outside-edge-page-read";

/** The name every device login of this driver gives its machine, in place of this machine's own
 *  host name, which its output must not carry. Every registration at the listener names it too, as
 *  its `host` (`machineID()` of `@legion/envoy-client/machine` reads ENVOY_MACHINE_ID before the
 *  kernel's host name, and the driver sets it to this before any command runs): the listener keeps
 *  that `host` and shows it in its session lists. */
const DEVICE_HOST = deviceLoginHost("outside-edge-e2e");
/** How long one page step waits for what it waits on. */
const PAGE_MS = 30_000;
/** How long the driver waits on a read the page's own script makes (`pageRead`): the read's own
 *  deadline, PAGE_MS from the driver's ask by the driver's clock, which the script runs
 *  (`readInPage`), and 5 s more for its answer to come back from the page, so where the page's
 *  main thread is free the read's own deadline answers first, however late the page started it. */
const PAGE_SCRIPT_MS = PAGE_MS + 5_000;
/** How long the driver waits for its browser to close. Chromium closes in a second or two, but
 *  its close waits on a page whose main thread loops (the driver's own test measured such closes
 *  of 75 s and more after the run's last row), and Playwright's close has no deadline: past this,
 *  the driver goes on to its verdict without it, and Playwright's exit handler kills the
 *  browser's processes (SIGKILL to their group) as the driver exits. */
const CLOSE_MS = 10_000;
/** How long the relay socket's first open may take at most, and a held socket's open again. */
const OPEN_MS = 30_000;
/** How long the device login's poll may wait for an approval at most, where
 *  OUTSIDE_EDGE_LOGIN_SECONDS, which the driver's own test alone may set, names no shorter time. */
const LOGIN_SECONDS = 300;

/** Starts Chromium with this process's environment but the driver's own inputs (`OUTSIDE_*`),
 *  which name the link, the state directory and the edge's origins: the browser has no use for
 *  them, and its processes are no place to keep them. */
function launchBrowser(): Promise<Browser> {
  const env: Record<string, string> = {};
  for (const [name, value] of Object.entries(process.env)) {
    if (value !== undefined && !name.startsWith("OUTSIDE_")) env[name] = value;
  }
  return chromium.launch({ env });
}

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

/** The names and codes of a request the far end gave no whole answer: no connection, no route, no
 *  name, a reset or a deadline, as an error's `name` or `code` holds them. Each client keeps the
 *  runtime's code of such a failure on its own error, which keeps no cause (`failureCode` of
 *  `@legion/contracts/log-text`), and this allowlist is what the driver reads of it: the broker
 *  client's request and trade (`BrokerTradeFailedError`, `TradeRetryingError`,
 *  `TradeUnansweredError`, ETIMEDOUT for one its deadline ended), the relay client's socket that
 *  did not open (`RelayNotOpenedError`, ETIMEDOUT for an upgrade, a trade or an open still
 *  unanswered at its bound) and the Envoy client's call that got no answer. An Envoy call whose
 *  status came and whose body did not keeps no code: the listener answered it. */
const UNREACHED: Readonly<Record<string, true>> = {
  TimeoutError: true,
  ECONNREFUSED: true,
  ConnectionRefused: true,
  ENOTFOUND: true,
  EAI_AGAIN: true,
  EHOSTUNREACH: true,
  ENETUNREACH: true,
  ECONNRESET: true,
  ETIMEDOUT: true,
  FailedToOpenSocket: true,
};

/** Whether `error`, or an error it was caused by, is a request the far end gave no answer, by the
 *  name or the code the client kept, never by its text. Such a step proved nothing, so it is
 *  UNKNOWN; any answer at all is evidence, so every other error is a FAIL. The listener's own
 *  refusal (`EnvoyApiError`) is an answer whatever its body says: its `code` is text the listener
 *  wrote, never the runtime's, so a refusal that names a code of the list is still a FAIL. */
function unreached(error: unknown): boolean {
  for (let at: unknown = error; at instanceof Error; at = at.cause) {
    if (at instanceof EnvoyApiError) return false;
    const code = "code" in at ? at.code : undefined;
    if (Object.hasOwn(UNREACHED, at.name)) return true;
    if (typeof code === "string" && Object.hasOwn(UNREACHED, code)) return true;
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

/** What every `run` reads, and all the sign-in steps read. */
interface SignInInputs {
  readonly invite: URL;
  readonly origin: string;
  readonly project: string;
  /** The rules step 4 judges: in the sign-in steps, those of the link's host alone. */
  readonly limits: readonly Limit[];
  readonly state: string;
  readonly tsv: string;
}

/** What `run` reads for every step. */
interface RunInputs extends SignInInputs {
  readonly peer: string;
  readonly broker: string;
  readonly listener: string;
  readonly tailnetCode: string | undefined;
  readonly replySeconds: number;
  /** How long the device login's poll waits for an approval (LOGIN_SECONDS, or the test's own). */
  readonly loginMs: number;
}

/** OUTSIDE_EDGE_STEPS's one value: the sign-in steps alone. */
const SIGN_IN_STEPS = "sign-in";

/** What `run` is asked: every step, or the sign-in steps alone, beside the rules of
 *  OUTSIDE_EDGE_LIMITS they leave unjudged, each of another host than the link's. */
type RunAsked =
  | { readonly steps: "every"; readonly inputs: RunInputs }
  | {
      readonly steps: typeof SIGN_IN_STEPS;
      readonly inputs: SignInInputs;
      readonly unasked: readonly Limit[];
    };

const PROJECT_KEY = /^[A-Z][A-Z0-9]{0,31}$/;
const USER_CODE = /^[A-Za-z0-9]{4}-?[A-Za-z0-9]{4}$/;

/** What every `run` reads last: the rules of OUTSIDE_EDGE_LIMITS, the state directory, made, and
 *  the file the rows are also written to. */
function limitsAndState(env: NodeJS.ProcessEnv): {
  limits: Limit[];
  state: string;
  tsv: string;
} {
  const limitsFile = required(env, "OUTSIDE_EDGE_LIMITS");
  let limits: Limit[];
  try {
    limits = parseLimits(readFileSync(limitsFile, "utf8"));
  } catch (error) {
    if (error instanceof LimitsError) throw new Refused(`OUTSIDE_EDGE_LIMITS: ${error.message}`);
    throw new Refused(`OUTSIDE_EDGE_LIMITS could not be read: ${messageOf(error)}`);
  }
  const state = stateDirectory(required(env, "OUTSIDE_EDGE_STATE"));
  return {
    limits,
    state,
    tsv: (env.OUTSIDE_EDGE_TSV ?? "").trim() || join(state, "outside-edge.tsv"),
  };
}

function runInputs(env: NodeJS.ProcessEnv): RunAsked {
  const steps = (env.OUTSIDE_EDGE_STEPS ?? "").trim();
  if (steps !== "" && steps !== SIGN_IN_STEPS) {
    throw new Refused(`OUTSIDE_EDGE_STEPS must be ${SIGN_IN_STEPS}, or unset for every step`);
  }
  const allowHttp = env.OUTSIDE_EDGE_ALLOW_HTTP === "1";
  const invite = originOf("OUTSIDE_INVITE_URL", required(env, "OUTSIDE_INVITE_URL"), allowHttp);
  if (invite.pathname !== "/enroll" || invite.hash.length <= 1) {
    throw new Refused("OUTSIDE_INVITE_URL is no enrollment link (<origin>/enroll#<token>)");
  }
  const project = required(env, "OUTSIDE_PROJECT");
  if (!PROJECT_KEY.test(project)) throw new Refused("OUTSIDE_PROJECT is no project key");
  if (steps === SIGN_IN_STEPS) return signInInputs(env, invite, project);
  const peer = required(env, "OUTSIDE_PEER_SESSION");
  if (/\s/.test(peer)) throw new Refused("OUTSIDE_PEER_SESSION holds white space");
  const broker = originOf("OUTSIDE_BROKER_URL", required(env, "OUTSIDE_BROKER_URL"), allowHttp);
  const listener = originOf(
    "OUTSIDE_LISTENER_URL",
    required(env, "OUTSIDE_LISTENER_URL"),
    allowHttp
  );
  const { limits, state, tsv } = limitsAndState(env);
  const tailnetCode = (env.OUTSIDE_TAILNET_USER_CODE ?? "").trim() || undefined;
  if (tailnetCode !== undefined && !USER_CODE.test(tailnetCode)) {
    throw new Refused("OUTSIDE_TAILNET_USER_CODE is no device-login code (XXXX-XXXX)");
  }
  const reply = (env.OUTSIDE_EDGE_REPLY_SECONDS ?? "").trim() || "30";
  const replySeconds = Number(reply);
  if (!/^[0-9]+$/.test(reply) || replySeconds < 1 || replySeconds > 600) {
    throw new Refused("OUTSIDE_EDGE_REPLY_SECONDS must be a whole number of seconds from 1 to 600");
  }
  // The driver's own test waits out the poll's deadline, which at 5 minutes a row cannot afford;
  // a run against a real edge never shortens it, so the variable is refused without that test's
  // OUTSIDE_EDGE_ALLOW_HTTP=1.
  const login = (env.OUTSIDE_EDGE_LOGIN_SECONDS ?? "").trim();
  if (login !== "" && !allowHttp) {
    throw new Refused(
      "OUTSIDE_EDGE_LOGIN_SECONDS is the driver's own test's, taken only with OUTSIDE_EDGE_ALLOW_HTTP=1"
    );
  }
  const loginSeconds = login === "" ? LOGIN_SECONDS : Number(login);
  if (!/^[0-9]*$/.test(login) || loginSeconds < 1 || loginSeconds > LOGIN_SECONDS) {
    throw new Refused(
      `OUTSIDE_EDGE_LOGIN_SECONDS must be a whole number of seconds from 1 to ${LOGIN_SECONDS}`
    );
  }
  return {
    steps: "every",
    inputs: {
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
      loginMs: loginSeconds * 1000,
      tsv,
    },
  };
}

/** The sign-in steps' inputs past the link and the project: the rules of the link's host, the one
 *  host those steps ask, the state and the TSV file; no peer, broker or listener, which they never
 *  ask, and no tailnet code, an approval they do not make. A limits file that names no rule of the
 *  link's host is refused: step 4 would judge nothing, and a run that judged nothing has not
 *  passed. */
function signInInputs(env: NodeJS.ProcessEnv, invite: URL, project: string): RunAsked {
  const { limits, state, tsv } = limitsAndState(env);
  const host = invite.hostname;
  const asked = limits.filter((limit) => limit.host === host);
  if (asked.length === 0) {
    throw new Refused(
      `OUTSIDE_EDGE_LIMITS names no rule of ${host}, the link's host, the one host the sign-in steps ask`
    );
  }
  if ((env.OUTSIDE_TAILNET_USER_CODE ?? "").trim() !== "") {
    throw new Refused(
      "OUTSIDE_TAILNET_USER_CODE asks for an approval, and the sign-in steps make none"
    );
  }
  return {
    steps: SIGN_IN_STEPS,
    inputs: { invite, origin: invite.origin, project, limits: asked, state, tsv },
    unasked: limits.filter((limit) => limit.host !== host),
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

/** What `within` answers for work whose time ran out first. */
const TIME_UP = Symbol("time up");

/** `work`'s value, or TIME_UP once `ms` passed first. Work that ends after that is let go: its
 *  failure is caught here, and nothing reads its value. Each wait of the driver's that has no
 *  deadline of its own (Playwright's `evaluate`, `storageState`, a browser's close and a
 *  response's body have none) is bounded through it, so an edge that holds one never holds the
 *  run with no verdict. */
async function within<T>(work: Promise<T>, ms: number): Promise<T | typeof TIME_UP> {
  work.catch(() => undefined);
  const ended = Promise.withResolvers<typeof TIME_UP>();
  const timer = setTimeout(() => ended.resolve(TIME_UP), Math.max(0, ms));
  try {
    return await Promise.race([work, ended.promise]);
  } finally {
    clearTimeout(timer);
  }
}

/** Closes `browser` within CLOSE_MS at most, naming on stderr one that did not close by then. */
async function closeBrowser(browser: Browser | undefined): Promise<void> {
  if (browser === undefined) return;
  if ((await within(browser.close(), CLOSE_MS)) === TIME_UP) {
    log(
      `the browser did not close within ${CLOSE_MS / 1000} s; its processes are killed as the driver exits`
    );
  }
}

/** A JSON read the page's own script makes, from its origin with its cookie, so the browser
 *  counts it as it counts the page's other requests (`readInPage`). It waits until PAGE_MS after
 *  the driver asked, by the driver's clock, at most: a read that got no answer by then, or none at
 *  all, is `unanswered`, with why; one whose status came and whose body did not is that status,
 *  the far end's answer, with `cut` saying why the body did not come. The page's script runs the
 *  read and that deadline, so a page that holds its main thread (a script that loops, or a
 *  `fetch` it replaced that never returns) runs neither, and Playwright's `evaluate` has no
 *  deadline of its own: the driver waits PAGE_SCRIPT_MS for the script at most, and a read the
 *  script held past that is `unanswered` too, naming the script. */
async function pageRead(page: Page, path: string): Promise<PageAnswer> {
  // One read is the measurement: the request's own answer, which no later page state changes.
  const evaluated = page.evaluate(readInPage, { at: path, until: Date.now() + PAGE_MS });
  const read = await within(evaluated, PAGE_SCRIPT_MS);
  return read === TIME_UP
    ? { unanswered: `the page's script held the read past ${PAGE_SCRIPT_MS / 1000} s` }
    : read;
}

/** What the machine-login page answered one code. */
type Approval =
  | { readonly credential: string }
  | { readonly refused: string; readonly unread: boolean };

/**
 * Approves `code` on `origin`'s machine-login page as the browser's signed-in person: it types the
 * code, looks it up, presses Approve, and reads the credential the page then names. A page that
 * did not load is unread, and so is one the driver could not drive (a box that takes no typing, a
 * button that cannot be pressed) and one that closed, or whose browser ended, while a step waited
 * on it (`PageGone`); one that loaded and did not approve is a refusal, with what it said.
 */
async function approveOnPage(page: Page, origin: string, code: string): Promise<Approval> {
  try {
    return await approveSteps(page, origin, code);
  } catch (error) {
    return {
      refused: `the machine-login page could not be driven: ${messageOf(error)}`,
      unread: true,
    };
  }
}

/** A page or its browser that closed while a page step waited on it: the driver's own instrument
 *  gone, which judged nothing of the edge. It is thrown, never read as a state the page did not
 *  show, so the step's caller reads it as it reads a page it could not drive: UNKNOWN (`run`'s
 *  catch, `approveOnPage`'s, `t5`'s). */
class PageGone extends Error {}

/** `step`, a wait of the driver's on `page`, where a page or a browser that closes first throws
 *  `PageGone` at once, and so does a step that failed once either had closed: a closed page's
 *  locators fail as those of a page that showed nothing, and a step that read that as the page's
 *  answer would judge the edge on the driver's own instrument gone. Any other failure of `step`
 *  is thrown as it came. `what` names what the step read, for `PageGone`'s text. */
async function whilePageOpen<T>(page: Page, what: string, step: () => Promise<T>): Promise<T> {
  const browser = page.context().browser();
  const pageClosed = () => new PageGone(`the page closed while the driver read ${what}`);
  const browserEnded = () => new PageGone(`the browser ended while the driver read ${what}`);
  const gone = Promise.withResolvers<never>();
  const onClose = () => gone.reject(pageClosed());
  const onDisconnect = () => gone.reject(browserEnded());
  page.once("close", onClose);
  browser?.once("disconnected", onDisconnect);
  const stepped = step();
  // A step a closed page ended goes on until its own time, and nothing reads its end.
  stepped.catch(() => undefined);
  try {
    return await Promise.race([stepped, gone.promise]);
  } catch (error) {
    if (error instanceof PageGone) throw error;
    // The step has ended: a browser gone by now is the driver's failure, not the page's.
    if (browser?.isConnected() === false) throw browserEnded();
    // Likewise a page closed by now: no later read of it would show anything.
    if (page.isClosed()) throw pageClosed();
    throw error;
  } finally {
    page.off("close", onClose);
    browser?.off("disconnected", onDisconnect);
  }
}

/** Which of `states` the page settled on within `ms` (`settledState` of `e2e/settled.ts`, which
 *  reads them again until one shows, the first that does winning), or undefined where none showed
 *  by then: a page step branches on what the page settled on, never on one instant's answer. A
 *  page or a browser that closed first throws `PageGone` at once (`whilePageOpen`): the poll would
 *  read its closed locators as failed reads until its time ran out, and so as a page that showed
 *  none of them. */
async function settledWithin<Name extends string>(
  what: string,
  states: readonly (readonly [Name, Locator])[],
  ms: number
): Promise<Name | undefined> {
  const page = states[0]?.[1].page();
  if (page === undefined) return undefined;
  try {
    return await whilePageOpen(page, what, () => settledState(what, states, ms));
  } catch (error) {
    if (error instanceof PageGone) throw error;
    return undefined;
  }
}

async function approveSteps(page: Page, origin: string, code: string): Promise<Approval> {
  try {
    await page.goto(`${origin}/credentials/machine`, { timeout: PAGE_MS });
  } catch (error) {
    return { refused: `the machine-login page did not load: ${messageOf(error)}`, unread: true };
  }
  const box = page.getByLabel("Code shown on the machine");
  try {
    await whilePageOpen(page, "the machine-login page's code box", () =>
      box.waitFor({ timeout: PAGE_MS })
    );
  } catch (error) {
    if (error instanceof PageGone) throw error;
    return { refused: "the machine-login page shows no code box", unread: false };
  }
  await box.fill(code, { timeout: PAGE_MS });
  await page.getByRole("button", { name: "Look up" }).click({ timeout: PAGE_MS });
  const approve = page.getByRole("button", { name: "Approve" });
  const alert = page.getByRole("alert").first();
  const looked = await settledWithin(
    "the machine-login page after Look up",
    [
      ["approve", approve],
      ["refused", alert],
    ],
    PAGE_MS
  );
  if (looked === undefined) {
    return { refused: "the page showed neither Approve nor a refusal for the code", unread: false };
  }
  if (looked === "refused") {
    return {
      refused: `the page refused the code: ${(await alert.innerText()).trim()}`,
      unread: false,
    };
  }
  await approve.click({ timeout: PAGE_MS });
  const decided = await settledWithin(
    "the machine-login page after Approve",
    [
      ["approved", page.getByText(/^Approved\. /).first()],
      ["refused", alert],
    ],
    PAGE_MS
  );
  if (decided === undefined) {
    return { refused: "the page did not say the code was approved", unread: false };
  }
  if (decided === "refused") {
    return {
      refused: `the approval was refused: ${(await alert.innerText()).trim()}`,
      unread: false,
    };
  }
  // A page still open that names no credential within PAGE_MS approved and named none, a FAIL; one
  // that closed meanwhile is `PageGone`, the driver's own failure, whose text says the page had
  // said Approved, so a person knows the credential may be live.
  let named = "";
  try {
    named = await whilePageOpen(
      page,
      "the credential the machine-login page named once it said Approved",
      () =>
        page
          .getByText(/^Credential \S+$/)
          .first()
          .innerText({ timeout: PAGE_MS })
    );
  } catch (error) {
    if (error instanceof PageGone) throw error;
  }
  const credential = /^Credential (\S+)$/.exec(named.trim())?.[1];
  if (credential === undefined) {
    return { refused: "the page approved the code and named no credential", unread: false };
  }
  return { credential };
}

/** Records an approved code and its credential, for `revoke`, naming the credential on stderr
 *  first: one approved and not recorded (a state the driver cannot write) is live, and no
 *  `approved.tsv` holds it for `revoke`, so the output is where a person reads which to end. */
function recordApproval(state: string, code: string, credential: string): void {
  log(`${code} approved as credential ${credential}`);
  appendFileSync(join(state, "approved.tsv"), `${code}\t${credential}\n`, { mode: 0o600 });
  chmodSync(join(state, "approved.tsv"), 0o600);
}

/** Step 1: the link enrolls a passkey, and the person's list is exactly the project. Answers the
 *  signed-in person, or undefined when the step did not pass. */
async function signIn(page: Page, context: BrowserContext, inputs: SignInInputs, rows: Rows) {
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
  const alert = page.getByRole("alert").first();
  const enrollment = await settledWithin(
    "the enrollment page",
    [
      ["create", create],
      ["refused", alert],
    ],
    PAGE_MS
  );
  if (enrollment === undefined) {
    rows.add({
      row: "sign-in",
      want,
      got: "no enrollment: the page shows no Create a passkey button",
      verdict: "FAIL",
    });
    return undefined;
  }
  if (enrollment === "refused") {
    rows.add({
      row: "sign-in",
      want,
      got: `the page refused the link: ${(await alert.innerText()).trim()}`,
      verdict: "FAIL",
    });
    return undefined;
  }
  await create.click({ timeout: PAGE_MS });
  const enrolled = await settledWithin(
    "the enrollment after Create a passkey",
    [
      ["signed-in", page.getByText(/^Signed in as /).first()],
      ["refused", alert],
    ],
    PAGE_MS
  );
  if (enrolled === undefined) {
    rows.add({
      row: "sign-in",
      want,
      got: "the passkey enrolled no sign-in within 30 s",
      verdict: "FAIL",
    });
    return undefined;
  }
  if (enrolled === "refused") {
    rows.add({
      row: "sign-in",
      want,
      got: `the enrollment was refused: ${(await alert.innerText()).trim()}`,
      verdict: "FAIL",
    });
    return undefined;
  }
  // Where the sign-in landed: the link's origin, the outside one, and no other.
  const landed = new URL(page.url()).origin;
  if (landed !== inputs.origin) {
    rows.add({
      row: "sign-in",
      want,
      got: `the passkey signed the person in at ${landed}, not at the link's origin`,
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
  // One read is the measurement here, not a sample: `pageRead` awaits the request's own answer,
  // or its absence, for PAGE_MS at most, and the page's drawing decides nothing of it.
  const whoami = await pageRead(page, "/auth/whoami");
  if ("unanswered" in whoami) {
    rows.add({
      row: "sign-in",
      want,
      got: `GET /auth/whoami got no answer: ${whoami.unanswered}`,
      verdict: "UNKNOWN",
    });
    return undefined;
  }
  const login = member(whoami.body, "login");
  if (whoami.status !== 200 || typeof login !== "string" || login === "") {
    rows.add({
      row: "sign-in",
      want,
      got:
        whoami.cut === undefined
          ? `GET /auth/whoami answered ${whoami.status} with no login`
          : `GET /auth/whoami answered ${whoami.status}, and its body did not come (${whoami.cut}), so it named no login`,
      verdict: "FAIL",
    });
    return undefined;
  }
  // The person's cookie, which `revoke` and `t5` read: a sign-in that set none signed nobody in
  // for them, whatever the page said.
  const cookie = (await context.cookies(inputs.origin))
    .map((one) => `${one.name}=${one.value}`)
    .join("; ");
  if (cookie === "") {
    rows.add({
      row: "sign-in",
      want,
      got: `the sign-in set no cookie for ${inputs.origin}`,
      verdict: "FAIL",
    });
    return undefined;
  }
  rows.add({
    row: "sign-in",
    want,
    got: `signed in at ${landed} as the invited person`,
    verdict: "PASS",
  });

  // One read is the measurement here, as for whoami above: the answer of the request itself,
  // awaited for PAGE_MS at most, which no later moment of the page would change.
  const projects = await pageRead(page, "/api/v1/projects");
  const wantProjects = `GET /api/v1/projects lists exactly [${inputs.project}]`;
  const keys =
    "status" in projects && Array.isArray(projects.body)
      ? projects.body.map((one) => String(member(one, "key"))).sort()
      : undefined;
  if ("unanswered" in projects) {
    rows.add({
      row: "projects",
      want: wantProjects,
      got: `got no answer: ${projects.unanswered}`,
      verdict: "UNKNOWN",
    });
  } else if (projects.status !== 200 || keys === undefined) {
    rows.add({
      row: "projects",
      want: wantProjects,
      got:
        projects.cut === undefined
          ? `answered ${projects.status}, no list`
          : `answered ${projects.status}, and its body did not come (${projects.cut}), so no list`,
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

  // What `approve`, `revoke` and a later step read, each this run's own and mode 0600. Playwright
  // saves the browser's state with no deadline of its own, reading the page's storage through a
  // script in the page, which a page that holds its main thread never runs: the save is waited on
  // for PAGE_MS at most, and one still waiting then stops the run, naming it. The state is written
  // here, not by Playwright, so a save that ends after the run gave up on it writes nothing.
  const saved = await within(context.storageState(), PAGE_MS);
  if (saved === TIME_UP) {
    throw new Error(
      `the save of the browser's signed-in state did not end within ${PAGE_MS / 1000} s, so the run kept none`
    );
  }
  keep(inputs.state, "browser.json", JSON.stringify(saved));
  keep(inputs.state, "cookie", cookie);
  keep(inputs.state, "person", login);
  keep(inputs.state, "origin", inputs.origin);
  return login;
}

/** Waits `ms`, or rejects with `signal`'s reason once it aborts. The device-login poll waits
 *  through it before each try, so an abort ends the poll at its next wait, naming why. */
function pause(ms: number, signal: AbortSignal): Promise<void> {
  if (signal.aborted) return Promise.reject(signal.reason);
  const { promise, resolve, reject } = Promise.withResolvers<void>();
  const aborted = () => {
    clearTimeout(timer);
    reject(signal.reason);
  };
  const timer = setTimeout(() => {
    signal.removeEventListener("abort", aborted);
    resolve();
  }, ms);
  signal.addEventListener("abort", aborted, { once: true });
  return promise;
}

/** Removes each whole copy of a device login the broker client kept beside OUTSIDE_EDGE_STATE's
 *  `device-login.json` where it could not save the login there (`device-login.json.kept-<uuid>`,
 *  `KeptCopyError` of `@legion/cipher-client`), which holds the login's key and refresh token;
 *  answers how many it removed. */
function removeKeptCopies(state: string): number {
  const copies = readdirSync(state).filter((name) =>
    /^device-login\.json\.kept-[0-9a-f-]{36}$/.test(name)
  );
  for (const name of copies) rmSync(join(state, name));
  return copies.length;
}

/** Step 2: a device login of this machine, approved on the page; then the tailnet code. Answers
 *  the login, or undefined when it was not made. */
async function deviceLogin(
  page: Page,
  person: string,
  inputs: RunInputs,
  rows: Rows
): Promise<KeptDeviceLogin | undefined> {
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
  // The poll's deadline as a reader says it: 5 minutes, or the driver's own test's seconds.
  const deadlineSaid =
    inputs.loginMs % 60_000 === 0
      ? `${inputs.loginMs / 60_000} minutes`
      : `${inputs.loginMs / 1000} s`;
  const late = new Error(`the poll got no approval within ${deadlineSaid}`);
  const deadline = setTimeout(() => stop.abort(late), inputs.loginMs);
  // How the polls fared: how many were sent and how many the broker answered, whether it answered
  // the last, and how the last it did not answer failed. The poll is sent again after every
  // request that got no answer (`finishDeviceLogin`), so a deadline whose last poll got no answer
  // is a broker the run did not reach at the end, UNKNOWN, whether it answered no poll at all or
  // answered and then fell silent; one that answered the last poll (`authorization_pending` to
  // the end, say) answered short of an approval, a FAIL.
  let polls = 0;
  let answered = 0;
  let lastAnswered = false;
  let lastUnanswered = "no poll was sent";
  const pollFetch = Object.assign(
    async (input: string | URL | Request, init?: RequestInit) => {
      polls += 1;
      try {
        const answer = await fetch(input, init);
        answered += 1;
        lastAnswered = true;
        return answer;
      } catch (error) {
        lastAnswered = false;
        lastUnanswered = messageOf(error);
        throw error;
      }
    },
    { preconnect: fetch.preconnect }
  );
  const polled = finishDeviceLogin(started, {
    path: join(inputs.state, "device-login.json"),
    sleep: (ms) => pause(ms, stop.signal),
    fetch: pollFetch,
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
  try {
    recordApproval(inputs.state, started.userCode, approval.credential);
  } catch (error) {
    // Named on stderr already, the credential is left for a person to end; the poll for its login
    // stops here, and `run` reads the error as one of its own.
    stop.abort(error);
    clearTimeout(deadline);
    throw error;
  }
  let login: KeptDeviceLogin;
  try {
    login = await polled;
  } catch (error) {
    if (error instanceof DeviceLoginNotKeptError) {
      // The broker answered the approval, and this machine could not keep the login in
      // OUTSIDE_EDGE_STATE: an error of the driver's own, which judged nothing of the edge. The
      // broker client's words name the file by its own setting, which the driver does not set, so
      // the row names the state's file instead. Where a save kept a whole copy of a login beside
      // that file (this one's, or an earlier run's in the same state), the driver, which uses no
      // copy, removes it: each credential is in approved.tsv, recorded before its poll, for
      // `revoke` to end, and a copy would keep its login's key and refresh token.
      const copies = removeKeptCopies(inputs.state);
      const removed =
        copies === 0
          ? ""
          : `; it removed ${copies === 1 ? "the copy of a login" : `${copies} copies of logins`} the broker client kept beside that file`;
      rows.add({
        row: "device-login",
        want,
        got: `approved on the page as credential ${approval.credential}, and the poll answered, but the driver could not keep the login in OUTSIDE_EDGE_STATE's device-login.json, an error of its own${removed}; revoke ends credential ${approval.credential}`,
        verdict: "UNKNOWN",
      });
      return undefined;
    }
    const unanswered = error === late && !lastAnswered;
    rows.add({
      row: "device-login",
      want,
      got: !unanswered
        ? `approved on the page, then the poll: ${messageOf(error)}`
        : answered === 0
          ? `approved on the page, then the broker answered no poll within ${deadlineSaid}: ${lastUnanswered}`
          : `approved on the page, then the broker answered ${answered} of the ${polls} polls, but not the last before the ${deadlineSaid} deadline: ${lastUnanswered}`,
      verdict: unanswered || unreached(error) ? "UNKNOWN" : "FAIL",
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

/** Where a session of the person's reaches the edge: the broker it trades at, the listener it
 *  registers at, the person's one project, and the state directory whose device login it trades. */
type SessionPlace = Pick<RunInputs, "broker" | "listener" | "project" | "state">;

/** A line of the driver's own about a session, on stderr. */
function log(line: string): void {
  console.error(`outside-edge: ${line}`);
}

/** The run's own outage report: each refusal logged, on stderr, as it comes. A session host
 *  tells its person once per outage (`inboundUnavailable`); a one-shot run has nobody to spare. */
const outage: InboundUnavailable = {
  refused: (error) => log(`the relay socket or the registration was refused: ${messageOf(error)}`),
  accepted: () => undefined,
};

/** `session`'s broker credential, traded for at the broker with the device login `run` kept. */
function sessionCredential(place: SessionPlace, session: string): RequestCredential {
  return brokerCredential(
    {
      AGENT_SECRETS_URL: place.broker,
      AGENT_SECRETS_MACHINE_IDENTITY: "device-login",
      AGENT_SECRETS_DEVICE_LOGIN_FILE: join(place.state, "device-login.json"),
      AGENT_SECRETS_PROJECTS: place.project,
    },
    session
  );
}

/** The relay delivery of `session` over `socket`, each frame handed to `deliver`. */
function sessionDelivery(
  session: string,
  socket: ListenerRelay,
  deliver: (frame: ConsumerFrame) => void
): ConsumerDelivery {
  const encoder = new TextEncoder();
  return createConsumerDelivery({
    sessionID: session,
    ledger: { delivered: () => false, liveFailed: async () => false },
    persist: () => true,
    publish: (subject, body) => socket.publish(subject, encoder.encode(body)),
    host: {
      deliver,
      gap: (notice) => log(notice.message),
      log: (message, error) =>
        log(error === undefined ? message : `${message}: ${messageOf(error)}`),
    },
  });
}

/** How long the relay socket's flush before a registration may take: one round trip, which the
 *  listener answers once it has read the subscription sent before it. The flush waits for an open
 *  socket first, which for one that closes as it opens is a wait with no end of its own. */
const FLUSH_MS = 10_000;

/** A registration that was never sent, so the listener holds nothing of it: its flush failed or
 *  got no answer within FLUSH_MS, or `stop` ended the wait first (`byStop`). */
class NothingSent extends Error {
  constructor(
    message: string,
    readonly byStop: boolean,
    options?: ErrorOptions
  ) {
    super(message, options);
  }
}

/** Rejects with `signal`'s reason once it aborts. */
function aborted(signal: AbortSignal): Promise<never> {
  const { promise, reject } = Promise.withResolvers<never>();
  if (signal.aborted) reject(signal.reason);
  else signal.addEventListener("abort", () => reject(signal.reason), { once: true });
  return promise;
}

/** The session's relay subject, held on its relay socket (`ListenerRelay.subscribe`, which the
 *  relay client takes again on every socket it opens after), each frame of it handed to
 *  `delivery`. A listener that refuses the subject answers an `err` frame, which ends the
 *  subscription with an error: `refused` rejects with it, and `failure` answers it once it came,
 *  so the refusal fails the registration that waits on it, and nothing leaves it unread. */
interface RelaySubject {
  readonly refused: Promise<never>;
  failure(): unknown;
}

function holdRelaySubject(socket: ListenerRelay, delivery: ConsumerDelivery): RelaySubject {
  const subscription = socket.subscribe(delivery.subject);
  const refused = Promise.withResolvers<never>();
  // Read by whichever wait it ends, and by none where none is waiting: never unhandled.
  refused.promise.catch(() => undefined);
  let failed: unknown;
  void (async () => {
    try {
      for await (const message of subscription) delivery.receive(message);
    } catch (error) {
      failed = error;
      refused.reject(error);
    }
  })();
  return { refused: refused.promise, failure: () => failed };
}

/** Registers `session` at the listener on the socket that holds its relay subject, as a session
 *  host does after each open: a flush first, so the listener has read the subject's `sub` before
 *  the registration asks for its consumer, then the registration (`sendRegistration`). Throws
 *  where the listener did not take it: `NothingSent` where no registration went out, because the
 *  flush failed, got no answer within FLUSH_MS or met a refusal of the subject, or because `stop`
 *  aborted first, while the flush waited or before the registration was sent. */
async function registerOnSocket(
  session: string,
  socket: ListenerRelay,
  client: EnvoyClient,
  delivery: ConsumerDelivery,
  subject: RelaySubject,
  stop?: AbortSignal
): Promise<void> {
  const bound = AbortSignal.timeout(FLUSH_MS);
  const flushed = socket.flush();
  // The flush a bound or a stop left rejects once the socket closes, and nothing reads that.
  flushed.catch(() => undefined);
  try {
    await Promise.race([
      flushed,
      subject.refused,
      aborted(stop === undefined ? bound : AbortSignal.any([bound, stop])),
    ]);
  } catch (error) {
    // What ended the wait: `stop`, whose reason the combined signal carries, the flush, or the
    // listener's refusal of the subject.
    if (stop !== undefined && error === stop.reason) {
      throw new NothingSent("stopped while the flush waited", true, { cause: error });
    }
    throw new NothingSent(
      bound.aborted
        ? `the relay socket's flush got no answer within ${FLUSH_MS / 1000} s, so no registration was sent`
        : `${messageOf(error)}, so no registration was sent`,
      false,
      { cause: error }
    );
  }
  await sendRegistration(session, client, delivery, stop);
}

/** The registration itself, which names the session's own agent subject alone and asks for
 *  consumer delivery on its relay subject; a renewal sends it again as it is, the subject being
 *  held already. Not sent once `stop` aborted (`NothingSent`). */
async function sendRegistration(
  session: string,
  client: EnvoyClient,
  delivery: ConsumerDelivery,
  stop?: AbortSignal
): Promise<void> {
  await registerWithRelay({
    relay: delivery,
    ownAgentSubject: legacyAgentSubject(session),
    followed: () => [],
    outage,
    send: async (topics, request) => {
      // A resend after a refusal, which left nothing registered, is not sent after a stop either.
      if (stop?.aborted === true) {
        throw new NothingSent("stopped before the registration was sent", true, {
          cause: stop.reason,
        });
      }
      return client.subscribe({
        sessionID: session,
        directory: "/outside-edge-e2e",
        topics,
        port: 0,
        title: "outside-edge-e2e",
        driving: false,
        delivery: request,
        plugin: { name: "outside-edge-e2e", version: "1" },
      });
    },
    answered: (result) => delivery.answered(result.delivery),
    // The session follows no topic, so a project refusal has nothing of it to drop: one that
    // names only its own agent subject fails the registration.
    drop: () => undefined,
    log,
  });
}

/** How long the end of a registration may take. */
const UNREGISTER_MS = 5_000;

/** Ends `session`'s registration at the listener, waiting at most UNREGISTER_MS: answers
 *  `unregistered`, or why not. */
async function unregister(client: EnvoyClient, session: string): Promise<string> {
  const bound = Promise.withResolvers<never>();
  const timer = setTimeout(
    () => bound.reject(new Error(`no answer within ${UNREGISTER_MS / 1000} s`)),
    UNREGISTER_MS
  );
  try {
    await Promise.race([
      client.unregisterSession({ sessionID: session, timeoutMs: UNREGISTER_MS }),
      bound.promise,
    ]);
    return "unregistered";
  } catch (error) {
    return `the unregister failed: ${messageOf(error).replaceAll(/\s+/g, " ")}`;
  } finally {
    clearTimeout(timer);
  }
}

/** Step 3: a message to the peer through the listener, and its reply on the relay socket. */
async function relay(inputs: RunInputs, rows: Rows): Promise<void> {
  const nonce = randomBytes(8).toString("hex");
  const want = `outside-e2e pong ${nonce} from ${inputs.peer} on the relay socket at ${inputs.listener} within ${inputs.replySeconds} s`;
  const session = `outside-e2e-${randomBytes(6).toString("hex")}`;
  // A session a run before this one kept is no session of this run's.
  rmSync(join(inputs.state, "session"), { force: true });
  const credential = sessionCredential(inputs, session);
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
  const client = createEnvoyClient({
    baseUrl: inputs.listener,
    fetch: globalThis.fetch,
    credential,
  });
  const delivery = sessionDelivery(session, socket, (frame) => {
    const envelope = EnvelopeSchema.safeParse(JSON.parse(frame.data));
    if (envelope.success) {
      const from = envelope.data.source_session ?? envelope.data.sender?.session_id;
      const text = (envelope.data.payload ?? envelope.data.payload_summary).trim();
      // The peer's answers: each its word and the nonce, and ` at <Unix ms>` after them.
      const says = (word: string) =>
        text === `outside-e2e ${word} ${nonce}` || text.startsWith(`outside-e2e ${word} ${nonce} `);
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
  });
  // Set while the session the listener took is written to OUTSIDE_EDGE_STATE: a write that fails
  // is an error of the driver's own, which judged nothing of the edge, so it is UNKNOWN.
  let keeping = false;
  try {
    await registerOnSocket(session, socket, client, delivery, holdRelaySubject(socket, delivery));
    // The session the listener took, for a later step that acts as this person's session.
    keeping = true;
    keep(inputs.state, "session", session);
    keeping = false;
    await client.send({
      sourceSessionID: session,
      targetSessionID: inputs.peer,
      message: `outside-e2e ping ${nonce} at ${Date.now()}`,
    });
  } catch (error) {
    rows.add(
      keeping
        ? {
            row: "relay",
            want,
            got: `the listener took ${session}, and the driver could not keep it, an error of its own: ${ownError(error, inputs.state)}`,
            verdict: "UNKNOWN",
          }
        : {
            row: "relay",
            want,
            got: `the session could not register or send: ${messageOf(error)}`,
            verdict: unreached(error) ? "UNKNOWN" : "FAIL",
          }
    );
    await leave(delivery, socket, client, session);
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
    await leave(delivery, socket, client, session);
  }
}

/** Leaves the listener: the registration, the delivery and the socket. Answers the unregister's
 *  own answer, `unregistered` or why not. */
async function close(
  delivery: ConsumerDelivery,
  socket: ListenerRelay,
  client: EnvoyClient,
  session: string
): Promise<string> {
  delivery.close();
  const unregistered = await unregister(client, session);
  await socket.close();
  return unregistered;
}

/** Step 3's end: `close`, and its unregister's answer printed, since a session the listener still
 *  holds stays registered until its record lapses, and the run's output says so. */
async function leave(
  delivery: ConsumerDelivery,
  socket: ListenerRelay,
  client: EnvoyClient,
  session: string
): Promise<void> {
  console.log(
    `outside-edge: session ${session}: ${await close(delivery, socket, client, session)}`
  );
}

async function run(env: NodeJS.ProcessEnv): Promise<number> {
  let asked: RunAsked;
  try {
    asked = runInputs(env);
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
  const { inputs } = asked;
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
  // The rows the sign-in steps do not make, each a line that is no row, so none reads PASS and no
  // verdict waits on it; printed first, so a run that stops names them too.
  if (asked.steps === SIGN_IN_STEPS) {
    const why = `OUTSIDE_EDGE_STEPS is ${SIGN_IN_STEPS}`;
    for (const row of ["device-login", "relay"]) {
      console.log(`outside-edge: not run: ${row}: ${why}`);
    }
    for (const limit of asked.unasked) {
      console.log(
        `outside-edge: not run: ${limitRow(limit)}: ${why}, which asks ${limit.host} nothing`
      );
    }
  }
  // Why the run's requests are not a whole run's, once a step was not run: step 4 does not pass a
  // threshold on such a count.
  let cut: string | undefined;
  const notRun = (row: string, want: string, why: string) => {
    cut ??= `the run did not reach ${row}: ${why}`;
    rows.add({ row, want, got: `not run: ${why}`, verdict: "UNKNOWN" });
  };
  let browser: Browser | undefined;
  try {
    browser = await launchBrowser();
    const context = await browser.newContext();
    countBrowser(context);
    const page = await context.newPage();
    const person = await signIn(page, context, inputs, rows);
    if (asked.steps === SIGN_IN_STEPS) {
      if (person === undefined) {
        notRun(
          "projects",
          `GET /api/v1/projects lists exactly [${inputs.project}]`,
          "the sign-in did not pass"
        );
      }
    } else {
      const every = asked.inputs;
      if (person === undefined) {
        notRun("device-login", "a device login approved on the page", "the sign-in did not pass");
        if (every.tailnetCode !== undefined)
          notRun("tailnet-code", `APPROVED ${every.tailnetCode}`, "the sign-in did not pass");
        notRun("relay", "a reply from the peer on the relay socket", "the sign-in did not pass");
      } else {
        const login = await deviceLogin(page, person, every, rows);
        if (every.tailnetCode !== undefined)
          await tailnetCode(page, every.tailnetCode, every, rows);
        if (login === undefined)
          notRun(
            "relay",
            "a reply from the peer on the relay socket",
            "the device login did not pass"
          );
        else await relay(every, rows);
      }
    }
  } catch (error) {
    // An error no step read, such as a page closed under a step, a browser that did not start, or
    // a file of OUTSIDE_EDGE_STATE the run could not write: the run judged nothing more, and its
    // text goes through the rows, which withhold the token. A file's error is named by its class
    // and the state's file (`ownError`), never by its message, which repeats the whole path.
    const own = error instanceof Error && typeof Reflect.get(error, "syscall") === "string";
    cut = `the run stopped: ${own ? ownError(error, inputs.state) : messageOf(error)}`;
    rows.add({
      row: "stopped",
      want: "every step judged",
      got: cut,
      verdict: "UNKNOWN",
    });
  } finally {
    await closeBrowser(browser);
  }
  const judged = judgeLimits(inputs.limits, counted, cut);
  for (const { host, perMinute } of judged.hostPeaks) {
    console.log(`outside-edge: peak ${host} ${perMinute} requests in 60 s`);
  }
  for (const row of judged.rows) rows.add(row);
  return rows.finish();
}

/** `approve <code>`: one more code, approved in the kept browser state. A browser that does not
 *  start, or a kept state it cannot load, is UNKNOWN (2), as a page it could not drive is. */
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
  let browser: Browser | undefined;
  let approval: Approval;
  try {
    browser = await launchBrowser();
    const context = await browser.newContext({ storageState: join(state, "browser.json") });
    approval = await approveOnPage(await context.newPage(), origin, code);
  } catch (error) {
    approval = { refused: `the browser could not be driven: ${messageOf(error)}`, unread: true };
  } finally {
    await closeBrowser(browser);
  }
  if ("credential" in approval) {
    recordApproval(state, code, approval.credential);
    console.log(`APPROVED ${code}`);
    return 0;
  }
  // One line, as every row is (`Rows`): the page's text may hold a line break, and a line it adds
  // could read as another answer, `APPROVED <code>` among them.
  console.log(
    `${approval.unread ? "UNKNOWN" : "FAIL"}: ${code} not approved: ${approval.refused}`.replaceAll(
      /[\t\r\n]+/g,
      " "
    )
  );
  return approval.unread ? 2 : 1;
}

/** The person's cookie `run` kept: refused where it is empty, since a read with it would be a
 *  read with no cookie, which says nothing of the person. */
function keptCookie(state: string): string {
  const cookie = kept(state, "cookie");
  if (cookie === "") throw new Refused("OUTSIDE_EDGE_STATE's cookie is empty: the run kept none");
  return cookie;
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
    cookie = keptCookie(state);
    person = kept(state, "person");
    const keep = (env.OUTSIDE_EDGE_REVOKE_KEEP ?? "").trim();
    if (keep !== "" && !USER_CODE.test(keep)) {
      throw new Refused("OUTSIDE_EDGE_REVOKE_KEEP is no device-login code (XXXX-XXXX)");
    }
    // One code in either form the driver takes, `ABCD-EFGH` or `abcdefgh`, as the broker reads a
    // typed code: upper-cased, the hyphen dropped.
    const bare = (code: string) => code.replaceAll("-", "").toUpperCase();
    approvals = kept(state, "approved.tsv")
      .split("\n")
      .filter((line) => line !== "")
      .map((line) => line.split("\t"))
      .filter(([code = ""]) => keep === "" || bare(code) !== bare(keep));
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

/** `grants`' list row: Dispatch's answer to a restricted member's own list of credential grants,
 *  200 holding only the grants of the sessions that act for them, each naming them as its
 *  enrollment's operator; every other grant dropped, whatever its approver. */
const GRANTS_LIST_STATUS = 200;
/** `grants`' revoke row passes on this alone: Dispatch's own refusal of a restricted member's
 *  revoke of a grant that is not of their own sessions, which relays nothing to the broker and
 *  answers an id no grant has the same. */
const GRANTS_REVOKE_REFUSAL = { status: 403, code: "RESTRICTED_MEMBER" } as const;
/** `grants`' revoke row fails on this, as on any 2xx: the broker's refusal of a revoke by a person
 *  the grant names nowhere, which reaches the person only where Dispatch relayed the revoke. */
const GRANTS_REVOKE_RELAYED = { status: 403, code: "NOT_APPROVER" } as const;
/** A credential grant's id, as the broker's revoke route takes it. */
const GRANT_ID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

/** An answer's status and the `code` its JSON body names, as one phrase. */
function statusWords(status: number, body: unknown): string {
  const code = member(body, "code");
  return typeof code === "string" && code !== "" ? `${status} ${code}` : `${status}`;
}

/** `grants`: what the person reaches, with the kept cookie, of the credential grants of sessions
 *  that do not act for them: the list, and a revoke of the grant OUTSIDE_GRANTS_PROBE_ID names. */
async function grants(env: NodeJS.ProcessEnv): Promise<number> {
  const listRow = "grants-list";
  const revokeRow = "grants-revoke";
  const tsvSet = (env.OUTSIDE_EDGE_GRANTS ?? "").trim();
  let state: string;
  let origin: string;
  let cookie: string;
  let person: string;
  try {
    state = required(env, "OUTSIDE_EDGE_STATE");
    origin = kept(state, "origin");
    cookie = keptCookie(state);
    person = kept(state, "person");
  } catch (error) {
    if (!(error instanceof Refused)) throw error;
    const rows = new Rows(tsvSet === "" ? undefined : tsvSet);
    for (const row of [listRow, revokeRow]) {
      rows.add({ row, want: "the run's kept cookie", got: error.message, verdict: "UNKNOWN" });
    }
    return rows.finish();
  }
  const rows = new Rows(tsvSet || join(state, "outside-edge-grants.tsv"));
  const probe = (env.OUTSIDE_GRANTS_PROBE_ID ?? "").trim();
  const self = person.toLowerCase();
  const headers = { Accept: "application/json", Cookie: cookie, Origin: origin };
  // The probe grant as the list below shows it, where it does: read again just before the revoke.
  let probeSeen: unknown;

  // The list. Every row whose session does not act for the person is one Dispatch let through;
  // the row names how many, and whether the probe grant is among them, and no grant's id or name.
  const wantList = `GET /api/v1/credential-grants?approver=me answers ${GRANTS_LIST_STATUS} holding only grants of sessions that act for ${person}`;
  try {
    const answer = await fetch(`${origin}/api/v1/credential-grants?approver=me`, {
      headers,
      signal: AbortSignal.timeout(PAGE_MS),
    });
    const body = await answer.json().catch(() => undefined);
    const listed = member(body, "grants");
    if (Array.isArray(listed))
      probeSeen = listed.find((grant) => member(grant, "grant_id") === probe);
    if (answer.status !== GRANTS_LIST_STATUS || !Array.isArray(listed)) {
      rows.add({
        row: listRow,
        want: wantList,
        got: `answered ${statusWords(answer.status, body)}${Array.isArray(listed) ? "" : ", with no grants list"}`,
        verdict: "UNKNOWN",
      });
    } else {
      const foreign = listed.filter((grant) => {
        const operator = member(member(grant, "enrollment"), "operator");
        return typeof operator !== "string" || operator.toLowerCase() !== self;
      });
      const probeWords =
        probe === ""
          ? ""
          : foreign.some((grant) => member(grant, "grant_id") === probe)
            ? "; the probe grant among them"
            : "; the probe grant not among them";
      rows.add({
        row: listRow,
        want: wantList,
        got: `listed ${listed.length} grants, ${foreign.length} of a session that does not act for ${person}${probeWords}`,
        verdict: foreign.length === 0 ? "PASS" : "FAIL",
      });
    }
  } catch (error) {
    rows.add({
      row: listRow,
      want: wantList,
      got: `GET /api/v1/credential-grants got no answer: ${messageOf(error)}`,
      verdict: "UNKNOWN",
    });
  }

  // The revoke of a grant the developer box chose as one no person may end, so a revoke Dispatch
  // lets through ends nothing either: judged by who refused it.
  const refusal = `${GRANTS_REVOKE_REFUSAL.status} ${GRANTS_REVOKE_REFUSAL.code}`;
  const wantRevoke = `the revoke of a grant of a session that does not act for ${person} is refused by Dispatch itself, ${refusal}`;
  if (probe === "" || !GRANT_ID.test(probe)) {
    rows.add({
      row: revokeRow,
      want: wantRevoke,
      got:
        probe === ""
          ? "OUTSIDE_GRANTS_PROBE_ID is not set, so no grant was named to revoke"
          : "OUTSIDE_GRANTS_PROBE_ID is no grant id (a UUID), so nothing was revoked",
      verdict: "UNKNOWN",
    });
    return rows.finish();
  }
  // The probe's precondition, read the moment before the revoke: where the list showed the probe
  // grant (as a list Dispatch let through does), it must still name no approver and no operator,
  // the grant no person may end at the broker. One that names either, the person among them, is
  // left alone.
  if (probeSeen !== undefined) {
    const fields = [
      member(probeSeen, "approver") === null ? "" : "approver",
      member(member(probeSeen, "enrollment"), "operator") === "" ? "" : "enrollment.operator",
    ].filter((field) => field !== "");
    if (fields.length > 0) {
      rows.add({
        row: revokeRow,
        want: wantRevoke,
        got: `the list shows the probe grant with ${fields.join(" and ")} set, so a revoke let through could end it; nothing was revoked`,
        verdict: "UNKNOWN",
      });
      return rows.finish();
    }
  }
  try {
    const answer = await fetch(
      `${origin}/api/v1/credential-grants/${encodeURIComponent(probe)}/revoke`,
      {
        method: "POST",
        headers: { ...headers, "Content-Type": "application/json" },
        body: "{}",
        signal: AbortSignal.timeout(PAGE_MS),
      }
    );
    const body = await answer.json().catch(() => undefined);
    const said = statusWords(answer.status, body);
    if (answer.ok) {
      rows.add({
        row: revokeRow,
        want: wantRevoke,
        got: `answered ${said}: Dispatch let the revoke through, and the broker took it`,
        verdict: "FAIL",
      });
    } else if (said === refusal) {
      rows.add({ row: revokeRow, want: wantRevoke, got: `refused ${said}`, verdict: "PASS" });
    } else if (said === `${GRANTS_REVOKE_RELAYED.status} ${GRANTS_REVOKE_RELAYED.code}`) {
      rows.add({
        row: revokeRow,
        want: wantRevoke,
        got: `answered ${said}, the broker's refusal: Dispatch relayed the revoke to the broker`,
        verdict: "FAIL",
      });
    } else {
      rows.add({ row: revokeRow, want: wantRevoke, got: `answered ${said}`, verdict: "UNKNOWN" });
    }
  } catch (error) {
    rows.add({
      row: revokeRow,
      want: wantRevoke,
      got: `the revoke got no answer: ${messageOf(error)}`,
      verdict: "UNKNOWN",
    });
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
 * page signed the person in again or that request answered 2xx, whether or not its body came,
 * and UNKNOWN otherwise: not tried, an alert with no answer to that request, any other answer
 * whose body did not come in before the window's end (every wait of the sign-in, that read among
 * them, ends 2 s before the deadline), or any other answer.
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
    cookie = keptCookie(state);
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
  let browser: Browser | undefined;
  try {
    browser = await launchBrowser();
    const page = await (await browser.newContext()).newPage();
    const passkey = await addVirtualPasskey(page);
    await passkey.cdp.send("WebAuthn.addCredential", {
      authenticatorId: passkey.authenticatorId,
      credential,
    });
    // Each answer of the sign-in's last request as it comes: its status at once, and the code its
    // body names once the body is in, a read the window's end bounds below. The page shows its
    // alert only after that answer, so one the page asked is in by the time the alert is read.
    const finishes: { readonly status: number; readonly code: Promise<unknown> }[] = [];
    page.on("response", (response) => {
      if (response.request().method() !== "POST") return;
      if (new URL(response.url()).pathname !== PASSKEY_FINISH) return;
      finishes.push({
        status: response.status(),
        code: response.json().then(
          (body: unknown) => member(body, "code"),
          () => undefined
        ),
      });
    });
    const bound = () => Math.max(1_000, Math.min(PAGE_MS, left()));
    await page.goto(`${origin}/`, { timeout: bound() });
    const button = page.getByRole("button", { name: "Sign in with a passkey" });
    await button.waitFor({ timeout: bound() });
    await button.click({ timeout: bound() });
    const alert = page.getByRole("alert").first();
    const waited = bound();
    const shown = await settledWithin(
      "the sign-in with the restored passkey",
      [
        ["signed-in", page.getByText(/^Signed in as /).first()],
        ["refused", alert],
      ],
      waited
    );
    const at = seconds();
    // The last answer's body, read for what is left of the window at most: Playwright reads a
    // response's body with no deadline of its own, so an edge that sends the status and then
    // holds the body back would hold the run past the deadline with no row written.
    const last = finishes.at(-1);
    const rest = Math.max(0, left());
    const code =
      last === undefined || shown === "signed-in" ? undefined : await within(last.code, rest);
    const late = code === TIME_UP;
    const answered =
      last === undefined
        ? ""
        : `POST ${PASSKEY_FINISH} answered ${last.status}${late ? "" : ` ${typeof code === "string" ? code : "with no code"}`}`;
    const bodyLate = `its body did not come in the ${Math.round(rest / 1000)} s left before the deadline`;
    if (shown === "signed-in") {
      rows.add({
        row: "T5-passkey",
        want: wantPasskey,
        got: `the passkey signed the person in again at=${at}, after the cookie's 401`,
        verdict: "FAIL",
      });
    } else if (last !== undefined && last.status >= 200 && last.status < 300) {
      // A 2xx is a sign-in whatever its body or the page says next: its status is the answer,
      // and it came, so a body held back proves nothing more.
      const said =
        shown === "refused"
          ? `; the page said: ${(await alert.innerText({ timeout: bound() })).trim()}`
          : "";
      rows.add({
        row: "T5-passkey",
        want: wantPasskey,
        got: `${answered} at=${at}, a sign-in after the cookie's 401${late ? `, though ${bodyLate}` : ""}${said}`,
        verdict: "FAIL",
      });
    } else if (late) {
      rows.add({
        row: "T5-passkey",
        want: wantPasskey,
        got: `${answered} at=${at}, and ${bodyLate}, so no refusal of the passkey was read`,
        verdict: "UNKNOWN",
      });
    } else if (shown === undefined) {
      rows.add({
        row: "T5-passkey",
        want: wantPasskey,
        got: `the sign-in showed neither a sign-in nor a refusal within ${Math.round(waited / 1000)} s`,
        verdict: "UNKNOWN",
      });
    } else {
      const said = (await alert.innerText({ timeout: bound() })).trim();
      if (last === undefined) {
        rows.add({
          row: "T5-passkey",
          want: wantPasskey,
          got: `no answer to POST ${PASSKEY_FINISH} at=${at}, so no refusal of the passkey was read; the page said: ${said}`,
          verdict: "UNKNOWN",
        });
      } else if (
        typeof code === "string" &&
        Object.hasOwn(PASSKEY_REFUSED, code) &&
        PASSKEY_REFUSED[code] === last.status
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
    }
  } catch (error) {
    rows.add({
      row: "T5-passkey",
      want: wantPasskey,
      got: `the sign-in showed neither a sign-in nor a refusal: ${messageOf(error)}`,
      verdict: "UNKNOWN",
    });
  } finally {
    await closeBrowser(browser);
  }
  return rows.finish();
}

/** What `hold` reads: where the session reaches the edge, the session `run` kept, how long it is
 *  held at most, and how often its registration is renewed. */
interface HoldInputs extends SessionPlace {
  readonly session: string;
  readonly seconds: number;
  readonly renewMs: number;
}

/** A session `run` keeps (`relay`). */
const KEPT_SESSION = /^outside-e2e-[0-9a-f]{12}$/;
/** How long a hold lasts when OUTSIDE_EDGE_HOLD_SECONDS is unset, and the least and most it may
 *  name: a check that acts as the person's session holds it while it runs. */
const HOLD_SECONDS = 1200;
const HOLD_LEAST_SECONDS = 60;
const HOLD_MOST_SECONDS = 3600;
/** How often a held registration is renewed when OUTSIDE_EDGE_RENEW_SECONDS is unset, and the
 *  most that may name: as a session host renews its own (`DEFAULT_HEARTBEAT_MS`, 90 s), under a
 *  third of the listener's window (`SESSION_LIVE_WINDOW_MS`, 300 s), past which the listener's
 *  record of the session lapses and is relayed no more. */
const RENEW_MOST_SECONDS = DEFAULT_HEARTBEAT_MS / 1000;
/** The close codes after which the relay client opens the held socket again as a routine, and the
 *  hold registers the session again on the new socket: 4401, the socket's token lapsed (an edge
 *  relay token lasts at most 300 s, and the listener closes the socket within 25 s of its
 *  expiry), and 1012, the listener is stopping. The relay client opens again at once after either
 *  (`@legion/envoy-client/listener-relay`'s header). Any other close, 4403 for a revoked token
 *  among them, ends the hold. */
const ROUTINE_CLOSES: ReadonlySet<number> = new Set([4401, 1012]);
/** How long `release` waits for the holder to end after its SIGTERM. */
const RELEASE_MS = 15_000;
/** The relay client's line for a socket the listener closed (`ListenerRelay`'s own), which names
 *  the close code and reason. */
const CLOSED_LINE = / closed \((.+)\); it opens again in [0-9]+ ms$/;
/** What a hold keeps in OUTSIDE_EDGE_STATE: `holder`, its pid and start, from its start, which
 *  `release` signals and, once that hold has ended, removes; `held`, the same, once the listener
 *  took the registration, which a check waits for, until the hold ends; and `released`, how the
 *  hold ended. */
const HOLDER = "holder";
const HELD = "held";
const RELEASED = "released";

/** Removes `held` from `state` once no hold holds: one an ended hold left would read, to a check
 *  that waits on a later hold of the directory, as that hold's. A `held` that is no file is none a
 *  hold wrote (it could not write it), and is left as it is. */
function dropHeld(state: string): void {
  const path = join(state, HELD);
  if (lstatSync(path, { throwIfNoEntry: false })?.isFile() === true) rmSync(path);
}

function holdInputs(env: NodeJS.ProcessEnv): HoldInputs {
  const allowHttp = env.OUTSIDE_EDGE_ALLOW_HTTP === "1";
  const state = required(env, "OUTSIDE_EDGE_STATE");
  const project = required(env, "OUTSIDE_PROJECT");
  if (!PROJECT_KEY.test(project)) throw new Refused("OUTSIDE_PROJECT is no project key");
  const broker = originOf("OUTSIDE_BROKER_URL", required(env, "OUTSIDE_BROKER_URL"), allowHttp);
  const listener = originOf(
    "OUTSIDE_LISTENER_URL",
    required(env, "OUTSIDE_LISTENER_URL"),
    allowHttp
  );
  const held = (env.OUTSIDE_EDGE_HOLD_SECONDS ?? "").trim() || String(HOLD_SECONDS);
  const seconds = Number(held);
  if (!/^[0-9]+$/.test(held) || seconds < HOLD_LEAST_SECONDS || seconds > HOLD_MOST_SECONDS) {
    throw new Refused(
      `OUTSIDE_EDGE_HOLD_SECONDS must be a whole number of seconds from ${HOLD_LEAST_SECONDS} to ${HOLD_MOST_SECONDS}`
    );
  }
  const renew = (env.OUTSIDE_EDGE_RENEW_SECONDS ?? "").trim() || String(RENEW_MOST_SECONDS);
  const renewSeconds = Number(renew);
  if (!/^[0-9]+$/.test(renew) || renewSeconds < 1 || renewSeconds > RENEW_MOST_SECONDS) {
    throw new Refused(
      `OUTSIDE_EDGE_RENEW_SECONDS must be a whole number of seconds from 1 to ${RENEW_MOST_SECONDS}`
    );
  }
  const session = kept(state, "session");
  if (!KEPT_SESSION.test(session)) {
    throw new Refused("OUTSIDE_EDGE_STATE's session is no session the driver kept");
  }
  if (!existsSync(join(state, "device-login.json"))) {
    throw new Refused("OUTSIDE_EDGE_STATE holds no device-login.json: run the driver first");
  }
  return {
    state,
    session,
    project,
    broker: broker.origin,
    listener: listener.origin,
    seconds,
    renewMs: renewSeconds * 1000,
  };
}

/** How a held session's hold ended: at SIGTERM; once its time passed; at a close of its socket
 *  that is no routine one (`ROUTINE_CLOSES`); or with its registration lost, `why` saying how: a
 *  renewal or a registration on a reopened socket that the listener did not take, a socket that
 *  did not open again in time, or the listener's refusal of its relay subject. */
type HoldEnd =
  | { readonly by: "SIGTERM" | "time" }
  | { readonly by: "close" | "lost"; readonly why: string };

/**
 * `hold`: the session `run` kept, registered at the listener again and held live, for a check that
 * acts as the person's session. It writes its pid and its start (field 22 of `/proc/<pid>/stat`)
 * to `holder` first, so `release` can end it from then on. It trades for the session's broker
 * credential with the kept device login, opens its relay socket and registers it as step 3 does;
 * once the listener took the registration it writes the same to `held` and prints
 * `HELD <session>`.
 *
 * While held, the session stays registered as a session host keeps its own: the registration is
 * renewed every OUTSIDE_EDGE_RENEW_SECONDS (90, a host's heartbeat, when unset), and a routine
 * close of the socket (`ROUTINE_CLOSES`: 4401, its token lapsed, which an edge relay token does
 * within 300 s; 1012, the listener stops) is followed by the relay client's reopen, after which
 * the session registers again on the new socket. Any other close, a reopen that does not come
 * within OPEN_MS, a renewal or registration the listener does not take, and a refusal of the
 * relay subject each end the hold: it prints `UNKNOWN: …` naming why, ends the registration,
 * writes `released` naming why, and exits 2.
 *
 * At SIGTERM, or once OUTSIDE_EDGE_HOLD_SECONDS have passed, it closes the socket first, so the
 * relay client opens none again and no registration follows, waits for a registration already on
 * the wire, which gets no second attempt after the end (its client's requests stop with the
 * hold), unregisters the session (at most UNREGISTER_MS), writes `released`, and prints
 * `RELEASED <session>`. `released` says `unregistered` only where the session was held registered
 * to the end, its last registration the listener took within the listener's window
 * (SESSION_LIVE_WINDOW_MS), and the listener answered the unregister: exit 0. A failed unregister
 * is `released` naming it, exit 2; a registration that lapsed is `UNKNOWN: …`, exit 2. Every end
 * of a hold that wrote `held` removes it as the end begins (`dropHeld`).
 *
 * A SIGTERM before the session is held holds nothing, so a holder its caller gives up on ends
 * before its caller sends SIGKILL: while the socket opens or its flush waits, the hold sends no
 * registration and exits 2 at once; while the listener answers one, the hold waits for that
 * answer (the client's bound), unregisters the session (UNREGISTER_MS) and exits 2. Neither
 * writes `held`; each writes `released` naming why.
 *
 * An error of the hold's own, not an answer of the listener's (an OUTSIDE_EDGE_STATE it cannot
 * write, say), ends it `UNKNOWN: the hold failed on an error of its own: …` and exit 2, as every
 * end that judged nothing of the edge does, never 1, a FAIL (`ownError` names it); a session the
 * listener took by then is unregistered first.
 */
async function hold(env: NodeJS.ProcessEnv): Promise<number> {
  // Taken from the start: a SIGTERM before the session is held stops the hold before it sends a
  // registration, or ends the registration it sent. `stop` aborts at every end, so no
  // registration is sent once the hold is ending.
  const stop = new AbortController();
  const ended = Promise.withResolvers<HoldEnd>();
  let end: HoldEnd | undefined;
  const finish = (how: HoldEnd) => {
    if (end !== undefined) return;
    end = how;
    stop.abort(new Error("why" in how ? how.why : how.by));
    ended.resolve(how);
  };
  const terminated = () => {
    log("hold got SIGTERM");
    finish({ by: "SIGTERM" });
  };
  process.on("SIGTERM", terminated);
  const named = (env.OUTSIDE_EDGE_STATE ?? "").trim();
  // What an error of the hold's own must end before the hold exits: the registration the listener
  // took, from then until the hold's end unregisters it, which would otherwise outlive the hold.
  let leaveOnError: (() => Promise<string>) | undefined;
  try {
    if (named !== "") {
      // A hold of this directory that still runs keeps its records: this one would take its place
      // in them, and a `release` would then end this one and leave that one holding. So this one
      // holds nothing, and leaves them.
      const holderFile = join(named, HOLDER);
      const earlier =
        lstatSync(holderFile, { throwIfNoEntry: false })?.isFile() === true
          ? /^([1-9][0-9]*) ([0-9]+)$/.exec(readFileSync(holderFile, "utf8").trim())
          : null;
      if (earlier !== null && holderRuns(Number(earlier[1]), earlier[2] ?? "")) {
        console.log(
          `UNKNOWN: no session held: the hold ${earlier[1]} still holds OUTSIDE_EDGE_STATE; release it first`
        );
        return 2;
      }
      // Any other hold before this one ended, released or not, and what it wrote says nothing of
      // this one: gone before any other input is read, so a `release` once this one has started
      // reads this one's end, never that one's, and a hold refused below leaves none of them.
      for (const file of [HOLDER, HELD, RELEASED]) rmSync(join(named, file), { force: true });
    }
    let inputs: HoldInputs;
    try {
      inputs = holdInputs(env);
    } catch (error) {
      if (!(error instanceof Refused)) throw error;
      console.log(`UNKNOWN: no session held: ${error.message}`);
      return 2;
    }
    const { state, session } = inputs;
    // `release` signals the pid `holder` names only while that pid's process started when this
    // one did, so a pid another process took after this one ended is never signalled.
    const holder = procStat(process.pid)?.start;
    if (holder === undefined) {
      console.log(
        "UNKNOWN: no session held: /proc/self/stat could not be read, which names this holder's start"
      );
      return 2;
    }
    keep(state, HOLDER, `${process.pid} ${holder}`);
    // An end before the session is held: printed, and written to `released`, which a `release`
    // that stopped this hold reads.
    const unheld = (why: string): number => {
      keep(state, RELEASED, why);
      console.log(`UNKNOWN: ${why}`);
      return 2;
    };
    const credential = sessionCredential(inputs, session);
    // Each close of the held socket, as the relay client logs it just after it reports the close
    // (`disconnect`): one line a close, read in order with those reports below.
    const closes: string[] = [];
    const logRelay = (line: string) => {
      log(line);
      const closed = CLOSED_LINE.exec(line);
      if (closed?.[1] !== undefined) closes.push(closed[1]);
    };
    const notSent = `no session held: SIGTERM came before ${session} was registered, and no registration was sent`;
    // The open ends at OPEN_MS, or at a SIGTERM.
    const opening = AbortSignal.any([AbortSignal.timeout(OPEN_MS), stop.signal]);
    let socket: ListenerRelay;
    try {
      socket = await connectListenerRelay(
        { url: inputs.listener, session, credential, outage, log: logRelay },
        opening
      );
    } catch (error) {
      return unheld(
        end?.by === "SIGTERM"
          ? notSent
          : `the relay socket of ${session} did not open: ${messageOf(error)}`
      );
    }
    // Read from now, so no report of the socket is missed, and taken in order once it is held.
    const statuses = socket.status();
    const client = createEnvoyClient({
      baseUrl: inputs.listener,
      fetch: globalThis.fetch,
      credential,
    });
    // Every registration goes through a client of its own whose requests stop with the hold: one
    // on the wire when the hold ends is answered or not, and no attempt of it is sent after the end,
    // the transport's second attempt of one that got no answer among them. The unregister, which
    // the end sends, goes through `client`.
    const registrar = createEnvoyClient({
      baseUrl: inputs.listener,
      fetch: Object.assign(
        (input: string | URL | Request, init?: RequestInit) =>
          stop.signal.aborted
            ? Promise.reject(new Error("the hold is ending, so no registration is sent"))
            : fetch(input, init),
        { preconnect: fetch.preconnect }
      ),
      credential,
    });
    // The held session reads nothing itself: each frame is taken, so none waits on it.
    const delivery = sessionDelivery(session, socket, (frame) => frame.done());
    const subject = holdRelaySubject(socket, delivery);
    // When the registration the listener last took was sent.
    let registeredAt = Date.now();
    try {
      await registerOnSocket(session, socket, registrar, delivery, subject, stop.signal);
    } catch (error) {
      if (error instanceof NothingSent) {
        // Nothing was sent, so there is nothing to unregister.
        delivery.close();
        await socket.close();
        return unheld(
          error.byStop ? notSent : `the listener did not register ${session}: ${messageOf(error)}`
        );
      }
      const unregistered = await close(delivery, socket, client, session);
      // A SIGTERM while the registration was before the listener holds nothing, whatever came of
      // the registration: an attempt it would have sent after the SIGTERM was not sent.
      return unheld(
        end === undefined
          ? `the listener did not register ${session}: ${messageOf(error)}; ${unregistered}`
          : `no session held: SIGTERM came while the listener answered the registration of ${session}; ${unregistered}`
      );
    }
    const refusedSubject = subject.failure();
    if (end !== undefined || refusedSubject !== undefined) {
      // The SIGTERM came while the listener answered, or the listener refused the subject after
      // it took the registration: the session it took is ended at once.
      delivery.close();
      const unregistered = await unregister(client, session);
      await socket.close();
      return unheld(
        end !== undefined
          ? `no session held: SIGTERM came while the listener answered the registration of ${session}; ${unregistered}`
          : `the listener refused the relay subject of ${session} after it took the registration: ${messageOf(refusedSubject)}; ${unregistered}`
      );
    }
    leaveOnError = () => close(delivery, socket, client, session);
    keep(state, HELD, `${process.pid} ${holder}`);
    console.log(`HELD ${session}`);

    // Held: renewed at its cadence, registered again on each socket a routine close reopens, one
    // registration after another, and lost at the first the listener does not take.
    let renewal: Timer | undefined;
    let reopening: { readonly why: string; readonly timer: Timer } | undefined;
    let registering: Promise<void> = Promise.resolve();
    const registerAgain = (what: string, onSocket: boolean): void => {
      registering = registering.then(async () => {
        if (end !== undefined) return;
        const sent = Date.now();
        try {
          if (onSocket) {
            await registerOnSocket(session, socket, registrar, delivery, subject, stop.signal);
          } else {
            await sendRegistration(session, registrar, delivery, stop.signal);
          }
        } catch (error) {
          // A hold that is ending says why itself; a routine close that came while this one
          // flushed reopens the socket, and the session registers again on the new one.
          if (end !== undefined || (error instanceof NothingSent && reopening !== undefined)) {
            return;
          }
          finish({ by: "lost", why: `${what} failed: ${messageOf(error)}` });
          return;
        }
        registeredAt = sent;
        log(`the listener took ${what} of ${session}`);
        clearTimeout(renewal);
        renewal = setTimeout(() => registerAgain("the renewal", false), inputs.renewMs);
      });
    };
    renewal = setTimeout(() => registerAgain("the renewal", false), inputs.renewMs);
    subject.refused.catch((error: unknown) =>
      finish({ by: "lost", why: `the listener refused its relay subject: ${messageOf(error)}` })
    );
    void (async () => {
      for await (const status of statuses) {
        if (status.type === "disconnect") {
          const why = closes.shift() ?? "no close code was logged";
          if (!ROUTINE_CLOSES.has(Number.parseInt(why, 10))) {
            finish({ by: "close", why });
            return;
          }
          clearTimeout(renewal);
          clearTimeout(reopening?.timer);
          reopening = {
            why,
            timer: setTimeout(
              () =>
                finish({
                  by: "lost",
                  why: `its relay socket did not open again within ${OPEN_MS / 1000} s of the close (${why})`,
                }),
              OPEN_MS
            ),
          };
          log(
            `the listener closed the held socket of ${session} (${why}), a routine close: it opens again, and ${session} registers again on it`
          );
        } else if (status.type === "reconnect" && reopening !== undefined) {
          clearTimeout(reopening.timer);
          const after = reopening.why;
          reopening = undefined;
          registerAgain(
            `the registration on its new relay socket after the close (${after})`,
            true
          );
        }
      }
    })();
    const timer = setTimeout(() => finish({ by: "time" }), inputs.seconds * 1000);
    const how = await ended.promise;
    // No longer held from here, however the end goes on.
    dropHeld(state);
    clearTimeout(timer);
    clearTimeout(renewal);
    clearTimeout(reopening?.timer);
    // Closed first, so the relay client opens no socket again and no registration follows the end.
    await socket.close();
    delivery.close();
    // A registration on the wire when the hold ended is answered before the unregister, which it
    // would otherwise write back; `stop` keeps any other from being sent.
    await registering;
    const unregistered = await unregister(client, session);
    leaveOnError = undefined;
    if (how.by === "close") {
      keep(state, RELEASED, `the listener closed the held socket (${how.why}); ${unregistered}`);
      console.log(
        `UNKNOWN: the listener closed the held socket of ${session} (${how.why}); it was not registered again`
      );
      return 2;
    }
    if (how.by === "lost") {
      const why = `the hold of ${session} lost its registration: ${how.why}; ${unregistered}`;
      keep(state, RELEASED, why);
      console.log(`UNKNOWN: ${why}`);
      return 2;
    }
    log(
      `the hold of ${session} ends: ${how.by === "time" ? `${inputs.seconds} s passed` : "SIGTERM"}`
    );
    // Held registered to the end: no registration was lost, and the last the listener took is
    // inside its window, which a hold whose process stalled past its renewals would not be.
    const age = Date.now() - registeredAt;
    if (age >= SESSION_LIVE_WINDOW_MS) {
      const why = `the registration of ${session} lapsed before the hold ended: the last the listener took was sent ${Math.round(age / 1000)} s before; ${unregistered}`;
      keep(state, RELEASED, why);
      console.log(`UNKNOWN: ${why}`);
      return 2;
    }
    keep(state, RELEASED, unregistered);
    console.log(`RELEASED ${session}`);
    return unregistered === "unregistered" ? 0 : 2;
  } catch (error) {
    // An error of the hold's own, not an answer of the listener's (an OUTSIDE_EDGE_STATE it cannot
    // write, say): the hold judged nothing of the edge, so it is UNKNOWN, exit 2, never a FAIL,
    // and a session it registered is unregistered first.
    const left = leaveOnError === undefined ? "" : `; ${await leaveOnError()}`;
    if (leaveOnError !== undefined) dropHeld(named);
    console.log(
      `UNKNOWN: the hold failed on an error of its own: ${ownError(error, named)}${left}`
    );
    return 2;
  } finally {
    process.off("SIGTERM", terminated);
  }
}

/** What an error a command met of its own says, as its output may print it: its class
 *  (`errorClass`: the syscall and the code of a file's), and OUTSIDE_EDGE_STATE (`state`), or the
 *  file of it, where the error names that path; never its message, which repeats the whole path. */
function ownError(error: unknown, state: string): string {
  const path = error instanceof Error && "path" in error ? error.path : undefined;
  if (typeof path !== "string" || state === "") return errorClass(error);
  const at = resolvePath(path);
  if (at === resolvePath(state)) return `${errorClass(error)} on OUTSIDE_EDGE_STATE`;
  if (dirname(at) === resolvePath(state)) {
    return `${errorClass(error)} on OUTSIDE_EDGE_STATE's ${basename(at)}`;
  }
  return errorClass(error);
}

/** What `/proc/<pid>/stat` says of process `pid`: its state (field 3) and its start, in clock
 *  ticks since boot (field 22); undefined where it names no process. */
function procStat(pid: number): { readonly state: string; readonly start: string } | undefined {
  let stat: string;
  try {
    stat = readFileSync(`/proc/${pid}/stat`, "utf8");
  } catch {
    return undefined;
  }
  // The fields after the name, which is in parentheses and may hold either: field 3 first.
  const fields = stat.slice(stat.lastIndexOf(")") + 2).split(" ");
  const [state] = fields;
  const start = fields[19];
  return state === undefined || start === undefined ? undefined : { state, start };
}

/** Whether the holder `holder` names still runs: the process at `pid` is the one that started at
 *  `start`, and it has not ended (a zombie, ended and not yet reaped, has). */
function holderRuns(pid: number, start: string): boolean {
  const stat = procStat(pid);
  return stat !== undefined && stat.start === start && stat.state !== "Z";
}

/**
 * `release`: ends the hold whose process `holder` names, by its pid and its start, which the hold
 * writes before anything else, so a hold not yet holding is ended as a holding one is. It sends
 * that process SIGTERM and waits up to RELEASE_MS for it to end, then prints `RELEASED <session>`
 * (0) when `released` says `unregistered`, which the hold writes only where it held the session
 * registered to its end and the listener answered the unregister, else `UNKNOWN: <why>` (2): no
 * `holder` at all, `released` naming why, or a holder still running at the end of the wait, which
 * is sent SIGKILL. A pid whose process did not start when `holder` says is no holder's any more,
 * and is not signalled: the held process is gone. A hold that ended wrote `released`, and its pid
 * is not signalled either. Once the hold has ended, however it ended, `holder` is spent: release
 * removes it, and a `held` a killed hold left, so a release after this one, as one before a later
 * hold of the directory has started, reads no hold rather than this hold's end. `released` stays,
 * for a check to read.
 */
async function release(env: NodeJS.ProcessEnv): Promise<number> {
  let state: string;
  let pid: number;
  let start: string;
  let session: string;
  try {
    state = required(env, "OUTSIDE_EDGE_STATE");
    if (!existsSync(join(state, HOLDER))) {
      console.log("UNKNOWN: no session is held");
      return 2;
    }
    const named = /^([1-9][0-9]*) ([0-9]+)$/.exec(kept(state, HOLDER));
    if (named === null) {
      throw new Refused("OUTSIDE_EDGE_STATE's holder names no process and its start");
    }
    pid = Number(named[1]);
    start = named[2] ?? "";
    session = kept(state, "session");
  } catch (error) {
    if (!(error instanceof Refused)) throw error;
    console.log(`UNKNOWN: ${error.message}`);
    return 2;
  }
  const released = join(state, RELEASED);
  // Prints `line` once the hold has ended, and spends its record first: `holder`, and a `held` a
  // killed hold left. Every end below that read the hold ended goes through it.
  const spent = (line: string, code: number): number => {
    rmSync(join(state, HOLDER), { force: true });
    dropHeld(state);
    console.log(line);
    return code;
  };
  if (!existsSync(released)) {
    if (!holderRuns(pid, start)) {
      // The hold may have ended, writing `released`, since the read above.
      if (!existsSync(released)) return spent("UNKNOWN: the held process is gone", 2);
    } else {
      try {
        process.kill(pid, "SIGTERM");
      } catch (error) {
        console.log(
          `UNKNOWN: the holder ${pid} of ${session} took no SIGTERM: ${messageOf(error)}`
        );
        return 2;
      }
      const until = Date.now() + RELEASE_MS;
      while (holderRuns(pid, start) && Date.now() < until) await Bun.sleep(100);
      if (holderRuns(pid, start)) {
        try {
          process.kill(pid, "SIGKILL");
        } catch {
          // It ended between the read and the kill.
        }
        return spent(
          `UNKNOWN: the holder ${pid} of ${session} still ran ${RELEASE_MS / 1000} s after its SIGTERM, so it was sent SIGKILL; ${session} may still be registered`,
          2
        );
      }
    }
  }
  if (!existsSync(released)) {
    return spent(`UNKNOWN: the holder ${pid} of ${session} ended and wrote no released`, 2);
  }
  const said = kept(state, RELEASED);
  if (said !== "unregistered") return spent(`UNKNOWN: ${session} was not released: ${said}`, 2);
  return spent(`RELEASED ${session}`, 0);
}

if (import.meta.main) {
  const [command = "run", argument] = Bun.argv.slice(2);
  const env = process.env;
  // Every registration names this as its machine (DEVICE_HOST's doc), never this machine's own.
  env.ENVOY_MACHINE_ID = DEVICE_HOST;
  let code: number;
  try {
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
      case "grants":
        code = await grants(env);
        break;
      case "t5":
        code = await t5(env);
        break;
      case "hold":
        code = await hold(env);
        break;
      case "release":
        code = await release(env);
        break;
      default:
        console.log(
          `UNKNOWN: usage: bun outside-edge.ts [run | approve <code> | revoke | grants | t5 | hold | release], not ${command}`
        );
        code = 2;
    }
  } catch (error) {
    // An error of the command's own, not an answer of the edge's (an OUTSIDE_EDGE_STATE it cannot
    // write, say): the command judged nothing of the edge, so it is UNKNOWN, exit 2, never 1,
    // which is a FAIL. Each command's own verdicts are its own; `hold` ends its errors itself,
    // unregistering a session it registered first.
    console.log(
      `UNKNOWN: ${command} failed on an error of its own: ${ownError(error, (env.OUTSIDE_EDGE_STATE ?? "").trim())}`
    );
    code = 2;
  }
  process.exit(code);
}
