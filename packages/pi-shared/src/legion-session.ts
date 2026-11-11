import { isLegionRole, type LegionRole } from "@legion/contracts";

export type LegionSessionKind =
  | { kind: "controller" }
  | { kind: "root-architect"; tree: string }
  | { kind: "phase-worker"; role: LegionRole; tree: string; issue: string }
  | { kind: "not-legion" };

/**
 * What kind of Legion session this environment describes. It lives here, and not in
 * `@sjawhar/pi-legion`, because both plugin entries now read it: the Legion entry to decide what
 * to claim, the Envoy entry to decide whether a process that loaded no Legion plugin is a role's
 * agent (`daemonLaunchedAgentProcess` below). Two copies could come to disagree about what a
 * Legion session is, and then the two entries would answer differently for the same pane.
 */
export function classifySession(env: NodeJS.ProcessEnv): LegionSessionKind {
  if (env.LEGION_CONTROLLER !== undefined && env.LEGION_TREE !== undefined) {
    throw new Error("Legion session has both controller and tree launch markers");
  }

  // The controller marker is LEGION_CONTROLLER alone. `legion controller start` also sets
  // LEGION_ROLE=controller on that process, but "controller" is not a LegionRole and this
  // extension never reads LEGION_ROLE for it — one signal, checked once, so the two markers can
  // never disagree in practice.
  if (env.LEGION_CONTROLLER === "1") return { kind: "controller" };

  if (env.LEGION_ROLE === undefined) return { kind: "not-legion" };

  if (!isLegionRole(env.LEGION_ROLE)) {
    throw new Error(`LEGION_ROLE "${env.LEGION_ROLE}" is not a Legion role`);
  }
  const tree = requiredEnvironment(env, "LEGION_TREE");
  const issue = requiredEnvironment(env, "LEGION_ISSUE");
  // The daemon roots an issue tree at itself: the root architect's own issue
  // key equals the tree's. A sub-architect on a child issue is a phase
  // worker like any other role.
  if (env.LEGION_ROLE === "architect" && issue === tree) {
    return { kind: "root-architect", tree };
  }
  return { kind: "phase-worker", role: env.LEGION_ROLE, tree, issue };
}

export function requiredEnvironment(env: NodeJS.ProcessEnv, key: string): string {
  const value = env[key];
  if (!value) throw new Error(`${key} is required for Legion`);
  return value;
}

/**
 * Whether this process is an Oh My Pi agent the daemon launched as a role's agent. The Envoy
 * entry ends such a process when the Legion entry left no load marker on `globalThis`
 * (INFRA-2883, R2): a plugin root that throws at load leaves Oh My Pi running with no `legion`
 * tool, and the role then spends a whole phase unable to claim, report or hand off.
 *
 * Three marks must hold together, and only together:
 *
 * 1. `--mode rpc` on this process's own command line. The daemon writes it for every Oh My Pi
 *    agent it launches, under either runtime (`packages/daemon/internal/runtime/sandbox/manifest.go`
 *    appends `"--mode", "rpc"`; `packages/daemon/internal/runtime/tmux/prompt.go` writes
 *    `" --mode rpc "`), so both spellings count: `--mode rpc` as two arguments, `--mode=rpc` as
 *    one. This is the mark the environment cannot give, because a command line belongs to the
 *    process that was started with it and no child inherits it.
 * 2. `LEGION_BOOT_TOKEN` or `LEGION_BOOT_TOKEN_FILE` in the environment: the daemon hands a boot
 *    token to the agent it launches for a role and to nothing else, so the daemon's image probe,
 *    which launches Oh My Pi with both plugin roots in a pod whose environment carries no boot
 *    token, and the operator-started controller, which deletes both names from its environment,
 *    are not this process.
 * 3. the shared classifier above reads the environment as a Legion session, a controller
 *    included.
 *
 * The environment alone is not enough, measured in this issue's pod on 2026-11-11: every
 * descendant of the agent inherits it, so a shell, a test and a person's own Oh My Pi inside a
 * role's pod all read the role and the boot token, and an environment-only answer would end all
 * three. The marks are read command line first because that one is the cheapest and the one that
 * rules out every inheriting descendant, and because the classifier throws on an environment the
 * daemon never writes (an unknown `LEGION_ROLE`, both launch markers at once), which no process
 * outside this one should have to meet.
 *
 * Production passes `process.env` and `process.argv`; a test passes its own values.
 */
export function daemonLaunchedAgentProcess(
  env: NodeJS.ProcessEnv,
  argv: readonly string[]
): boolean {
  if (!runsInRpcMode(argv)) return false;
  if (!env.LEGION_BOOT_TOKEN && !env.LEGION_BOOT_TOKEN_FILE) return false;
  return classifySession(env).kind !== "not-legion";
}

function runsInRpcMode(argv: readonly string[]): boolean {
  return argv.some(
    (argument, index) =>
      argument === "--mode=rpc" || (argument === "--mode" && argv[index + 1] === "rpc")
  );
}
