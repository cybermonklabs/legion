// scripts/e2e/lib/app-logins.ts: the bot login of each Legion role App a legion.yaml configures,
// as GitHub names it today, and the proof that the credential configured for each App belongs to
// the App id configured with it. GraphQL's viewer, under an installation token of the App, answers
// the App's bot login, `<slug>[bot]`, and `GET /apps/<slug>` under the same token answers the
// App's id and declared permissions: the identity the daemon reads at boot for each role it
// commits, pushes and reviews as (packages/daemon/internal/appauth, tokenIdentity). A stage proof
// compares GitHub's records with these instead of with a name of its own, so an App renamed on
// GitHub changes what this prints and no script, and a name another App takes over later is never
// mistaken for Legion's. lib/github-apps.sh's check_github_apps runs it.
//
//   bun scripts/e2e/lib/app-logins.ts [--roles <role>[,<role>]] <legion.yaml> [<GitHub API URL>]
//     prints {"implement":"<slug>[bot]","review":"<slug>[bot]"} (the roles --roles names, both by
//     default) and exits 0, or names what it could not read or prove on stderr and exits 1; without
//     <legion.yaml>, or with a --roles it does not know, it exits 2 with its usage. The API URL
//     defaults to https://api.github.com, the one the daemon calls. An App whose id is not the
//     configured app_id is refused, naming both ids, and so is an App without the write permission
//     its role's work needs (roleWrites), which also refuses the two Apps swapped whole.
//
// An App's credential is one of the two command fields, and exactly one of them, as the daemon
// admits exactly one (internal/config, readGitHubApp):
//   `token_command`, which lib/github-apps.sh writes for the implement role, is run as the daemon
//     runs it (internal/appauth, runTokenCommand): `sh -c`, stdin closed, the environment less the
//     NATS seed variables (internal/runtime, WithoutNATSSeeds) with LEGION_GITHUB_OWNER,
//     LEGION_GITHUB_REPOS and LEGION_GITHUB_READ_ONLY set empty (the release of every
//     installation, narrowed to nothing, as AppLogin asks), and its stdout read as the secrets
//     broker's release, whose first installation token is the one proved.
//   `private_key_command`, which lib/github-apps.sh writes for the review role so that the daemon
//     mints and renews that App's tokens out of its key by itself, as a reviewer pod does, is run
//     as the daemon runs that one (internal/config, executePrivateKeyCommand): the same `sh -c`
//     with stdin closed and the NATS seed variables dropped, but none of those three variables
//     touched, since the daemon sets none of them for a key command. Its trimmed stdout is the
//     App's PEM, which signs an App JWT here, and that JWT mints the token that is proved.
// Either command's stderr is relayed and kept for a failure's message alone. The key path proves
// at least what the token path proves: the token it mints goes through the same viewer and
// `GET /apps/<slug>` observations, so the App's bot login, its id and its role's write permission
// are read from GitHub for a key-configured App exactly as they are for a token-configured one.
// A credential stays in this process's memory: no token, no JWT and no byte of a key is printed,
// written to a file, or handed to another process.

import { createSign } from "node:crypto";
import { writeSync } from "node:fs";

/** The two App roles `github_apps` names (internal/appauth, Implement and Review). */
export type AppRole = "implement" | "review";
export type AppLogins = Partial<Record<AppRole, string>>;
const appRoles: readonly AppRole[] = ["implement", "review"];
/** The App permission each role's work cannot run without: the implement App pushes the issue's
 * branch, and the review App posts the tester's and architect's check runs. The implement App
 * holds `checks: read` alone, so the review role configured with it, the two swapped whole, is
 * refused here rather than hours later at the first check run. `GET /apps/<slug>` answers the
 * App's declared permissions, not one installation's granted set, so this catches a wrong App and
 * not an installation that has not accepted a permission the App has since asked for. */
const roleWrites: Record<AppRole, string> = { implement: "contents", review: "checks" };

