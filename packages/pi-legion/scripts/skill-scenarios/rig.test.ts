import { beforeAll, describe, expect, test } from "bun:test";
import { createHash } from "node:crypto";
import {
  appendFileSync,
  cpSync,
  existsSync,
  mkdirSync,
  readdirSync,
  readFileSync,
  realpathSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { homedir } from "node:os";
import path from "node:path";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import { memoryRoot, temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { argMaxForceHolds } from "../../../../scripts/e2e/lib/arg-max-force";
import { ownGroup, untilRunning } from "../../../../scripts/e2e/lib/own-group";
import { procRootOf } from "../../../../scripts/e2e/lib/proc-root";
import { rowWaits } from "../../../../scripts/e2e/lib/row-waits";
import { scriptAssignments, scriptFunctions } from "../../../../scripts/e2e/lib/script-functions";
import { OWNER_WALK_STAGES } from "./owner-walk";
import { modelProblem, transcriptModels } from "./scored-runs";
import { RunRow } from "./scored-runs-contracts";

// Every test here runs rig.sh and its stand-ins as real processes. On a loaded machine one test
// takes longer than bun's 5 s default (the eight-turn conversation took 11 s), so each test gets
// one minute, and a wait of a row on its processes (untilRunning) ends 20 s before that.
const waits = rowWaits(60_000);

// The judge runs in a Docker container; every other row here needs no container. Read once, so
// one warning names docker for the whole file.
const withoutDocker = skipWithout("docker");

// rig.sh's install_model_route, run with the checkout's real installers: the route a label's
// profile reaches Anthropic by, picked by LEGION_E2E_MODEL_ROUTE.
const root = path.resolve(import.meta.dir, "../../../..");
const fn = scriptFunctions(path.join(import.meta.dir, "rig.sh"));
/**
 * The directory this file's work directory goes in: memoryRoot, so /dev/shm where the machine has
 * a tmpfs there, unless its real path holds a character other than letters, digits and `/._-`, or
 * is in the developer's home, and then /tmp. A plain path, because the model route installers and
 * judge-omp write the path of each key command, and of the key file, into a profile as one plain
 * `!command` word, and refuse any other character in it (install-model-gateway.sh and
 * install-key-file-model-route.sh at `--dest`, rig.sh's judge_keys at the key file): under a
 * TMPDIR with a space or a quote in its name, every row that installs a route failed on that
 * refusal. Not in the developer's home, because childCwd is in this directory (memoryRoot gives
 * TMPDIR where /dev/shm does not suit, and a TMPDIR can be in the home). A tmpfs, because the
 * file's afterAll removes the whole directory under the file's 60 s: on the disk TMPDIR names,
 * that removal of about 1,600 entries took 70 to 90 s at a five-minute load of about 2,200, and
 * failed the file with "a beforeEach/afterEach hook timed out" after every row had passed. `need`
 * is about sixteen times the 1.8 MiB a run held there at its largest (1,392 entries).
 */
const memory = memoryRoot({ need: 32 * 1024 * 1024 });
const memoryPath = realpathSync(memory);
// A HOME that does not exist holds no config, and realpathSync would throw on it.
const developerHome = existsSync(homedir()) ? realpathSync(homedir()) : path.resolve(homedir());
/** Whether DIR, a real path, is the developer's home or a directory in it. Every path is in a
 * home of `/`. */
function inDeveloperHome(dir: string): boolean {
  const relative = path.relative(developerHome, dir);
  return !(relative === ".." || relative.startsWith(`..${path.sep}`) || path.isAbsolute(relative));
}
const workRoot =
  /^[A-Za-z0-9/._-]+$/.test(memoryPath) && !inDeveloperHome(memoryPath) ? memory : "/tmp";
if (workRoot !== memory) {
  console.error(
    `rig.test.ts: ${memory} holds a character a plain \`!command\` word cannot, or is in ${developerHome}, so the work directory is under /tmp`
  );
}
const work = temporaryDirectory("skill-scenarios-rig", { root: workRoot });
const operatorHome = path.join(work, "operator-home");
/** The cwd of each child process that childSpawn starts. The test runner's own cwd is in the
 * checkout, and every checkout on a devbox is in the developer's home. mise reads a config file
 * in the cwd and in each directory above it. Under a HOME that is not the developer's, it reads
 * the developer's ~/.config/mise/config.toml found there as the config of a project, not as its
 * global config. That file holds [settings], [env] and [tool_alias], and mise loads each of them
 * only from a trusted file. Under that HOME no record marks the file trusted, so every mise shim
 * exits 1 there, python3 included ("Config files in /home/<user>/.config/mise/config.toml are not
 * trusted", in services_up's row). The work directory is not in the developer's home (workRoot),
 * so no directory above this one is. */
const childCwd = path.join(work, "child-cwd");
// The home the probes must not reach, with the one thing they look for in it: a judge that could
// read it would list this directory. It and childCwd are staged in a hook, below the one the
// reservation registered: a run that selects no row of this file makes none of them. Each level
// is made on its own, never with `recursive`, so this hook makes nothing above the reserved root
// and a root the fixture did not make is an error here rather than a directory nothing removes.
beforeAll(() => {
  mkdirSync(operatorHome);
  mkdirSync(path.join(operatorHome, ".ssh"));
  writeFileSync(path.join(operatorHome, ".ssh", "id_ed25519"), "not a key\n", { mode: 0o600 });
  mkdirSync(childCwd);
});
/** The caller's PATH, which a row puts its stand-ins ahead of. */
const callerPath = process.env.PATH ?? "";
const miseData =
  process.env.MISE_DATA_DIR ??
  (process.env.XDG_DATA_HOME === undefined
    ? undefined
    : path.join(process.env.XDG_DATA_HOME, "mise"));
/** Runs ARGV, in childCwd, with the test's child environment and ENV over it: the caller's PATH,
 * a HOME of the test's own, and the caller's mise data and config directories when the caller
 * names them. A tool on PATH can be a mise shim (jq and jj are, on a devbox). Under a fresh HOME
 * such a shim finds neither its installs nor its global config, so it fails. make_omp_home keeps
 * the operator's two directories for the processes it starts for the same reason, and base_env
 * passes MISE_DATA_DIR to every agent. A caller that names neither variable gets neither: the
 * developer's own home decides nothing here. It is the one place this file builds that
 * environment, and its `cwd` is the only one in the file but the omp turn's below, so a child that
 * gets the test's HOME gets childCwd with it. Every child of this file starts through it but five,
 * and none of those gets the test's HOME: the key command, the libpq probe and the model stand-in
 * pass no environment, so they run under the caller's own HOME, where mise reads the caller's
 * config as its global config. runProcess's child gets PATH and SKILL_SCENARIO_RUN alone, and
 * runs sleep, not a mise shim. The model stand-in row's omp turn gets its profile's HOME and the
 * system's PATH alone, in a workspace under this file's work directory. */
function childSpawn(argv: string[], env: Record<string, string> = {}) {
  return Bun.spawnSync(argv, {
    env: {
      PATH: callerPath,
      HOME: operatorHome,
      ...(miseData === undefined ? {} : { MISE_DATA_DIR: miseData }),
      ...(process.env.XDG_CONFIG_HOME === undefined
        ? {}
        : { XDG_CONFIG_HOME: process.env.XDG_CONFIG_HOME }),
      ...env,
    },
    cwd: childCwd,
  });
}

let profiles = 0;
/** A label's profile directory with an empty Oh My Pi home, as make_omp_home leaves it. */
function profileDir(): string {
  const dir = path.join(work, `profile-${++profiles}`);
  mkdirSync(path.join(dir, "home", ".omp"), { recursive: true });
  return dir;
}
function keyFile(mode: number): string {
  const file = path.join(work, `key-${profiles}-${mode.toString(8)}`);
  writeFileSync(file, "sk-ant-rig-test-0000000000000000\n", { mode });
  return file;
}
function route(dir: string, env: Record<string, string>, model = "") {
  const result = childSpawn(
    [
      "bash",
      "-c",
      `route_model_id=claude-opus-4-8\n${fn("install_model_route")}\n${fn("install_provider_route")}\n${fn("route_config")}\ninstall_model_route "$1" skills-test "$2" "$3"`,
      "-",
      root,
      dir,
      model,
    ],
    env
  );
  return { code: result.exitCode, stderr: result.stderr.toString() };
}

describe("install_model_route", () => {
  test("key-file writes the profile's route, its key command under model-key and its record under key", () => {
    const dir = profileDir();
    const { code, stderr } = route(dir, {
      LEGION_E2E_MODEL_ROUTE: "key-file",
      LEGION_E2E_ANTHROPIC_KEY_FILE: keyFile(0o600),
    });
    expect({ code, stderr: code === 0 ? "" : stderr }).toEqual({ code: 0, stderr: "" });
    const models = readFileSync(
      path.join(dir, "home/.omp/profiles/skills-test/agent/models.yml"),
      "utf8"
    );
    expect(models).toContain(`!exec ${path.join(dir, "model-key", "anthropic-key")}`);
    expect(models).toContain("https://api.anthropic.com");
    const served = Bun.spawnSync([path.join(dir, "model-key", "anthropic-key")]);
    expect(served.stdout.toString().trim()).toBe("sk-ant-rig-test-0000000000000000");
    expect(existsSync(path.join(dir, "key", "anthropic-key.log"))).toBe(true);
  });

  test("key-file without LEGION_E2E_ANTHROPIC_KEY_FILE fails, naming the variable", () => {
    const dir = profileDir();
    const { code, stderr } = route(dir, { LEGION_E2E_MODEL_ROUTE: "key-file" });
    expect(code).toBe(1);
    expect(stderr).toContain("LEGION_E2E_ANTHROPIC_KEY_FILE is unset");
    expect(existsSync(path.join(dir, "home/.omp/profiles/skills-test/agent/models.yml"))).toBe(
      false
    );
  });

  test("a key file others can read is refused, and no key command is left behind", () => {
    const dir = profileDir();
    const { code, stderr } = route(dir, {
      LEGION_E2E_MODEL_ROUTE: "key-file",
      LEGION_E2E_ANTHROPIC_KEY_FILE: keyFile(0o644),
    });
    expect(code).not.toBe(0);
    expect(stderr).toContain("group or other can read it");
    expect(existsSync(path.join(dir, "model-key"))).toBe(false);
  });

  /** An operator's models.yml holding the gateway block the provider route names, and another. */
  function providerFile(apiKey: string): string {
    const file = path.join(work, `models-${profiles}.yml`);
    writeFileSync(
      file,
      [
        "providers:",
        "  openai:",
        '    apiKey: "!secrets get --value OPENAI_KEY"',
        "  box-gateway:",
        "    baseUrl: http://gateway.internal.example:4000",
        `    apiKey: "${apiKey}"`,
        "    api: anthropic-messages",
        "    authHeader: true",
        "    models:",
        "      - id: claude-opus-5-5",
        "        thinking:",
        "          mode: anthropic-adaptive",
        "          efforts: [low, high]",
        "",
      ].join("\n")
    );
    return file;
  }

  test("provider copies the named block, its !cat key command as written, with the rig's one model", () => {
    const dir = profileDir();
    const { code, stderr } = route(dir, {
      LEGION_E2E_MODEL_ROUTE: "provider",
      LEGION_E2E_MODEL_PROVIDER_FILE: providerFile("!cat /run/user/1000/box/gateway-key"),
      LEGION_E2E_MODEL_PROVIDER: "box-gateway",
    });
    expect({ code, stderr: code === 0 ? "" : stderr }).toEqual({ code: 0, stderr: "" });
    const agent = path.join(dir, "home/.omp/profiles/skills-test/agent");
    expect(JSON.parse(readFileSync(path.join(agent, "models.yml"), "utf8"))).toEqual({
      providers: {
        "box-gateway": {
          baseUrl: "http://gateway.internal.example:4000",
          apiKey: "!cat /run/user/1000/box/gateway-key",
          api: "anthropic-messages",
          authHeader: true,
          models: [{ id: "claude-opus-4-8" }],
        },
      },
    });
    const config = readFileSync(path.join(agent, "config.yml"), "utf8");
    expect(config).toContain("enabledModels:\n  - box-gateway/claude-opus-4-8\n");
    expect(config).toContain("  - anthropic\n");
    expect(config).toContain("  default: box-gateway/claude-opus-4-8\n");
    expect(config).toContain("  task: box-gateway/claude-opus-4-8\n");
    expect(existsSync(path.join(dir, "model-key"))).toBe(false);
  });

  test("provider with a model the block lists takes the block's own entry for it", () => {
    const dir = profileDir();
    const { code, stderr } = route(
      dir,
      {
        LEGION_E2E_MODEL_ROUTE: "provider",
        LEGION_E2E_MODEL_PROVIDER_FILE: providerFile("!cat /run/user/1000/box/gateway-key"),
        LEGION_E2E_MODEL_PROVIDER: "box-gateway",
      },
      "claude-opus-5-5"
    );
    expect({ code, stderr: code === 0 ? "" : stderr }).toEqual({ code: 0, stderr: "" });
    const agent = path.join(dir, "home/.omp/profiles/skills-test/agent");
    expect(
      JSON.parse(readFileSync(path.join(agent, "models.yml"), "utf8")).providers["box-gateway"]
        .models
    ).toEqual([
      { id: "claude-opus-5-5", thinking: { mode: "anthropic-adaptive", efforts: ["low", "high"] } },
    ]);
    expect(readFileSync(path.join(agent, "config.yml"), "utf8")).toContain(
      "  default: box-gateway/claude-opus-5-5\n"
    );
  });

  // An Oh My Pi config that disables a provider by name disables a models.yml block of that name
  // too, so a box whose gateway block is named `anthropic` would leave the agents no model.
  test("provider whose block is named after a built-in provider leaves that provider enabled", () => {
    const dir = profileDir();
    const file = path.join(work, `models-${profiles}-anthropic.yml`);
    writeFileSync(
      file,
      [
        "providers:",
        "  anthropic:",
        "    baseUrl: http://gateway.internal.example:4000",
        '    apiKey: "!cat /run/user/1000/box/gateway-key"',
        "    api: anthropic-messages",
        "",
      ].join("\n")
    );
    const { code, stderr } = route(dir, {
      LEGION_E2E_MODEL_ROUTE: "provider",
      LEGION_E2E_MODEL_PROVIDER_FILE: file,
      LEGION_E2E_MODEL_PROVIDER: "anthropic",
    });
    expect({ code, stderr: code === 0 ? "" : stderr }).toEqual({ code: 0, stderr: "" });
    const config = readFileSync(
      path.join(dir, "home/.omp/profiles/skills-test/agent/config.yml"),
      "utf8"
    );
    expect(config).toContain("enabledModels:\n  - anthropic/claude-opus-4-8\n");
    expect(config).toContain("disabledProviders:\n  - amazon-bedrock\n");
    expect(config).not.toContain("  - anthropic\n");
  });

  test("provider without LEGION_E2E_MODEL_PROVIDER_FILE fails, naming the variable", () => {
    const dir = profileDir();
    const { code, stderr } = route(dir, {
      LEGION_E2E_MODEL_ROUTE: "provider",
      LEGION_E2E_MODEL_PROVIDER: "box-gateway",
    });
    expect(code).toBe(1);
    expect(stderr).toContain("LEGION_E2E_MODEL_PROVIDER_FILE is unset");
  });

  test("provider refuses a block absent from the file, and one whose key is not read by !cat", () => {
    const missing = route(profileDir(), {
      LEGION_E2E_MODEL_ROUTE: "provider",
      LEGION_E2E_MODEL_PROVIDER_FILE: providerFile("!cat /run/user/1000/box/gateway-key"),
      LEGION_E2E_MODEL_PROVIDER: "elsewhere",
    });
    expect(missing.code).toBe(1);
    expect(missing.stderr).toContain("has no provider block elsewhere");
    const literal = profileDir();
    const inline = route(literal, {
      LEGION_E2E_MODEL_ROUTE: "provider",
      LEGION_E2E_MODEL_PROVIDER_FILE: providerFile("sk-literal-key"),
      LEGION_E2E_MODEL_PROVIDER: "box-gateway",
    });
    expect(inline.code).toBe(1);
    expect(inline.stderr).toContain('does not read its key as "!cat <absolute file>"');
    expect(existsSync(path.join(literal, "home/.omp/profiles/skills-test/agent/models.yml"))).toBe(
      false
    );
  });

  test("an unknown route fails, naming the three it takes", () => {
    const dir = profileDir();
    const { code, stderr } = route(dir, { LEGION_E2E_MODEL_ROUTE: "bedrock" });
    expect(code).toBe(1);
    expect(stderr).toContain("it is gateway, key-file or provider");
  });
});

// The key-file route's key command reads the operator's key for as long as it exists
// (install-key-file-model-route.sh: the caller removes --key-dir on every exit that keeps no
// profile). A setup that stops after the route is installed leaves no profile to use it.
describe("a setup that fails after install_model_route", () => {
  /** A checkout whose plugin installer and home maker do nothing, with the real key-file route. */
  function checkout(): string {
    const co = freshDir("route-checkout");
    mkdirSync(path.join(co, "scripts/e2e/lib"), { recursive: true });
    mkdirSync(path.join(co, "skills/asd-ste100"), { recursive: true });
    writeFileSync(path.join(co, "skills/asd-ste100/SKILL.md"), "# STE\n");
    writeFileSync(path.join(co, ".omp-pin"), "omp@pin\n");
    // The real key-file route, with the libs it sources: model-endpoint.sh for its base URL's
    // check, and omp-home.sh for the line that shuts the setup wizard, whose make_omp_home is then
    // replaced by one that only makes the home's .omp.
    for (const file of ["install-key-file-model-route.sh", "model-endpoint.sh", "omp-home.sh"]) {
      cpSync(path.join(root, "scripts/e2e/lib", file), path.join(co, "scripts/e2e/lib", file));
    }
    appendFileSync(
      path.join(co, "scripts/e2e/lib/omp-home.sh"),
      'make_omp_home() { mkdir -p "$1/.omp"; }\n'
    );
    writeFileSync(path.join(co, "scripts/e2e/lib/install-plugin-profile.sh"), "exit 0\n");
    return co;
  }
  const routeEnv = () => ({
    LEGION_E2E_MODEL_ROUTE: "key-file",
    LEGION_E2E_ANTHROPIC_KEY_FILE: keyFile(0o600),
  });

  // Catches: a label profile that fails after its route is installed (here the superpowers probe)
  // and keeps a key command that still reads the operator's key.
  test("cmd_profile removes the label's model-key and marks nothing ready", () => {
    const work = freshDir("route-work");
    const script = [
      "set -euo pipefail",
      ...[
        "fail",
        "label_dir",
        "on_exit",
        "drop_unready_keys",
        "install_model_route",
        "route_model_line",
        "cmd_profile",
      ].map(fn),
      "checkout_commit() { printf 'fixture parents=base\\n'; }",
      "install_superpowers() { fail 'the superpowers probe failed'; }",
      'work=$1; cmd_profile probe "$2"',
    ].join("\n");
    const failed = bash(script, [work, checkout()], {
      ...routeEnv(),
      SKILL_SCENARIOS_SUPERPOWERS: "set",
    });
    expect(failed.code).toBe(1);
    expect(failed.stderr).toContain("the superpowers probe failed");
    const P = path.join(work, "profiles/probe");
    expect(existsSync(path.join(P, "home/.omp/profiles/skills-probe/agent/models.yml"))).toBe(true);
    expect(existsSync(path.join(P, "model-key"))).toBe(false);
    expect(existsSync(path.join(P, "ready"))).toBe(false);

    // The same setup that succeeds keeps its key command: a ready profile uses it.
    const kept = freshDir("route-work");
    const ok = bash(
      script.replace("fail 'the superpowers probe failed'", ":"),
      [kept, checkout()],
      {
        ...routeEnv(),
        SKILL_SCENARIOS_SUPERPOWERS: "set",
      }
    );
    expect({ code: ok.code, stderr: ok.stderr }).toEqual({ code: 0, stderr: "" });
    expect(existsSync(path.join(kept, "profiles/probe/model-key/anthropic-key"))).toBe(true);
    expect(existsSync(path.join(kept, "profiles/probe/ready"))).toBe(true);
  });

  /** judge_profiles and its helpers, called on the checkout $1 and the work directory $2. A row
   * puts an install_model_route definition before it. */
  const judgeProfiles = [
    "set -euo pipefail",
    ...["fail", "on_exit", "drop_unready_keys", "judge_profiles"].map(fn),
    "checkout_commit() { printf 'fixture parents=base\\n'; }",
    "root=$1; work=$2; judge_model=fixture; judge_thinking=high; judge_profiles",
  ].join("\n");

  // Catches: a judge-profile build that fails at its second profile and keeps the first one's key
  // command.
  test("judge_profiles removes both profiles' model-key and marks nothing ready", () => {
    const work = freshDir("judge-route-work");
    // The bare profile's route cannot be installed, after the ste profile's is.
    const script = [
      fn("install_model_route").replace("install_model_route()", "installed_route()"),
      'install_model_route() { [ "$2" != judge-bare ] || { echo "no route for bare" >&2; return 1; }; installed_route "$@"; }',
      judgeProfiles,
    ].join("\n");
    const failed = bash(script, [checkout(), work], routeEnv());
    expect(failed.code).toBe(1);
    expect(failed.stderr).toContain("the judge's model route (key-file) could not be installed");
    const dir = path.join(work, "judge-profiles");
    expect(readFileSync(path.join(dir, "bare/install.log"), "utf8")).toBe("no route for bare\n");
    expect(existsSync(path.join(dir, "ste/home/.omp/profiles/judge-ste/agent/models.yml"))).toBe(
      true
    );
    expect(existsSync(path.join(dir, "ste/model-key"))).toBe(false);
    expect(existsSync(path.join(dir, "bare/model-key"))).toBe(false);
    expect(existsSync(path.join(dir, "ready"))).toBe(false);
  });

  // Catches: a judge profile that needs jj. No Oh My Pi and no jj runs under a judge's HOME on this
  // host, yet a home made by make_omp_home read the operator's jj config, so judge-omp failed on a
  // machine with no jj on PATH ("make_omp_home failed for the judge's ste profile"). Here the
  // checkout's omp-home.sh is the real one, and the only jj on PATH records its call and fails.
  test("judge_profiles builds both profiles and runs no jj", () => {
    const work = freshDir("judge-nojj-work");
    const co = checkout();
    cpSync(
      path.join(root, "scripts/e2e/lib/omp-home.sh"),
      path.join(co, "scripts/e2e/lib/omp-home.sh")
    );
    const bin = freshDir("judge-nojj-bin");
    const calls = path.join(bin, "jj-calls");
    writeFileSync(
      path.join(bin, "jj"),
      `#!/bin/sh\necho "$*" >>'${calls}'\necho "jj: not on this machine" >&2\nexit 127\n`,
      { mode: 0o755 }
    );
    const built = bash(`${fn("install_model_route")}\n${judgeProfiles}`, [co, work], {
      ...routeEnv(),
      PATH: `${bin}:${callerPath}`,
    });
    expect({ code: built.code, stderr: built.stderr }).toEqual({ code: 0, stderr: "" });
    expect(existsSync(calls)).toBe(false);
    const dir = path.join(work, "judge-profiles");
    for (const kind of ["ste", "bare"]) {
      expect(
        existsSync(path.join(dir, `${kind}/home/.omp/profiles/judge-${kind}/agent/models.yml`))
      ).toBe(true);
    }
    expect(
      existsSync(
        path.join(dir, "ste/home/.omp/profiles/judge-ste/agent/skills/asd-ste100/SKILL.md")
      )
    ).toBe(true);
    expect(existsSync(path.join(dir, "ready"))).toBe(true);
  });
});

/** Runs SCRIPT (rig.sh functions and a call of them) under bash with ARGS as $1..., through
 * childSpawn. */
function bash(script: string, args: string[] = [], env: Record<string, string> = {}) {
  const result = childSpawn(["bash", "-c", script, "-", ...args], env);
  return {
    code: result.exitCode,
    stdout: result.stdout.toString(),
    stderr: result.stderr.toString(),
  };
}
let dirs = 0;
/** A new directory under the test's work directory. */
function freshDir(name: string): string {
  const dir = path.join(work, `${name}-${++dirs}`);
  mkdirSync(dir, { recursive: true });
  return dir;
}

// Catches: a child of this file that starts in the test runner's cwd, or a work directory in the
// developer's home. Either way the developer's home is above the child's cwd, where a mise shim
// run under the test's HOME exits 1 (childCwd). Every child that gets the test's HOME starts
// through childSpawn, and bash() is one of them, so this row holds each one. CI's PATH holds no
// mise shim, so services_up's row passes in CI either way. Without childCwd this row fails in CI
// too, because a runner's checkout is in the runner's home.
test("a child of this file starts in childCwd, outside the developer's home", () => {
  const cwd = realpathSync(childCwd);
  expect(bash("pwd -P")).toEqual({ code: 0, stdout: `${cwd}\n`, stderr: "" });
  expect({ cwd, inDeveloperHome: inDeveloperHome(cwd) }).toEqual({ cwd, inDeveloperHome: false });
});

// cmd_profile records the label's commit in built-from with checkout_commit. A git worktree holds
// no .jj, and jj alone printed an empty commit line for it.
describe("checkout_commit", () => {
  const git = (dir: string, ...args: string[]) => {
    const result = childSpawn([
      "git",
      "-C",
      dir,
      "-c",
      "user.name=Rig Test",
      "-c",
      "user.email=rig@example.invalid",
      "-c",
      "commit.gpgsign=false",
      ...args,
    ]);
    if (result.exitCode !== 0) throw new Error(`git ${args.join(" ")}: ${result.stderr}`);
    return result.stdout.toString().trim();
  };
  function gitCheckout() {
    const dir = freshDir("git-checkout");
    git(dir, "init", "-q", "-b", "main");
    writeFileSync(path.join(dir, "a.txt"), "a\n");
    git(dir, "add", "-A");
    git(dir, "commit", "-qm", "first");
    const first = git(dir, "rev-parse", "HEAD");
    writeFileSync(path.join(dir, "b.txt"), "b\n");
    git(dir, "add", "-A");
    git(dir, "commit", "-qm", "second");
    return { dir, first, head: git(dir, "rev-parse", "HEAD") };
  }
  const commit = (dir: string) => bash(`${fn("checkout_commit")}\ncheckout_commit "$1"`, [dir]);

  // Catches: a checkout_commit that reads jj alone (an empty line for git), or names no parents.
  test("a clean git checkout prints its HEAD and HEAD's parents", () => {
    const { dir, first, head } = gitCheckout();
    expect(commit(dir)).toEqual({ code: 0, stdout: `${head} parents=${first}\n`, stderr: "" });
  });

  // Catches: a built-from that names a commit the label's files do not match.
  test("a git checkout with a change git status lists is marked dirty", () => {
    const { dir, first, head } = gitCheckout();
    writeFileSync(path.join(dir, "a.txt"), "changed\n");
    expect(commit(dir).stdout).toBe(`${head} parents=${first} dirty\n`);
  });

  // Catches: a checkout_commit that hides a failed read, which cmd_profile would record as an
  // empty commit line.
  test("a directory that is no checkout fails", () => {
    expect(commit(freshDir("no-checkout")).code).not.toBe(0);
  });
});

// cmd_live_read reads every path skill_reads prints. A skill can hold examples/ beside
// references/ (asd-ste100 holds examples/), and a read of SKILL.md and references/ alone left
// examples/ unread.
describe("skill_reads", () => {
  function checkout() {
    const co = freshDir("skills-checkout");
    const skill = path.join(co, "skills", "demo");
    for (const file of [
      "SKILL.md",
      "references/rules.md",
      "examples/before-after.md",
      "examples/data.json",
      "LICENSE",
    ]) {
      mkdirSync(path.dirname(path.join(skill, file)), { recursive: true });
      writeFileSync(path.join(skill, file), `${file}\n`);
    }
    return co;
  }
  const reads = (co: string, skill: string) =>
    bash(`${fn("fail")}\n${fn("skill_reads")}\nskill_reads "$1" "$2"`, [co, skill]);

  // Catches: a list that leaves examples/ out, or reads a file that is not markdown.
  test("SKILL.md first, then every other markdown file in byte order", () => {
    expect(reads(checkout(), "demo")).toEqual({
      code: 0,
      stdout: [
        "skill://demo",
        "skill://demo/examples/before-after.md",
        "skill://demo/references/rules.md",
        "",
      ].join("\n"),
      stderr: "",
    });
  });

  // Catches: a live read of a skill the checkout does not have, which reads nothing and passes.
  test("a skill the checkout lacks fails, naming it", () => {
    const co = checkout();
    const { code, stderr } = reads(co, "absent");
    expect(code).toBe(1);
    expect(stderr).toContain(`${co} has no skill absent`);
  });
});

// Oh My Pi walks a run's cwd's ancestors for a project `.omp`, past the run's own HOME. Under a
// work directory below the operator's HOME, a live read loaded the operator's plugins from
// $HOME/.omp in place of the label's packed ones, and nothing scored that as a failure.
describe("omp_ancestor", () => {
  const ancestor = (dir: string) => bash(`${fn("omp_ancestor")}\nomp_ancestor "$1"`, [dir]);

  // Catches: a check that misses a `.omp` above the work directory, the measured case.
  test("names the nearest ancestor that holds .omp", () => {
    const top = freshDir("omp-above");
    mkdirSync(path.join(top, ".omp"));
    const work = path.join(top, "a", "work");
    expect(ancestor(work)).toEqual({ code: 0, stdout: `${top}/.omp\n`, stderr: "" });
  });

  // Catches: a check that starts at the parent and so misses the work directory's own `.omp`.
  test("the work directory's own .omp counts", () => {
    const work = freshDir("omp-self");
    writeFileSync(path.join(work, ".omp"), "");
    expect(ancestor(work).stdout).toBe(`${work}/.omp\n`);
  });

  // Catches: a check that refuses every work directory.
  test("prints nothing and fails when no ancestor holds .omp", () => {
    expect(ancestor(path.join(freshDir("omp-none"), "work"))).toEqual({
      code: 1,
      stdout: "",
      stderr: "",
    });
  });

  // Catches: a rig.sh that defines the check and never runs it.
  test("rig.sh refuses such a work directory before it makes it, naming the .omp", () => {
    const top = freshDir("omp-rig");
    mkdirSync(path.join(top, ".omp"));
    const work = path.join(top, "work");
    const result = childSpawn(["bash", path.join(import.meta.dir, "rig.sh"), "score"], {
      SKILL_SCENARIOS_WORK: work,
    });
    expect(result.exitCode).toBe(1);
    expect(result.stderr.toString()).toContain(`${top}/.omp`);
    expect(existsSync(work)).toBe(false);
  });
});

// A spec-writing or spec-rewrite batch takes one run, numbered one above the highest run of its
// scenario and label, so a later batch never removes an earlier run (cmd_run starts from rm -rf).
describe("next_run", () => {
  const next = (work: string, scenario: string, label: string) =>
    bash(`${fn("next_run")}\nwork=$1\nnext_run "$2" "$3"`, [work, scenario, label]);

  // Catches: numbering that starts over at 1 and so overwrites run 1, or that counts directories
  // instead of reading their numbers.
  test("one above the highest number of the scenario and label", () => {
    const work = freshDir("numbering");
    for (const run of [
      "spec-writing-s1-1",
      "spec-writing-s1-10",
      "spec-writing-s1-x-4",
      "spec-writing-s1-2-7",
      "spec-rewrite-s1-30",
      "spec-writing-main-40",
    ])
      mkdirSync(path.join(work, "runs", run), { recursive: true });
    writeFileSync(path.join(work, "runs", "spec-writing-s1-99"), "");
    expect(next(work, "spec-writing", "s1")).toEqual({ code: 0, stdout: "11\n", stderr: "" });
  });

  // Catches: a first batch that fails because $work/runs does not exist yet.
  test("1 when the label has no run of the scenario", () => {
    expect(next(freshDir("numbering-empty"), "spec-rewrite", "s1").stdout).toBe("1\n");
  });
});

// A batch given SKILL_SCENARIOS_POSTGRES_URL makes each run's database on that server
// (own_dispatch), and names the server in its notes, its failures and each run's dispatch.json.
describe("safe_url", () => {
  const pgUrl = path.join(root, "packages/envoy/scripts/lib/pg-url.sh");
  // Catches: a note, a failure message or a run's dispatch.json that prints the password of a
  // server the operator gave, in a Postgres URL's user info as libpq reads it (to the first `@`,
  // a `?` in the password included), in its `password=` or `sslpassword=` parameter (its key
  // percent-decoded, as libpq reads `%70assword=`), or in a NATS
  // URL's user info as Go's net/url reads it (to the last `@`); a query left with a dangling `?`
  // or `&`; a safe_url that changes a URL with no password in it; and a URL pg-url.sh refuses (a
  // raw `@` in its hosts) printed as it is, where safe_url prints a placeholder.
  test("safe_url drops the password of a URL and keeps the rest", () => {
    expect(
      bash(`. ${JSON.stringify(pgUrl)}\n${fn("safe_url")}\nfor url; do safe_url "$url"; done`, [
        "postgres://u:p@h:1/d",
        "postgres://u@h:1/d",
        "nats://127.0.0.1:4222",
        "postgres://u:?@127.0.0.1:5433/legion_test",
        "postgres://u@localhost:5432@10.1.2.3/db",
        "postgres://u@127.0.0.1:5433/d?sslmode=disable&password=p&application_name=x",
        "postgresql://u@127.0.0.1:5433/d?sslpassword=p",
        "postgres://u:p@127.0.0.1:5433/d?password=p&sslmode=disable",
        "nats://u:p@ss@127.0.0.1:4222",
        "postgres://u@127.0.0.1:5433/d?%70assword=p&sslmode=disable",
      ])
    ).toEqual({
      code: 0,
      stdout:
        "postgres://u@h:1/d\npostgres://u@h:1/d\nnats://127.0.0.1:4222\n" +
        "postgres://u@127.0.0.1:5433/legion_test\n" +
        "<a Postgres URL pg-url.sh refuses>\n" +
        "postgres://u@127.0.0.1:5433/d?sslmode=disable&application_name=x\n" +
        "postgresql://u@127.0.0.1:5433/d\n" +
        "postgres://u@127.0.0.1:5433/d?sslmode=disable\n" +
        "nats://u@127.0.0.1:4222\n" +
        "postgres://u@127.0.0.1:5433/d?sslmode=disable\n",
      stderr: "-: refused a Postgres URL: a host holds a raw @ (the URL is not printed)\n",
    });
  });
});

// services_up reads a given SKILL_SCENARIOS_POSTGRES_URL before anything connects to that server
// or the batch writes the URL anywhere. python3 with libpq: its guards read the URL with this
// host's libpq (scripts/e2e/lib/loopback-dsn.py), which refuses every URL where it finds none.
const withoutPython = skipWithout("python3");
const withoutLibpq =
  !withoutPython &&
  Bun.spawnSync([
    "python3",
    "-c",
    "import ctypes.util, sys; sys.exit(ctypes.util.find_library('pq') is None)",
  ]).exitCode !== 0;
if (withoutLibpq) {
  console.warn(
    `Skipped the services_up rows in ${import.meta.path}. services_up reads a given Postgres ` +
      `URL with libpq, and ctypes.util.find_library("pq") finds none on this host; install libpq5.`
  );
}
describe.skipIf(withoutPython || withoutLibpq)("services_up", () => {
  const pgUrl = path.join(root, "packages/envoy/scripts/lib/pg-url.sh");
  const loopbackDsn = path.join(root, "scripts/e2e/lib/loopback-dsn.sh");
  /** services_up given URL and a NATS, in a services directory of its own, with a psql first on
   * PATH that appends to psql.log each time it runs. until_true runs its command once, so a URL
   * that passed every check would reach psql at once. Answers services_up's status and standard
   * error, whether psql ran, and what the services directory holds. */
  function up(url: string) {
    const dir = freshDir("services-up");
    const bin = path.join(dir, "bin");
    const log = path.join(dir, "psql.log");
    const services = path.join(dir, "services");
    mkdirSync(bin);
    mkdirSync(services);
    writeFileSync(path.join(bin, "psql"), `#!/bin/sh\necho ran >>${JSON.stringify(log)}\n`, {
      mode: 0o755,
    });
    const result = bash(
      `set -euo pipefail\n. "$1"\n. "$2"\n${fn("fail")}\n${fn("given_services")}\n` +
        `${fn("safe_url")}\n${fn("pg_sql")}\n${fn("services_up")}\n` +
        `until_true() { shift 2; "$@"; }\n` +
        `S=$3\nservices_env=$S/env\nservices_postgres=$S/postgres-url\nservices_up`,
      [pgUrl, loopbackDsn, services],
      {
        PATH: `${bin}:${callerPath}`,
        SKILL_SCENARIOS_POSTGRES_URL: url,
        SKILL_SCENARIOS_NATS_URL: "nats://127.0.0.1:4222",
      }
    );
    return {
      code: result.code,
      stderr: result.stderr,
      psqlRan: existsSync(log),
      written: readdirSync(services),
    };
  }
  const host = (name: string) =>
    `SKILL_SCENARIOS_POSTGRES_URL must name 127.0.0.1, localhost or ::1: libpq reads the host ${name}, and each run creates a database there`;
  const sslpassword =
    "SKILL_SCENARIOS_POSTGRES_URL holds an sslpassword parameter, a client key's passphrase, whose value this line does not print: pg_sql takes only the password out of the URL it gives psql, so psql's arguments, which every user of this machine can read, would hold the passphrase";

  // Catches: a services_up that connects to, or writes into its services file, a given URL that
  // its checks refuse, because a check is gone or runs after the write or the first psql: a host
  // that is not loopback, the `localhost?x@evil.example` shape whose host libpq reads as
  // evil.example (scripts/e2e/lib/loopback-dsn.sh), a key=value connection string, an
  // `sslpassword` parameter (its key percent-decoded too), a URL pg-url.sh refuses (a raw `@` in
  // its path, which pg-url.sh names on its own line first), and a value run_database_url refuses
  // to write into each run's connection string (a newline, which it refuses in a password the same
  // way). Each refusal is the one its own check prints, and holds no password.
  test("refuses a URL the runs cannot use before psql runs or the URL is written", () => {
    for (const [url, refusal, before = ""] of [
      ["postgres://u:p@db.example:5432/postgres", host("db.example")],
      ["postgres://localhost?x:p@evil.example:5432/postgres", host("evil.example")],
      [
        "host=127.0.0.1 port=5432 user=u password=p dbname=postgres",
        "SKILL_SCENARIOS_POSTGRES_URL is not a postgres:// or postgresql:// URL: the batch names the server in its notes and in each run's dispatch.json, and it can take a password out of a URL alone, never out of a key=value connection string",
      ],
      ["postgres://u:p@127.0.0.1:5432/postgres?sslmode=require&sslpassword=x", sslpassword],
      ["postgresql://u@localhost:5432/postgres?%73slpassword=x&sslmode=require", sslpassword],
      [
        "postgres://u@127.0.0.1:5432/post@gres",
        "SKILL_SCENARIOS_POSTGRES_URL is a URL pg-url.sh refuses, for the reason on the line above: pg_sql and safe_url take its password out with pg-url.sh, so the batch could neither reach that server nor name it",
        "-: refused a Postgres URL: an @ sits between the first / and the ? (the URL is not printed)\n",
      ],
      [
        "postgres://u@localhost:5432/postgres?application_name=x%0A",
        "SKILL_SCENARIOS_POSTGRES_URL gives application_name U+000A, a character that does not print: it reaches the rewritten connection string, where a caller writes that string into a file, and this refusal prints no value that holds one",
      ],
    ]) {
      expect({ url, ...up(url) }).toEqual({
        url,
        code: 1,
        stderr: `${before}skill-scenarios: ${refusal}\n`,
        psqlRan: false,
        written: [],
      });
    }
  });
});

// run_env_pids names each process whose environment carries a run under DIR, with its start time,
// for the exit handlers' stop_env_processes. It walks every process's environment, so a machine
// whose /proc/<pid>/environ entries together pass ARG_MAX must not hand them to grep as its
// arguments: past about 80,000 processes grep could not start, and the run had nothing to stop.
// perl: each row's processes and harness start through ownGroup (scripts/e2e/lib/own-group.ts).
const withoutPerl = skipWithout("perl");
describe.skipIf(withoutPerl)("run_env_pids and stop_env_processes", () => {
  const force = path.join(root, "scripts/e2e/lib/arg-max-force.sh");
  const processTree = path.join(root, "scripts/e2e/lib/process-tree.sh");
  const start = (pid: number) =>
    readFileSync(`/proc/${pid}/stat`, "utf8")
      .replace(/^.*\) /, "")
      .split(" ")[19];
  // Each row's run process and its harness shell run in process groups of their own (ownGroup,
  // scripts/e2e/lib/own-group.ts): stop_env_processes stops what it ends, and a stopped member of
  // the test runner's own group hangs the runner up where that group is orphaned, as in a Legion pod.
  // A row that sweeps waits until its run process runs sleep (untilRunning), and so leads its own
  // group, before it sweeps. The walk reads `proc_root`, a proc root of the row's run process alone
  // (scripts/e2e/lib/proc-root.ts), so a row's time is its own and not the host's process count.
  const runProcess = (dir: string) =>
    Bun.spawn(ownGroup(["sleep", "60"]), {
      env: { PATH: process.env.PATH ?? "", SKILL_SCENARIO_RUN: path.join(dir, "run-1") },
      stdout: "ignore",
      stderr: "ignore",
    });
  const groupBash = (script: string, args: string[], procRoot: string) => {
    const result = childSpawn(ownGroup(["bash", "-c", script, "-", ...args]), {
      proc_root: procRoot,
    });
    return {
      code: result.exitCode,
      stdout: result.stdout.toString(),
      stderr: result.stderr.toString(),
    };
  };

  // Catches: a walk that hands grep every /proc/<pid>/environ at once, which under the force
  // (scripts/e2e/lib/arg-max-force.sh) cannot start and names nothing. The force cannot hold under
  // gVisor (a Legion pod's kernel): there the row is skipped, by that name (argMaxForceHolds).
  test.skipIf(!argMaxForceHolds())(
    "names a process of the run, with its start time, where a program handed every /proc/<pid>/environ would not start",
    () => {
      const dir = freshDir("env-pids");
      const scratch = freshDir("env-pids-scratch");
      const run = runProcess(dir);
      try {
        const old = 'grep -lzsF -e "SKILL_SCENARIO_RUN=$2" "$proc_root"/[0-9]*/environ';
        const forced = groupBash(
          `set -euo pipefail\n. ${JSON.stringify(force)}\n. ${JSON.stringify(processTree)}\n${fn("run_env_pids")}\narg_max_force environ 6144 "$1" ${JSON.stringify(old)}\nrun_env_pids "$2"`,
          [scratch, dir],
          procRootOf(scratch, [run.pid])
        );
        expect(forced).toEqual({ code: 0, stdout: `${run.pid} ${start(run.pid)}\n`, stderr: "" });
      } finally {
        run.kill("SIGKILL");
      }
    }
  );

  // Catches: a stop_env_processes that does not end what its walk names. The loop that ends each
  // process, held to its start and its mark, is kill_held (scripts/e2e/lib/process-tree.sh), the
  // run's sweep's own; its rows, a pid that passed to another process and a parent that exits after
  // the freeze, are run-processes.test.ts's, and are not written a second time here.
  test("stops each process of the run its walk names, with the run's sweep's own loop", async () => {
    const dir = freshDir("env-stop");
    const run = runProcess(dir);
    await untilRunning(run.pid, "sleep", waits.end());
    try {
      const rig = path.join(root, "scripts/e2e/lib/rig.sh");
      const prelude = `set -euo pipefail\nroot=${JSON.stringify(root)} work=$1 evidence=$1 timeout_hook=\nnote() { :; }\nfail() { exit 1; }\n. ${JSON.stringify(rig)}\n${fn("stop_env_processes")}`;
      const own = groupBash(
        `${prelude}\n${fn("run_env_pids")}\nstop_env_processes "$1"; echo "status $?"`,
        [dir],
        procRootOf(dir, [run.pid])
      );
      expect(own).toEqual({ code: 0, stdout: "status 0\n", stderr: "" });
      expect(await run.exited).not.toBe(0);
      expect(run.signalCode).toBe("SIGKILL");
    } finally {
      run.kill("SIGKILL");
    }
  });
});

// cmd_batch and cmd_run read SKILL_SCENARIOS_SOURCE through source_dir before they start anything.
describe("source_dir", () => {
  function work(files: string[]) {
    const dir = freshDir("sources");
    mkdirSync(path.join(dir, "sources", "acme-1"), { recursive: true });
    for (const file of files) writeFileSync(path.join(dir, "sources", "acme-1", file), "x\n");
    return dir;
  }
  const sourceDir = (dir: string, scenario: string, name: string) =>
    bash(`${fn("fail")}\n${fn("source_dir")}\nwork=$1\nsource_dir "$2" "$3"`, [
      dir,
      scenario,
      name,
    ]);
  const full = ["title.txt", "messages.md", "asks.json"];

  // Catches: a spec-rewrite run seeded from a source with no old spec, which seed.ts would refuse
  // only after the batch started its services.
  test("spec-rewrite refuses a source without old-spec.md, naming it", () => {
    const dir = work(full);
    const { code, stderr } = sourceDir(dir, "spec-rewrite", "acme-1");
    expect(code).toBe(1);
    expect(stderr).toContain(`the source ${dir}/sources/acme-1 has no old-spec.md`);
  });

  // Catches: a spec-writing run refused for want of an old spec it never reads.
  test("spec-writing takes the same source, and prints its directory", () => {
    const dir = work(full);
    expect(sourceDir(dir, "spec-writing", "acme-1")).toEqual({
      code: 0,
      stdout: `${dir}/sources/acme-1\n`,
      stderr: "",
    });
    expect(sourceDir(work([...full, "old-spec.md"]), "spec-rewrite", "acme-1").code).toBe(0);
  });

  // Catches: a run that starts on a source missing a file seed.ts reads, or a name that leaves
  // $work/sources.
  test("refuses an unset name, a path, and a source missing a file", () => {
    const dir = work(["title.txt", "asks.json"]);
    expect(sourceDir(dir, "spec-writing", "").stderr).toContain("SKILL_SCENARIOS_SOURCE is unset");
    expect(sourceDir(dir, "spec-writing", "../sources/acme-1").stderr).toContain(
      "is not a directory name"
    );
    expect(sourceDir(dir, "spec-writing", "acme-1").stderr).toContain("has no messages.md");
  });
});

// The conversation of a spec run: rig.sh's spec_turns with stand-ins for omp (launch), seed.ts
// (seed_ts) and the judge's person stand-in (judge_ts). Each stand-in logs its call, and the
// seed.ts state stand-in hands back the state the test scripted for that turn.
describe("spec_turns", () => {
  interface Turn {
    asks?: number;
    approval?: boolean;
  }
  function conversation(
    turns: Turn[],
    options: { secondTranscript?: boolean; failAt?: number } = {}
  ) {
    const run = freshDir("spec-run");
    const states = freshDir("spec-states");
    mkdirSync(path.join(run, "sessions"));
    turns.forEach((turn, index) => {
      const t = index + 1;
      writeFileSync(
        path.join(states, `state-${t}.json`),
        JSON.stringify({
          turn: t,
          open_asks: Array.from({ length: turn.asks ?? 0 }, (_, k) => ({
            id: `ask-${t}-${k}`,
            question: "Q?",
            options: [],
            multiple: false,
          })),
          approval: turn.approval ? { ask_id: `approval-${t}`, version: t } : null,
        })
      );
    });
    const standIns = `
launch() {
  printf 'launch %s | %s\\n' "$*" "$(<"$R/prompt.txt")" >>"$R/calls"
  [ -n "\${SECOND:-}" ] && [ $# -gt 1 ] && : >"$R/sessions/fresh.jsonl"
  printf x >>"$R/launches"
  status=0
  [ "$(wc -c <"$R/launches")" != "\${FAIL_AT:-0}" ] || status=1
  printf 'output of %s\\nexit=%s\\n' "$*" "$status" >"$R/out.txt"
  printf '{}\\n' >>"$R/sessions/run.jsonl"
}
seed_ts() {
  printf 'seed %s\\n' "$*" >>"$R/calls"
  [ "$1" != state ] || cp "$STATES/state-$3.json" "$R/state-$3.json"
}
judge_ts() {
  printf 'judge %s\\n' "$*" >>"$R/calls"
  jq -c '.open_asks[] | {ask: .id, selected: [], text: "t"}' "$R/state-$3.json" >"$R/answers-$3.jsonl"
}`;
    const result = bash(
      `${fn("note")}\n${fn("fail")}\n${fn("spec_turns")}\n${standIns}\nR=$1\nspec_turns spec-writing-s1-1 LWEVAL-2 "Design it."`,
      [run],
      {
        STATES: states,
        ...(options.secondTranscript ? { SECOND: "1" } : {}),
        ...(options.failAt === undefined ? {} : { FAIL_AT: String(options.failAt) }),
      }
    );
    const read = (name: string) =>
      existsSync(path.join(run, name)) ? readFileSync(path.join(run, name), "utf8") : "";
    const calls = read("calls")
      .trimEnd()
      .split("\n")
      .map((line) => line.replaceAll(run, "<R>"));
    return { ...result, run, read, calls };
  }
  const answered = "The person answered your asks on LWEVAL-2.";
  const approved = "The person approved the spec of LWEVAL-2.";

  // Catches: a loop that answers before it reads the state, continues without --continue (a second
  // session), skips the stand-in, or sends another prompt than the three the rig.sh header gives.
  test("asks, then an approval: answer, continue, approve, continue once more, stop", () => {
    const { code, calls, read } = conversation([{ asks: 2 }, { approval: true }, {}]);
    expect(code).toBe(0);
    expect(calls).toEqual([
      "launch spec-writing-s1-1 | Design it.",
      "seed state <R>/fixture.json 1 <R>",
      "judge answer <R> 1",
      "seed answer <R>/fixture.json <R>/answers-1.jsonl",
      `launch spec-writing-s1-1 --continue | ${answered}`,
      "seed state <R>/fixture.json 2 <R>",
      "seed approve <R>/fixture.json",
      `launch spec-writing-s1-1 --continue | ${approved}`,
      "seed state <R>/fixture.json 3 <R>",
    ]);
    expect(read("turns.txt")).toBe("1 first exit=0\n2 answered exit=0\n3 approved exit=0\n");
    expect(read("answers.jsonl")).toBe(read("answers-1.jsonl"));
    expect(read("out-2.txt")).toBe("output of spec-writing-s1-1 --continue\nexit=0\n");
  });

  // Catches: a loop that answers the open asks when an approval request is open too (the rig.sh
  // header's order puts the approval first).
  test("an open approval request goes before open asks", () => {
    const { calls } = conversation([{ asks: 1, approval: true }, {}]);
    expect(calls.slice(1, 3)).toEqual([
      "seed state <R>/fixture.json 1 <R>",
      "seed approve <R>/fixture.json",
    ]);
    expect(calls.some((call) => call.startsWith("judge"))).toBe(false);
  });

  // Catches: a loop that continues a session with nothing for it to answer.
  test("no open ask and no approval request ends the run after its turn", () => {
    const { code, calls, read } = conversation([{}]);
    expect(code).toBe(0);
    expect(calls).toEqual([
      "launch spec-writing-s1-1 | Design it.",
      "seed state <R>/fixture.json 1 <R>",
    ]);
    expect(read("turns.txt")).toBe("1 first exit=0\n");
  });

  // Catches: a run past 8 turns, or an answer after the eighth turn that no turn reads.
  test("stops after 8 turns, answering nothing after the eighth", () => {
    const { calls, read } = conversation(Array.from({ length: 9 }, () => ({ asks: 1 })));
    expect(calls.filter((call) => call.startsWith("launch"))).toHaveLength(8);
    expect(calls.filter((call) => call.startsWith("judge"))).toHaveLength(7);
    expect(calls.at(-1)).toBe("seed state <R>/fixture.json 8 <R>");
    expect(read("turns.txt").trimEnd().split("\n")).toHaveLength(8);
  });

  // Catches: a --continue that starts a second transcript beside the first, which every reader of
  // the run would then read as one of two.
  test("a later turn that leaves a second transcript fails the run, naming its sessions", () => {
    const { code, stderr, run } = conversation([{ asks: 1 }, {}], { secondTranscript: true });
    expect(code).toBe(1);
    expect(stderr).toContain(`after turn 2, ${run}/sessions holds 2 transcripts`);
  });

  // Catches: a loop that answers and continues a session whose turn failed or timed out.
  test("a turn that does not exit 0 ends the run after its state", () => {
    const { code, calls, read } = conversation([{ asks: 1 }, { asks: 1 }, {}], { failAt: 2 });
    expect(code).toBe(0);
    expect(calls.at(-1)).toBe("seed state <R>/fixture.json 2 <R>");
    expect(calls.filter((call) => call.startsWith("judge"))).toHaveLength(1);
    expect(read("turns.txt")).toBe("1 first exit=0\n2 answered exit=1\n");
  });
});

// cmd_profile's superpowers step, with a stand-in mise: it records the environment omp got and
// writes a transcript whose one tool result is the text in result.txt beside it.
describe("install_superpowers", () => {
  function setup(result: string) {
    const bin = freshDir("superpowers-bin");
    const label = freshDir("superpowers-label");
    const src = freshDir("superpowers-src");
    for (const skill of ["writing-plans", "brainstorming"]) {
      mkdirSync(path.join(src, "skills", skill), { recursive: true });
      writeFileSync(
        path.join(src, "skills", skill, "SKILL.md"),
        `---\nname: ${skill}\n---\n\n# Skill ${skill}\n`
      );
    }
    writeFileSync(path.join(src, "package.json"), '{"version":"9.9.9"}\n');
    writeFileSync(path.join(label, "built-from"), "checkout /x\n");
    // The transcript line the stand-in omp leaves, made here: the stand-in runs under the probe's
    // own `env -i`, where a jq on PATH may be a mise shim that cannot run.
    writeFileSync(
      path.join(bin, "probe.jsonl"),
      `${JSON.stringify({ message: { role: "toolResult", content: [{ type: "text", text: result }] } })}\n`
    );
    // `mise where` names the pin's install, whose bin/omp the probe runs by path; any other call is
    // the probe's `mise x <pin> -- <omp> -p …`.
    mkdirSync(path.join(bin, "pin", "bin"), { recursive: true });
    writeFileSync(path.join(bin, "pin", "bin", "omp"), "#!/bin/sh\n", { mode: 0o755 });
    writeFileSync(
      path.join(bin, "mise"),
      `#!/usr/bin/env bash
if [ "$1" = where ]; then echo "$(dirname "$0")/pin"; exit 0; fi
printf '%s\\n' "$@" >"$(dirname "$0")/argv.txt"
env >"$(dirname "$0")/env.txt"
while [ $# -gt 0 ] && [ "$1" != --session-dir ]; do shift; done
mkdir -p "$2"
cp "$(dirname "$0")/probe.jsonl" "$2/probe.jsonl"
echo done
`,
      { mode: 0o755 }
    );
    const run = () =>
      bash(
        `${fn("fail")}\n${fn("pinned_omp")}\n${fn("rig_omp")}\n${fn("install_superpowers")}\nsuperpowers_skills=(writing-plans brainstorming)\ninstall_superpowers "$1" skills-s1 omp@pin`,
        [label],
        { PATH: `${bin}:${callerPath}`, SKILL_SCENARIOS_SUPERPOWERS: src, USER: "rig" }
      );
    return { bin, label, run };
  }

  // Catches: a profile step that copies the skills somewhere the pinned Oh My Pi does not read, or
  // gives the probe the operator's Envoy (the label's pi-envoy would register with it).
  test("copies both skills into the profile, probes with no Envoy, and marks the label", () => {
    const { bin, label, run } = setup("# Skill writing-plans\n\nbody");
    expect(run()).toMatchObject({ code: 0, stderr: "" });
    const skills = path.join(label, "home/.omp/profiles/skills-s1/agent/skills");
    expect(readFileSync(path.join(skills, "brainstorming/SKILL.md"), "utf8")).toContain(
      "# Skill brainstorming"
    );
    expect(existsSync(path.join(skills, "writing-plans/SKILL.md"))).toBe(true);
    expect(existsSync(path.join(label, "superpowers"))).toBe(true);
    // Each variable is read whole, by name: a substring would also match a longer name such as
    // X_OMP_PROFILE, and env can print any variable first.
    const env = new Map(
      readFileSync(path.join(bin, "env.txt"), "utf8")
        .split("\n")
        .flatMap((line): [string, string][] => {
          const eq = line.indexOf("=");
          return eq > 0 ? [[line.slice(0, eq), line.slice(eq + 1)]] : [];
        })
    );
    expect(env.get("ENVOY_URL")).toBe("http://127.0.0.1:1");
    expect(env.get("OMP_PROFILE")).toBe("skills-s1");
    expect(env.get("HOME")).toBe(`${label}/home`);
    expect(readFileSync(path.join(label, "built-from"), "utf8")).toContain(
      "superpowers-skill writing-plans "
    );
    // The probe names the pin's own binary, so no omp earlier on PATH stands in for it.
    expect(readFileSync(path.join(bin, "argv.txt"), "utf8")).toStartWith(
      `x\nomp@pin\n--\n${bin}/pin/bin/omp\n-p\n`
    );
  });

  // Catches: a probe that passes when Oh My Pi did not load the skill.
  test("fails when the read answers Unknown skill, and marks nothing", () => {
    const { label, run } = setup("Unknown skill: writing-plans\nAvailable: dispatch");
    const { code, stderr } = run();
    expect(code).toBe(1);
    expect(stderr).toContain("does not load skill://writing-plans (Unknown skill)");
    expect(existsSync(path.join(label, "superpowers"))).toBe(false);
  });

  // Catches: a probe that passes when the agent read nothing at all.
  test("fails when no tool result holds the skill's heading", () => {
    const { label, run } = setup("some other file");
    const { code, stderr } = run();
    expect(code).toBe(1);
    expect(stderr).toContain("read no skill://writing-plans");
    expect(existsSync(path.join(label, "superpowers"))).toBe(false);
  });
});

// rig.sh judge-omp, the whole script, in its Docker container (Docker is on the devbox and on CI's
// ubuntu-24.04 runners), with the real key-file route and a stand-in omp that records its argv and
// environment. SKILL_SCENARIOS_JUDGE_OMP puts the stand-in where the pinned omp's binary is
// mounted, so no test needs a model. The checkout's own skills/asd-ste100 is the ste skill.
describe("judge-omp", () => {
  // Each row starts one or two judge containers, and a container's start on a loaded host has
  // taken longer than the file's 60 s bound, so these rows get a bound of their own.
  const judgeBound = 180_000;
  // Every row that reaches the container needs Docker, so a machine without it (a Legion
  // worker's pod) skips those rows and the rest of the file still runs. GitHub Actions is the
  // exception, as in extensions/controller-start-omp.test.ts: its ubuntu-24.04 runners have
  // Docker, and a runner that lost it must fail these rows rather than quietly skip them.
  // The bad-argument row is not skipped: judge-omp refuses its arguments before it looks for
  // Docker, so it runs anywhere.
  const onActions = process.env.GITHUB_ACTIONS === "true";
  const skip = withoutDocker && !onActions;
  // Staged in a hook: Bun runs this body even where the rows do not run, and a write here would
  // make the suite's work directory and leave it behind.
  let work = "";
  let outside = "";
  let omp = "";
  beforeAll(() => {
    work = freshDir("judge-work");
    outside = path.join(work, "outside.txt");
    writeFileSync(outside, "a file beside the judge's directory\n");
    mkdirSync(path.join(work, "runs"), { recursive: true });
    omp = path.join(freshDir("judge-omp-bin"), "omp");
    // The stand-in writes everything through the one file the judge may write, answer.json: argv
    // (one word a line), then `env` lines, then one line per probe (`<name> allowed|denied`), then
    // one `mode <name> <bits>` line per key path, which only a judge holding the key can read.
    writeFileSync(
      omp,
      `#!/bin/bash
probe() { if "\${@:2}" >/dev/null 2>&1; then echo "probe $1 allowed"; else echo "probe $1 denied"; fi; }
# DROP: write a marker into every directory this judge can write outside its own directory.
# SEEK: report any marker another judge left anywhere this judge can read.
if [ -n "\${DROP:-}" ]; then
  find / \\( -path /proc -o -path /sys -o -path /dev -o -path "$PWD" \\) -prune -o -type d -writable -print 2>/dev/null |
    while IFS= read -r d; do printf 'private input\\n' >"$d/leak-$DROP" 2>/dev/null; done
fi
{
  printf 'argv %s\\n' "$0" "$@"
  printf 'env %s\\n' "PWD=$PWD" "HOME=$HOME" "OMP_PROFILE=$OMP_PROFILE"
  compgen -e | grep -q '^LEGION_E2E' && echo "env LEGION_E2E set"
  probe read-outside cat "$OUTSIDE"
  probe read-home-ssh ls "$OPERATOR_HOME/.ssh"
  probe list-work ls "$WORK/runs"
  probe read-own cat "$PWD/prompt.txt"
  probe write-own-dir touch "$PWD/new.txt"
  probe write-sessions touch "$PWD/sessions/s.jsonl"
  probe read-skill cat "$HOME/.omp/profiles/$OMP_PROFILE/agent/skills/asd-ste100/SKILL.md"
  probe read-models cat "$HOME/.omp/profiles/$OMP_PROFILE/agent/models.yml"
  key=$(sed -n 's/.*!exec \\(.*\\)"$/\\1/p' "$HOME/.omp/profiles/$OMP_PROFILE/agent/models.yml" | head -1)
  probe key-command test -x "$key"
  probe gateway-base-url grep -q 'baseUrl: https://gateway.internal.example/anthropic' "$HOME/.omp/profiles/$OMP_PROFILE/agent/models.yml"
  probe token-is-jwt bash -c '[[ "$("$1")" =~ ^eyJ[A-Za-z0-9_-]*\\.[A-Za-z0-9_-]+\\.[A-Za-z0-9_-]+$ ]]' - "$key"
  probe dbus-set test -n "\${DBUS_SESSION_BUS_ADDRESS:-}"
  printf 'mode %s\\n' "key-dir $(stat -c %a /opt/key 2>/dev/null || echo none)" \\
    "token $(stat -c %a /opt/key/token 2>/dev/null || echo none)"
  [ -z "\${SEEK:-}" ] || echo "seek $(find / \\( -path /proc -o -path /sys -o -path /dev \\) -prune -o -name 'leak-*' -print 2>/dev/null | head -1)"
} >"$PWD/answer.json"
echo judged
exit "$(cat "$PWD/exit-status" 2>/dev/null || echo 0)"
`,
      { mode: 0o755 }
    );
  });
  const judge = (
    kind: string,
    dir: string,
    output = "answer.json",
    env: Record<string, string> = {}
  ) =>
    childSpawn(["bash", path.join(import.meta.dir, "rig.sh"), "judge-omp", kind, dir, output], {
      USER: "rig",
      SKILL_SCENARIOS_WORK: work,
      SKILL_SCENARIOS_JUDGE_OMP: omp,
      SKILL_SCENARIOS_JUDGE_ENV: `OUTSIDE=${outside} OPERATOR_HOME=${operatorHome} WORK=${work}`,
      LEGION_E2E_MODEL_ROUTE: "key-file",
      LEGION_E2E_ANTHROPIC_KEY_FILE: keyFile(0o600),
      ...env,
    });
  let jobs = 0;
  function judgeDir(): string {
    const dir = path.join(work, "judging", `job-${++jobs}`);
    mkdirSync(dir, { recursive: true });
    writeFileSync(path.join(dir, "prompt.txt"), "Write answer.json.\n");
    return dir;
  }
  /** The stand-in's record of one kind (`argv`, `env`, `probe`), from the judge's answer file. */
  const recorded = (dir: string, kind: string) =>
    readFileSync(path.join(dir, "answer.json"), "utf8")
      .split("\n")
      .filter((line) => line.startsWith(`${kind} `))
      .map((line) => line.slice(kind.length + 1));
  /**
   * A judge's exit code, and whether the rig refused: any line of its stderr starting
   * `skill-scenarios: `, the prefix note() and fail() give every word of the rig's own. Equality
   * against the whole stderr string read a byte from docker, getent, mise or flock as a failed
   * row; what a row is about is that the judge ran and the rig refused nothing. A result that is
   * not clean carries the stderr that was read, so a failing row still names what went wrong; a
   * clean result reports no stderr.
   */
  const judged = (result: { exitCode: number; stderr: Buffer }) => {
    const stderr = result.stderr.toString();
    const refused = stderr.split("\n").some((line) => line.startsWith("skill-scenarios: "));
    const clean = !refused && result.exitCode === 0;
    return { code: result.exitCode, refused, stderr: clean ? "" : stderr };
  };
  /** What `judged` reads back from a judge that ran and a rig that refused nothing. */
  const judgedCleanly = { code: 0, refused: false, stderr: "" };

  // Catches: a judge session with the label's plugins, the operator's skills or the default model,
  // run outside its own directory, or with no record of its model.
  test.skipIf(skip)(
    "ste runs omp in the directory on its prompt, no plugins, asd-ste100 alone, the pinned model",
    () => {
      const dir = judgeDir();
      const result = judge("ste", dir);
      expect(judged(result)).toEqual(judgedCleanly);
      expect(recorded(dir, "argv")).toEqual([
        "/opt/omp/omp",
        "-p",
        "Write answer.json.",
        "--no-extensions",
        "--skills",
        "asd-ste100",
        "--model",
        "anthropic/claude-opus-4-8",
        "--thinking",
        "high",
        "--session-dir",
        `${dir}/sessions`,
      ]);
      expect(recorded(dir, "env")).toEqual([
        `PWD=${dir}`,
        "HOME=/home/judge",
        "OMP_PROFILE=judge-ste",
      ]);
      expect(readFileSync(path.join(dir, "out.txt"), "utf8")).toBe("judged\nexit=0\n");
      expect(readFileSync(path.join(dir, "judge-model.txt"), "utf8")).toBe(
        "model anthropic/claude-opus-4-8\nthinking high\n"
      );
      const skill = path.join(work, "judge-profiles/ste/home/.omp/profiles/judge-ste/agent/skills");
      expect(readFileSync(path.join(skill, "asd-ste100/SKILL.md"), "utf8")).toBe(
        readFileSync(path.join(root, "skills/asd-ste100/SKILL.md"), "utf8")
      );
      expect(
        readFileSync(
          path.join(work, "judge-profiles/ste/home/.omp/profiles/judge-ste/agent/models.yml"),
          "utf8"
        )
      ).toContain("https://api.anthropic.com");
    },
    judgeBound
  );

  // Catches: a judge that reads the run's transcript, other runs or the operator's files beside its
  // own directory (as a findings judge did, given no check), or that writes anything but its one
  // answer file and its sessions.
  test.skipIf(skip)(
    "a judge sees only its own directory, read-only, with its answer file and sessions writable",
    () => {
      const dir = judgeDir();
      expect(judge("ste", dir).exitCode).toBe(0);
      expect(Object.fromEntries(recorded(dir, "probe").map((line) => line.split(" ")))).toEqual({
        "read-outside": "denied",
        "read-home-ssh": "denied",
        "list-work": "denied",
        "read-own": "allowed",
        "write-own-dir": "denied",
        "write-sessions": "allowed",
        "read-skill": "allowed",
        "read-models": "allowed",
        "key-command": "allowed",
        "gateway-base-url": "denied",
        "token-is-jwt": "denied",
        "dbus-set": "denied",
      });
      expect(existsSync(path.join(dir, "new.txt"))).toBe(false);
    },
    judgeBound
  );

  // Catches: a bare judge that still loads skills, or a judge-omp that hides omp's failure.
  test.skipIf(skip)(
    "bare loads no skill, and judge-omp exits with omp's status",
    () => {
      const dir = judgeDir();
      writeFileSync(path.join(dir, "exit-status"), "3\n");
      const result = judge("bare", dir);
      expect(result.exitCode).toBe(3);
      expect(recorded(dir, "argv")).toContain("--no-skills");
      expect(recorded(dir, "argv")).not.toContain("--skills");
      expect(recorded(dir, "env")).toContain("OMP_PROFILE=judge-bare");
      expect(recorded(dir, "probe")).toContain("read-skill denied");
      expect(readFileSync(path.join(dir, "out.txt"), "utf8")).toBe("judged\nexit=3\n");
      expect(
        existsSync(
          path.join(work, "judge-profiles/bare/home/.omp/profiles/judge-bare/agent/skills")
        )
      ).toBe(false);
    },
    judgeBound
  );

  // Catches: a mount two judges share that one can write and the next can read, such as the key
  // command's call record, a path one judge could leave its private input in for another to read.
  // The first judge writes a marker into every directory it can write outside its own directory;
  // the second, of the same kind, looks for one anywhere it can read.
  test.skipIf(skip)(
    "one judge cannot leave a file that a second judge can read",
    () => {
      const first = judgeDir();
      const env = (word: string) => ({
        SKILL_SCENARIOS_JUDGE_ENV: `OUTSIDE=${outside} OPERATOR_HOME=${operatorHome} WORK=${work} ${word}`,
      });
      expect(judge("ste", first, "answer.json", env("DROP=one")).exitCode).toBe(0);
      const second = judgeDir();
      expect(judge("ste", second, "answer.json", env("SEEK=1")).exitCode).toBe(0);
      expect(recorded(second, "seek")).toEqual([""]);
    },
    judgeBound
  );

  // Catches: a judge kind the rig has no profile for, a directory with no prompt, and an answer
  // file name that escapes the judge's directory or names another of its files.
  test(
    "refuses another kind, a directory with no prompt.txt and a bad answer name",
    () => {
      expect(judge("plugins", judgeDir()).stderr.toString()).toContain("is ste or bare");
      expect(judge("ste", freshDir("judging-empty")).stderr.toString()).toContain(
        "has no prompt.txt"
      );
      for (const output of ["../answer.json", "sessions", "prompt.txt", "a/b.json", ""]) {
        const result = judge("ste", judgeDir(), output);
        expect(result.exitCode).not.toBe(0);
        expect(result.stderr.toString()).toContain("is not the name of an answer file");
      }
    },
    judgeBound
  );

  // The gateway route with the real installer and key command, and a stand-in hawk-token first on
  // PATH that mints a JWT and counts each mint. The key command needs the operator's home and
  // session bus, so it runs on the host; the container gets only the token it printed.
  describe("on the gateway route", () => {
    // Staged in a hook, for the reason the describe above gives.
    let gatewayWork = "";
    let fakeBin = "";
    let mints = "";
    beforeAll(() => {
      gatewayWork = freshDir("judge-gateway-work");
      mkdirSync(path.join(gatewayWork, "runs"), { recursive: true });
      fakeBin = freshDir("judge-gateway-bin");
      mints = path.join(gatewayWork, "mints.log");
      writeFileSync(
        path.join(fakeBin, "hawk-token"),
        `#!/usr/bin/env bash
printf '%s\\n' "$$" >>"$FAKE_MINT_LOG"
payload=$(printf '{"exp":%s}' "$(($(date +%s) + 3600))" | base64 -w0 | tr '+/' '-_' | tr -d '=')
printf 'eyJhbGciOiJub25lIn0.%s.c2ln\\n' "$payload"
`,
        { mode: 0o755 }
      );
    });
    const gatewayJudge = (kind: string, dir: string, env: Record<string, string> = {}) =>
      judge(kind, dir, "answer.json", {
        PATH: `${fakeBin}:${callerPath}`,
        SKILL_SCENARIOS_WORK: gatewayWork,
        SKILL_SCENARIOS_JUDGE_ENV: `OUTSIDE=${outside} OPERATOR_HOME=${operatorHome} WORK=${gatewayWork}`,
        LEGION_E2E_MODEL_ROUTE: "gateway",
        LEGION_E2E_MODEL_GATEWAY_URL: "https://gateway.internal.example/anthropic",
        DBUS_SESSION_BUS_ADDRESS: "unix:path=/nonexistent/bus",
        FAKE_MINT_LOG: mints,
        ...env,
      });
    const mintCount = () =>
      existsSync(mints) ? readFileSync(mints, "utf8").split("\n").filter(Boolean).length : 0;
    function gatewayDir(): string {
      const dir = path.join(gatewayWork, "judging", `job-${++jobs}`);
      mkdirSync(dir, { recursive: true });
      writeFileSync(path.join(dir, "prompt.txt"), "Write answer.json.\n");
      return dir;
    }

    // Catches: a gateway judge that cannot start in its container, one that gets the operator's
    // home or session bus, one whose models.yml still names the host key command (which the
    // container cannot run), one that mints more than once per judge run, a token the judge can
    // read only by a path outside its mounts, and a token left on disk once the judge ends.
    test.skipIf(skip)(
      "a gateway judge starts in the container on a token the host minted once, and the token file is gone after",
      () => {
        // The first judge-omp call builds both profiles, and each install's preflight mints once.
        // Count only a later call's mints: one judge run is one call of the key command.
        expect(gatewayJudge("bare", gatewayDir()).exitCode).toBe(0);
        const dir = gatewayDir();
        const before = mintCount();
        const result = gatewayJudge("ste", dir);
        expect(judged(result)).toEqual(judgedCleanly);
        expect(mintCount() - before).toBeLessThanOrEqual(1);
        expect(recorded(dir, "env")).toEqual([
          `PWD=${dir}`,
          "HOME=/home/judge",
          "OMP_PROFILE=judge-ste",
        ]);
        expect(Object.fromEntries(recorded(dir, "probe").map((line) => line.split(" ")))).toEqual({
          "read-outside": "denied",
          "read-home-ssh": "denied",
          "list-work": "denied",
          "read-own": "allowed",
          "write-own-dir": "denied",
          "write-sessions": "allowed",
          "read-skill": "allowed",
          "read-models": "allowed",
          "key-command": "allowed",
          "gateway-base-url": "allowed",
          "token-is-jwt": "allowed",
          "dbus-set": "denied",
        });
        expect(existsSync(`${dir}.key`)).toBe(false);
        expect(readFileSync(path.join(dir, "answer.json"), "utf8")).not.toMatch(/eyJ[\w-]*\./);
      },
      judgeBound
    );

    // Catches: a token directory or file that others can read while the judge runs. The modes
    // are the judge's own reading of them from inside the container, recorded while it ran
    // (`recorded(dir, "mode")`), rather than a watcher's from outside: the stand-in omp sleeps
    // for nothing now, so there is no window for a watcher to look through, and a row that
    // waited for one would be waiting on a sleep.
    test.skipIf(skip)(
      "the token file is the owner's alone while the judge runs",
      () => {
        const dir = gatewayDir();
        const result = gatewayJudge("bare", dir);
        expect(judged(result)).toEqual(judgedCleanly);
        expect(recorded(dir, "mode")).toEqual(["key-dir 700", "token 600"]);
        expect(existsSync(`${dir}.key`)).toBe(false);
      },
      judgeBound
    );

    // Catches: a token file left behind when the judge fails, and a failed mint that starts the
    // judge anyway.
    test.skipIf(skip)(
      "removes the token file when the judge fails, and starts no judge when the key command fails",
      () => {
        const dir = gatewayDir();
        writeFileSync(path.join(dir, "exit-status"), "3\n");
        expect(gatewayJudge("bare", dir).exitCode).toBe(3);
        expect(existsSync(`${dir}.key`)).toBe(false);
        const refused = gatewayDir();
        const broken = freshDir("judge-gateway-broken");
        writeFileSync(path.join(broken, "hawk-token"), "#!/usr/bin/env bash\nexit 1\n", {
          mode: 0o755,
        });
        rmSync(path.join(gatewayWork, "judge-profiles/cache"), { recursive: true, force: true });
        for (const kind of ["ste", "bare"])
          rmSync(path.join(gatewayWork, `judge-profiles/${kind}/cache`), {
            recursive: true,
            force: true,
          });
        const result = gatewayJudge("ste", refused, { PATH: `${broken}:${callerPath}` });
        expect(result.exitCode).toBe(1);
        expect(result.stderr.toString()).toContain(
          "judge-omp: the gateway's key command gave no key"
        );
        expect(existsSync(path.join(refused, "answer.json"))).toBe(false);
      },
      judgeBound
    );
  });

  // Catches: a judge profile whose built-from records an empty commit line when the rig's root is
  // no checkout, so a judge's model run cannot be traced to the code that built it.
  test.skipIf(skip)(
    "fails, naming the rig's root, when the root is no checkout",
    () => {
      const top = freshDir("judge-root");
      const scripts = path.join(top, "packages/pi-legion/scripts/skill-scenarios");
      mkdirSync(scripts, { recursive: true });
      writeFileSync(
        path.join(scripts, "rig.sh"),
        readFileSync(path.join(import.meta.dir, "rig.sh"))
      );
      for (const file of [
        ".omp-pin",
        "skills/asd-ste100/SKILL.md",
        "packages/envoy/scripts/lib/pg-url.sh",
      ]) {
        mkdirSync(path.dirname(path.join(top, file)), { recursive: true });
        writeFileSync(path.join(top, file), readFileSync(path.join(root, file)));
      }
      mkdirSync(path.join(top, "scripts/e2e"), { recursive: true });
      cpSync(path.join(root, "scripts/e2e/lib"), path.join(top, "scripts/e2e/lib"), {
        recursive: true,
      });
      const rootWork = freshDir("judge-root-work");
      const result = childSpawn(
        ["bash", path.join(scripts, "rig.sh"), "judge-omp", "ste", judgeDir(), "answer.json"],
        {
          USER: "rig",
          SKILL_SCENARIOS_WORK: rootWork,
          SKILL_SCENARIOS_JUDGE_OMP: omp,
          LEGION_E2E_MODEL_ROUTE: "key-file",
          LEGION_E2E_ANTHROPIC_KEY_FILE: keyFile(0o600),
          GIT_CEILING_DIRECTORIES: path.dirname(top),
        }
      );
      expect(result.exitCode).toBe(1);
      expect(result.stderr.toString()).toContain(`cannot read the commit of ${top} with jj or git`);
      expect(existsSync(path.join(rootWork, "judge-profiles/ready"))).toBe(false);
    },
    judgeBound
  );
});

// A spec run's plan row reads skill://writing-plans, which only a label built with
// SKILL_SCENARIOS_SUPERPOWERS holds (cmd_profile marks it with a `superpowers` file).
describe("require_superpowers", () => {
  const check = (label: string) =>
    bash(`${fn("fail")}\n${fn("require_superpowers")}\nP=$1\nrequire_superpowers spec-writing`, [
      label,
    ]);

  // Catches: a spec run on a label with no writing-plans, whose plan row would then fail for the
  // rig's fault and not the skill's.
  test("refuses a label built without SKILL_SCENARIOS_SUPERPOWERS, naming the variable", () => {
    const label = freshDir("label");
    const { code, stderr } = check(label);
    expect(code).toBe(1);
    expect(stderr).toContain("SKILL_SCENARIOS_SUPERPOWERS");
    writeFileSync(path.join(label, "superpowers"), "");
    expect(check(label).code).toBe(0);
  });
});

// launch with a stand-in tmux that starts the pane's command a moment later, as a real tmux server
// does, and a stand-in mise that prints the omp command line it was given.
describe("launch", () => {
  // Catches: a later turn that reads the previous turn's `exit=` line as its own and returns at
  // once, and extra omp arguments placed anywhere but after the prompt.
  test("a later turn passes its arguments to omp and waits for its own exit line", () => {
    const run = freshDir("launch-run");
    const bin = freshDir("launch-bin");
    for (const dir of ["cwd", "sessions"]) mkdirSync(path.join(run, dir));
    writeFileSync(path.join(run, "prompt.txt"), "The person answered your asks on LWEVAL-2.\n");
    writeFileSync(path.join(run, "pane.env"), `PATH=${bin}:${process.env.PATH ?? ""}\n`);
    writeFileSync(path.join(run, "out.txt"), "the first turn\nexit=0\n");
    mkdirSync(path.join(bin, "pin", "bin"), { recursive: true });
    writeFileSync(path.join(bin, "pin", "bin", "omp"), "#!/bin/sh\n", { mode: 0o755 });
    writeFileSync(
      path.join(bin, "mise"),
      `#!/bin/sh\nif [ "$1" = where ]; then echo "${bin}/pin"; exit 0; fi\nshift 3\necho "$@"\n`,
      { mode: 0o755 }
    );
    const standIns = `
tmux() { case $1 in new-session) (command sleep 0.3 && bash -c "\${@: -1}") & ;; esac; }
sleep() { command sleep 0.05; }`;
    const result = bash(
      `${fn("fail")}\n${fn("pinned_omp")}\n${fn("rig_omp")}\n${fn("launch")}\n${standIns}\nR=$1 tag=t pin=omp@pin\nlaunch spec-writing-s1-1 --continue\ncp "$R/out.txt" "$R/seen"`,
      [run],
      { PATH: `${bin}:${callerPath}` }
    );
    expect(result.code).toBe(0);
    expect(readFileSync(path.join(run, "seen"), "utf8")).toBe(
      `${bin}/pin/bin/omp -p The person answered your asks on LWEVAL-2. --continue --session-dir ${run}/sessions\nexit=0\n`
    );
  });
});

// The tester-proof world's gh routes (worker_routes), answered by the run's gh stand-in. A tester
// finds its pull request with the worker prompt's lookup, `gh pr list --repo <the issue's
// repository> --head legion/<issue> --state open --json number --jq '.[0].number'`
// (packages/daemon/internal/prompts/go/worker-common.md).
describe("worker_routes", () => {
  const pr = {
    number: 1001,
    url: "https://github.com/example/widgets/pull/1001",
    headRefName: "legion/LWEVAL-1",
    headRefOid: "099b05dc19f7f7e5058e791d16ae05f0b123a2cc",
    body: "Dispatch: LWEVAL-1",
  };
  /** The run directory whose fixtures.json holds worker_routes' routes for `pr`: made by the first
   * call, inside a test, since the file's work directory is made in a hook, and read by every call
   * after it, since no call here edits a body. */
  let run: string | undefined;
  /** `gh <args>` answered by the gh stand-in from worker_routes' fixtures.json for `pr`. */
  function gh(...args: string[]) {
    if (run === undefined) {
      run = freshDir("worker-routes");
      const routes = bash(`${fn("worker_routes")}\nworker_routes "$1" >"$2/fixtures.json"`, [
        JSON.stringify(pr),
        run,
      ]);
      expect(routes).toEqual({ code: 0, stdout: "", stderr: "" });
    }
    const result = childSpawn(
      [process.execPath, path.join(import.meta.dir, "gh-standin.ts"), "gh", ...args],
      { SKILL_SCENARIO_RUN: run }
    );
    return {
      code: result.exitCode,
      stdout: result.stdout.toString(),
      stderr: result.stderr.toString(),
    };
  }

  // Catches: a world whose stand-in answers the prompt's lookup of the pull request with a 404,
  // after which a tester took the pull request to be unreachable and left its body unedited; and a
  // list of the open pull requests with no head that answers none, which tells a tester that lists
  // the repository's pull requests that none exists.
  test("a list of the PR's branch, or of no head, lists the PR, picked to the fields asked", () => {
    expect(
      gh(
        "pr",
        "list",
        "--repo",
        "example/widgets",
        "--head",
        "legion/LWEVAL-1",
        "--state",
        "open",
        "--json",
        "number",
        "--jq",
        ".[0].number"
      )
    ).toEqual({ code: 0, stdout: "1001\n", stderr: "" });
    for (const filter of [
      ["--head", "legion/LWEVAL-1", "--state", "open"],
      ["--head=legion/LWEVAL-1", "--state", "open"],
      ["-H", "legion/LWEVAL-1", "--state", "open"],
      ["--state", "open"],
    ]) {
      expect(gh("pr", "list", ...filter, "--json", "number,headRefOid")).toEqual({
        code: 0,
        stdout: `[{"number":1001,"headRefOid":"${pr.headRefOid}"}]\n`,
        stderr: "",
      });
    }
  });

  // Catches: a list of another branch, or of the closed or merged pull requests (on the PR's own
  // branch too), that names the fixture's open PR, or that fails where GitHub answers an empty
  // list.
  test("a list of any other head, or of closed or merged pull requests, is empty", () => {
    for (const filter of [
      ["--head", "legion/LWEVAL-10"],
      ["--head", "legion/LWEVAL-2"],
      ["--state", "closed"],
      ["-s", "merged"],
      ["--head", "legion/LWEVAL-1", "--state", "closed"],
      ["--state", "merged", "--head", "legion/LWEVAL-1"],
    ]) {
      expect(gh("pr", "list", ...filter, "--json", "number")).toEqual({
        code: 0,
        stdout: "[]\n",
        stderr: "",
      });
    }
  });
});

// ---- The rig runs an owner: the scored scenario owner-walk ----
//
// Each row reads rig.sh's functions inside its own body: a rig that lacks one fails that row alone,
// naming the function (scriptFunctions), and the rows above it still run.

const scenarios = import.meta.dir;
const assignment = scriptAssignments(path.join(scenarios, "rig.sh"));
/** rig.sh's definitions of NAMES, one after another. */
const defs = (...names: string[]) => names.map((name) => fn(name)).join("\n");
/** The sha256 of a file's bytes, in hex. */
const sha256 = (file: string) => createHash("sha256").update(readFileSync(file)).digest("hex");
/** git in DIR, with no user or system configuration and no signing; its stdout, as it printed it.
 * A failure throws with git's stderr. */
function git(dir: string, ...args: string[]): string {
  const result = childSpawn(["git", "-C", dir, "-c", "commit.gpgsign=false", ...args], {
    GIT_CONFIG_GLOBAL: "/dev/null",
    GIT_CONFIG_NOSYSTEM: "1",
    GIT_AUTHOR_NAME: "rig",
    GIT_AUTHOR_EMAIL: "rig@example.invalid",
    GIT_COMMITTER_NAME: "rig",
    GIT_COMMITTER_EMAIL: "rig@example.invalid",
  });
  if (result.exitCode !== 0)
    throw new Error(`git ${args.join(" ")} in ${dir}: ${result.stderr.toString()}`);
  return result.stdout.toString();
}
/** The lines of a JSON-lines file, each parsed, or none when the file is not there. */
function jsonLines(file: string): Record<string, unknown>[] {
  if (!existsSync(file)) return [];
  return readFileSync(file, "utf8")
    .split("\n")
    .filter((line) => line !== "")
    .map((line) => JSON.parse(line) as Record<string, unknown>);
}

/**
 * A label's repository of two commits, the files a manifest names beside it, and a manifest that
 * uses each of C4's six verbs once. The checkout holds what cmd_profile reads of one, each a
 * stand-in: the plugin installer, the home maker, the pin and a skill.
 */
function labelRepo() {
  const repo = freshDir("label-repo");
  const put = (file: string, text: string, mode = 0o644) => {
    mkdirSync(path.dirname(path.join(repo, file)), { recursive: true });
    writeFileSync(path.join(repo, file), text, { mode });
  };
  git(repo, "init", "-q");
  put("a.txt", "a1\n");
  put("b.md", "keep\ndrop me\nkeep too\n");
  put("c.txt", "c\n");
  put("f.txt", "f\n");
  put("g.txt", "say old here\n");
  put("scripts/e2e/lib/install-plugin-profile.sh", "#!/usr/bin/env bash\nexit 0\n", 0o755);
  put("scripts/e2e/lib/omp-home.sh", 'make_omp_home() { mkdir -p "$1/.omp"; }\n');
  put(".omp-pin", "github:example/oh-my-pi@1.2.3-4\n");
  put("skills/demo/SKILL.md", "# demo\n");
  git(repo, "add", "-A");
  git(repo, "commit", "-qm", "first");
  const first = git(repo, "rev-parse", "HEAD").trim();
  put("a.txt", "a2\n");
  put("d.txt", "d\n");
  put("f.txt", "f\nadded\n");
  git(repo, "add", "-A");
  git(repo, "commit", "-qm", "second");
  const second = git(repo, "rev-parse", "HEAD").trim();
  const files = freshDir("label-files");
  const file = (name: string, text: string) => {
    mkdirSync(path.dirname(path.join(files, name)), { recursive: true });
    writeFileSync(path.join(files, name), text);
  };
  put("c.txt", "c\nnew\n");
  file("patches/p.patch", git(repo, "diff", "--", "c.txt"));
  put("c.txt", "c\n");
  file("patches/u.patch", git(repo, "diff", first, second, "--", "f.txt"));
  file("drops/drop.txt", "drop me\n");
  file("drops/old.txt", "old");
  file("drops/new.txt", "new");
  file("drops/twice.txt", "keep");
  file("drops/none.txt", "not in the file");
  file(
    "every-verb.txt",
    [
      "# one line of each verb",
      `file ${first} a.txt`,
      `reverse ${second} d.txt`,
      "patch patches/p.patch",
      "unpatch patches/u.patch",
      "drop b.md drops/drop.txt",
      "replace g.txt drops/old.txt drops/new.txt",
      "",
    ].join("\n")
  );
  return { repo, second, files };
}

/** `rig.sh label-tree NAME COMMIT MANIFEST` of the repository ROOT, its manifest's files named from
 * FILES, under the work directory WORK. */
function labelTree(
  root: string,
  files: string,
  work: string,
  name: string,
  commit: string,
  manifest: string
) {
  return bash(
    `${defs("fail", "label_dir", "cmd_label_tree", "label_tree_apply", "label_tree_text")}\nroot=$1 here=$2 rig_files=$3 work=$4\ncmd_label_tree "$5" "$6" "$7"`,
    [root, scenarios, files, work, name, commit, manifest]
  );
}

describe("label_tree", () => {
  // Catches: a verb applied wrongly or not at all, a record that names another manifest than the
  // one applied, and a profile whose built-from leaves the manifest out.
  test("label_tree applies each verb and records the manifest's sha256", () => {
    const { repo, second, files } = labelRepo();
    const work = freshDir("label-tree-work");
    const manifest = path.join(files, "every-verb.txt");
    const made = labelTree(repo, files, work, "every-verb", second, manifest);
    expect({ code: made.code, stderr: made.code === 0 ? "" : made.stderr }).toEqual({
      code: 0,
      stderr: "",
    });
    const tree = path.join(work, "label-trees/every-verb/tree");
    const read = (file: string) => readFileSync(path.join(tree, file), "utf8");
    expect({
      file: read("a.txt"),
      reverse: existsSync(path.join(tree, "d.txt")),
      patch: read("c.txt"),
      unpatch: read("f.txt"),
      drop: read("b.md"),
      replace: read("g.txt"),
    }).toEqual({
      file: "a1\n",
      reverse: false,
      patch: "c\nnew\n",
      unpatch: "f\n",
      drop: "keep\nkeep too\n",
      replace: "say new here\n",
    });
    const named = ["patches/p.patch", "patches/u.patch", "drops/drop.txt", "drops/old.txt"];
    const record = [
      `commit ${second}`,
      `manifest ${sha256(manifest)}`,
      ...[...named, "drops/new.txt"].map(
        (f) => `manifest-file ${f} ${sha256(path.join(files, f))}`
      ),
      "",
    ].join("\n");
    expect(readFileSync(path.join(work, "label-trees/every-verb/label-tree"), "utf8")).toBe(record);
    expect(made.stdout).toBe(record);
    expect(readFileSync(path.join(work, "label-trees/every-verb/diff.numstat"), "utf8")).toBe(
      [
        "1\t1\ta.txt",
        "0\t1\tb.md",
        "1\t0\tc.txt",
        "0\t1\td.txt",
        "0\t1\tf.txt",
        "1\t1\tg.txt",
        "",
      ].join("\n")
    );
    // The tree's profile copies the record's manifest lines into built-from.
    const profile = bash(
      `${defs("fail", "label_dir", "on_exit", "drop_unready_keys", "checkout_commit", "install_model_route", "install_standin_route", "route_config", "route_model_line", "cmd_profile")}\n${assignment("model_standin")}\n${assignment("model_standin_unrouted")}\nwork=$1\ncmd_profile every-verb "$2" >/dev/null`,
      [work, tree],
      { SKILL_SCENARIOS_MODEL: "standin" }
    );
    expect({ code: profile.code, stderr: profile.code === 0 ? "" : profile.stderr }).toEqual({
      code: 0,
      stderr: "",
    });
    const manifestLines = record
      .split("\n")
      .filter((line) => line.startsWith("manifest"))
      .map((line) => `${line}\n`)
      .join("");
    expect(readFileSync(path.join(work, "profiles/every-verb/built-from"), "utf8")).toContain(
      manifestLines
    );
  });

  // Catches: a drop that takes the first of two copies, or that takes out nothing and passes, and
  // a failed tree left behind for a later profile to build from.
  test("label_tree fails a drop found other than once", () => {
    const { repo, second, files } = labelRepo();
    for (const [text, count] of [
      ["drops/twice.txt", 2],
      ["drops/none.txt", 0],
    ] as const) {
      const work = freshDir("label-tree-drop");
      const manifest = path.join(files, `drop-${count}.txt`);
      writeFileSync(manifest, `drop b.md ${text}\n`);
      const made = labelTree(repo, files, work, "dropped", second, manifest);
      expect(made.code).toBe(1);
      expect(made.stderr).toContain(
        `line 1 of ${realpathSync(manifest)} (drop b.md ${text}) does not apply: the text of ${files}/${text} occurs ${count} times in ${work}/label-trees/dropped/tree/b.md, not once`
      );
      expect(existsSync(path.join(work, "label-trees/dropped"))).toBe(false);
      expect(git(repo, "worktree", "list", "--porcelain")).not.toContain(work);
    }
  });

  // Catches: a failed worktree add that names no reason, its log removed with the tree, and a
  // hint that misses the prune a tree deleted by hand needs before git adds it again.
  test("a label tree whose worktree add fails names git's reason and the prune a tree deleted by hand needs", () => {
    const { repo, second, files } = labelRepo();
    const work = freshDir("label-tree-gone");
    const manifest = path.join(files, "every-verb.txt");
    expect(labelTree(repo, files, work, "gone", second, manifest).code).toBe(0);
    // The tree deleted by hand: git still holds it registered.
    rmSync(path.join(work, "label-trees/gone"), { recursive: true, force: true });
    const again = labelTree(repo, files, work, "gone", second, manifest);
    expect(again.code).toBe(1);
    expect(again.stderr).toContain(
      `git worktree add of ${second} failed for the label tree gone: `
    );
    expect(again.stderr).toContain("label-trees/gone/tree");
    expect(again.stderr).toContain(`git -C ${repo} worktree prune`);
    expect(existsSync(path.join(work, "label-trees/gone"))).toBe(false);
  });

  const controls = path.join(scenarios, "controls");
  /** The two files of #4902's head (6b0ece5c101b1d63bec807f90417a6c75bb0870f) that the control
   * manifests change, committed as controls/pr-4902-head/ (controls/patches/README.md): each
   * fixture, the path it has in the head, and its blob there. */
  const PR_4902_FILES = [
    [
      "architect-common.md",
      "packages/daemon/internal/prompts/go/architect-common.md",
      "f77cd71515fd18717867424b23ab9994cd725504",
    ],
    [
      "plan.md",
      "skills/legion-architect/references/plan.md",
      "15df444d14aa31cdfc192f83a047fc93164550d7",
    ],
  ] as const;
  /** A repository of one commit that holds #4902's head's two files at their paths, built from the
   * fixtures, each held to its blob at that head: no row reads the network or a ref that moves. */
  function repo4902(): { repo: string; head: string } {
    const repo = freshDir("pr-4902");
    git(repo, "init", "-q");
    for (const [fixture, file] of PR_4902_FILES) {
      mkdirSync(path.dirname(path.join(repo, file)), { recursive: true });
      cpSync(path.join(controls, "pr-4902-head", fixture), path.join(repo, file));
    }
    git(repo, "add", "-A");
    git(repo, "commit", "-qm", "the two files of #4902's head");
    for (const [, file, blob] of PR_4902_FILES)
      expect({ file, blob: git(repo, "rev-parse", `HEAD:${file}`).trim() }).toEqual({ file, blob });
    return { repo, head: git(repo, "rev-parse", "HEAD").trim() };
  }

  // Catches: a control manifest that no longer applies to the head whose sentences it takes out,
  // and a label-tree that reads a ref the checkout lacks as a tree to build.
  test("the owner-walk manifests apply to #4902's head's two files, committed as fixtures", () => {
    const absent = freshDir("no-4902");
    git(absent, "init", "-q");
    const none = freshDir("no-4902-work");
    const missing = labelTree(
      absent,
      scenarios,
      none,
      "ctl-d",
      "refs/pull/4902/head",
      path.join(controls, "owner-walk-D.txt")
    );
    expect(missing.code).toBe(3);
    expect(missing.stderr).toContain("UNKNOWN: refs/pull/4902/head not in this checkout");
    expect(existsSync(path.join(none, "label-trees/ctl-d"))).toBe(false);
    const { repo, head } = repo4902();
    for (const [name, manifest] of [
      ["ctl-d", "owner-walk-D.txt"],
      ["ctl-d-strict", "owner-walk-D-strict.txt"],
      ["ctl-e", "owner-walk-E.txt"],
    ] as const) {
      const made = labelTree(
        repo,
        scenarios,
        freshDir(`at-4902-${name}`),
        name,
        head,
        path.join(controls, manifest)
      );
      expect({ name, code: made.code, stderr: made.code === 0 ? "" : made.stderr }).toEqual({
        name,
        code: 0,
        stderr: "",
      });
    }
  });

  // Catches: a D whose tree takes out more than #4902's own lines (a manifest line too many, a
  // patch of another file), which would make D's control a control of something else.
  test("D's tree differs from its commit in the patch's lines alone, at #4902's head's two files", () => {
    const { repo, head } = repo4902();
    const patch = path.join(controls, "patches/owner-walk-D.patch");
    const holds = (work: string, name: string) =>
      bash(`${defs("fail", "cmd_unpatch_holds")}\nwork=$1\ncmd_unpatch_holds "$2" "$3"`, [
        work,
        name,
        patch,
      ]);
    const work = freshDir("d-holds");
    const made = labelTree(
      repo,
      scenarios,
      work,
      "ctl-d",
      head,
      path.join(controls, "owner-walk-D.txt")
    );
    expect({ code: made.code, stderr: made.code === 0 ? "" : made.stderr }).toEqual({
      code: 0,
      stderr: "",
    });
    // The manifest control reads this line: one more line of the manifest changes it.
    console.log(
      `D's tree at ${head}, its record: ${made.stdout.split("\n").find((line) => line.startsWith("manifest ")) ?? "no manifest line"}`
    );
    expect(readFileSync(path.join(work, "label-trees/ctl-d/diff.numstat"), "utf8")).toBe(
      "0\t2\tpackages/daemon/internal/prompts/go/architect-common.md\n"
    );
    expect(holds(work, "ctl-d")).toEqual({
      code: 0,
      stdout:
        "the label tree ctl-d differs from its commit in the lines of owner-walk-D.patch alone: 0 2 packages/daemon/internal/prompts/go/architect-common.md\n",
      stderr: "",
    });
    // D-strict's tree takes out three lines more, so the same check fails it.
    const strictWork = freshDir("d-strict-holds");
    expect(
      labelTree(
        repo,
        scenarios,
        strictWork,
        "ctl-d-strict",
        head,
        path.join(controls, "owner-walk-D-strict.txt")
      ).code
    ).toBe(0);
    const strict = holds(strictWork, "ctl-d-strict");
    expect(strict.code).toBe(1);
    expect(strict.stderr).toContain("differs from its commit in more than the lines of");
  });
});

describe("a label older than its entry", () => {
  // Catches: a scored batch that runs a label with no label-texts.sh, whose texts the rig would
  // then have to write itself.
  test("a label with no label-texts.sh is refused by name", () => {
    const co = freshDir("old-label");
    const check = bash(`${defs("fail", "require_label_texts")}\nrequire_label_texts "$1" old`, [
      co,
    ]);
    expect(check.code).toBe(1);
    expect(check.stderr).toContain(
      `label old: its checkout ${co} has no packages/pi-legion/scripts/skill-scenarios/label-texts.sh`
    );
    // rig.sh batch-scored refuses it before any service starts.
    const work = freshDir("old-label-work");
    const P = path.join(work, "profiles/old");
    const agent = path.join(P, "home/.omp/profiles/skills-old/agent");
    mkdirSync(agent, { recursive: true });
    writeFileSync(
      path.join(agent, "config.yml"),
      "extensionHandlers:\n  toolCallTimeoutMs: 120000\n"
    );
    for (const [file, text] of [
      ["ready", ""],
      ["checkout", `${co}\n`],
      ["pin", "github:example/oh-my-pi@1.2.3-4\n"],
      ["profile", "skills-old\n"],
    ] as const)
      writeFileSync(path.join(P, file), text);
    const batch = childSpawn(
      ["bash", path.join(scenarios, "rig.sh"), "batch-scored", "owner-walk", "1", "old"],
      { SKILL_SCENARIOS_WORK: work, SKILL_SCENARIOS_OWNER_WALK_CASE: "root-B-claude" }
    );
    expect({
      code: batch.exitCode,
      named: batch.stderr.toString().includes("label old: its checkout"),
    }).toEqual({
      code: 1,
      named: true,
    });
    expect(existsSync(path.join(work, "services/up"))).toBe(false);
  });

  // Catches: a form-A launch whose guidance a label cannot take run as if it had none, or scored.
  test("a form-A launch at a label whose launch refuses planGuidanceFile is a rig error naming it", () => {
    const R = freshDir("guided-run");
    const co = freshDir("guided-label");
    const work = freshDir("guided-work");
    const entry = path.join(co, "packages/pi-legion/scripts/skill-scenarios/label-texts.sh");
    mkdirSync(path.dirname(entry), { recursive: true });
    // A label whose TestRigLaunch takes no plan guidance: it refuses the field by name, as
    // rig_launch_test.go's decodeRigLaunchRequest does with DisallowUnknownFields.
    writeFileSync(
      entry,
      [
        "#!/usr/bin/env bash",
        `if [ "$1" = launch ] && jq -e 'has("planGuidanceFile")' "$2" >/dev/null; then`,
        "  echo '=== RUN   TestRigLaunch'",
        '  echo "    rig_launch_test.go:54: the launch request $2: json: unknown field \\"planGuidanceFile\\""',
        "  echo '--- FAIL: TestRigLaunch (0.00s)'",
        "  exit 1",
        "fi",
        "exit 0",
        "",
      ].join("\n"),
      { mode: 0o755 }
    );
    const services = path.join(work, "services-env");
    writeFileSync(
      services,
      "ENVOY_URL=http://127.0.0.1:1\nENVOY_NATS_URL=nats://127.0.0.1:2\nENVOY_TOKEN_FILE=/rig/envoy-token\n"
    );
    writeFileSync(
      path.join(R, "world-request.json"),
      JSON.stringify({
        project: "LWEVAL",
        repository: "example/csv-importer",
        defaultBranch: "main",
        issues: [{ key: "LWEVAL-4", tree: "LWEVAL-4" }],
        subject: "LWEVAL-4",
        role: "architect",
      })
    );
    writeFileSync(path.join(R, "world-answer.json"), JSON.stringify({ starts: [], notices: [] }));
    const result = bash(
      [
        defs("fail", "service_value", "launch_request", "label_texts", "scored_rig_error"),
        "R=$1 co=$2 work=$3 P=$3/profiles/head services_env=$4 pg_url=postgres://127.0.0.1:1/postgres",
        'launch_request "$R/world-request.json" "$R/world-answer.json" http://127.0.0.1:1 "$R/guidance.md" >"$R/launch-request.json"',
        'if (label_texts launch "$R/launch-request.json" "$R/launch-answer.json") 2>"$R/err"; then exit 9; fi',
        'scored_rig_error owner-walk-root-A-claude-head-1 head root-A-claude "$(grep "^skill-scenarios: " "$R/err" | tail -n 1)"',
      ].join("\n"),
      [R, co, work, services]
    );
    expect({ code: result.code, stderr: result.stderr }).toEqual({ code: 0, stderr: "" });
    expect(
      JSON.parse(readFileSync(path.join(R, "launch-request.json"), "utf8")).planGuidanceFile
    ).toBe(path.join(R, "guidance.md"));
    const row = RunRow.parse(
      JSON.parse(
        readFileSync(path.join(work, "runs/owner-walk-root-A-claude-head-1/row.json"), "utf8")
      )
    );
    expect(row.outcome).toBe("rig-error");
    expect(row.outcome === "rig-error" ? row.error : "").toContain(
      'unknown field "planGuidanceFile"'
    );
  });
});

describe("launch_env", () => {
  // Catches: a run that inherits a deployment's stall limits, so its end waits on another clock
  // than the plugin's defaults.
  test("a launch whose environment holds a LEGION_STALL_ variable is refused, naming it", () => {
    const dir = freshDir("launch-env");
    let answers = 0;
    const answer = (env: Record<string, string>) => {
      const file = path.join(dir, `answer-${++answers}.json`);
      writeFileSync(
        file,
        JSON.stringify({ systemPromptFile: "/rig/system.md", env, published: [] })
      );
      return file;
    };
    const run = (file: string) => bash(`${defs("fail", "launch_env")}\nlaunch_env "$1"`, [file]);
    expect(run(answer({ ENVOY_URL: "http://127.0.0.1:1", LEGION_ROLE: "architect" }))).toEqual({
      code: 0,
      stdout: "ENVOY_URL=http://127.0.0.1:1\nLEGION_ROLE=architect\n",
      stderr: "",
    });
    const stalled = run(
      answer({ ENVOY_URL: "http://127.0.0.1:1", LEGION_STALL_REMINDER_MS: "1000" })
    );
    expect({ code: stalled.code, stdout: stalled.stdout }).toEqual({ code: 1, stdout: "" });
    expect(stalled.stderr).toContain("the launch's environment holds LEGION_STALL_REMINDER_MS");
  });
});

// A batch given SKILL_SCENARIOS_POSTGRES_URL and SKILL_SCENARIOS_NATS_URL, as scored-job.sh gives
// it the runner pod's own: services_up, then own_dispatch in each run. Its guards read the URL with
// libpq through python3, as the services_up rows above do.
describe.skipIf(withoutPython || withoutLibpq)("a batch given its Postgres and NATS", () => {
  // Catches: a batch in a pod with no Docker that still starts a container; a batch that sizes
  // the given Postgres for more runs than it runs at once, which refuses an owner-walk batch (one
  // run at a time) on the pod's server; a run's database made somewhere other than the server it
  // was given, or its Dispatch started on another database or from another checkout than the
  // label's; a dispatch.json that names the server with its password; and a run whose label entry
  // gets no login to that server (pg_url, which label_texts gives it as LEGION_TEST_PG_DSN).
  test("services_up starts no container, and each run's database is made with psql on that server", () => {
    const dir = freshDir("given");
    const bin = freshDir("given-bin");
    const calls = path.join(dir, "calls");
    const stub = (tool: string, body = "") =>
      writeFileSync(path.join(bin, tool), `#!/bin/sh\necho "${tool} $*" >>${calls}\n${body}`, {
        mode: 0o755,
      });
    stub("psql", 'case "$*" in *"SHOW max_connections"*) echo 200 ;; esac\n');
    stub("docker", "exit 1\n");
    stub("bun");
    const url = "postgres://rig:pw@127.0.0.1:5999/postgres?sslmode=disable";
    const login = "postgres://rig@127.0.0.1:5999/postgres?sslmode=disable";
    const script = [
      "set -euo pipefail",
      '. "$1"',
      '. "$2"',
      defs("fail", "note", "given_services", "safe_url", "pg_sql", "services_up", "own_dispatch"),
      'until_true() { shift 2; "$@"; }',
      "connections_needed() { per_run=117; needs=$((per_run * parallel + 20)); }",
      'nats_ready() { echo "nats_ready $1" >>"$calls"; }',
      "listener_up() { envoy_port=1; }",
      'start_dispatch() { echo "start_dispatch $1 | $2" >>"$calls"; dispatch_port=1; }',
      "root=$3 here=$3/packages/pi-legion/scripts/skill-scenarios S=$4/services calls=$4/calls tag=t",
      'mkdir -p "$S"',
      "services_env=$S/env services_postgres=$S/postgres-url",
      'if [ $# -ge 5 ]; then services_up "$5"; else services_up; fi',
      'for R in "$4/runs/owner-walk-root-B-claude-head-1" "$4/runs/owner-walk-root-B-claude-head-2"; do',
      '  mkdir -p "$R"',
      "  pg_url=''",
      "  own_dispatch project /label/checkout",
      '  echo "pg_url $pg_url" >>"$calls"',
      '  jq -c . "$R/dispatch.json" >>"$calls"',
      "done",
    ].join("\n");
    const env = {
      PATH: `${bin}:${callerPath}`,
      SKILL_SCENARIOS_POSTGRES_URL: url,
      SKILL_SCENARIOS_NATS_URL: "nats://127.0.0.1:4999",
    };
    const pgUrl = path.join(root, "packages/envoy/scripts/lib/pg-url.sh");
    const loopbackDsn = path.join(root, "scripts/e2e/lib/loopback-dsn.sh");
    const result = bash(script, [pgUrl, loopbackDsn, root, dir, "1"], env);
    expect({ code: result.code, stderr: result.code === 0 ? "" : result.stderr }).toEqual({
      code: 0,
      stderr: "",
    });
    expect(result.stdout).toContain(
      `services up: listener :1, NATS nats://127.0.0.1:4999, Postgres ${login}`
    );
    const seen = readFileSync(calls, "utf8").trimEnd().split("\n");
    expect(seen.filter((line) => line.startsWith("docker "))).toEqual([]);
    expect(seen).toContain("nats_ready nats://127.0.0.1:4999");
    const runs = ["run_owner_walk_root_b_claude_head_1", "run_owner_walk_root_b_claude_head_2"];
    for (const run of runs) {
      expect(seen.filter((line) => line.endsWith(` -c CREATE DATABASE ${run}`))).toEqual([
        `psql ${login} -X -q -w -v ON_ERROR_STOP=1 -A -t -F \t -c CREATE DATABASE ${run}`,
      ]);
      const started = seen.filter((line) => line.includes(`dbname='${run}'`));
      expect(started).toHaveLength(1);
      expect(started[0]).toStartWith("start_dispatch ");
      expect(started[0]).toEndWith(" | /label/checkout");
      expect(seen).toContain(
        JSON.stringify({ postgres: login, database: run, port: 1, url: "http://127.0.0.1:1" })
      );
    }
    const logins = seen.filter((line) => line.startsWith("pg_url "));
    expect(logins).toEqual([`pg_url ${url}`, `pg_url ${url}`]);

    // The same server sized for SKILL_SCENARIOS_PARALLEL's default of 5 runs at once is refused
    // before any run, since it admits one; and one of the two URLs alone gives no servers.
    const five = bash(script, [pgUrl, loopbackDsn, root, freshDir("given-five")], env);
    expect(five.code).toBe(1);
    expect(five.stderr).toContain("SKILL_SCENARIOS_PARALLEL is 5");
    const half = bash(`${defs("given_services")}\ngiven_services || echo none`, [], {
      SKILL_SCENARIOS_POSTGRES_URL: url,
    });
    expect(half).toEqual({ code: 0, stdout: "none\n", stderr: "" });
  });
});

describe("install_provider_route's two new forms", () => {
  // Catches: a gateway route a build pod cannot use (its key is a command, and a GPT model speaks
  // the Responses API), or one that takes a key it cannot run.
  test("install_provider_route takes an executable key command and an openai-responses block, and refuses another form", () => {
    const dir = freshDir("provider-forms");
    const key = path.join(dir, "model-token");
    writeFileSync(key, "#!/bin/sh\necho token\n", { mode: 0o755 });
    const plain = path.join(dir, "model-token-plain");
    writeFileSync(plain, "token\n", { mode: 0o644 });
    let files = 0;
    const file = (api: string, apiKey: string) => {
      const out = path.join(dir, `gpt-${++files}.yml`);
      const block = {
        baseUrl: "http://gateway.example.invalid:4000/v1",
        api,
        apiKey,
        authHeader: true,
        models: [{ id: "openai/gpt-6.1-sol" }],
      };
      writeFileSync(out, JSON.stringify({ providers: { "gateway-openai": block } }));
      return out;
    };
    const env = (providers: string) => ({
      LEGION_E2E_MODEL_ROUTE: "provider",
      LEGION_E2E_MODEL_PROVIDER_FILE: providers,
      LEGION_E2E_MODEL_PROVIDER: "gateway-openai",
    });
    const taken = profileDir();
    const ok = route(taken, env(file("openai-responses", `!${key}`)), "openai/gpt-6.1-sol");
    expect({ code: ok.code, stderr: ok.code === 0 ? "" : ok.stderr }).toEqual({
      code: 0,
      stderr: "",
    });
    const agent = path.join(taken, "home/.omp/profiles/skills-test/agent");
    expect(JSON.parse(readFileSync(path.join(agent, "models.yml"), "utf8"))).toEqual({
      providers: {
        "gateway-openai": {
          baseUrl: "http://gateway.example.invalid:4000/v1",
          api: "openai-responses",
          apiKey: `!${key}`,
          authHeader: true,
          models: [{ id: "openai/gpt-6.1-sol" }],
        },
      },
    });
    const config = readFileSync(path.join(agent, "config.yml"), "utf8");
    expect(config).toContain("  default: gateway-openai/openai/gpt-6.1-sol\n");
    // The built-in openai provider is disabled, so no session falls back to it without a word.
    expect(config).toContain("\n  - openai\n");
    for (const [api, apiKey, refusal] of [
      [
        "openai-responses",
        `!${plain}`,
        `reads its key with ${plain}, which is not an executable file`,
      ],
      [
        "openai-responses",
        "!model-token",
        'does not read its key as "!cat <absolute file>" or "!<absolute executable>"',
      ],
      [
        "google-generative-ai",
        `!${key}`,
        "speaks google-generative-ai, not anthropic-messages or openai-responses",
      ],
    ] as const) {
      const refused = route(profileDir(), env(file(api, apiKey)), "openai/gpt-6.1-sol");
      expect({
        code: refused.code,
        refusal: refused.stderr.includes(refusal) ? refusal : refused.stderr,
      }).toEqual({
        code: 1,
        refusal,
      });
    }
  });
});

describe("rig_omp", () => {
  // Catches: a run on an Oh My Pi other than the label's pin, which a pod's own install could be.
  test("SKILL_SCENARIOS_OMP is refused when its version is not the pin", () => {
    const install = freshDir("omp-install");
    mkdirSync(path.join(install, "bin"));
    const omp = path.join(install, "bin/omp");
    const loose = path.join(install, "omp-1.2.3");
    for (const file of [omp, loose])
      writeFileSync(file, "#!/bin/sh\necho omp/1.2.3\n", { mode: 0o755 });
    const pick = (binary: string, pin: string) =>
      bash(`${defs("fail", "pinned_omp", "rig_omp")}\nrig_omp "$1"`, [pin], {
        SKILL_SCENARIOS_OMP: binary,
      });
    expect(pick(omp, "github:example/oh-my-pi@1.2.3-7")).toEqual({
      code: 0,
      stdout: `${omp}\n`,
      stderr: "",
    });
    const other = pick(omp, "github:example/oh-my-pi@1.2.4-1");
    expect(other.code).toBe(1);
    expect(other.stderr).toContain(
      `SKILL_SCENARIOS_OMP ${omp} prints --version 'omp/1.2.3', and the label's .omp-pin names github:example/oh-my-pi@1.2.4-1 (omp/1.2.4)`
    );
    const elsewhere = pick(loose, "github:example/oh-my-pi@1.2.3-7");
    expect(elsewhere.code).toBe(1);
    expect(elsewhere.stderr).toContain("is not <dir>/bin/omp");
  });

  // Catches: a launch or a superpowers probe that asks mise for the pin's Oh My Pi where
  // SKILL_SCENARIOS_OMP names the one to run, as a build pod with no mise does.
  test("launch and install_superpowers run the Oh My Pi SKILL_SCENARIOS_OMP names, and call no mise", () => {
    const install = freshDir("omp-direct");
    mkdirSync(path.join(install, "bin"));
    const omp = path.join(install, "bin/omp");
    // The pod's Oh My Pi: it prints the pin's release for --version; any other call is recorded,
    // and leaves the probe's transcript in the session directory it is given.
    writeFileSync(
      omp,
      `#!/usr/bin/env bash
if [ "\${1:-}" = --version ]; then echo omp/1.2.3; exit 0; fi
printf '%s\\n' "$*" >>${JSON.stringify(path.join(install, "calls.txt"))}
while [ $# -gt 0 ] && [ "$1" != --session-dir ]; do shift; done
if [ $# -ge 2 ]; then mkdir -p "$2" && cp ${JSON.stringify(path.join(install, "probe.jsonl"))} "$2/probe.jsonl"; fi
echo "the pod's omp ran"
`,
      { mode: 0o755 }
    );
    writeFileSync(
      path.join(install, "probe.jsonl"),
      `${JSON.stringify({ message: { role: "toolResult", content: [{ type: "text", text: "# Skill writing-plans" }] } })}\n`
    );
    // A mise that records each call and fails, as no mise answers in a pod.
    const tools = freshDir("omp-direct-tools");
    const miseCalls = path.join(install, "mise-calls.txt");
    writeFileSync(
      path.join(tools, "mise"),
      `#!/bin/sh\necho "mise $*" >>${JSON.stringify(miseCalls)}\nexit 1\n`,
      { mode: 0o755 }
    );
    const env = {
      PATH: `${tools}:${callerPath}`,
      SKILL_SCENARIOS_OMP: omp,
    };
    const pin = "github:example/oh-my-pi@1.2.3-4";

    const run = freshDir("omp-direct-run");
    for (const dir of ["cwd", "sessions"]) mkdirSync(path.join(run, dir));
    writeFileSync(path.join(run, "prompt.txt"), "Plan the issue.\n");
    writeFileSync(path.join(run, "pane.env"), `PATH=${tools}:${callerPath}\n`);
    const standIns = `
tmux() { case $1 in new-session) (command sleep 0.3 && bash -c "\${@: -1}") & ;; esac; }
sleep() { command sleep 0.05; }`;
    const launched = bash(
      `${defs("fail", "pinned_omp", "rig_omp", "launch")}\n${standIns}\nR=$1 tag=t pin=$2\nlaunch owner-walk-s1-1`,
      [run, pin],
      env
    );
    expect({ code: launched.code, stderr: launched.stderr }).toEqual({ code: 0, stderr: "" });
    expect(readFileSync(path.join(run, "out.txt"), "utf8")).toBe("the pod's omp ran\nexit=0\n");

    const label = freshDir("omp-direct-label");
    const src = freshDir("omp-direct-superpowers");
    for (const skill of ["writing-plans", "brainstorming"]) {
      mkdirSync(path.join(src, "skills", skill), { recursive: true });
      writeFileSync(
        path.join(src, "skills", skill, "SKILL.md"),
        `---\nname: ${skill}\n---\n\n# Skill ${skill}\n`
      );
    }
    writeFileSync(path.join(label, "built-from"), "checkout /x\n");
    const probed = bash(
      `${defs("fail", "pinned_omp", "rig_omp", "install_superpowers")}\nsuperpowers_skills=(writing-plans brainstorming)\ninstall_superpowers "$1" skills-s1 "$2"`,
      [label, pin],
      { ...env, SKILL_SCENARIOS_SUPERPOWERS: src, USER: "rig" }
    );
    expect({ code: probed.code, stderr: probed.stderr }).toEqual({ code: 0, stderr: "" });
    expect(existsSync(path.join(label, "superpowers"))).toBe(true);

    const calls = readFileSync(path.join(install, "calls.txt"), "utf8").trimEnd().split("\n");
    expect(calls).toHaveLength(2);
    expect(calls[0]).toStartWith("-p Plan the issue.");
    expect(calls[1]).toStartWith("-p Call the read tool once on the path skill://writing-plans");
    expect(existsSync(miseCalls)).toBe(false);
  });
});

describe("owner_walk_case", () => {
  // Catches: an owner-walk run with no case, or one owner-walk.ts does not know, which would run
  // a world no cell counts.
  test("SKILL_SCENARIOS_OWNER_WALK_CASE is refused when unset or unknown, naming its values", () => {
    const listed = childSpawn([process.execPath, path.join(scenarios, "owner-walk.ts"), "cases"]);
    const cases = listed.stdout
      .toString()
      .trim()
      .split("\n")
      .map((line) => line.split(" ")[0] ?? "");
    expect(cases).toContain("root-B-claude");
    const pick = (value?: string) =>
      bash(
        `${defs("fail", "owner_walk_case")}\nhere=$1\nowner_walk_case`,
        [scenarios],
        value === undefined ? {} : { SKILL_SCENARIOS_OWNER_WALK_CASE: value }
      );
    expect(pick("root-B-claude")).toEqual({ code: 0, stdout: "root-B-claude\n", stderr: "" });
    const unset = pick();
    expect(unset.code).toBe(1);
    expect(unset.stderr).toContain(
      `SKILL_SCENARIOS_OWNER_WALK_CASE is unset: an owner-walk run takes one case, <kind>-<form>-<family>, of ${cases.join(" ")}`
    );
    const unknown = pick("root-F-claude");
    expect(unknown.code).toBe(1);
    expect(unknown.stderr).toContain(
      `SKILL_SCENARIOS_OWNER_WALK_CASE is 'root-F-claude', none of owner-walk's cases: ${cases.join(" ")}`
    );
  });
});

describe("batch_scored", () => {
  // Catches: a cell that launches without end while its label cannot boot, and one that reads a
  // cell short of its count as anything but UNKNOWN.
  test("batch_scored stops a cell at three times its count of launches, and prints UNKNOWN for it", () => {
    // A launch that writes its run's row: a rig error, or scored from the launch SCORED_FROM on.
    const attempt = [
      "scored_attempt() {",
      "  local dir=$work/runs/$1-$3-$2-$4",
      '  mkdir -p "$dir" && printf "%s\\n" "$4" >>"$work/launches"',
      '  if [ -n "$SCORED_FROM" ] && [ "$4" -ge "$SCORED_FROM" ]; then echo \'{"outcome":"scored"}\' >"$dir/row.json"; else echo \'{"outcome":"rig-error"}\' >"$dir/row.json"; fi',
      "}",
    ].join("\n");
    const cell = (work: string, count: number, scoredFrom?: number) =>
      bash(
        `${defs("fail", "next_run", "scored_count", "batch_scored_cell")}\n${attempt}\nwork=$1\nbatch_scored_cell owner-walk head root-B-claude "$2"`,
        [work, String(count)],
        scoredFrom === undefined ? {} : { SCORED_FROM: String(scoredFrom) }
      );
    const never = freshDir("cell-never");
    expect(cell(never, 2)).toEqual({
      code: 0,
      stdout:
        "UNKNOWN: owner-walk head root-B-claude: 0 scored runs of 2 after 6 launches, three times its count\n",
      stderr: "",
    });
    expect(readFileSync(path.join(never, "launches"), "utf8")).toBe("1\n2\n3\n4\n5\n6\n");
    const second = freshDir("cell-second");
    expect(cell(second, 1, 2)).toEqual({ code: 0, stdout: "", stderr: "" });
    expect(readFileSync(path.join(second, "launches"), "utf8")).toBe("1\n2\n");
  });
});

describe("rig.sh cell", () => {
  /** The sentence of a clause of the guidance stage, by its id. */
  const sentence = (id: string): string => {
    const found = OWNER_WALK_STAGES.guidance?.clauses.find((each) => each.id === id);
    if (found === undefined) throw new Error(`the guidance stage has no clause ${id}`);
    return found.sentence;
  };
  /** One run of a case under a label, as a scored batch leaves it: its world.json, and its scored
   * row.json whose clauses pass as `passes` says. */
  function plantScored(
    work: string,
    label: string,
    owner: { kind: "root" | "child"; form: "B" | "D"; family: "claude" | "gpt" },
    n: number,
    passes: Record<string, boolean>
  ): void {
    const kase = `${owner.kind}-${owner.form}-${owner.family}`;
    const name = `owner-walk-${kase}-${label}-${n}`;
    const dir = path.join(work, "runs", name);
    mkdirSync(dir, { recursive: true });
    const world = {
      scenario: "owner-walk",
      stage: "guidance",
      ...owner,
      issue: "issue.md",
      plansProject: "LWEVAL",
      plansKeys: ["LWEVAL-1"],
      guidance:
        owner.form === "D"
          ? {
              file: "guidance-first.md",
              implementerFamily: "claude",
              reviewerFamily: "gpt",
              implementerModel: "claude-model",
              reviewerModel: "gpt-model",
            }
          : null,
      questions: ["Would another family review the change?"],
    };
    writeFileSync(path.join(dir, "world.json"), `${JSON.stringify(world)}\n`);
    const clauses = Object.entries(passes).map(([id, pass]) => ({
      id,
      clause: sentence(id),
      pass,
      evidence: `${id}: planted`,
      counts: {},
    }));
    const row = RunRow.parse({
      outcome: "scored",
      run: name,
      scenario: "owner-walk",
      label,
      case: kase,
      pass: clauses.every((clause) => clause.pass),
      clauses,
      reach: [],
    });
    writeFileSync(path.join(dir, "row.json"), `${JSON.stringify(row)}\n`);
  }
  /** The twelve owners of form D under a label, three runs of each kind and family, in that order;
   * owner `i` passes read when `passes(i)`. */
  function plantControl(work: string, label: string, passes: (owner: number) => boolean): void {
    let owner = 0;
    for (const kind of ["root", "child"] as const)
      for (const family of ["claude", "gpt"] as const)
        for (let n = 1; n <= 3; n += 1) {
          plantScored(work, label, { kind, form: "D", family }, n, { read: passes(owner) });
          owner += 1;
        }
  }
  /** `rig.sh cell owner-walk LABEL CASE COUNT`'s one line. */
  function line(work: string, label: string, kase: string, count: number): string {
    const cell = childSpawn(
      ["bash", path.join(scenarios, "rig.sh"), "cell", "owner-walk", label, kase, String(count)],
      { SKILL_SCENARIOS_WORK: work }
    );
    expect({ code: cell.exitCode, stderr: cell.stderr.toString() }).toEqual({
      code: 0,
      stderr: "",
    });
    return cell.stdout.toString().trimEnd();
  }

  // Catches: a cell line that gives a control form's case the rule of a case's cell, pass when
  // every run passes, where the control counts its twelve owners and passes when fewer than six
  // of them pass its clause: the last line of a controls Job would read the opposite verdict.
  test("rig.sh cell gives a control form's case the control's verdict over its twelve owners, and a case form its own", () => {
    const work = freshDir("cell-lines");
    // Five of twelve pass read, and none of root-claude's three: the control passes.
    plantControl(work, "ctl-d", (owner) => [3, 6, 9, 10, 11].includes(owner));
    // Six of twelve, root-claude's three among them: the control fails.
    plantControl(work, "ctl-d-strict", (owner) => [0, 1, 2, 3, 6, 9].includes(owner));
    const five = line(work, "ctl-d", "root-D-claude", 3);
    const six = line(work, "ctl-d-strict", "root-D-claude", 3);
    expect([five, six].map((text) => text.slice(text.lastIndexOf(" verdict ") + 9))).toEqual([
      "pass",
      "fail",
    ]);
    expect(five).toBe(
      "cell owner-walk ctl-d root-D-claude: the control guidance/D over every owner: 12 runs, 12 scored, 0 rig errors, count 12; read 5 of 12; it passes when fewer than 6 pass read; verdict pass"
    );
    expect(six).toBe(
      "cell owner-walk ctl-d-strict root-D-claude: the control guidance/D over every owner: 12 runs, 12 scored, 0 rig errors, count 12; read 6 of 12; it passes when fewer than 6 pass read; verdict fail"
    );

    // A case form's cell is its case's runs at the count given, and passes when each passes.
    const b = { kind: "root", form: "B", family: "claude" } as const;
    const all = { read: true, choices: true, "earlier-and-fresh": true };
    for (let n = 1; n <= 3; n += 1) plantScored(work, "head", b, n, all);
    for (let n = 1; n <= 3; n += 1) plantScored(work, "head-two", b, n, { ...all, read: n !== 2 });
    expect(line(work, "head", "root-B-claude", 3)).toBe(
      "cell owner-walk head root-B-claude: 3 runs, 3 scored, 0 rig errors, count 3; each clause: read 3 of 3, choices 3 of 3, earlier-and-fresh 3 of 3; runs passed 3 of 3; verdict pass"
    );
    expect(line(work, "head-two", "root-B-claude", 3)).toBe(
      "cell owner-walk head-two root-B-claude: 3 runs, 3 scored, 0 rig errors, count 3; each clause: read 2 of 3, choices 3 of 3, earlier-and-fresh 3 of 3; runs passed 2 of 3; verdict fail"
    );
  });
});

describe("rig.sh batch", () => {
  // Catches: a general batch that runs owner-walk beside batch-scored, with its own copies of
  // batch-scored's owner-walk steps.
  test("rig.sh batch refuses owner-walk, naming batch-scored, before it starts anything", () => {
    const work = freshDir("batch-owner-walk");
    const batch = childSpawn(
      ["bash", path.join(scenarios, "rig.sh"), "batch", "owner-walk", "1", "head"],
      { SKILL_SCENARIOS_WORK: work, SKILL_SCENARIOS_OWNER_WALK_CASE: "root-B-claude" }
    );
    expect(batch.exitCode).toBe(1);
    expect(batch.stderr.toString()).toContain("rig.sh batch-scored owner-walk");
    expect(existsSync(path.join(work, "services"))).toBe(false);
  });
});

describe("the recorded plans answer", () => {
  // Catches: a key map that gives a recorded issue another key than the one the seed's create
  // gets (seed.ts refuses it), and a source whose sha256 is not its answer's.
  test("plans_source maps the recorded issues to LWEVAL-1 to LWEVAL-N in the order of their numbers", () => {
    const dir = freshDir("plans-source");
    const answer = path.join(dir, "plans-answer.json");
    writeFileSync(
      answer,
      `${JSON.stringify({
        project: "SRX1",
        issues: [
          { issue: "SRX1-1", plan: { issue: "SRX1-1" } },
          { issue: "SRX1-10", plan: { issue: "SRX1-10" } },
          { issue: "SRX1-2", plan: { issue: "SRX1-2" } },
        ],
      })}\n`
    );
    const source = bash(
      `${defs("plans_source")}\n${assignment("owner_walk_project")}\nplans_source "$1" "$2"`,
      [answer, "c".repeat(40)]
    );
    expect(source.code).toBe(0);
    expect(JSON.parse(source.stdout)).toEqual({
      commit: "c".repeat(40),
      sha256: sha256(answer),
      keyMap: { SRX1: "LWEVAL", "SRX1-1": "LWEVAL-1", "SRX1-2": "LWEVAL-2", "SRX1-10": "LWEVAL-3" },
    });
  });

  // Catches: a decoder that keeps a body whose bytes are not the ones the capture printed.
  test("decode_plans_answer keeps the body whose sha256 the log printed, and refuses another", () => {
    const dir = freshDir("plans-decode");
    const body = Buffer.from(
      `${JSON.stringify({ project: "SRX1", issues: [], note: "x".repeat(200) })}\n`
    );
    const lines = (body.toString("base64").match(/.{1,76}/g) ?? []).join("\n");
    const log = (sum: string) => {
      const file = path.join(dir, `pod-${sum.slice(0, 4)}.log`);
      writeFileSync(
        file,
        `--- PASS: TestPlanRatingsLive (1.00s)\nplans answer begin\n${lines}\nplans answer end\nplans answer sha256 ${sum}\n`
      );
      return file;
    };
    const decode = (file: string, out: string) =>
      bash(`${defs("fail", "decode_plans_answer")}\ndecode_plans_answer "$1" "$2"`, [file, out]);
    const right = createHash("sha256").update(body).digest("hex");
    const kept = path.join(dir, "kept.json");
    expect(decode(log(right), kept)).toEqual({ code: 0, stdout: "", stderr: "" });
    expect(readFileSync(kept).equals(body)).toBe(true);
    const refused = path.join(dir, "refused.json");
    const wrong = decode(log("0".repeat(64)), refused);
    expect(wrong.code).toBe(1);
    expect(wrong.stderr).toContain(
      `has the sha256 ${right}, and the log printed ${"0".repeat(64)}`
    );
    expect(existsSync(refused)).toBe(false);
  });
});

/**
 * A stand-in of Oh My Pi in RPC mode for launch_rpc: it answers `prompt` and `get_state` as the
 * real one does, runs each turn as agent_start and agent_end, and writes to the daemon stand-in's
 * log (`$SKILL_SCENARIO_RUN/daemon.log`) the claim's claims/ready and the activity reports its plan
 * (FAKE_OMP_PLAN) gives, as the label's plugin does at the claim's boot and at session_stop. What it
 * saw and when goes to fake-omp.jsonl.
 */
const FAKE_OMP = String.raw`#!/usr/bin/env bash
if [ "$1" = --version ]; then echo omp/1.2.3; exit 0; fi
run=$SKILL_SCENARIO_RUN plan=$FAKE_OMP_PLAN
note() { jq -cn --argjson at "$(date +%s%3N)" --argjson e "$1" '{at: $at} + $e' >>"$run/fake-omp.jsonl"; }
now() { date -u +%Y-%m-%dT%H:%M:%S.%3NZ; }
seconds() { jq -rn --argjson ms "$1" '$ms / 1000'; }
report() {
  jq -cn --arg at "$(now)" --argjson r "$(jq -c ".reports[$1] // {running: false}" "$plan")" \
    '{at: $at, path: "/legion/v1/claims/activity", status: 204,
      body: {at: $at, backgroundWork: {running: $r.running, kinds: ($r.kinds // [])}, phase: {nextReminderAt: ($r.next // null)}}}' >>"$run/daemon.log"
  note "{\"report\": $1}"
}
turn() {
  : >"$run/fake-omp.streaming"
  echo '{"type":"agent_start"}'
  note "{\"agentStart\": \"$1\"}"
  [ "$(jq -r '.hang // false' "$plan")" = true ] && return
  sleep "$(seconds "$(jq '.turnMs // 200' "$plan")")"
  report 0
  rm -f "$run/fake-omp.streaming"
  echo '{"type":"agent_end"}'
  note "{\"agentEnd\": \"$1\"}"
  local i count
  count=$(jq '.reports // [] | length' "$plan")
  for ((i = 1; i < count; i++)); do
    (sleep "$(seconds "$(jq ".reports[$i].afterMs" "$plan")")" && report "$i") &
  done
}
(sleep "$(seconds "$(jq '.readyAfterMs // 500' "$plan")")" &&
  jq -cn --arg at "$(now)" '{at: $at, path: "/legion/v1/claims/ready", status: 204}' >>"$run/daemon.log" &&
  note '{"ready": true}') &
self=$(jq '.selfStartAfterMs // -1' "$plan")
if [ "$self" != -1 ]; then (sleep "$(seconds "$self")" && turn notice) & fi
turns=0
while IFS= read -r line; do
  id=$(jq -r .id <<<"$line")
  case $(jq -r .type <<<"$line") in
  prompt)
    note "$(jq -c '{prompt: .message}' <<<"$line")"
    if [ -e "$run/fake-omp.streaming" ]; then
      jq -cn --arg id "$id" '{type: "response", id: $id, command: "prompt", success: false}'
    else
      jq -cn --arg id "$id" '{type: "response", id: $id, command: "prompt", success: true}'
      turn "$turns" &
      turns=$((turns + 1))
    fi
    ;;
  get_state)
    streaming=false
    [ ! -e "$run/fake-omp.streaming" ] || streaming=true
    jq -cn --arg id "$id" --argjson s "$streaming" '{type: "response", id: $id, command: "get_state", success: true, data: {isStreaming: $s}}'
    ;;
  esac
done
note '{"stdinClosed": true}'
`;

interface FakeOmpPlan {
  readyAfterMs?: number;
  turnMs?: number;
  hang?: boolean;
  selfStartAfterMs?: number;
  reports?: { running: boolean; kinds?: string[]; next?: string | null; afterMs?: number }[];
}

/** launch_rpc BOUND START... -- THEN... on the fake Oh My Pi with PLAN, its rpc_poll 0.3 s, in a
 * run directory of its own; TRANSCRIPT, when given, is the session's transcript beforehand. */
function rpcRun(
  name: string,
  plan: FakeOmpPlan,
  starts: string[],
  thens: string[] = [],
  bound = 30,
  transcript?: object[]
) {
  const R = freshDir(name);
  const bin = path.join(R, "omp-install/bin");
  mkdirSync(bin, { recursive: true });
  writeFileSync(path.join(bin, "omp"), FAKE_OMP, { mode: 0o755 });
  for (const dir of ["logs", "sessions", "cwd"]) mkdirSync(path.join(R, dir));
  if (transcript !== undefined)
    writeFileSync(
      path.join(R, "sessions/session.jsonl"),
      transcript.map((entry) => `${JSON.stringify(entry)}\n`).join("")
    );
  writeFileSync(path.join(R, "plan.json"), JSON.stringify(plan));
  writeFileSync(path.join(R, "system-prompt.md"), "the label's system prompt\n");
  writeFileSync(
    path.join(R, "pane.env"),
    [
      `HOME=${R}`,
      `PATH=${callerPath}`,
      `SKILL_SCENARIO_RUN=${R}`,
      `FAKE_OMP_PLAN=${path.join(R, "plan.json")}`,
      "",
    ].join("\n")
  );
  const files = (kind: string, texts: string[]) =>
    texts.map((text, i) => {
      const file = path.join(R, `${kind}-${i + 1}.txt`);
      writeFileSync(file, text);
      return file;
    });
  const script = [
    "root=$1 R=$2 evidence=$2 work=$2/work timeout_hook=",
    defs("fail", "note"),
    '. "$root/scripts/e2e/lib/rig.sh"',
    defs(
      "pinned_omp",
      "rig_omp",
      "rpc_now",
      "rpc_send",
      "rpc_answer",
      "rpc_state",
      "rpc_turn",
      "rpc_activity",
      "rpc_settled",
      "rpc_ready",
      "rpc_prompt",
      "rpc_wait",
      "rpc_reminders",
      "rpc_bound",
      "rpc_end",
      "launch_rpc"
    ),
    assignment("rpc_ms"),
    "rpc_poll=0.3 rpc_ready_bound=10 omp_pid='' rpc_fd='' pin=github:example/oh-my-pi@1.2.3-4",
    "bound=$3",
    "shift 3",
    'launch_rpc "$bound" "$@"',
  ].join("\n");
  const result = bash(
    script,
    [root, R, String(bound), ...files("start", starts), "--", ...files("then", thens)],
    {
      SKILL_SCENARIOS_OMP: path.join(bin, "omp"),
    }
  );
  const events = jsonLines(path.join(R, "fake-omp.jsonl"));
  const at = (key: string, value: unknown) =>
    events.find((event) => event[key] === value)?.at as number | undefined;
  return {
    ...result,
    R,
    events,
    at,
    prompts: events.filter((event) => "prompt" in event),
    sent: jsonLines(path.join(R, "rpc-sent.jsonl")).map(
      (line): Record<string, unknown> & { ms: number } => ({
        ...(line.sent as Record<string, unknown>),
        ms: line.ms as number,
      })
    ),
    out: existsSync(path.join(R, "out.txt")) ? readFileSync(path.join(R, "out.txt"), "utf8") : "",
  };
}

describe("launch_rpc", () => {
  // Catches: a task sent before the claim is ready (the plugin would take it unclaimed), sent
  // twice, or changed on the way (quoting, a trailing newline dropped).
  test("launch_rpc sends the start task unchanged as one RPC prompt, after claims/ready", () => {
    const task =
      'Task: rig-marker-7f3.\nIt holds "quotes", a $HOME, a back\\slash and a tab\there.\n';
    const run = rpcRun("rpc-start", { readyAfterMs: 1500, reports: [{ running: false }] }, [task]);
    expect({ code: run.code, stderr: run.code === 0 ? "" : run.stderr }).toEqual({
      code: 0,
      stderr: "",
    });
    expect(run.out).toBe("exit=0\n");
    expect(run.prompts.map((event) => event.prompt)).toEqual([task]);
    expect(
      run.sent.filter((command) => command.type === "prompt").map((command) => command.message)
    ).toEqual([task]);
    expect(run.at("ready", true)).toBeLessThan(run.prompts[0]?.at as number);
    expect(jsonLines(path.join(run.R, "rpc.jsonl")).length).toBeGreaterThan(run.sent.length);
  });

  // Catches: a run ended while the session's own job runs: in RPC mode that job's end starts a
  // new turn, which the run would cut off.
  test("launch_rpc does not end a run whose activity report says background work runs, and ends it once the report says none", () => {
    const run = rpcRun(
      "rpc-background",
      {
        reports: [
          { running: true, kinds: ["bash"] },
          { running: false, afterMs: 2500 },
        ],
      },
      ["Do the work.\n"]
    );
    expect({ code: run.code, stderr: run.code === 0 ? "" : run.stderr }).toEqual({
      code: 0,
      stderr: "",
    });
    expect(run.out).toBe("exit=0\n");
    const none = run.at("report", 1) as number;
    expect(run.sent.some((command) => command.type === "get_state" && command.ms < none)).toBe(
      true
    );
    expect(run.at("stdinClosed", true)).toBeGreaterThanOrEqual(none);
  });

  // Catches: a phase worker's run ended while Legion's reminder is armed, which would start one
  // more turn the run never sees.
  test("launch_rpc does not end a phase worker's run while its activity report names a reminder armed, and ends it once its phase closed", () => {
    const run = rpcRun(
      "rpc-reminder",
      {
        reports: [
          { running: false, next: "2026-11-24T23:00:00.000Z" },
          { running: false, next: null, afterMs: 2500 },
        ],
      },
      ["Do the work.\n"]
    );
    expect({ code: run.code, stderr: run.code === 0 ? "" : run.stderr }).toEqual({
      code: 0,
      stderr: "",
    });
    expect(run.out).toBe("exit=0\n");
    const closed = run.at("report", 1) as number;
    expect(run.sent.some((command) => command.type === "get_state" && command.ms < closed)).toBe(
      true
    );
    expect(run.at("stdinClosed", true)).toBeGreaterThanOrEqual(closed);
  });

  // Catches: an owner's run held past its settle, or its questions sent before its turn ended.
  test("an owner's run ends at its first settle with no background work, since its report names no reminder", () => {
    const run = rpcRun(
      "rpc-owner",
      { reports: [{ running: false, next: null }] },
      ["Plan the issue.\n"],
      ["Which step do you add?\n"]
    );
    expect({ code: run.code, stderr: run.code === 0 ? "" : run.stderr }).toEqual({
      code: 0,
      stderr: "",
    });
    expect(run.out).toBe("exit=0\n");
    expect(run.prompts.map((event) => event.prompt)).toEqual([
      "Plan the issue.\n",
      "Which step do you add?\n",
    ]);
    expect(run.prompts[1]?.at).toBeGreaterThan(run.at("agentEnd", "0") as number);
    expect(
      (run.at("stdinClosed", true) as number) - (run.at("agentEnd", "1") as number)
    ).toBeLessThan(3000);
  });

  // Catches: a run with no start task (a child owner, whom the catch-up notice alone starts) that
  // sends its questions before the notice's turn, or ends before it.
  test("launch_rpc with no start task waits for the turn a notice starts, and sends its questions once that turn ends", () => {
    const run = rpcRun(
      "rpc-notice",
      { selfStartAfterMs: 2000, reports: [{ running: false }] },
      [],
      ["Which step do you add?\n"]
    );
    expect({ code: run.code, stderr: run.code === 0 ? "" : run.stderr }).toEqual({
      code: 0,
      stderr: "",
    });
    expect(run.out).toBe("exit=0\n");
    expect(run.prompts.map((event) => event.prompt)).toEqual(["Which step do you add?\n"]);
    expect(run.prompts[0]?.at).toBeGreaterThan(run.at("agentEnd", "notice") as number);
  });

  // Catches: a bound that reads as an end, or a held run whose row cannot say what held it.
  test("launch_rpc writes exit=bound at the run's bound, and the row names what held it: the reminder and the count of reminders, the background work, or the turn", () => {
    const held = (name: string, plan: FakeOmpPlan, transcript?: object[]) => {
      const run = rpcRun(name, plan, ["Do the work.\n"], [], 2, transcript);
      const file = path.join(run.R, "held.txt");
      return {
        code: run.code,
        out: run.out,
        held: existsSync(file) ? readFileSync(file, "utf8") : "no held.txt",
      };
    };
    const bound = "the run did not end within 2 seconds";
    expect(held("bound-turn", { hang: true })).toEqual({
      code: 0,
      out: "exit=bound\n",
      held: `${bound}: a turn running\n`,
    });
    expect(held("bound-work", { reports: [{ running: true, kinds: ["bash", "task"] }] })).toEqual({
      code: 0,
      out: "exit=bound\n",
      held: `${bound}: background work running: bash, task\n`,
    });
    const reminder = { type: "custom_message", customType: "legion-phase-reminder" };
    expect(
      held("bound-reminder", { reports: [{ running: false, next: "2026-11-24T23:05:00.000Z" }] }, [
        reminder,
        { type: "message" },
        reminder,
      ])
    ).toEqual({
      code: 0,
      out: "exit=bound\n",
      held: `${bound}: a reminder armed for 2026-11-24T23:05:00.000Z, after 2 reminders\n`,
    });
  }, 120_000);
});

describe("the model line of a stand-in label", () => {
  // Catches: a try whose one run is a rig error for its model, so it scores nothing.
  test("with SKILL_SCENARIOS_MODEL=standin, the label's built-from names the stand-in as its model line, so the try's run passes the model check and is no rig error", () => {
    const dir = freshDir("standin-model-line");
    const line = bash(
      `${defs("route_model_line")}\n${assignment("model_standin")}\nroute_model_line`,
      [],
      { SKILL_SCENARIOS_MODEL: "standin" }
    );
    expect(line).toEqual({ code: 0, stdout: "model standin/standin-model\n", stderr: "" });
    // A transcript of a turn the stand-in answered, in the fields transcriptModels reads.
    const entries = [
      { type: "model_change", model: "standin/standin-model" },
      {
        type: "message",
        message: {
          role: "assistant",
          provider: "standin",
          model: "standin-model",
          content: [{ type: "text", text: "stand-in" }],
        },
      },
    ] as unknown as Parameters<typeof modelProblem>[1];
    writeFileSync(
      path.join(dir, "built-from"),
      `checkout /rig/label\ncommit ${"a".repeat(40)}\n${line.stdout}`
    );
    expect(modelProblem(dir, entries)).toBeUndefined();
    // Control: a built-from that names the gateway's model makes the same run a rig error.
    writeFileSync(
      path.join(dir, "built-from"),
      `checkout /rig/label\ncommit ${"a".repeat(40)}\nmodel gateway/claude-opus-5-5\n`
    );
    expect(modelProblem(dir, entries)).toBe(
      "its transcript names the model standin/standin-model, and its label's built-from names gateway/claude-opus-5-5"
    );
  });

  const omp = process.env.LEGION_TEST_OMP;
  const skipOmp = omp === undefined && process.env.GITHUB_ACTIONS !== "true";
  if (skipOmp)
    console.error(
      "UNKNOWN: rig.test.ts's model stand-in row runs the real Oh My Pi and needs LEGION_TEST_OMP (the runner's --omp sets it); it does not run here"
    );

  // Catches: a stand-in route that reaches no stand-in (the turn goes nowhere, or to a real
  // provider), and a transcript that names another model than the one built-from holds.
  test.skipIf(skipOmp)(
    "the model stand-in mode routes the provider to model-standin.ts, and the transcript holds its fixed message",
    async () => {
      if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
      // Imported here, not at the file's top: a rig without the stand-in then fails this row
      // alone, as each row here fails alone for a function rig.sh lacks.
      const { MODEL_STANDIN_MESSAGE } = await import("./model-standin");
      const dir = freshDir("standin-route");
      const home = path.join(dir, "home");
      const profile = "skills-standin";
      const fns = `${defs("fail", "install_standin_route", "route_config", "route_model_standin", "route_model_line")}\n${assignment("model_standin")}\n${assignment("model_standin_unrouted")}`;
      const installed = bash(
        `${fns}\ninstall_standin_route "$1" "$2" && route_model_line >"$3/built-from"`,
        [profile, home, dir],
        {
          SKILL_SCENARIOS_MODEL: "standin",
        }
      );
      expect({ code: installed.code, stderr: installed.stderr }).toEqual({ code: 0, stderr: "" });
      const log = path.join(dir, "model-standin.jsonl");
      const server = Bun.spawn([process.execPath, path.join(scenarios, "model-standin.ts"), log], {
        stdout: "pipe",
        stderr: "pipe",
      });
      try {
        const reader = server.stdout.getReader();
        let printed = "";
        while (!printed.includes("\n")) {
          const { value, done } = await reader.read();
          if (done) break;
          printed += new TextDecoder().decode(value);
        }
        const base = /^listening on (\S+)/.exec(printed)?.[1] ?? "";
        expect(base).toStartWith("http://");
        const routed = bash(`${fns}\nroute_model_standin "$1" "$2" "$3"`, [home, profile, base]);
        expect({ code: routed.code, stderr: routed.stderr }).toEqual({ code: 0, stderr: "" });
        const sessions = path.join(dir, "sessions");
        const workspace = path.join(dir, "cwd");
        mkdirSync(sessions);
        mkdirSync(workspace);
        const turn = Bun.spawnSync(
          [
            omp,
            "-p",
            "Say hello.",
            "--no-extensions",
            "--no-skills",
            "--no-rules",
            "--no-lsp",
            "--no-title",
            "--session-dir",
            sessions,
          ],
          {
            cwd: workspace,
            env: {
              HOME: home,
              OMP_PROFILE: profile,
              PATH: "/usr/local/bin:/usr/bin:/bin",
              PI_NOTIFICATIONS: "off",
              PI_NO_TITLE: "1",
            },
            timeout: 120_000,
          }
        );
        expect({
          code: turn.exitCode,
          stderr: turn.exitCode === 0 ? "" : turn.stderr.toString(),
        }).toEqual({ code: 0, stderr: "" });
        const entries = (readdirSync(sessions, { recursive: true }) as string[])
          .filter((file) => file.endsWith(".jsonl"))
          .flatMap((file) => jsonLines(path.join(sessions, file))) as unknown as Parameters<
          typeof modelProblem
        >[1];
        const said = (
          entries as unknown as {
            message?: { role?: string; content?: { type?: string; text?: string }[] };
          }[]
        )
          .filter((entry) => entry.message?.role === "assistant")
          .flatMap((entry) =>
            (entry.message?.content ?? [])
              .filter((part) => part.type === "text")
              .map((part) => part.text)
          );
        expect(said).toContain(MODEL_STANDIN_MESSAGE);
        expect(transcriptModels(entries)).toEqual(["standin/standin-model"]);
        expect(modelProblem(dir, entries)).toBeUndefined();
        expect(readFileSync(log, "utf8")).toContain('"path":"/anthropic/v1/messages","answered":1');
      } finally {
        server.kill();
        await server.exited;
      }
      // Control: the same build without the variable takes the route LEGION_E2E_MODEL_ROUTE
      // names, here the gateway's provider block, and its model line names that block.
      const key = path.join(dir, "model-token");
      writeFileSync(key, "#!/bin/sh\necho token\n", { mode: 0o755 });
      const providers = path.join(dir, "providers.yml");
      const block = {
        baseUrl: "http://gateway.example.invalid:4000",
        api: "anthropic-messages",
        apiKey: `!${key}`,
        authHeader: true,
        models: [{ id: "claude-opus-5-5" }],
      };
      writeFileSync(providers, JSON.stringify({ providers: { gateway: block } }));
      const gatewayEnv = {
        LEGION_E2E_MODEL_ROUTE: "provider",
        LEGION_E2E_MODEL_PROVIDER_FILE: providers,
        LEGION_E2E_MODEL_PROVIDER: "gateway",
      };
      const gateway = profileDir();
      const viaGateway = route(gateway, gatewayEnv, "claude-opus-5-5");
      expect({
        code: viaGateway.code,
        stderr: viaGateway.code === 0 ? "" : viaGateway.stderr,
      }).toEqual({ code: 0, stderr: "" });
      const models = JSON.parse(
        readFileSync(path.join(gateway, "home/.omp/profiles/skills-test/agent/models.yml"), "utf8")
      );
      expect(Object.keys(models.providers)).toEqual(["gateway"]);
      expect(
        bash(`${fns}\nroute_model_line`, [], {
          ...gatewayEnv,
          LEGION_E2E_MODEL_PROVIDER_MODEL: "claude-opus-5-5",
        }).stdout
      ).toBe("model gateway/claude-opus-5-5\n");
    },
    180_000
  );
});

describe("the reach lines", () => {
  // Catches: a reach entry a Job's log leaves out, or a line printed for a run that reached
  // nothing. scored-job.sh prints its reach lines with `rig.sh rows`, and `rig.sh score` with the
  // same cmd_rows.
  test("scored-job.sh and rig.sh score print a reach line for each entry of a planted run's reach, and none for a run whose reach is empty", () => {
    const work = freshDir("reach-rows");
    const plantRow = (name: string, row: object) => {
      mkdirSync(path.join(work, "runs", name), { recursive: true });
      writeFileSync(path.join(work, "runs", name, "row.json"), JSON.stringify(row));
    };
    const scored = (run: string, reach: object[]) => ({
      outcome: "scored",
      run,
      scenario: "owner-walk",
      label: "head",
      case: "root-B-claude",
      pass: false,
      clauses: [{ id: "c", clause: "c", pass: false, evidence: "e", counts: {} }],
      reach,
    });
    plantRow(
      "owner-walk-root-B-claude-head-1",
      scored("owner-walk-root-B-claude-head-1", [
        { session: "ses-a", message: 4, tool: "bash", host: "metadata.google.internal" },
        { session: "ses-b", message: 9, tool: "fetch", host: "storage.googleapis.com" },
      ])
    );
    plantRow("owner-walk-root-B-claude-head-2", scored("owner-walk-root-B-claude-head-2", []));
    const want = [
      "reach owner-walk-root-B-claude-head-1: bash names metadata.google.internal (session ses-a, message 4)",
      "reach owner-walk-root-B-claude-head-1: fetch names storage.googleapis.com (session ses-b, message 9)",
    ];
    const reachOf = (out: string) => out.split("\n").filter((line) => line.startsWith("reach "));
    // `rig.sh rows`, the command scored-job.sh runs.
    const rows = childSpawn(["bash", path.join(scenarios, "rig.sh"), "rows", "owner-walk"], {
      SKILL_SCENARIOS_WORK: work,
    });
    expect(rows.exitCode).toBe(0);
    expect(reachOf(rows.stdout.toString())).toEqual(want);
    expect(
      rows.stdout
        .toString()
        .split("\n")
        .filter((line) => line.startsWith("row "))
    ).toHaveLength(2);
    // rig.sh score's cmd_score, with the scoring and the cells, which read a whole run, stood in.
    const score = bash(
      `${defs("note", "fail", "cmd_rows", "cmd_score")}\nscore_scored_run() { :; }\nbun() { :; }\nwork=$1 here=$2\ncmd_score owner-walk`,
      [work, scenarios]
    );
    expect(score.code).toBe(0);
    expect(reachOf(score.stdout)).toEqual(want);
  });
});

describe("scored-job.sh", () => {
  /** A repository holding a copy of scored-job.sh beside a planted rig.sh, and a planted bun on
   * PATH that answers `install` and `owner-walk.ts cases`. The planted rig.sh records each call and
   * the variables it was handed, and plays one scored run and one rig error of the case. */
  function plantedJob() {
    const repo = freshDir("job-repo");
    const here = path.join(repo, "packages/pi-legion/scripts/skill-scenarios");
    mkdirSync(here, { recursive: true });
    cpSync(path.join(scenarios, "scored-job.sh"), path.join(here, "scored-job.sh"));
    writeFileSync(
      path.join(here, "rig.sh"),
      String.raw`#!/usr/bin/env bash
work=$SKILL_SCENARIOS_WORK
printf '%s\n' "$*" >>"$work/rig-calls"
case $1 in
batch-scored)
  printf 'case=%s model=%s route=%s provider=%s pg=%s nats=%s omp=%s plans=%s\n' "$SKILL_SCENARIOS_OWNER_WALK_CASE" "$SKILL_SCENARIOS_MODEL" \
    "$LEGION_E2E_MODEL_ROUTE" "$LEGION_E2E_MODEL_PROVIDER" "$SKILL_SCENARIOS_POSTGRES_URL" "$SKILL_SCENARIOS_NATS_URL" "$SKILL_SCENARIOS_OMP" "$SKILL_SCENARIOS_PLANS_ANSWER" >>"$work/rig-env"
  mkdir -p "$work/runs/owner-walk-$SKILL_SCENARIOS_OWNER_WALK_CASE-$4-1" "$work/runs/owner-walk-$SKILL_SCENARIOS_OWNER_WALK_CASE-$4-2/logs"
  echo '{"outcome":"scored"}' >"$work/runs/owner-walk-$SKILL_SCENARIOS_OWNER_WALK_CASE-$4-1/row.json"
  echo '{"outcome":"rig-error"}' >"$work/runs/owner-walk-$SKILL_SCENARIOS_OWNER_WALK_CASE-$4-2/row.json"
  echo 'the planted daemon line' >"$work/runs/owner-walk-$SKILL_SCENARIOS_OWNER_WALK_CASE-$4-2/logs/daemon.log"
  ;;
rows) echo 'row {"outcome":"scored","run":"owner-walk-root-B-claude-head-1"}' && echo 'reach owner-walk-root-B-claude-head-1: bash names storage.googleapis.com (session s, message 1)' ;;
cell) echo "cell owner-walk head root-B-claude: 2 runs, 1 scored, 1 rig errors, count $5; each clause: read 0 of 1; runs passed 0 of 1; verdict fail" ;;
esac
`
    );
    git(repo, "init", "-q");
    git(repo, "add", "-A");
    git(repo, "commit", "-qm", "the job");
    const bin = freshDir("job-bin");
    writeFileSync(
      path.join(bin, "bun"),
      String.raw`#!/bin/sh
case "$1 $2" in
"install --frozen-lockfile") exit 0 ;;
*"/owner-walk.ts cases") printf 'root-A-claude 3\nroot-B-claude 3\nchild-B-claude 3\n' ;;
*) echo "the planted bun has no answer to $*" >&2; exit 9 ;;
esac
`,
      { mode: 0o755 }
    );
    const work = freshDir("job-work");
    const run = (env: Record<string, string>) => {
      const result = childSpawn(
        [
          "bash",
          path.join(here, "scored-job.sh"),
          "owner-walk",
          "head",
          "--case",
          "root-B-claude",
          "--count",
          "1",
        ],
        { PATH: `${bin}:${callerPath}`, SKILL_SCENARIOS_WORK: work, ...env }
      );
      const read = (file: string) =>
        existsSync(path.join(work, file)) ? readFileSync(path.join(work, file), "utf8") : "";
      return {
        code: result.exitCode,
        stdout: result.stdout.toString(),
        stderr: result.stderr.toString(),
        calls: read("rig-calls"),
        env: read("rig-env"),
      };
    };
    return { repo: realpathSync(repo), work, run };
  }
  const pod = {
    LEGION_TEST_PG_DSN: "postgres://ci@127.0.0.1:5432/postgres",
    LEGION_TEST_NATS_URL: "nats://127.0.0.1:4222",
    LEGION_TEST_OMP: "/work/omp/bin/omp",
  };

  // Catches: a Job that skips a step, runs them out of order, hands the rig another case, store or
  // Oh My Pi than the pod's, hides a rig error's logs, or ends on a line that is not its cell's.
  test("scored-job.sh owner-walk head --case root-B-claude --count 1 with the model stand-in calls the rig's steps in order and prints each row and the cell's last line", () => {
    const { repo, work, run } = plantedJob();
    const job = run({ ...pod, SKILL_SCENARIOS_MODEL: "standin" });
    expect({ code: job.code, stderr: job.stderr }).toEqual({ code: 0, stderr: "" });
    expect(job.calls).toBe(
      [
        `plans-capture ${repo} ${work}/plans`,
        `profile head ${repo}`,
        "batch-scored owner-walk 1 head",
        "rows owner-walk",
        "cell owner-walk head root-B-claude 1",
        "",
      ].join("\n")
    );
    expect(job.env).toBe(
      `case=root-B-claude model=standin route= provider= pg=${pod.LEGION_TEST_PG_DSN} nats=${pod.LEGION_TEST_NATS_URL} omp=${pod.LEGION_TEST_OMP} plans=${work}/plans\n`
    );
    const lines = job.stdout.trim().split("\n");
    expect(lines).toContain("scored-job: the model: the model stand-in (model-standin.ts)");
    expect(lines).toContain('row {"outcome":"scored","run":"owner-walk-root-B-claude-head-1"}');
    expect(lines).toContain(
      "reach owner-walk-root-B-claude-head-1: bash names storage.googleapis.com (session s, message 1)"
    );
    const tail = lines.indexOf(
      "scored-job: rig error owner-walk-root-B-claude-head-2: the last lines of runs/owner-walk-root-B-claude-head-2/logs/daemon.log"
    );
    expect(lines[tail + 1]).toBe("  the planted daemon line");
    expect(lines.at(-1)).toBe(
      "scored-job owner-walk head root-B-claude, label head: 2 runs, 1 scored, 1 rig errors, count 1; each clause: read 0 of 1; runs passed 0 of 1; verdict fail"
    );
  });

  // Catches: a Job with no stand-in that runs on whatever route the pod gives, or with no gateway
  // it was told of. Its control: the same line without the variable names the gateway's block.
  test("scored-job.sh without the model stand-in takes the gateway's block of its family, and refuses to start without the gateway's root", () => {
    const refused = plantedJob().run(pod);
    expect({ code: refused.code, calls: refused.calls }).toEqual({ code: 2, calls: "" });
    expect(refused.stderr).toContain("SKILL_SCENARIOS_GATEWAY_URL is unset");
    const { work, run } = plantedJob();
    // The key command the runner's --model-gateway writes, scored-job.sh's default.
    const modelToken = "!/work/bin/model-token";
    const job = run({
      ...pod,
      SKILL_SCENARIOS_GATEWAY_URL: "http://gateway.example.invalid:4000/",
      SKILL_SCENARIOS_MODEL_ID: "claude-opus-5-5",
    });
    expect({ code: job.code, stderr: job.stderr }).toEqual({ code: 0, stderr: "" });
    expect(job.env).toContain("model= route=provider provider=gateway ");
    expect(JSON.parse(readFileSync(path.join(work, "providers.yml"), "utf8"))).toEqual({
      providers: {
        gateway: {
          baseUrl: "http://gateway.example.invalid:4000",
          api: "anthropic-messages",
          apiKey: modelToken,
          authHeader: true,
          models: [{ id: "claude-opus-5-5" }],
        },
      },
    });
  });
});

/** The head a made label's daemon entry reports in ready.json. */
const madeHead = "a".repeat(40);
/**
 * label-texts.sh of a made label. `daemon <request> <ready> <status> <record>` plays the plan in
 * <runDir>/stub-plan.json, its runDir read from the request: ready.json once `readyAfterMs` has
 * passed (never when it is null), with a transcript and a subagent's under its sessionDir; each
 * status of `statuses` at its `afterMs`; and at SIGTERM the plan's `record`, the PASS line and the
 * plan's `exit`. It records its arguments (stub-argv), its LEGION_TEST_PG_DSN (stub-pg) and each
 * event with its epoch milliseconds (stub-events.jsonl). `build [--compile]` records its arguments
 * in $RIG_TEXTS_BIN/build-calls.
 */
const STUB_ENTRY = String.raw`#!/usr/bin/env bash
set -u
case ${"${1:-}"} in
build) mkdir -p "$RIG_TEXTS_BIN" && printf '%s\n' "$*" >>"$RIG_TEXTS_BIN/build-calls" && exit 0 ;;
daemon) ;;
*) exit 2 ;;
esac
request=$2 ready=$3 status=$4 record=$5
run=$(jq -r .runDir "$request")
plan=$run/stub-plan.json
event() { jq -cn --argjson at "$(date +%s%3N)" --arg e "$1" '{at: $at, event: $e}' >>"$run/stub-events.jsonl"; }
printf '%s\n' "$*" >"$run/stub-argv"
printf '%s\n' "${"${LEGION_TEST_PG_DSN:-}"}" >"$run/stub-pg"
term=0
trap 'term=1' TERM
start=$(date +%s%3N)
ready_at=$(jq '.readyAfterMs // 300' "$plan")
statuses=$(jq -c '.statuses // []' "$plan")
count=$(jq length <<<"$statuses")
next=0 wrote=0
sessions=$(jq -r .home "$request")/.omp/agent/sessions/rig
event start
while [ "$term" = 0 ]; do
  now=$(($(date +%s%3N) - start))
  if [ "$wrote" = 0 ] && [ "$ready_at" != null ] && [ "$now" -ge "$ready_at" ]; then
    mkdir -p "$sessions/ses_pane"
    printf '%s\n' '{"type":"session","id":"ses_pane"}' >"$sessions/ses_pane.jsonl"
    printf '%s\n' '{"type":"session","id":"sub"}' >"$sessions/ses_pane/sub.jsonl"
    jq -n --arg sessions "$sessions" --arg head "${madeHead}" --argjson extra "$(jq -c '.ready // {}' "$plan")" \
      '{daemonUrl: "http://127.0.0.1:1", claim: "c-implementer", session: "ses_pane", replaced: "ses_fake",
        generation: 2, head: $head, systemPrompt: "/rig/system.md", sessionDir: $sessions} + $extra' >"$ready.tmp"
    mv "$ready.tmp" "$ready"
    wrote=1
    event ready
  fi
  while [ "$next" -lt "$count" ] && [ "$now" -ge "$(jq ".[$next].afterMs" <<<"$statuses")" ]; do
    jq -c ".[$next].status" <<<"$statuses" >"$status.tmp"
    mv "$status.tmp" "$status"
    event "status $next"
    next=$((next + 1))
  done
  sleep 0.05 &
  wait $!
done
event term
jq '.record // {issue: "ACME-1", phase: "merging", generation: 2, calls: [], defects: [], steps: []}' "$plan" >"$record"
echo '--- PASS: TestRigDaemonLive (1.00s)'
exit "$(jq '.exit // 0' "$plan")"
`;

/** The files of a made world with `daemon: label`, W1's shape, which the rig takes. */
const madeWorld = (fields: Record<string, unknown> = {}): Record<string, string> => ({
  "world.json": JSON.stringify({
    daemon: "label",
    scenario: "forward-only",
    world: "retro-defect",
    role: "implementer",
    moment: "retro",
    issue: "ACME-1",
    title: "List rows a page at a time",
    steps: ["implementer", "reviewer", "acceptance:owner"],
    repository: "acme/widgets",
    defaultBranch: "main",
    pull: 41,
    passes: ["c-list-rows"],
    item: 1,
    ...fields,
  }),
  "anchors.json": JSON.stringify({ defect: ["src/config.ts", "loadConfig"] }),
  "spec.md": "# List rows a page at a time\n\n`listRows(rows, limit)` returns at most `limit` rows.\n",
  "verification.md":
    '# Verification\n\n:::check{#c-list-rows surface="cli" run="bun test" when="before_merge"}\nThe list test passes.\n:::\n',
  "fixtures.json": '{"routes":[]}\n',
  "head.patch": "",
});
/** A status file the stand-in entry writes: the turn, and the newest activity report after it. */
const status = (turn: "running" | "idle", activity: object | null) => ({
  at: "2026-11-26T00:00:00.000Z",
  paneAlive: true,
  turn,
  activity,
});
const ended = status("idle", { backgroundWork: { running: false }, phase: { nextReminderAt: null } });

/**
 * run_forward on a made label whose entry is STUB_ENTRY, playing PLAN, in a run directory of its
 * own, with own_dispatch, seed.ts and the Dispatch read of the results stood in (each call goes to
 * <R>/rig-calls): RESULTS_MISSING, when set, is the backstop's answer of a check with no pass. The
 * Oh My Pi that rig_omp names records any run but `--version` in <work>/omp-ran. WORLD's files go
 * over madeWorld's. MODEL_LINE, when given, is the built-from's model line, and the label's profile
 * then holds the stand-in route.
 */
function labelRun(
  name: string,
  plan: object,
  options: {
    world?: Record<string, string>;
    env?: Record<string, string>;
    readyBound?: number;
    modelLine?: string;
  } = {}
) {
  const R = freshDir(name);
  const co = freshDir(`${name}-label`);
  const work = freshDir(`${name}-work`);
  const worlds = freshDir(`${name}-worlds`);
  const entry = path.join(co, "packages/pi-legion/scripts/skill-scenarios/label-texts.sh");
  mkdirSync(path.dirname(entry), { recursive: true });
  writeFileSync(entry, STUB_ENTRY, { mode: 0o755 });
  mkdirSync(path.join(work, "profiles/head/home/.omp/profiles/skills-head/agent"), { recursive: true });
  const world = path.join(worlds, "forward-only/retro-defect");
  mkdirSync(world, { recursive: true });
  for (const [file, text] of Object.entries({ ...madeWorld(), ...options.world }))
    writeFileSync(path.join(world, file), text);
  for (const dir of ["bin", "sessions", "cwd", "logs"]) mkdirSync(path.join(R, dir));
  writeFileSync(path.join(R, "stub-plan.json"), JSON.stringify(plan));
  writeFileSync(
    path.join(R, "built-from"),
    `checkout ${co}\ncommit ${"b".repeat(40)}\n${options.modelLine === undefined ? "" : `${options.modelLine}\n`}`
  );
  writeFileSync(
    path.join(work, "services-env"),
    "ENVOY_URL=http://127.0.0.1:1\nENVOY_NATS_URL=nats://127.0.0.1:2\nENVOY_TOKEN_FILE=/rig/envoy-token\n"
  );
  const omp = path.join(work, "omp-install/bin/omp");
  mkdirSync(path.dirname(omp), { recursive: true });
  writeFileSync(
    omp,
    `#!/bin/sh\n[ "$1" = --version ] && { echo omp/1.2.3; exit 0; }\ntouch ${JSON.stringify(path.join(work, "omp-ran"))}\n`,
    { mode: 0o755 }
  );
  const script = [
    "set -euo pipefail",
    "root=$1 R=$2 co=$3 work=$4 forward_worlds=$5 here=$6 evidence=$2 timeout_hook=",
    "P=$work/profiles/head services_env=$work/services-env profile=skills-head pin=github:example/oh-my-pi@1.2.3-4",
    `rpc_poll=0.2 rpc_ready_bound=${options.readyBound ?? 10} entry_pid='' model_pid='' dispatch_port=1 pg_url=''`,
    defs("fail", "note"),
    '. "$root/scripts/e2e/lib/rig.sh"',
    defs(
      "require_label_texts",
      "base_env",
      "standins",
      "pod_standins",
      "pinned_omp",
      "rig_omp",
      "service_value",
      "install_standin_route",
      "route_config",
      "route_model_standin",
      "forward_holds",
      "daemon_request",
      "forward_ready",
      "forward_script",
      "forward_status",
      "forward_held",
      "forward_wait",
      "forward_end",
      "run_forward"
    ),
    assignment("model_standin"),
    assignment("model_standin_unrouted"),
    assignment("path_check_defect"),
    'own_dispatch() { printf "own_dispatch %s\\n" "$*" >>"$R/rig-calls"; pg_url=postgres://rig@127.0.0.1:1/postgres; }',
    'seed_ts() { printf "seed %s\\n" "$*" >>"$R/rig-calls"; }',
    'forward_results() { printf "results %s\\n" "$1" >>"$R/rig-calls"; [ -z "${RESULTS_MISSING:-}" ] || { printf "%s\\n" "$RESULTS_MISSING"; return 1; }; }',
    `trap 'for pid in "$entry_pid" "$model_pid"; do [ -z "$pid" ] || kill -KILL "$pid" 2>/dev/null || true; done' EXIT`,
    options.modelLine === undefined ? "" : 'install_standin_route "$profile" "$P/home"',
    "run_forward forward-only-retro-defect-head-1 forward-only retro-defect",
  ].join("\n");
  const result = bash(script, [root, R, co, work, worlds, scenarios], {
    USER: "rig",
    SKILL_SCENARIOS_OMP: omp,
    ...options.env,
  });
  const read = (file: string) =>
    existsSync(path.join(R, file)) ? readFileSync(path.join(R, file), "utf8") : "";
  return {
    ...result,
    R,
    co,
    work,
    omp,
    world,
    read,
    events: jsonLines(path.join(R, "stub-events.jsonl")) as { at: number; event: string }[],
    calls: read("rig-calls"),
  };
}

/** The forward branch of scored-job.sh with a planted rig.sh beside it, which records each call and
 * the forward variables batch-scored is handed, plants one scored run of each world it batches,
 * fails label-tree when PLANT_TREE_FAIL is set, and answers forward-cell with a planted line; and
 * a planted bun that answers `install` and `score.ts path-check`. */
function plantedForwardJob() {
  const repo = freshDir("forward-job-repo");
  const here = path.join(repo, "packages/pi-legion/scripts/skill-scenarios");
  mkdirSync(here, { recursive: true });
  cpSync(path.join(scenarios, "scored-job.sh"), path.join(here, "scored-job.sh"));
  writeFileSync(
    path.join(here, "rig.sh"),
    String.raw`#!/usr/bin/env bash
work=$SKILL_SCENARIOS_WORK
printf '%s\n' "$*" >>"$work/rig-calls"
case $1 in
label-tree) [ -z "${"${PLANT_TREE_FAIL:-}"}" ] || { echo "the manifest $4 does not exist" >&2; exit 1; } ;;
batch-scored)
  printf 'world=%s check=%s control=%s model=%s\n' "$SKILL_SCENARIOS_FORWARD_WORLD" "${"${SKILL_SCENARIOS_PATH_CHECK:-}"}" \
    "${"${SKILL_SCENARIOS_PATH_CONTROL:-}"}" "${"${SKILL_SCENARIOS_MODEL:-}"}" >>"$work/rig-env"
  mkdir -p "$work/runs/$2-$SKILL_SCENARIOS_FORWARD_WORLD-$4-1"
  echo '{"outcome":"scored"}' >"$work/runs/$2-$SKILL_SCENARIOS_FORWARD_WORLD-$4-1/row.json"
  ;;
forward-cell) echo "CONTROL BIT: $3 $4 1/5, 4 failed runs noticed" && echo "head Job of $4: may start" ;;
esac
`
  );
  git(repo, "init", "-q");
  git(repo, "add", "-A");
  git(repo, "commit", "-qm", "the job");
  const bin = freshDir("forward-job-bin");
  writeFileSync(
    path.join(bin, "bun"),
    String.raw`#!/bin/sh
case "$1 $2" in
"install --frozen-lockfile") exit 0 ;;
*"/score.ts path-check") echo "PATH OK d-planted" ;;
*) echo "the planted bun has no answer to $*" >&2; exit 9 ;;
esac
`,
    { mode: 0o755 }
  );
  const work = freshDir("forward-job-work");
  const run = (args: string[], env: Record<string, string> = {}) => {
    const result = childSpawn(["bash", path.join(here, "scored-job.sh"), ...args], {
      PATH: `${bin}:${callerPath}`,
      SKILL_SCENARIOS_WORK: work,
      ...env,
    });
    const read = (file: string) =>
      existsSync(path.join(work, file)) ? readFileSync(path.join(work, file), "utf8") : "";
    return {
      code: result.exitCode,
      stdout: result.stdout.toString(),
      stderr: result.stderr.toString(),
      calls: read("rig-calls"),
      env: read("rig-env"),
    };
  };
  return { repo: realpathSync(repo), work, run };
}

// A world whose world.json names `daemon: label` runs its agent as a real pane of the label's whole
// daemon (C4 of dispatch://INFRA-5581's plan): the rig runs the label's `label-texts.sh daemon`,
// and starts neither the daemon stand-in nor an Oh My Pi of its own.
describe("a world with daemon: label", () => {
  // Catches: a label-daemon run that still starts daemon-standin.ts or Oh My Pi in RPC mode, so the
  // agent's calls reach a stand-in and not the label's daemon, and a request that names another
  // run's files.
  test("a world with daemon: label starts no daemon-standin and no Oh My Pi", () => {
    const run = labelRun("label-world", { statuses: [{ afterMs: 500, status: ended }] });
    expect({ code: run.code, stderr: run.stderr }).toEqual({ code: 0, stderr: "" });
    expect(existsSync(path.join(run.R, "logs/daemon.log"))).toBe(false);
    expect(existsSync(path.join(run.R, "rpc-events.jsonl"))).toBe(false);
    expect(existsSync(path.join(run.work, "omp-ran"))).toBe(false);
    expect(run.read("stub-argv")).toBe(
      `daemon ${run.R}/daemon-request.json ${run.R}/ready.json ${run.R}/status.json ${run.R}/record.json\n`
    );
    expect(run.read("stub-pg")).toBe("postgres://rig@127.0.0.1:1/postgres\n");
    expect(run.calls).toBe(
      [
        `own_dispatch project ${run.co}`,
        `seed forward ${run.R}/fixture.json ${run.world}`,
        `results ${madeHead}`,
        `seed capture-forward ${run.R}/fixture.json ${run.R}`,
        "",
      ].join("\n")
    );
    expect(JSON.parse(run.read("daemon-request.json"))).toEqual({
      moment: "retro",
      role: "implementer",
      issue: "ACME-1",
      steps: ["implementer", "reviewer", "acceptance:owner"],
      world: run.world,
      dispatchUrl: "http://127.0.0.1:1",
      dispatchTokenFile: `${run.work}/services/dispatch-token`,
      envoyUrl: "http://127.0.0.1:1",
      natsUrl: "nats://127.0.0.1:2",
      postgresUrl: "postgres://rig@127.0.0.1:1/postgres",
      ghLog: `${run.R}/calls.jsonl`,
      omp: run.omp,
      home: `${run.R}/home`,
      profile: "skills-head",
      paneLogs: `${run.R}/panes`,
      paneDaemon: "label",
      runDir: run.R,
      envoyTokenFile: "/rig/envoy-token",
    });
    expect(run.read("out.txt")).toBe("exit=0\n");
    // The gh stand-in is first on the PATH the request names the run's bin of.
    expect(existsSync(path.join(run.R, "bin/gh"))).toBe(true);
    // Control: a world.json that names another daemon is refused by name, before anything starts.
    const other = labelRun(
      "label-world-other",
      { statuses: [{ afterMs: 500, status: ended }] },
      {
        world: { "world.json": madeWorld({ daemon: "standin" })["world.json"] ?? "" },
      }
    );
    expect(other.code).toBe(1);
    expect(other.stderr).toContain("the world forward-only/retro-defect is refused:");
    expect(other.stderr).toContain("daemon");
    expect(other.calls).toBe("");
  });

  // Catches: a run cut off while its turn runs, its background work runs or a reminder is armed,
  // which PR #5461's three end facts forbid, and a bound read as an end.
  test("the run ends on the status file's three facts, and not before", () => {
    const statuses = [
      { afterMs: 400, status: status("running", null) },
      {
        afterMs: 900,
        status: status("idle", {
          backgroundWork: { running: true, kinds: ["task"] },
          phase: { nextReminderAt: null },
        }),
      },
      {
        afterMs: 1400,
        status: status("idle", {
          backgroundWork: { running: false },
          phase: { nextReminderAt: "2026-11-26T00:10:00.000Z" },
        }),
      },
      { afterMs: 1900, status: ended },
    ];
    const run = labelRun("label-status", { statuses });
    expect({ code: run.code, stderr: run.stderr }).toEqual({ code: 0, stderr: "" });
    const at = (event: string) => run.events.find((each) => each.event === event)?.at ?? -1;
    expect(at("status 3")).toBeGreaterThan(0);
    expect(at("term")).toBeGreaterThanOrEqual(at("status 3"));
    expect(run.read("out.txt")).toBe("exit=0\n");
    // At its bound the run ends `exit=bound`, and held.txt names what held it.
    const held = labelRun("label-status-bound", { statuses: statuses.slice(0, 2) }, { env: { SKILL_SCENARIOS_TIMEOUT: "2" } });
    expect({ code: held.code, stderr: held.stderr }).toEqual({ code: 0, stderr: "" });
    expect(held.read("out.txt")).toBe("exit=bound\n");
    expect(held.read("held.txt")).toBe(
      "the run did not end within 2 seconds: background work running: task\n"
    );
  });

  // Catches: a run whose daemon never writes its record (no SIGTERM), a record kept elsewhere, and
  // transcripts left in the pane's HOME, where the scorer reads none.
  test("the run's end sends SIGTERM, keeps record.json, and copies the transcripts from ready.sessionDir", () => {
    const record = {
      issue: "ACME-1",
      phase: "merging",
      generation: 2,
      calls: [],
      defects: [],
      steps: [{ task: "t1", step: 3, kind: "retro", role: "implementer", attempt: 1, result: "passed" }],
    };
    const run = labelRun("label-end", { statuses: [{ afterMs: 500, status: ended }], record });
    expect({ code: run.code, stderr: run.stderr }).toEqual({ code: 0, stderr: "" });
    expect(run.events.map((each) => each.event)).toContain("term");
    expect(JSON.parse(run.read("record.json"))).toEqual(record);
    expect(run.read("sessions/ses_pane.jsonl")).toBe('{"type":"session","id":"ses_pane"}\n');
    expect(run.read("sessions/ses_pane/sub.jsonl")).toBe('{"type":"session","id":"sub"}\n');
    // Control: an entry that exits 1 after SIGTERM fails the run, naming its log.
    const failed = labelRun("label-end-exit", { statuses: [{ afterMs: 500, status: ended }], exit: 1 });
    expect(failed.code).toBe(1);
    expect(failed.stderr).toContain("label-texts.sh daemon of label head exited 1");
  });

  // Catches: a launch whose pane never became ready waited on without end, or scored as a run.
  test("a ready file that never comes within the bound makes the launch a rig error", () => {
    const run = labelRun("label-no-ready", { readyAfterMs: null }, { readyBound: 1 });
    expect(run.code).toBe(1);
    const line = run.stderr.split("\n").filter((each) => each.startsWith("skill-scenarios: ")).at(-1) ?? "";
    expect(line).toContain("the label daemon took no claims/ready of the real pane within 1s");
    expect(existsSync(path.join(run.R, "out.txt"))).toBe(false);
    // The batch writes its row as the rig's error (scored_rig_error), of the forward scenario.
    const work = freshDir("label-no-ready-rows");
    const row = bash(
      `${defs("fail", "scored_rig_error")}\nwork=$1\nscored_rig_error forward-only-retro-defect-head-1 head retro-defect "$2"`,
      [work, line]
    );
    expect(row.code).toBe(0);
    const parsed = RunRow.parse(
      JSON.parse(readFileSync(path.join(work, "runs/forward-only-retro-defect-head-1/row.json"), "utf8"))
    );
    expect(parsed).toMatchObject({ outcome: "rig-error", scenario: "forward-only", case: "retro-defect" });
    // A ready.json that fails RigDaemonReady is no ready either.
    const bad = labelRun("label-bad-ready", { ready: { head: "not-a-head" } }, { readyBound: 1 });
    expect(bad.code).toBe(1);
    expect(bad.stderr).toContain("ready.json fails RigDaemonReady: head:");
  });

  // Catches: a record the scorer cannot read taken as a run with no defect and no move back.
  test("a record that fails RigDaemonRecord is a rig error", () => {
    const run = labelRun("label-bad-record", {
      statuses: [{ afterMs: 500, status: ended }],
      record: { issue: "ACME-1", calls: [] },
    });
    expect(run.code).toBe(1);
    expect(run.stderr).toContain("record.json fails RigDaemonRecord: phase:");
  });

  // Catches: a path check whose label must pass its route case before the run, so a label whose
  // route is deleted never reaches the plugin's calls, and a scored batch on a label built with no
  // test run.
  test("--path-check builds with build --compile, and a scored batch with the full build", () => {
    const co = freshDir("build-label");
    const entry = path.join(co, "packages/pi-legion/scripts/skill-scenarios/label-texts.sh");
    mkdirSync(path.dirname(entry), { recursive: true });
    writeFileSync(entry, STUB_ENTRY, { mode: 0o755 });
    const build = (env: Record<string, string>) => {
      const bin = freshDir("build-bin");
      const result = bash(
        `${defs("fail", "label_texts", "build_label_texts")}\nco=$1 P=$2 pg_url=postgres://rig@127.0.0.1:1/postgres\nbuild_label_texts`,
        [co, freshDir("build-profile")],
        { RIG_TEXTS_BIN: bin, ...env }
      );
      return { ...result, calls: readFileSync(path.join(bin, "build-calls"), "utf8") };
    };
    expect(build({ SKILL_SCENARIOS_PATH_CHECK: "1" })).toMatchObject({ code: 0, calls: "build --compile\n" });
    expect(build({})).toMatchObject({ code: 0, calls: "build\n" });
    // The Job hands the batch the path check's variable with --path-check alone.
    const job = plantedForwardJob();
    const checked = job.run(["forward-only", "head", "--world", "retro-defect", "--count", "1", "--path-check"], {
      SKILL_SCENARIOS_MODEL: "standin-report",
    });
    expect({ code: checked.code, stderr: checked.stderr }).toEqual({ code: 0, stderr: "" });
    expect(checked.env).toBe("world=retro-defect check=1 control= model=standin-report\n");
    expect(checked.stdout.split("\n")).toContain("PATH OK d-planted");
    const scored = plantedForwardJob().run(["forward-only", "head", "--world", "retro-defect", "--count", "1"], {
      SKILL_SCENARIOS_MODEL: "standin",
    });
    expect({ code: scored.code, stderr: scored.stderr }).toEqual({ code: 0, stderr: "" });
    expect(scored.env).toBe("world=retro-defect check= control= model=standin\n");
    expect(scored.stdout).not.toContain("PATH OK");
    // A path check needs the scripted model.
    const refused = plantedForwardJob().run(["forward-only", "head", "--world", "retro-defect", "--path-check"], {
      SKILL_SCENARIOS_MODEL: "standin",
    });
    expect({ code: refused.code, calls: refused.calls }).toEqual({ code: 2, calls: "" });
    expect(refused.stderr).toContain("set SKILL_SCENARIOS_MODEL=standin-report");
  });

  // Catches: a scripted model whose report names another issue or head than the run's, or whose
  // completion comes first, and a script written for a run on the real model.
  test("with standin-report the rig writes standin-script.json with the report, then the completion", () => {
    const run = labelRun(
      "label-script",
      { statuses: [{ afterMs: 500, status: ended }] },
      { env: { SKILL_SCENARIOS_MODEL: "standin-report" }, modelLine: "model standin/standin-model" }
    );
    expect({ code: run.code, stderr: run.stderr }).toEqual({ code: 0, stderr: "" });
    expect(JSON.parse(run.read("standin-script.json"))).toEqual({
      calls: [
        {
          tool: "legion",
          input: {
            op: "report_defect",
            issue: "ACME-1",
            defect: "path check: src/config.ts loadConfig returns DEFAULTS on a parse error",
            head: madeHead,
          },
        },
        {
          tool: "legion",
          input: { op: "handoff_complete", summary: "path check", plan_rating: 3, plan_reason: "path check" },
        },
      ],
    });
    // The run's own profile reaches the run's model stand-in.
    const models = JSON.parse(run.read("home/.omp/profiles/skills-head/agent/models.yml"));
    expect(models.providers.standin.baseUrl).toStartWith("http://");
    expect(models.providers.standin.baseUrl).not.toContain("model-standin.invalid");
    // Control: a run on the plain stand-in gets no script.
    const plain = labelRun(
      "label-script-plain",
      { statuses: [{ afterMs: 500, status: ended }] },
      { env: { SKILL_SCENARIOS_MODEL: "standin" }, modelLine: "model standin/standin-model" }
    );
    expect({ code: plain.code, stderr: plain.stderr }).toEqual({ code: 0, stderr: "" });
    expect(existsSync(path.join(plain.R, "standin-script.json"))).toBe(false);
  });

  // Catches: the intercepting proxy given to a scored run, whose report would then never reach the
  // label's daemon, or a control that never gets it.
  test("--path-control intercept writes paneDaemon intercept-report, and no other run does", () => {
    const request = (env: Record<string, string>) => {
      const R = freshDir("pane-daemon");
      const world = freshDir("pane-daemon-world");
      writeFileSync(path.join(world, "world.json"), madeWorld()["world.json"] ?? "");
      const services = path.join(R, "services-env");
      writeFileSync(services, "ENVOY_URL=http://e\nENVOY_NATS_URL=nats://n\nENVOY_TOKEN_FILE=/t\n");
      const omp = path.join(R, "omp-install/bin/omp");
      mkdirSync(path.dirname(omp), { recursive: true });
      writeFileSync(omp, "#!/bin/sh\necho omp/1.2.3\n", { mode: 0o755 });
      const result = bash(
        `${defs("fail", "pinned_omp", "rig_omp", "service_value", "daemon_request")}\nR=$1 work=$1 services_env=$2 pin=github:example/oh-my-pi@1.2.3-4 profile=skills-head dispatch_port=1 pg_url=postgres://p\ndaemon_request "$3"`,
        [R, services, world],
        { SKILL_SCENARIOS_OMP: omp, ...env }
      );
      return { ...result, paneDaemon: result.code === 0 ? JSON.parse(result.stdout).paneDaemon : null };
    };
    expect(request({ SKILL_SCENARIOS_PATH_CONTROL: "intercept" })).toMatchObject({ code: 0, paneDaemon: "intercept-report" });
    expect(request({})).toMatchObject({ code: 0, paneDaemon: "label" });
    const other = request({ SKILL_SCENARIOS_PATH_CONTROL: "proxy" });
    expect(other.code).toBe(1);
    expect(other.stderr).toContain("SKILL_SCENARIOS_PATH_CONTROL is 'proxy': it is intercept, or unset");
    // The Job hands the batch the control's variable with --path-control intercept alone.
    const job = plantedForwardJob().run(
      ["forward-only", "head", "--world", "retro-defect", "--count", "1", "--path-check", "--path-control", "intercept"],
      { SKILL_SCENARIOS_MODEL: "standin-report" }
    );
    expect({ code: job.code, stderr: job.stderr }).toEqual({ code: 0, stderr: "" });
    expect(job.env).toBe("world=retro-defect check=1 control=intercept model=standin-report\n");
    const alone = plantedForwardJob().run(
      ["forward-only", "head", "--world", "retro-defect", "--path-control", "intercept"],
      { SKILL_SCENARIOS_MODEL: "standin-report" }
    );
    expect({ code: alone.code, calls: alone.calls }).toEqual({ code: 2, calls: "" });
  });

  // Catches: a world whose task names the plant, so every run would notice it from its task alone
  // (Review Focus 3).
  test("a world whose task holds a defect anchor is refused", () => {
    const run = labelRun(
      "label-anchor",
      { statuses: [{ afterMs: 500, status: ended }] },
      {
        world: {
          "spec.md": "# List rows\n\nKeep `src/config.ts` as it is.\n",
        },
      }
    );
    expect(run.code).toBe(1);
    expect(run.stderr).toContain('the task\'s text holds the defect anchor "src/config.ts", in spec.md');
    expect({ calls: run.calls, argv: run.read("stub-argv") }).toEqual({ calls: "", argv: "" });
  });

  // Catches: a world whose retro gate would return the task for a check that has no pass, a cause
  // that stays (Review Focus 4), refused neither before the start nor once the head is known.
  test("a forward-only run does not launch while a before_merge check has no pass", () => {
    const run = labelRun(
      "label-unpassed",
      { statuses: [{ afterMs: 500, status: ended }] },
      {
        world: {
          "verification.md":
            '# Verification\n\n:::check{#c-list-rows surface="cli" run="bun test" when="before_merge"}\nThe list test passes.\n:::\n\n:::check{#c-page-size surface="cli" run="bun test" when="before_merge"}\nThe page size holds.\n:::\n',
        },
      }
    );
    expect(run.code).toBe(1);
    expect(run.stderr).toContain(
      "verification.md holds the before_merge check c-page-size, which world.json's passes does not name"
    );
    expect({ calls: run.calls, argv: run.read("stub-argv") }).toEqual({ calls: "", argv: "" });
    // The backstop: the run's Dispatch shows a check with no pass at the head the entry built.
    const unrecorded = labelRun(
      "label-unrecorded",
      { statuses: [{ afterMs: 500, status: ended }] },
      { env: { RESULTS_MISSING: `the before_merge check c-list-rows of ACME-1 has no pass at ${madeHead}` } }
    );
    expect(unrecorded.code).toBe(1);
    expect(unrecorded.stderr).toContain(
      `the run's Dispatch shows a before_merge check with no pass at the head ${madeHead}, so the run stops: the before_merge check c-list-rows of ACME-1 has no pass at ${madeHead}`
    );
    expect(existsSync(path.join(unrecorded.R, "out.txt"))).toBe(false);
  });
});
