/**
 * The rig's reading of the Legion daemon: `daemon-pane.go`, run with `go run -overlay` as a main
 * inside a checkout's daemon module, which is how it reaches the daemon's internal packages. The
 * rig asks it rather than restating the daemon: a change to the daemon reaches the rig by
 * construction. The one exception is `daemon-pane.go`'s own table of the handoff words, which the
 * program carries because one program runs against both checkouts of a comparison and the daemon's
 * table has not always had the same name; its comment says so. `workerPane` builds a phase
 * worker's whole pane environment over it.
 */
import { createHash, randomUUID } from "node:crypto";
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  renameSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import os from "node:os";
import path from "node:path";
import type { LegionRole } from "@legion/contracts";
import { DEFAULT_ENVOY_URL } from "@legion/envoy-client/envoy-settings";
import { sessionConfigFile, sessionEnvoySettings } from "@legion/envoy-client/session-settings";

/** This checkout's daemon module, the one the rig reads unless a run names another checkout's. */
export const DAEMON_MODULE = path.resolve(import.meta.dir, "../../../daemon");

const PROGRAM = path.join(import.meta.dir, "daemon-pane.go");

/** Runs daemon-pane.go in `module` with `args`, `input` on its stdin (the caller's own stdin when
 * there is none), and returns its stdout. */
export function runDaemonPane(module: string, args: readonly string[], input?: string): string {
  const scratch = mkdtempSync(path.join(os.tmpdir(), "daemon-pane-"));
  try {
    const overlay = path.join(scratch, "overlay.json");
    const main = path.join(module, "cmd", "daemon-pane", "main.go");
    writeFileSync(overlay, JSON.stringify({ Replace: { [main]: PROGRAM } }));
    const result = Bun.spawnSync(["go", "run", "-overlay", overlay, "./cmd/daemon-pane", ...args], {
      cwd: module,
      stdin: input === undefined ? "inherit" : new TextEncoder().encode(input),
      stdout: "pipe",
      stderr: "pipe",
    });
    if (result.exitCode !== 0) {
      throw new Error(
        `daemon-pane ${args.join(" ")} in ${module} exited ${result.exitCode}: ${result.stderr.toString().trim()}`
      );
    }
    return result.stdout.toString();
  } finally {
    rmSync(scratch, { recursive: true, force: true });
  }
}

/** The daemon's refusal of a completion of a phase the completing role does not run: its code
 * and the daemon's own message for it. */
export interface Refusal {
  readonly code: string;
  readonly message: string;
}

/** The refusal daemon-pane.go's `refusal not-current-phase` printed as `printed`, or undefined
 * when it printed anything but what that command prints: the code, then the message, each a
 * non-empty line of its own. Fewer lines, an empty one, or more than two are a refusal half
 * read, which is a sentence the daemon never sends, so the caller reads such a printout as none
 * rather than answering part of it. */
export function refusalIn(printed: string): Refusal | undefined {
  const [code = "", message = "", ...rest] = printed.split("\n");
  if (code === "" || message === "" || rest.length !== 1 || rest[0] !== "") return undefined;
  return { code, message };
}

/** The daemon module of `checkout`. */
function daemonOf(checkout: string): string {
  return path.join(checkout, "packages", "daemon");
}

/** The Go sources of the package at `dir` but its tests, in byte order, each a full path; none for
 * a directory that is not there, whose reading then refuses on its own terms. */
function goSources(dir: string): string[] {
  if (!existsSync(dir)) return [];
  return readdirSync(dir)
    .filter((name) => name.endsWith(".go") && !name.endsWith("_test.go"))
    .sort()
    .map((name) => path.join(dir, name));
}

/** Where daemon-pane.go's answers are kept between processes (`cachedRead`): under the rig's work
 * directory (`SKILL_SCENARIOS_WORK`, which rig.sh exports), else under `TMPDIR`; never under HOME
 * or a `/tmp` nobody named. With neither set nothing is kept, and every read runs the program. */