/** The credential fields an App of `github_apps` may configure, in the order a message names
 * them. */
const appCredentialFields = ["token_command", "private_key_command"] as const;

/** One App of `github_apps`: its id, which of the two credential fields configures it, and that
 * field's command. The field's name travels with the command, so every message about the command
 * names the field it was read from rather than the one this file happens to expect. */
export interface AppCredentialSource {
  readonly appId: string;
  readonly kind: (typeof appCredentialFields)[number];
  readonly command: string;
}

/** `value[key]` when `value` is an object that has `key`, else undefined: the one read of the
 * config's, the release's and GitHub's parsed shapes, which are outside data. */
function member(value: unknown, key: string): unknown {
  if (value === null || typeof value !== "object" || !(key in value)) return undefined;
  return Object.getOwnPropertyDescriptor(value, key)?.value;
}

/** The Apps of `roles` that `github_apps` configures in the legion.yaml text `text`, or a throw
 * naming the role whose id is missing, or whose credential fields are neither one nor a command. */
export function appCredentialSources(
  text: string,
  roles: readonly AppRole[] = appRoles
): Partial<Record<AppRole, AppCredentialSource>> {
  const apps = member(Bun.YAML.parse(text), "github_apps");
  if (apps === undefined || apps === null || typeof apps !== "object") {
    throw new Error("the config has no github_apps");
  }
  const source = (role: AppRole): AppCredentialSource => {
    const app = member(apps, role);
    const appId = member(app, "app_id");
    if (typeof appId !== "string" && typeof appId !== "number") {
      throw new Error(`github_apps.${role}.app_id is not set`);
    }
    const configured = appCredentialFields.filter((field) => {
      const command = member(app, field);
      return typeof command === "string" && command !== "";
    });
    // Exactly one, which is what the daemon admits of an App too (internal/config,
    // readGitHubApp): a role with neither field names no credential this can run, and a role
    // with both leaves the credential the daemon would use to the order of a rule here.
    if (configured.length === 0) {
      throw new Error(
        `github_apps.${role} names its credential without a token_command or a private_key_command, the two sources this reads`
      );
    }
    if (configured.length > 1) {
      throw new Error(
        `github_apps.${role} names both a token_command and a private_key_command: an App configures exactly one credential source`
      );
    }
    const field = configured[0];
    return { appId: String(appId), kind: field, command: member(app, field) as string };
  };
  return Object.fromEntries(roles.map((role) => [role, source(role)]));
}

/** The variables holding a NATS nkey seed's value, which no command the daemon starts sees
 * (internal/runtime, natsSeedVariables). */
const natsSeedVariables = ["NATS_NKEY_SEED", "NATS_DAEMON_NKEY_SEED"];

/** Runs one App's credential command — the token_command the daemon runs for every installation,
 * or the private_key_command it runs once to read a key — and returns its stdout. The command's
 * stderr is passed through to this process's stderr as it is written, so an operator sees what
 * `agent-secrets` prints while it waits for a person's grant (the request's id). A failure is
 * named as the daemon names it: its field and exit code, then the command's stderr when it wrote
 * any; its stdout is never quoted, since it is the credential. */
/** The most a credential command may print on either stream: a release of a few installations,
 * like a PEM, is a few kilobytes, as the daemon bounds a release (internal/appauth,
 * releaseLimit). */
const outputLimit = 1 << 20;
/** How long one credential command may run here: a deployment may hold a role's github-app
 * secret at human tier, whose release waits on a person and which `agent-secrets` waits 30
 * minutes for by default, and an operator's key command may read a store that waits on a person
 * too. */
const commandLimitMs = 35 * 60 * 1000;
/** How long the command's output may go on after the command itself has exited, before the run
 * gives up on a descendant that is holding the pipes open. What it bounds is the time since the
 * exit that this process does not spend blocked writing that output on: the relay writes
 * synchronously, so a caller that is behind holds one write for as long as it stays behind, and
 * one 300,008-byte line relayed into a pipe whose reader slept eight seconds blocked for those
 * eight seconds. Every time the bound is made of — the exit, the blocked time, and each re-arm
 * of its timer — is read from performance.now(), the clock no step of the wall clock moves. */
