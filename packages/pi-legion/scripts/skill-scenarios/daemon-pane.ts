/**
 * The rig's reading of the Legion daemon: `daemon-pane.go`, run with `go run -overlay` as a main
 * inside a checkout's daemon module, which is how it reaches the daemon's internal packages. The
 * rig asks it rather than restating the daemon: a change to the daemon reaches the rig by
 * construction. The exceptions are `daemon-pane.go`'s own copies of the daemon's table of the
 * handoff words, its resolution of the real gh, its join and write of a system prompt and the parts
 * of a step of a role of the deployment (`stepPromptPaths`), which the program carries because one
 * program runs against each label's checkout of the daemon module, and an older one names them
 * otherwise or lacks them; each one's comment says so, and `daemon-pane_test.go`, which
 * `goInDaemonModule` runs with the program in this checkout's module, holds the last to the
 * daemon's own method. An architect's prompt is composed by the daemon's own function, in
 * `daemon-pane-architect.go`, which is built in only where the checkout declares that function
 * (`daemonPaneOverlay`). `workerPane` builds a phase worker's whole pane environment over it,
 * `composeRolePrompt` the prompt a launch of any role starts with, and `composeStepPrompt` the
 * prompt a launch of a step of a role of the rig's deployment starts with.
 */
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  rmdirSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import os from "node:os";
import path from "node:path";
import type { LegionRole } from "@legion/contracts";
import { DEFAULT_ENVOY_URL } from "@legion/envoy-client/envoy-settings";
import { sessionConfigFile, sessionEnvoySettings } from "@legion/envoy-client/session-settings";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";

/** This checkout's daemon module, the one the rig reads unless a run names another checkout's. */
export const DAEMON_MODULE = path.resolve(import.meta.dir, "../../../daemon");

const PROGRAM = path.join(import.meta.dir, "daemon-pane.go");

/** The part of daemon-pane.go that calls daemon.ArchitectAddressing, the daemon's composition of
 * what an architect is told after its addressing. */
const ARCHITECT_PART = path.join(import.meta.dir, "daemon-pane-architect.go");

/** A declaration of daemon.ArchitectAddressing: gofmt puts the `func` of a top-level function in
 * column 0, where a comment naming it has `//`. */