function factsCache(): string | undefined {
  const work = process.env.SKILL_SCENARIOS_WORK;
  if (work !== undefined && work !== "") return path.join(work, "daemon-facts");
  const tmp = process.env.TMPDIR;
  if (tmp !== undefined && tmp !== "") return path.join(tmp, "skill-scenarios-daemon-facts");
  return undefined;
}

/** daemon-pane.go's answer to `args`, run in `module`, kept in the facts cache under a key of
 * exactly what decides it: the program's own source, `keyArgs` (the command with no path in it),
 * and every source file the read parses, each by its path under `root` and its bytes. A boot
 * whose key is kept reads the answer from the file, with no `go run`; any other runs the program
 * and writes the answer whole (a temporary file renamed into place), so a reader never sees half
 * of one. A read that fails writes nothing, and throws as the program refused. The key names no
 * absolute path, so a second checkout of the same sources is the same answer. */
function cachedRead(
  module: string,
  args: readonly string[],
  keyArgs: readonly string[],
  root: string,
  sources: readonly string[]
): string {
  const dir = factsCache();
  if (dir === undefined) return runDaemonPane(module, args);
  const hash = createHash("sha256").update(readFileSync(PROGRAM)).update(JSON.stringify(keyArgs));
  for (const source of sources)
    hash.update(`\0${path.relative(root, source)}\0`).update(readFileSync(source));
  const file = path.join(dir, `${hash.digest("hex")}.out`);
  if (existsSync(file)) return readFileSync(file, "utf8");
  const answer = runDaemonPane(module, args);
  mkdirSync(dir, { recursive: true });
  const written = `${file}.${process.pid}.${randomUUID()}.tmp`;
  writeFileSync(written, answer);
  renameSync(written, file);
  return answer;
}

/** The daemon's own refusal of `role`'s completion while the issue `issue` stands in `phase`, as
 * the daemon of `checkout` writes it: read out of that checkout's engine and intake sources by
 * daemon-pane.go (`refusalIn`), or undefined when that printout is no refusal. The daemon
 * stand-in answers such a completion with it and its tests read it the same way, so no copy of
 * the sentence and no second reading of it lives here. */
export function notCurrentPhase(
  checkout: string,
  role: string,
  phase: string,
  issue: string
): Refusal | undefined {
  const daemon = daemonOf(checkout);
  const engine = path.join(daemon, "internal", "workflow", "engine.go");
  const intake = path.join(daemon, "internal", "intake");
  const command = ["refusal", "not-current-phase", role, phase, issue];
  return refusalIn(
    cachedRead(DAEMON_MODULE, [...command, engine, intake], command, daemon, [
      engine,
      ...goSources(intake),
    ])
  );
}

/** The phase `role`'s worker is started in by the daemon of `checkout`, the first of its
 * transition table the role works, and the word of the handoff record that phase ends with, ""
 * when it ends with none: daemon-pane.go's `phase`, which runs the daemon's own functions and so
 * runs inside that checkout's module. Its answer is the phase and workflow tables and the claim
 * roles they name, so those three packages are what keeps it (`cachedRead`). */
export function labelPhase(checkout: string, role: string): { phase: string; word: string } {
  const daemon = daemonOf(checkout);
  const internal = path.join(daemon, "internal");
  const [phase = "", word = ""] = cachedRead(daemon, ["phase", role], ["phase", role], daemon, [
    ...goSources(path.join(internal, "phase")),
    ...goSources(path.join(internal, "workflow")),
    ...goSources(path.join(internal, "claim")),
  ]).split("\n");
  return { phase, word };
}

/** The checkout the run under `runDir` was built from: the `checkout` line of its built-from,
 * which rig.sh's cmd_profile writes for the label and cmd_run copies into the run before any
 * agent boots. Its daemon is the one the label's runs stand in for (`labelRoutes`). Throws for a
 * run with no such line: what that run's daemon serves is unknown. */