const drainLimitMs = 5000;
/** The GitHub token forms, which a command's stderr may carry and no log or error may. */
const tokenPattern = /(gh[opsur]_[A-Za-z0-9]{16,}|github_pat_[A-Za-z0-9_]{16,})/g;
/** A GitHub account login: what a release may name an installation by (internal/appauth,
 * loginPattern). */
const loginPattern = /^[A-Za-z0-9][A-Za-z0-9-]{0,38}$/;
/** How `agent-secrets` refuses a name this deployment's broker holds no secret under: the code
 * and the text of that one refusal. It is the one token-command failure whose fix is an input of
 * the run rather than anything on GitHub, so it is told apart from the rest; nothing here asks
 * the broker anything of its own, since a probe of a human-tier secret raises a person's
 * approval card. */
const unknownSecretPattern = /UNKNOWN_SECRET|no agent secret has this name/;

/** `text` with every GitHub token in it replaced. */
export function redactTokens(text: string): string {
  return text.replace(tokenPattern, "[redacted]");
}

/** Writes every byte of `text` to the descriptor `fd` before it returns, so the kernel holds
 * them and this process holds none. Every exit in this file is explicit, and an explicit exit
 * discards whatever of this process's output is still buffered, so nothing here goes through
 * `process.stdout` or `process.stderr`: a stream write's callback is not a flush while another
 * write is queued ahead of it. A 1,000,111-byte `process.stderr.write` issued behind a relayed
 * megabyte called its callback 1 to 10 ms later and reported `writableLength === 0` while the
 * caller had received 212,992, 425,984 or 638,976 bytes of it, whole multiples of the
 * 212,992-byte socket send buffer that `Bun.spawn`'s `stderr: "pipe"` gives the child, and the
 * `process.exit` that followed discarded the rest.
 *
 * `EAGAIN` is waited out rather than reported. A blocking descriptor never raises it: 1 MiB
 * against a reader that slept three seconds blocked for 2,985 ms and raised it zero times. But
 * the first read of `process.stderr` sets `O_NONBLOCK` on the open pipe, the flag belongs to the
 * pipe rather than to the process, and bun leaves it set when it exits, so every later writer on
 * that pipe inherits it
 * (docs/solutions/testing/bun-console-log-drops-what-a-full-non-blocking-pipe-cannot-take-and-exits-0.md),
 * and a stage proof runs many bun commands in one shell group. Sleeping a millisecond and
 * retrying the same offset delivered all 2,000,148 bytes of one such group's stderr, where
 * letting the `EAGAIN` out delivered 66,381 and a stack trace in place of the diagnosis.
 *
 * Every other failure ends the write, `EPIPE` among them, as does a write that takes none of a
 * non-empty buffer: there is nowhere to put the rest and no second channel to complain on, and
 * the exit code the caller reads is the part of the contract that still holds. So nothing here
 * throws, since a throw out of the relay prints its stack with nothing yet awaiting it and then
 * reaches the `catch` by way of the `Promise.all` that later takes it, which exits 1 where the
 * caller was promised the logins and exit 0. Nothing bounds the wait either, and nothing can,
 * since a deadline inside this loop would never fire against a write that blocks: a reader that
 * stalls holds this write for as long as it stalls, and the evidence the helper exists to print
 * reaches it whole. Which of this file's bounds such a reader can still reach is in the comment
 * above `await child.exited`. */
function writeAll(fd: number, text: string): void {
  const bytes = Buffer.from(text, "utf8");
  for (let written = 0; written < bytes.length; ) {
    let count: number;
    try {
      count = writeSync(fd, bytes, written);
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "EAGAIN") return;
      Bun.sleepSync(1);
      continue;
    }
    if (count === 0) return;
    written += count;
  }
}

