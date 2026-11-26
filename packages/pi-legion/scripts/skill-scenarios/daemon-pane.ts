/**
 * The rig's reading of the Legion daemon: `daemon-pane.go`, run with `go run -overlay` as a main
 * inside a checkout's daemon module, which is how it reaches the daemon's internal packages. The
 * rig asks it rather than restating the daemon: a change to the daemon reaches the rig by
 * construction. The exceptions are `daemon-pane.go`'s own copies of the daemon's table of the
 * handoff words, its resolution of the real gh, its join and write of a system prompt and the
 * architect's `production_check` sentence, which the program carries because one program runs
 * against each label's checkout of the daemon module, and an older one names them otherwise or
 * lacks them; each one's comment says so. `workerPane` builds a pane's whole environment over it
 * (a phase worker's, an architect's or the daemon-launched controller's), and `composeRolePrompt`
 * the prompt a launch of any role starts with, the interactive controller's included.
 */
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import type { LegionRole } from "@legion/contracts";
import { DEFAULT_ENVOY_URL } from "@legion/envoy-client/envoy-settings";
import { sessionConfigFile, sessionEnvoySettings } from "@legion/envoy-client/session-settings";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";

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

/** A role the daemon launches: a phase worker's or an architect's, or the project's controller. */
export type LaunchRole = LegionRole | "controller";

/** Where a launch's claim stands: on the root issue of the rig's tree (`root`) or on a child issue
 * under that root (`child`), for every role but the controller; for the controller, how its launch
 * starts, the daemon's own under `controller: daemon` (`headless`) or `legion controller start`'s
 * (`interactive`). */
export type PromptPosition = "root" | "child" | "headless" | "interactive";

/** The backend a role runs on (daemon `runtime.Backends`): Oh My Pi, or Claude Code, which runs the
 * phase workers alone and is told its own prompt part after the role's parts. */
export type PromptBackend = "omp" | "claude";

/** The prompt a launch of `role` starts with, as the daemon in `module` composes it (daemon-pane.go
 * `prompt`): the role's parts, the backend's part (`backend`, Oh My Pi by default, which has
 * none), a blank line, then its addressing: a worker's and an architect's for the rig's repository,
 * after which the architect of a tree's root is told the design gate steps and every architect the
 * `production_check` line; the controller's is the design gate steps and its reach. Byte for byte
 * as the daemon writes the prompt file. The claim is the rig's: its issue is the root of its tree,
 * or a child under that root. */
export function composeRolePrompt(
  module: string,
  role: LaunchRole,
  position: PromptPosition,
  backend: PromptBackend = "omp"
): string {
  return runDaemonPane(module, ["prompt", role, position, backend]);
}

/** The claim a pane is launched for and the rig's stand-ins for what the daemon knows at boot
 * (daemon-pane.go's paneRequest). `tree` is the root of the tree a child's issue is in (omitted: the
 * issue is a root); the controller's claim names no issue and no tree and says how its launch
 * starts in `controller`. `ghToken` stands in for the claim's GitHub App token: it is
 * rendered into the claim's gh bundle under `<stateDir>/secrets/<claim>-gh`, the pane's
 * `LEGION_GH_OWNERS_DIR`, whose one owner's directory is its `GH_CONFIG_DIR`, and its
 * `LEGION_GH_PATH` is `ghPath` when the rig names one (its recording stand-in), else the real gh as
 * the daemon resolves it at boot, the first `gh` on `path`, refused when that is a script; omitted,
 * the pane gets no gh variable, as from a daemon with no GitHub Apps. */
interface PaneRequest {
  readonly project: string;
  readonly issue: string;
  readonly tree?: string;
  readonly role: string;
  readonly controller?: "headless" | "interactive";
  readonly backend?: PromptBackend;
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

/** The claim a pane is launched for: its project, issue and role; `tree`, the root of the tree a
 * child's issue is in (omitted: a claim on a root); and for the controller, whose claim names no
 * issue (`issue: ""`) and no tree, how its launch starts (`controller`). The interactive
 * controller has no pane: `workerPane` refuses it, and `composeRolePrompt` composes its prompt. */
export interface WorkerClaim {
  readonly project: string;
  readonly issue: string;
  readonly tree?: string;
  readonly role: LaunchRole;
  readonly controller?: "headless" | "interactive";
}

/** Where a pane's daemon is read from: a checkout's daemon module (`DAEMON_MODULE` for this one),
 * whose embedded role prompts compose the pane's system prompt when `systemPrompt` is set, for the
 * backend the role runs on (`backend`, Oh My Pi by default). */
export interface PaneSource {
  readonly daemonModule: string;
  readonly systemPrompt?: boolean;
  readonly backend?: PromptBackend;
  /** The real gh the pane is told, as an operator names one with `LEGION_GH_PATH`: the rig's
   * recording stand-in. Omitted, the daemon's own resolution on the inherited PATH. */
  readonly ghPath?: string;
}

/** The pane the Legion daemon's tmux runtime gives a phase worker, an architect or the daemon's own
 * controller (`claim.controller` `headless`) for `claim`, pointed at the
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
    backend: source.backend,
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