export function labelCheckout(runDir: string): string {
  const file = path.join(runDir, "built-from");
  const line = existsSync(file)
    ? readFileSync(file, "utf8")
        .split("\n")
        .find((one) => one.startsWith("checkout "))
    : undefined;
  const checkout = line?.slice("checkout ".length).trim() ?? "";
  if (checkout === "")
    throw new Error(
      `${file} names no checkout (rig.sh's cmd_profile writes a \`checkout <dir>\` line), so the daemon that run stands in for is unknown`
    );
  return checkout;
}

/** Every route the daemon of `checkout` registers, `<METHOD> <path>` each, read out of that
 * checkout's API source by daemon-pane.go's `routes`, which this checkout's daemon module runs:
 * the reading parses the other checkout's files and compiles nothing of them, so a substitute
 * checkout holding the source alone is read the same way. Throws what the reading refused. */
export function labelRoutes(checkout: string): readonly string[] {
  const daemon = daemonOf(checkout);
  const api = path.join(daemon, "internal", "api");
  return cachedRead(DAEMON_MODULE, ["routes", api], ["routes"], daemon, goSources(api))
    .split("\n")
    .filter((line) => line !== "");
}

/** Whether `routes` (`labelRoutes`) serves a request of `method` to `asked`; `method` undefined
 * is a request whose method is not recorded, which any route of that path serves. A segment the
 * daemon names `{name}` matches any one non-empty segment, and `{name...}` the rest of the path,
 * as Go's ServeMux matches them. */
export function serves(
  routes: readonly string[],
  method: string | undefined,
  asked: string
): boolean {
  const parts = asked.split("/");
  return routes.some((pattern) => {
    const space = pattern.indexOf(" ");
    if (method !== undefined && pattern.slice(0, space) !== method) return false;
    const segments = pattern.slice(space + 1).split("/");
    for (const [at, segment] of segments.entries()) {
      if (/^\{[^}]*\.\.\.\}$/.test(segment)) return parts.length > at;
      const part = parts[at];
      if (part === undefined) return false;
      if (/^\{[^}]*\}$/.test(segment) ? part === "" : part !== segment) return false;
    }
    return parts.length === segments.length;
  });
}

/** The default the daemon of `checkout` gives the count setting `key` when legion.yaml leaves it
 * out, which is the value the rig's daemon runs with, having no legion.yaml: daemon-pane.go's
 * `count-default`, over that checkout's config source. Throws what the reading refused. */
export function countDefault(checkout: string, key: string): number {
  const daemon = daemonOf(checkout);
  const config = path.join(daemon, "internal", "config", "config.go");
  const command = ["count-default", key];
  const printed = cachedRead(DAEMON_MODULE, [...command, config], command, daemon, [config]).trim();
  const value = Number(printed);
  if (!Number.isSafeInteger(value))
    throw new Error(`daemon-pane count-default ${key} printed ${JSON.stringify(printed)}`);
  return value;
}

/** The claim refusal `name` (claim.InvalidSecret, claim.StaleGeneration, …) of the daemon of
 * `checkout`, its status and its message: daemon-pane.go's `claim-refusal`, over that checkout's
 * claim source, which the stand-in answers a caller it refuses with. Throws what the reading
 * refused. */
export function claimRefusal(checkout: string, name: string): { status: number; message: string } {
  const daemon = daemonOf(checkout);
  const claim = path.join(daemon, "internal", "claim");
  const command = ["claim-refusal", name];
  const [status = "", message = ""] = cachedRead(
    DAEMON_MODULE,
    [...command, claim],
    command,
    daemon,
    goSources(claim)
  ).split("\n");
  if (!/^\d+$/.test(status) || message === "")
    throw new Error(`daemon-pane claim-refusal ${name} printed no status and message`);
  return { status: Number(status), message };
}