async function runCredentialCommand(
  field: string,
  command: string,
  kind: AppCredentialSource["kind"]
): Promise<string> {
  const env: Record<string, string | undefined> = { ...process.env };
  for (const name of natsSeedVariables) delete env[name];
  // The daemon sets the three narrowing variables empty for a token command, which it runs for
  // every installation, and sets none of them for a key command, which reads a key no
  // installation narrows (internal/config, executePrivateKeyCommand). So a key command here sees
  // whatever this process's environment holds under those names, as the daemon's would.
  if (kind === "token_command") {
    env.LEGION_GITHUB_OWNER = "";
    env.LEGION_GITHUB_REPOS = "";
    env.LEGION_GITHUB_READ_ONLY = "";
  }
  let child: Bun.Subprocess<"ignore", "pipe", "pipe">;
  try {
    // detached: the command leads a process group of its own, which a kill ends whole.
    child = Bun.spawn(["sh", "-c", command], {
      env,
      stdin: "ignore",
      stdout: "pipe",
      stderr: "pipe",
      detached: true,
    });
  } catch (error) {
    throw new Error(
      `${field} could not run: ${error instanceof Error ? error.message : String(error)}`
    );
  }
  /** Signals that the run has stopped waiting on the command's output, because the drain bound
   * below passed or `end` killed the group. `end` resolves it too, so that the kill
   * commandLimitMs makes ends the wait as well: the group kill cannot reach a descendant that
   * left the group, and the streams it holds would otherwise outlive it. */
  const drainWait = Promise.withResolvers<false>();
  let ended = "";
  const end = (why: string) => {
    if (ended !== "") return;
    ended = why;
    try {
      process.kill(-child.pid, "SIGKILL");
    } catch {
      child.kill("SIGKILL");
    }
    drainWait.resolve(false);
  };
  const deadline = setTimeout(
    () => end(`did not finish within ${commandLimitMs / 60000} minutes`),
    commandLimitMs
  );
  const decoder = new TextDecoder();
  let stderr = "";
  let stderrBytes = 0;
  let pendingLine = "";
  /** How long this process has spent blocked in its own relay writes, which the drain bound
   * below does not count against the command. */
  let blockedMs = 0;
  const relayLine = (line: string) => {
    const clean = redactTokens(line);
    const before = performance.now();
    writeAll(2, clean);
    blockedMs += performance.now() - before;
    stderr += clean;
  };
  const relay = (async () => {
    for await (const chunk of child.stderr) {
      if (ended !== "") continue;
      stderrBytes += chunk.byteLength;
      if (stderrBytes > outputLimit) {
        pendingLine = "";
        end(`wrote more than ${outputLimit} bytes to stderr`);
        continue;
      }
      pendingLine += decoder.decode(chunk, { stream: true });
      let newline = pendingLine.indexOf("\n");
      while (newline >= 0) {
        relayLine(pendingLine.slice(0, newline + 1));
        pendingLine = pendingLine.slice(newline + 1);
        newline = pendingLine.indexOf("\n");
      }
    }
    if (pendingLine !== "" && ended === "") relayLine(pendingLine);
  })();
  const chunks: Uint8Array[] = [];
  let size = 0;
  const collect = (async () => {
    for await (const chunk of child.stdout) {
      size += chunk.length;
      if (size > outputLimit) {
        end(`printed more than ${outputLimit} bytes, which no credential is`);
        continue;
      }
      chunks.push(chunk);
    }
  })();
  // The command's own process may exit while a descendant that left its process group still
  // holds the pipes open, which the group kill cannot reach: the run holds the streams to
  // drainLimitMs, measured as that constant defines it, and fails rather than wait on them past
  // it. Only the blocks after the exit are given back, because the bound runs from the exit as
  // the continuation below reads it: a block that ended before that read is no part of the
  // bound, and one that is still running at the exit delays that read itself by the rest of it.
  // Giving back a block from before the exit as well would hand the descendant another stall to
  // hold the pipes in: a 300,002-byte line written before the exit, against a reader that slept
  // eight seconds, was failed at 21 seconds where the bound it is held to is 13. What the bound
  // does not count is the output itself, the escaped descendant's among it, so a descendant that
  // prints while it holds the pipes open is failed at the bound like a silent one. The one wait
  // this leaves unbounded is a caller that stays behind for good, which commandLimitMs cannot
  // end: its timer cannot run while a write holds the thread (writeAll's own comment), and a
  // 3-second limit against a reader that slept 15 seconds fired at 15, once the reader woke. It
  // ends a caller that is slow but still reading, between its writes; a caller that stops
  // reading holds the write, and the run, until the job's own timeout.
  const status = await child.exited;
  const exitedAt = performance.now();
  /** What blockedMs held at the exit, which the bound below subtracts so that only the blocks
   * after the exit count against it. This continuation runs between writes, never inside one,
   * so the snapshot is exact. */
  const blockedAtExit = blockedMs;
  let drainTimer: ReturnType<typeof setTimeout> | undefined;
  const armDrain = (ms: number) => {
    drainTimer = setTimeout(() => {
      // blockedMs grows while the timer waits, so a firing that is early re-arms for the rest.
      const left = exitedAt + drainLimitMs + (blockedMs - blockedAtExit) - performance.now();
      if (left <= 0) drainWait.resolve(false);
      else armDrain(left);
    }, ms);
  };
  armDrain(drainLimitMs);
  const drained = await Promise.race([
    Promise.all([relay, collect]).then(() => true),
    drainWait.promise,
  ]);
  clearTimeout(drainTimer);
  clearTimeout(deadline);
  if (!drained) end("exited while a process it started still held its output open");
  if (ended !== "") throw new Error(`${field} ${ended}`);
  if (status !== 0) {
    const failed = `${field} failed (exit ${status})`;
    throw new Error(stderr.trim() === "" ? failed : `${failed}: ${stderr.trim()}`);
  }
  return Buffer.concat(chunks).toString("utf8");
}

