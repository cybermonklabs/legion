/**
 * The rig's reading of the Legion daemon: `daemon-pane.go`, run with `go run -overlay` as a main
 * inside a checkout's daemon module, which is how it reaches the daemon's internal packages. The
 * rig asks it rather than restating the daemon: a change to the daemon reaches the rig by
 * construction. The exceptions are `daemon-pane.go`'s own copies of the daemon's table of the
 * handoff words, its resolution of the real gh, its join and write of a system prompt, and its rule
 * of an owner's launch and its read and copy of a plan guidance, which the program carries because
 * one program runs against each label's checkout of the daemon module, and an older one names them
 * otherwise or lacks them; each one's comment says so. `daemon-pane_test.go` holds four of them to
 * the daemon's at this checkout, the owner rule, the read, the copy and the join of a prompt
 * (`goInDaemonModule` compiles it with the program under `go test -c`).
 * `workerPane` builds a phase worker's whole pane environment over it, and `composeRolePrompt` the
 * prompt a launch of any role starts with.
 */
import { existsSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import type { LegionRole } from "@legion/contracts";
import { DEFAULT_ENVOY_URL } from "@legion/envoy-client/envoy-settings";
import { sessionConfigFile, sessionEnvoySettings } from "@legion/envoy-client/session-settings";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";

/** This checkout's daemon module, the one the rig reads unless a run names another checkout's. */
export const DAEMON_MODULE = path.resolve(import.meta.dir, "../../../daemon");

const PROGRAM = path.join(import.meta.dir, "daemon-pane.go");

/** Runs `./cmd/daemon-pane` in `module` and returns its stdout: for `run`, the program, `go run`,
 * with `args`; for `test`, its test binary, built by `go test -c`, with `args`, the binary's flags
 * (`-test.run`, `-test.v`). `input` is its stdin, the caller's own when there is none. Each key of
 * `files` is a path of the module, and the overlay stands the rig's file its value names there:
 * `daemon-pane.go` as `cmd/daemon-pane/main.go`, for a test `daemon-pane_test.go` as
 * `cmd/daemon-pane/main_test.go` beside it, and a file of the module for a run against a daemon
 * that differs from this checkout's in that file alone. The
 * `cmd/daemon-pane` directory is the overlay's alone, never made in the checkout, and `go test`
 * runs a test binary and its vet in the package's directory on disk, so a test is built with vet
 * off and its binary run in the run's scratch directory. */
export function goInDaemonModule(
  module: string,
  files: Record<string, string>,
  command: "run" | "test",
  args: readonly string[],
  input?: string
): string {
  const scratch = mkdtempSync(path.join(os.tmpdir(), "daemon-pane-"));
  try {
    const overlay = path.join(scratch, "overlay.json");
    const replace = Object.fromEntries(
      Object.entries(files).map(([name, file]) => [path.join(module, name), file])
    );
    writeFileSync(overlay, JSON.stringify({ Replace: replace }));
    if (command === "run") {
      return spawnOrThrow(
        ["go", "run", "-overlay", overlay, "./cmd/daemon-pane", ...args],
        module,
        input
      );
    }
    const binary = path.join(scratch, "daemon-pane.test");
    const built = spawnOrThrow(
      ["go", "test", "-c", "-vet=off", "-o", binary, "-overlay", overlay, "./cmd/daemon-pane"],
      module
    );
    // `go test -c` exits 0 and writes no binary for a package with no test file.
    if (!existsSync(binary)) {
      throw new Error(`go test -c in ${module} wrote no test binary: ${built.trim()}`);
    }
    return spawnOrThrow([binary, ...args], scratch, input);
  } finally {
    rmSync(scratch, { recursive: true, force: true });
  }
}

/** Runs `argv` in `cwd`, `input` on its stdin (the caller's own stdin when there is none), and
 * returns its stdout, or throws with its stderr and its stdout, where a test binary reports a
 * failed test, when it exits other than 0. */
function spawnOrThrow(argv: readonly string[], cwd: string, input?: string): string {
  const result = Bun.spawnSync([...argv], {
    cwd,
    stdin: input === undefined ? "inherit" : new TextEncoder().encode(input),
    stdout: "pipe",
    stderr: "pipe",
  });
  if (result.exitCode !== 0) {
    const report = [result.stderr.toString().trim(), result.stdout.toString().trim()]
      .filter((text) => text !== "")
      .join("\n");
    throw new Error(`${argv.join(" ")} in ${cwd} exited ${result.exitCode}: ${report}`);
  }
  return result.stdout.toString();
}

/** Runs daemon-pane.go in `module` with `args`, `input` on its stdin (the caller's own stdin when
 * there is none), and returns its stdout. */
export function runDaemonPane(module: string, args: readonly string[], input?: string): string {
  return goInDaemonModule(module, { "cmd/daemon-pane/main.go": PROGRAM }, "run", args, input);
}

/** The prompt a launch of `role` starts with, as the daemon in `module` composes it (daemon-pane.go
 * `prompt`): the role's parts, a blank line, then its addressing, and for the architect of a tree's
 * root (`root`) the design gate steps after that, byte for byte as the daemon writes the prompt
 * file. The claim is the rig's: its issue is the root of its tree, or a child under that root.
 * `planGuidance` is the path of a deployment's plan guidance file: an owner's prompt then ends with
 * a blank line and the daemon's copy of that file, and any other role's is unchanged; a checkout
 * whose daemon has no key `plan_guidance` refuses it. */
export function composeRolePrompt(
  module: string,
  role: LegionRole,
  root: boolean,
  options: { planGuidance?: string } = {}
): string {
  const guidance =
    options.planGuidance === undefined ? [] : ["--plan-guidance", options.planGuidance];
  return runDaemonPane(module, ["prompt", role, root ? "root" : "child", ...guidance]);
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