/** The claim a pane is launched for and the rig's stand-ins for what the daemon knows at boot
 * (daemon-pane.go's paneRequest). `repository` and `defaultBranch` are the run's own world's,
 * which the pane's addressing names and whose owner the claim's gh bundle is the directory of.
 * `ghToken` stands in for the claim's GitHub App token: it is
 * rendered into the claim's gh bundle under `<stateDir>/secrets/<claim>-gh`, the pane's
 * `LEGION_GH_OWNERS_DIR`, whose one owner's directory is its `GH_CONFIG_DIR`, and its
 * `LEGION_GH_PATH` is `ghPath` when the rig names one (its recording stand-in), else the real gh as
 * the daemon resolves it at boot, the first `gh` on `path`, refused when that is a script; omitted,
 * the pane gets no gh variable, as from a daemon with no GitHub Apps. */
interface PaneRequest {
  readonly project: string;
  readonly issue: string;
  readonly role: string;
  readonly repository: string;
  readonly defaultBranch: string;
  readonly stateDir: string;
  readonly workspace: string;
  readonly daemonUrl: string;
  readonly envoyUrl: string;
  readonly natsUrls: readonly string[];
  readonly bootTokenFile: string;
  readonly ghToken?: string;
  readonly path: string;
  readonly ghPath?: string;
  readonly systemPrompt?: boolean;
}

/** What the daemon tells the pane: every variable, PATH included, and, for a request that asks for
 * it, the one `--append-system-prompt` argument as shell text. */
export interface DaemonPane {
  readonly env: Record<string, string>;
  readonly systemPromptArgument?: string;
}

/** The stand-in for the claim's GitHub App token: `daemon-pane.go` renders it into the claim's gh
 * files, and the pane's own `gh auth token` prints it. A placeholder, never a secret. */
export const RIG_GH_TOKEN = "ghs_rig_token";

/** Where a worker pane runs: the run directory that stands for the daemon's state directory and
 * the issue's workspace, the stand-in daemon's port, and the Oh My Pi profile. */
export interface WorkerLaunch {
  readonly rig: string;
  readonly port: number;
  readonly profile: string;
}

/** The claim a worker pane is launched for: its project, issue and role, and the repository and
 * default branch of the run's own world, which the pane's addressing names and whose owner the
 * claim's gh bundle is the directory of. The caller reads both out of the run's world
 * (worker-pane.ts); why they are the world's and no name of this rig's own: rig.sh's worker
 * world comment, which holds that story once. */
export interface WorkerClaim {
  readonly project: string;
  readonly issue: string;
  readonly role: LegionRole;
  readonly repository: string;
  readonly defaultBranch: string;
}

/** Where a pane's daemon is read from: a checkout's daemon module (`DAEMON_MODULE` for this one),
 * whose embedded role prompts compose the pane's system prompt when `systemPrompt` is set. */
export interface PaneSource {
  readonly daemonModule: string;
  readonly systemPrompt?: boolean;
  /** The real gh the pane is told, as an operator names one with `LEGION_GH_PATH`: the rig's
   * recording stand-in. Omitted, the daemon's own resolution on the inherited PATH. */
  readonly ghPath?: string;
}

