// scripts/e2e/lib/app-logins.ts: the bot login of each Legion role App a legion.yaml configures,
// as GitHub names it today, and the proof that each App's token command releases tokens of the App
// id configured for it. GraphQL's viewer, under an installation token of the App, answers the
// App's bot login, `<slug>[bot]`, and `GET /apps/<slug>` under the same token answers the App's id
// and declared permissions: the identity the daemon reads at boot for each role it commits,
// pushes and reviews as (packages/daemon/internal/appauth, tokenIdentity). A stage proof compares
// GitHub's records with these instead of with a name of its own, so an App renamed on GitHub
// changes what this prints and no script, and a name another App takes over later is never
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
// Each App's tokens come from its `github_apps.<role>.token_command`, run as the daemon runs it
// (internal/appauth, runTokenCommand): `sh -c`, stdin closed, the environment less the NATS seed
// variables (internal/runtime, WithoutNATSSeeds) with LEGION_GITHUB_OWNER, LEGION_GITHUB_REPOS and
// LEGION_GITHUB_READ_ONLY set empty (the release of every installation, narrowed to nothing, as
// AppLogin asks), stdout read as the secrets broker's release, and stderr relayed and kept for a
// failure's message alone.
// A token stays in this process's memory: it is never printed, written to a file, or handed to
// another process. An App configured with any other credential source is refused, since every
// stage proof's file names a token command.

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

/** One App of `github_apps`: its id and the command that prints its release of tokens. */
export interface AppTokenSource {
  readonly appId: string;
  readonly tokenCommand: string;
}

/** `value[key]` when `value` is an object that has `key`, else undefined: the one read of the
 * config's, the release's and GitHub's parsed shapes, which are outside data. */
function member(value: unknown, key: string): unknown {
  if (value === null || typeof value !== "object" || !(key in value)) return undefined;
  return Object.getOwnPropertyDescriptor(value, key)?.value;
}

/** The Apps of `roles` that `github_apps` configures in the legion.yaml text `text`, or a throw
 * naming the key that is missing or not a command. */
export function appTokenSources(
  text: string,
  roles: readonly AppRole[] = appRoles
): Partial<Record<AppRole, AppTokenSource>> {
  const apps = member(Bun.YAML.parse(text), "github_apps");
  if (apps === undefined || apps === null || typeof apps !== "object") {
    throw new Error("the config has no github_apps");
  }
  const source = (role: AppRole): AppTokenSource => {
    const app = member(apps, role);
    const appId = member(app, "app_id");
    const command = member(app, "token_command");
    if (typeof appId !== "string" && typeof appId !== "number") {
      throw new Error(`github_apps.${role}.app_id is not set`);
    }
    if (typeof command !== "string" || command === "") {
      throw new Error(
        `github_apps.${role} names its credential without a token_command, the one source this reads`
      );
    }
    return { appId: String(appId), tokenCommand: command };
  };
  return Object.fromEntries(roles.map((role) => [role, source(role)]));
}

/** The variables holding a NATS nkey seed's value, which no command the daemon starts sees
 * (internal/runtime, natsSeedVariables). */
const natsSeedVariables = ["NATS_NKEY_SEED", "NATS_DAEMON_NKEY_SEED"];

/** Runs one App's token_command as the daemon runs it for every installation and returns its
 * stdout. The command's stderr is passed through to this process's stderr as it is written, so an
 * operator sees what `agent-secrets` prints while it waits for a person's grant (the request's
 * id). A failure is named as the daemon names it: its field and exit code, then the command's
 * stderr when it wrote any; its stdout is never quoted. */
/** The most a token command may print on either stream: a release of a few installations is a
 * few kilobytes, as the daemon bounds it (internal/appauth, releaseLimit). */
const outputLimit = 1 << 20;
/** How long one token command may run here: a review App's first grant waits on a person, and
 * `agent-secrets` waits 30 minutes for a decision by default. */
const commandLimitMs = 35 * 60 * 1000;
/** How long the command's output may go without progress once the command itself has exited,
 * before the run gives up on a descendant that is holding the pipes open. It bounds idle time
 * rather than time since the exit, so a relay write that blocks against a reader that is behind
 * re-arms it instead of spending it. */
const drainIdleLimitMs = 5000;
/** The GitHub token forms, which a command's stderr may carry and no log or error may. */
const tokenPattern = /(gh[opsur]_[A-Za-z0-9]{16,}|github_pat_[A-Za-z0-9_]{16,})/g;
/** A GitHub account login: what a release may name an installation by (internal/appauth,
 * loginPattern). */
const loginPattern = /^[A-Za-z0-9][A-Za-z0-9-]{0,38}$/;

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
 * throws, since a throw out of the relay rejects its promise before anything handles it, which
 * kills this process with a stack and leaves a token command's process group alive. Nothing
 * bounds the wait either, and nothing can, since a deadline inside this loop would never fire
 * against a write that blocks: a reader that stalls forever used to make the helper exit after
 * five seconds with its own diagnosis truncated, and now makes it wait under the job's own
 * timeout, with the evidence the helper exists to print kept whole. */
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