/** The token of the first installation, by account login, of the release `output`, as the daemon
 * reads one (internal/appauth, parseRelease and anyInstallation). Every refusal names the field
 * and the account and never quotes the output, which holds tokens. */
export function firstInstallationToken(output: string, field: string, now: Date): string {
  let release: unknown;
  try {
    release = JSON.parse(output.trim());
  } catch {
    release = undefined;
  }
  const installations = member(release, "installations");
  if (installations === null || typeof installations !== "object" || installations === undefined) {
    throw new Error(
      `${field} printed no release (want {"installations":{"<account>":{"installation_id":…,"token":…,"expires_at":…}}})`
    );
  }
  const owners = Object.keys(installations).sort((a, b) =>
    a.toLowerCase() < b.toLowerCase() ? -1 : a.toLowerCase() > b.toLowerCase() ? 1 : 0
  );
  if (owners.length === 0) throw new Error(`${field} released no installation`);
  for (const owner of owners) {
    if (!loginPattern.test(owner)) {
      throw new Error(`${field} released an installation whose account is not a GitHub login`);
    }
    const entry = member(installations, owner);
    const id = member(entry, "installation_id");
    if (typeof id !== "number" && typeof id !== "string") {
      throw new Error(`${field} released ${owner}'s installation with no installation id`);
    }
    const token = member(entry, "token");
    const expires = member(entry, "expires_at");
    const at = typeof expires === "string" ? Date.parse(expires) : Number.NaN;
    if (typeof token !== "string" || token === "" || Number.isNaN(at)) {
      throw new Error(
        `${field} released ${owner}'s installation without a token and an RFC 3339 expires_at`
      );
    }
    if (at <= now.getTime()) {
      throw new Error(
        `${field} released ${owner}'s installation with a token that expired at ${expires}`
      );
    }
  }
  return member(member(installations, owners[0]), "token") as string;
}