/** The pane the Legion daemon's tmux runtime gives a phase worker for `claim`, pointed at the
 * scratch state directory and the stand-in daemon, over the caller's own environment. Everything
 * the daemon tells the pane comes from the daemon's own functions in `source`'s module
 * (`daemon-pane.go`): the claim's identity, the daemon URL, the state directory and workspace,
 * Envoy (the listener and NATS the pane's own extension reaches from the caller's environment and
 * the profile's envoy.json, `envoySettings`, which reads no Envoy from the workspace's
 * `.envoy/envoy.json`; the default listener and no NATS when that Envoy is off), the claim's gh
 * bundle, which holds `RIG_GH_TOKEN` (`LEGION_GH_OWNERS_DIR`, `GH_CONFIG_DIR` and `LEGION_GH_PATH`,
 * `source.ghPath`, or the first `gh` on the inherited PATH, which must be no script), with
 * `GH_TOKEN`, `GITHUB_TOKEN` and `GH_HOST` set empty,
 * `PI_SHELL_PREFIX`, the four XDG base directories, the boot token pointer
 * (`<state>/secrets/boot`, the file the stand-in checks), and PATH with the launcher directory
 * first; no grant file, since the `legion` tool mints its grants in-process and no agent runs
 * `legion` from bash; and, with `systemPrompt`, the system prompt argument, whose addressing
 * names `claim.repository` and `claim.defaultBranch`, the world's own. An inherited
 * `ANTHROPIC_API_KEY`, every `LEGION_*` and `DISPATCH_*` value and the caller's own GitHub
 * variables are dropped first. The profile's agent directory, and so its envoy.json, is under the
 * inherited `HOME`. Throws `envoy: <why>` when that Envoy is off with an error, or on with inbound
 * messages off (`inbound.off`: no NATS server, or its seed refused), since the pane's Legion claim
 * refuses then. */
export function workerPane(
  launch: WorkerLaunch,
  inherited: NodeJS.ProcessEnv,
  claim: WorkerClaim,
  source: PaneSource
): DaemonPane {
  const env: Record<string, string> = {};
  for (const [key, value] of Object.entries(inherited)) {
    if (value === undefined) continue;
    if (key === "ANTHROPIC_API_KEY" || key.startsWith("LEGION_") || key.startsWith("DISPATCH_"))
      continue;
    if (["GH_CONFIG_DIR", "GH_TOKEN", "GITHUB_TOKEN", "GH_HOST"].includes(key)) continue;
    if (key === "OMP_SESSION_ID" || key === "TMUX" || key === "TMUX_PANE") continue;
    env[key] = value;
  }
  const home = env.HOME ?? os.homedir();
  const stateDir = path.join(launch.rig, "state");
  const workspace = path.join(launch.rig, "ws");
  const profileEnv = {
    ...env,
    OMP_PROFILE: launch.profile,
    PI_CODING_AGENT_DIR: path.join(home, ".omp", "profiles", launch.profile, "agent"),
  };
  const envoy = sessionEnvoySettings(sessionConfigFile("omp", profileEnv), profileEnv, {
    cwd: workspace,
  });
  if (!envoy.enabled && envoy.error !== null) throw new Error(`envoy: ${envoy.error}`);
  if (envoy.enabled && "off" in envoy.inbound) throw new Error(`envoy: ${envoy.inbound.off}`);
  const request: PaneRequest = {
    ...claim,
    stateDir,
    workspace,
    daemonUrl: `http://127.0.0.1:${launch.port}`,
    envoyUrl: envoy.enabled ? envoy.url : DEFAULT_ENVOY_URL,
    natsUrls: envoy.enabled && "natsUrls" in envoy.inbound ? envoy.inbound.natsUrls : [],
    bootTokenFile: path.join(stateDir, "secrets", "boot"),
    ghToken: RIG_GH_TOKEN,
    path: env.PATH ?? "",
    systemPrompt: source.systemPrompt,
    ghPath: source.ghPath,
  };
  const pane = JSON.parse(
    runDaemonPane(source.daemonModule, ["pane"], JSON.stringify(request))
  ) as DaemonPane;
  return {
    ...pane,
    env: {
      ...env,
      OMP_PROFILE: profileEnv.OMP_PROFILE,
      PI_PROFILE: launch.profile,
      PI_CODING_AGENT_DIR: profileEnv.PI_CODING_AGENT_DIR,
      PI_NOTIFICATIONS: "off",
      PI_NO_TITLE: "1",
      ...pane.env,
    },
  };
}