async function runTokenCommand(field: string, command: string): Promise<string> {
  const env: Record<string, string | undefined> = { ...process.env };
  for (const name of natsSeedVariables) delete env[name];
  env.LEGION_GITHUB_OWNER = "";
  env.LEGION_GITHUB_REPOS = "";
  env.LEGION_GITHUB_READ_ONLY = "";
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
  let ended = "";
  const end = (why: string) => {
    if (ended !== "") return;
    ended = why;
    try {
      process.kill(-child.pid, "SIGKILL");
    } catch {
      child.kill("SIGKILL");
    }
  };
  const deadline = setTimeout(
    () => end(`did not finish within ${commandLimitMs / 60000} minutes`),
    commandLimitMs
  );
  const decoder = new TextDecoder();
  let stderr = "";
  let stderrBytes = 0;
  let pendingLine = "";
  /** When either stream last delivered or took bytes, which the drain bound counts from. */
  let lastProgress = Date.now();
  const relayLine = (line: string) => {
    const clean = redactTokens(line);
    writeAll(2, clean);
    // The write is synchronous and may have blocked for as long as the caller was behind, which
    // is progress: the bound below counts from here, not from before it.
    lastProgress = Date.now();
    stderr += clean;
  };
  const relay = (async () => {
    for await (const chunk of child.stderr) {
      lastProgress = Date.now();
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
      lastProgress = Date.now();
      size += chunk.length;
      if (size > outputLimit) {
        end(`printed more than ${outputLimit} bytes, which no release is`);
        continue;
      }
      chunks.push(chunk);
    }
  })();
  // The command's own process may exit while a descendant that left its process group still holds
  // the pipes open, which the group kill cannot reach: once the streams have gone
  // drainIdleLimitMs without delivering anything the run fails rather than wait on them. The
  // bound counts idle time rather than wall-clock time since the exit because the relay writes
  // this process's stderr synchronously, so a caller that is behind blocks the relay for as long
  // as it stays behind: one 300,007-byte line relayed into a pipe whose reader slept eight
  // seconds blocked for those eight seconds, and a bound measured from the exit spent itself on
  // that block and replaced the command's own exit 3 and quoted stderr with `exited while a
  // process it started still held its output open`. A write that completes is progress and
  // re-arms the bound, so a slow reader can no longer reach it; it does still count against
  // commandLimitMs, the 35 minutes the whole command gets, which no reader a stage proof runs
  // this under can plausibly stretch a write past.
  const status = await child.exited;
  lastProgress = Date.now();
  const drainWait = Promise.withResolvers<boolean>();
  let drainTimer: ReturnType<typeof setTimeout> | undefined;
  const armDrain = (ms: number) => {
    drainTimer = setTimeout(() => {
      const idle = Date.now() - lastProgress;
      if (idle >= drainIdleLimitMs) drainWait.resolve(false);
      else armDrain(drainIdleLimitMs - idle);
    }, ms);
  };
  armDrain(drainIdleLimitMs);
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

/** One GitHub read under `token`: the parsed JSON body, or a throw naming `read`, GitHub's status
 * and the head of its body, never the token. */
async function githubRead(read: string, url: string, token: string, init: RequestInit = {}) {
  let response: Response;
  let body: string;
  try {
    response = await fetch(url, {
      ...init,
      headers: {
        Authorization: `token ${token}`,
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
  const head = redactTokens(body.split(token).join("<token>")).slice(0, 300);
  if (!response.ok) throw new Error(`${read} answered ${response.status}: ${head}`);
  try {
    return { answer: JSON.parse(body) as unknown, head };
  } catch {
    return { answer: undefined, head };
  }
}

/** The bot login GitHub gives the App `source` names: GraphQL's viewer under the first
 * installation token of the App's release, `<slug>[bot]`, whose `GET <api>/apps/<slug>` must
 * answer `source.appId` and its role's write permission (roleWrites). Every failure names the App
 * by its role and id: a command that fails or prints no release, a GitHub that never answered, an
 * answer that is not 2xx, a viewer that is no App's bot, one whose App id is not `source.appId`
 * (naming both ids), and one without its role's write permission. */
export async function appLogin(
  role: AppRole,
  source: AppTokenSource,
  api: string,
  now: () => Date = () => new Date()
): Promise<string> {
  const app = `the ${role} App (app_id ${source.appId})`;
  const field = `github_apps.${role}.token_command`;
  const base = api.replace(/\/+$/, "");
  let token: string;
  try {
    token = firstInstallationToken(await runTokenCommand(field, source.tokenCommand), field, now());
  } catch (error) {
    throw new Error(`${app}: ${error instanceof Error ? error.message : String(error)}`);
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
    throw new Error(`${app}: ${field} released a token that acts as ${login}, not an App's bot`);
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
      `${read} answered App id ${id}, want ${source.appId}: the token command releases another App's tokens`
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
  const sources = appTokenSources(await Bun.file(configPath).text(), roles);
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