/** One GitHub read under `credential`, sent in the scheme GitHub asks for it — `token` for an
 * installation token, `Bearer` for an App JWT: the parsed JSON body, or a throw naming `read`,
 * GitHub's status and the head of its body, never the credential, whichever of the two it is.
 * An App JWT is a credential like a token, so GitHub's body is masked of it the same way. */
async function githubRead(
  read: string,
  url: string,
  credential: string,
  init: RequestInit = {},
  scheme: "token" | "Bearer" = "token"
) {
  let response: Response;
  let body: string;
  try {
    response = await fetch(url, {
      ...init,
      headers: {
        Authorization: `${scheme} ${credential}`,
        Accept: "application/vnd.github+json",
        "User-Agent": "legion-e2e",
        ...(init.body === undefined ? {} : { "Content-Type": "application/json" }),
      },
      signal: AbortSignal.timeout(30_000),
    });
    body = await response.text();
  } catch (error) {
    throw new Error(`${read} failed: ${error instanceof Error ? error.message : String(error)}`);
  }
  const head = redactTokens(body.split(credential).join("<credential>")).slice(0, 300);
  if (!response.ok) throw new Error(`${read} answered ${response.status}: ${head}`);
  try {
    return { answer: JSON.parse(body) as unknown, head };
  } catch {
    return { answer: undefined, head };
  }
}

/** The App JWT the PEM `key` signs for the App `appId`, the credential GitHub takes on the two
 * routes below: `{"alg":"RS256","typ":"JWT"}` over `{"iss":<app_id>,"iat":…,"exp":…}`, each part
 * base64url. `iat` is a minute behind `now` so a GitHub whose clock trails this one still takes
 * it, and `exp` is nine minutes past `iat`, inside the ten GitHub allows; `now` is the caller's
 * clock, so a proof drives both. A key `createSign` refuses names the App and the field alone:
 * the error it raises may quote what it tried to read, and no part of a key belongs in a
 * message. */
function appJwt(key: string, appId: string, now: Date, app: string, field: string): string {
  const part = (value: object) => Buffer.from(JSON.stringify(value), "utf8").toString("base64url");
  const issued = Math.floor(now.getTime() / 1000) - 60;
  const signed = `${part({ alg: "RS256", typ: "JWT" })}.${part({
    iss: appId,
    iat: issued,
    exp: issued + 9 * 60,
  })}`;
  try {
    return `${signed}.${createSign("RSA-SHA256").update(signed).sign(key, "base64url")}`;
  } catch {
    throw new Error(`${app}: ${field} printed no private key an App JWT can be signed with`);
  }
}

/** The installation token the key-configured App `source` mints for itself, as the daemon does
 * from the key its private_key_command prints: the command's trimmed stdout signs an App JWT,
 * `GET <api>/app/installations` under that JWT names an installation of the App, and
 * `POST <api>/app/installations/<id>/access_tokens` under it mints that installation's token.
 * GitHub answers the mint `{"token":…,"expires_at":…}`, which is GitHub's own body and no broker
 * release, so nothing here reads it as one. Minting a token at all, rather than reading the
 * App's slug from `GET <api>/app` under the JWT, is deliberate: the token is what carries the
 * viewer observation of appLogin, the one that refuses an App whose viewer is a person. `now` is
 * the clock rather than a reading of it, and it is read after the command returns: the command
 * runs to commandLimitMs, 35 minutes, where the JWT it signs lives nine (appJwt), so a key
 * command that waits that long on a person would otherwise present a JWT that expired while it
 * waited and the run would be refused with GitHub's 401 in place of its real cause. Every
 * refusal names the App by its role and id, and none quotes the key, the JWT or the token. */