const ARCHITECT_ADDRESSING = /^func ArchitectAddressing\(/m;

/** The files `go run -overlay` builds daemon-pane.go with in `module`, each keyed by its name under
 * `<module>/cmd/daemon-pane/` (`goInDaemonModule`): the program, and `daemon-pane-architect.go`
 * beside it where a file of the module's internal/daemon package, not a test's, declares
 * daemon.ArchitectAddressing. A checkout older than that function lacks it, and a build with a call
 * it cannot resolve fails in every mode, so there the program is built alone: it serves every mode
 * and role as it did, and refuses an architect's prompt, naming the function the checkout lacks.
 * A checkout that declares the function with other parameters fails the build in every mode, as
 * one whose AddressingFragment takes other parameters does. */
export function daemonPaneOverlay(module: string): Record<string, string> {
  const overlay: Record<string, string> = { "main.go": PROGRAM };
  const daemon = path.join(module, "internal", "daemon");
  const declared = readdirSync(daemon).some(
    (file) =>
      file.endsWith(".go") &&
      !file.endsWith("_test.go") &&
      ARCHITECT_ADDRESSING.test(readFileSync(path.join(daemon, file), "utf8"))
  );
  if (declared) overlay["architect.go"] = ARCHITECT_PART;
  return overlay;
}

/** Runs `go <command> -overlay <overlay> ./cmd/daemon-pane ...args` in `module`, a checkout's
 * daemon module, where the overlay puts each of `files` (a file's name under
 * `<module>/cmd/daemon-pane/`, and the rig's file that stands there), with `input` on its stdin
 * (the caller's own stdin when there is none), and returns its stdout. It throws, with what the
 * command printed, when the command exits other than 0. */
export function goInDaemonModule(
  module: string,
  files: Record<string, string>,
  command: "run" | "test",
  args: readonly string[],
  input?: string
): string {
  const scratch = mkdtempSync(path.join(os.tmpdir(), "daemon-pane-"));
  const directory = path.join(module, "cmd", "daemon-pane");
  // `go test` vets the package and runs its test binary in the package's own directory, which
  // only the overlay holds, so a test run makes that directory, empty, and removes it after.
  const made = command === "test" && !existsSync(directory);
  try {
    const overlay = path.join(scratch, "overlay.json");
    const replace = Object.fromEntries(
      Object.entries(files).map(([name, file]) => [path.join(directory, name), file])
    );
    writeFileSync(overlay, JSON.stringify({ Replace: replace }));
    if (made) mkdirSync(directory);
    const go = ["go", command, "-overlay", overlay, "./cmd/daemon-pane", ...args];
    const result = Bun.spawnSync(go, {
      cwd: module,
      stdin: input === undefined ? "inherit" : new TextEncoder().encode(input),
      stdout: "pipe",
      stderr: "pipe",
    });
    if (result.exitCode !== 0) {
      const printed = [result.stderr.toString().trim(), result.stdout.toString().trim()];
      throw new Error(
        `go ${command} ./cmd/daemon-pane ${args.join(" ")} in ${module} exited ${result.exitCode}: ${printed.filter((text) => text !== "").join("\n")}`
      );
    }
    return result.stdout.toString();
  } finally {
    if (made && existsSync(directory)) rmdirSync(directory);
    rmSync(scratch, { recursive: true, force: true });
  }
}

/** Runs daemon-pane.go in `module`, with daemon-pane-architect.go where the module's daemon
 * declares the function it calls (`daemonPaneOverlay`), with `args`, `input` on its stdin (the
 * caller's own stdin when there is none), and returns its stdout. */
export function runDaemonPane(module: string, args: readonly string[], input?: string): string {
  return goInDaemonModule(module, daemonPaneOverlay(module), "run", args, input);
}

/** The prompt a launch of `role` starts with, as the daemon in `module` composes it (daemon-pane.go
 * `prompt`): the role's parts, a blank line, then its addressing, which for an architect goes on
 * with what the module's daemon tells every architect after it (daemon.ArchitectAddressing): the
 * roles of the rig's deployment and the models, for the architect of a tree's root (`root`) then
 * the design gate steps, and the production check, byte for byte as the daemon writes the prompt
 * file. The claim is the rig's: its issue is the root of its tree, or a child under that root.
 * Throws for an architect in a checkout whose daemon declares no ArchitectAddressing
 * (`daemonPaneOverlay`). */
export function composeRolePrompt(module: string, role: LegionRole, root: boolean): string {
  return runDaemonPane(module, ["prompt", role, root ? "root" : "child"]);
}

/** The two roles of the rig's deployment (daemon-pane.go's `rigRoles`): the checker, whose steps
 * are of the type testing, and the critic, of the type review. */
export type RigRole = "checker" | "critic";

/** The prompt a launch of a step of `role` starts with, as the daemon in `module` composes it
 * (daemon-pane.go `prompt step`): the headless part every phase worker is told, the role's own
 * prompt file, `promptFile`, then the daemon's part every phase worker ends with, a blank line, then
 * the addressing of the rig's claim of that role on a child issue, byte for byte as the daemon
 * writes the prompt file. It throws where the module's daemon has no key `roles`, since no launch
 * of that daemon starts a role of a deployment. */
export function composeStepPrompt(
  module: string,
  promptFile: string,
  role: RigRole = "checker"
): string {
  return runDaemonPane(module, ["prompt", "step", path.resolve(promptFile), role]);
}

/** The claim a pane is launched for and the rig's stand-ins for what the daemon knows at boot
 * (daemon-pane.go's paneRequest). `ghToken` stands in for the claim's GitHub App token: it is
 * rendered into the claim's gh bundle under `<stateDir>/secrets/<claim>-gh`, the pane's
 * `LEGION_GH_OWNERS_DIR`, whose one owner's directory is its `GH_CONFIG_DIR`, and its
 * `LEGION_GH_PATH` is `ghPath` when the rig names one (its recording stand-in), else the real gh as
 * the daemon resolves it at boot, the first `gh` on `path`, refused when that is a script; omitted,
 * the pane gets no gh variable, as from a daemon with no GitHub Apps. */
interface PaneRequest {
  readonly project: string;
  readonly issue: string;
  readonly role: string;
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
 * files, and the pane's own `gh auth token` prints it. Random, as a real one is, since a process
 * that reads it holds it and scrubs it from every line it prints
 * (`@legion/contracts/held-secrets`). */
export const RIG_GH_TOKEN = fixtureSecret("gh");

/** Where a worker pane runs: the run directory that stands for the daemon's state directory and
 * the issue's workspace, the stand-in daemon's port, and the Oh My Pi profile. */
export interface WorkerLaunch {
  readonly rig: string;
  readonly port: number;
  readonly profile: string;
}

/** The claim a worker pane is launched for: its project, issue and role. */
export interface WorkerClaim {
  readonly project: string;
  readonly issue: string;
  readonly role: LegionRole;
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
 * `legion` from bash; and, with `systemPrompt`, the system prompt argument. An inherited
 * `ANTHROPIC_API_KEY`, every `LEGION_*` and `DISPATCH_*` value, the caller's own GitHub variables
 * and its session variables (`OMP_SESSION_ID`, `TMUX`, `TMUX_PANE` and `OMP_SESSION_EXCLUSIVE`,
 * the hold the daemon gives a resumed launch alone, where a scenario pane is a fresh launch) are
 * dropped first. The profile's agent directory, and so its envoy.json, is under the inherited
 * `HOME`. Rejects `envoy: <why>` when that Envoy is off with an error, or on with inbound messages
 * off (`inbound.off`: no NATS server, or its seed refused), since the pane's Legion claim refuses
 * then. */
export async function workerPane(
  launch: WorkerLaunch,
  inherited: NodeJS.ProcessEnv,
  claim: WorkerClaim,
  source: PaneSource
): Promise<DaemonPane> {
  const env: Record<string, string> = {};
  for (const [key, value] of Object.entries(inherited)) {
    if (value === undefined) continue;
    if (key === "ANTHROPIC_API_KEY" || key.startsWith("LEGION_") || key.startsWith("DISPATCH_"))
      continue;
    if (["GH_CONFIG_DIR", "GH_TOKEN", "GITHUB_TOKEN", "GH_HOST"].includes(key)) continue;
    if (["OMP_SESSION_ID", "OMP_SESSION_EXCLUSIVE", "TMUX", "TMUX_PANE"].includes(key)) continue;
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
  const envoy = await sessionEnvoySettings(sessionConfigFile("omp", profileEnv), profileEnv, {
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