async function mintedInstallationToken(
  source: AppCredentialSource,
  app: string,
  field: string,
  base: string,
  now: () => Date
): Promise<string> {
  let key: string;
  try {
    key = (await runCredentialCommand(field, source.command, source.kind)).trim();
  } catch (error) {
    throw new Error(`${app}: ${error instanceof Error ? error.message : String(error)}`);
  }
  // The key never leaves this scope: it signs below and is named in no message. Output that is
  // empty is refused here rather than carried into a signature, as the daemon refuses it
  // (internal/config, executePrivateKeyCommand).
  if (key === "") throw new Error(`${app}: ${field} produced empty output`);
  const jwt = appJwt(key, source.appId, now(), app, field);
  const read = `GET /app/installations as ${app}`;
  const listed = await githubRead(read, `${base}/app/installations`, jwt, {}, "Bearer");
  if (!Array.isArray(listed.answer)) {
    throw new Error(`${read} answered no list of installations: ${listed.head}`);
  }
  if (listed.answer.length === 0) {
    throw new Error(`${read} answered no installation: an App installed nowhere mints no token`);
  }
  const installation = member(listed.answer[0], "id");
  if (typeof installation !== "number" && typeof installation !== "string") {
    throw new Error(`${read} answered an installation with no id: ${listed.head}`);
  }
  const mint = `POST /app/installations/${installation}/access_tokens as ${app}`;
  const minted = await githubRead(
    mint,
    `${base}/app/installations/${encodeURIComponent(String(installation))}/access_tokens`,
    jwt,
    { method: "POST" },
    "Bearer"
  );
  const token = member(minted.answer, "token");
  if (typeof token !== "string" || token === "") {
    throw new Error(`${mint} answered no installation token: ${minted.head}`);
  }
  return token;
}

/** The bot login GitHub gives the App `source` names: GraphQL's viewer under an installation
 * token of the App — the first of the release its token_command prints, or the one its
 * private_key_command's key mints (mintedInstallationToken) — `<slug>[bot]`, whose
 * `GET <api>/apps/<slug>` must answer `source.appId` and its role's write permission
 * (roleWrites). Every failure names the App by its role and id: a command that fails or prints
 * no credential, a GitHub that never answered, an answer that is not 2xx, a viewer that is no
 * App's bot, one whose App id is not `source.appId` (naming both ids), and one without its
 * role's write permission. A token command the secrets broker refused outright, holding no
 * secret under the name it gave, also names the input that names that secret, since the fix is
 * there and not on GitHub. */
export async function appLogin(
  role: AppRole,
  source: AppCredentialSource,
  api: string,
  now: () => Date = () => new Date()
): Promise<string> {
  const app = `the ${role} App (app_id ${source.appId})`;
  const field = `github_apps.${role}.${source.kind}`;
  const base = api.replace(/\/+$/, "");
  /** How a message names where the token this proves came from, which is the field's doing
   * either way: a token_command releases one, a private_key_command's key mints one. */
  const gave = source.kind === "token_command" ? `${field} released` : `${field}'s key minted`;
  let token: string;
  if (source.kind === "private_key_command") {
    token = await mintedInstallationToken(source, app, field, base, now);
  } else {
    try {
      token = firstInstallationToken(
        await runCredentialCommand(field, source.command, source.kind),
        field,
        now()
      );
    } catch (error) {
      const reason = error instanceof Error ? error.message : String(error);
      // The command's whole stderr is in `reason`, wherever in it the broker's refusal landed,
      // and the role is in hand here, so the one failure an input fixes can name that input.
      // `role` is one of two literals, so the name formed is `LEGION_E2E_IMPLEMENT_APP_SECRET`
      // or `LEGION_E2E_REVIEW_APP_SECRET`. Only the first of the two can be reached now: the
      // branch is held to a token_command, and lib/github-apps.sh gives the review App a key
      // command, which asks the broker nothing this names an input for.
      if (unknownSecretPattern.test(reason)) {
        throw new Error(
          `${app}: the secrets broker serves no github-app secret under the name ${field} gives (UNKNOWN_SECRET): set LEGION_E2E_${role.toUpperCase()}_APP_SECRET to the name of a github-app secret this deployment's broker holds for that App`
        );
      }
      throw new Error(`${app}: ${reason}`);
    }
  }
  const viewer = await githubRead(`GraphQL viewer as ${app}`, `${base}/graphql`, token, {
    method: "POST",
    body: JSON.stringify({ query: "{viewer{login}}" }),
  });
  const login = member(member(member(viewer.answer, "data"), "viewer"), "login");
  if (typeof login !== "string" || login === "") {
    throw new Error(`GraphQL viewer as ${app} answered no login: ${viewer.head}`);
  }
  const slug = login.endsWith("[bot]") ? login.slice(0, -"[bot]".length) : "";
  if (slug === "") {
    throw new Error(`${app}: ${gave} a token that acts as ${login}, not an App's bot`);
  }
  const read = `GET /apps/${slug} as ${app}`;
  const { answer, head } = await githubRead(
    read,
    `${base}/apps/${encodeURIComponent(slug)}`,
    token
  );
  const id = member(answer, "id");
  if (typeof id !== "number" && typeof id !== "string") {
    throw new Error(`${read} answered no App id: ${head}`);
  }
  if (String(id) !== source.appId) {
    throw new Error(
      `${read} answered App id ${id}, want ${source.appId}: ${gave} another App's token`
    );
  }
  const permission = roleWrites[role];
  const granted = member(member(answer, "permissions"), permission);
  if (granted !== "write" && granted !== "admin") {
    throw new Error(
      `${read} answered permissions.${permission} ${JSON.stringify(granted ?? null)}, want write: the ${role} role's work needs it, so the App is not the ${role} App`
    );
  }
  return login;
}

/** The bot logins of the Apps of `roles`, refusing a config whose two roles resolve to one App,
 * which the daemon refuses at boot too (internal/daemon, mintAtBoot). */
export async function appLogins(
  configPath: string,
  api: string,
  roles: readonly AppRole[] = appRoles
): Promise<AppLogins> {
  const sources = appCredentialSources(await Bun.file(configPath).text(), roles);
  const logins: AppLogins = {};
  for (const role of roles) {
    const source = sources[role];
    if (source !== undefined) logins[role] = await appLogin(role, source, api);
  }
  if (logins.implement !== undefined && logins.implement === logins.review) {
    throw new Error(`both github_apps roles are the App ${logins.review}: the workflow needs two`);
  }
  return logins;
}

/** The roles a `--roles` value names, comma-separated, each once; undefined for any other value. */
function parseRoles(value: string): AppRole[] | undefined {
  const roles = value.split(",");
  const known = (role: string): role is AppRole => (appRoles as readonly string[]).includes(role);
  if (!roles.every(known) || new Set(roles).size !== roles.length) return undefined;
  return roles.filter(known);
}

if (import.meta.main) {
  const args = process.argv.slice(2);
  let roles: AppRole[] | undefined = [...appRoles];
  if (args[0] === "--roles") {
    roles = parseRoles(args[1] ?? "");
    args.splice(0, 2);
  }
  const [configPath, api = "https://api.github.com"] = args;
  if (roles === undefined || configPath === undefined || configPath === "") {
    writeAll(
      2,
      "usage: bun scripts/e2e/lib/app-logins.ts [--roles <role>[,<role>]] <legion.yaml> [<GitHub API URL>]\n"
    );
    process.exit(2);
  }
  // Exit explicitly: a stream reader a token command's escaped descendant still feeds would keep
  // the event loop, and so the run, alive past the drain bound.
  try {
    writeAll(1, `${JSON.stringify(await appLogins(configPath, api, roles))}\n`);
    process.exit(0);
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error);
    writeAll(2, `app-logins: ${message}\n`);
    process.exit(1);
  }
}
